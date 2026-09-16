// Pure decision helper for the wedged-orphan kill+replace path.
//
// Context: on app launch, after both attach attempts fail (inspectExistingDaemon
// and resolveDaemonFromPort both returned null/non-ready), a process may still
// be holding the daemon port. Spawning a new daemon then makes the Go child
// collide on the port and exit 1. This helper encodes the decision: kill the
// holder when the run-file names a PID that is still alive (a hung/wedged holder
// that bound the port but is not answering /healthz). The probe disjunct
// (probe !== null) is kept as a defensive guard only: by the time this helper
// is called, resolveDaemonFromPort already returned null, so a holder that
// answers /healthz at this point is unexpected. If it does happen (e.g. a race),
// we still replace it rather than colliding on spawn.
//
// Kept side-effect free and dependency-injected (no node:* or electron imports)
// so it can be exercised in vitest without the Electron polyfill layer.

import type { DaemonProbe } from "./daemon-attach";
import type { RunFileInfo } from "./daemon-discovery";
import type { ProcessState } from "./daemon-replace";

/**
 * What to do about whatever holds the daemon port once both attach paths have
 * returned null (P9).
 *
 * The rule is the one the daemon and the CLI follow: NEVER SIGNAL A PROCESS
 * WHOSE OWNERSHIP IS NOT PROVEN. A PID that is merely alive proves nothing --
 * the OS reuses PIDs -- and signalling its process group can kill an unrelated
 * program. So there is no signal action at all:
 *
 * - nothing answers and no run-file PID is alive          -> spawn;
 * - an AO daemon answers AND it is provably the one the run-file names
 *   (same PID, same instance when both carry one) AND it passes this launch's
 *   identity check                                         -> ask it to shut
 *   down through its own /shutdown endpoint, then spawn;
 * - anything else (a live PID with no answering daemon, a daemon that is not
 *   the run-file's, a daemon of another checkout/installation) -> refuse, touch
 *   nothing, and tell the person which process holds the port.
 */
export type PortHolderDecision =
	| { action: "spawn" }
	| { action: "graceful_shutdown" }
	| { action: "refuse"; reason: string };

export type PortHolderFacts = {
	/** /healthz answer on the daemon port, or null when nothing valid answered. */
	probe: DaemonProbe | null;
	/** The run-file's PID and instance, when a run-file could be read. */
	runFilePid: number | null;
	runFileInstanceId?: string;
	/** Whether the run-file PID is a live process (kill(pid, 0)). */
	runFilePidAlive: boolean;
	/** This launch's identity check on the probe: null when it matches. */
	identityError: string | null;
};

export function decidePortHolderTakeover(facts: PortHolderFacts): PortHolderDecision {
	const { probe, runFilePid, runFileInstanceId, runFilePidAlive, identityError } = facts;
	if (!probe) {
		if (runFilePid && runFilePidAlive) {
			return {
				action: "refuse",
				reason: `the run-file names process ${runFilePid}, which is alive but is not answering as an AO daemon; AO will not signal a process whose ownership it cannot prove. Stop it yourself, then restart the app.`,
			};
		}
		return { action: "spawn" };
	}
	if (!runFilePid || probe.pid !== runFilePid) {
		return {
			action: "refuse",
			reason: `an AO daemon (pid ${probe.pid}) answers on the port but it is not the daemon the run-file names; stop it yourself, then restart the app.`,
		};
	}
	if (runFileInstanceId && probe.instanceId && probe.instanceId !== runFileInstanceId) {
		return {
			action: "refuse",
			reason: `the daemon answering (instance ${probe.instanceId}) is not the incarnation the run-file names (${runFileInstanceId}); stop it yourself, then restart the app.`,
		};
	}
	if (identityError) {
		return { action: "refuse", reason: identityError };
	}
	return { action: "graceful_shutdown" };
}

export type BrowserDaemonOwnershipDecision =
	| { action: "attach" }
	| { action: "replace"; keepAlive: boolean };

/**
 * A daemon can authenticate to only the Electron launch that handed it the
 * memory-only browser runtime token. Reuse within that launch is safe. Across
 * launches we replace it gracefully, preserving the original persistence mode:
 * app-owned daemons remain app-owned; persistent/headless daemons remain alive
 * after the window closes.
 */
export function browserDaemonOwnershipDecision(
	currentAppRunId: string,
	existing: { owner?: string; appRunId?: string },
): BrowserDaemonOwnershipDecision {
	if (existing.appRunId === currentAppRunId) return { action: "attach" };
	return { action: "replace", keepAlive: existing.owner !== "app" };
}

/** Verdicts that prove nothing either way at this moment; worth asking again, never acting on. */
export function isTransientOwnershipVerdict(verdict: ShutdownProof["verdict"]): boolean {
	return verdict === "unhealthy" || verdict === "undetermined";
}

