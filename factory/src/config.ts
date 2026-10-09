// Configuration comes from the environment so the Deployment is the single
// place it is set. Secrets are read from files, never from variables, so they
// cannot leak into child processes through the environment.
import { readFileSync } from "node:fs";

export type Config = {
	listen: string;
	dataDir: string; // jobs, conversations (PVC)
	// Model gateway (Envoy AI Gateway, OpenAI-compatible).
	modelBaseUrl: string;
	modelKeyHeader: string;
	modelKey: string;
	modelRoutes: string[];
	defaultRoute: string;
	// Publisher sidecar on loopback.
	publisherUrl: string;
	publisherToken: string;
	// Egress proxy for tool commands; empty disables egress.
	egressProxy: string;
	egressAgent: string;
	egressKey: string;
	repo: string; // owner/name, for the checkout tarball
	// Commands run as this uid/gid (the factory must be root); 0 runs them as
	// the factory's own user, for local development only.
	sandboxUid: number;
	toolTimeoutSec: number;
	jobTimeoutMin: number;
	node: string;
	python: string;
};

function env(name: string, fallback?: string): string {
	const v = process.env[name] ?? fallback;
	if (v === undefined) throw new Error(`${name} is required`);
	return v;
}

export function readSecret(path: string): string {
	const v = readFileSync(path, "utf8").trim();
	if (!v) throw new Error(`${path} is empty`);
	return v;
}

function optionalSecret(path: string): string {
	return path ? readSecret(path) : "";
}

export function loadConfig(): Config {
	const routes = env("FACTORY_MODEL_ROUTES", "agent,default,sim")
		.split(",")
		.map((s) => s.trim())
		.filter(Boolean);
	const egressProxy = env("FACTORY_EGRESS_PROXY", "");
	return {
		listen: env("FACTORY_LISTEN", "0.0.0.0:8080"),
		dataDir: env("FACTORY_DATA_DIR", "/data"),
		modelBaseUrl: env("FACTORY_MODEL_BASE_URL"),
		modelKeyHeader: env("FACTORY_MODEL_KEY_HEADER", "x-kodo-gateway-key"),
		modelKey: readSecret(env("FACTORY_MODEL_KEY_FILE", "/run/tap/model/api-key")),
		modelRoutes: routes,
		defaultRoute: env("FACTORY_MODEL_ROUTE", routes[0] ?? "agent"),
		publisherUrl: env("FACTORY_PUBLISHER_URL", "http://127.0.0.1:7071"),
		publisherToken: readSecret(env("FACTORY_PUBLISHER_TOKEN_FILE", "/run/tap/publisher/token")),
		egressProxy,
		egressAgent: env("FACTORY_EGRESS_AGENT", "tap-factory"),
		egressKey: egressProxy ? optionalSecret(env("FACTORY_EGRESS_KEY_FILE", "/run/tap/egress/key")) : "",
		repo: env("FACTORY_REPO", "ipedrazas/tap"),
		sandboxUid: Number(env("FACTORY_SANDBOX_UID", "61000")),
		toolTimeoutSec: Number(env("FACTORY_TOOL_TIMEOUT", "600")),
		jobTimeoutMin: Number(env("FACTORY_JOB_TIMEOUT_MIN", "60")),
		node: env("FACTORY_NODE", "/usr/local/bin/node"),
		python: env("FACTORY_PYTHON", "/usr/bin/python3"),
	};
}
