// The brief: what an agent takes, fetches, does and delivers, drafted by the
// model from the spec before anything is built. Specs written for people are
// silently incomplete; checkBrief (plain code, not a model) turns every gap
// into a question, and the build is checked against the approved brief.
import { type Static, Type } from "@earendil-works/pi-ai";
import type { Permissions } from "./permissions.ts";

const basis = Type.String({
	description: 'Why this is so: a short quote from the spec, "api" if you checked it against the live API, or "assumption" if neither says it.',
});

export const BriefSchema = Type.Object({
	purpose: Type.String({ description: "One sentence: what the agent is for." }),
	purpose_basis: basis,
	user_inputs: Type.Array(
		Type.Object({
			name: Type.String({ description: "snake_case name, used as a source: user:<name>" }),
			description: Type.String({ description: "What a person types, e.g. 'a GitHub pull request URL'." }),
			kind: Type.Union([Type.Literal("value"), Type.Literal("document")], {
				description: "value: something short a person knows or copies (URL, id, name, question). document: a body of content they would have to paste (a diff, a file, logs).",
			}),
			basis,
		}),
	),
	tools: Type.Array(
		Type.Object({
			name: Type.String({ description: "snake_case tool name; for an MCP tool, the server's own tool name" }),
			mcp_server: Type.Optional(Type.String({ description: "Set for tools served by a remote MCP server: the server's name in agent.yaml." })),
			does: Type.String({ description: "One sentence." }),
			effects: Type.Union([Type.Literal("read"), Type.Literal("write"), Type.Literal("irreversible")]),
			host: Type.String({ description: 'The host it calls, e.g. "api.github.com" (no scheme or path), or "none" if it calls nothing.' }),
			secret: Type.Optional(Type.String({ description: "UPPER_SNAKE name of the API key it uses, if any." })),
			secret_required: Type.Optional(Type.Boolean({ description: "false if the tool also works without the key (e.g. public data)." })),
			inputs: Type.Array(
				Type.Object({
					name: Type.String(),
					type: Type.Union([Type.Literal("string"), Type.Literal("integer"), Type.Literal("number"), Type.Literal("boolean")]),
					source: Type.String({
						description: 'Where the value comes from: "user:<user_input>", "tool:<tool>.<output>" (another tool\'s output), or "constant". Nothing else exists: no files, no /workspace, no prompt placeholders.',
					}),
				}),
			),
			outputs: Type.Array(Type.String(), { description: "The fields it returns." }),
			basis,
		}),
	),
	delivers: Type.Object({
		to: Type.String({ description: '"chat" if the answer goes back to the person, otherwise the name of the write tool that sends it.' }),
		basis,
	}),
	examples: Type.Array(
		Type.Object({
			user_says: Type.String(),
			tools_called: Type.Array(Type.String()),
			answer_shape: Type.String({ description: "What the answer contains." }),
		}),
		{ description: "At least two realistic exchanges." },
	),
	open_questions: Type.Array(Type.String(), { description: "What you could not resolve from the spec or the API." }),
});

export type Brief = Static<typeof BriefSchema>;

export type FindingKind = "unsourced" | "bulk" | "host" | "secret" | "no_tools" | "effect" | "delivers" | "examples" | "unused" | "assumption" | "model";

export type Finding = {
	id: string;
	kind: FindingKind;
	about: string;
	question: string;
	choices?: string[];
};

const HOST = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:\d{1,5})?$/;
const SECRET = /^[A-Z][A-Z0-9_]{1,63}$/;
// Names that are documents, whatever kind the draft gave them.
const BULKY = /\b(diff|patch|contents?|file|files|logs?|source|document|transcript|body)\b/i;

const words = (s: string) => s.replace(/[_-]+/g, " ");

