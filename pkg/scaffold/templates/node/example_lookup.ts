// example_lookup: argv --id <value>; prints one JSON value on stdout.
// Expected failures (not found, bad upstream reply) are JSON with an "error"
// field and exit 0, so the model sees a useful message. Exit non-zero only on bugs.
import { parseArgs } from "node:util";

const { values } = parseArgs({ options: { id: { type: "string" } }, strict: true });

const res = await fetch(`https://api.example.com/items/${encodeURIComponent(values.id!)}`, {
  signal: AbortSignal.timeout(10_000),
});
if (res.status === 404) {
  process.stdout.write(JSON.stringify({ error: "not_found", id: values.id }));
} else if (!res.ok) {
  process.stdout.write(JSON.stringify({ error: "upstream", status: res.status }));
} else {
  const item = await res.json();
  process.stdout.write(JSON.stringify({ id: values.id, name: item.name }));
}
