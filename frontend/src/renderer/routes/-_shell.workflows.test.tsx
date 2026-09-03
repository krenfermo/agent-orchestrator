import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkflowsList } from "./_shell.workflows";

const { useProjectsListMock, useWorkflowRunsMock, useExecutionPolicyMock, openGlobalSettingsMock } = vi.hoisted(() => ({
	useProjectsListMock: vi.fn(),
	useWorkflowRunsMock: vi.fn(),
	useExecutionPolicyMock: vi.fn(),
	openGlobalSettingsMock: vi.fn(),
}));

vi.mock("@tanstack/react-router", async (importOriginal) => {
	const actual = await importOriginal<typeof import("@tanstack/react-router")>();
	return {
		...actual,
		Link: ({ children, to }: { children: React.ReactNode; to: string }) => <a href={to}>{children}</a>,
	};
});

vi.mock("../hooks/useProjectsList", () => ({
	useProjectsList: useProjectsListMock,
}));

vi.mock("../hooks/useWorkflowRuns", () => ({
	useWorkflowRuns: useWorkflowRunsMock,
	// The strategy/approval vocabularies are plain data the form renders from;
	// mocking only the hook would leave them undefined at render time.
	EXECUTION_STRATEGIES: ["task", "autonomous", "master"] as const,
	APPROVAL_POLICIES: ["automatic", "manual"] as const,
	REPAIR_POLICIES: ["disabled", "suggest", "automatic"] as const,
	PLACEMENTS: ["direct_branch", "isolated_worktree", "auto"] as const,
}));

vi.mock("../hooks/useExecutionPolicy", () => ({
	useExecutionPolicy: useExecutionPolicyMock,
}));

// P3-A: the creation summary states the daemon's project-memory mode. It is a
// daemon fact, not a form input, so the form's tests stub the read rather than
// standing up a query client for one string.
vi.mock("../hooks/useSettings", () => ({
	useSettings: () => ({ settings: { defaultSessionMode: "tui", chatHarnesses: [], memoryMode: "off" } }),
}));

vi.mock("../stores/ui-store", () => ({
	useUiStore: (selector: (state: { openGlobalSettings: typeof openGlobalSettingsMock }) => unknown) =>
		selector({ openGlobalSettings: openGlobalSettingsMock }),
}));

const PROJECTS = [
	{ id: "proj-a", name: "Project A", path: "/repos/a", kind: "single_repo" as const, sessionPrefix: "a", valid: true, repo: "https://github.com/acme/a" },
	{ id: "proj-b", name: "Project B", path: "/repos/b", kind: "single_repo" as const, sessionPrefix: "b", valid: true },
];

beforeEach(() => {
	vi.clearAllMocks();
	useWorkflowRunsMock.mockReturnValue({
		runs: [],
		isLoading: false,
		error: undefined,
		createRun: vi.fn().mockResolvedValue({}),
		creating: false,
		createError: undefined,
	});
	useExecutionPolicyMock.mockReturnValue({
		policy: { autonomousMode: false },
		isLoading: false,
		error: undefined,
	});
});

