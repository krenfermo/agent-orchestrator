// Replacing a daemon the app did not launch (P11 Stage 2 incident, 2026-09-16).
// Run with:
//   cd frontend && npx vitest run --config vite.renderer.config.ts src/shared/daemon-replace.test.ts
import { describe, expect, it } from "vitest";
import { DAEMON_SERVICE_NAME, type DaemonProbe } from "./daemon-attach";
import {
	REPLACEMENT_RETRY_DELAYS_MS,
	awaitPreviousDaemonExit,
	classifyDaemonStartRefusal,
	decideReplacementRetry,
	type ProcessState,
} from "./daemon-replace";

const probe = (pid: number): DaemonProbe => ({ status: "ok", service: DAEMON_SERVICE_NAME, pid });

type Timeline = {
	/** Virtual ms at which /healthz stops answering (the HTTP server closed). */
	httpClosedAt: number;
	/** Virtual ms at which the process leaves "alive"; Infinity = never. */
	exitsAt: number;
	/** What the process is once it has left "alive". */
	afterExit?: ProcessState;
	/** A different daemon that starts answering on the port at this time. */
	foreignPidAt?: number;
	/** Process-state lookups fail (ps unavailable). */
	stateUnknown?: boolean;
};

function simulate(t: Timeline) {
	let clock = 0;
	const deps = {
		pid: 13668,
		port: 3002,
		now: () => clock,
		sleep: async (ms: number) => {
			clock += ms;
		},
		probe: async () => {
			if (t.foreignPidAt !== undefined && clock >= t.foreignPidAt) return probe(4242);
			return clock < t.httpClosedAt ? probe(13668) : null;
		},
		processState: async (): Promise<ProcessState> => {
			if (t.stateUnknown) return "unknown";
			return clock < t.exitsAt ? "alive" : (t.afterExit ?? "gone");
		},
		timeoutMs: 30_000,
		pollMs: 100,
	};
	return deps;
}

describe("awaitPreviousDaemonExit", () => {
	it("does not report the daemon gone while its process (and so daemon.lock) is still alive", async () => {
		// The incident: HTTP closed at once, the process unwound for a while longer
		// holding <data>/daemon.lock, and the replacement spawned into that gap.
		const result = await awaitPreviousDaemonExit(simulate({ httpClosedAt: 0, exitsAt: 300 }));
		expect(result.ok).toBe(true);
		expect(result.ok && result.waitedMs).toBeGreaterThanOrEqual(300);
	});

	it("waits while the daemon still answers /healthz", async () => {
		const result = await awaitPreviousDaemonExit(simulate({ httpClosedAt: 1_000, exitsAt: 1_200 }));
		expect(result.ok && result.waitedMs).toBeGreaterThanOrEqual(1_200);
	});

	it("treats a zombie as exited: its descriptors, and the flock, are already released", async () => {
		// A daemon whose parent never reaps it (e.g. a monitor that spawned it) stays
		// a zombie; kill(pid, 0) still succeeds, so liveness alone would wait forever.
		const result = await awaitPreviousDaemonExit(simulate({ httpClosedAt: 0, exitsAt: 200, afterExit: "zombie" }));
		expect(result).toEqual({ ok: true, waitedMs: 200 });
	});

	it("fails closed when the process never exits", async () => {
		const result = await awaitPreviousDaemonExit(simulate({ httpClosedAt: 0, exitsAt: Infinity }));
		expect(result.ok).toBe(false);
		expect(!result.ok && result.reason).toContain("13668");
	});

	it("fails closed when process state cannot be determined", async () => {
		const result = await awaitPreviousDaemonExit(simulate({ httpClosedAt: 0, exitsAt: 0, stateUnknown: true }));
		expect(result.ok).toBe(false);
	});

	it("fails closed at once when a different daemon answers the port while waiting", async () => {
		const deps = simulate({ httpClosedAt: 0, exitsAt: Infinity, foreignPidAt: 500 });
		const result = await awaitPreviousDaemonExit(deps);
		expect(result.ok).toBe(false);
		expect(!result.ok && result.reason).toContain("4242");
		expect(deps.now()).toBeLessThan(1_000);
	});
});

describe("classifyDaemonStartRefusal", () => {
	it("recognizes the daemon's own lock refusals verbatim", () => {
		expect(
			classifyDaemonStartRefusal(
				'time=... msg="daemon starting"\nanother AO daemon holds data dir /Users/x/.ao/data; refusing to start\n',
			),
		).toBe("data_dir_lock");
		expect(
			classifyDaemonStartRefusal("another AO daemon owns run-file /Users/x/.ao/dev/running.json; refusing to start"),
		).toBe("run_file_lock");
	});

	it("does not treat a live-daemon refusal or any other failure as a lock refusal", () => {
		expect(
			classifyDaemonStartRefusal(
				"daemon already running for data dir /d (pid 1, port 3002, run-file /r); refusing to start",
			),
		).toBeNull();
		expect(classifyDaemonStartRefusal("invalid AO_PORT 0: out of range 1-65535")).toBeNull();
		expect(classifyDaemonStartRefusal("")).toBeNull();
	});

	it("only the last refusal counts (a log file keeps older runs' lines)", () => {
		expect(
			classifyDaemonStartRefusal(
				"another AO daemon holds data dir /d; refusing to start\ndaemon already running for data dir /d (pid 1); refusing to start",
			),
		).toBeNull();
	});
});

describe("decideReplacementRetry", () => {
	const refusal = "another AO daemon holds data dir /Users/x/.ao/data; refusing to start";
	const base = {
		replacedPid: 13668,
		attempt: 0,
		exitCode: 1,
		signal: null,
		output: refusal,
		portProbe: null,
		liveRunFilePid: null,
	};

	it("retries a lock refusal right after a proven replacement, with bounded backoff", () => {
		expect(decideReplacementRetry(base)).toEqual({ action: "retry", delayMs: REPLACEMENT_RETRY_DELAYS_MS[0] });
		expect(decideReplacementRetry({ ...base, attempt: 2 })).toEqual({
			action: "retry",
			delayMs: REPLACEMENT_RETRY_DELAYS_MS[2],
		});
		expect(decideReplacementRetry({ ...base, attempt: REPLACEMENT_RETRY_DELAYS_MS.length }).action).toBe("fail");
	});

	it("never retries when no replacement was in progress", () => {
		expect(decideReplacementRetry({ ...base, replacedPid: null }).action).toBe("fail");
	});

	it("never retries a crash, a signal, or an unrecognized failure", () => {
		expect(decideReplacementRetry({ ...base, signal: "SIGKILL", exitCode: null }).action).toBe("fail");
		expect(decideReplacementRetry({ ...base, exitCode: 0 }).action).toBe("fail");
		expect(decideReplacementRetry({ ...base, output: "panic: boom" }).action).toBe("fail");
	});

	it("fails closed when another daemon has claimed the port or the run-file meanwhile", () => {
		const other = decideReplacementRetry({ ...base, portProbe: probe(777) });
		expect(other.action).toBe("fail");
		expect(other.action === "fail" && other.reason).toContain("777");
		expect(decideReplacementRetry({ ...base, liveRunFilePid: 888 }).action).toBe("fail");
	});
});
