// fetch_page: argv --url <https URL>; prints one JSON value on stdout.
// Expected failures are JSON with an "error" field and exit 0.
import { parseArgs } from "node:util";
import { fetchPage, isPageError } from "./page.ts";

const MAX_TEXT = 20_000;
const { values } = parseArgs({ options: { url: { type: "string" } }, strict: true });

const page = await fetchPage(values.url!);
if (isPageError(page)) {
  process.stdout.write(JSON.stringify(page));
} else {
  const truncated = page.text.length > MAX_TEXT;
  process.stdout.write(JSON.stringify({ ...page, text: page.text.slice(0, MAX_TEXT), ...(truncated ? { truncated } : {}) }));
}
