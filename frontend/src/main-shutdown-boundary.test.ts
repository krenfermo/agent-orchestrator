// @vitest-environment node
//
// P9 boundary, pinned at the source across frontend/src and frontend/scripts:
// exactly one place sends /shutdown and it proves ownership first; exactly one
// place links the supervisor socket (which makes a daemon stop itself when the
// app quits) and every link carries a proven identity and a verify gate; signals
// go only to processes this app spawned. A tripwire, not a proof: it scans code
// lines (comments stripped), so a new shutdown/link/kill path must be added here
// deliberately, with its justification.
import { readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

const FRONTEND = path.resolve(__dirname, "..");
const SRC = path.join(FRONTEND, "src");

function sources(dir: string): string[] {
	return readdirSync(dir).flatMap((name) => {
		const full = path.join(dir, name);
		if (statSync(full).isDirectory()) return ["landing", "node_modules", "dist"].includes(name) ? [] : sources(full);
		return /\.(ts|tsx|mjs|js)$/.test(name) && !/\.test\./.test(name) ? [full] : [];
	});
}

// Code lines only: drop // comments (not the // of a URL), block-comment lines, and JSDoc bodies.
function codeLines(file: string): Array<{ file: string; line: string }> {
	return readFileSync(file, "utf8")
		.split("\n")
		.map((line) => line.replace(/(^|\s)\/\/.*$/, ""))
		.filter((line) => !/^\s*(\*|\/\*)/.test(line) && line.trim() !== "")
		.map((line) => ({ file: path.relative(FRONTEND, file), line: line.trim() }));
}

const ALL = [...sources(SRC), ...sources(path.join(FRONTEND, "scripts"))].flatMap(codeLines);
const main = readFileSync(path.join(SRC, "main.ts"), "utf8");

describe("desktop app daemon-termination boundary (P9 §15)", () => {
	it("mentions the /shutdown control route in exactly one code line, in main.ts", () => {
		// The route as a string literal ending ("/shutdown", '/shutdown', `.../shutdown`), however the request is made.
		const hits = ALL.filter(({ line }) => /\/shutdown["'`?]/.test(line));
		expect(hits.map((h) => `${h.file}: ${h.line}`)).toEqual([
			expect.stringMatching(/^src\/main\.ts: const response = await fetch\(`http:\/\/127\.0\.0\.1:\$\{port\}\/shutdown`, \{$/),
		]);
	});

	it("proves ownership before that request", () => {
		const fn = main.slice(main.indexOf("async function gracefullyReplaceDaemonForBrowser"));
		const proofAt = fn.indexOf("const proof = await proveDaemonOwnership(launch, port);");
		const refuseAt = fn.indexOf('if (proof.verdict !== "verified") throw new DaemonOwnershipRefusal(proof);');
		const fetchAt = fn.indexOf("/shutdown`");
		expect(proofAt).toBeGreaterThan(-1);
		expect(refuseAt).toBeGreaterThan(proofAt);
		expect(fetchAt).toBeGreaterThan(refuseAt);
	});

	it("links the supervisor socket from one gated helper only, always with a proven identity", () => {
		const socketHits = ALL.filter(({ line }) => /supervise\.sock|ao-supervise|connectSupervisor\(/.test(line)).map(
			(h) => h.file,
		);
		expect(new Set(socketHits)).toEqual(new Set(["src/main.ts", "src/main/supervisor-link.ts"]));
		expect(main.match(/connectSupervisor\(/g)).toHaveLength(1);
		const helper = main.slice(main.indexOf("function establishSupervisorLink"));
		expect(helper.slice(0, helper.indexOf("\n}\n"))).toContain("verify,");
		const calls = main.match(/(?<!function )establishSupervisorLink\([^)]*\)/g) ?? [];
		expect(calls.length).toBeGreaterThan(0);
		for (const call of calls) {
			expect(call).toBe("establishSupervisorLink(launch, { pid: proof.pid, port: proof.port, instanceId: proof.instanceId })");
		}
	});

	it("signals only processes this app spawned", () => {
		const kills = ALL.filter(({ line }) => /process\.kill\(|\.kill\(|killpg/.test(line)).map((h) => `${h.file}: ${h.line}`);
		// Each allowed site and why it is not a foreign daemon:
		const allowed = [
			/^src\/main\.ts: process\.kill\(pid, 0\);$/, // liveness probe, signal 0
			/^src\/main\.ts: child\.kill\("SIGKILL"\);$/, // login-shell env probe this app spawned
			/^src\/main\.ts: process\.kill\(-child\.pid, "SIGTERM"\);$/, // killDaemon: this launch's own daemon child group
			/^src\/main\.ts: child\.kill\("SIGTERM"\);$/, // killDaemon fallback, same child
			/^src\/main\/process-state\.ts: kill: Kill = \(p, s\) => process\.kill\(p, s\),$/, // readProcessState uses signal 0 only
			/^src\/main\/agent-browser-runtime\.ts: (process\.kill\(pid, 0\);|child\.kill\(\);)$/, // browser runtime children, not daemons
		];
		for (const k of kills) expect(allowed.some((re) => re.test(k)), k).toBe(true);
	});
});
