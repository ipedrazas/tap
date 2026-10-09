---
name: exchange-eval-agent
runner: runner-node
reference: exchange-agent
egress: api.frankfurter.dev:443
secrets:
effects: read
---
An agent that converts amounts between currencies and shows how an exchange rate moved over the last few days or weeks, using the free Frankfurter API (European Central Bank reference rates, no API key). Two read-only tools: one converts an amount from one currency to one or more others at the latest rate, the other returns the daily rate history between two currencies for a date range.
