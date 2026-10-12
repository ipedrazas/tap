// get_pull_request_diff_page: next chunk of a PR's file list.
// Fetches pages page, page+1, page+2 (stopping early once a page has fewer
// than 100 files). Same annotation and truncation as get_pull_request_diff,
// without the metadata request. Returns next_page so the model can chain
// another call (the 3rd+ chunk's page is this call's returned next_page).
import { parseArgs } from "node:util";
import { CALL_BUDGET, FILE_BUDGET, fetchFilePages, out, processFile } from "./github.ts";

const { values } = parseArgs({
  options: { owner: { type: "string" }, repo: { type: "string" }, number: { type: "string" }, page: { type: "string" } },
  strict: true,
});
const owner = values.owner!;
const repo = values.repo!;
const number = Number(values.number);
const page = Number(values.page);

const res = await fetchFilePages(owner, repo, number, page, 3);
if (!res.ok) {
  out({ ...res.error, owner, repo, number, page });
} else {
  const files: any[] = [];
  const omitted: { filename: string; reason: string }[] = [];
  let remaining = CALL_BUDGET;
  for (const p of res.pages) {
    for (const raw of p.files) {
      const budget = Math.min(FILE_BUDGET, remaining);
      if (budget <= 0) {
        omitted.push({ filename: raw.filename ?? "(unknown)", reason: "over_budget" });
        continue;
      }
      const { file, chars } = processFile(raw, budget);
      if (file.skipped) {
        files.push(file);
        continue;
      }
      remaining -= chars;
      files.push(file);
    }
  }
  const last = res.pages[res.pages.length - 1];
  out({
    files,
    files_returned: files.length,
    next_page: res.lastFull ? last.page + 1 : null,
    omitted,
  });
}