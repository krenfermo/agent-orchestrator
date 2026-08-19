import type { CapacitySnapshot } from "../hooks/useCapacity";
import type { ProviderDescriptor, ProviderProfile } from "../hooks/useProviderProfiles";

/** Whether AO can even find the provider's CLI/binary on this instance. */
export type InstalledState = "installed" | "not_installed" | "unknown";

/** Whether this AO user's isolated runtime-home is authenticated. */
export type AccountState = "connected" | "not_connected" | "auth_error" | "unknown";

/** Whether AO can actually route work to this provider right now. */
export type ExecutionState = "ready" | "disabled" | "setup_required" | "unavailable" | "capacity_limited" | "testing";

export type ProviderStatus = {
	installed: InstalledState;
	account: AccountState;
	execution: ExecutionState;
};

/**
 * Derives the three distinct provider-connection axes (Checkpoint 8P-E.1
 * Phase 2) from real backend signals only — never invents a state. `busy`
 * covers both the in-flight Test Connection call and Connect, since both
 * re-probe the same underlying auth state.
 */
export function deriveProviderStatus(
	descriptor: Pick<ProviderDescriptor, "available">,
	profile: Pick<ProviderProfile, "authState" | "enabled"> | undefined,
	capacity: Pick<CapacitySnapshot, "state"> | undefined,
	busy: boolean,
): ProviderStatus {
	if (!descriptor.available) {
		return { installed: "unknown", account: "unknown", execution: "unavailable" };
	}
	if (!profile) {
		return { installed: "unknown", account: "unknown", execution: "setup_required" };
	}

	const installed: InstalledState = profile.authState === "not_installed" ? "not_installed" : "installed";

	let account: AccountState;
	switch (profile.authState) {
		case "authenticated":
			account = "connected";
			break;
		case "unauthenticated":
			account = "not_connected";
			break;
		case "error":
			account = "auth_error";
			break;
		case "not_installed":
			account = "not_connected";
			break;
		default:
			account = "unknown";
	}

	let execution: ExecutionState;
	if (busy) {
		execution = "testing";
	} else if (installed === "not_installed") {
		execution = "unavailable";
	} else if (!profile.enabled) {
		execution = "disabled";
	} else if (account !== "connected") {
		execution = "setup_required";
	} else if (capacity && (capacity.state === "limited" || capacity.state === "cooldown")) {
		execution = "capacity_limited";
	} else if (capacity && capacity.state === "unavailable") {
		execution = "unavailable";
	} else {
		execution = "ready";
	}

	return { installed, account, execution };
}
