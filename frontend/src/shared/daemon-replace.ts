// Replacing a daemon the app did not launch (P11 Stage 2 incident, 2026-09-16).
//
// The app asks such a daemon to stop through its own /shutdown and then spawns
// its own. The daemon closes its HTTP listener first and releases
// <data>/daemon.lock only when its process exits (the kernel drops the flock
// with the last descriptor). "/healthz stopped answering" therefore does NOT
// mean "the data dir is free": a replacement spawned into that gap refuses to
// start. The previous daemon is gone only when its process is.
//
// Kept side-effect free and dependency-injected (no node:* or electron imports)
// so it can be exercised in vitest with a virtual clock.

import type { DaemonProbe } from "./daemon-attach";

/**
 * alive   -- the process exists and is not a zombie;
 * zombie  -- it exited but its parent has not reaped it: every descriptor, and
 *            so the flock, is already released;
 * gone    -- no such process;
 * unknown -- could not be determined. Never treated as exited.
 */
export type ProcessState = "alive" | "zombie" | "gone" | "unknown";

export type PreviousDaemonExitDeps = {
	/** The PID of the daemon that was asked to stop (proven by its own /healthz). */
	pid: number;
	port: number;
	probe: (port: number) => Promise<DaemonProbe | null>;
	processState: (pid: number) => Promise<ProcessState>;
	now: () => number;
	sleep: (ms: number) => Promise<void>;
	timeoutMs: number;
	pollMs: number;
};

export type PreviousDaemonExitResult = { ok: true; waitedMs: number } | { ok: false; reason: string };

/**
 * The daemon's own graceful-shutdown cap is 10 s (DefaultShutdownTimeout), after
 * which it force-closes and unwinds. Twice that bounds a healthy exit.
 */
export const PREVIOUS_DAEMON_EXIT_TIMEOUT_MS = 20_000;
export const PREVIOUS_DAEMON_EXIT_POLL_MS = 50;

/**
 * Wait until the stopped daemon's process has exited (or is a zombie) and
 * nothing answers /healthz on its port. Fails closed: a process that outlives
 * the deadline, a process whose state cannot be read, or a different daemon
 * answering the port all return { ok: false } and the caller must not spawn.
 */
export async function awaitPreviousDaemonExit(deps: PreviousDaemonExitDeps): Promise<PreviousDaemonExitResult> {
	const start = deps.now();
	const deadline = start + deps.timeoutMs;
	let lastState: ProcessState = "unknown";
	let answering = true;
	for (;;) {
		const answer = await deps.probe(deps.port);
		if (answer && answer.pid !== deps.pid) {
			return {
				ok: false,
				reason: `a different AO daemon (pid ${answer.pid}) answered on port ${deps.port} while waiting for pid ${deps.pid} to exit; AO will not start another over it.`,
			};
		}
		answering = answer !== null;
		lastState = await deps.processState(deps.pid);
		if (!answering && (lastState === "gone" || lastState === "zombie")) {
			return { ok: true, waitedMs: deps.now() - start };
		}
		if (deps.now() >= deadline) break;
		await deps.sleep(deps.pollMs);
	}
	const seconds = Math.round(deps.timeoutMs / 1000);
	if (lastState === "unknown") {
		return {
			ok: false,
			reason: `could not confirm that the previous AO daemon (pid ${deps.pid}) exited within ${seconds} seconds; AO will not start a second daemon over its data dir.`,
		};
	}
	return {
		ok: false,
		reason: `the previous AO daemon (pid ${deps.pid}) did not exit within ${seconds} seconds${answering ? " and still answers /healthz" : ""}; AO will not start a second daemon over its data dir.`,
	};
}

/** Which exclusive-ownership lock a daemon refused to start over, if any. */
export type DaemonStartRefusal = "data_dir_lock" | "run_file_lock";

// Verbatim from backend/internal/daemon/daemon.go. Anything else is not a lock
// refusal and is never retried.
const REFUSALS: ReadonlyArray<[RegExp, DaemonStartRefusal]> = [
	[/another AO daemon holds data dir .+; refusing to start/, "data_dir_lock"],
	[/another AO daemon owns run-file .+; refusing to start/, "run_file_lock"],
];

/** Classify a daemon's startup output. Only the LAST refusal line counts. */
export function classifyDaemonStartRefusal(output: string): DaemonStartRefusal | null {
	const lines = output.split(/\r?\n/).filter((line) => line.includes("refusing to start"));
	const last = lines.at(-1);
	if (!last) return null;
	for (const [pattern, refusal] of REFUSALS) if (pattern.test(last)) return refusal;
	return null;
}

export const REPLACEMENT_RETRY_DELAYS_MS: readonly number[] = [250, 500, 1_000];

export type ReplacementRetryFacts = {
	/** Set only when THIS start stopped a previous daemon and proved its exit. */
	replacedPid: number | null;
	/** 0 for the first spawn after the replacement. */
	attempt: number;
	exitCode: number | null;
	signal: string | null;
	/** The spawned child's own output (its log slice in keep-alive mode). */
	output: string;
	/** Anything now answering /healthz on the daemon port. */
	portProbe: DaemonProbe | null;
	/** A run-file naming a live process, if one exists. */
	liveRunFilePid: number | null;
};

export type ReplacementRetryDecision = { action: "retry"; delayMs: number } | { action: "fail"; reason: string };

/**
 * A spawned replacement exited. Retry only the one condition this replacement
 * can explain: it refused to start over an ownership lock, right after we
 * proved the daemon we replaced had exited, and nothing else has since claimed
 * the port or the run-file. Every other case -- a crash, a signal, an
 * unrecognized message, a competing daemon, exhausted attempts -- fails closed.
 */
export function decideReplacementRetry(facts: ReplacementRetryFacts): ReplacementRetryDecision {
	if (facts.replacedPid === null) {
		return { action: "fail", reason: "the daemon exited and no replacement was in progress" };
	}
	if (facts.signal !== null || facts.exitCode === 0 || facts.exitCode === null) {
		return { action: "fail", reason: "the replacement daemon did not exit with a startup refusal" };
	}
	const refusal = classifyDaemonStartRefusal(facts.output);
	if (!refusal) {
		return { action: "fail", reason: "the replacement daemon failed for a reason other than a held ownership lock" };
	}
	if (facts.portProbe) {
		return {
			action: "fail",
			reason: `another AO daemon (pid ${facts.portProbe.pid}) now answers on the port; AO will not replace it again.`,
		};
	}
	if (facts.liveRunFilePid !== null) {
		return {
			action: "fail",
			reason: `the run-file now names a live process (pid ${facts.liveRunFilePid}); AO will not start over it.`,
		};
	}
	const delayMs = REPLACEMENT_RETRY_DELAYS_MS[facts.attempt];
	if (delayMs === undefined) {
		return {
			action: "fail",
			reason: `the ${refusal === "data_dir_lock" ? "data dir" : "run-file"} lock was still held after ${REPLACEMENT_RETRY_DELAYS_MS.length} retries`,
		};
	}
	return { action: "retry", delayMs };
}
