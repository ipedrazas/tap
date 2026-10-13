// post_review_comment: one request to POST /repos/{owner}/{repo}/issues/{number}/comments.
// The body is the review in markdown; a footer marks it as the agent's.
import { parseArgs } from "node:util";
import { commentsUrl, ghPost, out } from "./github.ts";

const { values } = parseArgs({
  options: { owner: { type: "string" }, repo: { type: "string" }, number: { type: "string" }, body: { type: "string" } },
  strict: true,
});
const owner = values.owner!;
const repo = values.repo!;
const number = Number(values.number);
const review = (values.body ?? "").trim();

const FOOTER = "\n\n---\n<sub>Automated review by gh-pr-agent. Findings are evidence-backed but unverified by a human.</sub>\n<!-- tap:gh-pr-agent -->";

if (review === "") {
  out({ error: "empty_body", detail: "the review is empty; nothing was posted", owner, repo, number });
} else {
  const r = await ghPost(commentsUrl(owner, repo, number), { body: review + FOOTER });
  if (!r.ok) {
    out({ ...r.error, owner, repo, number });
  } else {
    let d: any = {};
    try {
      d = JSON.parse(r.text);
    } catch {
      // Posted; the response body is only used for the link.
    }
    out({ posted: true, id: d.id ?? null, url: d.html_url ?? null, owner, repo, number });
  }
}
