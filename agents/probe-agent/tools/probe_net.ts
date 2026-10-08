// probe_net: attempt one network path; ok means a response came back.
import { parseArgs } from "node:util";
import https from "node:https";
import http from "node:http";
import net from "node:net";

const { values } = parseArgs({ options: { target: { type: "string" } }, strict: true });

// Direct requests bypass the proxy env on purpose: they must be stopped by the network policy.
function direct(url: string, mod: typeof https | typeof http): Promise<number> {
  return new Promise((resolve, reject) => {
    const req = mod.get(url, { timeout: 4000, agent: false, rejectUnauthorized: false } as any, (res) => { res.resume(); resolve(res.statusCode ?? 0); });
    req.on("timeout", () => req.destroy(new Error("timeout")));
    req.on("error", reject);
  });
}

async function viaProxy(url: string): Promise<number> {
  const res = await fetch(url, { signal: AbortSignal.timeout(8000) });
  return res.status;
}

async function run(target: string): Promise<number> {
  switch (target) {
    case "declared": return viaProxy("https://example.com/");
    case "undeclared": return viaProxy("https://www.google.com/");
    case "direct": return direct("https://example.com/", https);
    case "kube_api": return direct("https://10.43.0.1/version", https);
    case "model_gateway": return direct("http://kodo-inference.envoy-gateway-system.svc.cluster.local/v1/models", http);
    case "egress_proxy_no_token": {
      // A raw socket, so Node's env-proxy support cannot attach this tool's credential.
      return new Promise((resolve, reject) => {
        const sock = net.connect({ host: "egress-proxy.tap-system.svc.cluster.local", port: 3128, timeout: 4000 });
        sock.on("connect", () => sock.write("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"));
        sock.once("data", (buf) => { sock.destroy(); resolve(Number(buf.toString().split(" ")[1])); });
        sock.on("timeout", () => sock.destroy(new Error("timeout")));
        sock.on("error", reject);
      });
    }
  }
  throw new Error("unknown target");
}

try {
  const status = await run(values.target!);
  process.stdout.write(JSON.stringify({ target: values.target, reached: true, status }));
} catch (e: any) {
  process.stdout.write(JSON.stringify({ target: values.target, reached: false, error: String(e?.cause?.code ?? e?.code ?? e?.message ?? e) }));
}
