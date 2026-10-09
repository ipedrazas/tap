// The factory's instructions are the new-agent skill itself, read from the
// job's checkout (so a PR that improves the skill improves the factory),
// preceded by an addendum for unattended runs.
import { readFileSync } from "node:fs";
import { join } from "node:path";

const addendum = readFileSync(new URL("./addendum.md", import.meta.url), "utf8");

export type JobParams = { name: string; runner: string; owner: string };

export function instructions(repoDir: string, p: JobParams): string {
	const skill = readFileSync(join(repoDir, ".claude/skills/new-agent/SKILL.md"), "utf8").replace(/^---\n[\s\S]*?\n---\n/, "");
	const head = addendum.replaceAll("{{name}}", p.name).replaceAll("{{runner}}", p.runner).replaceAll("{{owner}}", p.owner);
	return `${head}\n\n---\n\n# The new-agent skill\n\n${skill}`;
}

export function task(spec: string): string {
	return `Build the agent described in this spec.\n\n<spec>\n${spec}\n</spec>`;
}
