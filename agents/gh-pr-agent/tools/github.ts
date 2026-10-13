// Shared plumbing for gh-pr-agent's GitHub REST API tools.
// All tools stay on api.github.com; raw file bodies come straight from the
// contents endpoint (no redirects to raw.githubusercontent.com).

export const BASE = "https://api.github.com";
export const UA = "gh-pr-agent/0.1 (tap agent)";
const TOKEN = process.env.GITHUB_TOKEN ?? "";

// Per-call patch budgets (chars of raw patch text).
export const CALL_BUDGET = 60_000;
export const FILE_BUDGET = 8_000;

export function headers(raw: boolean): Record<string, string> {
  const h: Record<string, string> = {
    "User-Agent": UA,
    Accept: raw ? "application/vnd.github.raw+json" : "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
  };
  // Optional token: send only when set and non-empty. Public repos work without it.
  if (TOKEN !== "") h.Authorization = `Bearer ${TOKEN}`;
  return h;
}

export type GhResult =
  | { ok: true; status: number; text: string }
  | { ok: false; error: Record<string, unknown> };

export async function gh(url: URL, raw = false, perRequestMs = 15_000): Promise<GhResult> {
  return send(url, { headers: headers(raw) }, perRequestMs);
}

// ghPost sends a JSON body. Writes need a token: anonymous calls get 401.
export async function ghPost(url: URL, body: unknown, perRequestMs = 15_000): Promise<GhResult> {
  return send(url, { method: "POST", headers: { ...headers(false), "Content-Type": "application/json" }, body: JSON.stringify(body) }, perRequestMs);
}

async function send(url: URL, init: RequestInit, perRequestMs: number): Promise<GhResult> {
  let res: Response;
  try {
    res = await fetch(url, { ...init, signal: AbortSignal.timeout(perRequestMs) });
  } catch (e) {
    return { ok: false, error: { error: "upstream_unreachable", detail: String((e as Error)?.message ?? e) } };
  }
  const text = await res.text();
  if (res.status === 404) {
    return { ok: false, error: { error: "not_found", detail: firstLine(text).slice(0, 160) } };
  }
  if (res.status === 403) {
    // 403 with a consumed rate limit: report reset time so the model can tell
    // the person when to retry. Any other 403 is a permissions problem.
    if (res.headers.get("x-ratelimit-remaining") === "0") {
      const reset = Number(res.headers.get("x-ratelimit-reset") ?? 0);
      return { ok: false, error: { error: "rate_limited", reset, detail: "anonymous GitHub quota exhausted; the token is not set or was not sent" } };
    }
    return { ok: false, error: { error: "forbidden", detail: "no access to this resource with the current token (writes need a token with pull request write access)" } };
  }
  if (res.status === 401) {
    return { ok: false, error: { error: "unauthorized", detail: "token missing or rejected by GitHub" } };
  }
  if (!res.ok) {
    return { ok: false, error: { error: "upstream", status: res.status, detail: firstLine(text).slice(0, 160) } };
  }
  return { ok: true, status: res.status, text };
}

export function out(v: unknown): void {
  process.stdout.write(JSON.stringify(v));
}

export function firstLine(s: string): string {
  const i = s.indexOf("\n");
  return i < 0 ? s : s.slice(0, i);
}

// --- URL builders -----------------------------------------------------------

export function pullsUrl(owner: string, repo: string, number: number): URL {
  return new URL(`${BASE}/repos/${owner}/${repo}/pulls/${number}`);
}

// PR conversation comments are issue comments.
export function commentsUrl(owner: string, repo: string, number: number): URL {
  return new URL(`${BASE}/repos/${owner}/${repo}/issues/${number}/comments`);
}

export function filesUrl(owner: string, repo: string, number: number, page: number): URL {
  const u = new URL(`${BASE}/repos/${owner}/${repo}/pulls/${number}/files`);
  u.searchParams.set("per_page", "100");
  u.searchParams.set("page", String(page));
  return u;
}

