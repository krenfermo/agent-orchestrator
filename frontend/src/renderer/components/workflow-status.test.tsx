import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { I18nextProvider } from "react-i18next";
import { createAppI18n } from "../i18n/instance";
import {
	WorkflowActions,
	WorkflowCompletionSummary,
	WorkflowExecutionLocation,
	WorkflowRepairInline,
	WorkflowStatusPanel,
	WorkflowTechnicalDetails,
	WorkflowTimeline,
} from "./workflow-status";
import type { WorkflowPresentation } from "../lib/workflow-presentation";

/**
 * workflow-status.test.tsx — P3-A §30's UI expectations, asserted on the
 * surface a person actually reads.
 *
 * Every fixture below is a `presentation` object exactly as the daemon emits
 * one. That is deliberate: these tests prove the renderer says the right thing
 * about a given projection, and the daemon's own tests prove the projection is
 * the right one for a given set of durable rows. Neither side gets to be the
 * only place the rule lives.
 */

function renderWith(ui: React.ReactElement) {
	return render(<I18nextProvider i18n={createAppI18n("en")}>{ui}</I18nextProvider>);
}

function presentation(overrides: Partial<WorkflowPresentation> = {}): WorkflowPresentation {
	return {
		stage: "working",
		requiresHuman: false,
		automaticActionActive: false,
		summaryCode: "working",
		technical: { phase: "running", runState: "running" },
		...overrides,
	} as WorkflowPresentation;
}

