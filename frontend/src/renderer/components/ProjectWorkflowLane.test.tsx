import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { WorkflowBoardCard } from "./ProjectWorkflowLane";
import type { BoardWorkflow } from "../hooks/useProjectBoard";

function boardWorkflow(overrides: Partial<BoardWorkflow> = {}): BoardWorkflow {
	return {
		workflowId: "wf-1",
		projectId: "agent-orchestrator",
		objective: "WF2 Backup/Restore",
		state: "running",
		phase: "running",
		stage: "working",
		requiresHuman: false,
		automaticActionActive: false,
		executionMode: "autonomous",
		lastActivityAt: new Date().toISOString(),
		lastMeaningfulActivityAt: new Date().toISOString(),
		reviewCycles: 0,
		tasksTotal: 0,
		tasksCompleted: 0,
		tasksRunning: 0,
		tasksBlocked: 0,
		tasksEligible: 0,
		tasksFailed: 0,
		tasksNeedsAttention: 0,
		...overrides,
	};
}

describe("WorkflowBoardCard", () => {
	// The report this whole checkpoint started from: the worker session goes
	// idle for the entire review, and the board said the work was idle too.
	it("says Reviewing while the review step runs, never Inactive", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "reviewing",
					steps: [
						{ kind: "plan", state: "completed" },
						{ kind: "work", state: "completed" },
						{ kind: "review", state: "running" },
						{ kind: "fix", state: "pending" },
						{ kind: "verify", state: "pending" },
					],
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-phase-badge")).toHaveTextContent("Reviewing");
		expect(screen.queryByText(/inactive/i)).toBeNull();
		expect(screen.queryByText(/needs you/i)).toBeNull();
	});

	// A review that asked for changes is progress. AO applies the fix itself,
	// so this must not read as a request for a decision.
	it("does not ask for a human when AO is applying its own fix", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "fixing",
					attention: "ao_internal",
					attentionReason: "review_changes_requested",
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-phase-badge")).toHaveTextContent("Fixing");
		expect(screen.queryByText(/needs you/i)).toBeNull();
	});

	// The genuine case: a stop only the user can clear, with the remedy spelled
	// out rather than left to be guessed.
	it("shows Needs you with a reason and an action for a real human decision", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "needs_attention",
					state: "needs_attention",
					attention: "human_decision",
					attentionReason: "dirty_worktree",
					attentionAction: "Commit, stash or discard the local changes in the target repository.",
				})}
			/>,
		);
		const notice = screen.getByRole("note");
		expect(within(notice).getByText(/needs you/i)).toBeInTheDocument();
		expect(within(notice).getByText(/dirty_worktree/)).toBeInTheDocument();
		expect(within(notice).getByText(/Commit, stash or discard/)).toBeInTheDocument();
	});

	// needs_attention with nothing recorded must stay honest rather than
	// acquiring an invented reason.
	it("says so plainly when needs_attention has no recorded reason", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({ phase: "needs_attention", state: "needs_attention", attention: "human_decision" })}
			/>,
		);
		expect(screen.getByText(/no recorded reason/i)).toBeInTheDocument();
	});

	// Checkpoint 8P-E.13: a planner AO is retrying is AO's own problem. The
	// real MEDUSA objective rendered "Te necesita" for a planner_timeout no
	// human answer could repair; the card must now read as a wait.
	it("reads as a wait, not a request, while AO is retrying", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "retrying",
					state: "pending",
					attention: "ao_internal",
					attentionReason: "planner_retry_scheduled",
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-phase-badge")).toHaveTextContent("Retrying");
		expect(screen.queryByText(/needs you/i)).toBeNull();
	});

	it("names the current task and the total for a master run", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "reviewing",
					tasksTotal: 7,
					tasksCompleted: 1,
					tasksRunning: 1,
					tasksBlocked: 5,
					currentTaskOrdinal: 2,
					currentTaskTitle: "Backend backup API",
					tasks: [
						{ ordinal: 1, title: "Lifecycle mapping", state: "completed" },
						{ ordinal: 2, title: "Backend backup API", state: "running", phase: "reviewing" },
						{ ordinal: 3, title: "Backend tests", state: "blocked" },
					],
				})}
			/>,
		);
		expect(screen.getByText(/Task 2 of 7/)).toBeInTheDocument();
		const tasks = within(screen.getByTestId("workflow-child-tasks")).getAllByRole("listitem");
		expect(tasks).toHaveLength(3);
		expect(tasks[2]).toHaveTextContent("Blocked");
	});

	// The seven planner statuses parallel execution created. A task state alone
	// cannot express any of them: two tasks that are both "running" read the
	// same whether or not they are running together, and a task queued for the
	// integration lane reads as "running" while nothing is running at all.
	it("labels each planner status a task can be in", () => {
		const statuses = [
			["running_in_parallel", "Running in parallel"],
			["waiting_for_dependency", "Waiting for dependency"],
			["waiting_for_conflict", "Waiting for conflict"],
			["ready_to_integrate", "Ready to integrate"],
			["integrating", "Integrating"],
			["conflict", "Conflict"],
			["integrated", "Integrated"],
		] as const;
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					tasksTotal: statuses.length,
					tasks: statuses.map(([status], index) => ({
						ordinal: index + 1,
						title: `Task ${index + 1}`,
						state: "running" as const,
						planner: {
							status,
							dependencies: [],
							integrationDependencies: [],
							parallelGroup: 1,
							parallelGroupSize: statuses.length,
							writeScope: { writePaths: [], readPaths: [], packages: [], components: [], files: [] },
						},
					})),
				})}
			/>,
		);
		for (const [status, label] of statuses) {
			expect(screen.getByTestId(`workflow-task-planner-${status}`)).toHaveTextContent(label);
		}
	});

	// The badge is additive. A task the daemon has nothing planner-level to say
	// about keeps the row it always had, with its state and nothing else.
	it("adds no planner badge to a task with no planner status", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					tasksTotal: 1,
					tasks: [{ ordinal: 1, title: "Lifecycle mapping", state: "blocked" }],
				})}
			/>,
		);
		const row = within(screen.getByTestId("workflow-child-tasks")).getAllByRole("listitem")[0];
		expect(row).toHaveTextContent("Blocked");
		expect(row.querySelector('[data-slot="badge"]')).toBeNull();
	});

	// The checklist is what makes "where is this run" legible at a glance, and
	// the never-executed advance step must not appear in it.
	it("renders the plan/work/review/fix/verify checklist", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					steps: [
						{ kind: "plan", state: "completed" },
						{ kind: "work", state: "completed" },
						{ kind: "review", state: "running" },
						{ kind: "fix", state: "pending" },
						{ kind: "verify", state: "pending" },
					],
				})}
			/>,
		);
		const checklist = within(screen.getByTestId("workflow-step-checklist"));
		for (const label of ["Plan", "Work", "Review", "Fix", "Verify"]) {
			expect(checklist.getByText(label)).toBeInTheDocument();
		}
		expect(checklist.queryByText(/advance/i)).toBeNull();
	});

	// A diagnostic-only field: shows the run this card is tracking so it can be
	// cross-referenced against logs, without becoming part of the card's design.
	it("shows the workflow run id as a read-only diagnostic field", () => {
		render(<WorkflowBoardCard workflow={boardWorkflow({ workflowId: "wf-diagnostic-123" })} />);
		expect(screen.getByTestId("workflow-run-id")).toHaveTextContent("wf-diagnostic-123");
	});

	// The value is a bare id, so the field needs a name on hover and for
	// assistive tech without changing what the card shows.
	it("names the run id field for hover and assistive tech", () => {
		render(<WorkflowBoardCard workflow={boardWorkflow({ workflowId: "wf-diagnostic-123" })} />);
		expect(screen.getByTestId("workflow-run-id")).toHaveAttribute(
			"title",
			"Autonomous workflow run ID",
		);
	});

	it("surfaces the wait reason and the scheduled retry for a capacity wait", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "waiting_for_capacity",
					state: "waiting",
					attention: "ao_internal",
					waitReason: "reviewer_capacity",
					nextWakeAt: new Date(Date.now() + 60_000).toISOString(),
				})}
			/>,
		);
		expect(screen.getByText(/reviewer_capacity/)).toBeInTheDocument();
		expect(screen.queryByText(/needs you/i)).toBeNull();
	});
	// Checkpoint 8P-E.13A: "Blocked" on its own could mean a branch about to be
	// handed over or one held by a workflow that stopped hours ago. The card
	// names the branch, the holder, and which of the two this is.
	it("names the branch and holder a blocked run is queued on", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "blocked",
					state: "waiting",
					attention: "ao_internal",
					waitReason: "branch_lock",
					branchWait: {
						branch: "feat/engineering-control-center",
						repoPath: "/repos/agent-orchestrator",
						heldByWorkflowRunId: "wf-3220567f",
						heldByState: "running",
						autoResume: true,
					},
				})}
			/>,
		);
		const wait = within(screen.getByTestId("workflow-branch-wait"));
		expect(wait.getByText(/feat\/engineering-control-center/)).toBeInTheDocument();
		expect(wait.getByText(/wf-3220567f/)).toBeInTheDocument();
		expect(screen.queryByText(/needs you/i)).toBeNull();
	});

	// The one case where the queue is genuinely stuck: the holder needs a
	// decision, so the card says the branch is not coming back on its own —
	// still without claiming THIS run is the one needing a decision.
	it("says a branch held by a stopped workflow will not free itself", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "blocked",
					state: "waiting",
					attention: "ao_internal",
					branchWait: {
						branch: "feat/engineering-control-center",
						heldByWorkflowRunId: "wf-3220567f",
						heldByState: "needs_attention",
						heldByReason: "the owning workflow needs a human decision",
						autoResume: false,
					},
				})}
			/>,
		);
		const wait = within(screen.getByTestId("workflow-branch-wait"));
		expect(wait.getByText(/continued or cancelled/i)).toBeInTheDocument();
		expect(screen.queryByText(/needs you/i)).toBeNull();
	});
	// The whole complaint in one test: a run whose worker session is idle for
	// the entire review is still live work, and the card has to look like it —
	// accent, spinner, and a block saying what is happening this second.
	it("marks an actively progressing run as active even while its worker session is idle", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "reviewing",
					harness: "claude-code",
					currentTaskTitle: "Backend backup API",
					steps: [
						{ kind: "plan", state: "completed" },
						{ kind: "work", state: "completed" },
						{ kind: "review", state: "running" },
					],
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-card-wf-1")).toHaveAttribute("data-active", "true");
		expect(screen.getByTestId("workflow-spinner")).toBeInTheDocument();
		const panel = screen.getByTestId("workflow-activity-panel");
		expect(panel).toHaveTextContent("Working right now");
		expect(panel).toHaveTextContent("AO is reviewing the changes");
		expect(panel).toHaveTextContent("Backend backup API");
	});

	it("shows a finished run as done, with no spinner and no activity block", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "completed",
					state: "completed",
					steps: [
						{ kind: "plan", state: "completed" },
						{ kind: "work", state: "completed" },
						{ kind: "review", state: "completed" },
					],
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-phase-badge")).toHaveTextContent("Completed");
		expect(screen.queryByTestId("workflow-spinner")).toBeNull();
		expect(screen.queryByTestId("workflow-activity-panel")).toBeNull();
		expect(screen.getByTestId("workflow-card-wf-1")).not.toHaveAttribute("data-active");
		const checklist = within(screen.getByTestId("workflow-step-checklist"));
		expect(checklist.getAllByText("done")).toHaveLength(3);
	});

	it("shows a needs_attention run as a warning, never as work in progress", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "needs_attention",
					state: "needs_attention",
					attention: "human_decision",
					attentionReason: "dirty_worktree",
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-phase-badge")).toHaveTextContent("Needs attention");
		expect(screen.queryByTestId("workflow-spinner")).toBeNull();
		expect(screen.queryByTestId("workflow-activity-panel")).toBeNull();
		expect(screen.getByRole("note").querySelector("svg")).not.toBeNull();
	});

	it("shows a queued run as neutral: no spinner, no accent, no activity block", () => {
		render(<WorkflowBoardCard workflow={boardWorkflow({ phase: "queued", state: "pending" })} />);
		expect(screen.getByTestId("workflow-phase-badge")).toHaveTextContent("Queued");
		expect(screen.queryByTestId("workflow-spinner")).toBeNull();
		expect(screen.queryByTestId("workflow-activity-panel")).toBeNull();
		expect(screen.getByTestId("workflow-card-wf-1")).not.toHaveAttribute("data-active");
	});

	// Branch ownership is not execution. A run queued behind a lock must be
	// named as such and must not borrow any of the active treatment.
	it("says a branch wait is a wait, not work being executed", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					phase: "blocked",
					state: "waiting",
					attention: "ao_internal",
					waitReason: "branch_lock",
					branchWait: {
						branch: "feat/engineering-control-center",
						heldByWorkflowRunId: "wf-3220567f",
						autoResume: true,
					},
				})}
			/>,
		);
		const wait = within(screen.getByTestId("workflow-branch-wait"));
		expect(wait.getByText(/waiting for branch/i)).toBeInTheDocument();
		expect(screen.queryByTestId("workflow-spinner")).toBeNull();
		expect(screen.queryByTestId("workflow-activity-panel")).toBeNull();
		expect(screen.getByTestId("workflow-card-wf-1")).not.toHaveAttribute("data-active");
	});
});

