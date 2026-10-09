---
name: countries-eval-agent
runner: runner-node
reference: countries-agent
egress: api.restcountries.com:443
secrets: RESTCOUNTRIES_API_KEY
effects: read
---
An agent that answers questions about countries (capital, population, area, languages, currencies, neighbours, region) and compares two countries side by side, using the REST Countries API v3.1 at restcountries.com. Two read-only tools: look up one country by name or code, and compare two countries.
