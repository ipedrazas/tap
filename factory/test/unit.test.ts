import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, realpathSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { confine, confinedEnv, PathEscape } from "../src/confine.ts";
import { mint, proxyUrl } from "../src/egress.ts";
import { agentFiles, changedOutside, snapshot } from "../src/gates.ts";
import { checkRequest, gateFailure } from "../src/jobs.ts";
import { sandboxCommand, sandboxEnv } from "../src/tools.ts";

function jobDir() {
	const root = realpathSync(mkdtempSync(join(tmpdir(), "factory-")));
	mkdirSync(join(root, "repo/agents"), { recursive: true });
	return { root, repo: join(root, "repo") };
}

test("token matches pkg/egress (TestTokenVector)", () => {
	const tok = mint("0123456789abcdef0123456789abcdef", { a: "tap-factory", s: "bash", e: 1700000000, n: "0011223344556677" });
	assert.equal(
		tok,
		"eyJhIjoidGFwLWZhY3RvcnkiLCJzIjoiYmFzaCIsImUiOjE3MDAwMDAwMDAsIm4iOiIwMDExMjIzMzQ0NTU2Njc3In0.SfXwQHA-X6fxcfowMdsxEsWt-ttRryZRUYPcaPTsdnA",
	);
	assert.match(proxyUrl({ proxy: "p:3128", agent: "tap-factory", key: "k".repeat(32) }, "bash", 60), /^http:\/\/tap-factory:[\w-]+\.[\w-]+@p:3128$/);
});

test("confine keeps paths inside the job directory", () => {
	const { root, repo } = jobDir();
	assert.equal(confine(root, repo, "agents/x/agent.yaml"), join(repo, "agents/x/agent.yaml"));
	assert.equal(confine(root, repo, `${root}/tmp/new/file`), join(root, "tmp/new/file"));
	for (const p of ["/etc/passwd", "../../etc/passwd", "~/.ssh/id_rsa", "/run/tap/model/api-key", root + "/../x"]) {
		assert.throws(() => confine(root, repo, p), PathEscape, p);
	}
	// A symlink planted by a command must not lead outside.
	symlinkSync("/etc", join(repo, "etc-link"));
	assert.throws(() => confine(root, repo, "etc-link/passwd"), PathEscape);
	symlinkSync("/nonexistent-target", join(repo, "dangling"));
	assert.throws(() => confine(root, repo, "dangling/x"), PathEscape);
});

test("confinedEnv refuses file operations outside the job directory", async () => {
	const { root, repo } = jobDir();
	const env = confinedEnv({ root, cwd: repo });
	const ctx = BACKGROUND_CONTEXT;
	assert.equal((await env.writeFile("agents/a.txt", "hi", ctx)).ok, true);
	const read = await env.readTextFile("agents/a.txt", ctx);
	assert.ok(read.ok && read.value === "hi");
	const outside = await env.readTextFile("/etc/hosts", ctx);
	assert.equal(outside.ok, false);
	assert.equal((await env.writeFile("/tmp/factory-escape", "x", ctx)).ok, false);
	assert.equal((await env.renameFile("agents/a.txt", "/tmp/factory-escape", ctx)).ok, false);
	const tmp = await env.createTempFile({ prefix: "spill" }, ctx);
	assert.ok(tmp.ok && tmp.value.startsWith(join(root, "tmp")));
});

test("sandboxCommand quotes the script for bash -c", () => {
	assert.equal(sandboxCommand("echo hi", 0), "echo hi");
	const cmd = sandboxCommand(`echo 'it'"s" $HOME`, 61000);
	assert.match(cmd, /^exec setpriv --reuid=61000 --regid=61000 --clear-groups --inh-caps=-all --no-new-privs -- \/bin\/bash -c /);
	// The quoted part must reach bash unchanged.
	const quoted = cmd.slice(cmd.indexOf("-c ") + 3);
	const out = execFileSync("/bin/bash", ["-c", `printf %s ${quoted}`], { encoding: "utf8" });
	assert.equal(out, `echo 'it'"s" $HOME`);
});

test("sandboxEnv inherits nothing", () => {
	process.env.FACTORY_SECRET_PROBE = "leak";
	const env = sandboxEnv("/data/work/j1", { uid: 61000, path: "/usr/bin:/bin", extraEnv: { A: "1" } });
	assert.deepEqual(Object.keys(env).sort(), ["A", "HOME", "LANG", "PATH", "TMPDIR"]);
	assert.equal(env.HOME, "/data/work/j1/home");
});

test("changedOutside and agentFiles", () => {
	const { repo } = jobDir();
	writeFileSync(join(repo, "Taskfile.yml"), "a");
	const before = snapshot(repo);
	mkdirSync(join(repo, "agents/x-agent/tools/__pycache__"), { recursive: true });
	writeFileSync(join(repo, "agents/x-agent/agent.yaml"), "y");
	writeFileSync(join(repo, "agents/x-agent/tools/t.py"), "print()", { mode: 0o755 });
	writeFileSync(join(repo, "agents/x-agent/tools/__pycache__/t.pyc"), "junk");
	assert.deepEqual(changedOutside(before, snapshot(repo), "agents/x-agent"), []);
	writeFileSync(join(repo, "Taskfile.yml"), "b");
	writeFileSync(join(repo, "agents/x-agentevil"), "c");
	assert.deepEqual(changedOutside(before, snapshot(repo), "agents/x-agent"), ["Taskfile.yml", "agents/x-agentevil"]);
	const files = agentFiles(repo, "agents/x-agent");
	assert.deepEqual(
		files.map((f) => [f.path, f.executable]),
		[
			["agents/x-agent/agent.yaml", false],
			["agents/x-agent/tools/t.py", true],
		],
	);
	symlinkSync("/etc/passwd", join(repo, "agents/x-agent/link"));
	assert.throws(() => agentFiles(repo, "agents/x-agent"), /not a regular file/);
});

test("checkRequest", () => {
	const ok = { name: "weather-agent", spec: "An agent that answers weather questions using Open-Meteo." };
	assert.equal(checkRequest(ok, ["agent"]), undefined);
	assert.match(checkRequest({ ...ok, name: "weather" }, ["agent"])!, /-agent/);
	assert.match(checkRequest({ ...ok, name: "../x-agent" }, ["agent"])!, /DNS label/);
	assert.match(checkRequest({ ...ok, spec: "short" }, ["agent"])!, /spec/);
	assert.match(checkRequest({ ...ok, runner: "runner-go" }, ["agent"])!, /runner/);
	assert.match(checkRequest({ ...ok, route: "gpt" }, ["agent"])!, /route/);
	assert.equal(checkRequest({ ...ok, hide: ["echo-agent"], publish: false }, ["agent"]), undefined);
	assert.match(checkRequest({ ...ok, hide: ["echo-agent"] }, ["agent"])!, /publish: false/);
	assert.match(checkRequest({ ...ok, hide: ["../.."], publish: false }, ["agent"])!, /hide/);
	const g = {
		ok: false,
		validate: { code: 1, output: "" },
		fixtures: { code: 0, output: "", passed: 0, failed: 0, skipped: 0 },
		scope: { ok: false, outside: ["Taskfile.yml"] },
		diff: { code: 3, output: "" },
	};
	assert.equal(gateFailure(g), "gates failed: validate; fixtures (0 passed, 0 failed); changes outside the agent: Taskfile.yml");
});
