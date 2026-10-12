// The console-only routes: agent.yaml reads and edit PRs through the publisher.
import assert from "node:assert/strict";
import { test } from "node:test";
import { api, checkEdit } from "../src/api.ts";
import { type EditRequest, PublisherError } from "../src/publisher.ts";

const BASE = "0123456789abcdef0123456789abcdef01234567";

async function serve() {
	const edits: EditRequest[] = [];
	const publisher = {
		agent: async (name: string) => {
			if (name !== "echo-agent") throw new PublisherError(404, "no agent");
			return { sha: BASE, agentYaml: "metadata: {}\n" };
		},
		edit: async (req: EditRequest) => {
			if (req.egress?.nope) throw new PublisherError(422, "agent has no tool nope");
			if (req.egress?.boom) throw new PublisherError(500, "github down");
			edits.push(req);
			return { number: 9, html_url: "https://github.com/o/r/pull/9" };
		},
	};
	const server = api({ factory: {} as never, store: {} as never, harness: {} as never, routes: [], publisher });
	await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
	const { port } = server.address() as { port: number };
	const call = (method: string, path: string, body?: unknown, user = "alice@hiddenfield.dev") =>
		fetch(`http://127.0.0.1:${port}${path}`, {
			method,
			headers: user ? { "x-tap-user": user, "content-type": "application/json" } : {},
			body: body === undefined ? undefined : JSON.stringify(body),
		});
	return { server, call, edits };
}

test("console routes read agent.yaml and open edit PRs as the user", async () => {
	const { server, call, edits } = await serve();
	try {
		let res = await call("GET", "/console/agents/echo-agent");
		assert.equal(res.status, 200);
		assert.deepEqual(await res.json(), { sha: BASE, agentYaml: "metadata: {}\n" });
		assert.equal((await call("GET", "/console/agents/other-agent")).status, 404);
		assert.equal((await call("GET", "/console/agents/echo-agent", undefined, "")).status, 401);

		res = await call("POST", "/console/agents/echo-agent/pr", { base: BASE, title: "Echo", egress: { echo: ["a.com:443"] } });
		assert.equal(res.status, 200);
		assert.deepEqual(await res.json(), { number: 9, html_url: "https://github.com/o/r/pull/9" });
		assert.deepEqual(edits, [{ agent: "echo-agent", base: BASE, user: "alice@hiddenfield.dev", title: "Echo", egress: { echo: ["a.com:443"] } }]);

		assert.equal((await call("POST", "/console/agents/echo-agent/pr", { base: BASE, egress: { nope: [] } })).status, 422);
		assert.equal((await call("POST", "/console/agents/echo-agent/pr", { base: BASE, egress: { boom: [] } })).status, 502);
		assert.equal((await call("POST", "/console/agents/echo-agent/pr", { base: "main", title: "x" })).status, 422);
		assert.equal((await call("POST", "/console/agents/echo-agent/pr", { base: BASE, title: "x", agent: "other" })).status, 422);
		// The agent comes from the path and the user from the header, never the body.
		assert.equal(edits.length, 1);
	} finally {
		server.close();
	}
});

test("checkEdit", () => {
	assert.equal(checkEdit({ base: BASE, title: "" }), undefined);
	assert.equal(checkEdit({ base: BASE, egress: { t: [] } }), undefined);
	assert.match(checkEdit({ base: BASE }) ?? "", /nothing to change/);
	assert.match(checkEdit({ base: BASE, egress: { t: "a.com:443" } }) ?? "", /list of strings/);
	assert.match(checkEdit({ base: BASE, egress: [] }) ?? "", /egress must be/);
	assert.match(checkEdit({ base: BASE, title: 3 }) ?? "", /title/);
	assert.match(checkEdit([]) ?? "", /object/);
});