export function contentsUrl(owner: string, repo: string, path: string, ref: string): URL {
  // Slashes are path separators; encode each segment so "/" stays literal.
  const enc = path.split("/").map((s) => encodeURIComponent(s)).join("/");
  const u = new URL(`${BASE}/repos/${owner}/${repo}/contents/${enc}`);
  u.searchParams.set("ref", ref);
  return u;
}

// --- Skipped-file detection -------------------------------------------------

// True when the file is generated or vendored output nobody reviews by hand.
export function generatedReason(filename: string): string | null {
  const base = filename.split("/").pop() ?? "";
  if (/\.min\.(js|css|mjs|cjs)$/i.test(filename)) return "generated";
  if (/\.(map|d\.ts\.map)$/i.test(filename)) return "generated";
  if (/(^|\/)__snapshots__\//.test(filename) || /\.snap$/.test(filename)) return "generated";
  if (/(^|\/)vendor\//i.test(filename)) return "vendored";
  if (/(^|\/)dist\//i.test(filename)) return "generated";
  if (/(^|\/)build\//i.test(filename)) return "generated";
  if (/(^|\/)node_modules\//i.test(filename)) return "vendored";
  if (/(^|\/)\.next\//i.test(filename)) return "generated";
  if (/\.(pb|pb\.gw)\.go$/i.test(filename)) return "generated";
  if (/\.(g\.ts|generated\.ts)$/i.test(filename)) return "generated";
  if (/^(?:package-lock\.json|pnpm-lock\.yaml|yarn\.lock|yarn\.locked|composer\.lock|Cargo\.lock|Gemfile\.lock|poetry\.lock|Pipfile\.lock|flake\.lock|uv\.lock|go\.sum)$/i.test(base)) return "generated";
  return null;
}

// --- Hunk annotation --------------------------------------------------------

export type AnnotatedLine = { n: number | null; marker: string; text: string };

export type Annotated = {
  lines: AnnotatedLine[];
  truncated: boolean;
  chars: number; // raw chars consumed from the patch (against the budget)
  first_line: number;
  last_line: number;
};

const HUNK_RE = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/;

// Parse a unified diff patch, prefixing every content line with its line number
// in the NEW file (removed lines get the OLD line number and marker "-") using
// hunk-header arithmetic, so citations are right without the model doing math.
// Truncates at hunk boundaries once `budget` raw chars are consumed.
export function annotatePatch(patch: string, budget: number): Annotated {
  const rawLines = patch.split("\n");
  // Group lines into hunks: a hunk is its "@@" header plus the lines after it.
  const hunks: string[][] = [];
  let cur: string[] = [];
  for (const ln of rawLines) {
    if (ln.startsWith("@@ ")) {
      if (cur.length > 0) hunks.push(cur);
      cur = [ln];
    } else if (ln !== "") {
      cur.push(ln);
    }
  }
  if (cur.length > 0) hunks.push(cur);

  const lines: AnnotatedLine[] = [];
  let chars = 0;
  let truncated = false;
  let first_line = 0;
  let last_line = 0;
  const note = (n: number | null, marker: string, text: string) => {
    lines.push({ n, marker, text });
    if (typeof n === "number" && n > 0) {
      if (first_line === 0 || n < first_line) first_line = n;
      if (n > last_line) last_line = n;
    }
  };

  for (const hunk of hunks) {
    const hunkChars = hunk.join("\n").length + 1;
    if (chars + hunkChars > budget) {
      truncated = true; // cut at this hunk boundary
      break;
    }
    chars += hunkChars;
    let oldN: number | null = null;
    let newN: number | null = null;
    for (const ln of hunk) {
      const m = HUNK_RE.exec(ln);
      if (m) {
        oldN = Number(m[1]);
        newN = Number(m[3]);
        note(null, "@@", ln);
      } else if (ln.startsWith("+")) {
        if (newN !== null) newN += 1;
        note(newN, "", ln);
      } else if (ln.startsWith("-")) {
        if (oldN !== null) oldN += 1;
        note(oldN, "-", ln);
      } else if (ln.startsWith("\\")) {
        note(null, "", ln); // "\ No newline at end of file"
      } else {
        if (oldN !== null) oldN += 1;
        if (newN !== null) newN += 1;
        note(newN, " ", ln); // context line
      }
    }
  }
  return { lines, truncated, chars, first_line, last_line };
}

// --- File processing shared by the diff tools -------------------------------

export type DiffFile = {
  filename: string;
  status: string;
  previous_filename: string | null;
  additions: number;
  deletions: number;
  skipped: boolean;
  reason: string | null;
  patch_truncated: boolean;
  first_line: number;
  last_line: number;
  lines: AnnotatedLine[];
};

export type RawFile = { filename: string; status: string; previous_filename?: string | null; additions?: number; deletions?: number; patch?: string };

// Validate a repo-relative path (defense in depth behind the schema pattern).
export function pathError(p: string): string | null {
  if (p === "") return null;
  if (p.startsWith("/") || p.startsWith("-")) return "path must not start with / or -";
  if (p.length > 255) return "path too long";
  for (const seg of p.split("/")) {
    if (seg === "." || seg === "..") return "path must not contain . or .. segments";
  }
  return null;
}

export function processFile(f: RawFile, budget: number): { file: DiffFile; chars: number } {
  const filename = f.filename ?? "";
  const gen = generatedReason(filename);
  if (gen !== null) {
    return { file: { filename, status: f.status ?? "modified", previous_filename: f.previous_filename ?? null, additions: f.additions ?? 0, deletions: f.deletions ?? 0, skipped: true, reason: gen, patch_truncated: false, first_line: 0, last_line: 0, lines: [] }, chars: 0 };
  }
  if (!f.patch) {
    // No patch: binary or too large for GitHub to render.
    return { file: { filename, status: f.status ?? "modified", previous_filename: f.previous_filename ?? null, additions: f.additions ?? 0, deletions: f.deletions ?? 0, skipped: true, reason: "binary_or_too_large", patch_truncated: false, first_line: 0, last_line: 0, lines: [] }, chars: 0 };
  }
  const a = annotatePatch(f.patch, budget);
  return {
    file: { filename, status: f.status ?? "modified", previous_filename: f.previous_filename ?? null, additions: f.additions ?? 0, deletions: f.deletions ?? 0, skipped: false, reason: null, patch_truncated: a.truncated, first_line: a.first_line, last_line: a.last_line, lines: a.lines },
    chars: a.chars,
  };
}

// Fetch pages startPage, startPage+1, ... (maxPages pages total) until a page
// holds fewer than 100 files. Returns per-page raw file lists in order.
export async function fetchFilePages(owner: string, repo: string, number: number, startPage: number, maxPages: number): Promise<{ ok: true; pages: { page: number; files: RawFile[] }[]; lastFull: boolean } | { ok: false; error: Record<string, unknown> }> {
  const pages: { page: number; files: RawFile[] }[] = [];
  let page = startPage;
  for (let i = 0; i < maxPages; i++) {
    const r = await gh(filesUrl(owner, repo, number, page));
    if (!r.ok) return { ok: false, error: r.error };
    let arr: unknown;
    try {
      arr = JSON.parse(r.text);
    } catch {
      return { ok: false, error: { error: "upstream", detail: "files response was not JSON" } };
    }
    if (!Array.isArray(arr)) {
      return { ok: false, error: { error: "upstream", detail: "files response had no file list" } };
    }
    pages.push({ page, files: arr as RawFile[] });
    if (arr.length < 100) break;
    page += 1;
  }
  const last = pages[pages.length - 1];
  return { ok: true, pages, lastFull: last === undefined ? false : last.files.length >= 100 };
}