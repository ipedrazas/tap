// Client for the publisher sidecar (cmd/factory-publisher) on loopback.
import type { AgentFile } from "./gates.ts";

export type PullRequest = { number: number; html_url: string };

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
		if (!res.ok) throw new Error(`publisher ${path}: ${res.status} ${text.trim()}`);
		return JSON.parse(text) as T;
	}

	async head(): Promise<string> {
		return (await this.call<{ sha: string }>("GET", "/v1/head")).sha;
	}

	pr(req: { job: string; agent: string; base: string; title: string; body: string; files: AgentFile[] }): Promise<PullRequest> {
		return this.call("POST", "/v1/pr", {
			...req,
			files: req.files.map((f) => ({ path: f.path, content: f.content.toString("base64"), executable: f.executable || undefined })),
		});
	}
}
