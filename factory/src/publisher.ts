// Client for the publisher sidecar (cmd/factory-publisher) on loopback.
import type { AgentFile } from "./gates.ts";

export type PullRequest = { number: number; html_url: string };

// AgentYaml is an agent's agent.yaml at the head of the base branch.
export type AgentYaml = { sha: string; agentYaml: string };

// EditRequest asks the publisher for a PR that changes an existing agent's
// tool egress and/or metadata.title (see pkg/factory/edit.go).
export type EditRequest = { agent: string; base: string; user: string; title?: string; egress?: Record<string, string[]> };

// PublisherError carries the publisher's HTTP status so callers can pass it on.
export class PublisherError extends Error {
	status: number;
	constructor(status: number, message: string) {
		super(message);
		this.status = status;
	}
}

export class Publisher {
	private url: string;
	private token: string;
	constructor(url: string, token: string) {
		this.url = url;
		this.token = token;
	}

	private async call<T>(method: string, path: string, body?: unknown): Promise<T> {
		const res = await fetch(this.url + path, {
			method,
			headers: { authorization: `Bearer ${this.token}`, "content-type": "application/json" },
			body: body === undefined ? undefined : JSON.stringify(body),
		});
		const text = await res.text();
		if (!res.ok) throw new PublisherError(res.status, `publisher ${path}: ${res.status} ${text.trim()}`);
		return JSON.parse(text) as T;
	}

	async head(): Promise<string> {
		return (await this.call<{ sha: string }>("GET", "/v1/head")).sha;
	}

	agent(name: string): Promise<AgentYaml> {
		return this.call("GET", `/v1/agents/${encodeURIComponent(name)}`);
	}

	edit(req: EditRequest): Promise<PullRequest> {
		return this.call("POST", "/v1/edit", req);
	}

	pr(req: { job: string; agent: string; base: string; title: string; body: string; files: AgentFile[] }): Promise<PullRequest> {
		return this.call("POST", "/v1/pr", {
			...req,
			files: req.files.map((f) => ({ path: f.path, content: f.content.toString("base64"), executable: f.executable || undefined })),
		});
	}
}
