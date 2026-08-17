import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowCapacityWaitBanner, type WorkflowRunView } from "./workflow-capacity-wait-banner";

function baseRun(overrides: Partial<WorkflowRunView>): WorkflowRunView {
	return {
		id: "wf-1",
		projectId: "p",
		objective: "ship the thing",
		state: "waiting",
		createdAt: new Date().toISOString(),
		updatedAt: new Date().toISOString(),
		...overrides,
	};
}

describe("WorkflowCapacityWaitBanner", () => {
	it("renders nothing when no wake is open", () => {
		const { container } = render(<WorkflowCapacityWaitBanner run={baseRun({})} />);
		expect(container).toBeEmptyDOMElement();
	});

	it("never fabricates a wake time — omitted entirely without nextWakeAt even if waitReason is set", () => {
		const { container } = render(<WorkflowCapacityWaitBanner run={baseRun({ waitReason: "worker_capacity" })} />);
		expect(container).toBeEmptyDOMElement();
	});

	it("shows the real reason, attempt count, and next retry time when a wake is open", () => {
		render(
			<WorkflowCapacityWaitBanner
				run={baseRun({
					waitReason: "worker_capacity",
					nextWakeAt: "2026-01-01T00:05:00.000Z",
					wakeAttemptCount: 3,
				})}
			/>,
		);
		expect(screen.getByText("Waiting for capacity")).toBeInTheDocument();
		expect(screen.getByText("Worker")).toBeInTheDocument();
		expect(screen.getByText("Attempt 3")).toBeInTheDocument();
	});

	it("falls back to the raw reason string for an unrecognized reason value", () => {
		render(
			<WorkflowCapacityWaitBanner
				run={baseRun({ waitReason: "some_future_reason", nextWakeAt: "2026-01-01T00:05:00.000Z" })}
			/>,
		);
		expect(screen.getByText("some_future_reason")).toBeInTheDocument();
	});
});
