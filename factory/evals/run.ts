// task factory:eval: run the eval specs against the deployed factory as dry
// runs (no PR) and score each result against its reference agent. Specs with
// `expect: questions` are bad on purpose: they pass if intake stops with
// questions about where the agent's inputs come from. The factory is reached
// with kubectl exec, like task factory:submit.
//
//	node --experimental-strip-types factory/evals/run.ts [--route agent] [--only hn] [--score <results.json>]
import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";
import { parseArgs } from "node:util";
import { permissions } from "../src/permissions.ts";

export { permissions };

export type EvalSpec = {
	file: string;
	name: string;
	runner: string;
	reference: string;
	egress: string[];
	secrets: string[];
	secretCount: number;
	effects: string;
	expect?: "questions";
	spec: string;
};

export function parseSpec(file: string, text: string): EvalSpec {
	const m = /^---\n([\s\S]*?)\n---\n([\s\S]*)$/.exec(text);
	if (!m) throw new Error(`${file}: missing frontmatter`);
	const fm: Record<string, string> = {};
	for (const line of m[1]!.split("\n")) {
		const i = line.indexOf(":");
		if (i > 0) fm[line.slice(0, i).trim()] = line.slice(i + 1).trim();
	}
	const list = (v?: string) => (v ? v.split(",").map((s) => s.trim()).filter(Boolean) : []);
	return {
		file,
		name: fm.name!,
		runner: fm.runner ?? "runner-node",
		reference: fm.reference ?? "",
		egress: list(fm.egress),
		secrets: /^\d+$/.test(fm.secrets ?? "") ? [] : list(fm.secrets),
		secretCount: /^\d+$/.test(fm.secrets ?? "") ? Number(fm.secrets) : list(fm.secrets).length,
		expect: fm.expect === "questions" ? "questions" : undefined,
		effects: fm.effects ?? "read",
		spec: m[2]!.trim(),
	};
}

type Job = {
	id: string;
	name: string;
	status: string;
	error?: string;
	startedAt?: string;
	finishedAt?: string;
	answer?: string;
	gates?: { validate: { code: number }; fixtures: { passed: number; failed: number }; scope: { ok: boolean }; diff: { output: string } };
	usage?: { models: Record<string, { totalTokens?: number }> };
	findings?: { id: string; kind: string }[];
};

export type Score = {
	spec: string;
	job: string;
	status: string;
	validate: boolean;
	fixtures: string;
	scope: boolean;
	egress: { got: string[]; extra: string[]; missing: string[] };
	// Secret names are the model's choice unless the spec names them, so
	// only the number of secrets is scored; the names are reported.
	secrets: { got: string[]; want: number; countMatches: boolean };
	questions: string[]; // finding kinds when the job stopped for questions
	effects: { got: string[]; widerThanExpected: boolean };
	decisions: boolean;
	friction: boolean;
	tokens: number;
	minutes: number;
	pass: boolean;
	error?: string;
};

const EFFECT_RANK: Record<string, number> = { read: 0, write: 1, irreversible: 2 };

const compare = (got: string[], want: string[]) => ({
	got,
	extra: got.filter((g) => !want.includes(g)),
	missing: want.filter((w) => !got.includes(w)),
});

export function score(s: EvalSpec, job: Job): Score {
	const g = job.gates;
	const p = permissions(g?.diff.output ?? "");
	const egress = compare(p.egress, s.egress);
	const secrets = { got: p.secrets, want: s.secretCount, countMatches: p.secrets.length === s.secretCount };
	const widerThanExpected = p.effects.some((e) => (EFFECT_RANK[e] ?? 9) > (EFFECT_RANK[s.effects] ?? 0));
	const tokens = Object.values(job.usage?.models ?? {}).reduce((n, u) => n + (u.totalTokens ?? 0), 0);
	const minutes = job.startedAt && job.finishedAt ? (Date.parse(job.finishedAt) - Date.parse(job.startedAt)) / 60000 : 0;
	const r: Score = {
		spec: basename(s.file, ".md"),
		job: job.id,
		status: job.status,
		validate: g?.validate.code === 0,
		fixtures: g ? `${g.fixtures.passed}/${g.fixtures.passed + g.fixtures.failed}` : "-",
		scope: g?.scope.ok ?? false,
		egress,
		secrets,
		effects: { got: p.effects, widerThanExpected },
		questions: job.status === "questions" ? [...new Set((job.findings ?? []).map((f) => f.kind))].sort() : [],
		decisions: /decisions for the user/i.test(job.answer ?? ""),
		friction: /friction log/i.test(job.answer ?? ""),
		tokens,
		minutes: Math.round(minutes * 10) / 10,
		pass: false,
		error: job.error,
	};
	if (s.expect === "questions") {
		// The point is that intake notices the agent has no source for its data.
		r.pass = job.status === "questions" && r.questions.some((k) => ["unsourced", "bulk", "no_tools"].includes(k));
		return r;
	}
	// Extra hosts or secrets are a widening the reviewer must catch; missing
	// ones usually mean the agent could not work. Both fail the eval.
	r.pass =
		job.status === "done" && r.validate && r.scope && (g?.fixtures.failed ?? 1) === 0 &&
		egress.extra.length === 0 && egress.missing.length === 0 &&
		secrets.countMatches &&
		!widerThanExpected && r.decisions;
	return r;
}

