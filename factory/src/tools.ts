// The tools the factory's model gets: pi-durable's read/write/edit (confined
// by confinedEnv) and bash, which runs every command as the sandbox user with
// a clean environment and a fresh egress credential. Nothing here can push,
// open a PR or reach the cluster; publishing is the orchestrator's job.
import { createBashTool, createEditTool, createReadTool, createWriteTool } from "@earendil-works/pi-durable/tools";
import { dirname } from "node:path";
import { defineExtension, type Extension, type ToolRegistration } from "@earendil-works/pi-durable";
import { type Minter, proxyEnv, proxyUrl } from "./egress.ts";

export type SandboxOptions = {
	uid: number; // 0 runs commands as the factory's own user (local development)
	path: string;
	extraEnv: Record<string, string>;
};

// sandboxCommand runs script as uid with no supplementary groups and no
// capabilities. The factory is root with only SETUID/SETGID/CHOWN/KILL/
// DAC_OVERRIDE; the sandbox user can read neither the model key nor the
// publisher token (0400 root).
export function sandboxCommand(script: string, uid: number): string {
	if (uid === 0) return script;
	const quoted = `'${script.replaceAll("'", `'\\''`)}'`;
	return `exec setpriv --reuid=${uid} --regid=${uid} --clear-groups --inh-caps=-all --no-new-privs -- /bin/bash -c ${quoted}`;
}

// A job's conversation runs with cwd <jobDir>/repo; everything else the job
// owns (home, tmp) sits next to it.
export function jobDirOf(cwd: string): string {
	return dirname(cwd);
}

// sandboxEnv is the whole environment a command sees: nothing is inherited.
export function sandboxEnv(jobDir: string, opts: SandboxOptions): Record<string, string> {
	return {
		PATH: opts.path,
		HOME: `${jobDir}/home`,
		TMPDIR: `${jobDir}/tmp`,
		LANG: "C.UTF-8",
		...opts.extraEnv,
	};
}

export type ToolsOptions = SandboxOptions & {
	minter?: Minter;
	timeoutSec: number;
};

export function factoryTools(opts: ToolsOptions): Extension {
	const bash = createBashTool({
		prepare: async (execution, api, context) => {
			const agent = await api.agent(context);
			if (!agent.cwd) throw new Error("conversation has no working directory");
			execution.inheritEnv = false;
			execution.env = sandboxEnv(jobDirOf(agent.cwd), opts);
			if (opts.minter) {
				Object.assign(execution.env, proxyEnv(proxyUrl(opts.minter, "bash", opts.timeoutSec + 30)));
			}
			execution.command = sandboxCommand(execution.command, opts.uid);
		},
	});
	// The model picks bash's timeout (in seconds) and there is no default;
	// clamp it so a hung command cannot hold the job forever.
	const clamped: ToolRegistration = {
		...bash,
		description: `${bash.description} Commands time out after ${opts.timeoutSec} seconds at most.`,
		execute: (args, api, context) => {
			const a = args as { command: string; timeout?: number };
			const timeout = Math.min(a.timeout && a.timeout > 0 ? a.timeout : opts.timeoutSec, opts.timeoutSec);
			return bash.execute({ ...a, timeout }, api as never, context);
		},
	} as ToolRegistration;
	return defineExtension({
		name: "factory-tools",
		tools: [createReadTool(), createWriteTool(), createEditTool(), clamped] as ToolRegistration[],
	});
}
