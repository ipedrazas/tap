// probe_files: try to read paths a tool must not reach; report the outcome per path.
import { readFileSync, readdirSync } from "node:fs";

const paths = [
  "/run/tap/token/token",
  "/run/tap/secrets/PROBE_TOKEN",
  "/run/tap/egress/key",
  "/run/tap/model/api-key",
  "/proc/1/environ",
  "/workspace/sessions",
  "/workspace/.home/probe_secret",
];
const result: Record<string, string> = {};
for (const p of paths) {
  try {
    try { readdirSync(p); } catch (e: any) { if (e.code !== "ENOTDIR") throw e; readFileSync(p); }
    result[p] = "readable";
  } catch (e: any) {
    result[p] = e.code === "ENOENT" ? "absent" : "denied";
  }
}
result.uid = String(process.getuid?.());
result.env_has_probe_token = String("PROBE_TOKEN" in process.env);
process.stdout.write(JSON.stringify(result));
