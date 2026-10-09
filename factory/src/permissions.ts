// permissions reads the "+ ..." lines `tapctl diff` prints for a new agent:
// tools with their effects, egress hosts and secrets.
export type Permissions = {
	tools: Record<string, string>; // name -> effects
	egress: string[];
	secrets: string[];
	effects: string[];
};

export function permissions(diff: string): Permissions {
	const tools: Record<string, string> = {};
	const egress = new Set<string>();
	const secrets = new Set<string>();
	for (const line of diff.split("\n")) {
		let m = /^\+ egress (\S+) on /.exec(line);
		if (m) egress.add(m[1]!);
		m = /^\+ secret (\S+) on /.exec(line);
		if (m) secrets.add(m[1]!);
		m = /^\+ tool (\S+) \(effects: (\w+)\)/.exec(line);
		if (m) tools[m[1]!] = m[2]!;
	}
	return { tools, egress: [...egress].sort(), secrets: [...secrets].sort(), effects: [...new Set(Object.values(tools))].sort() };
}
