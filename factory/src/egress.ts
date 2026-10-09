// Per-call egress credentials, byte-compatible with pkg/egress/token.go:
// "<b64url(json claims)>.<b64url(HMAC-SHA256(key, payload))>". The proxy
// allows each scope only the hosts in its own policy for this client.
import { createHmac, randomBytes } from "node:crypto";

export type Claims = { a: string; s: string; e: number; n?: string };

export function mint(key: string, claims: Claims): string {
	// Field order matches Go's json.Marshal of egress.Claims.
	const c = { a: claims.a, s: claims.s, e: claims.e, n: claims.n ?? randomBytes(8).toString("hex") };
	const payload = Buffer.from(JSON.stringify(c)).toString("base64url");
	const mac = createHmac("sha256", key).update(payload).digest("base64url");
	return `${payload}.${mac}`;
}

export type Minter = { proxy: string; agent: string; key: string };

export function proxyUrl(m: Minter, scope: string, ttlSec: number): string {
	const token = mint(m.key, { a: m.agent, s: scope, e: Math.floor(Date.now() / 1000) + ttlSec });
	return `http://${m.agent}:${token}@${m.proxy}`;
}

// The same conventions as egress.ProxyEnv: curl, Python, Go, git and Node
// (built-in fetch only with NODE_USE_ENV_PROXY=1).
export function proxyEnv(url: string): Record<string, string> {
	return {
		HTTPS_PROXY: url,
		https_proxy: url,
		HTTP_PROXY: url,
		http_proxy: url,
		NO_PROXY: "",
		no_proxy: "",
		NODE_USE_ENV_PROXY: "1",
	};
}
