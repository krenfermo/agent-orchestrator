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
