// The gates the orchestrator runs after the model says it is done. They do
// not trust the model's report: validation and fixtures run again, and the
// checkout is compared file by file with what was downloaded.
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { lstatSync, readdirSync, readFileSync, readlinkSync } from "node:fs";
import { join } from "node:path";
import { sandboxCommand, sandboxEnv, type SandboxOptions } from "./tools.ts";

export type RunResult = { code: number; output: string };

const MAX_OUTPUT = 64 << 10;

// run executes a shell script as the sandbox user and returns its exit code
// and combined output. It has no network access unless extraEnv carries a
// proxy credential.
export function run(script: string, cwd: string, jobDir: string, sb: SandboxOptions, timeoutSec: number, extraEnv: Record<string, string> = {}): Promise<RunResult> {
	return new Promise((resolve) => {
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
	});
}

// A tree snapshot: relative path -> content hash (or link target).
export type Tree = Record<string, string>;

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
			else if (st.isFile()) out[r] = `${st.mode & 0o111 ? "x" : "f"}:${createHash("sha256").update(readFileSync(p)).digest("hex")}`;
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
	for (const [p, h] of Object.entries(after)) if (!inAgent(p) && before[p] !== h) changed.add(p);
	for (const p of Object.keys(before)) if (!inAgent(p) && !(p in after)) changed.add(p);
	return [...changed].sort();
}

export type AgentFile = { path: string; content: Buffer; executable: boolean };

// Byproducts of running tools locally, never part of a bundle.
const JUNK = new Set(["__pycache__", ".DS_Store", "node_modules"]);

// agentFiles reads the new agent's directory for the pull request. Links and
// special files are refused: a bundle is regular files only.
export function agentFiles(repoDir: string, agentDir: string): AgentFile[] {
	const files: AgentFile[] = [];
	const walk = (rel: string) => {
		for (const name of readdirSync(join(repoDir, rel)).sort()) {
			if (JUNK.has(name)) continue;
			const r = `${rel}/${name}`;
			const st = lstatSync(join(repoDir, r));
			if (st.isDirectory()) walk(r);
			else if (st.isFile()) files.push({ path: r, content: readFileSync(join(repoDir, r)), executable: (st.mode & 0o111) !== 0 });
			else throw new Error(`${r} is not a regular file`);
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
};

export async function runGates(name: string, repoDir: string, jobDir: string, before: Tree, sb: SandboxOptions): Promise<GateResults> {
	const agentDir = `agents/${name}`;
	const validate = await run(`tapctl validate ${agentDir}`, repoDir, jobDir, sb, 120);
	const fx = await run(`tap-test-local ${name}`, repoDir, jobDir, sb, 900);
	const m = /^(\d+) passed, (\d+) failed, (\d+) skipped$/m.exec(fx.output);
	const fixtures = { ...fx, passed: Number(m?.[1] ?? 0), failed: Number(m?.[2] ?? 0), skipped: Number(m?.[3] ?? 0) };
	const diff = await run(`tapctl diff ${agentDir}`, repoDir, jobDir, sb, 60);
	const outside = changedOutside(before, snapshot(repoDir), agentDir);
	const scope = { ok: outside.length === 0, outside };
	const ok = validate.code === 0 && fx.code === 0 && m !== null && fixtures.failed === 0 && fixtures.passed > 0 && scope.ok;
	return { ok, validate, fixtures, scope, diff };
}
