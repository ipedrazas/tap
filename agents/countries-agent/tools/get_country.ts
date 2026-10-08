// get_country: argv --country <name or ISO code>; prints one JSON value on stdout.
// Expected failures are JSON with an "error" field and exit 0.
import { parseArgs } from "node:util";
import { isFailure, lookup, neighbours } from "./restcountries.ts";

const { values } = parseArgs({ options: { country: { type: "string" } }, strict: true });

const found = await lookup(values.country!);
if (isFailure(found)) {
  process.stdout.write(JSON.stringify(found));
} else {
  const { country, other_matches } = found;
  let neighbour_list: { code: string; name: string }[] = country.borders.map((code) => ({ code, name: code }));
  let neighbours_error: string | undefined;
  if (country.borders.length > 0 && country.alpha3) {
    const n = await neighbours(country.alpha3);
    if (isFailure(n)) {
      neighbours_error = n.error;
    } else {
      const names = new Map(n.map((x) => [x.code, x.name]));
      neighbour_list = country.borders.map((code) => ({ code, name: names.get(code) ?? code }));
    }
  }
  const { borders, ...rest } = country;
  process.stdout.write(
    JSON.stringify({
      ...rest,
      neighbours: neighbour_list,
      ...(neighbours_error ? { neighbours_error } : {}),
      other_matches,
    }),
  );
}
