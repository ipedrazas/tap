// A whole job against a scripted model: checkout, conversation with real
// tools, gates (with stand-in tapctl and tap-test-local) and publishing.
import assert from "node:assert/strict";
import { chmodSync, mkdirSync, mkdtempSync, realpathSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { createModels, fauxAssistantMessage, fauxProvider, fauxText, fauxToolCall } from "@earendil-works/pi-ai";
import { createRegistry, Harness, MemoryStorage } from "@earendil-works/pi-durable";
import { api } from "../src/api.ts";
import { confinedEnv } from "../src/confine.ts";
import { Factory, type Job, JobStore, saveBrief } from "../src/jobs.ts";
import type { Publisher } from "../src/publisher.ts";
import { factoryTools, jobDirOf } from "../src/tools.ts";
import { promptOnlyReviewBrief, thingBrief } from "./fixtures.ts";

const submit = (brief: unknown) => fauxAssistantMessage([fauxToolCall("submit_brief", brief as never)], { stopReason: "toolUse" });

const BASE = "0123456789abcdef0123456789abcdef01234567";

function fakeBin(dir: string) {
	mkdirSync(dir, { recursive: true });
	const script = (name: string, body: string) => {
		writeFileSync(join(dir, name), `#!/bin/bash\n${body}\n`);
		chmodSync(join(dir, name), 0o755);
	};
	script("tapctl", `case "$1" in validate) test -f "$2/agent.yaml" ;; diff) echo "+ tool get_thing (effects: read)"; exit 3 ;; esac`);
	script("tap-test-local", `test -f "agents/$1/agent.yaml" && echo "PASS get_thing / ok" && echo && echo "1 passed, 0 failed, 0 skipped"`);
}

async function setup(responses: Parameters<ReturnType<typeof fauxProvider>["setResponses"]>[0]) {
	const dir = realpathSync(mkdtempSync(join(tmpdir(), "factory-job-")));
	fakeBin(join(dir, "bin"));
	const faux = fauxProvider({ provider: "kodo", models: [{ id: "sim" }] });
	faux.setResponses(responses);
	const models = createModels();
	models.setProvider(faux.provider);
	const sandbox = { uid: 0, path: `${join(dir, "bin")}:/usr/bin:/bin`, extraEnv: {} };
	const tools = factoryTools({ ...sandbox, timeoutSec: 30, onBrief: saveBrief });
	const registry = createRegistry();
	registry.install(tools);
	const harness = await Harness.open(
		new MemoryStorage(),
		{
			models,
			registry,
			settings: { extensions: [tools], toolExecution: "sequential" },
			env: ({ cwd }) => confinedEnv({ root: jobDirOf(cwd!), cwd: cwd! }),
		},
		BACKGROUND_CONTEXT,
	);
	const prs: unknown[] = [];
	const publisher = {
		head: async () => BASE,
		pr: async (req: unknown) => {
			prs.push(req);
			return { number: 7, html_url: "https://github.com/o/r/pull/7" };
		},
	} as unknown as Publisher;
	const store = new JobStore(join(dir, "jobs"));
	const factory = new Factory({
		harness,
		store,
		publisher,
		workDir: join(dir, "work"),
		repo: "o/r",
		routes: ["sim"],
		defaultRoute: "sim",
		sandbox,
		jobTimeoutMin: 1,
		fetch: async (_base, repoDir) => {
			mkdirSync(join(repoDir, ".claude/skills/new-agent"), { recursive: true });
			writeFileSync(join(repoDir, ".claude/skills/new-agent/SKILL.md"), "---\nname: new-agent\n---\n# Create a tap agent\n");
			mkdirSync(join(repoDir, "agents/echo-agent"), { recursive: true });
			writeFileSync(join(repoDir, "agents/echo-agent/agent.yaml"), "name: echo-agent\n");
			writeFileSync(join(repoDir, "Taskfile.yml"), "version: 3\n");
		},
		log: () => {},
	});
	const wait = async (id: string): Promise<Job> => {
		for (let i = 0; i < 200; i++) {
			const j = store.get(id)!;
			if (["done", "failed", "aborted"].includes(j.status)) return j;
			await new Promise((r) => setTimeout(r, 25));
		}
		throw new Error("job did not finish");
	};
	const waitFor = async (id: string, status: string): Promise<Job> => {
		for (let i = 0; i < 200; i++) {
			const j = store.get(id)!;
			if (j.status === status || ["done", "failed", "aborted"].includes(j.status)) return j;
			await new Promise((r) => setTimeout(r, 25));
		}
		throw new Error(`job never reached ${status}`);
	};
	const newFactory = () => new Factory({ ...(factory as unknown as { o: ConstructorParameters<typeof Factory>[0] }).o });
	return { dir, factory, store, prs, wait, waitFor, faux, harness, models, newFactory };
}

const spec = "An agent that returns a thing from the Thing API at api.thing.example.";

test("a job writes the agent, passes the gates and opens a PR", async () => {
	const { factory, prs, wait, harness, faux } = await setup([
		submit(thingBrief),
		fauxAssistantMessage([fauxToolCall("write", { path: "agents/thing-agent/agent.yaml", content: "name: thing-agent\n" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxToolCall("read", { path: "/etc/hosts" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxToolCall("bash", { command: "echo $HOME; env | sort | cut -d= -f1 | tr '\\n' ' '" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("## Summary\nthing-agent fetches things.\n\n## Decisions for the user\nnone")]),
	]);
	const job = await wait(factory.submit({ name: "thing-agent", spec, route: "sim" }, "alice@example.com").id);
	assert.equal(job.status, "done", job.error);
	assert.equal(job.base, BASE);
	assert.equal(job.gates?.ok, true);
	assert.equal(job.gates?.fixtures.passed, 1);
	assert.deepEqual(job.pr, { number: 7, url: "https://github.com/o/r/pull/7" });
	assert.match(job.answer!, /thing-agent fetches things/);
	assert.equal(faux.state.callCount, 5);

	const pr = prs[0] as { agent: string; base: string; title: string; body: string; files: { path: string }[] };
	assert.equal(pr.agent, "thing-agent");
	assert.equal(pr.base, BASE);
	assert.deepEqual(
		pr.files.map((f) => f.path),
		["agents/thing-agent/agent.yaml"],
	);
	assert.match(pr.body, /thing-agent fetches things/);
	assert.match(pr.body, /\+ tool get_thing/);
	assert.match(pr.body, /requested by alice@example.com/);

	// The model saw a refusal for /etc/hosts, and bash ran with a clean environment.
	const conv = (await harness.conversation(job.conversationId as never, BACKGROUND_CONTEXT))!;
	const ctx = await conv.context(BACKGROUND_CONTEXT);
	const results = ctx.messages.filter((m) => m.role === "toolResult").map((m) => JSON.stringify(m.content));
	assert.match(results[0]!, /Brief received/);
	assert.match(results[2]!, /outside the job directory/);
	assert.match(results[3]!, /\/home/);
	assert.doesNotMatch(results[3]!, /FACTORY|NODE_OPTIONS|USER |SHELL /);
	assert.match(results[3]!, /HOME LANG PATH PWD SHLVL TMPDIR/);
	await harness.close(BACKGROUND_CONTEXT);
});

test("changes outside the agent fail the gates and open no PR", async () => {
	const { factory, prs, wait, harness } = await setup([
		submit(thingBrief),
		fauxAssistantMessage([fauxToolCall("write", { path: "agents/thing-agent/agent.yaml", content: "name: thing-agent\n" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxToolCall("bash", { command: "echo pwned >> Taskfile.yml" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("done")]),
	]);
	const job = await wait(factory.submit({ name: "thing-agent", spec, route: "sim" }, "bob").id);
	assert.equal(job.status, "failed");
	assert.match(job.error!, /changes outside the agent: Taskfile.yml/);
	assert.equal(prs.length, 0);
	await harness.close(BACKGROUND_CONTEXT);
});

test("an existing agent is refused before the model runs", async () => {
	const { factory, wait, faux, harness } = await setup([submit(thingBrief)]);
	const job = await wait(factory.submit({ name: "echo-agent", spec, route: "sim" }, "bob").id);
	assert.equal(job.status, "failed");
	assert.match(job.error!, /already exists/);
	assert.equal(faux.state.callCount, 0);
	await harness.close(BACKGROUND_CONTEXT);
});

test("hide removes the reference agent before the model starts", async () => {
	const { factory, prs, wait, harness } = await setup([
		submit(thingBrief),
		fauxAssistantMessage([fauxToolCall("bash", { command: "ls agents" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxToolCall("write", { path: "agents/thing-agent/agent.yaml", content: "x\n" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("report")]),
	]);
	const job = await wait(factory.submit({ name: "thing-agent", spec, route: "sim", publish: false, hide: ["echo-agent"] }, "eval").id);
	assert.equal(job.status, "done", job.error);
	assert.equal(job.gates?.scope.ok, true);
	assert.equal(prs.length, 0);
	const conv = (await harness.conversation(job.conversationId as never, BACKGROUND_CONTEXT))!;
	const ls = (await conv.context(BACKGROUND_CONTEXT)).messages.filter((m) => m.role === "toolResult")[1]!;
	assert.doesNotMatch(JSON.stringify(ls.content), /echo-agent/);
	await harness.close(BACKGROUND_CONTEXT);
});

test("publish: false stops after the gates", async () => {
	const { factory, prs, wait, harness } = await setup([
		submit(thingBrief),
		fauxAssistantMessage([fauxToolCall("write", { path: "agents/thing-agent/agent.yaml", content: "x\n" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("report")]),
	]);
	const job = await wait(factory.submit({ name: "thing-agent", spec, route: "sim", publish: false }, "eval").id);
	assert.equal(job.status, "done", job.error);
	assert.equal(job.pr, undefined);
	assert.equal(prs.length, 0);
	await harness.close(BACKGROUND_CONTEXT);
});

// The console opens /events for finished jobs too (and EventSource
// reconnects); closing the stream must not leave a rejected promise behind,
// which crashed the factory in production.
test("the events stream of a finished job closes cleanly", async () => {
	const { factory, store, wait, harness } = await setup([
		submit(thingBrief),
		fauxAssistantMessage([fauxToolCall("write", { path: "agents/thing-agent/agent.yaml", content: "x\n" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("report")]),
	]);
	const job = await wait(factory.submit({ name: "thing-agent", spec, route: "sim", publish: false }, "eval").id);
	const rejections: unknown[] = [];
	const onRejection = (e: unknown) => rejections.push(e);
	process.on("unhandledRejection", onRejection);
	const server = api({ factory, store, harness, routes: ["sim"] });
	await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
	const { port } = server.address() as { port: number };
	try {
		for (let i = 0; i < 3; i++) {
			const res = await fetch(`http://127.0.0.1:${port}/v1/jobs/${job.id}/events`, { headers: { "x-tap-user": "t" } });
			const body = await res.text();
			assert.match(body, /"status":"done"/);
		}
		// One stream dropped by the client mid-way.
		const ac = new AbortController();
		const res = await fetch(`http://127.0.0.1:${port}/v1/jobs/${job.id}/events`, { headers: { "x-tap-user": "t" }, signal: ac.signal });
		ac.abort();
		await res.text().catch(() => {});
		await new Promise((r) => setTimeout(r, 1500));
		assert.deepEqual(rejections, []);
	} finally {
		process.off("unhandledRejection", onRejection);
		server.close();
		await harness.close(BACKGROUND_CONTEXT);
	}
});

test("gaps in the brief become questions; answers revise it, then it builds", async () => {
	const { factory, prs, waitFor, wait, faux, harness, store } = await setup([
		submit(promptOnlyReviewBrief),
		submit(thingBrief),
		fauxAssistantMessage([fauxToolCall("write", { path: "agents/thing-agent/agent.yaml", content: "x\n" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("## Summary\nbuilt")]),
	]);
	const id = factory.submit({ name: "thing-agent", spec }, "alice").id;
	const q = await waitFor(id, "questions");
	assert.equal(q.status, "questions");
	assert.ok(q.findings!.some((f) => f.id === "unsourced:review_pr.diff_path"));
	assert.equal(faux.state.callCount, 1, "nothing is built while questions are open");

	assert.equal(typeof factory.answer(id, { nope: "x" }, false), "string");
	assert.equal(typeof factory.answer(id, {}, false), "string");
	const fid = "unsourced:review_pr.diff_path";
	const r = factory.answer(id, { [fid]: "Add a tool that fetches the thing by id" }, false);
	assert.equal(typeof r, "object");
	const job = await wait(id);
	assert.equal(job.status, "done", job.error);
	assert.equal(job.approved, true);
	assert.equal(job.qa?.[0]?.answer, "Add a tool that fetches the thing by id");
	assert.equal(job.gates?.brief?.ok, true);
	const pr = prs[0] as { body: string };
	assert.match(pr.body, /## Approved brief/);
	assert.match(pr.body, /id ← user:thing_id/);
	assert.match(pr.body, /Add a tool that fetches the thing by id/);
	// The model saw the answer before revising.
	const conv = (await harness.conversation(job.conversationId as never, BACKGROUND_CONTEXT))!;
	const users = (await conv.context(BACKGROUND_CONTEXT)).messages.filter((m) => m.role === "user").map((m) => JSON.stringify(m.content));
	assert.match(users[1]!, /Add a tool that fetches the thing by id/);
	assert.match(users[2]!, /Phase 2, build/);
	assert.equal(store.get(id)!.findings?.length, 0);
	await harness.close(BACKGROUND_CONTEXT);
});

test("assume builds on the gaps and records them", async () => {
	const gappy = { ...thingBrief, purpose_basis: "assumption" };
	const { factory, prs, wait, harness } = await setup([
		submit(gappy),
		fauxAssistantMessage([fauxToolCall("write", { path: "agents/thing-agent/agent.yaml", content: "x\n" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("report")]),
	]);
	const job = await wait(factory.submit({ name: "thing-agent", spec, assume: true }, "eval").id);
	assert.equal(job.status, "done", job.error);
	assert.deepEqual(job.accepted?.map((f) => f.id), ["assumption:purpose"]);
	assert.match((prs[0] as { body: string }).body, /## Built on these assumptions/);
	await harness.close(BACKGROUND_CONTEXT);
});

test("a build that drifts from the brief fails the gates", async () => {
	const withHost = { ...thingBrief, tools: [{ ...thingBrief.tools[0]!, host: "api.thing.example" }] };
	const { factory, prs, wait, harness } = await setup([
		submit(withHost),
		fauxAssistantMessage([fauxToolCall("write", { path: "agents/thing-agent/agent.yaml", content: "x\n" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("report")]),
	]);
	const job = await wait(factory.submit({ name: "thing-agent", spec }, "bob").id);
	assert.equal(job.status, "failed");
	assert.match(job.error!, /differs from the approved brief: host api.thing.example:443 is in the brief but not in the agent/);
	assert.equal(prs.length, 0);
	await harness.close(BACKGROUND_CONTEXT);
});

test("a job waiting for answers is not resumed on restart", async () => {
	const { factory, waitFor, faux, harness, store, newFactory } = await setup([submit(promptOnlyReviewBrief)]);
	const id = factory.submit({ name: "thing-agent", spec }, "alice").id;
	await waitFor(id, "questions");
	newFactory().resume();
	await new Promise((r) => setTimeout(r, 200));
	assert.equal(store.get(id)!.status, "questions");
	assert.equal(faux.state.callCount, 1);
	await harness.close(BACKGROUND_CONTEXT);
});