describe("the human status headline", () => {
	// §3: the sentence is the title. The reason code is still there, and it is
	// underneath.
	it("leads with a human sentence and keeps the technical code secondary", async () => {
		const view = presentation({
			stage: "needs_attention",
			requiresHuman: true,
			summaryCode: "dirty_worktree",
			technical: {
				phase: "needs_attention",
				runState: "needs_attention",
				attentionReason: "dirty_worktree",
				attentionDetail: "Commit, stash or discard the local changes.",
			},
		});
		renderWith(
			<>
				<WorkflowStatusPanel presentation={view} />
				<WorkflowTechnicalDetails presentation={view} />
			</>,
		);
		expect(screen.getByTestId("workflow-status-summary")).toHaveTextContent(
			"There are pending changes in Git. AO cannot start until they are saved.",
		);
		// The code is not the headline...
		expect(screen.getByTestId("workflow-status-summary")).not.toHaveTextContent("dirty_worktree");
		// ...and it is not lost either.
		expect(screen.getByTestId("workflow-technical")).toHaveTextContent("dirty_worktree");
	});

	// A summary code this build has no copy for still says something true, and
	// never renders blank.
	it("falls back to the daemon's own sentence for a code it has no copy for", () => {
		renderWith(
			<WorkflowStatusPanel
				presentation={presentation({
					stage: "needs_attention",
					requiresHuman: true,
					summaryCode: "a_reason_added_after_this_build",
					technical: {
						phase: "needs_attention",
						runState: "needs_attention",
						attentionDetail: "AO could not prove which commit the review approved.",
					},
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-status-summary")).toHaveTextContent(
			"AO could not prove which commit the review approved.",
		);
	});

	// §16: every state answers "what do I do", including the states where the
	// answer is "nothing".
	it("answers what to do in each of the three shapes", () => {
		const { rerender } = renderWith(<WorkflowStatusPanel presentation={presentation()} />);
		expect(screen.getByTestId("workflow-status-guidance")).toHaveTextContent(
			"AO is working. You do not need to do anything.",
		);
		rerender(
			<I18nextProvider i18n={createAppI18n("en")}>
				<WorkflowStatusPanel
					presentation={presentation({ stage: "correcting", automaticActionActive: true, summaryCode: "repair_active" })}
				/>
			</I18nextProvider>,
		);
		expect(screen.getByTestId("workflow-status-guidance")).toHaveTextContent(
			"AO found a problem and is repairing it automatically.",
		);
		rerender(
			<I18nextProvider i18n={createAppI18n("en")}>
				<WorkflowStatusPanel
					presentation={presentation({ stage: "needs_attention", requiresHuman: true, summaryCode: "needs_attention" })}
				/>
			</I18nextProvider>,
		);
		expect(screen.getByTestId("workflow-status-guidance")).toHaveTextContent(
			"AO cannot continue without a decision from you.",
		);
	});

	// §2: stages, and no invented percentage. §29: the state reaches a screen
	// reader as words, not only as a color.
	it("renders the progression as stages with a state each, and no percentage", () => {
		renderWith(
			<WorkflowStatusPanel
				presentation={presentation({
					progress: [
						{ stage: "preparing", state: "completed" },
						{ stage: "working", state: "completed" },
						{ stage: "reviewing", state: "current" },
						{ stage: "correcting", state: "future", optional: true },
						{ stage: "verifying", state: "future" },
					],
				})}
			/>,
		);
		const progress = screen.getByTestId("workflow-progress");
		expect(progress).toHaveTextContent("Reviewing");
		expect(progress).toHaveTextContent("in progress");
		expect(progress.textContent).not.toMatch(/\d+%/);
		expect(progress.querySelector('[data-progress-stage="reviewing"]')).toHaveAttribute(
			"data-progress-state",
			"current",
		);
	});
});

describe("the offered actions", () => {
	// §4/§5: a disabled action is shown WITH its reason. "Where did the button
	// go" is not a question the UI can answer; "why is it greyed out" is.
	it("shows an unavailable action with the reason instead of hiding it", () => {
		renderWith(
			<WorkflowActions
				handlers={{ continue: vi.fn() }}
				presentation={presentation({
					stage: "correcting",
					automaticActionActive: true,
					summaryCode: "repair_active",
					actions: [
						{ id: "continue", enabled: false, disabledReason: "repair_active" },
						{ id: "cancel", enabled: true },
					],
				})}
			/>,
		);
		const button = screen.getByTestId("workflow-action-continue");
		expect(button).toBeDisabled();
		expect(screen.getByTestId("workflow-actions")).toHaveTextContent("AO is already repairing this.");
	});

	// §7/§18: an explicit branch choice is not quietly overridable from the UI
	// either. The offer is present so the rule is visible, and refused.
	it("refuses the worktree remedy for an explicitly chosen branch, and says why", () => {
		renderWith(
			<WorkflowActions
				handlers={{ cancel: vi.fn() }}
				presentation={presentation({
					stage: "waiting",
					summaryCode: "branch_wait",
					actions: [
						{ id: "wait", primary: true, enabled: true },
						{ id: "use_isolated_worktree", enabled: false, disabledReason: "placement_explicit" },
						{ id: "cancel", enabled: true },
					],
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-action-use_isolated_worktree")).toBeDisabled();
		expect(screen.getByTestId("workflow-actions")).toHaveTextContent(
			"You chose this branch explicitly, so AO will not move the work to a worktree on its own.",
		);
	});

	it("runs the handler for an action the daemon enabled", async () => {
		const onCommit = vi.fn();
		renderWith(
			<WorkflowActions
				handlers={{ commit_and_continue: onCommit }}
				presentation={presentation({
					stage: "needs_attention",
					requiresHuman: true,
					summaryCode: "dirty_worktree",
					recommendedAction: "commit_and_continue",
					actions: [{ id: "commit_and_continue", primary: true, enabled: true }],
				})}
			/>,
		);
		await userEvent.click(screen.getByTestId("workflow-action-commit_and_continue"));
		expect(onCommit).toHaveBeenCalledOnce();
	});
});

describe("the execution location", () => {
	// §9: a direct-branch run is never told about a merge target, because there
	// is nothing to merge.
	it("never shows a merge target for a direct-branch run", () => {
		renderWith(
			<WorkflowExecutionLocation
				projectId="proj-1"
				presentation={presentation({
					placement: {
						type: "direct_branch",
						chosenBy: "user",
						repoPath: "/repo",
						executionBranch: "feat/x",
						mergeTarget: "feat/x",
						integrationRequired: false,
			integration: "not_required",
					},
				})}
			/>,
		);
		const location = screen.getByTestId("workflow-location");
		expect(location).toHaveTextContent("Current branch");
		expect(location).toHaveTextContent("feat/x");
		expect(location).not.toHaveTextContent("Integrates into");
		expect(location).not.toHaveTextContent("Worktree");
	});

	// §8: a worktree run says where it is and where the work has to land.
	it("shows the worktree and its integration target for an isolated run", () => {
		renderWith(
			<WorkflowExecutionLocation
				projectId="proj-1"
				presentation={presentation({
					placement: {
						type: "isolated_worktree",
						chosenBy: "automatic",
						repoPath: "/repo",
						executionBranch: "ao/wf-1/t1",
						worktreePath: "/data/ao/worktrees/wf-1",
						mergeTarget: "main",
						integrationRequired: true,
			integration: "pending",
					},
				})}
			/>,
		);
		const location = screen.getByTestId("workflow-location");
		expect(location).toHaveTextContent("Isolated worktree");
		// §10: an automatic decision is visible as one.
		expect(location).toHaveTextContent("AO chose it");
		expect(location).toHaveTextContent("/data/ao/worktrees/wf-1");
		expect(location).toHaveTextContent("Integrates into");
		expect(location).toHaveTextContent("main");
	});

	it("says so plainly when no placement has been frozen yet", () => {
		renderWith(<WorkflowExecutionLocation projectId="proj-1" presentation={presentation()} />);
		expect(screen.getByTestId("workflow-location")).toHaveTextContent(
			"AO has not frozen an execution location for this run yet.",
		);
	});
});

describe("the activity timeline", () => {
	// §15: human lines in order, with the technical qualifier beside them.
	it("renders the daemon's bounded event list in order", () => {
		renderWith(
			<WorkflowTimeline
				events={[
					{ at: "2026-09-01T10:32:00Z", kind: "started" },
					{ at: "2026-09-01T10:33:00Z", kind: "worker_launched", detail: "codex" },
					{ at: "2026-09-01T10:52:00Z", kind: "completed" },
				]}
			/>,
		);
		const timeline = screen.getByTestId("workflow-timeline");
		const lines = timeline.querySelectorAll("li");
		expect(lines).toHaveLength(3);
		expect(lines[0]).toHaveTextContent("Started");
		expect(lines[1]).toHaveTextContent("Agent launched");
		expect(lines[1]).toHaveTextContent("codex");
		expect(lines[2]).toHaveTextContent("Completed");
	});
});

describe("what a finished run reports", () => {
	// §9/§26: the single most important thing a completed direct-branch run must
	// NOT say. The work is already on the branch the user chose; asking them
	// whether to merge it is the confusion this checkpoint exists to remove.
	it("tells a completed direct-branch run there is nothing to integrate", () => {
		renderWith(
			<WorkflowCompletionSummary
				commitSha="abc123def4567"
				presentation={presentation({
					stage: "completed",
					summaryCode: "completed",
					placement: {
						type: "direct_branch",
						chosenBy: "user",
						repoPath: "/repo",
						executionBranch: "feat/x",
						mergeTarget: "feat/x",
						integrationRequired: false,
			integration: "not_required",
					},
				})}
				verdict="approved"
				verificationPassed
			/>,
		);
		const summary = screen.getByTestId("workflow-completion");
		expect(summary).toHaveTextContent("Not required — the work is already on the branch");
		expect(summary).toHaveTextContent("abc123def456");
		expect(summary).toHaveTextContent("Passed");
		expect(summary).not.toHaveTextContent("Verified, not yet integrated");
	});

	// §8: the worktree case is the one where something IS left to do, and it
	// says so instead of leaving the user to discover an orphan branch.
	it("tells a completed worktree run its work still has to be integrated", () => {
		renderWith(
			<WorkflowCompletionSummary
				commitSha={undefined}
				presentation={presentation({
					stage: "completed",
					summaryCode: "completed",
					placement: {
						type: "isolated_worktree",
						chosenBy: "automatic",
						repoPath: "/repo",
						executionBranch: "ao/wf-1/t1",
						mergeTarget: "main",
						integrationRequired: true,
			integration: "pending",
					},
				})}
				verdict="approved"
				verificationPassed
			/>,
		);
		expect(screen.getByTestId("workflow-completion")).toHaveTextContent("Verified, not yet integrated");
	});

	it("renders nothing for a run that has not finished", () => {
		renderWith(
			<WorkflowCompletionSummary
				commitSha={undefined}
				presentation={presentation()}
				verdict={undefined}
				verificationPassed={undefined}
			/>,
		);
		expect(screen.queryByTestId("workflow-completion")).toBeNull();
	});
});

describe("the automatic repair, inline", () => {
	// §6: the repair lives inside the run it repairs, states which attempt of
	// how many, and keeps the audit trail one link away rather than hidden.
	it("shows the attempt count and a way into the repair run", () => {
		renderWith(
			<WorkflowRepairInline
				renderLink={(runId, label) => <a href={`/workflows/${runId}`}>{label}</a>}
				repair={{ active: true, attempt: 2, budget: 3, runId: "wf-repair-2", exhausted: false }}
			/>,
		);
		const panel = screen.getByTestId("workflow-repair");
		expect(panel).toHaveTextContent("Automatic repair");
		expect(panel).toHaveTextContent("Attempt 2 of 3");
		expect(screen.getByRole("link", { name: "View details" })).toHaveAttribute("href", "/workflows/wf-repair-2");
	});

	// A run that never needed a repair shows nothing, not a zeroed row.
	it("renders nothing for a run with no repair", () => {
		renderWith(
			<WorkflowRepairInline
				renderLink={() => null}
				repair={{ active: false, attempt: 0, budget: 3, exhausted: false }}
			/>,
		);
		expect(screen.queryByTestId("workflow-repair")).toBeNull();
	});
});

describe("the execution behind the status (P3-D §24)", () => {
	// An operator diagnosing a run has to be able to say WHICH execution it is
	// about without opening the database: the attempt, whose provider is
	// running it, the session it owns, and what authority AO grants it.
	it("shows the attempt, provider, session and authority in the technical detail", () => {
		const view = presentation({
			technical: {
				phase: "running",
				runState: "running",
				attemptId: "wfa-2858cb4d",
				attemptNumber: 2,
				provider: "claude-code",
				sessionId: "repo-9",
				authority: "active",
				lastEventPhase: "worker_dispatched",
			},
		});
		renderWith(<WorkflowTechnicalDetails presentation={view} />);
		const tech = screen.getByTestId("workflow-technical");
		expect(tech).toHaveTextContent("wfa-2858cb4d");
		expect(tech).toHaveTextContent("#2");
		expect(tech).toHaveTextContent("claude-code");
		expect(tech).toHaveTextContent("repo-9");
		expect(tech).toHaveTextContent("active");
		expect(tech).toHaveTextContent("worker_dispatched");
	});

	// And a fact the daemon does not hold renders as nothing at all. A blank
	// row labelled "Provider" reads as a finding; an absent one reads as what
	// it is.
	it("renders no row for an execution fact the daemon did not send", () => {
		const view = presentation({
			technical: { phase: "running", runState: "running", attemptId: "wfa-1" },
		});
		renderWith(<WorkflowTechnicalDetails presentation={view} />);
		const tech = screen.getByTestId("workflow-technical");
		expect(tech).toHaveTextContent("wfa-1");
		expect(tech).not.toHaveTextContent("Provider");
		expect(tech).not.toHaveTextContent("Session");
	});
});
