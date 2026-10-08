// convert_currency: argv --amount <n> --from <CCY> --to <CCY>; prints one JSON value.
import { parseArgs } from "node:util";
import { BASE, get, out } from "./frankfurter.ts";

const { values } = parseArgs({
  options: { amount: { type: "string" }, from: { type: "string" }, to: { type: "string" } },
  strict: true,
});
const amount = Number(values.amount);
const from = values.from!;
const to = values.to!;

if (from === to) {
  out({ error: "same_currency", detail: "from and to are the same currency" });
} else {
  const url = new URL(`${BASE}/latest`);
  url.searchParams.set("from", from);
  url.searchParams.set("to", to);
  const r = await get(url);
  if (!r.ok) {
    out({ ...r.error, from, to });
  } else {
    const rate = r.data.rates?.[to];
    if (typeof rate !== "number") {
      out({ error: "upstream", detail: "rate missing from reply", from, to });
    } else {
      out({
        amount,
        from,
        to,
        rate,
        converted: Math.round(amount * rate * 10000) / 10000,
        rate_date: r.data.date,
        source: "European Central Bank reference rate via frankfurter.dev",
      });
    }
  }
}
