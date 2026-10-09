// The factory's instructions are the new-agent skill itself, read from the
// job's checkout (so a PR that improves the skill improves the factory),
// preceded by an addendum for unattended runs. The job then drives two
// phases with its messages: intake (draft the brief) and build.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Brief, Finding } from "./brief.ts";
import type { QA } from "./jobs.ts";

const addendum = readFileSync(new URL("./addendum.md", import.meta.url), "utf8");

export type JobParams = { name: string; runner: string; owner: string };

export function instructions(repoDir: string, p: JobParams): string {
	const skill = readFileSync(join(repoDir, ".claude/skills/new-agent/SKILL.md"), "utf8").replace(/^---\n[\s\S]*?\n---\n/, "");
	const head = addendum.replaceAll("{{name}}", p.name).replaceAll("{{runner}}", p.runner).replaceAll("{{owner}}", p.owner);
	return `${head}\n\n---\n\n# The new-agent skill\n\n${skill}`;
}

export function intakeTask(spec: string): string {
	return [
		"Phase 1, intake. Read the spec below and draft the brief for this agent: what people give it, each tool and where every one of its inputs comes from, the hosts and secrets, the effects, where results go, and two or more example exchanges.",
		"Look at the real API (curl) where it settles a host, an auth scheme or a field name. Mark anything the spec doesn't say and you didn't check as an assumption, and list what you can't resolve under open_questions.",
		"Don't write the agent yet. Finish by calling submit_brief.",
		"",
		"<spec>",
		spec,
		"</spec>",
	].join("\n");
}

export function answersTask(qa: QA[], proceed: boolean): string {
	const lines = ["The person answered some of the questions about your brief:", ""];
	for (const q of qa) lines.push(`- Q: ${q.question}`, `  A: ${q.answer}`);
	if (qa.length === 0) lines.push("- (no answers)");
	lines.push("");
	lines.push(
		proceed
			? "They chose to proceed: anything still open is yours to decide; keep it conservative and mark it as an assumption. Revise the brief and call submit_brief again."
			: "Revise the brief to reflect the answers and call submit_brief again. Don't write the agent yet.",
	);
	return lines.join("\n");
}

export function buildTask(brief: Brief, qa: QA[], accepted: Finding[]): string {
	const lines = [
		"Phase 2, build. The brief below is approved. Build exactly it: the same tools (names and effects), hosts and secrets. The factory checks the agent against the brief and fails the job if they differ.",
		"If you find the brief can't work as written, don't change the design: stop and explain why in your final answer.",
		"",
		"<brief>",
		JSON.stringify(brief, null, 2),
		"</brief>",
	];
	if (qa.length) {
		lines.push("", "Answers the person gave during intake:");
		for (const q of qa) lines.push(`- Q: ${q.question}`, `  A: ${q.answer}`);
	}
	if (accepted.length) {
		lines.push("", "Gaps you are building on as assumptions (list them under Decisions for the user):");
		for (const f of accepted) lines.push(`- ${f.question}`);
	}
	return lines.join("\n");
}
