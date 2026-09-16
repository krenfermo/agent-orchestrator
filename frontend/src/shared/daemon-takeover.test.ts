// Unit tests for the port-holder takeover decision (P9) and browser ownership. Run with:
//   cd frontend && npx vitest run src/shared/daemon-takeover.test.ts
import { describe, expect, it } from "vitest";
import { DAEMON_SERVICE_NAME, type DaemonProbe } from "./daemon-attach";
import type { RunFileInfo } from "./daemon-discovery";
import {
	browserDaemonOwnershipDecision,
	decidePortHolderTakeover,
	proveDaemonOwnedForShutdown,
	type ShutdownProofFacts,
} from "./daemon-takeover";

const probe = (pid: number, instanceId?: string): DaemonProbe => ({
	status: "ok",
	service: DAEMON_SERVICE_NAME,
	pid,
	instanceId,
});

describe("decidePortHolderTakeover (P9: never signal an unverified process)", () => {
	it("spawns when nothing answers and no run-file PID is alive", () => {
		expect(
			decidePortHolderTakeover({ probe: null, runFilePid: null, runFilePidAlive: false, identityError: null }),
		).toEqual({ action: "spawn" });
		expect(
			decidePortHolderTakeover({ probe: null, runFilePid: 4242, runFilePidAlive: false, identityError: null }),
		).toEqual({ action: "spawn" });
	});

	it("refuses -- and never signals -- a live PID that is not answering as an AO daemon (reused or wedged)", () => {
		const decision = decidePortHolderTakeover({
			probe: null,
			runFilePid: 4242,
			runFilePidAlive: true,
			identityError: null,
		});
		expect(decision.action).toBe("refuse");
	});

	it("refuses a daemon that is not the one the run-file names", () => {
		expect(
			decidePortHolderTakeover({ probe: probe(9999), runFilePid: 4242, runFilePidAlive: true, identityError: null })
				.action,
		).toBe("refuse");
		expect(
			decidePortHolderTakeover({ probe: probe(9999), runFilePid: null, runFilePidAlive: false, identityError: null })
				.action,
		).toBe("refuse");
	});

	it("refuses an answering daemon whose instance differs from the run-file's", () => {
		expect(
			decidePortHolderTakeover({
				probe: probe(4242, "aod-answering"),
				runFilePid: 4242,
				runFileInstanceId: "aod-recorded",
				runFilePidAlive: true,
				identityError: null,
			}).action,
		).toBe("refuse");
	});

	it("refuses a verified-PID daemon that fails this launch's identity check", () => {
		expect(
			decidePortHolderTakeover({
				probe: probe(4242, "aod-1"),
				runFilePid: 4242,
				runFileInstanceId: "aod-1",
				runFilePidAlive: true,
				identityError: "Another AO daemon is already running from /elsewhere",
			}),
		).toEqual({ action: "refuse", reason: "Another AO daemon is already running from /elsewhere" });
	});

	it("asks a proven daemon to shut down gracefully -- it has no signal action at all", () => {
		expect(
			decidePortHolderTakeover({
				probe: probe(4242, "aod-1"),
				runFilePid: 4242,
				runFileInstanceId: "aod-1",
				runFilePidAlive: true,
				identityError: null,
			}),
		).toEqual({ action: "graceful_shutdown" });
	});
});

describe("browserDaemonOwnershipDecision", () => {
	it("reuses a daemon authenticated by this app launch", () => {
		expect(browserDaemonOwnershipDecision("run-1", { owner: "app", appRunId: "run-1" })).toEqual({
			action: "attach",
		});
	});

	it("replaces an older app daemon without making it persistent", () => {
		expect(browserDaemonOwnershipDecision("run-2", { owner: "app", appRunId: "run-1" })).toEqual({
			action: "replace",
			keepAlive: false,
		});
	});

	it("preserves persistent and headless daemon lifetime during replacement", () => {
		expect(browserDaemonOwnershipDecision("run-2", { owner: "persistent", appRunId: "run-1" })).toEqual({
			action: "replace",
			keepAlive: true,
		});
		expect(browserDaemonOwnershipDecision("run-2", {})).toEqual({ action: "replace", keepAlive: true });
	});
});

