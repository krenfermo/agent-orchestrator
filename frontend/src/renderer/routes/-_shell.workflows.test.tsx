import { render, screen } from "@testing-library/react";
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
}));

vi.mock("../hooks/useExecutionPolicy", () => ({
	useExecutionPolicy: useExecutionPolicyMock,
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

	it("lets the user deliberately choose Autonomous per run, independent of the global execution policy", async () => {
		const createRun = vi.fn().mockResolvedValue({});
		useWorkflowRunsMock.mockReturnValue({
			runs: [],
			isLoading: false,
			error: undefined,
			createRun,
			creating: false,
			createError: undefined,
		});
		// Global policy defaults to manual; the create form must still offer
		// an explicit Autonomous choice for this one run (Checkpoint 8P-D.1).
		useExecutionPolicyMock.mockReturnValue({ policy: { autonomousMode: false }, isLoading: false, error: undefined });
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		await userEvent.type(screen.getByLabelText(/objective/i), "Ship the thing");
		await userEvent.click(screen.getByRole("radio", { name: /autonomous workflow/i }));
		await userEvent.click(screen.getByRole("button", { name: /create/i }));

		expect(createRun).toHaveBeenCalledWith(expect.objectContaining({ autonomous: true }));
	});
});
