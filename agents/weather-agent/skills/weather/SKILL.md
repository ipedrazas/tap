---
name: weather
description: How to read forecast output, including WMO weather codes
---

# Reading forecast output

`forecast` returns `current` (temperature_c, wind_kmh, weather) and `daily` (date, min_c, max_c, weather) for 3 days, in the place's local timezone.

`weather` is already translated from the WMO code. If it says `code N`, the code was unknown; describe it as "mixed conditions".
