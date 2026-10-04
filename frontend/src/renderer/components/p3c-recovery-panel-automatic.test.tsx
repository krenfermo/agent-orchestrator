import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { WorkflowRecoveryPanel } from "./workflow-recovery-panel";
import type { components } from "../../api/schema";

const { useWorkflowRecoveryMock } = vi.hoisted(() => ({ useWorkflowRecoveryMock: vi.fn() }));

vi.mock("../hooks/useWorkflowRecovery", async (importOriginal) => {
	const actual = await importOriginal<typeof import("../hooks/useWorkflowRecovery")>();
	return { ...actual, useWorkflowRecovery: useWorkflowRecoveryMock };
});

vi.mock("@tanstack/react-router", () => ({
	Link: ({ children }: { children: React.ReactNode }) => <a href="#">{children}</a>,
}));

type RecoveryView = components["schemas"]["WorkflowRecoveryView"];
type RunView = components["schemas"]["WorkflowRunView"];

/**
 * P3-C §14: while AO is acting on a run by itself, the panel must not offer the
 * user a button that would start the same thing again.
 *
 * The interesting part is that P1-B's recovery assessment still says a repair is
 * "available" here — correctly, because it answers "is this condition
 * repairable". The panel must nonetheless offer nothing, because the Advisor's
 * flag answers the different question the buttons depend on: is AO already
 * doing it. These tests pin that the second question is the one that governs.
 */

function run(overrides: Partial<RunView> = {}): RunView {
	return {
		id: "wf-1",
		projectId: "proj-1",
		objective: "ship the thing",
		state: "needs_attention",
		createdAt: "2026-09-02T00:00:00Z",
		updatedAt: "2026-09-02T00:00:00Z",
		executionMode: "autonomous",
		phase: "needs_attention",
		lastActivityAt: "2026-09-02T00:00:00Z",
		canContinue: false,
		...overrides,
	} as RunView;
}

function recovery(overrides: Partial<RecoveryView> = {}): RecoveryView {
	return {
		recommendedAction: "repair",
		reasonCode: "verify_budget_exhausted",
		explanation: "Verification kept failing after every automatic fix attempt.",
		automaticAllowed: false,
		planReusable: "not_applicable",
		repairAvailable: true,
		repairEligibility: "eligible",
		obligation: "verify",
		strategy: "task",
		targetRunId: "wf-1",
		version: "v1",
		...overrides,
	} as RecoveryView;
}

function mockRecovery(view: RecoveryView) {
	useWorkflowRecoveryMock.mockReturnValue({
		recovery: view,
		repairPlan: { eligibility: "eligible", mode: "automatic", spent: 0, budget: 2, automaticAllowed: true },
		isLoading: false,
		error: undefined,
		run: vi.fn(),
		pending: false,
		pendingOperation: undefined,
		actionError: undefined,
	});
}

beforeEach(() => {
	useWorkflowRecoveryMock.mockReset();
});

describe("P3-C recovery panel while AO is acting", () => {
	it("offers no operation at all while an automatic action is in flight", () => {
		mockRecovery(recovery());
		render(<WorkflowRecoveryPanel run={run({ automaticActionActive: true })} />);

		// The explanation is still shown — a person watching a repair should be
		// able to read what it is repairing.
		expect(screen.getByText(/Verification kept failing/)).toBeInTheDocument();
		// But no button invites them to start a second remedy.
		expect(screen.queryByRole("button")).not.toBeInTheDocument();
		expect(screen.getByText(/AO is handling this by itself/i)).toBeInTheDocument();
	});

	it("offers the repair the moment AO is NOT acting, from the same assessment", () => {
		mockRecovery(recovery());
		render(<WorkflowRecoveryPanel run={run({ automaticActionActive: false })} />);

		expect(screen.getByRole("button", { name: /repair/i })).toBeInTheDocument();
		expect(screen.queryByText(/AO is handling this by itself/i)).not.toBeInTheDocument();
	});

	it("treats an absent flag as 'AO is not acting', so an older daemon behaves as before", () => {
		mockRecovery(recovery());
		render(<WorkflowRecoveryPanel run={run()} />);

		expect(screen.getByRole("button", { name: /repair/i })).toBeInTheDocument();
	});
});
