// @vitest-environment node
import { describe, expect, it } from "vitest";
import { shouldSignalAttention, shouldToast, type NotificationType } from "./notification-signals";

const ALL_TYPES: NotificationType[] = [
	"needs_input",
	"ready_to_merge",
	"pr_merged",
	"pr_closed_unmerged",
	"task_completed",
	"workflow_completed",
	"task_needs_attention",
	"workflow_needs_attention",
	"task_failed",
	"workflow_failed",
];

describe("shouldToast", () => {
	it("fires a toast for every backend notification type", () => {
		for (const type of ALL_TYPES) {
			expect(shouldToast({ title: `${type} title` }, true)).toBe(true);
		}
	});

	it("does not toast without a title or when notifications are unsupported", () => {
		expect(shouldToast({ title: "" }, true)).toBe(false);
		expect(shouldToast({}, true)).toBe(false);
		expect(shouldToast({ title: "needs input" }, false)).toBe(false);
	});
});

describe("shouldSignalAttention", () => {
	it("signals for the actionable types", () => {
		expect(shouldSignalAttention("needs_input")).toBe(true);
		expect(shouldSignalAttention("ready_to_merge")).toBe(true);
	});

	it("does not bounce/flash for informational PR outcomes", () => {
		expect(shouldSignalAttention("pr_merged")).toBe(false);
		expect(shouldSignalAttention("pr_closed_unmerged")).toBe(false);
	});

	// Finished work is news the toast already delivers. Nothing is waiting on
	// the user, so bouncing the dock would interrupt with nothing to act on —
	// and a fleet of tasks finishing would make it constant.
	it("does not bounce/flash for completions", () => {
		expect(shouldSignalAttention("task_completed")).toBe(false);
		expect(shouldSignalAttention("workflow_completed")).toBe(false);
	});

	// A run that stopped on a decision, or ended without doing the work, is
	// waiting on the user in exactly the way a blocked agent is: nothing moves
	// again until they look.
	it("signals for stops and failures", () => {
		expect(shouldSignalAttention("task_needs_attention")).toBe(true);
		expect(shouldSignalAttention("workflow_needs_attention")).toBe(true);
		expect(shouldSignalAttention("task_failed")).toBe(true);
		expect(shouldSignalAttention("workflow_failed")).toBe(true);
	});

	it("does not signal for unknown or missing types", () => {
		expect(shouldSignalAttention("some_future_type")).toBe(false);
		expect(shouldSignalAttention(undefined)).toBe(false);
	});
});
