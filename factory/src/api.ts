// HTTP API. It is reached only through the console (NetworkPolicy), which
// sets x-tap-user from the identity the gateway verified, or from inside the
// pod by an operator (kubectl exec).
//
//	GET  /healthz
//	GET  /v1/jobs                 newest first, without bulky fields
//	POST /v1/jobs                 {name, spec, runner?, owner?, publish?, route?, hide?}
//	GET  /v1/jobs/{id}
//	GET  /v1/jobs/{id}/events     SSE: job status and the model's progress
//	POST /v1/jobs/{id}/answers    {answers: {questionId: text}, proceed?}
//	POST /v1/jobs/{id}/abort
//
// Routes the console calls itself, after checking that x-tap-user may edit
// the agent. They sit outside /v1 so the console's /api/factory/* -> /v1/*
// proxy cannot reach them from a browser.
//
//	GET  /console/agents/{name}     -> {sha, agentYaml} (agent.yaml on the base branch)
//	POST /console/agents/{name}/pr  {base, title?, egress?} -> {number, html_url}
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { BACKGROUND_CONTEXT, withCancel } from "@earendil-works/chord/context";
import { type AgentEvent, type Harness, watchEvents } from "@earendil-works/pi-durable";
import { checkRequest, type Factory, type Job, type JobRequest, type JobStore } from "./jobs.ts";
import { type Publisher, PublisherError } from "./publisher.ts";

const MAX_BODY = 64 << 10;
const USER = "x-tap-user";

export type ApiOptions = {
	factory: Factory;
	store: JobStore;
	harness: Harness;
	routes: readonly string[];
	publisher: Pick<Publisher, "agent" | "edit">;
};

function send(res: ServerResponse, status: number, body: unknown): void {
	res.writeHead(status, { "content-type": "application/json", "cache-control": "no-store" });
	res.end(JSON.stringify(body));
}

function readBody(req: IncomingMessage): Promise<string> {
	return new Promise((resolve, reject) => {
		let data = "";
		req.setEncoding("utf8");
		req.on("data", (chunk: string) => {
			data += chunk;
			if (data.length > MAX_BODY) {
				reject(new Error("body too large"));
				req.destroy();
			}
		});
		req.on("end", () => resolve(data));
		req.on("error", reject);
	});
}

export function summary(j: Job) {
	return {
		id: j.id,
		name: j.name,
		status: j.status,
		user: j.user,
		runner: j.runner,
		route: j.route,
		publish: j.publish,
		createdAt: j.createdAt,
		finishedAt: j.finishedAt,
		error: j.error,
		questions: j.status === "questions" ? (j.findings?.length ?? 0) : undefined,
		pr: j.pr,
	};
}

// compact turns pi-durable events into the small records the console shows.
export function compact(e: AgentEvent): Record<string, unknown>[] {
	switch (e.type) {
		case "message_update":
			return e.changes.flatMap((c) => (c.type === "text_delta" ? [{ t: "text", d: c.delta }] : []));
		case "tool_execution_start": {
			const args = JSON.stringify(e.args);
			return [{ t: "tool", name: e.toolName, args: args.length > 400 ? `${args.slice(0, 400)}…` : args }];
		}
		case "tool_execution_end":
			return [{ t: "tool_end", name: e.toolName }];
		case "turn_end":
			return [{ t: "turn" }];
		case "auto_retry_start":
			return [{ t: "retry", attempt: e.attempt, error: e.errorMessage }];
		case "snapshot":
			return [{ t: "snapshot", entries: e.entries.length, tools: e.tools.length }];
		default:
			return [];
	}
}

// checkEdit validates the shape of a console edit body; the publisher does
// the real checks.
export function checkEdit(b: unknown): string | undefined {
	if (typeof b !== "object" || b === null || Array.isArray(b)) return "body must be an object";
	const o = b as Record<string, unknown>;
	for (const k of Object.keys(o)) if (!["base", "title", "egress"].includes(k)) return `unknown field ${k}`;
	if (typeof o.base !== "string" || !/^[0-9a-f]{40}$/.test(o.base)) return "base must be a full commit sha";
	if (o.title !== undefined && typeof o.title !== "string") return "title must be a string";
	if (o.egress !== undefined) {
		if (typeof o.egress !== "object" || o.egress === null || Array.isArray(o.egress)) return "egress must be an object of tool -> [host:port]";
		for (const [tool, hosts] of Object.entries(o.egress)) {
			if (!Array.isArray(hosts) || hosts.some((h) => typeof h !== "string")) return `egress.${tool} must be a list of strings`;
		}
	}
	if (o.title === undefined && o.egress === undefined) return "nothing to change: give a title or egress";
	return undefined;
}