/**
 * P9 §15 for the desktop app: the ONLY condition under which Electron may ask a
 * daemon it did not spawn to stop through /shutdown. It is the same definition
 * of "verified" that `ao stop` uses (backend/internal/cli/daemon_discovery.go
 * inspectRunFile) -- run-file of this data dir, live PID, probe answering as that
 * PID, instance, installation and data dir -- with one tightening because the
 * stop is automatic: every identity field must be PRESENT on both sides. A
 * daemon too old to state its identity is verified enough for a person running
 * `ao stop`; it is not verified enough for an app to stop it on its own.
 *
 * Any verdict other than "verified" means: send nothing, touch nothing, tell the
 * person which daemon holds the data dir.
 */
export type ShutdownProofFacts = {
	/** The port the caller is about to POST /shutdown to. */
	port: number;
	/** The run-file this launch reads (its provenance), parsed; null when absent or unreadable. */
	runFile: RunFileInfo | null;
	/** The run-file PID's process state (zombie = exited). */
	runFilePidState: ProcessState;
	/** /healthz on `port`, or null when nothing valid answered. */
	probe: DaemonProbe | null;
	/** The data dir this launch's own daemon serves; null when it cannot be resolved. */
	expectedDataDir: string | null;
	/** This launch's checkout / bundle / configured-binary identity check on the probe (null = ours). */
	launchIdentityError: string | null;
	/** Canonical comparison (symlinks resolved), as the daemon's own sameDataDir. */
	sameDataDir: (a: string, b: string) => boolean;
};

export type ShutdownProof =
	| {
			verdict: "verified";
			pid: number;
			port: number;
			instanceId: string;
			installationId: string;
			dataDir: string;
	  }
	| {
			verdict:
				| "no_provenance"
				| "foreign"
				| "stale"
				| "unhealthy"
				| "undetermined"
				| "running_unverified"
				| "identity_mismatch";
			reason: string;
	  };

export function proveDaemonOwnedForShutdown(f: ShutdownProofFacts): ShutdownProof {
	const rf = f.runFile;
	if (!rf) {
		return {
			verdict: "no_provenance",
			reason: `an AO daemon answers on port ${f.port} but no run-file of this launch names it`,
		};
	}
	if (rf.port !== f.port) {
		return {
			verdict: "running_unverified",
			reason: `the run-file names port ${rf.port}, not the port ${f.port} being stopped`,
		};
	}
	if (!rf.dataDir || !f.expectedDataDir) {
		return { verdict: "no_provenance", reason: "the run-file does not record which data dir its daemon serves" };
	}
	if (!f.sameDataDir(rf.dataDir, f.expectedDataDir)) {
		return {
			verdict: "foreign",
			reason: `the run-file belongs to data dir ${rf.dataDir}, not ${f.expectedDataDir}`,
		};
	}
	if (f.runFilePidState === "gone" || f.runFilePidState === "zombie") {
		return { verdict: "stale", reason: `the run-file names process ${rf.pid}, which has exited` };
	}
	if (f.runFilePidState !== "alive") {
		// Not a mismatch: nothing is proven either way right now (e.g. ps timed out).
		return {
			verdict: "undetermined",
			reason: `the state of run-file process ${rf.pid} cannot be determined right now`,
		};
	}
	const p = f.probe;
	if (!p) return { verdict: "unhealthy", reason: `process ${rf.pid} is alive but no AO daemon answers on port ${f.port}` };
	if (p.pid !== rf.pid) {
		return {
			verdict: "running_unverified",
			reason: `the daemon answering is pid ${p.pid}, but the run-file names ${rf.pid}`,
		};
	}
	if (!p.dataDir) {
		return { verdict: "running_unverified", reason: `the daemon (pid ${p.pid}) does not report its data dir` };
	}
	if (!f.sameDataDir(p.dataDir, f.expectedDataDir)) {
		return { verdict: "foreign", reason: `the daemon (pid ${p.pid}) serves data dir ${p.dataDir}, not ${f.expectedDataDir}` };
	}
	if (!p.instanceId || !rf.instanceId || p.instanceId !== rf.instanceId) {
		return {
			verdict: "running_unverified",
			reason: `the daemon's incarnation (${p.instanceId ?? "unreported"}) is not proven to be the run-file's (${rf.instanceId ?? "unrecorded"})`,
		};
	}
	if (!p.installationId || !rf.installationId || p.installationId !== rf.installationId) {
		return {
			verdict: "running_unverified",
			reason: `the daemon's installation (${p.installationId ?? "unreported"}) is not proven to be the run-file's (${rf.installationId ?? "unrecorded"})`,
		};
	}
	if (f.launchIdentityError) return { verdict: "identity_mismatch", reason: f.launchIdentityError };
	return {
		verdict: "verified",
		pid: rf.pid,
		port: f.port,
		instanceId: p.instanceId,
		installationId: p.installationId,
		dataDir: p.dataDir,
	};
}
