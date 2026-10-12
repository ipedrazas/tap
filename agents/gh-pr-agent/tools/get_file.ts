// get_file: read a numbered window of one file at a ref.
// GET /repos/{owner}/{repo}/contents/{path}?ref={ref} with the raw accept;
// the body is the whole file and the tool returns only the requested window
// (at most 400 lines). Directories, binaries and deleted files get a JSON
// error, not a crash.
import { parseArgs } from "node:util";
import { contentsUrl, gh, out, pathError } from "./github.ts";

const { values } = parseArgs({
  options: { owner: { type: "string" }, repo: { type: "string" }, path: { type: "string" }, ref: { type: "string" }, start_line: { type: "string" }, end_line: { type: "string" } },
  strict: true,
});
const MAX_WINDOW = 400;

async function main(): Promise<void> {
  const owner = values.owner!;
  const repo = values.repo!;
  const path = values.path!;
  const ref = values.ref!;
  const startLine = Number(values.start_line);
  const endLine = Number(values.end_line);

  const pe = pathError(path);
  if (pe !== null) {
    out({ error: "invalid_path", detail: pe, path });
    return;
  }

  const r = await gh(contentsUrl(owner, repo, path, ref), /* raw = */ true, /* perRequestMs = */ 20_000);
  if (!r.ok) {
    const detail = r.error.error === "not_found"
      ? "file or ref not found on " + owner + "/" + repo + "; if this is a fork PR, the head sha lives on the head repository - call get_file again with the head repo's owner and repo (head_repo_full_name from get_pull_request)"
      : undefined;
    out({ ...r.error, ...(detail ? { detail } : {}), path, ref });
    return;
  }
  const text = r.text;

  // A directory listing comes back as a JSON array even with the raw accept.
  if (text.trimStart().startsWith("[")) {
    try {
      const arr = JSON.parse(text);
      if (Array.isArray(arr) && arr.length > 0 && typeof arr[0] === "object" && arr[0] !== null && "type" in (arr[0] as object)) {
        out({ error: "directory", path, ref, detail: `${path} is a directory, not a file` });
        return;
      }
    } catch {
      // not JSON: an actual file whose content starts with "["
    }
  }

  // Binary files come back with NUL bytes; refuse to render them.
  if (text.slice(0, 8192).includes("\u0000")) {
    out({ error: "binary", path, ref, detail: `${path} is a binary file` });
    return;
  }

  const lines = text.split("\n");
  if (lines.length > 0 && lines[lines.length - 1] === "") lines.pop();
  const totalLines = lines.length;

  const start = Math.max(1, Math.min(startLine, totalLines === 0 ? 1 : totalLines));
  let end = Math.min(endLine, totalLines);
  if (end - start + 1 > MAX_WINDOW) end = start + MAX_WINDOW - 1;
  const windowLines = [];
  for (let n = start; n <= end; n++) {
    windowLines.push({ n, text: lines[n - 1] ?? "" });
  }
  out({ lines: windowLines, total_lines: totalLines, window_start: start, window_end: end });
}

await main();