---
name: exchange
description: How to read the output of convert_currency and rate_history (ECB reference rates via Frankfurter)
---

# exchange

Rates are European Central Bank reference rates, published once per working day around 16:00 CET. There are no rates for weekends or TARGET holidays, so a 30-day window has about 21-23 observations. Only ~30 currencies are covered: AUD BRL CAD CHF CNY CZK DKK EUR GBP HKD HUF IDR ILS INR ISK JPY KRW MXN MYR NOK NZD PHP PLN RON SEK SGD THB TRY USD ZAR.

## convert_currency
- `amount`, `from`, `to`: echo of the request.
- `rate`: units of `to` per 1 unit of `from`.
- `converted`: `amount * rate`, rounded to 4 decimals.
- `rate_date`: the date the rate was published (may be before today).

## rate_history
- `base`, `quote`: the pair; rates are units of `quote` per 1 `base`.
- `days`: requested window in calendar days, ending at the latest published date.
- `start_date` / `start_rate`, `end_date` / `end_rate`: first and last observation in the window.
- `change`: `end_rate - start_rate`; `change_pct`: percent change, 2 decimals. Positive means `base` strengthened against `quote`.
- `min`, `max`: `{date, rate}` of the lowest and highest observation.
- `observations`: number of daily rates in the window.
- `series`: `{date, rate}` points; `sampled: true` means it was thinned evenly to 40 points (first and last always included).

## Errors (all exit normally with an `error` field)
- `unknown_currency`: the ECB does not publish one of the codes.
- `same_currency`: both codes are the same; no conversion needed.
- `invalid_request`: upstream rejected the request; `detail` says why.
- `upstream` (with `status`) / `upstream_unreachable`: the API failed; suggest trying again later.
- `no_data`: no rates in the window.