describe("WorkflowBoardCard cancel-and-archive", () => {
	// The action is only offered where it is meaningful. A workflow AO is
	// actively driving is not stale, and the renderer never offers to retire it.
	it("offers the action for a stale workflow and withholds it for a running one", () => {
		const { rerender } = render(
			<WorkflowBoardCard
				onArchive={async () => {}}
				workflow={boardWorkflow({ workflowId: "wf-stale", state: "needs_attention", phase: "needs_attention" })}
			/>,
		);
		expect(screen.getByTestId("workflow-archive-wf-stale")).toBeInTheDocument();

		rerender(
			<WorkflowBoardCard
				onArchive={async () => {}}
				workflow={boardWorkflow({ workflowId: "wf-live", state: "running", phase: "running" })}
			/>,
		);
		expect(screen.queryByTestId("workflow-archive-wf-live")).toBeNull();
	});

	// The confirmation has to say both halves: execution stops, history stays.
	it("confirms before archiving and explains that history is preserved", async () => {
		const user = userEvent.setup();
		const onArchive = vi.fn().mockResolvedValue(undefined);
		render(
			<WorkflowBoardCard
				onArchive={onArchive}
				workflow={boardWorkflow({ workflowId: "wf-stale", state: "needs_attention", phase: "needs_attention" })}
			/>,
		);

		await user.click(screen.getByTestId("workflow-archive-wf-stale"));
		const dialog = await screen.findByRole("dialog");
		expect(within(dialog).getByText(/cancel and archive this workflow\?/i)).toBeInTheDocument();
		expect(within(dialog).getByText(/nothing is deleted/i)).toBeInTheDocument();
		expect(onArchive).not.toHaveBeenCalled();

		await user.click(within(dialog).getByRole("button", { name: /cancel and archive/i }));
		expect(onArchive).toHaveBeenCalledWith("wf-stale");
	});

	// An archived card is history: it renders, and it offers no way to stop
	// something that is already stopped.
	it("never offers the action on an archived card", () => {
		render(
			<WorkflowBoardCard
				archived
				onArchive={async () => {}}
				workflow={boardWorkflow({ workflowId: "wf-old", state: "cancelled", phase: "cancelled" })}
			/>,
		);
		expect(screen.queryByTestId("workflow-archive-wf-old")).toBeNull();
	});
});

