// Jobs: one spec in, one pull request out. Each job is a pi-durable
// conversation plus a small JSON record; every step can be re-run after a
// restart (the checkout is skipped once recorded, the submission is
// idempotent by request id, the gates are re-run, publishing is skipped once
// a PR exists).
import { chownSync, existsSync, mkdirSync, readdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { Context } from "@earendil-works/chord";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { AssistantEntry, type Conversation, type Harness, UsageDoc, type UsageState } from "@earendil-works/pi-durable";
import { PROVIDER } from "./model.ts";
import { type Minter, proxyEnv, proxyUrl } from "./egress.ts";
import { agentFiles, type GateResults, run, runGates, snapshot, type Tree } from "./gates.ts";
import { instructions, task } from "./prompt.ts";
import type { Publisher, PullRequest } from "./publisher.ts";
import type { SandboxOptions } from "./tools.ts";

export type JobStatus = "queued" | "checkout" | "running" | "gates" | "publishing" | "done" | "failed" | "aborted";
const TERMINAL: readonly JobStatus[] = ["done", "failed", "aborted"];

export type JobRequest = {
	name: string;
	spec: string;
	runner?: string;
	owner?: string;
	publish?: boolean;
	route?: string;
};

export type Job = {
	id: string;
	name: string;
	spec: string;
	runner: string;
	owner: string;
	publish: boolean;
	route: string;
	user: string;
	status: JobStatus;
	createdAt: string;
	updatedAt: string;
	startedAt?: string;
	finishedAt?: string;
	error?: string;
	base?: string;
	conversationId?: number; // pi-durable ConversationId
	answer?: string;
	gates?: GateResults;
	pr?: { number: number; url: string };
	usage?: UsageState;
};

export const NAME = /^[a-z][a-z0-9-]{1,38}[a-z0-9]$/;
const RUNNERS = ["runner-node", "runner-python"];
const MAX_SPEC = 16 << 10;

export function checkRequest(r: JobRequest, routes: readonly string[]): string | undefined {
	if (typeof r.name !== "string" || !NAME.test(r.name) || !r.name.endsWith("-agent")) return "name must be a DNS label ending in -agent";
	if (typeof r.spec !== "string" || r.spec.trim().length < 20 || r.spec.length > MAX_SPEC) return `spec must be 20..${MAX_SPEC} characters`;
	if (r.runner !== undefined && !RUNNERS.includes(r.runner)) return `runner must be one of ${RUNNERS.join(", ")}`;
	if (r.owner !== undefined && (typeof r.owner !== "string" || !/^[a-z][a-z0-9-]{0,39}$/.test(r.owner))) return "owner must be a short lowercase name";
	if (r.route !== undefined && !routes.includes(r.route)) return `route must be one of ${routes.join(", ")}`;
	return undefined;
}

export class JobStore {
	private dir: string;
	constructor(dir: string) {
		this.dir = dir;
		mkdirSync(dir, { recursive: true });
	}

	get(id: string): Job | undefined {
		if (!/^[a-z0-9-]+$/.test(id)) return undefined;
		const p = join(this.dir, `${id}.json`);
		return existsSync(p) ? (JSON.parse(readFileSync(p, "utf8")) as Job) : undefined;
	}

	list(): Job[] {
		return readdirSync(this.dir)
			.filter((f) => f.endsWith(".json"))
			.map((f) => JSON.parse(readFileSync(join(this.dir, f), "utf8")) as Job)
			.sort((a, b) => b.createdAt.localeCompare(a.createdAt));
	}

	put(job: Job): Job {
		job.updatedAt = new Date().toISOString();
		const p = join(this.dir, `${job.id}.json`);
		writeFileSync(`${p}.tmp`, JSON.stringify(job, null, 2));
		renameSync(`${p}.tmp`, p);
		return job;
	}
}

export type FactoryOptions = {
	harness: Harness;
	store: JobStore;
	publisher: Publisher;
	workDir: string;
	repo: string;
	routes: readonly string[];
	defaultRoute: string;
	sandbox: SandboxOptions;
	minter?: Minter;
	jobTimeoutMin: number;
	// fetch replaces the tarball download (tests).
	fetch?: (base: string, repoDir: string) => Promise<void>;
	log: (msg: string, fields?: Record<string, unknown>) => void;
};

function newId(): string {
	const t = new Date().toISOString().replace(/[-:T]/g, "").slice(0, 12); // yyyymmddhhmm
	return `${t}-${Math.random().toString(36).slice(2, 6)}`;
}

export class Factory {
	private queue: string[] = [];
	private running = false;
	private aborts = new Map<string, () => void>();

	private o: FactoryOptions;
	constructor(o: FactoryOptions) {
		this.o = o;
	}

	// resume re-queues every job a previous process left unfinished.
	resume(): void {
		this.o.harness.resume();
		for (const job of this.o.store.list().reverse()) {
			if (!TERMINAL.includes(job.status)) this.enqueue(job.id);
		}
	}

	submit(req: JobRequest, user: string): Job {
		const now = new Date().toISOString();
		const job = this.o.store.put({
			id: newId(),
			name: req.name,
			spec: req.spec,
			runner: req.runner ?? "runner-node",
			owner: req.owner ?? "platform",
			publish: req.publish ?? true,
			route: req.route ?? this.o.defaultRoute,
			user,
			status: "queued",
			createdAt: now,
			updatedAt: now,
		});
		this.o.log("job submitted", { job: job.id, agent: job.name, user, publish: job.publish, route: job.route });
		this.enqueue(job.id);
		return job;
	}

	async abort(id: string): Promise<Job | undefined> {
		const job = this.o.store.get(id);
		if (!job || TERMINAL.includes(job.status)) return job;
		this.queue = this.queue.filter((q) => q !== id);
		this.aborts.get(id)?.();
		if (job.conversationId) {
			const conv = await this.o.harness.conversation(job.conversationId as never, BACKGROUND_CONTEXT);
			await conv?.abort(BACKGROUND_CONTEXT);
		}
		return this.finish(this.o.store.get(id) ?? job, "aborted", "aborted by user");
	}

	private enqueue(id: string): void {
		this.queue.push(id);
		void this.drain();
	}

	private async drain(): Promise<void> {
		if (this.running) return;
		this.running = true;
		try {
			for (let id = this.queue.shift(); id; id = this.queue.shift()) {
				const job = this.o.store.get(id);
				if (!job || TERMINAL.includes(job.status)) continue;
				try {
					await this.process(job);
				} catch (e) {
					const latest = this.o.store.get(id) ?? job;
					if (!TERMINAL.includes(latest.status)) this.finish(latest, "failed", (e as Error).message);
				}
			}
		} finally {
			this.running = false;
		}
	}

	private set(job: Job, status: JobStatus, extra: Partial<Job> = {}): Job {
		const latest = this.o.store.get(job.id);
		if (latest && TERMINAL.includes(latest.status)) throw new Error(`job ${job.id} is ${latest.status}`);
		Object.assign(job, extra, { status });
		this.o.log("job status", { job: job.id, agent: job.name, status });
		return this.o.store.put(job);
	}

	private finish(job: Job, status: JobStatus, error?: string): Job {
		Object.assign(job, { status, error, finishedAt: new Date().toISOString() });
		this.o.log("job finished", { job: job.id, agent: job.name, status, error, pr: job.pr?.url });
		return this.o.store.put(job);
	}

	private dirs(job: Job) {
		const jobDir = join(this.o.workDir, job.id);
		return { jobDir, repoDir: join(jobDir, "repo"), beforeFile: join(jobDir, "before.json") };
	}

	private async process(job: Job): Promise<void> {
		job.startedAt ??= new Date().toISOString();
		const { jobDir, repoDir, beforeFile } = this.dirs(job);

		if (!existsSync(beforeFile)) {
			this.set(job, "checkout");
			await this.checkout(job, jobDir, repoDir);
			if (existsSync(join(repoDir, "agents", job.name))) {
				this.finish(job, "failed", `agents/${job.name} already exists on main; the factory only creates new agents`);
				return;
			}
			writeFileSync(beforeFile, JSON.stringify(snapshot(repoDir)));
		}
		const before = JSON.parse(readFileSync(beforeFile, "utf8")) as Tree;

		this.set(job, "running");
		const { answer, conv } = await this.converse(job, repoDir);
		const usage = await this.o.harness.snapshot(UsageDoc, conv.id, BACKGROUND_CONTEXT);
		this.set(job, "gates", { answer, usage: usage as UsageState | undefined });

		const gates = await runGates(job.name, repoDir, jobDir, before, this.o.sandbox);
		this.set(job, "gates", { gates });
		if (!gates.ok) {
			this.finish(job, "failed", gateFailure(gates));
			return;
		}
		if (!job.publish) {
			this.finish(job, "done");
			return;
		}
		if (!job.pr) {
			this.set(job, "publishing");
			const pr = await this.publish(job, repoDir);
			job.pr = { number: pr.number, url: pr.html_url };
		}
		this.finish(job, "done");
	}

	private async checkout(job: Job, jobDir: string, repoDir: string): Promise<void> {
		const base = job.base ?? (await this.o.publisher.head());
		this.set(job, "checkout", { base });
		mkdirSync(jobDir, { recursive: true, mode: 0o711 });
		for (const d of ["repo", "home", "tmp"]) {
			const p = join(jobDir, d);
			mkdirSync(p, { recursive: true });
			if (this.o.sandbox.uid !== 0) chownSync(p, this.o.sandbox.uid, this.o.sandbox.uid);
		}
		if (this.o.fetch) return this.o.fetch(base, repoDir);
		const env = this.o.minter ? proxyEnv(proxyUrl(this.o.minter, "checkout", 300)) : {};
		const url = `https://codeload.github.com/${this.o.repo}/tar.gz/${base}`;
		const r = await run(
			`set -o pipefail; curl -fsSL --retry 3 '${url}' | tar -xz --no-same-owner --no-same-permissions --strip-components=1 -C repo`,
			jobDir,
			jobDir,
			this.o.sandbox,
			300,
			env,
		);
		if (r.code !== 0) throw new Error(`checkout of ${base} failed: ${r.output.trim().slice(-500)}`);
	}

	private async converse(job: Job, repoDir: string): Promise<{ answer: string; conv: Conversation }> {
		const ctx = BACKGROUND_CONTEXT;
		let conv = job.conversationId ? await this.o.harness.conversation(job.conversationId as never, ctx) : undefined;
		if (!conv) {
			conv = await this.o.harness.createConversation(
				{
					ownership: { kind: "ownerless" },
					agent: {
						model: { provider: PROVIDER, modelId: job.route },
						cwd: repoDir,
						instructions: instructions(repoDir, { name: job.name, runner: job.runner, owner: job.owner }),
					},
				},
				ctx,
			);
			this.set(job, "running", { conversationId: conv.id });
		}
		const submission = await conv.submit({ type: "input", content: task(job.spec), requestId: `job:${job.id}` }, ctx);
		const settled = await this.withTimeout(job, conv, submission.wait(ctx));
		if (settled.status !== "done" || settled.type !== "input") {
			throw new Error(`model run ended without an answer: ${settled.reason ?? settled.status}`);
		}
		const answerId = settled.answer;
		const entry = await conv.commit((tx) => tx.entry(AssistantEntry, answerId), ctx);
		return { answer: assistantText(entry), conv };
	}

	private withTimeout<T>(job: Job, conv: Conversation, p: Promise<T>): Promise<T> {
		return new Promise<T>((resolve, reject) => {
			const stop = (reason: string) => {
				void conv.abort(BACKGROUND_CONTEXT);
				reject(new Error(reason));
			};
			const timer = setTimeout(() => stop(`timed out after ${this.o.jobTimeoutMin} minutes`), this.o.jobTimeoutMin * 60_000);
			this.aborts.set(job.id, () => stop("aborted by user"));
			p.then(resolve, reject).finally(() => {
				clearTimeout(timer);
				this.aborts.delete(job.id);
			});
		});
	}

	private publish(job: Job, repoDir: string): Promise<PullRequest> {
		return this.o.publisher.pr({
			job: job.id,
			agent: job.name,
			base: job.base!,
			title: `Add ${job.name} (factory)`,
			body: prBody(job),
			files: agentFiles(repoDir, `agents/${job.name}`),
		});
	}
}

// assistantText extracts the text blocks of an AssistantEntry record.
function assistantText(entry: unknown): string {
	const message = (entry as { model?: { content?: { type: string; text?: string }[] }[] } | undefined)?.model?.[0];
	return (message?.content ?? [])
		.filter((b) => b.type === "text" && b.text)
		.map((b) => b.text)
		.join("\n")
		.trim();
}

export function gateFailure(g: GateResults): string {
	const failed: string[] = [];
	if (g.validate.code !== 0) failed.push("validate");
	if (g.fixtures.code !== 0 || g.fixtures.failed > 0 || g.fixtures.passed === 0) failed.push(`fixtures (${g.fixtures.passed} passed, ${g.fixtures.failed} failed)`);
	if (!g.scope.ok) failed.push(`changes outside the agent: ${g.scope.outside.slice(0, 5).join(", ")}`);
	return `gates failed: ${failed.join("; ")}`;
}

const fence = (s: string) => `\`\`\`\n${s.trim().replaceAll("```", "ʼʼʼ")}\n\`\`\``;

export function prBody(job: Job): string {
	const g = job.gates!;
	const tokens = Object.values(job.usage?.models ?? {}).reduce((n, u) => n + ((u as { totalTokens?: number }).totalTokens ?? 0), 0);
	const parts = [
		job.answer || "_The model gave no report._",
		"---",
		"## Factory gates",
		`- \`tapctl validate\`: ${g.validate.code === 0 ? "pass" : "fail"}`,
		`- fixtures (local): ${g.fixtures.passed} passed, ${g.fixtures.failed} failed, ${g.fixtures.skipped} skipped`,
		"- scope: only `agents/" + job.name + "/` changed",
		"",
		"Permission diff:",
		fence(g.diff.output),
		"",
		"`task agent:test` (runner image, gVisor) has not run yet. It runs before signing, as for any agent.",
		"",
		"<details><summary>Spec</summary>",
		"",
		fence(job.spec),
		"",
		"</details>",
		"",
		`Factory job \`${job.id}\`, requested by ${job.user}, model route \`${job.route}\`, base \`${job.base}\`, ${tokens} tokens.`,
	];
	const body = parts.join("\n");
	return body.length > 60000 ? `${body.slice(0, 59000)}\n\n_(truncated)_` : body;
}

export type { Context };
