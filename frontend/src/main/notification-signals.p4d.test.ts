import { describe, expect, it } from "vitest";
import { shouldSignalAttention, shouldToast } from "./notification-signals";

// The session-scoped kinds P4-D added. All three pass the same test the set
// applies: AO has no autonomous move left, so nothing happens until the user
// looks.

describe("session-scoped notification signals", () => {
	it.each(["human_question_required", "repair_exhausted", "integration_failed"])(
		"%s demands attention",
		(type) => {
			expect(shouldSignalAttention(type)).toBe(true);
		},
	);

	// shouldToast is deliberately type-independent, so a new backend type can
	// never silently lose its toast. Asserting it here is what keeps that true
	// for the three types P4-D added.
	it.each(["human_question_required", "repair_exhausted", "integration_failed"])(
		"%s still fires an OS toast",
		(type) => {
			expect(shouldToast({ title: `a ${type} happened` }, true)).toBe(true);
		},
	);

	// The counterexample that keeps the set meaningful: finished work is news,
	// not a demand, so it must not bounce the dock.
	it("leaves completions on the toast-only side", () => {
		expect(shouldSignalAttention("task_completed")).toBe(false);
		expect(shouldSignalAttention("workflow_completed")).toBe(false);
	});
});
