// task factory:eval: re-run the phase 4–5 specs against the deployed
// factory as dry runs (no PR) and score each result against its reference
// agent. The factory is reached with kubectl exec, like task factory:submit.
//
//	node --experimental-strip-types factory/evals/run.ts [--route agent] [--only hn] [--score <results.json>]
import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";
import { parseArgs } from "node:util";

export type EvalSpec = {
	file: string;
	name: string;
	runner: string;
	reference: string;
	egress: string[];
	secrets: string[];
	effects: string;
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
		secrets: list(fm.secrets),
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
};

export type Score = {
	spec: string;
	job: string;
	status: string;
	validate: boolean;
	fixtures: string;
	scope: boolean;
	egress: { got: string[]; extra: string[]; missing: string[] };
	secrets: { got: string[]; extra: string[]; missing: string[] };
	effects: { got: string[]; widerThanExpected: boolean };
	decisions: boolean;
	friction: boolean;
	tokens: number;
	minutes: number;
	pass: boolean;
	error?: string;
};

const EFFECT_RANK: Record<string, number> = { read: 0, write: 1, irreversible: 2 };

// permissions reads the "+ ..." lines of `tapctl diff` for a new agent.
export function permissions(diff: string) {
	const egress = new Set<string>();
	const secrets = new Set<string>();
	const effects = new Set<string>();
	for (const line of diff.split("\n")) {
		let m = /^\+ egress (\S+) on /.exec(line);
		if (m) egress.add(m[1]!);
		m = /^\+ secret (\S+) on /.exec(line);
		if (m) secrets.add(m[1]!);
		m = /^\+ tool \S+ \(effects: (\w+)\)/.exec(line);
		if (m) effects.add(m[1]!);
	}
	return { egress: [...egress].sort(), secrets: [...secrets].sort(), effects: [...effects].sort() };
}

const compare = (got: string[], want: string[]) => ({
	got,
	extra: got.filter((g) => !want.includes(g)),
	missing: want.filter((w) => !got.includes(w)),
});

export function score(s: EvalSpec, job: Job): Score {
	const g = job.gates;
	const p = permissions(g?.diff.output ?? "");
	const egress = compare(p.egress, s.egress);
	const secrets = compare(p.secrets, s.secrets);
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
		decisions: /decisions for the user/i.test(job.answer ?? ""),
		friction: /friction log/i.test(job.answer ?? ""),
		tokens,
		minutes: Math.round(minutes * 10) / 10,
		pass: false,
		error: job.error,
	};
	// Extra hosts or secrets are a widening the reviewer must catch; missing
	// ones usually mean the agent could not work. Both fail the eval.
	r.pass =
		job.status === "done" && r.validate && r.scope && (g?.fixtures.failed ?? 1) === 0 &&
		egress.extra.length === 0 && egress.missing.length === 0 &&
		secrets.extra.length === 0 && secrets.missing.length === 0 &&
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
	const rows = [["spec", "status", "validate", "fixtures", "scope", "egress", "secrets", "effects", "decisions", "friction", "tokens", "min", "pass"]];
	const diff = (c: { extra: string[]; missing: string[] }) =>
		c.extra.length || c.missing.length ? [...c.extra.map((x) => `+${x}`), ...c.missing.map((x) => `-${x}`)].join(" ") : "ok";
	for (const s of scores) {
		rows.push([s.spec, s.status, s.validate ? "✓" : "✗", s.fixtures, s.scope ? "✓" : "✗", diff(s.egress), diff(s.secrets),
			s.effects.widerThanExpected ? `wider (${s.effects.got.join(",")})` : "ok", s.decisions ? "✓" : "✗", s.friction ? "✓" : "✗",
			String(s.tokens), String(s.minutes), s.pass ? "PASS" : "FAIL"]);
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
			const j = factory("POST", "jobs", { name: s.name, spec: s.spec, runner: s.runner, publish: false, route: values.route }) as Job;
			console.error(`submitted ${s.name}: ${j.id}`);
			return j.id;
		});
		const deadline = Date.now() + 4 * 3600_000;
		for (;;) {
			jobs = ids.map((id) => factory("GET", `jobs/${id}`) as Job);
			const open = jobs.filter((j) => !["done", "failed", "aborted"].includes(j.status));
			if (open.length === 0) break;
			if (Date.now() > deadline) throw new Error(`timed out waiting for ${open.map((j) => j.name).join(", ")}`);
			console.error(`${new Date().toISOString()} waiting: ${open.map((j) => `${j.name}=${j.status}`).join(" ")}`);
			await sleep(30_000);
		}
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
