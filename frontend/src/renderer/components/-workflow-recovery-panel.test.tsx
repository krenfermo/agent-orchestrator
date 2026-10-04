import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { WorkflowRecoveryPanel, offeredOperations } from "./workflow-recovery-panel";
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

function run(overrides: Partial<RunView> = {}): RunView {
	return {
		id: "wf-1",
		projectId: "proj-1",
		objective: "ship the thing",
		state: "needs_attention",
		createdAt: "2026-08-29T00:00:00Z",
		updatedAt: "2026-08-29T00:00:00Z",
		executionMode: "autonomous",
		phase: "needs_attention",
		lastActivityAt: "2026-08-29T00:00:00Z",
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
		blockingCondition: "the deterministic checks still fail",
		obligation: "verify",
		strategy: "task",
		targetRunId: "wf-1",
		version: "v1",
		...overrides,
	} as RecoveryView;
}

const runOperation = vi.fn();

function mockRecovery(view: RecoveryView | undefined, extra: Record<string, unknown> = {}) {
	useWorkflowRecoveryMock.mockReturnValue({
		recovery: view,
		repairPlan: { eligibility: "eligible", mode: "suggest", spent: 0, budget: 2, automaticAllowed: false },
		isLoading: false,
		error: undefined,
		run: runOperation,
		pending: false,
		pendingOperation: undefined,
		actionError: undefined,
		...extra,
	});
}

beforeEach(() => {
	runOperation.mockClear();
	useWorkflowRecoveryMock.mockReset();
});

describe("WorkflowRecoveryPanel", () => {
	it("renders what happened, the recommendation, automation and the blocking condition", () => {
		mockRecovery(recovery());
		render(<WorkflowRecoveryPanel run={run()} />);

		expect(screen.getByText(/Verification kept failing/)).toBeInTheDocument();
		expect(screen.getByText("Recommended action")).toBeInTheDocument();
		expect(screen.getByText("Automation")).toBeInTheDocument();
		expect(screen.getByText("Needs you")).toBeInTheDocument();
		expect(screen.getByText("the deterministic checks still fail")).toBeInTheDocument();
		expect(screen.getByText("Verification to run")).toBeInTheDocument();
		expect(screen.getByText(/Available \(0 of 2 used\)/)).toBeInTheDocument();
	});

	it("offers Repair when the backend says one is available, and calls the repair endpoint", async () => {
		mockRecovery(recovery());
		render(<WorkflowRecoveryPanel run={run()} />);

		await userEvent.click(screen.getByRole("button", { name: "Repair" }));
		expect(runOperation).toHaveBeenCalledWith("repair");
	});

	// The safety contract: an action the backend has refused is ABSENT, not
	// disabled and not "try anyway".
	it("never offers Repair when the backend says the condition is not repairable", () => {
		mockRecovery(
			recovery({
				recommendedAction: "inspect_repository",
				repairAvailable: false,
				repairEligibility: "ineligible",
				reasonCode: "verify_approved_head_unprovable",
			}),
		);
		render(<WorkflowRecoveryPanel run={run()} />);

		expect(screen.queryByRole("button", { name: "Repair" })).not.toBeInTheDocument();
		expect(screen.getByText("Not a repairable condition")).toBeInTheDocument();
	});

	it("offers Resume only when the backend recommends it", async () => {
		mockRecovery(recovery({ recommendedAction: "resume", repairAvailable: false, repairEligibility: "ineligible" }));
		render(<WorkflowRecoveryPanel run={run()} />);

		await userEvent.click(screen.getByRole("button", { name: "Resume" }));
		expect(runOperation).toHaveBeenCalledWith("resume");
	});

	it("offers Reuse plan for an exact plan and Regenerate plan for a stale one", async () => {
		mockRecovery(recovery({ recommendedAction: "reuse_plan", planReusable: "exact", repairAvailable: false }));
		const { unmount } = render(<WorkflowRecoveryPanel run={run()} />);
		await userEvent.click(screen.getByRole("button", { name: "Reuse plan" }));
		expect(runOperation).toHaveBeenCalledWith("reuse_plan");
		expect(screen.queryByRole("button", { name: "Regenerate plan" })).not.toBeInTheDocument();
		unmount();

		mockRecovery(
			recovery({ recommendedAction: "regenerate_plan", planReusable: "stale_but_revalidatable", repairAvailable: false }),
		);
		render(<WorkflowRecoveryPanel run={run()} />);
		await userEvent.click(screen.getByRole("button", { name: "Regenerate plan" }));
		expect(runOperation).toHaveBeenCalledWith("regenerate_plan");
		expect(screen.queryByRole("button", { name: "Reuse plan" })).not.toBeInTheDocument();
		expect(screen.getByText(/Stale/)).toBeInTheDocument();
	});

	it("disables every action while one is in flight", () => {
		mockRecovery(recovery({ recommendedAction: "resume" }), { pending: true, pendingOperation: "resume" });
		render(<WorkflowRecoveryPanel run={run()} />);

		for (const button of screen.getAllByRole("button")) {
			expect(button).toBeDisabled();
		}
		expect(screen.getByText("Working…")).toBeInTheDocument();
	});

	// A backend refusal must never read as success.
	it("surfaces a backend error instead of swallowing it", () => {
		mockRecovery(recovery(), { actionError: "this objective's plan is stale_but_revalidatable and cannot be reused" });
		render(<WorkflowRecoveryPanel run={run()} />);
		expect(screen.getByText(/cannot be reused/)).toBeInTheDocument();
	});

	it("says so when AO has no action it can take", () => {
		mockRecovery(
			recovery({
				recommendedAction: "unrecoverable",
				repairAvailable: false,
				repairEligibility: "unknown_condition",
				planReusable: "not_applicable",
			}),
		);
		render(<WorkflowRecoveryPanel run={run()} />);
		expect(screen.getByText(/no action it can take/)).toBeInTheDocument();
		expect(screen.queryByRole("button")).not.toBeInTheDocument();
	});

	it("sends the operator to the child that actually stopped", async () => {
		mockRecovery(recovery({ recommendedAction: "operator_action", repairAvailable: false, targetRunId: "wf-child" }));
		render(<WorkflowRecoveryPanel run={run()} />);
		await waitFor(() => expect(screen.getByText(/actually stopped/)).toBeInTheDocument());
	});

	it("renders nothing for a healthy running run", () => {
		mockRecovery(undefined);
		const { container } = render(<WorkflowRecoveryPanel run={run({ state: "running", phase: "running" })} />);
		expect(container).toBeEmptyDOMElement();
	});
});

// offeredOperations is the whole of the UI's authorization logic; it is a
// projection of backend fields and must never consult anything else.
describe("offeredOperations", () => {
	it("derives every offer from a backend-computed field", () => {
		expect(offeredOperations(recovery({ recommendedAction: "resume", repairAvailable: false }))).toEqual(["resume"]);
		expect(offeredOperations(recovery({ recommendedAction: "repair", repairAvailable: true }))).toEqual(["repair"]);
		expect(
			offeredOperations(recovery({ recommendedAction: "operator_action", repairAvailable: false, planReusable: "not_reusable" })),
		).toEqual([]);
		expect(
			offeredOperations(recovery({ recommendedAction: "authenticate", repairAvailable: false, planReusable: "not_applicable" })),
		).toEqual([]);
		// A terminal or unrecoverable run offers nothing at all.
		expect(offeredOperations(recovery({ recommendedAction: "terminal", repairAvailable: false }))).toEqual([]);
		expect(offeredOperations(recovery({ recommendedAction: "unrecoverable", repairAvailable: false }))).toEqual([]);
	});
});
