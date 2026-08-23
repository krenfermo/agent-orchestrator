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
		executionMode: "manual",
		phase: "waiting",
		lastActivityAt: new Date().toISOString(),
		canContinue: false,
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

	// Checkpoint 8P-E.3: a real autonomous run showed "Waiting for capacity"
	// while a routine autonomous_progress heartbeat was pending and Claude's
	// own health record said available/dispatch-succeeded -- the banner must
	// not render for this reason even though nextWakeAt is set.
	it("renders nothing for a routine autonomous_progress heartbeat wake", () => {
		const { container } = render(
			<WorkflowCapacityWaitBanner
				run={baseRun({ waitReason: "autonomous_progress", nextWakeAt: "2026-01-01T00:05:00.000Z", wakeAttemptCount: 69 })}
			/>,
		);
		expect(container).toBeEmptyDOMElement();
	});

	// The normalized capacity projection: the banner must report the provider
	// policy the backend derived, not re-derive it from the wake reason. This
	// is the wf-57f90ff2 shape -- a reviewer wait whose real cause is one stale
	// transient launch failure AO is now re-probing.
	it("renders the normalized capacity wait: reason, independence, known reset and provider health age", () => {
		render(
			<WorkflowCapacityWaitBanner
				run={baseRun({
					waitReason: "reviewer_capacity",
					nextWakeAt: "2026-01-01T00:05:00.000Z",
					wakeAttemptCount: 3,
					capacityWait: {
						role: "reviewer",
						reason: "provider_health_stale",
						independenceRequired: true,
						nextAttemptAt: "2026-01-01T00:05:00.000Z",
						knownResetAt: "2026-01-01T01:00:00.000Z",
						attempt: 4,
						probing: true,
						providers: [
							{
								profileId: "prof-claude",
								provider: "anthropic",
								harness: "claude-code",
								displayName: "Claude",
								capacity: "unknown",
								healthState: "cooldown",
								healthReason: "agent_start_failed (unknown)",
								failureClass: "agent_start_failed",
								healthAgeSeconds: 10_800,
								recovery: "cooldown",
								probeEligible: true,
							},
						],
					},
				})}
			/>,
		);
		expect(screen.getByText(/re-checking the provider/i)).toBeInTheDocument();
		expect(screen.getByText(/An independent reviewer is required/i)).toBeInTheDocument();
		expect(screen.getByText(/Known reset:/i)).toBeInTheDocument();
		// The projection's own attempt count wins over the raw wake row's.
		expect(screen.getByText("Attempt 4")).toBeInTheDocument();
		expect(screen.getByText(/Claude.*observed 3h 00m ago.*agent_start_failed/i)).toBeInTheDocument();
	});
});