describe("WorkflowsList", () => {
	it("renders a project select, never a free-text projectId input", () => {
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		expect(screen.getByRole("combobox", { name: "Project" })).toBeInTheDocument();
		expect(screen.queryByPlaceholderText(/project id/i)).not.toBeInTheDocument();
		expect(screen.queryByRole("textbox", { name: /project/i })).not.toBeInTheDocument();
	});

	it("shows a CTA to Settings → Projects when no projects are registered", async () => {
		useProjectsListMock.mockReturnValue({ projects: [], isLoading: false, error: undefined });
		render(<WorkflowsList />);

		expect(screen.getByText(/No projects registered/i)).toBeInTheDocument();
		const cta = screen.getByRole("button", { name: /Go to Settings/i });
		await userEvent.click(cta);
		expect(openGlobalSettingsMock).toHaveBeenCalledWith("projects");

		// No project select or objective form is offered without a project to pick.
		expect(screen.queryByRole("combobox", { name: "Project" })).not.toBeInTheDocument();
	});

	it("submits the selected project's real id, not free text", async () => {
		const createRun = vi.fn().mockResolvedValue({});
		useWorkflowRunsMock.mockReturnValue({
			runs: [],
			isLoading: false,
			error: undefined,
			createRun,
			creating: false,
			createError: undefined,
		});
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		await userEvent.type(screen.getByLabelText(/objective/i), "Ship the thing");
		await userEvent.click(screen.getByRole("button", { name: /create/i }));

		expect(createRun).toHaveBeenCalledWith(expect.objectContaining({ projectId: "proj-b" }));
	});

	it("offers the three execution strategies as a first-class choice and sends the one picked", async () => {
		const createRun = vi.fn().mockResolvedValue({});
		useWorkflowRunsMock.mockReturnValue({
			runs: [],
			isLoading: false,
			error: undefined,
			createRun,
			creating: false,
			createError: undefined,
		});
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		// All three are offered. The strategy is the run's orchestration
		// choice, not something inferred from an approval toggle.
		expect(within(screen.getByRole("group", { name: "Execution strategy" })).getByRole("radio", { name: /^Task/ })).toBeInTheDocument();
		expect(within(screen.getByRole("group", { name: "Execution strategy" })).getByRole("radio", { name: /^Autonomous/ })).toBeInTheDocument();
		expect(within(screen.getByRole("group", { name: "Execution strategy" })).getByRole("radio", { name: /^Master/ })).toBeInTheDocument();
		// Autonomous is the default for normal project work.
		expect(within(screen.getByRole("group", { name: "Execution strategy" })).getByRole("radio", { name: /^Autonomous/ })).toBeChecked();

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		await userEvent.type(screen.getByLabelText(/objective/i), "Rename the flag");
		await userEvent.click(within(screen.getByRole("group", { name: "Execution strategy" })).getByRole("radio", { name: /^Task/ }));
		await userEvent.click(screen.getByRole("button", { name: /create/i }));

		expect(createRun).toHaveBeenCalledWith(expect.objectContaining({ strategy: "task" }));
	});

	it("keeps approval independent of execution strategy", async () => {
		const createRun = vi.fn().mockResolvedValue({});
		useWorkflowRunsMock.mockReturnValue({
			runs: [],
			isLoading: false,
			error: undefined,
			createRun,
			creating: false,
			createError: undefined,
		});
		// Global policy defaults to manual approval; the create form must still
		// offer an explicit per-run choice (Checkpoint 8P-D.1), and choosing it
		// must not disturb the strategy.
		useExecutionPolicyMock.mockReturnValue({ policy: { autonomousMode: false }, isLoading: false, error: undefined });
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		await userEvent.type(screen.getByLabelText(/objective/i), "Ship the thing");
		await userEvent.click(within(screen.getByRole("group", { name: "Execution strategy" })).getByRole("radio", { name: /^Master/ }));
		await userEvent.click(within(screen.getByRole("group", { name: "Approval" })).getByRole("radio", { name: /^Automatic/ }));
		await userEvent.click(screen.getByRole("button", { name: /create/i }));

		expect(createRun).toHaveBeenCalledWith(
			expect.objectContaining({ strategy: "master", approvalPolicy: "automatic" }),
		);
	});

	it("offers the repair policy and defaults it to suggest", async () => {
		const createRun = vi.fn().mockResolvedValue({});
		useWorkflowRunsMock.mockReturnValue({
			runs: [],
			isLoading: false,
			error: undefined,
			createRun,
			creating: false,
			createError: undefined,
		});
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		// Suggest is the default: a repair writes code, and opting into that
		// unattended must be a decision somebody made.
		expect(within(screen.getByRole("group", { name: "Automatic repair" })).getByRole("radio", { name: /^Suggest/ })).toBeChecked();

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		await userEvent.type(screen.getByLabelText(/objective/i), "Ship the thing");
		await userEvent.click(within(screen.getByRole("group", { name: "Automatic repair" })).getByRole("radio", { name: /^Automatic/ }));
		await userEvent.click(screen.getByRole("button", { name: /create/i }));

		expect(createRun).toHaveBeenCalledWith(expect.objectContaining({ repairPolicy: "automatic" }));
	});

	// P3-A §7/§12: where the work happens is a first-class choice made before
	// the run exists, not something discovered afterwards from a worktree that
	// appeared. "Auto" is the default because it is what AO did before this
	// choice existed -- defaulting to an explicit placement would silently move
	// every existing user's work.
	it("offers the placement as an explicit choice, defaults it to auto, and sends the one picked", async () => {
		const createRun = vi.fn().mockResolvedValue({});
		useWorkflowRunsMock.mockReturnValue({
			runs: [],
			isLoading: false,
			error: undefined,
			createRun,
			creating: false,
			createError: undefined,
		});
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		const placement = screen.getByRole("group", { name: "Where the work happens" });
		expect(within(placement).getByRole("radio", { name: /^Automatic/ })).toBeChecked();
		// The branch option says what it means, including the consequence that
		// makes it different: there is nothing to integrate afterwards.
		expect(within(placement).getByRole("radio", { name: /^Current branch/ })).toBeInTheDocument();

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		await userEvent.type(screen.getByLabelText(/objective/i), "Ship the thing");
		await userEvent.click(within(placement).getByRole("radio", { name: /^Current branch/ }));
		await userEvent.click(screen.getByRole("button", { name: /create/i }));

		expect(createRun).toHaveBeenCalledWith(expect.objectContaining({ placement: "direct_branch" }));
	});

	// §12: the choices that change execution semantics are restated where the
	// user is about to act on them, not hidden behind an "advanced" disclosure.
	it("summarises every semantic choice above the button that starts the run", async () => {
		useWorkflowRunsMock.mockReturnValue({
			runs: [],
			isLoading: false,
			error: undefined,
			createRun: vi.fn(),
			creating: false,
			createError: undefined,
		});
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		const summary = screen.getByTestId("task-creation-summary");
		expect(summary).toHaveTextContent("Strategy");
		expect(summary).toHaveTextContent("Approval");
		expect(summary).toHaveTextContent("Automatic repair");
		expect(summary).toHaveTextContent("Where the work happens");
		expect(summary).toHaveTextContent("Project memory");
		// No project chosen yet: it says so rather than showing an empty row
		// that reads as "none".
		expect(summary).toHaveTextContent("Select a project to see where the work will happen.");

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		expect(screen.getByTestId("task-creation-summary")).toHaveTextContent("Project B");
	});

	it("defaults approval to the caller's stored execution policy", async () => {
		const createRun = vi.fn().mockResolvedValue({});
		useWorkflowRunsMock.mockReturnValue({
			runs: [],
			isLoading: false,
			error: undefined,
			createRun,
			creating: false,
			createError: undefined,
		});
		useExecutionPolicyMock.mockReturnValue({ policy: { autonomousMode: true }, isLoading: false, error: undefined });
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		expect(within(screen.getByRole("group", { name: "Approval" })).getByRole("radio", { name: /^Automatic/ })).toBeChecked();

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		await userEvent.type(screen.getByLabelText(/objective/i), "Ship the thing");
		await userEvent.click(screen.getByRole("button", { name: /create/i }));

		expect(createRun).toHaveBeenCalledWith(expect.objectContaining({ approvalPolicy: "automatic" }));
	});
});
