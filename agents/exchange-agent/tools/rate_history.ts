// rate_history: argv --base <CCY> --quote <CCY> --days <n>; prints one JSON value.
// The window ends at the latest published rate date (taken from /latest), not the
// local clock, so results are anchored to real data and fixtures are deterministic.
import { parseArgs } from "node:util";
import { BASE, get, out } from "./frankfurter.ts";

const MAX_POINTS = 40;

const { values } = parseArgs({
  options: { base: { type: "string" }, quote: { type: "string" }, days: { type: "string" } },
  strict: true,
});
const base = values.base!;
const quote = values.quote!;
const days = Number.parseInt(values.days ?? "30", 10);

const round = (n: number) => Math.round(n * 1e6) / 1e6;

async function main(): Promise<unknown> {
  if (base === quote) return { error: "same_currency", detail: "base and quote are the same currency" };

  const latestUrl = new URL(`${BASE}/latest`);
  latestUrl.searchParams.set("from", base);
  latestUrl.searchParams.set("to", quote);
  const latest = await get(latestUrl);
  if (!latest.ok) return { ...latest.error, base, quote };
  const end: string = latest.data.date;
  if (typeof end !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(end)) return { error: "upstream", detail: "no date in reply" };

  const startDate = new Date(`${end}T00:00:00Z`);
  startDate.setUTCDate(startDate.getUTCDate() - days);
  const start = startDate.toISOString().slice(0, 10);

  const histUrl = new URL(`${BASE}/${start}..${end}`);
  histUrl.searchParams.set("from", base);
  histUrl.searchParams.set("to", quote);
  const hist = await get(histUrl);
  if (!hist.ok) return { ...hist.error, base, quote };

  const series: { date: string; rate: number }[] = Object.entries(hist.data.rates ?? {})
    .map(([date, r]: [string, any]) => ({ date, rate: r?.[quote] }))
    .filter((p) => typeof p.rate === "number")
    .sort((a, b) => a.date.localeCompare(b.date));
  if (series.length === 0) return { error: "no_data", base, quote, start_date: start, end_date: end };

  const first = series[0];
  const last = series[series.length - 1];
  let min = first;
  let max = first;
  for (const p of series) {
    if (p.rate < min.rate) min = p;
    if (p.rate > max.rate) max = p;
  }

  let sampled = series;
  if (series.length > MAX_POINTS) {
    const step = (series.length - 1) / (MAX_POINTS - 1);
    sampled = Array.from({ length: MAX_POINTS }, (_, i) => series[Math.round(i * step)]);
  }

  return {
    base,
    quote,
    days,
    start_date: first.date,
    end_date: last.date,
    start_rate: first.rate,
    end_rate: last.rate,
    change: round(last.rate - first.rate),
    change_pct: Math.round(((last.rate - first.rate) / first.rate) * 10000) / 100,
    min,
    max,
    observations: series.length,
    series: sampled,
    sampled: sampled.length < series.length,
    source: "European Central Bank reference rates via frankfurter.dev",
  };
}

out(await main());
