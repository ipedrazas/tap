// tap-factory: turns an agent spec into a pull request, unattended. It has no
// registry, cluster or GitHub credentials; the publisher sidecar holds the
// GitHub token and opens the PR.
import { chmodSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { createRegistry, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { api } from "./api.ts";
import { confinedEnv } from "./confine.ts";
import { loadConfig } from "./config.ts";
import { Factory, JobStore, saveBrief } from "./jobs.ts";
import { gatewayModels } from "./model.ts";
import { Publisher } from "./publisher.ts";
import { factoryTools, jobDirOf } from "./tools.ts";

function log(msg: string, fields: Record<string, unknown> = {}): void {
	process.stdout.write(`${JSON.stringify({ time: new Date().toISOString(), level: "INFO", msg, component: "factory", ...fields })}\n`);
}

// A stray rejection (a dropped event stream, say) must not take down the
// process that runs everyone's jobs; log it instead. Jobs resume on restart
// anyway, but a crash loop stops new ones.
process.on("unhandledRejection", (e) => log("unhandled rejection", { level: "ERROR", error: String(e) }));

const cfg = loadConfig();
const isRoot = process.getuid?.() === 0;
if (isRoot && cfg.sandboxUid === 0) throw new Error("refusing to run commands as root; set FACTORY_SANDBOX_UID");
if (!isRoot && cfg.sandboxUid !== 0) throw new Error("FACTORY_SANDBOX_UID needs the factory to run as root (SETUID/SETGID)");

// The sandbox user may enter its job's directory but not list the others, and
// cannot read job records or transcripts at all.
const workDir = join(cfg.dataDir, "work");
mkdirSync(workDir, { recursive: true });
mkdirSync(join(cfg.dataDir, "jobs"), { recursive: true });
if (isRoot) {
	chmodSync(cfg.dataDir, 0o711);
	chmodSync(workDir, 0o711);
	chmodSync(join(cfg.dataDir, "jobs"), 0o700);
}
const sandbox = {
	uid: cfg.sandboxUid,
	path: "/usr/local/bin:/usr/bin:/bin",
	extraEnv: { FACTORY_NODE: cfg.node, FACTORY_PYTHON: cfg.python },
};
const minter = cfg.egressProxy ? { proxy: cfg.egressProxy, agent: cfg.egressAgent, key: cfg.egressKey } : undefined;

const registry = createRegistry();
const tools = factoryTools({ ...sandbox, minter, timeoutSec: cfg.toolTimeoutSec, onBrief: saveBrief });
registry.install(tools);

const dbFile = join(cfg.dataDir, "factory.sqlite");
const storage = await openNodeSqliteStorage(dbFile);
if (isRoot) for (const f of [dbFile, `${dbFile}-wal`, `${dbFile}-shm`]) try { chmodSync(f, 0o600); } catch {}
const harness = await Harness.open(
	storage,
	{
		models: gatewayModels({ baseUrl: cfg.modelBaseUrl, keyHeader: cfg.modelKeyHeader, key: cfg.modelKey, routes: cfg.modelRoutes }),
		registry,
		settings: {
			extensions: [tools],
			stream: { timeoutMs: 300_000 },
			retry: { maxRetries: 5 },
			toolExecution: "sequential",
		},
		env: ({ cwd }) => {
			if (!cwd) throw new Error("conversation has no working directory");
			return confinedEnv({ root: jobDirOf(cwd), cwd, uid: cfg.sandboxUid || undefined });
		},
		onReport: (e) => log("harness error", { level: "ERROR", error: String(e) }),
	},
	BACKGROUND_CONTEXT,
);

const store = new JobStore(join(cfg.dataDir, "jobs"));
const factory = new Factory({
	harness,
	store,
	publisher: new Publisher(cfg.publisherUrl, cfg.publisherToken),
	workDir,
	repo: cfg.repo,
	routes: cfg.modelRoutes,
	defaultRoute: cfg.defaultRoute,
	sandbox,
	minter,
	jobTimeoutMin: cfg.jobTimeoutMin,
	log,
});
factory.resume();

const [host, port] = cfg.listen.split(":");
const server = api({ factory, store, harness, routes: cfg.modelRoutes });
server.listen(Number(port), host, () => log("listening", { addr: cfg.listen, routes: cfg.modelRoutes, sandbox_uid: cfg.sandboxUid, egress: Boolean(minter) }));

for (const sig of ["SIGTERM", "SIGINT"] as const) {
	process.on(sig, () => {
		log("shutting down", { signal: sig });
		server.close();
		// Unfinished work stays pending in storage and resumes on the next start.
		void harness.close(BACKGROUND_CONTEXT).finally(() => process.exit(0));
	});
}
