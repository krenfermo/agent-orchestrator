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
	REVIEW_DEPTHS: ["none", "light", "deep"] as const,
	DEFAULT_REVIEW_DEPTH: { task: "none", autonomous: "light", master: "deep" } as const,
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

/**
 * Fills the first (always-present) verification command row. A Task cannot be
 * created without one, so every test that submits a Task goes through here.
 */
async function fillFirstVerificationCommand(command: string, args: string) {
	const verification = screen.getByTestId("task-verification");
	await userEvent.type(within(verification).getByLabelText("Command"), command);
	if (args !== "") await userEvent.type(within(verification).getByLabelText("Arguments"), args);
}

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
		// A Task must say how it will be checked before it can be created.
		await fillFirstVerificationCommand("go", "test ./...");
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

	// P5-A: review depth is a fifth independent axis. Its default TRACKS the
	// selected strategy (none for a bounded Task, light for Autonomous, deep
	// for Master) until the user expresses an opinion of their own, which is
	// the same shape the approval default already uses.
	it("defaults the review depth from the selected strategy and sends the one picked", async () => {
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

		const depth = screen.getByRole("group", { name: "Review depth" });
		const strategies = screen.getByRole("group", { name: "Execution strategy" });

		// Autonomous is the form's default strategy, so a bounded review is the
		// default depth.
		expect(within(depth).getByRole("radio", { name: /^Bounded review/ })).toBeChecked();

		// Switching to Master moves the default to a full review, without the
		// user having touched the depth control.
		await userEvent.click(within(strategies).getByRole("radio", { name: /^Master/ }));
		expect(within(depth).getByRole("radio", { name: /^Full review/ })).toBeChecked();

		// Switching to Task moves it to no reviewer.
		await userEvent.click(within(strategies).getByRole("radio", { name: /^Task/ }));
		expect(within(depth).getByRole("radio", { name: /^No reviewer/ })).toBeChecked();

		// And an explicit click wins from then on: changing the strategy again
		// must not silently overwrite a choice the user made.
		await userEvent.click(within(depth).getByRole("radio", { name: /^Full review/ }));
		await userEvent.click(within(strategies).getByRole("radio", { name: /^Autonomous/ }));
		expect(within(depth).getByRole("radio", { name: /^Full review/ })).toBeChecked();

		await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
		await userEvent.click(await screen.findByText("Project B"));
		await userEvent.type(screen.getByLabelText(/objective/i), "Ship the thing");
		await userEvent.click(screen.getByRole("button", { name: /create/i }));

		expect(createRun).toHaveBeenCalledWith(expect.objectContaining({ reviewDepth: "deep" }));
	});

	// A user who picks the cheapest option must be able to see, right there,
	// that AO will not honour it for a change that matters.
	it("states the risk clamp next to the review-depth control", async () => {
		useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
		render(<WorkflowsList />);

		const depth = screen.getByRole("group", { name: "Review depth" });
		expect(within(depth).getByText(/deeper of your choice and the change's own risk/i)).toBeInTheDocument();
		expect(within(depth).getByText(/payments/i)).toBeInTheDocument();
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

	// The defect: the form offered no way to say how a Task would be verified,
	// so it sent none, the daemon accepted the empty plan, and
	// wf-aee38f69-081c-48d1-a3a4-53430ca70682 did the whole job and then failed
	// at Verify with verify_ambiguous. Nothing could be fixed at that point --
	// the checks could only have been supplied here.
	describe("task verification", () => {
		const selectTask = async () =>
			userEvent.click(
				within(screen.getByRole("group", { name: "Execution strategy" })).getByRole("radio", { name: /^Task/ }),
			);

		const readyToCreate = async () => {
			await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
			await userEvent.click(await screen.findByText("Project B"));
			await userEvent.type(screen.getByLabelText(/objective/i), "Add Farewell()");
			await selectTask();
		};

		it("offers verification commands and acceptance criteria for a Task, and nothing for a planned strategy", async () => {
			useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
			render(<WorkflowsList />);

			// Autonomous is the default strategy, and its checks are its
			// planner's output -- there is nothing for the user to state here.
			expect(screen.queryByTestId("task-verification")).not.toBeInTheDocument();

			await selectTask();
			const verification = screen.getByTestId("task-verification");
			expect(within(verification).getByLabelText("Command")).toBeInTheDocument();
			expect(within(verification).getByLabelText("Arguments")).toBeInTheDocument();
			expect(within(verification).getByLabelText(/Acceptance criteria/)).toBeInTheDocument();

			await userEvent.click(
				within(screen.getByRole("group", { name: "Execution strategy" })).getByRole("radio", { name: /^Master/ }),
			);
			expect(screen.queryByTestId("task-verification")).not.toBeInTheDocument();
		});

		it("sends the commands and criteria exactly as typed", async () => {
			const createRun = vi.fn().mockResolvedValue({});
			useWorkflowRunsMock.mockReturnValue({
				runs: [], isLoading: false, error: undefined, createRun, creating: false, createError: undefined,
			});
			useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
			render(<WorkflowsList />);

			await readyToCreate();
			const verification = screen.getByTestId("task-verification");
			await userEvent.type(within(verification).getByLabelText("Command"), "go");
			await userEvent.type(within(verification).getByLabelText("Arguments"), "test ./...");
			await userEvent.type(within(verification).getByLabelText(/Working directory/), "backend");
			await userEvent.type(within(verification).getByLabelText(/Timeout/), "120");
			await userEvent.type(
				within(verification).getByLabelText(/Acceptance criteria/),
				'Farewell("") returns "Goodbye!"\nNo unrelated files are modified.',
			);

			// A second command, added by the user rather than invented for them.
			await userEvent.click(within(verification).getByRole("button", { name: "Add a command" }));
			const rows = within(screen.getByTestId("task-verification")).getAllByLabelText("Command");
			await userEvent.type(rows[1], "go");
			const argRows = within(screen.getByTestId("task-verification")).getAllByLabelText("Arguments");
			await userEvent.type(argRows[1], "build ./...");
			const retrySafe = within(screen.getByTestId("task-verification")).getAllByRole("checkbox");
			await userEvent.click(retrySafe[1]);

			await userEvent.click(screen.getByRole("button", { name: /create/i }));

			expect(createRun).toHaveBeenCalledWith(
				expect.objectContaining({
					strategy: "task",
					acceptanceCriteria: ['Farewell("") returns "Goodbye!"', "No unrelated files are modified."],
					verification: {
						commands: [
							{
								command: "go",
								args: ["test", "./..."],
								workingDirectory: "backend",
								timeoutSeconds: 120,
								requiredExitCode: 0,
								retrySafe: true,
							},
							{ command: "go", args: ["build", "./..."], requiredExitCode: 0, retrySafe: false },
						],
					},
				}),
			);
		});

		it("will not create a Task with no command, and says why", async () => {
			const createRun = vi.fn().mockResolvedValue({});
			useWorkflowRunsMock.mockReturnValue({
				runs: [], isLoading: false, error: undefined, createRun, creating: false, createError: undefined,
			});
			useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
			render(<WorkflowsList />);

			await readyToCreate();

			const create = screen.getByRole("button", { name: /create/i });
			expect(create).toBeDisabled();
			expect(screen.getByText(/at least one command AO can run/i)).toBeInTheDocument();

			await userEvent.click(create);
			expect(createRun).not.toHaveBeenCalled();

			// One command is enough, and the refusal goes away with it.
			await fillFirstVerificationCommand("go", "test ./...");
			expect(screen.queryByText(/at least one command AO can run/i)).not.toBeInTheDocument();
			expect(screen.getByRole("button", { name: /create/i })).toBeEnabled();
		});

		it("refuses a half-typed command rather than sending it", async () => {
			const createRun = vi.fn().mockResolvedValue({});
			useWorkflowRunsMock.mockReturnValue({
				runs: [], isLoading: false, error: undefined, createRun, creating: false, createError: undefined,
			});
			useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
			render(<WorkflowsList />);

			await readyToCreate();
			const verification = screen.getByTestId("task-verification");

			// Arguments with no program to run them.
			await userEvent.type(within(verification).getByLabelText("Arguments"), "test ./...");
			expect(screen.getByText("Name the program to run.")).toBeInTheDocument();
			expect(screen.getByRole("button", { name: /create/i })).toBeDisabled();

			await userEvent.type(within(verification).getByLabelText("Command"), "go");
			expect(screen.queryByText("Name the program to run.")).not.toBeInTheDocument();

			// A working directory that leaves the workspace.
			await userEvent.type(within(verification).getByLabelText(/Working directory/), "../../etc");
			expect(screen.getByText(/must stay inside the workspace/i)).toBeInTheDocument();
			expect(screen.getByRole("button", { name: /create/i })).toBeDisabled();
			await userEvent.clear(within(verification).getByLabelText(/Working directory/));

			// A timeout past the daemon's ceiling.
			await userEvent.type(within(verification).getByLabelText(/Timeout/), "7200");
			expect(screen.getByText(/between 0 and 3600/i)).toBeInTheDocument();
			expect(screen.getByRole("button", { name: /create/i })).toBeDisabled();
			await userEvent.clear(within(verification).getByLabelText(/Timeout/));

			// An exit code that is not a number.
			await userEvent.type(within(verification).getByLabelText(/Exit code/), "ok");
			expect(screen.getByText(/must be a whole number/i)).toBeInTheDocument();
			expect(screen.getByRole("button", { name: /create/i })).toBeDisabled();
			await userEvent.clear(within(verification).getByLabelText(/Exit code/));

			await userEvent.click(screen.getByRole("button", { name: /create/i }));
			expect(createRun).toHaveBeenCalledWith(
				expect.objectContaining({
					verification: { commands: [{ command: "go", args: ["test", "./..."], requiredExitCode: 0, retrySafe: true }] },
				}),
			);
		});

		it("states the commands and criteria in the summary above the button", async () => {
			useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
			render(<WorkflowsList />);

			// A planned run says where its checks come from rather than showing
			// an empty row that reads as "none".
			expect(screen.getByTestId("task-creation-summary")).toHaveTextContent("Derived by the planner");

			await selectTask();
			expect(screen.getByTestId("task-creation-summary")).toHaveTextContent("No command declared yet");
			expect(screen.getByTestId("task-creation-summary")).toHaveTextContent("AO's own criteria");

			await fillFirstVerificationCommand("go", "test ./...");
			await userEvent.type(
				within(screen.getByTestId("task-verification")).getByLabelText(/Acceptance criteria/),
				"Both cases are covered by a test.",
			);
			const summary = screen.getByTestId("task-creation-summary");
			expect(summary).toHaveTextContent("go test ./...");
			expect(summary).toHaveTextContent("Both cases are covered by a test.");
		});

		it("sends no verification and no criteria for a planned strategy", async () => {
			const createRun = vi.fn().mockResolvedValue({});
			useWorkflowRunsMock.mockReturnValue({
				runs: [], isLoading: false, error: undefined, createRun, creating: false, createError: undefined,
			});
			useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
			render(<WorkflowsList />);

			await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
			await userEvent.click(await screen.findByText("Project B"));
			await userEvent.type(screen.getByLabelText(/objective/i), "Build search");
			await userEvent.click(screen.getByRole("button", { name: /create/i }));

			const sent = createRun.mock.calls[0][0];
			expect(sent).not.toHaveProperty("verification");
			expect(sent).not.toHaveProperty("acceptanceCriteria");
		});
	});
});
