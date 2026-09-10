import { describe, expect, it } from "vitest";
import { classifyTrustValidity, trustValidityTone } from "./trust-validity";

const now = new Date("2026-06-01T12:00:00Z");
const past = "2026-01-01T00:00:00Z";
const future = "2027-01-01T00:00:00Z";

describe("classifyTrustValidity", () => {
	it("is active inside an open-ended window", () => {
		expect(classifyTrustValidity({ status: "active", validFrom: past }, now)).toEqual({
			state: "active",
			stillVerifiesHistory: true,
		});
	});

	it("is not-yet-valid when validFrom is in the future", () => {
		expect(classifyTrustValidity({ status: "active", validFrom: future }, now).state).toBe(
			"not-yet-valid",
		);
	});

	it("is expired once validUntil has passed", () => {
		expect(
			classifyTrustValidity({ status: "active", validFrom: past, validUntil: past }, now).state,
		).toBe("expired");
	});

	it("is retired when the status says so and the window is still open", () => {
		expect(
			classifyTrustValidity({ status: "retired", validFrom: past, validUntil: future }, now).state,
		).toBe("retired");
	});

	// The distinction the whole file exists for: closing a window on schedule
	// keeps history, and revoking repudiates it.
	it("keeps history for expired and retired, and refuses it for revoked", () => {
		for (const input of [
			{ status: "active", validFrom: past, validUntil: past },
			{ status: "retired", validFrom: past },
		]) {
			expect(classifyTrustValidity(input, now).stillVerifiesHistory).toBe(true);
		}
		expect(classifyTrustValidity({ status: "revoked", validFrom: past }, now)).toEqual({
			state: "revoked",
			stillVerifiesHistory: false,
		});
	});

	// Revoked wins over every window comparison, exactly as the backend's
	// UsableAt does: a revoked key is refused whatever its dates say.
	it("reports revoked even when the window looks fine or has not opened", () => {
		expect(classifyTrustValidity({ status: "revoked", validFrom: future }, now).state).toBe(
			"revoked",
		);
		expect(
			classifyTrustValidity({ status: "revoked", validFrom: past, validUntil: future }, now).state,
		).toBe("revoked");
	});

	it("treats an absent validUntil as no expiry rather than as expired", () => {
		expect(classifyTrustValidity({ status: "active", validFrom: past }, now).state).toBe("active");
		expect(
			classifyTrustValidity({ status: "active", validFrom: past, validUntil: null }, now).state,
		).toBe("active");
	});

	it("does not classify on an unparseable date", () => {
		expect(classifyTrustValidity({ status: "active", validFrom: "not a date" }, now).state).toBe(
			"active",
		);
	});

	// Colour is a second signal, never the only one.
	it("gives revoked and the in-between states distinct tones", () => {
		expect(trustValidityTone("active")).toBe("success");
		expect(trustValidityTone("revoked")).toBe("error");
		for (const s of ["expired", "retired", "not-yet-valid"] as const) {
			expect(trustValidityTone(s)).toBe("warning");
		}
	});
});
