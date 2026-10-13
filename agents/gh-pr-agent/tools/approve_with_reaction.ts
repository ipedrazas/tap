// approve_with_reaction: one request to POST /repos/{owner}/{repo}/issues/{number}/reactions
// with content "+1". The content is fixed: the tool can add a 👍 and nothing else.
import { parseArgs } from "node:util";
import { ghPost, out, reactionsUrl } from "./github.ts";

const { values } = parseArgs({
  options: { owner: { type: "string" }, repo: { type: "string" }, number: { type: "string" } },
  strict: true,
});
const owner = values.owner!;
const repo = values.repo!;
const number = Number(values.number);

const r = await ghPost(reactionsUrl(owner, repo, number), { content: "+1" });
if (!r.ok) {
  out({ ...r.error, owner, repo, number });
} else {
  // 201 adds the reaction; 200 means this account had already reacted.
  out({ reacted: true, already: r.status === 200, owner, repo, number });
}
