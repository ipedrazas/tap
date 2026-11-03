// links-resolve: every source URL loads (2xx after redirects) and is not a
// "page not found" page served with 200. Collects each page's text as
// evidence for source-supports-claim, which reads what the pages say.
// Reads {task, output, attempt} on stdin; prints {pass, reason, details, evidence}.
import { fetchPage, isPageError } from "../tools/page.ts";
import { parseReport, plural, readInput, result } from "./report.ts";

const MAX_SOURCES = 25;
const EVIDENCE_CHARS = 6_000;
const CONCURRENCY = 4;

const { output } = await readInput();
const { sources } = parseReport(output);
if (sources.length === 0) result(false, "the report lists no sources to check");
if (sources.length > MAX_SOURCES) result(false, `the report lists ${sources.length} sources; at most ${MAX_SOURCES} can be checked`);

type Outcome = { n: number; url: string; ok: boolean; problem?: string; final_url?: string; title?: string; text?: string };

async function check(s: { n: number; url: string }): Promise<Outcome> {
  const p = await fetchPage(s.url);
  if (isPageError(p)) return { n: s.n, url: s.url, ok: false, problem: `${p.error}: ${p.message}` };
  if (p.status < 200 || p.status > 299) return { n: s.n, url: s.url, ok: false, problem: `HTTP ${p.status}`, final_url: p.final_url };
  if (p.soft_404) return { n: s.n, url: s.url, ok: false, problem: `soft 404: ${p.soft_404_reason}`, final_url: p.final_url, title: p.title };
  return { n: s.n, url: s.url, ok: true, final_url: p.final_url, title: p.title, text: p.text.slice(0, EVIDENCE_CHARS) };
}

const outcomes: Outcome[] = new Array(sources.length);
let next = 0;
await Promise.all(
  Array.from({ length: Math.min(CONCURRENCY, sources.length) }, async () => {
    for (let i = next++; i < sources.length; i = next++) outcomes[i] = await check(sources[i]!);
  }),
);

const bad = outcomes.filter((o) => !o.ok);
const evidence = {
  pages: outcomes.filter((o) => o.ok).map((o) => ({ source: o.n, url: o.final_url, title: o.title, text: o.text })),
};
if (bad.length === 0) result(true, sources.length === 1 ? "the source loads" : `all ${sources.length} sources load`, undefined, evidence);
result(
  false,
  `${bad.length} of ${plural(sources.length, "source")} ${bad.length === 1 ? "does" : "do"} not resolve: ${bad.map((b) => `[${b.n}] ${b.url} (${b.problem})`).join("; ")}`.slice(0, 1500),
  bad.map(({ n, url, problem, final_url }) => ({ source: n, url, problem, final_url })),
  evidence,
);
