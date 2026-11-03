// claims-cited: every factual sentence of the report cites a listed source,
// every citation points at a listed source, and every source is cited.
// Reads {task, output, attempt} on stdin; prints {pass, reason, details}.
import { parseReport, plural, readInput, result } from "./report.ts";

const MAX_LISTED = 20;
const { output } = await readInput();
const report = parseReport(output);

if (!report.hasSources) result(false, 'the report has no "## Sources" section');
if (report.sources.length === 0) result(false, 'the "## Sources" section lists no sources in the form "1. Title — https://…"');
if (report.claims.length === 0) result(false, "the report makes no statements to check");

const listed = new Set(report.sources.map((s) => s.n));
const uncited = report.claims.filter((c) => c.cites.length === 0);
const dangling = [...new Set(report.claims.flatMap((c) => c.cites).filter((n) => !listed.has(n)))].sort((a, b) => a - b);
const cited = new Set(report.claims.flatMap((c) => c.cites));
const unused = report.sources.filter((s) => !cited.has(s.n)).map((s) => s.n);

const problems: string[] = [];
if (uncited.length) problems.push(`${uncited.length} of ${plural(report.claims.length, "statement")} ${uncited.length === 1 ? "has" : "have"} no citation`);
if (dangling.length) problems.push(`cites ${dangling.map((n) => `[${n}]`).join(", ")} but Sources does not list ${dangling.length === 1 ? "it" : "them"}`);
if (unused.length) problems.push(`Sources lists ${unused.map((n) => `${n}`).join(", ")} but the report never cites ${unused.length === 1 ? "it" : "them"}`);

if (problems.length === 0) {
  result(true, `${plural(report.claims.length, "statement")}, all cited; ${plural(report.sources.length, "source")}, all used`);
}
result(false, problems.join("; "), {
  uncited: uncited.slice(0, MAX_LISTED).map((c) => c.text.slice(0, 300)),
  dangling,
  unused,
});