describe("proveDaemonOwnedForShutdown (P9 §15: /shutdown only a verified daemon)", () => {
	const DATA = "/Users/x/.ao/data";
	const runFile = (over: Partial<RunFileInfo> = {}): RunFileInfo => ({
		pid: 4242,
		port: 3002,
		startedAtMs: 1,
		instanceId: "aod-1",
		installationId: "aoi-1",
		dataDir: DATA,
		...over,
	});
	const health = (over: Partial<DaemonProbe> = {}): DaemonProbe => ({
		status: "ok",
		service: DAEMON_SERVICE_NAME,
		pid: 4242,
		instanceId: "aod-1",
		installationId: "aoi-1",
		dataDir: DATA,
		...over,
	});
	const facts = (over: Partial<ShutdownProofFacts> = {}): ShutdownProofFacts => ({
		port: 3002,
		runFile: runFile(),
		runFilePidState: "alive",
		probe: health(),
		expectedDataDir: DATA,
		launchIdentityError: null,
		sameDataDir: (a, b) => a === b,
		...over,
	});

	it("1. same PID + instance + installation + canonical data dir + run-file provenance: verified", () => {
		expect(proveDaemonOwnedForShutdown(facts())).toEqual({
			verdict: "verified",
			pid: 4242,
			port: 3002,
			instanceId: "aod-1",
			installationId: "aoi-1",
			dataDir: DATA,
		});
		// Canonicalization is the caller's sameDataDir (realpath), not string equality.
		const viaSymlink = facts({ runFile: runFile({ dataDir: "/link/data" }), sameDataDir: () => true });
		expect(proveDaemonOwnedForShutdown(viaSymlink).verdict).toBe("verified");
	});

	it("2. PID matches but the instance differs: refused", () => {
		expect(proveDaemonOwnedForShutdown(facts({ probe: health({ instanceId: "aod-2" }) })).verdict).toBe(
			"running_unverified",
		);
	});

	it("3. installation differs: refused", () => {
		expect(proveDaemonOwnedForShutdown(facts({ probe: health({ installationId: "aoi-2" }) })).verdict).toBe(
			"running_unverified",
		);
		expect(proveDaemonOwnedForShutdown(facts({ runFile: runFile({ installationId: "aoi-2" }) })).verdict).toBe(
			"running_unverified",
		);
	});

	it("4. data dir differs (run-file or daemon): foreign, refused", () => {
		expect(proveDaemonOwnedForShutdown(facts({ runFile: runFile({ dataDir: "/other/data" }) })).verdict).toBe(
			"foreign",
		);
		expect(proveDaemonOwnedForShutdown(facts({ probe: health({ dataDir: "/other/data" }) })).verdict).toBe("foreign");
	});

	it("5. a daemon that only answers the port, with no run-file provenance: refused", () => {
		expect(proveDaemonOwnedForShutdown(facts({ runFile: null })).verdict).toBe("no_provenance");
		// The run-file names another port than the one about to be shut down.
		expect(proveDaemonOwnedForShutdown(facts({ port: 3001 })).verdict).toBe("running_unverified");
	});

	it("6. identity_mismatch (another checkout / install / configured binary): refused", () => {
		const verdict = proveDaemonOwnedForShutdown(
			facts({ launchIdentityError: "Another AO daemon is already running from /checkout-a" }),
		);
		expect(verdict.verdict).toBe("identity_mismatch");
	});

	it("7. running_unverified / not ready without a complete identity: refused", () => {
		for (const probe of [
			health({ instanceId: undefined }),
			health({ installationId: undefined }),
			health({ dataDir: undefined }),
			health({ pid: 9999 }),
		]) {
			expect(proveDaemonOwnedForShutdown(facts({ probe })).verdict).toBe("running_unverified");
		}
		for (const rf of [runFile({ instanceId: undefined }), runFile({ installationId: undefined })]) {
			expect(proveDaemonOwnedForShutdown(facts({ runFile: rf })).verdict).toBe("running_unverified");
		}
		expect(proveDaemonOwnedForShutdown(facts({ runFile: runFile({ dataDir: undefined }) })).verdict).toBe(
			"no_provenance",
		);
		expect(proveDaemonOwnedForShutdown(facts({ probe: null })).verdict).toBe("unhealthy");
		expect(proveDaemonOwnedForShutdown(facts({ runFilePidState: "unknown" })).verdict).toBe("running_unverified");
		expect(proveDaemonOwnedForShutdown(facts({ runFilePidState: "gone" })).verdict).toBe("stale");
		expect(proveDaemonOwnedForShutdown(facts({ expectedDataDir: null })).verdict).toBe("no_provenance");
	});
});

describe("P9 shutdown boundary: checkouts and legitimate replacement", () => {
	const DATA = "/Users/x/.ao/dev/data";
	const rf: RunFileInfo = { pid: 7, port: 3002, startedAtMs: 1, instanceId: "aod-a", installationId: "aoi-dev", dataDir: DATA };
	const probeA: DaemonProbe = {
		status: "ok",
		service: DAEMON_SERVICE_NAME,
		pid: 7,
		instanceId: "aod-a",
		installationId: "aoi-dev",
		dataDir: DATA,
		workingDirectory: "/checkouts/A/backend",
	};
	const base: ShutdownProofFacts = {
		port: 3002,
		runFile: rf,
		runFilePidState: "alive",
		probe: probeA,
		expectedDataDir: DATA,
		launchIdentityError: null,
		sameDataDir: (a, b) => a === b,
	};

	it("9. opening checkout B never stops checkout A's fully P9-verified daemon", () => {
		// Same data dir, run-file, PID, instance and installation: verified for the INSTALLATION,
		// but B's launch identity (its checkout) does not match A's daemon.
		const fromB = proveDaemonOwnedForShutdown({
			...base,
			launchIdentityError: "Another AO daemon is already running from /checkouts/A/backend; expected this checkout at /checkouts/B/backend.",
		});
		expect(fromB.verdict).toBe("identity_mismatch");
	});

	it("10. the launch's own previous daemon (same checkout/binary) is still replaced", () => {
		expect(proveDaemonOwnedForShutdown(base).verdict).toBe("verified");
	});
});
