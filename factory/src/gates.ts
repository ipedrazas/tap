// The gates the orchestrator runs after the model says it is done. They do
// not trust the model's report: validation and fixtures run again, and the
// checkout is compared file by file with what was downloaded.
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { closeSync, constants, lstatSync, openSync, readdirSync, readFileSync, readlinkSync, readSync } from "node:fs";
import { join } from "node:path";
import { type Brief, conformance } from "./brief.ts";
import { permissions } from "./permissions.ts";
import { killSandbox, sandboxCommand, sandboxEnv, type SandboxOptions } from "./tools.ts";

export type RunResult = { code: number; output: string };

const MAX_OUTPUT = 64 << 10;

// run executes a shell script as the sandbox user and returns its exit code
// and combined output. It has no network access unless extraEnv carries a
// proxy credential. No process of the sandbox user runs before or after it:
// the gates check files that a leftover could otherwise change behind them.
export function run(script: string, cwd: string, jobDir: string, sb: SandboxOptions, timeoutSec: number, extraEnv: Record<string, string> = {}): Promise<RunResult> {
	killSandbox(sb.uid);
	return new Promise<RunResult>((resolve) => {
		const child = spawn("/bin/bash", ["-c", sandboxCommand(script, sb.uid)], {
			cwd,
			env: { ...sandboxEnv(jobDir, sb), ...extraEnv },
			stdio: ["ignore", "pipe", "pipe"],
			detached: true,
		});
		let output = "";
		const add = (b: Buffer) => {
			if (output.length < MAX_OUTPUT) output += b.toString("utf8");
		};
		child.stdout.on("data", add);
		child.stderr.on("data", add);
		const timer = setTimeout(() => {
			try {
				process.kill(-child.pid!, "SIGKILL");
			} catch {}
			output += `\n[killed after ${timeoutSec}s]`;
		}, timeoutSec * 1000);
		child.on("close", (code) => {
			clearTimeout(timer);
			resolve({ code: code ?? -1, output: output.slice(0, MAX_OUTPUT) });
		});
		child.on("error", (e) => {
			clearTimeout(timer);
			resolve({ code: -1, output: String(e) });
		});
	}).finally(() => killSandbox(sb.uid));
}

// A tree snapshot: relative path -> content hash (or link target).
export type Tree = Record<string, string>;

// The gates run as root within the factory's memory limit, so files are
// hashed in chunks, and one over MAX_HASHED isn't read at all: a sparse file
// made with truncate -s would otherwise be read whole and crash the factory.
// changedOutside fails scope on such a file anywhere outside the agent.
export const MAX_HASHED = 64 << 20;
const TOO_LARGE = "too-large:";

function hashFile(p: string, size: number): string {
	if (size > MAX_HASHED) return `${TOO_LARGE}${size}`;
	const h = createHash("sha256");
	const buf = Buffer.alloc(1 << 20);
	const fd = openSync(p, constants.O_RDONLY | constants.O_NOFOLLOW);
	try {
		for (let n = readSync(fd, buf); n > 0; n = readSync(fd, buf)) h.update(buf.subarray(0, n));
	} finally {
		closeSync(fd);
	}
	return h.digest("hex");
}

export function snapshot(root: string, skip: (rel: string) => boolean = () => false): Tree {
	const out: Tree = {};
	const walk = (rel: string) => {
		for (const name of readdirSync(join(root, rel)).sort()) {
			const r = rel ? `${rel}/${name}` : name;
			if (skip(r)) continue;
			const p = join(root, r);
			const st = lstatSync(p);
			if (st.isDirectory()) walk(r);
			else if (st.isSymbolicLink()) out[r] = `link:${readlinkSync(p)}`;
			else if (st.isFile()) out[r] = `${st.mode & 0o111 ? "x" : "f"}:${hashFile(p, st.size)}`;
			else out[r] = "special";
		}
	};
	walk("");
	return out;
}

// outside lists every path outside agentDir that was added, changed or removed.
export function changedOutside(before: Tree, after: Tree, agentDir: string): string[] {
	const inAgent = (p: string) => p.startsWith(`${agentDir}/`);
	const changed = new Set<string>();
	for (const [p, h] of Object.entries(after)) if (!inAgent(p) && (before[p] !== h || h.includes(TOO_LARGE))) changed.add(p);
	for (const p of Object.keys(before)) if (!inAgent(p) && !(p in after)) changed.add(p);
	return [...changed].sort();
}

export type AgentFile = { path: string; content: Buffer; executable: boolean };

// Byproducts of running tools locally, never part of a bundle.
const JUNK = new Set(["__pycache__", ".DS_Store", "node_modules"]);

// agentFiles reads the new agent's directory for the pull request. Links and
// special files are refused: a bundle is regular files only.
// The publisher's limits (pkg/factory): checked before reading, so a huge
// file fails the job instead of the factory.
const MAX_FILES = 500;
const MAX_TOTAL = 5 << 20;

export function agentFiles(repoDir: string, agentDir: string): AgentFile[] {
	const files: AgentFile[] = [];
	let total = 0;
	const walk = (rel: string) => {
		for (const name of readdirSync(join(repoDir, rel)).sort()) {
			if (JUNK.has(name)) continue;
			const r = `${rel}/${name}`;
			const st = lstatSync(join(repoDir, r));
			if (st.isDirectory()) walk(r);
			else if (st.isFile()) {
				total += st.size;
				if (files.length >= MAX_FILES || total > MAX_TOTAL) throw new Error(`${agentDir} is over ${MAX_FILES} files or ${MAX_TOTAL >> 20} MiB`);
				files.push({ path: r, content: readFileSync(join(repoDir, r)), executable: (st.mode & 0o111) !== 0 });
			} else throw new Error(`${r} is not a regular file`);
		}
	};
	walk(agentDir);
	return files;
}

export type GateResults = {
	ok: boolean;
	validate: RunResult;
	fixtures: RunResult & { passed: number; failed: number; skipped: number };
	scope: { ok: boolean; outside: string[] };
	diff: RunResult;
	brief?: { ok: boolean; problems: string[] };
};

export async function runGates(name: string, repoDir: string, jobDir: string, before: Tree, sb: SandboxOptions, brief?: Brief): Promise<GateResults> {
	const agentDir = `agents/${name}`;
	const validate = await run(`tapctl validate ${agentDir}`, repoDir, jobDir, sb, 120);
	const fx = await run(`tap-test-local ${name}`, repoDir, jobDir, sb, 900);
	const m = /^(\d+) passed, (\d+) failed, (\d+) skipped$/m.exec(fx.output);
	const fixtures = { ...fx, passed: Number(m?.[1] ?? 0), failed: Number(m?.[2] ?? 0), skipped: Number(m?.[3] ?? 0) };
	const diff = await run(`tapctl diff ${agentDir}`, repoDir, jobDir, sb, 60);
	const outside = changedOutside(before, snapshot(repoDir), agentDir);
	const scope = { ok: outside.length === 0, outside };
	const problems = brief ? conformance(brief, permissions(diff.output)) : [];
	const briefGate = brief ? { ok: problems.length === 0, problems } : undefined;
	const ok = validate.code === 0 && fx.code === 0 && m !== null && fixtures.failed === 0 && fixtures.passed > 0 && scope.ok && (briefGate?.ok ?? true);
	return { ok, validate, fixtures, scope, diff, ...(briefGate ? { brief: briefGate } : {}) };
}