async function consoleRoute(o: ApiOptions, user: string, parts: string[], req: IncomingMessage, res: ServerResponse): Promise<void> {
	if (parts[1] !== "agents" || !parts[2]) return send(res, 404, { error: "not found" });
	const name = decodeURIComponent(parts[2]);
	try {
		if (parts.length === 3 && req.method === "GET") return send(res, 200, await o.publisher.agent(name));
		if (parts.length === 4 && parts[3] === "pr" && req.method === "POST") {
			let body: unknown;
			try {
				body = JSON.parse(await readBody(req));
			} catch (e) {
				return send(res, 400, { error: `bad request: ${(e as Error).message}` });
			}
			const problem = checkEdit(body);
			if (problem) return send(res, 422, { error: problem });
			const b = body as { base: string; title?: string; egress?: Record<string, string[]> };
			return send(res, 200, await o.publisher.edit({ agent: name, base: b.base, user, title: b.title, egress: b.egress }));
		}
	} catch (e) {
		if (e instanceof PublisherError) {
			const status = [400, 404, 409, 422].includes(e.status) ? e.status : 502;
			return send(res, status, { error: e.message });
		}
		throw e;
	}
	return send(res, 404, { error: "not found" });
}

async function events(o: ApiOptions, job: Job, req: IncomingMessage, res: ServerResponse): Promise<void> {
	res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-store", connection: "keep-alive" });
	const write = (data: unknown) => res.write(`data: ${JSON.stringify(data)}\n\n`);
	const { context, cancel } = withCancel(BACKGROUND_CONTEXT);
	let stream: Awaited<ReturnType<typeof watchEvents>> | undefined;
	let last = "";
	const poll = setInterval(() => {
		const j = o.store.get(job.id);
		if (!j) return;
		const key = `${j.status}:${j.updatedAt}`;
		if (key !== last) {
			last = key;
			write({ t: "job", job: summary(j) });
		}
		if (!stream && j.conversationId) void attach(j.conversationId).catch(() => {});
		if (["done", "failed", "aborted", "questions"].includes(j.status)) close();
	}, 1000);
	let closed = false;
	const attach = async (conversationId: number) => {
		if (stream || closed) return;
		let s: Awaited<ReturnType<typeof watchEvents>>;
		try {
			s = await watchEvents(o.harness, conversationId as never, context);
		} catch {
			return; // cancelled while attaching
		}
		// Cancelling the context ends the watch with an AbortError; that's how
		// it stops, not a failure.
		s.closed.catch(() => {});
		if (closed) {
			await s.stop().catch(() => {});
			return;
		}
		stream = s;
		s.start(async (batch) => {
			for (const e of batch) for (const c of compact(e)) write(c);
		});
	};
	const close = () => {
		if (closed) return;
		closed = true;
		clearInterval(poll);
		const s = stream;
		void (s ? s.stop() : Promise.resolve()).catch(() => {}).finally(cancel);
		res.end();
	};
	req.on("close", close);
}

export function api(o: ApiOptions): Server {
	return createServer(async (req, res) => {
		try {
			const url = new URL(req.url ?? "/", "http://factory");
			const parts = url.pathname.split("/").filter(Boolean);
			if (req.method === "GET" && url.pathname === "/healthz") return send(res, 200, { ok: true });
			const user = req.headers[USER];
			if (typeof user !== "string" || !user) return send(res, 401, { error: `missing ${USER}` });
			if (parts[0] === "console") return consoleRoute(o, user, parts, req, res);
			if (parts[0] !== "v1" || parts[1] !== "jobs") return send(res, 404, { error: "not found" });
			if (parts.length === 2 && req.method === "GET") return send(res, 200, o.store.list().map(summary));
			if (parts.length === 2 && req.method === "POST") {
				let body: JobRequest;
				try {
					body = JSON.parse(await readBody(req)) as JobRequest;
				} catch (e) {
					return send(res, 400, { error: `bad request: ${(e as Error).message}` });
				}
				const problem = checkRequest(body, o.routes);
				if (problem) return send(res, 422, { error: problem });
				if (o.store.list().some((j) => j.name === body.name && !["done", "failed", "aborted"].includes(j.status))) {
					return send(res, 409, { error: `a job for ${body.name} is already running` });
				}
				return send(res, 202, o.factory.submit(body, user));
			}
			const job = parts[2] ? o.store.get(parts[2]) : undefined;
			if (!job) return send(res, 404, { error: "no such job" });
			if (parts.length === 3 && req.method === "GET") return send(res, 200, job);
			if (parts.length === 4 && parts[3] === "events" && req.method === "GET") return events(o, job, req, res);
			if (parts.length === 4 && parts[3] === "abort" && req.method === "POST") return send(res, 200, await o.factory.abort(job.id));
			if (parts.length === 4 && parts[3] === "answers" && req.method === "POST") {
				let body: { answers?: Record<string, string>; proceed?: boolean };
				try {
					body = JSON.parse(await readBody(req));
				} catch (e) {
					return send(res, 400, { error: `bad request: ${(e as Error).message}` });
				}
				if (body.answers !== undefined && (typeof body.answers !== "object" || Array.isArray(body.answers))) return send(res, 422, { error: "answers must be an object" });
				const r = o.factory.answer(job.id, body.answers ?? {}, body.proceed === true);
				return typeof r === "string" ? send(res, 409, { error: r }) : send(res, 202, r);
			}
			return send(res, 404, { error: "not found" });
		} catch (e) {
			if (!res.headersSent) send(res, 500, { error: (e as Error).message });
		}
	});
}
