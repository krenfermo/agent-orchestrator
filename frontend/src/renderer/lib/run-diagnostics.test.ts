import { describe, expect, it } from "vitest";
import type { components } from "../../api/schema";
import { buildRunDiagnostics, formatRunDiagnostics } from "./run-diagnostics";

type WorkflowRunDetailView = components["schemas"]["WorkflowRunDetailView"];

/**
 * Two things are under test, and the second matters more than the first.
 *
 *  1. The bundle carries the facts an operator would otherwise open SQLite for:
 *     run id, step, attempt, reason code, decisions, evidence references and
 *     the actions AO does and does not offer.
 *  2. It does NOT carry the things that would make pasting it unsafe. Those
 *     assertions are the reason run-diagnostics.ts is an allowlist, and they are
 *     written so that widening it silently would fail here.
 */
function detail(overrides: Partial<WorkflowRunDetailView> = {}): WorkflowRunDetailView {
	return {
		run: {
			id: "wf-1234abcd",
			projectId: "proj-7",
			objective: "Fix the flaky checkout test",
			state: "needs_attention",
			phase: "blocked",
			executionMode: "autonomous",
			canContinue: false,
			createdAt: "2026-09-01T10:00:00.000Z",
			lastActivityAt: "2026-09-01T10:30:00.000Z",
		},
		steps: [],
		...overrides,
	} as WorkflowRunDetailView;
}

describe("buildRunDiagnostics", () => {
	it("carries the run identity and the stop, in the daemon's own vocabulary", () => {
		const d = buildRunDiagnostics(
			detail({
				run: {
					...detail().run,
					attentionReason: "verify_ambiguous",
					attention: "human_decision",
				},
				presentation: {
					stage: "needs_attention",
					summaryCode: "verify_ambiguous",
					requiresHuman: true,
					automaticActionActive: false,
					technical: { errorClass: "verify_ambiguous", attemptId: "att-9", attemptNumber: 2 },
				},
			} as Partial<WorkflowRunDetailView>),
		);
		expect(d.runId).toBe("wf-1234abcd");
		expect(d.attentionReason).toBe("verify_ambiguous");
		expect(d.errorClass).toBe("verify_ambiguous");
		expect(d.attemptId).toBe("att-9");
		expect(d.attemptNumber).toBe(2);
	});

	it("carries the refused actions with their reasons, not just the offered ones", () => {
		const d = buildRunDiagnostics(
			detail({
				advice: {
					category: "human_action",
					requiresHuman: true,
					automaticActionActive: false,
					repairSpent: 1,
					repairBudget: 3,
					repairable: false,
					retryable: false,
					authority: {},
					availableActions: ["cancel"],
					blockedActions: [{ action: "repair", reason: "repair_exhausted" }],
				},
			} as Partial<WorkflowRunDetailView>),
		);
		expect(d.availableActions).toEqual(["cancel"]);
		expect(d.blockedActions).toEqual([{ action: "repair", reason: "repair_exhausted" }]);
		expect(formatRunDiagnostics(d)).toContain("blocked:repair: repair_exhausted");
	});

	it("carries each step's evidence reference, including an unprovable change set", () => {
		const d = buildRunDiagnostics(
			detail({
				steps: [
					{
						id: "wfs-1",
						kind: "review",
						ordinal: 3,
						state: "failed",
						createdAt: "2026-09-01T10:10:00.000Z",
						updatedAt: "2026-09-01T10:20:00.000Z",
						reviewPolicy: {
							decision: "required",
							complexity: "low",
							evaluatedAt: "2026-09-01T10:15:00.000Z",
							policyVersion: "v3",
							reasons: ["change_set_unprovable"],
							facts: {
								acceptanceCriteriaEmpty: false,
								changedFileCount: 0,
								changedFilePaths: [],
								changedFilesUnprovable: true,
								unprovableChangeSetReason: "the work step recorded no base commit",
								hasExactContentCheckForSoleChangedFile: false,
								priorWorkProviderAttempts: 1,
								verifyCommandCount: 1,
								verifyFileCheckCount: 0,
							},
						},
						attempts: [
							{
								id: "att-1",
								attemptNumber: 1,
								startedAt: "2026-09-01T10:11:00.000Z",
								outcome: "failed",
								errorClass: "verify_ambiguous",
								harness: "claude-code",
								model: "opus",
							},
						],
					},
				],
			} as Partial<WorkflowRunDetailView>),
		);
		const step = d.steps[0];
		expect(step.ordinal).toBe(3);
		expect(step.changedFilesUnprovable).toBe(true);
		expect(step.unprovableChangeSetReason).toBe("the work step recorded no base commit");
		expect(step.attempts[0].errorClass).toBe("verify_ambiguous");

		const text = formatRunDiagnostics(d);
		expect(text).toContain("## Step 3 — review (failed)");
		expect(text).toContain("changeSet: unprovable");
		expect(text).toContain("attempt 1: failed · verify_ambiguous · claude-code/opus");
	});

	// --- Redaction. These are the assertions that keep the bundle pasteable. ---

	it("reduces an absolute worktree path to its basename, so no home directory travels", () => {
		const d = buildRunDiagnostics(
			detail({
				presentation: {
					stage: "working",
					summaryCode: "working",
					requiresHuman: false,
					automaticActionActive: false,
					technical: {},
					placement: {
						type: "isolated_worktree",
						chosenBy: "automatic",
						integration: "pending",
						integrationRequired: true,
						worktreePath: "/Users/a-real-person/Projects/ao-worktrees/wf-1234abcd",
						repoPath: "/Users/a-real-person/Projects/medusa",
					},
				},
			} as Partial<WorkflowRunDetailView>),
		);
		expect(d.worktreeName).toBe("wf-1234abcd");
		const text = formatRunDiagnostics(d);
		expect(text).not.toContain("a-real-person");
		expect(text).not.toContain("/Users/");
	});

	it("carries only the objective's first line, never the pasted specification body", () => {
		const d = buildRunDiagnostics(
			detail({
				run: {
					...detail().run,
					objective:
						"Rotate the staging credentials\n\nUse token sk-live-DO-NOT-LEAK-9999 from the vault entry.",
				},
			} as Partial<WorkflowRunDetailView>),
		);
		expect(d.title).toBe("Rotate the staging credentials");
		expect(formatRunDiagnostics(d)).not.toContain("sk-live-DO-NOT-LEAK-9999");
	});

	// The allowlist, asserted as a property rather than as a list: a field the
	// API grows later must not reach the bundle just by existing.
	it("ignores fields it was not explicitly taught, so the API growing cannot widen it", () => {
		const withExtra = detail() as WorkflowRunDetailView & Record<string, unknown>;
		withExtra.someFutureSecret = "AKIA-SHOULD-NEVER-APPEAR";
		(withExtra.run as unknown as Record<string, unknown>).providerToken = "token-should-never-appear";

		const text = formatRunDiagnostics(buildRunDiagnostics(withExtra));
		expect(text).not.toContain("AKIA-SHOULD-NEVER-APPEAR");
		expect(text).not.toContain("token-should-never-appear");
	});

	it("omits absent facts rather than printing empty or invented ones", () => {
		const text = formatRunDiagnostics(buildRunDiagnostics(detail()));
		expect(text).toContain("run: wf-1234abcd");
		expect(text).not.toContain("errorClass:");
		expect(text).not.toContain("undefined");
		expect(text).not.toContain("null");
	});
});
