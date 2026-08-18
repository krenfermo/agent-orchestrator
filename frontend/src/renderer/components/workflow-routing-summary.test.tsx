import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowRoutingSummary, type RoutingDecisionView } from "./workflow-routing-summary";

function decision(overrides: Partial<RoutingDecisionView> = {}): RoutingDecisionView {
	return {
		role: "worker",
		waiting: false,
		fallbackUsed: false,
		...overrides,
	} as RoutingDecisionView;
}

describe("WorkflowRoutingSummary", () => {
	it("renders the preferred provider with a friendly reason label", () => {
		render(
			<WorkflowRoutingSummary
				routing={decision({
					preferredProfile: { id: "p1", provider: "anthropic", harness: "claude-code", displayName: "Claude" },
					selectedProfile: { id: "p1", provider: "anthropic", harness: "claude-code", displayName: "Claude" },
					reasonCodes: ["user_preferred_provider"],
				})}
			/>,
		);
		expect(screen.getByText("Worker")).toBeInTheDocument();
		expect(screen.getByText("Claude")).toBeInTheDocument();
		expect(screen.getByText("Preferred by your policy")).toBeInTheDocument();
	});

	it("shows a fallback line when the selected provider differs from the preferred one", () => {
		render(
			<WorkflowRoutingSummary
				routing={decision({
					preferredProfile: { id: "p1", provider: "anthropic", harness: "claude-code", displayName: "Claude" },
					selectedProfile: { id: "p2", provider: "openai", harness: "codex", displayName: "Codex" },
					fallbackUsed: true,
					reasonCodes: ["preferred_unavailable", "fallback_selected"],
				})}
			/>,
		);
		expect(screen.getByText("Codex")).toBeInTheDocument();
		expect(screen.getByText("Preferred provider unavailable")).toBeInTheDocument();
		expect(screen.getByText("Claude unavailable → fallback selected")).toBeInTheDocument();
	});

	it("shows a waiting message naming the preferred provider, never the internal reason code", () => {
		render(
			<WorkflowRoutingSummary
				routing={decision({
					role: "reviewer",
					waiting: true,
					preferredHarness: "claude-code",
					reasonCodes: ["review_independence_required", "waiting_for_capacity"],
				})}
			/>,
		);
		expect(screen.getByText("Reviewer")).toBeInTheDocument();
		expect(screen.getByText("Waiting for claude-code")).toBeInTheDocument();
		expect(screen.getByText("Independent reviewer required")).toBeInTheDocument();
		expect(screen.queryByText("waiting_for_capacity")).not.toBeInTheDocument();
		expect(screen.queryByText("review_independence_required")).not.toBeInTheDocument();
	});
});
