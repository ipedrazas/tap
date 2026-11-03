// web_search: argv --query <text> --count <n>; prints one JSON value on stdout.
// Uses the Exa search API (key in EXA_API_KEY), asking for one highlight per
// result as its snippet. Expected failures are JSON with an "error" field and
// exit 0.
import { parseArgs } from "node:util";

const { values } = parseArgs({ options: { query: { type: "string" }, count: { type: "string" } }, strict: true });

let res: Response;
try {
  res = await fetch("https://api.exa.ai/search", {
    method: "POST",
    headers: { accept: "application/json", "content-type": "application/json", "x-api-key": process.env.EXA_API_KEY ?? "" },
    body: JSON.stringify({
      query: values.query,
      numResults: Number(values.count ?? "6"),
      contents: { highlights: { numSentences: 2, highlightsPerUrl: 1 } },
    }),
    signal: AbortSignal.timeout(20_000),
  });
} catch (e) {
  process.stdout.write(JSON.stringify({ error: "unreachable", message: String((e as Error).message).slice(0, 200) }));
  process.exit(0);
}
if (res.status === 401 || res.status === 403) {
  process.stdout.write(JSON.stringify({ error: "unauthorized" }));
} else if (res.status === 429) {
  process.stdout.write(JSON.stringify({ error: "rate_limited" }));
} else if (!res.ok) {
  process.stdout.write(JSON.stringify({ error: "upstream", status: res.status }));
} else {
  type Result = { title?: string | null; url?: string; publishedDate?: string | null; highlights?: string[]; text?: string };
  const body = (await res.json()) as { results?: Result[] };
  const results = (body.results ?? []).map((r) => ({
    title: (r.title ?? "").trim(),
    url: r.url ?? "",
    snippet: (r.highlights?.[0] ?? r.text?.slice(0, 300) ?? "").replace(/\s+/g, " ").trim(),
    ...(r.publishedDate ? { published: r.publishedDate.slice(0, 10) } : {}),
  }));
  process.stdout.write(JSON.stringify({ query: values.query, results }));
}
