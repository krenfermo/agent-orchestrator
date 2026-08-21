import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowActivityPanel, WorkflowPhaseBadge, WorkflowStepChecklist } from "./workflow-activity";

describe("WorkflowPhaseBadge", () => {
	// The whole point of the animation: it marks the runs that are actually
	// moving, and only those.
	it("spins with an accessible label while a phase is genuinely running", () => {
		render(<WorkflowPhaseBadge phase="running" />);
		expect(screen.getByTestId("workflow-phase-badge")).toHaveTextContent("Running");
		const spinner = screen.getByRole("status", { name: "In progress" });
		expect(spinner.querySelector("svg")).toHaveClass("animate-spin");
	});

	it("spins for every executing phase and for no other", () => {
		for (const phase of ["planning", "running", "reviewing", "fixing", "verifying"] as const) {
			const view = render(<WorkflowPhaseBadge phase={phase} />);
			expect(view.queryByTestId("workflow-spinner"), phase).not.toBeNull();
			view.unmount();
		}
		for (const phase of [
			"queued",
			"waiting",
			"waiting_for_capacity",
			"retrying",
			"blocked",
			"needs_attention",
			"completed",
			"failed",
			"cancelled",
		] as const) {
			const view = render(<WorkflowPhaseBadge phase={phase} />);
			expect(view.queryByTestId("workflow-spinner"), phase).toBeNull();
			view.unmount();
		}
	});

	// Reduced motion must not cost the user the information: the icon stays and
	// the label still says "In progress", only the spin is dropped.
	it("drops the animation but not the meaning under reduced motion", () => {
		render(<WorkflowPhaseBadge phase="reviewing" />);
		const spinner = screen.getByRole("status", { name: "In progress" });
		expect(spinner.querySelector("svg")).toHaveClass("motion-reduce:animate-none");
	});

	// Color alone never carries the state — the phase is always spelled out.
	it("names the phase in text for every tone", () => {
		render(<WorkflowPhaseBadge phase="needs_attention" />);
		expect(screen.getByTestId("workflow-phase-badge")).toHaveTextContent("Needs attention");
	});
});

describe("WorkflowStepChecklist", () => {
	it("marks completed with a check, running with a spinner, and pending with neither", () => {
		render(
			<WorkflowStepChecklist
				steps={[
					{ kind: "plan", state: "completed" },
					{ kind: "work", state: "running" },
					{ kind: "review", state: "pending" },
					{ kind: "fix", state: "failed" },
				]}
			/>,
		);
		const items = screen.getAllByRole("listitem");
		// The state reaches assistive tech as text, not as an icon color.
		expect(items[0]).toHaveTextContent("done");
		expect(items[1]).toHaveTextContent("in progress");
		expect(items[2]).toHaveTextContent("not started");
		expect(items[3]).toHaveTextContent("failed");
		expect(items[0].querySelector(".animate-spin")).toBeNull();
		expect(items[1].querySelector(".animate-spin")).not.toBeNull();
		expect(items[2].querySelector(".animate-spin")).toBeNull();
		expect(items[3].querySelector(".animate-spin")).toBeNull();
	});
});

describe("WorkflowActivityPanel", () => {
	it("says what AO is doing right now, with only the facts it was given", () => {
		render(
			<WorkflowActivityPanel
				facts={[
					{ label: "Elapsed", value: "7m" },
					{ label: "Branch", value: undefined },
					{ label: "Tokens processed", value: "Unknown" },
				]}
				phase="reviewing"
			/>,
		);
		const panel = screen.getByTestId("workflow-activity-panel");
		expect(panel).toHaveTextContent("Working right now");
		expect(panel).toHaveTextContent("AO is reviewing the changes");
		expect(panel).toHaveTextContent("7m");
		// An unmeasured value stays Unknown and an absent one is simply not shown —
		// neither becomes a 0.
		expect(panel).toHaveTextContent("Unknown");
		expect(panel).not.toHaveTextContent("Branch");
		expect(panel).not.toHaveTextContent("0");
	});

	it("renders nothing at all for a phase that is not executing", () => {
		for (const phase of ["queued", "blocked", "needs_attention", "completed", "failed", "cancelled"] as const) {
			const view = render(<WorkflowActivityPanel phase={phase} />);
			expect(view.queryByTestId("workflow-activity-panel"), phase).toBeNull();
			view.unmount();
		}
	});
});
