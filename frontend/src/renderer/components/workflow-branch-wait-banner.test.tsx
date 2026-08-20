import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowBranchWaitBanner, type WorkflowRunView } from "./workflow-branch-wait-banner";
import { WorkflowCapacityWaitBanner } from "./workflow-capacity-wait-banner";

function baseRun(overrides: Partial<WorkflowRunView>): WorkflowRunView {
	return {
		id: "wf-2",
		projectId: "agent-orchestrator",
		objective: "ship the control center",
		state: "waiting",
		createdAt: new Date().toISOString(),
		updatedAt: new Date().toISOString(),
		executionMode: "autonomous",
		...overrides,
	};
}

describe("WorkflowBranchWaitBanner", () => {
	it("renders nothing when the run is not waiting on a branch", () => {
		const { container } = render(<WorkflowBranchWaitBanner run={baseRun({})} />);
		expect(container).toBeEmptyDOMElement();
	});

	it("names the branch and the workflow that currently owns it", () => {
		render(
			<WorkflowBranchWaitBanner
				run={baseRun({
					branchWait: {
						branch: "feat/engineering-control-center",
						repoPath: "/repos/agent-orchestrator",
						heldByWorkflowRunId: "WF-1",
					},
				})}
			/>,
		);
		expect(screen.getByText("Waiting for branch")).toBeInTheDocument();
		expect(screen.getByText("feat/engineering-control-center")).toBeInTheDocument();
		expect(screen.getByText("Currently used by WF-1")).toBeInTheDocument();
		expect(screen.getByText("/repos/agent-orchestrator")).toBeInTheDocument();
	});

	// A branch wait is a local blocker, not a provider one. Showing "Waiting for
	// capacity" would send the user to their provider plan for a problem that is
	// really another running workflow.
	it("does not also render as a capacity wait", () => {
		const run = baseRun({
			waitReason: "branch_lock",
			nextWakeAt: "2026-01-01T00:05:00.000Z",
			branchWait: { branch: "main", heldByWorkflowRunId: "WF-1" },
		});
		const { container } = render(<WorkflowCapacityWaitBanner run={run} />);
		expect(container).toBeEmptyDOMElement();
	});
});
