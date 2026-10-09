// Briefs shared by the tests.
import type { Brief } from "../src/brief.ts";

// What a good brief for the stand-in agent in job.test.ts looks like: one
// read tool, no host, sourced inputs, two examples. Matches the fake tapctl diff.
export const thingBrief: Brief = {
	purpose: "Returns a thing.",
	purpose_basis: "returns a thing",
	user_inputs: [{ name: "thing_id", description: "the id of a thing", kind: "value", basis: "returns a thing" }],
	tools: [
		{
			name: "get_thing",
			does: "Looks up a thing by id.",
			effects: "read",
			host: "none",
			inputs: [{ name: "id", type: "string", source: "user:thing_id" }],
			outputs: ["name"],
			basis: "returns a thing",
		},
	],
	delivers: { to: "chat", basis: "returns a thing" },
	examples: [
		{ user_says: "What is thing 1?", tools_called: ["get_thing"], answer_shape: "its name" },
		{ user_says: "Name of thing 2?", tools_called: ["get_thing"], answer_shape: "its name" },
	],
	open_questions: [],
};

// pr-review-agent as the factory drafted it from a reviewer prompt: the diff
// and context come from /workspace files nobody writes.
export const promptOnlyReviewBrief: Brief = {
	purpose: "Reviews pull requests for security, correctness, reliability and performance issues.",
	purpose_basis: "performing automated Pull Request (PR) reviews",
	user_inputs: [
		{ name: "title", description: "the PR title", kind: "value", basis: "{PR_TITLE}" },
		{ name: "git_diff", description: "the git diff of the PR", kind: "value", basis: "{GIT_DIFF}" },
	],
	tools: [
		{
			name: "review_pr",
			does: "Loads and validates PR data for analysis.",
			effects: "read",
			host: "none",
			inputs: [
				{ name: "title", type: "string", source: "user:title" },
				{ name: "diff_path", type: "string", source: "/workspace/diff.txt" },
				{ name: "repository_context_path", type: "string", source: "workspace file" },
			],
			outputs: ["pr"],
			basis: "assumption",
		},
	],
	delivers: { to: "chat", basis: "Return valid JSON only" },
	examples: [{ user_says: "Review this PR", tools_called: ["review_pr"], answer_shape: "findings JSON" }],
	open_questions: [],
};

// The same agent designed properly: it fetches the PR itself.
export const reviewBrief: Brief = {
	purpose: "Reviews a GitHub pull request for security, correctness, reliability and performance defects.",
	purpose_basis: "review a pull request",
	user_inputs: [{ name: "pr_url", description: "a GitHub pull request URL", kind: "value", basis: "given a PR URL" }],
	tools: [
		{
			name: "get_pull_request",
			does: "Fetches a PR's title, description, changed files and diff from the GitHub REST API.",
			effects: "read",
			host: "api.github.com",
			secret: "GITHUB_TOKEN",
			secret_required: false,
			inputs: [{ name: "url", type: "string", source: "user:pr_url" }],
			outputs: ["title", "body", "files", "diff"],
			basis: "api",
		},
	],
	delivers: { to: "chat", basis: "answer in chat" },
	examples: [
		{ user_says: "Review https://github.com/o/r/pull/1", tools_called: ["get_pull_request"], answer_shape: "up to five findings" },
		{ user_says: "Anything risky in o/r#7?", tools_called: ["get_pull_request"], answer_shape: "findings or none" },
	],
	open_questions: [],
};