function factory(method: string, path: string, body?: unknown): unknown {
	const user = execFileSync("git", ["config", "user.email"], { encoding: "utf8" }).trim() || "eval";
	const args = ["-n", "tap-factory", "exec", "-i", "deploy/factory", "-c", "factory", "--", "curl", "-sS", "-f", "-X", method,
		"-H", `x-tap-user: ${user}`, "-H", "content-type: application/json"];
	if (body !== undefined) args.push("--data-binary", "@-");
	args.push(`http://127.0.0.1:8080/v1/${path}`);
	const out = execFileSync("kubectl", args, { input: body === undefined ? "" : JSON.stringify(body), encoding: "utf8" });
	return JSON.parse(out);
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function table(scores: Score[]): string {
	const rows = [["spec", "status", "validate", "fixtures", "scope", "egress", "secrets", "effects", "decisions", "friction", "questions", "tokens", "min", "pass"]];
	const diff = (c: { extra: string[]; missing: string[] }) =>
		c.extra.length || c.missing.length ? [...c.extra.map((x) => `+${x}`), ...c.missing.map((x) => `-${x}`)].join(" ") : "ok";
	const secrets = (c: Score["secrets"]) =>
		c.countMatches ? (c.got.length ? `ok (${c.got.join(",")})` : "ok") : `${c.got.length} vs ${c.want} (${c.got.join(",") || "none"})`;
	for (const s of scores) {
		rows.push([s.spec, s.status, s.validate ? "✓" : "✗", s.fixtures, s.scope ? "✓" : "✗", diff(s.egress), secrets(s.secrets),
			s.effects.widerThanExpected ? `wider (${s.effects.got.join(",")})` : "ok", s.decisions ? "✓" : "✗", s.friction ? "✓" : "✗",
			s.questions.join(",") || "-", String(s.tokens), String(s.minutes), s.pass ? "PASS" : "FAIL"]);
	}
	const widths = rows[0]!.map((_, i) => Math.max(...rows.map((r) => r[i]!.length)));
	return rows.map((r) => r.map((c, i) => c.padEnd(widths[i]!)).join("  ")).join("\n");
}

async function main() {
	const { values } = parseArgs({ options: { route: { type: "string", default: "agent" }, only: { type: "string" }, score: { type: "string" } } });
	const dir = new URL(".", import.meta.url).pathname;
	const specs = readdirSync(dir)
		.filter((f) => f.endsWith(".md") && (!values.only || f === `${values.only}.md`))
		.map((f) => parseSpec(join(dir, f), readFileSync(join(dir, f), "utf8")));

	let jobs: Job[];
	if (values.score) {
		// Re-score a previous run without the cluster.
		jobs = (JSON.parse(readFileSync(values.score, "utf8")) as { jobs: Job[] }).jobs;
	} else {
		const ids = specs.map((s) => {
			const j = factory("POST", "jobs", {
				name: s.name,
				spec: s.spec,
				runner: s.runner,
				publish: false,
				route: values.route,
				hide: s.reference ? [s.reference] : [],
				// Good specs build on whatever intake drafts; bad ones must stop and ask.
				assume: s.expect !== "questions",
			}) as Job;
			console.error(`submitted ${s.name}: ${j.id}`);
			return j.id;
		});
		const deadline = Date.now() + 4 * 3600_000;
		for (;;) {
			jobs = ids.map((id) => factory("GET", `jobs/${id}`) as Job);
			const open = jobs.filter((j) => !["done", "failed", "aborted", "questions"].includes(j.status));
			if (open.length === 0) break;
			if (Date.now() > deadline) throw new Error(`timed out waiting for ${open.map((j) => j.name).join(", ")}`);
			console.error(`${new Date().toISOString()} waiting: ${open.map((j) => `${j.name}=${j.status}`).join(" ")}`);
			await sleep(30_000);
		}
		// Nobody will answer an eval's questions.
		for (const j of jobs) if (j.status === "questions") factory("POST", `jobs/${j.id}/abort`, {});
	}
	const scores = specs.map((s) => score(s, jobs.find((j) => j.name === s.name) ?? ({ id: "-", name: s.name, status: "missing" } as Job)));
	console.log(table(scores));
	if (!values.score) {
		const out = join(".tap/evals", `${new Date().toISOString().replace(/[:.]/g, "-")}.json`);
		writeFileSync(out, JSON.stringify({ route: values.route, scores, jobs }, null, 2));
		console.error(`results: ${out}`);
	}
	process.exitCode = scores.every((s) => s.pass) ? 0 : 1;
}

if (import.meta.url === `file://${process.argv[1]}`) await main();
