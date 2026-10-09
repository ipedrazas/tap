import assert from "node:assert/strict";
import { test } from "node:test";
import { checkBrief, conformance, renderBrief } from "../src/brief.ts";
import { permissions } from "../src/permissions.ts";
import { promptOnlyReviewBrief, reviewBrief, thingBrief } from "./fixtures.ts";

const kinds = (b: typeof thingBrief) => checkBrief(b).map((f) => f.id);

test("a well-formed brief has no findings", () => {
	assert.deepEqual(checkBrief(thingBrief), []);
	assert.deepEqual(checkBrief(reviewBrief), []);
});

test("the pr-review-agent design is caught at intake", () => {
	const ids = kinds(promptOnlyReviewBrief);
	assert.ok(ids.includes("unsourced:review_pr.diff_path"), ids.join(" "));
	assert.ok(ids.includes("unsourced:review_pr.repository_context_path"));
	assert.ok(ids.includes("bulk:git_diff"), "a diff is a document, whatever kind the draft says");
	assert.ok(ids.includes("assumption:tool.review_pr"));
	assert.ok(ids.includes("examples:examples"));
	const f = checkBrief(promptOnlyReviewBrief).find((x) => x.id === "unsourced:review_pr.diff_path")!;
	assert.match(f.question, /nothing provides/);
	assert.ok(f.choices?.some((c) => /fetches/.test(c)));
});

test("sources must name a real input or another tool's output", () => {
	const b = structuredClone(reviewBrief);
	b.tools.push({ name: "summarise", does: "x", effects: "read", host: "none", inputs: [{ name: "d", type: "string", source: "tool:get_pull_request.diff" }, { name: "n", type: "string", source: "tool:get_pull_request.nope" }, { name: "s", type: "string", source: "tool:summarise.d" }, { name: "c", type: "integer", source: "constant" }], outputs: ["d"], basis: "api" });
	b.examples[0]!.tools_called.push("summarise");
	assert.deepEqual(kinds(b), ["unsourced:summarise.n", "unsourced:summarise.s"]);
});

test("writes, other destinations, bad hosts and open questions become questions", () => {
	const b = structuredClone(reviewBrief);
	b.tools.push({ name: "post_comment", does: "Posts the review as a PR comment.", effects: "write", host: "https://api.github.com/repos", secret: "github-token", inputs: [], outputs: [], basis: "assumption" });
	b.delivers = { to: "post_comment", basis: "assumption" };
	b.open_questions = ["Which repositories?"];
	assert.deepEqual(kinds(b).sort(), [
		"assumption:delivers",
		"assumption:tool.post_comment",
		"delivers:delivers",
		"effect:post_comment",
		"host:post_comment",
		"model:q1",
		"secret:post_comment",
		"unused:post_comment",
	]);
});

test("an agent with no tools is a question", () => {
	assert.deepEqual(kinds({ ...thingBrief, tools: [], examples: thingBrief.examples.map((e) => ({ ...e, tools_called: [] })) }), ["no_tools:agent"]);
});

test("conformance compares the built agent with the brief", () => {
	const diff = `+ tool get_pull_request (effects: read)
+ secret GITHUB_TOKEN on get_pull_request
+ egress api.github.com:443 on get_pull_request`;
	assert.deepEqual(conformance(reviewBrief, permissions(diff)), []);
	const drift = `+ tool get_pull_request (effects: write)
+ tool extra (effects: read)
+ egress api.github.com:443 on get_pull_request
+ egress evil.example:443 on extra`;
	assert.deepEqual(conformance(reviewBrief, permissions(drift)), [
		"tool get_pull_request has effects write, the brief says read",
		"tool extra is not in the brief",
		"host evil.example:443 is not in the brief",
		"secret GITHUB_TOKEN is in the brief but not in the agent",
	]);
	// MCP tools are named <server>__<tool> by tapctl.
	const mcp = { ...thingBrief, tools: [{ ...thingBrief.tools[0]!, name: "ask", mcp_server: "deepwiki", host: "mcp.deepwiki.com" }] };
	assert.deepEqual(conformance(mcp, permissions("+ tool deepwiki__ask (effects: read)\n+ egress mcp.deepwiki.com:443 on mcp:deepwiki")), []);
});

test("renderBrief shows where inputs come from", () => {
	const md = renderBrief(reviewBrief);
	assert.match(md, /url ← user:pr_url/);
	assert.match(md, /GITHUB_TOKEN \(optional\)/);
});