/**
 * P3-B: everything the card says about status comes from the daemon's
 * `presentation`. These tests pin that down — the card must not compute a
 * status, must not offer an action the daemon did not offer, and must show a
 * repair under the run it repairs rather than beside it.
 */
describe("WorkflowBoardCard — the shared projection", () => {
	function presentation(overrides: Partial<NonNullable<BoardWorkflow["presentation"]>> = {}) {
		return {
			stage: "needs_attention",
			requiresHuman: true,
			automaticActionActive: false,
			summaryCode: "dirty_worktree",
			recommendedAction: "commit_and_continue",
			actions: [
				{ id: "commit_and_continue", primary: true, enabled: true },
				{ id: "cancel", enabled: true },
			],
			progress: [
				{ stage: "preparing", state: "completed" },
				{ stage: "working", state: "blocked" },
				{ stage: "completed", state: "future" },
			],
			technical: {},
			...overrides,
		} as NonNullable<BoardWorkflow["presentation"]>;
	}

	it("renders the daemon's sentence and stage rather than a state name", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					state: "needs_attention",
					phase: "needs_attention",
					stage: "needs_attention",
					requiresHuman: true,
					presentation: presentation(),
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-stage-badge")).toHaveTextContent("Needs you");
		expect(screen.getByTestId("workflow-status-summary")).toHaveTextContent(
			"There are pending changes in Git",
		);
		expect(screen.getByTestId("workflow-progress")).toBeInTheDocument();
	});

	it("names the recommended action the daemon offers, and no other", () => {
		render(<WorkflowBoardCard workflow={boardWorkflow({ presentation: presentation() })} />);
		expect(screen.getByTestId("workflow-recommended-action")).toHaveTextContent("Commit and continue");
	});

	// §8: a recommendation the daemon offers DISABLED must not appear on the
	// card — the Board may never present an action the run page would refuse.
	it("hides a recommended action the daemon offers disabled", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					presentation: presentation({
						recommendedAction: "repair",
						actions: [{ id: "repair", enabled: false, disabledReason: "repair_active" }],
					}),
				})}
			/>,
		);
		expect(screen.queryByTestId("workflow-recommended-action")).toBeNull();
	});

	// §6: a repair belongs under the run it repairs, with attempt N of M and a
	// way into its own run — and with none of the origin's buttons.
	it("shows repairs inline under their origin, with a link to each repair run", async () => {
		const onOpenRun = vi.fn();
		render(
			<WorkflowBoardCard
				onOpenRun={onOpenRun}
				workflow={boardWorkflow({
					presentation: presentation({ stage: "correcting", requiresHuman: false, automaticActionActive: true, summaryCode: "repair_active", recommendedAction: undefined, actions: [] }),
					repairs: [
						{
							workflowId: "wf-repair-1",
							attempt: 1,
							budget: 3,
							stage: "working",
							requiresHuman: false,
							state: "running",
							active: true,
							lastMeaningfulActivityAt: new Date().toISOString(),
						},
					],
				})}
			/>,
		);
		const repair = screen.getByTestId("workflow-repair-wf-repair-1");
		expect(repair).toHaveTextContent("Attempt 1 of 3");
		await userEvent.click(screen.getByTestId("workflow-repair-open-wf-repair-1"));
		expect(onOpenRun).toHaveBeenCalledWith("wf-repair-1");
	});

	// §15/§16: the card names the frozen placement and the specific integration
	// answer. A direct-branch run is never told anything is pending.
	it("shows the frozen placement and says nothing about integrating a direct-branch run", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					presentation: presentation({
						placement: {
							type: "direct_branch",
							chosenBy: "user",
							executionBranch: "feat/x",
							integrationRequired: false,
							integration: "not_required",
						},
					}),
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-placement")).toHaveTextContent("feat/x");
		expect(screen.queryByTestId("workflow-integration")).toBeNull();
	});

	it("says an isolated worktree is not yet integrated", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					presentation: presentation({
						placement: {
							type: "isolated_worktree",
							chosenBy: "automatic",
							executionBranch: "ao/wf-1",
							integrationRequired: true,
							integration: "pending",
						},
					}),
				})}
			/>,
		);
		expect(screen.getByTestId("workflow-integration")).toHaveTextContent("Not yet integrated");
	});

	// §11: the card reports the run's last MEANINGFUL act. A bookkeeping write a
	// second ago must not make a run stalled for hours read as active.
	it("reports the last meaningful activity, not the last write", () => {
		const hoursAgo = new Date(Date.now() - 5 * 60 * 60 * 1000).toISOString();
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({
					lastActivityAt: new Date().toISOString(),
					lastMeaningfulActivityAt: hoursAgo,
					presentation: presentation(),
				})}
			/>,
		);
		expect(screen.queryByText(/Last activity just now/)).toBeNull();
	});

	// §17: a card never carries a whole specification.
	it("says a long specification is only summarised here", () => {
		render(
			<WorkflowBoardCard
				workflow={boardWorkflow({ objectiveTruncated: true, presentation: presentation() })}
			/>,
		);
		expect(screen.getByTestId("workflow-spec-hint")).toBeInTheDocument();
	});
});
