---
name: countries
description: Output fields of get_country and compare_countries, and how to interpret them
---

# countries

## get_country

Returns one country object:

| Field | Meaning |
| --- | --- |
| `name`, `official_name` | common and official English names |
| `alpha2`, `alpha3` | ISO 3166-1 codes |
| `capitals` | capital city names; some countries have several (e.g. South Africa), some none |
| `region`, `subregion`, `continents` | UN-style region (e.g. "Europe"), subregion (e.g. "Western Europe"), continents |
| `population` | current population estimate (refreshed every few hours upstream) |
| `area_km2` | land area in square kilometres |
| `density_per_km2` | `population / area_km2`, one decimal, computed by the tool |
| `languages` | official/recognised language names in English |
| `currencies` | `{code, name, symbol}` per currency; `code` is ISO 4217 |
| `neighbours` | countries sharing a land border, as `{code, name}`; empty for islands. `name` falls back to the code if names couldn't be fetched (then `neighbours_error` is set) |
| `landlocked` | true if the country has no coastline |
| `flag` | flag emoji |
| `other_matches` | other countries the name search also matched; the tool chose an exact name match first, otherwise the first hit |

## compare_countries

Returns `first` and `second` (each a country object as above, with `borders` as alpha-3 codes instead of `neighbours`) and `comparison`:

| Field | Meaning |
| --- | --- |
| `population_ratio`, `area_ratio`, `density_ratio` | first ÷ second, two decimals (2.5 means first is 2.5× second; 0.12 means about an eighth) |
| `same_region` | both in the same `region` |
| `share_border` | they are land neighbours |
| `shared_languages`, `shared_currencies` | languages (names) and currency codes both have |

If either lookup fails, the result is `{"error": "lookup_failed", "first": ..., "second": ...}`, where the failing side carries its own `error` (`not_found`, `unauthorized`, `rate_limited`, `upstream`).

## Errors

- `not_found`: no country matched; try the ISO code or another spelling.
- `unauthorized` / `forbidden`: the API key is missing, invalid or over quota.
- `rate_limited`: more than 20 requests per 10 seconds upstream.
- `upstream`, `bad_upstream_reply`: the API failed; `status` and `message` say how.
