// compare_countries: argv --first <country> --second <country>; prints one JSON value on stdout.
// Expected failures are JSON with an "error" field and exit 0.
import { parseArgs } from "node:util";
import { isFailure, lookup, type Country } from "./restcountries.ts";

const { values } = parseArgs({
  options: { first: { type: "string" }, second: { type: "string" } },
  strict: true,
});

const a = await lookup(values.first!);
const b = await lookup(values.second!);

if (isFailure(a) || isFailure(b)) {
  process.stdout.write(
    JSON.stringify({
      error: "lookup_failed",
      first: isFailure(a) ? a : { name: a.country.name },
      second: isFailure(b) ? b : { name: b.country.name },
    }),
  );
} else {
  const x: Country = a.country;
  const y: Country = b.country;
  const ratio = (p: number | null, q: number | null) =>
    p !== null && q !== null && q !== 0 ? Math.round((p / q) * 100) / 100 : null;
  const lower = (l: string[]) => new Set(l.map((s) => s.toLowerCase()));
  const yl = lower(y.languages);
  const yc = new Set(y.currencies.map((c) => c.code));
  process.stdout.write(
    JSON.stringify({
      first: x,
      second: y,
      comparison: {
        population_ratio: ratio(x.population, y.population),
        area_ratio: ratio(x.area_km2, y.area_km2),
        density_ratio: ratio(x.density_per_km2, y.density_per_km2),
        same_region: x.region !== null && x.region === y.region,
        share_border: (x.alpha3 !== null && y.borders.includes(x.alpha3)) || (y.alpha3 !== null && x.borders.includes(y.alpha3)),
        shared_languages: x.languages.filter((l) => yl.has(l.toLowerCase())),
        shared_currencies: x.currencies.filter((c) => yc.has(c.code)).map((c) => c.code),
      },
    }),
  );
}
