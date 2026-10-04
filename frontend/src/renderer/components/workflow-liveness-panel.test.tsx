import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowLivenessPanel } from "./workflow-liveness-panel";
import type { components } from "../../api/schema";

type Liveness = components["schemas"]["ControllersWorkerLivenessResponse"];

// The wf-1c2cb9bd shape, on the wire: a transition clock frozen twenty minutes
// ago and a signal clock from three seconds ago.
function liveness(overrides: Partial<Liveness> = {}): Liveness {
	const now = Date.now();
	return {
		observed: true,
		sessionId: "medusa-3",
		stepId: "step-work",
		stepKind: "work",
		state: "active",
		lastSignalAt: new Date(now - 3_000).toISOString(),
		lastTransitionAt: new Date(now - 20 * 60_000).toISOString(),
		silentForSeconds: 3,
		...overrides,
	};
}

describe("WorkflowLivenessPanel", () => {
	// PHASE J.1 / PHASE H: the headline defect. A worker twenty minutes into a
	// turn must read as heard-from-seconds-ago, and must NOT read as stuck.
	it("reports the signal clock, not the transition clock, as the activity figure", () => {
		render(<WorkflowLivenessPanel liveness={liveness()} />);
		expect(screen.getByTestId("worker-last-signal").textContent).toContain("3");
		expect(screen.getByTestId("worker-last-transition").textContent).toContain("20");
		expect(screen.getByTestId("worker-liveness-headline").textContent).toBe("Working");
	});

	// Both clocks stay on screen. The transition one is a real fact -- "working
	// on the same thing since 17:10" -- and was only ever a misreport when it
	// was LABELLED as liveness.
	it("keeps the transition clock beside the signal clock rather than replacing it", () => {
		render(<WorkflowLivenessPanel liveness={liveness()} />);
		expect(screen.getByText("Last signal")).toBeTruthy();
		expect(screen.getByText("Last state change")).toBeTruthy();
	});

	// The one case that may say the agent has gone quiet: a signal clock that is
	// itself old. Ten minutes is deliberately generous -- a worker running a
	// test suite is silent for minutes and is perfectly healthy.
	it("only says an agent is quiet when the SIGNAL clock is old", () => {
		render(<WorkflowLivenessPanel liveness={liveness({ silentForSeconds: 1_200 })} />);
		expect(screen.getByTestId("worker-liveness-headline").textContent).toBe("Gone quiet");
	});

	it("does not call an agent quiet on an old transition clock alone", () => {
		render(
			<WorkflowLivenessPanel
				liveness={liveness({
					silentForSeconds: 5,
					lastTransitionAt: new Date(Date.now() - 6 * 3_600_000).toISOString(),
				})}
			/>,
		);
		expect(screen.getByTestId("worker-liveness-headline").textContent).toBe("Working");
	});

	// A finished run has no live agent, and "heard from 0s ago" under one would
	// be the same class of misreport inverted.
	it("renders nothing when there is no running agent to ask", () => {
		const { container } = render(<WorkflowLivenessPanel liveness={{ observed: false, silentForSeconds: null }} />);
		expect(container.firstChild).toBeNull();
	});

	it("renders nothing when the daemon sent no liveness block at all", () => {
		const { container } = render(<WorkflowLivenessPanel liveness={undefined} />);
		expect(container.firstChild).toBeNull();
	});
});
