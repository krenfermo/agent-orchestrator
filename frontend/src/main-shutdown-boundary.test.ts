// @vitest-environment node
//
// P9 boundary, pinned at the source: the desktop app has exactly one place that
// sends /shutdown, it proves ownership before the request, and every supervisor
// link (which makes a daemon stop itself when the app quits) carries a proven
// identity and a verify gate. A new path that bypasses either fails here.
import { readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

const SRC = path.resolve(__dirname);
const main = readFileSync(path.join(SRC, "main.ts"), "utf8");

function sources(dir: string): string[] {
	return readdirSync(dir).flatMap((name) => {
		const full = path.join(dir, name);
		if (statSync(full).isDirectory()) return name === "landing" || name === "node_modules" ? [] : sources(full);
		return /\.(ts|tsx|mjs)$/.test(name) && !/\.test\./.test(name) ? [full] : [];
	});
}

describe("desktop app daemon-termination boundary (P9 §15)", () => {
	it("sends /shutdown from exactly one place, and only after proving ownership", () => {
		const hits = sources(SRC).flatMap((file) =>
			readFileSync(file, "utf8")
				.split("\n")
				.filter((line) => /fetch\(.*\/shutdown/.test(line))
				.map((line) => `${path.relative(SRC, file)}: ${line.trim()}`),
		);
		expect(hits).toHaveLength(1);
		expect(hits[0]).toMatch(/^main\.ts:/);
		const fn = main.slice(main.indexOf("async function gracefullyReplaceDaemonForBrowser"));
		const proofAt = fn.indexOf("await proveDaemonOwnership(launch, port)");
		const refuseAt = fn.indexOf('if (proof.verdict !== "verified") throw new DaemonOwnershipRefusal(proof)');
		const fetchAt = fn.indexOf("/shutdown`");
		expect(proofAt).toBeGreaterThan(-1);
		expect(refuseAt).toBeGreaterThan(proofAt);
		expect(fetchAt).toBeGreaterThan(refuseAt);
	});

	it("links the supervisor only through the verified, gated helper", () => {
		expect(main.match(/connectSupervisor\(/g)).toHaveLength(1);
		const link = main.slice(main.indexOf("function establishSupervisorLink"));
		expect(link.slice(0, link.indexOf("\n}\n"))).toContain("verify,");
		const calls = main.match(/(?<!function )establishSupervisorLink\([^)]*\)/g) ?? [];
		expect(calls.length).toBeGreaterThan(0);
		for (const call of calls) {
			expect(call).toMatch(/establishSupervisorLink\(launch, \{ pid: proof\.pid, port: proof\.port, instanceId: proof\.instanceId \}\)/);
		}
	});

	it("signals only the daemon child this launch spawned", () => {
		const kills = main.split("\n").filter((line) => /process\.kill\(|\.kill\("SIG/.test(line));
		for (const line of kills) {
			expect(line).toMatch(/process\.kill\(pid, 0\)|process\.kill\(-child\.pid, "SIGTERM"\)|child\.kill\("SIG(TERM|KILL)"\)/);
		}
	});
});
