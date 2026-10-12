// get_pull_request_file_patch: full hunk-annotated patch of exactly one file.
// GitHub ignores a ?path= query on the files endpoint (verified against the
// live API), so this pages the file list and selects the exact filename
// client-side, at most 15 requests. The single file gets the whole ~60k char
// call budget, so an 8k-truncated file from the diff tools comes back whole.
import { parseArgs } from "node:util";
import { CALL_BUDGET, fetchFilePages, out, pathError, processFile } from "./github.ts";

const { values } = parseArgs({
  options: { owner: { type: "string" }, repo: { type: "string" }, number: { type: "string" }, path: { type: "string" } },
  strict: true,
});

async function main(): Promise<void> {
  const owner = values.owner!;
  const repo = values.repo!;
  const number = Number(values.number);
  const path = values.path!;

  const pe = pathError(path);
  if (pe !== null) {
    out({ error: "invalid_path", detail: pe, path });
    return;
  }

  // Fetch pages one at a time (up to 15 requests) until the file is found or
  // the list is exhausted (a page with fewer than 100 files ends the list).
  for (let page = 1; page <= 15; page++) {
    const r = await fetchFilePages(owner, repo, number, page, 1);
    if (!r.ok) {
      out({ ...r.error, owner, repo, number, path });
      return;
    }
    const filesOnPage = r.pages[0]?.files ?? [];
    for (const raw of filesOnPage) {
      if (raw.filename === path) {
        const { file } = processFile(raw, CALL_BUDGET);
        if (file.skipped) {
          out({ error: "skipped", reason: file.reason, path, detail: "this file has no patch on GitHub (binary, too large, or generated/vendored)" });
        } else {
          out({ file });
        }
        return;
      }
    }
    if (filesOnPage.length < 100) {
      out({ error: "not_found", path, detail: `no changed file named "${path}" in this PR` });
      return;
    }
  }
  out({ error: "not_found", path, detail: `file not found within the first 15 pages (1500 files) of this PR's file list` });
}

await main();