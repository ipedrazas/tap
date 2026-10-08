// Isolation checks for a deployed probe-agent. Runs inside the runner
// container (as the runner, which may read the token) and drives each probe
// tool through /v1/call, so every probe runs exactly as a real tool would.
import { readFileSync } from "node:fs";

const token = readFileSync("/run/tap/token/token", "utf8").trim();
async function call(tool, args = {}) {
  const r = await fetch("http://127.0.0.1:7070/v1/call", {
    method: "POST",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    body: JSON.stringify({ tool, args, call_id: `iso-${tool}-${args.target ?? ""}` }),
  });
  const body = await r.json();
  if (!body.ok) throw new Error(`${tool} failed: ${JSON.stringify(body.error)}`);
  return body.output;
}

const results = [];
const check = (name, pass, detail) => results.push({ name, pass, detail });

const files = await call("probe_files");
for (const [path, outcome] of Object.entries(files)) {
  if (!path.startsWith("/")) continue;
  check(`file ${path} not readable by tools`, outcome !== "readable", outcome);
}
check("tool runs as non-root", files.uid !== "0", `uid ${files.uid}`);
check("undeclared tool has no secret in env", files.env_has_probe_token === "false", files.env_has_probe_token);

const secret = await call("probe_secret");
check("declaring tool receives its secret", secret.has_token === true, JSON.stringify(secret));
check("tools run under different uids", String(secret.uid) !== files.uid, `${secret.uid} vs ${files.uid}`);

const net = async (target) => call("probe_net", { target });
const declared = await net("declared");
check("declared host reachable via proxy", declared.reached && declared.status === 200, JSON.stringify(declared));
const undeclared = await net("undeclared");
check("undeclared host refused by proxy", !undeclared.reached, JSON.stringify(undeclared));
const direct = await net("direct");
check("direct internet blocked by network policy", !direct.reached, JSON.stringify(direct));
const kube = await net("kube_api");
check("kube API blocked by network policy", !kube.reached, JSON.stringify(kube));
const model = await net("model_gateway");
check("model gateway refuses tools (no key)", !model.reached || model.status === 401 || model.status === 403, JSON.stringify(model));
const noTok = await net("egress_proxy_no_token");
check("proxy refuses calls without a credential", !noTok.reached || noTok.status >= 400, JSON.stringify(noTok));

let failed = 0;
for (const r of results) {
  if (!r.pass) failed++;
  console.log(`${r.pass ? "PASS" : "FAIL"} ${r.name}  (${r.detail})`);
}
console.log(`\n${results.length - failed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
