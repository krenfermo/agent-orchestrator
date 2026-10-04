// @vitest-environment node
//
// Real-binary reproduction of the P11 Stage 2 incident: ask a running daemon to
// stop through /shutdown, wait with awaitPreviousDaemonExit, spawn the
// replacement over the same data dir, and count "refusing to start".
//
// Opt-in (spawns real daemons on scratch data dirs, never ~/.ao/data):
//   AO_REPLACE_E2E_BINARY=/path/to/ao AO_REPLACE_E2E_TRIALS=10 \
//   AO_REPLACE_E2E_SEED_DATA=/scratch/restored/data   # optional: cloned per trial; a real
//                                                     # database unwinds long enough to expose the race \
//   AO_REPLACE_E2E_INFLIGHT=1                         # optional: a client mid-request when /shutdown
//                                                     # arrives. Shutdown closes the listener (so /healthz
//                                                     # stops answering) but waits for that connection,
//                                                     # holding daemon.lock, for up to ShutdownTimeout. \
//     npx vitest run --config vite.renderer.config.ts src/shared/daemon-replace.e2e.test.ts
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, openSync, readFileSync, rmSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { readProcessState } from "../main/process-state";
import { parseDaemonProbe, type DaemonProbe } from "./daemon-attach";
import { awaitPreviousDaemonExit } from "./daemon-replace";

const binary = process.env.AO_REPLACE_E2E_BINARY ?? "";
const trials = Number(process.env.AO_REPLACE_E2E_TRIALS ?? "10");
const seedData = process.env.AO_REPLACE_E2E_SEED_DATA ?? "";
const inflight = process.env.AO_REPLACE_E2E_INFLIGHT === "1";

async function freePort(): Promise<number> {
	return new Promise((resolve, reject) => {
		const server = net.createServer();
		server.once("error", reject);
		server.listen(0, "127.0.0.1", () => {
			const { port } = server.address() as net.AddressInfo;
			server.close(() => resolve(port));
		});
	});
}

async function healthz(port: number): Promise<DaemonProbe | null> {
	try {
		const response = await fetch(`http://127.0.0.1:${port}/healthz`, { signal: AbortSignal.timeout(1_000) });
		return response.ok ? parseDaemonProbe("healthz", await response.json()) : null;
	} catch {
		return null;
	}
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

function startDaemon(dir: string, port: number, logName: string): ChildProcess {
	const log = openSync(path.join(dir, logName), "a");
	const env: NodeJS.ProcessEnv = {};
	for (const [key, value] of Object.entries(process.env)) if (!key.startsWith("AO_")) env[key] = value;
	Object.assign(env, {
		AO_DATA_DIR: path.join(dir, "data"),
		AO_RUN_FILE: path.join(dir, "running.json"),
		AO_PORT: String(port),
	});
	return spawn(binary, ["daemon"], { env, cwd: dir, stdio: ["ignore", log, log] });
}

async function waitReady(port: number, child: ChildProcess, timeoutMs = 60_000): Promise<DaemonProbe | null> {
	const deadline = Date.now() + timeoutMs;
	while (Date.now() < deadline) {
		if (child.exitCode !== null || child.signalCode !== null) return null;
		const answer = await healthz(port);
		if (answer && answer.pid === child.pid) return answer;
		await sleep(50);
	}
	return null;
}

function exited(child: ChildProcess): Promise<void> {
	if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
	return new Promise((resolve) => child.once("exit", () => resolve()));
}

describe.skipIf(!binary)("daemon replacement against the real binary", () => {
	it(
		`replacement never collides with the previous daemon's data-dir lock (${trials} trials)`,
		async () => {
			const outcomes: string[] = [];
			for (let trial = 0; trial < trials; trial++) {
				const dir = mkdtempSync(path.join(os.tmpdir(), "ao-replace-e2e-"));
				// APFS clone (-c): a copy-on-write data dir per trial, never the seed itself.
				if (seedData) execFileSync("cp", ["-cR", seedData, path.join(dir, "data")]);
				const port = await freePort();
				const first = startDaemon(dir, port, "first.log");
				let second: ChildProcess | null = null;
				let pending: net.Socket | null = null;
				try {
					const ready = await waitReady(port, first);
					expect(ready, readFileSync(path.join(dir, "first.log"), "utf8")).not.toBeNull();
					if (inflight) {
						// Request line and one header, never the terminating blank line.
						pending = net.connect(port, "127.0.0.1");
						pending.on("error", () => undefined);
						await new Promise<void>((resolve) => pending!.once("connect", () => resolve()));
						pending.write("GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n");
						await sleep(100);
					}
					const response = await fetch(`http://127.0.0.1:${port}/shutdown`, { method: "POST" });
					expect(response.ok).toBe(true);
					const waited = await awaitPreviousDaemonExit({
						pid: ready!.pid,
						port,
						probe: healthz,
						processState: (pid) => readProcessState(pid),
						now: Date.now,
						sleep,
						timeoutMs: 30_000,
						// Poll as tightly as the incident's replacement followed /shutdown (26 ms).
						pollMs: 2,
					});
					expect(waited.ok, JSON.stringify(waited)).toBe(true);
					second = startDaemon(dir, port, "second.log");
					const secondReady = await waitReady(port, second);
					const log = readFileSync(path.join(dir, "second.log"), "utf8");
					outcomes.push(secondReady ? "ready" : /refusing to start/.test(log) ? "refused" : `other: ${log.slice(-300)}`);
				} finally {
					pending?.destroy();
					for (const child of [first, second]) {
						if (child && child.exitCode === null && child.signalCode === null) {
							child.kill("SIGTERM");
							await exited(child);
						}
					}
					rmSync(dir, { recursive: true, force: true });
				}
			}
			expect(outcomes).toEqual(Array(trials).fill("ready"));
		},
		trials * 90_000,
	);
});
