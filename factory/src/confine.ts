// The built-in read/write/edit tools run inside the factory process, which is
// root (it needs SETUID to run commands as the sandbox user). NodeExecutionEnv
// accepts any absolute path, so every file operation goes through confine():
// the path, with symlinks resolved, must stay inside the job's directory.
// Files the factory creates are handed to the sandbox user.
import { chownSync, existsSync, lstatSync, realpathSync } from "node:fs";
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import type { Context } from "@earendil-works/chord";
import { err, type ExecutionEnv, FileError } from "@earendil-works/pi-durable/env";
import { NodeExecutionEnv } from "@earendil-works/pi-durable/env/node";

export class PathEscape extends Error {}

// confine returns the real path of p (relative to cwd) if it lies within
// root; otherwise it throws PathEscape.
export function confine(root: string, cwd: string, p: string): string {
	if (p.startsWith("~")) throw new PathEscape(`${p}: home-relative paths are not allowed`);
	const realRoot = realpathSync(root);
	const abs = isAbsolute(p) ? resolve(p) : resolve(cwd, p);
	// Resolve the deepest existing ancestor; the rest does not exist yet.
	let existing = abs;
	const rest: string[] = [];
	while (!existsSync(existing) && !isSymlink(existing)) {
		const parent = dirname(existing);
		if (parent === existing) break;
		rest.unshift(basename(existing));
		existing = parent;
	}
	// A dangling symlink would be followed by a write; refuse it.
	if (!existsSync(existing)) throw new PathEscape(`${p} goes through a dangling symlink`);
	const real = join(realpathSync(existing), ...rest);
	const rel = relative(realRoot, real);
	if (rel === ".." || rel.startsWith(`..${sep}`) || isAbsolute(rel)) {
		throw new PathEscape(`${p} is outside the job directory`);
	}
	return real;
}

function isSymlink(p: string): boolean {
	try {
		return lstatSync(p).isSymbolicLink();
	} catch {
		return false;
	}
}

// Methods whose leading arguments are paths, and how many.
const PATH_ARGS: Record<string, number> = {
	absolutePath: 1,
	readTextFile: 1,
	openTextLineReader: 1,
	readTextLines: 1,
	readBinaryFile: 1,
	openBinaryReader: 1,
	writeFile: 1,
	appendFile: 1,
	truncateFile: 1,
	flushFile: 1,
	renameFile: 2,
	fileInfo: 1,
	listDir: 1,
	openDirReader: 1,
	canonicalPath: 1,
	exists: 1,
	createDir: 1,
	remove: 1,
};
const CREATES = new Set(["writeFile", "appendFile", "createDir", "renameFile"]);

export type ConfinedOptions = {
	root: string; // the job directory
	cwd: string; // the checkout
	uid?: number; // owner for files the factory creates; undefined leaves them
};

// confinedEnv wraps NodeExecutionEnv so every path argument is checked and
// temp files live inside the job directory.
export function confinedEnv(opts: ConfinedOptions): ExecutionEnv {
	const inner = new NodeExecutionEnv({ cwd: opts.cwd, shellEnv: {} });
	const tmp = join(opts.root, "tmp");
	const own = (p: string) => {
		if (opts.uid === undefined) return;
		// Hand new files and the directories created for them to the sandbox user.
		let cur = p;
		while (cur.startsWith(opts.root) && cur !== opts.root) {
			try {
				const st = lstatSync(cur);
				if (st.uid === opts.uid) break;
				chownSync(cur, opts.uid, opts.uid);
			} catch {
				break;
			}
			cur = dirname(cur);
		}
	};
	return new Proxy(inner, {
		get(target, prop, receiver) {
			const value = Reflect.get(target, prop, receiver);
			if (typeof prop !== "string" || typeof value !== "function") return value;
			if (prop === "createTempDir") {
				return (prefix: string | undefined, context: Context) =>
					target.createDir(tmp, { recursive: true }, context).then(() => {
						own(tmp);
						return target.joinPath([tmp, `${prefix ?? "tmp"}${Date.now()}${Math.random().toString(36).slice(2, 8)}`], context);
					}).then(async (r) => {
						if (!r.ok) return r;
						const made = await target.createDir(r.value, { recursive: true }, context);
						own(r.value);
						return made.ok ? r : made;
					});
			}
			if (prop === "createTempFile") {
				return async (options: { prefix?: string; suffix?: string } | undefined, context: Context) => {
					await target.createDir(tmp, { recursive: true }, context);
					own(tmp);
					const p = join(tmp, `${options?.prefix ?? "tmp"}${Date.now()}${Math.random().toString(36).slice(2, 8)}${options?.suffix ?? ""}`);
					const w = await target.writeFile(p, "", context);
					own(p);
					return w.ok ? { ok: true, value: p } : w;
				};
			}
			if (prop === "exec") {
				return (command: string | readonly string[], options: { cwd?: string } | undefined, context: Context) => {
					try {
						const cwd = options?.cwd ? confine(opts.root, opts.cwd, options.cwd) : opts.cwd;
						return target.exec(command, { ...options, cwd }, context);
					} catch (e) {
						return Promise.resolve(err(e as Error));
					}
				};
			}
			if (prop === "watch") {
				return () => Promise.resolve(err(new FileError("not_supported", "watch is disabled in the factory")));
			}
			const n = PATH_ARGS[prop];
			if (n === undefined) return value.bind(target);
			return async (...args: unknown[]) => {
				const paths: string[] = [];
				try {
					for (let i = 0; i < n; i++) {
						const real = confine(opts.root, opts.cwd, String(args[i]));
						args[i] = real;
						paths.push(real);
					}
				} catch (e) {
					return err(new FileError("permission_denied", (e as Error).message, String(args[0])));
				}
				const result = await value.apply(target, args);
				if (CREATES.has(prop) && result?.ok) own(paths[paths.length - 1]!);
				return result;
			};
		},
	}) as ExecutionEnv;
}
