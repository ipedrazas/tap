// echo_text: argv --text <value>; prints JSON on stdout.
import { parseArgs } from "node:util";

const { values } = parseArgs({ options: { text: { type: "string" } }, strict: true });
const text: string = values.text ?? "";
process.stdout.write(JSON.stringify({ text, length: text.length }));
