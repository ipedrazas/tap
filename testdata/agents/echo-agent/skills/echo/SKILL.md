---
name: echo
description: How to use echo_text and current_time, and what their outputs mean
---

# Echo

- `echo_text` returns `{ "text": string, "length": number }`. `length` counts UTF-16 code units.
- `current_time` returns `{ "utc": string }` in ISO 8601.

Text starting with `-` is rejected by the tool's schema. Ask the user to rephrase, or prefix it with a space.
