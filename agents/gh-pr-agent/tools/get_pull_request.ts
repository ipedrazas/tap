// get_pull_request: one request to GET /repos/{owner}/{repo}/pulls/{number}.
import { parseArgs } from "node:util";
import { gh, out, pullsUrl } from "./github.ts";

const { values } = parseArgs({
  options: { owner: { type: "string" }, repo: { type: "string" }, number: { type: "string" } },
  strict: true,
});
const owner = values.owner!;
const repo = values.repo!;
const number = Number(values.number);

const BODY_CAP = 4000;

const r = await gh(pullsUrl(owner, repo, number));
if (!r.ok) {
  out({ ...r.error, owner, repo, number });
} else {
  let d: any;
  try {
    d = JSON.parse(r.text);
  } catch {
    out({ error: "upstream", detail: "pull response was not JSON", owner, repo, number });
  }
  const body = typeof d.body === "string" ? d.body : "";
  out({
    title: d.title ?? null,
    body: body.slice(0, BODY_CAP),
    body_truncated: body.length > BODY_CAP,
    author: d.user?.login ?? null,
    state: d.state ?? null,
    draft: d.draft ?? false,
    merged: d.merged ?? false,
    base_ref: d.base?.ref ?? null,
    base_sha: d.base?.sha ?? null,
    head_ref: d.head?.ref ?? null,
    head_sha: d.head?.sha ?? null,
    head_repo_full_name: d.head?.repo?.full_name ?? null,
    additions: d.additions ?? 0,
    deletions: d.deletions ?? 0,
    changed_files: d.changed_files ?? 0,
    labels: Array.isArray(d.labels) ? d.labels.map((l: any) => l.name).filter((n: unknown): n is string => typeof n === "string") : [],
  });
}