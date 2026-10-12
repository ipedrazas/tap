// get_pull_request_diff: first chunk of a PR's file list.
// Fetches pages 1..3 (until a page has fewer than 100 files), plus PR metadata
// in parallel for files_total. Annotates every patch line with new-file line
// numbers, truncates at hunk boundaries over budget, marks generated/vendored
// and patchless files as skipped, and reports next_page/omitted so the model
// knows what it has not seen yet.
import { parseArgs } from "node:util";
import { CALL_BUDGET, FILE_BUDGET, fetchFilePages, gh, out, processFile, pullsUrl } from "./github.ts";

const { values } = parseArgs({
  options: { owner: { type: "string" }, repo: { type: "string" }, number: { type: "string" } },
  strict: true,
});
const owner = values.owner!;
const repo = values.repo!;
const number = Number(values.number);

const [pagesRes, metaRes] = await Promise.all([
  fetchFilePages(owner, repo, number, 1, 3),
  gh(pullsUrl(owner, repo, number)),
]);

if (!pagesRes.ok) {
  out({ ...pagesRes.error, owner, repo, number });
} else {
  const filesTotal = metaRes.ok
    ? (() => {
        try {
          const m = JSON.parse(metaRes.text);
          return typeof m.changed_files === "number" ? m.changed_files : null;
        } catch {
          return null;
        }
      })()
    : null;

  // Walk files in page order, honoring the per-call and per-file patch budgets.
  const files: any[] = [];
  const omitted: { filename: string; reason: string }[] = [];
  let remaining = CALL_BUDGET;
  for (const page of pagesRes.pages) {
    for (const raw of page.files) {
      const budget = Math.min(FILE_BUDGET, remaining);
      if (budget <= 0) {
        omitted.push({ filename: raw.filename ?? "(unknown)", reason: "over_budget" });
        continue;
      }
      const { file, chars } = processFile(raw, budget);
      if (file.skipped) {
        files.push(file); // listed so the model knows it exists, with a reason
        continue;
      }
      remaining -= chars;
      files.push(file);
    }
  }

  const last = pagesRes.pages[pagesRes.pages.length - 1];
  out({
    files,
    files_returned: files.length,
    files_total: filesTotal,
    next_page: pagesRes.lastFull ? last.page + 1 : null,
    omitted,
  });
}