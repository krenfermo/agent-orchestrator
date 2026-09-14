// Unit tests for the port-holder takeover decision (P9) and browser ownership. Run with:
//   cd frontend && npx vitest run src/shared/daemon-takeover.test.ts
import { describe, expect, it } from "vitest";
import { DAEMON_SERVICE_NAME, type DaemonProbe } from "./daemon-attach";
import { browserDaemonOwnershipDecision, decidePortHolderTakeover } from "./daemon-takeover";

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
