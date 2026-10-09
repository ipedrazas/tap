// HTTP API. It is reached only through the console (NetworkPolicy), which
// sets x-tap-user from the identity the gateway verified, or from inside the
// pod by an operator (kubectl exec).
//
//	GET  /healthz
//	GET  /v1/jobs                 newest first, without bulky fields
//	POST /v1/jobs                 {name, spec, runner?, owner?, publish?, route?, hide?}
//	GET  /v1/jobs/{id}
//	GET  /v1/jobs/{id}/events     SSE: job status and the model's progress
//	POST /v1/jobs/{id}/abort
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { BACKGROUND_CONTEXT, withCancel } from "@earendil-works/chord/context";
import { type AgentEvent, type Harness, watchEvents } from "@earendil-works/pi-durable";
import { checkRequest, type Factory, type Job, type JobRequest, type JobStore } from "./jobs.ts";

const MAX_BODY = 64 << 10;
const USER = "x-tap-user";

export type ApiOptions = { factory: Factory; store: JobStore; harness: Harness; routes: readonly string[] };

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
		if (["done", "failed", "aborted"].includes(j.status)) close();
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
			return send(res, 404, { error: "not found" });
		} catch (e) {
			if (!res.headersSent) send(res, 500, { error: (e as Error).message });
		}
	});
}
