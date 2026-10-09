import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { parseSpec, permissions, score } from "../evals/run.ts";

const dir = new URL("../evals/", import.meta.url).pathname;

test("every eval spec parses and names a new agent", () => {
	const specs = readdirSync(dir).filter((f) => f.endsWith(".md")).map((f) => parseSpec(f, readFileSync(join(dir, f), "utf8")));
	assert.equal(specs.length, 4);
	for (const s of specs) {
		assert.match(s.name, /^[a-z][a-z0-9-]+-agent$/);
		assert.ok(s.spec.length > 100, s.file);
		assert.ok(s.egress.length > 0, s.file);
	}
});

const diff = `countries-eval-agent 0.1.0; effectsPolicy: write=deny irreversible=deny
new agent; every permission is new:
+ tool compare_countries (effects: read)
+ tool get_country (effects: read)
+ secret RESTCOUNTRIES_API_KEY on get_country
+ egress api.restcountries.com:443 on compare_countries
+ egress api.restcountries.com:443 on get_country
permissions widened: human review required`;

test("score compares permissions with the reference", () => {
	assert.deepEqual(permissions(diff), { egress: ["api.restcountries.com:443"], secrets: ["RESTCOUNTRIES_API_KEY"], effects: ["read"] });
	const spec = parseSpec("countries.md", readFileSync(join(dir, "countries.md"), "utf8"));
	const job = {
		id: "j1", name: spec.name, status: "done", answer: "## Decisions for the user\n- v5\n## Friction log\nnone",
		startedAt: "2026-10-09T10:00:00Z", finishedAt: "2026-10-09T10:12:30Z",
		gates: { validate: { code: 0 }, fixtures: { passed: 14, failed: 0 }, scope: { ok: true }, diff: { output: diff } },
		usage: { models: { "kodo/agent": { totalTokens: 1234 } } },
	};
	const s = score(spec, job);
	assert.equal(s.pass, true);
	assert.equal(s.minutes, 12.5);
	assert.equal(s.tokens, 1234);
	const wider = score(spec, { ...job, gates: { ...job.gates, diff: { output: `${diff}\n+ egress evil.example:443 on get_country\n+ tool post_it (effects: write)` } } });
	assert.equal(wider.pass, false);
	assert.deepEqual(wider.egress.extra, ["evil.example:443"]);
	assert.equal(wider.effects.widerThanExpected, true);
});
