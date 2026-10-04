import { describe, expect, it } from "vitest";
import { deriveProviderStatus } from "./provider-status";

describe("deriveProviderStatus", () => {
	it("never reports Ready merely because a profile exists with unknown auth state", () => {
		const status = deriveProviderStatus({ available: true }, { authState: "unknown", enabled: true }, undefined, false);
		expect(status.execution).not.toBe("ready");
		expect(status.execution).toBe("setup_required");
	});

	it("reports Ready only when installed, enabled, authenticated, and capacity is not limited", () => {
		const status = deriveProviderStatus(
			{ available: true },
			{ authState: "authenticated", enabled: true },
			{ state: "available" },
			false,
		);
		expect(status).toEqual({ installed: "installed", account: "connected", execution: "ready" });
	});

	it("reports Setup required for enabled-but-unauthenticated (never Ready from binary presence alone)", () => {
		const status = deriveProviderStatus({ available: true }, { authState: "unauthenticated", enabled: true }, undefined, false);
		expect(status.execution).toBe("setup_required");
		expect(status.account).toBe("not_connected");
	});

	it("distinguishes not-installed from merely unauthenticated", () => {
		const status = deriveProviderStatus({ available: true }, { authState: "not_installed", enabled: true }, undefined, false);
		expect(status.installed).toBe("not_installed");
		expect(status.execution).toBe("unavailable");
	});

	it("reports Disabled when connected but not enabled", () => {
		const status = deriveProviderStatus({ available: true }, { authState: "authenticated", enabled: false }, undefined, false);
		expect(status.account).toBe("connected");
		expect(status.execution).toBe("disabled");
	});

	it("reports Capacity limited only for an otherwise-ready profile hitting a real capacity signal", () => {
		const status = deriveProviderStatus(
			{ available: true },
			{ authState: "authenticated", enabled: true },
			{ state: "limited" },
			false,
		);
		expect(status.execution).toBe("capacity_limited");
	});

	it("reports Testing while an action is in flight, taking priority over other states", () => {
		const status = deriveProviderStatus({ available: true }, { authState: "unauthenticated", enabled: true }, undefined, true);
		expect(status.execution).toBe("testing");
	});

	it("reports auth_error distinctly from not_connected", () => {
		const status = deriveProviderStatus({ available: true }, { authState: "error", enabled: true }, undefined, false);
		expect(status.account).toBe("auth_error");
	});

	it("reports unavailable for a provider with no real adapter, regardless of profile", () => {
		const status = deriveProviderStatus({ available: false }, undefined, undefined, false);
		expect(status.execution).toBe("unavailable");
	});
});
