import { describe, expect, it } from "vitest";
import { isCapacityWaitReason } from "./workflow-wake-reason";

describe("isCapacityWaitReason", () => {
	it("is false for no reason", () => {
		expect(isCapacityWaitReason(undefined)).toBe(false);
		expect(isCapacityWaitReason(null)).toBe(false);
		expect(isCapacityWaitReason("")).toBe(false);
	});

	// Checkpoint 8P-E.3: the routine autonomous heartbeat is not a capacity
	// wait and must never be labeled as one.
	it("is false for autonomous_progress", () => {
		expect(isCapacityWaitReason("autonomous_progress")).toBe(false);
	});

	it("is true for every known capacity-shaped reason", () => {
		for (const reason of [
			"capacity_reset",
			"capacity_probe",
			"transient_retry",
			"question_resolver_capacity",
			"reviewer_capacity",
			"worker_capacity",
			"planner_capacity",
		]) {
			expect(isCapacityWaitReason(reason)).toBe(true);
		}
	});

	it("fails open (treats as capacity) for an unrecognized future reason", () => {
		expect(isCapacityWaitReason("some_future_reason")).toBe(true);
	});
});
