// Jobs: one spec in, one pull request out. Each job is a pi-durable
// conversation plus a small JSON record. First the model drafts a brief
// (intake); checkBrief turns its gaps into questions and the job waits for
// answers. Then it builds exactly the approved brief. Every step can be
// re-run after a restart (the checkout is skipped once recorded, submissions
// are idempotent by request id, the gates are re-run, publishing is skipped
// once a PR exists).
import { chownSync, existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { Context } from "@earendil-works/chord";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { AssistantEntry, type Conversation, type Harness, UsageDoc, type UsageState } from "@earendil-works/pi-durable";
import { PROVIDER } from "./model.ts";
import { type Minter, proxyEnv, proxyUrl } from "./egress.ts";
import { type Brief, checkBrief, type Finding, renderBrief } from "./brief.ts";
import { agentFiles, type GateResults, run, runGates, snapshot, type Tree } from "./gates.ts";
import { answersTask, buildTask, instructions, intakeTask } from "./prompt.ts";
import type { Publisher, PullRequest } from "./publisher.ts";
import type { SandboxOptions } from "./tools.ts";

export type JobStatus = "queued" | "checkout" | "intake" | "questions" | "running" | "gates" | "publishing" | "done" | "failed" | "aborted";
const TERMINAL: readonly JobStatus[] = ["done", "failed", "aborted"];
// Waiting for a person: not resumed on restart, not counted as running.
export const WAITING: readonly JobStatus[] = ["questions"];

export type JobRequest = {
	name: string;
	spec: string;
	runner?: string;
	owner?: string;
	publish?: boolean;
	route?: string;
	// Agents removed from the checkout before the model starts, so an eval
	// can't copy the reference agent it is scored against. Dry runs only.
	hide?: string[];
	// Build on the brief's assumptions instead of asking about gaps (evals).
	assume?: boolean;
};

export type QA = { round: number; id: string; question: string; answer: string };

export type Job = {
	id: string;
	name: string;
	spec: string;
	runner: string;
	owner: string;
	publish: boolean;
	route: string;
	hide?: string[];
	assume?: boolean;
	user: string;
	status: JobStatus;
	createdAt: string;
	updatedAt: string;
	startedAt?: string;
	finishedAt?: string;
	error?: string;
	base?: string;
	conversationId?: number; // pi-durable ConversationId
	brief?: Brief;
	findings?: Finding[]; // open questions while status is "questions"
	accepted?: Finding[]; // gaps built on as assumptions
	qa?: QA[];
	intakeRound?: number;
	intakeSent?: number;
	proceed?: boolean;
	approved?: boolean;
	answer?: string;
	gates?: GateResults;
	pr?: { number: number; url: string };
	usage?: UsageState;
};

// saveBrief is where submit_brief puts the brief; the job reads it back.
export function saveBrief(jobDir: string, brief: Brief): void {
	writeFileSync(join(jobDir, "brief.json"), JSON.stringify(brief, null, 2));
}

export const NAME = /^[a-z][a-z0-9-]{1,38}[a-z0-9]$/;
const RUNNERS = ["runner-node", "runner-python"];
const MAX_SPEC = 16 << 10;

export function checkRequest(r: JobRequest, routes: readonly string[]): string | undefined {
	if (typeof r.name !== "string" || !NAME.test(r.name) || !r.name.endsWith("-agent")) return "name must be a DNS label ending in -agent";
	if (typeof r.spec !== "string" || r.spec.trim().length < 20 || r.spec.length > MAX_SPEC) return `spec must be 20..${MAX_SPEC} characters`;
	if (r.runner !== undefined && !RUNNERS.includes(r.runner)) return `runner must be one of ${RUNNERS.join(", ")}`;
	if (r.owner !== undefined && (typeof r.owner !== "string" || !/^[a-z][a-z0-9-]{0,39}$/.test(r.owner))) return "owner must be a short lowercase name";
	if (r.route !== undefined && !routes.includes(r.route)) return `route must be one of ${routes.join(", ")}`;
	if (r.hide !== undefined) {
		if (!Array.isArray(r.hide) || r.hide.length > 10 || !r.hide.every((h) => typeof h === "string" && NAME.test(h))) return "hide must list up to 10 agent names";
		// A checkout with agents removed is not main: never publish from it.
		if (r.hide.length > 0 && r.publish !== false) return "hide needs publish: false";
	}
	if (r.assume !== undefined && typeof r.assume !== "boolean") return "assume must be a boolean";
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
			if (!TERMINAL.includes(job.status) && !WAITING.includes(job.status)) this.enqueue(job.id);
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
			hide: req.hide?.length ? req.hide : undefined,
			assume: req.assume || undefined,
			user,
			status: "queued",
			createdAt: now,
			updatedAt: now,
		});
		this.o.log("job submitted", { job: job.id, agent: job.name, user, publish: job.publish, route: job.route });
		this.enqueue(job.id);
		return job;
	}

	// answer records a person's answers to the open questions and resumes the
	// job: the model revises the brief, which is checked again. With proceed,
	// whatever is still open is built on as an assumption.
	answer(id: string, answers: Record<string, string>, proceed: boolean): Job | string {
		const job = this.o.store.get(id);
		if (!job) return "no such job";
		if (job.status !== "questions") return `job is ${job.status}, not waiting for answers`;
		const open = new Map((job.findings ?? []).map((f) => [f.id, f]));
		const round = (job.intakeRound ?? 0) + 1;
		const qa: QA[] = [];
		for (const [fid, text] of Object.entries(answers ?? {})) {
			const f = open.get(fid);
			if (!f) return `no open question ${fid}`;
			if (typeof text !== "string" || text.length > 2000) return `answer to ${fid} must be text of at most 2000 characters`;
			if (text.trim()) qa.push({ round, id: fid, question: f.question, answer: text.trim() });
		}
		if (qa.length === 0 && !proceed) return "answer at least one question, or proceed";
		this.o.log("job answered", { job: job.id, agent: job.name, answers: qa.length, proceed });
		Object.assign(job, { qa: [...(job.qa ?? []), ...qa], intakeRound: round, proceed: proceed || undefined, status: "queued" as JobStatus });
		this.o.store.put(job);
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
			for (const h of job.hide ?? []) rmSync(join(repoDir, "agents", h), { recursive: true, force: true });
			writeFileSync(beforeFile, JSON.stringify(snapshot(repoDir)));
		}
		const before = JSON.parse(readFileSync(beforeFile, "utf8")) as Tree;

		const conv = await this.conversation(job, repoDir);
		if (!job.approved) {
			this.set(job, "intake");
			const round = job.intakeRound ?? 0;
			const briefFile = join(jobDir, "brief.json");
			if (job.intakeSent !== round) {
				rmSync(briefFile, { force: true });
				this.set(job, "intake", { intakeSent: round });
			}
			const content = round === 0 ? intakeTask(job.spec) : answersTask((job.qa ?? []).filter((q) => q.round === round), Boolean(job.proceed));
			await this.ask(job, conv, content, `job:${job.id}:intake:${round}`);
			if (!existsSync(briefFile)) throw new Error("the model ended intake without submitting a brief");
			const brief = JSON.parse(readFileSync(briefFile, "utf8")) as Brief;
			const findings = checkBrief(brief);
			if (findings.length > 0 && !job.assume && !job.proceed) {
				this.set(job, "questions", { brief, findings });
				return;
			}
			this.set(job, "running", { brief, findings: [], accepted: findings, approved: true });
		}

		this.set(job, "running");
		const answer = await this.ask(job, conv, buildTask(job.brief!, job.qa ?? [], job.accepted ?? []), `job:${job.id}:build`);
		const usage = await this.o.harness.snapshot(UsageDoc, conv.id, BACKGROUND_CONTEXT);
		this.set(job, "gates", { answer, usage: usage as UsageState | undefined });

		const gates = await runGates(job.name, repoDir, jobDir, before, this.o.sandbox, job.brief);
		this.set(job, "gates", { gates });
		if (!gates.ok) {
			this.finish(job, "failed", gateFailure(gates));
			return;
		}
		if (!job.publish || job.hide?.length) {
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

	private async conversation(job: Job, repoDir: string): Promise<Conversation> {
		const ctx = BACKGROUND_CONTEXT;
		const existing = job.conversationId ? await this.o.harness.conversation(job.conversationId as never, ctx) : undefined;
		if (existing) return existing;
		const conv = await this.o.harness.createConversation(
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
		this.set(job, job.status, { conversationId: conv.id });
		return conv;
	}

	// ask submits one input and returns the model's final text.
	private async ask(job: Job, conv: Conversation, content: string, requestId: string): Promise<string> {
		const ctx = BACKGROUND_CONTEXT;
		const submission = await conv.submit({ type: "input", content, requestId }, ctx);
		const settled = await this.withTimeout(job, conv, submission.wait(ctx));
		if (settled.status !== "done" || settled.type !== "input") {
			throw new Error(`model run ended without an answer: ${settled.reason ?? settled.status}`);
		}
		const answerId = settled.answer;
		const entry = await conv.commit((tx) => tx.entry(AssistantEntry, answerId), ctx);
		return assistantText(entry);
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
	if (g.brief && !g.brief.ok) failed.push(`agent differs from the approved brief: ${g.brief.problems.slice(0, 5).join("; ")}`);
	return `gates failed: ${failed.join("; ")}`;
}

const fence = (s: string) => `\`\`\`\n${s.trim().replaceAll("```", "ʼʼʼ")}\n\`\`\``;

export function prBody(job: Job): string {
	const g = job.gates!;
	const tokens = Object.values(job.usage?.models ?? {}).reduce((n, u) => n + ((u as { totalTokens?: number }).totalTokens ?? 0), 0);
	const parts = [
		job.answer || "_The model gave no report._",
		"---",
		...(job.brief ? ["## Approved brief", "", renderBrief(job.brief), ""] : []),
		...(job.qa?.length ? ["## Questions and answers", "", ...job.qa.map((q) => `- **${q.question}**\n  ${q.answer}`), ""] : []),
		...(job.accepted?.length ? ["## Built on these assumptions", "", ...job.accepted.map((f) => `- ${f.question}`), ""] : []),
		"## Factory gates",
		`- \`tapctl validate\`: ${g.validate.code === 0 ? "pass" : "fail"}`,
		`- fixtures (local): ${g.fixtures.passed} passed, ${g.fixtures.failed} failed, ${g.fixtures.skipped} skipped`,
		"- scope: only `agents/" + job.name + "/` changed",
		...(g.brief ? [`- brief: tools, hosts, secrets and effects ${g.brief.ok ? "match" : "differ"}`] : []),
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
