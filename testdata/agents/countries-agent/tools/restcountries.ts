// Shared helpers for the REST Countries v5 API (https://api.restcountries.com/countries/v5).
// v5 needs a bearer key in RESTCOUNTRIES_API_KEY; the older keyless v3.1 API is retired.

export const BASE = "https://api.restcountries.com/countries/v5";

const FIELDS = [
  "names.common",
  "names.official",
  "codes.alpha_2",
  "codes.alpha_3",
  "capitals",
  "region",
  "subregion",
  "continents",
  "population",
  "area",
  "languages",
  "currencies",
  "borders",
  "landlocked",
  "flag.emoji",
].join(",");

export type Country = {
  name: string;
  official_name: string | null;
  alpha2: string | null;
  alpha3: string | null;
  capitals: string[];
  region: string | null;
  subregion: string | null;
  continents: string[];
  population: number | null;
  area_km2: number | null;
  density_per_km2: number | null;
  languages: string[];
  currencies: { code: string; name: string; symbol: string | null }[];
  borders: string[];
  landlocked: boolean | null;
  flag: string | null;
};

export type Failure = { error: string; [k: string]: unknown };

async function get(url: URL): Promise<{ status: number; body: any }> {
  const res = await fetch(url, {
    headers: {
      accept: "application/json",
      authorization: `Bearer ${process.env.RESTCOUNTRIES_API_KEY ?? ""}`,
    },
    signal: AbortSignal.timeout(10_000),
  });
  let body: any = null;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  return { status: res.status, body };
}

function upstreamError(status: number, body: any): Failure {
  const message = body?.errors?.[0]?.message ?? null;
  if (status === 401) return { error: "unauthorized", status, message };
  if (status === 403) return { error: "forbidden", status, message };
  if (status === 429) return { error: "rate_limited", status, message };
  return { error: "upstream", status, message };
}

// Build the lookup URL: ISO alpha-2/alpha-3 codes go to an exact code lookup,
// anything else to the "name" aggregate search (common, official, alternate, native names).
export function lookupURL(query: string): URL {
  const q = query.trim();
  let url: URL;
  if (/^[A-Za-z]{2}$/.test(q)) {
    url = new URL(`${BASE}/codes.alpha_2/${encodeURIComponent(q.toUpperCase())}`);
  } else if (/^[A-Za-z]{3}$/.test(q)) {
    url = new URL(`${BASE}/codes.alpha_3/${encodeURIComponent(q.toUpperCase())}`);
  } else {
    url = new URL(`${BASE}/name`);
    url.searchParams.set("q", q);
    url.searchParams.set("limit", "10");
  }
  url.searchParams.set("response_fields", FIELDS);
  return url;
}

function normalise(o: any): Country {
  const population = typeof o?.population === "number" ? o.population : null;
  const area = typeof o?.area?.kilometers === "number" ? o.area.kilometers : null;
  return {
    name: o?.names?.common ?? "",
    official_name: o?.names?.official ?? null,
    alpha2: o?.codes?.alpha_2 ?? null,
    alpha3: o?.codes?.alpha_3 ?? null,
    capitals: Array.isArray(o?.capitals) ? o.capitals.map((c: any) => c?.name).filter(Boolean) : [],
    region: o?.region ?? null,
    subregion: o?.subregion || null,
    continents: Array.isArray(o?.continents) ? o.continents : [],
    population,
    area_km2: area,
    density_per_km2: population !== null && area ? Math.round((population / area) * 10) / 10 : null,
    languages: Array.isArray(o?.languages) ? o.languages.map((l: any) => l?.name).filter(Boolean) : [],
    currencies: Array.isArray(o?.currencies)
      ? o.currencies.map((c: any) => ({ code: c?.code ?? "", name: c?.name ?? "", symbol: c?.symbol ?? null }))
      : [],
    borders: Array.isArray(o?.borders) ? o.borders : [],
    landlocked: typeof o?.landlocked === "boolean" ? o.landlocked : null,
    flag: o?.flag?.emoji ?? null,
  };
}

// Pick the best match from a name search: exact common/official name first, then the first hit.
function pick(objects: any[], query: string): any {
  const q = query.trim().toLowerCase();
  return (
    objects.find((o) => o?.names?.common?.toLowerCase() === q) ??
    objects.find((o) => o?.names?.official?.toLowerCase() === q) ??
    objects[0]
  );
}

export async function lookup(query: string): Promise<
  { country: Country; other_matches: string[] } | Failure
> {
  const { status, body } = await get(lookupURL(query));
  if (status === 404) return { error: "not_found", query };
  if (status !== 200) return upstreamError(status, body);
  const objects = body?.data?.objects;
  if (!Array.isArray(objects)) return { error: "bad_upstream_reply", query };
  if (objects.length === 0) return { error: "not_found", query };
  const best = pick(objects, query);
  const other_matches = objects
    .filter((o) => o !== best)
    .map((o) => o?.names?.common)
    .filter(Boolean)
    .slice(0, 5);
  return { country: normalise(best), other_matches };
}

// Countries that list `alpha3` in their borders, i.e. its neighbours, by name.
export async function neighbours(alpha3: string): Promise<{ code: string; name: string }[] | Failure> {
  const url = new URL(`${BASE}/borders/${encodeURIComponent(alpha3)}`);
  url.searchParams.set("response_fields", "names.common,codes.alpha_3");
  url.searchParams.set("limit", "100");
  const { status, body } = await get(url);
  if (status === 404) return [];
  if (status !== 200) return upstreamError(status, body);
  const objects = body?.data?.objects;
  if (!Array.isArray(objects)) return { error: "bad_upstream_reply" };
  return objects.map((o: any) => ({ code: o?.codes?.alpha_3 ?? "", name: o?.names?.common ?? "" }));
}

export function isFailure(x: unknown): x is Failure {
  return typeof x === "object" && x !== null && !Array.isArray(x) && "error" in x;
}