export function checkBrief(b: Brief): Finding[] {
	const out: Finding[] = [];
	const add = (kind: FindingKind, about: string, question: string, choices?: string[]) => {
		const id = `${kind}:${about}`.replace(/[^A-Za-z0-9_.:-]+/g, "_");
		if (!out.some((f) => f.id === id)) out.push({ id, kind, about, question, ...(choices ? { choices } : {}) });
	};
	const userInputs = new Map(b.user_inputs.map((u) => [u.name, u]));
	const outputs = new Map(b.tools.map((t) => [t.name, new Set(t.outputs)]));

	for (const u of b.user_inputs) {
		if (u.kind === "document" || BULKY.test(words(u.name)) || BULKY.test(u.description)) {
			add("bulk", u.name, `People would have to paste "${u.description}" into the chat. Should the agent fetch it itself instead (from where, given what?), or will people really paste it?`, [
				"The agent fetches it (say from where, given what)",
				"People paste it; keep it as typed input",
			]);
		}
		if (u.basis === "assumption") add("assumption", `input.${u.name}`, `I assumed people give the agent "${u.description}". Is that right?`, ["Yes", "No (say what they give instead)"]);
	}

	if (b.tools.length === 0) {
		add("no_tools", "agent", "The draft has no tools, so the agent can only answer from the model's own knowledge. Is that enough, or what should it look up, and where?", [
			"No tools: the model alone is enough",
			"It needs to look things up (say what and where)",
		]);
	}
	for (const t of b.tools) {
		for (const i of t.inputs) {
			const src = i.source.trim();
			const [kind, rest = ""] = src.split(/:(.*)/s);
			let ok = false;
			if (src === "constant") ok = true;
			else if (kind === "user") ok = userInputs.has(rest);
			else if (kind === "tool") {
				const [tool, field] = rest.split(".");
				ok = tool !== t.name && outputs.get(tool ?? "")?.has(field ?? "") === true;
			}
			if (!ok) {
				add("unsourced", `${t.name}.${i.name}`, `Where does ${t.name}'s "${i.name}" come from? The draft says "${src}", which nothing provides: people only type into the chat, and no one writes files or fills in placeholders for the agent.`, [
					"Add a tool that fetches it (say from where)",
					"The person types it (say what exactly)",
					"Drop it",
				]);
			}
		}
		if (t.host !== "none" && !HOST.test(t.host)) {
			add("host", t.name, `${t.name} calls "${t.host}", which is not a hostname. Which host does it call (e.g. api.example.com)?`);
		}
		if (t.host === "none" && t.secret) add("host", t.name, `${t.name} uses ${t.secret} but calls no host. Which host is the key for?`);
		if (t.secret && !SECRET.test(t.secret)) add("secret", t.name, `${t.name}'s secret "${t.secret}" must be an UPPER_SNAKE name. What should it be called?`);
		if (t.effects !== "read") {
			add("effect", t.name, `${t.name} ${t.effects === "write" ? "changes" : "irreversibly changes"} something outside the agent (${t.does}). Should the agent be allowed to do that?`, [
				"Yes, run it automatically",
				"Yes, but deny it until approvals exist (effectsPolicy: deny)",
				"No: make the agent read-only",
			]);
		}
		if (t.basis === "assumption") add("assumption", `tool.${t.name}`, `I assumed the agent needs ${t.name}: ${t.does}${t.host !== "none" ? ` (via ${t.host})` : ""}. Is that right?`, ["Yes", "No (say what instead)"]);
	}

	if (b.delivers.to !== "chat") {
		add("delivers", "delivers", `The draft sends the result with ${b.delivers.to} instead of answering in the chat. Is that what you want?`, ["Yes", "No: answer in the chat"]);
	}
	if (b.delivers.basis === "assumption") add("assumption", "delivers", `I assumed the result goes to: ${b.delivers.to}. Is that right?`, ["Yes", "No (say where)"]);
	if (b.purpose_basis === "assumption") add("assumption", "purpose", `I read the purpose as: "${b.purpose}". Is that right?`, ["Yes", "No (say what it is for)"]);

	if (b.examples.length < 2) add("examples", "examples", "Give one or two things a person would ask this agent, so the tools and fixtures match real use.");
	const used = new Set(b.examples.flatMap((e) => e.tools_called));
	for (const t of b.tools) if (!used.has(t.name)) add("unused", t.name, `No example uses ${t.name}. When would the agent call it? (Or drop it.)`);

	b.open_questions.forEach((q, i) => add("model", `q${i + 1}`, q));
	return out;
}

// conformance compares the built agent's permissions (tapctl diff) with the
// approved brief: tools, hosts, secrets and effects must match exactly.
export function conformance(b: Brief, got: Permissions): string[] {
	const problems: string[] = [];
	const wantTools = new Map(b.tools.map((t) => [t.mcp_server ? `${t.mcp_server}__${t.name}` : t.name, t.effects]));
	for (const [name, effects] of wantTools) {
		if (!(name in got.tools)) problems.push(`tool ${name} is in the brief but not in the agent`);
		else if (got.tools[name] !== effects) problems.push(`tool ${name} has effects ${got.tools[name]}, the brief says ${effects}`);
	}
	for (const name of Object.keys(got.tools)) if (!wantTools.has(name)) problems.push(`tool ${name} is not in the brief`);
	const hp = (h: string) => (h.includes(":") ? h : `${h}:443`);
	const wantHosts = new Set(b.tools.filter((t) => t.host !== "none").map((t) => hp(t.host.toLowerCase())));
	for (const h of wantHosts) if (!got.egress.includes(h)) problems.push(`host ${h} is in the brief but not in the agent`);
	for (const h of got.egress) if (!wantHosts.has(h)) problems.push(`host ${h} is not in the brief`);
	const wantSecrets = new Set(b.tools.flatMap((t) => (t.secret ? [t.secret] : [])));
	for (const s of wantSecrets) if (!got.secrets.includes(s)) problems.push(`secret ${s} is in the brief but not in the agent`);
	for (const s of got.secrets) if (!wantSecrets.has(s)) problems.push(`secret ${s} is not in the brief`);
	return problems;
}

// render is the brief as Markdown, for the PR description.
export function renderBrief(b: Brief): string {
	const lines = [`**Purpose:** ${b.purpose}`, "", "**People give it:**"];
	for (const u of b.user_inputs) lines.push(`- \`${u.name}\`: ${u.description} (${u.kind})`);
	lines.push("", "**Tools:**");
	for (const t of b.tools) {
		const auth = t.secret ? `, ${t.secret}${t.secret_required === false ? " (optional)" : ""}` : "";
		lines.push(`- \`${t.mcp_server ? `${t.mcp_server}__` : ""}${t.name}\` (${t.effects}; ${t.host}${auth}): ${t.does}`);
		for (const i of t.inputs) lines.push(`  - ${i.name} ← ${i.source}`);
	}
	lines.push("", `**Delivers to:** ${b.delivers.to}`, "", "**Examples:**");
	for (const e of b.examples) lines.push(`- "${e.user_says}" → ${e.tools_called.join(", ") || "no tools"}: ${e.answer_shape}`);
	return lines.join("\n");
}
