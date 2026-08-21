import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowBoardCard } from "./ProjectWorkflowLane";
import type { BoardWorkflow } from "../hooks/useProjectBoard";

function boardWorkflow(overrides: Partial<BoardWorkflow> = {}): BoardWorkflow {
	return {
		workflowId: "wf-1",
		projectId: "agent-orchestrator",
		objective: "WF2 Backup/Restore",
		state: "running",
		phase: "running",
		executionMode: "autonomous",
		lastActivityAt: new Date().toISOString(),
		reviewCycles: 0,
		tasksTotal: 0,
		tasksCompleted: 0,
		tasksRunning: 0,
		tasksBlocked: 0,
		tasksEligible: 0,
		tasksFailed: 0,
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
});
