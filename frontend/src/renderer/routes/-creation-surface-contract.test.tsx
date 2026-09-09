import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nextProvider } from "react-i18next";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

/**
 * The contract test for AO's two creation surfaces.
 *
 * The defect this exists for was not a broken button: both surfaces worked.
 * It was that a surface whose field was labelled **Task** created a *session*,
 * while a Task *workflow run* was created somewhere else entirely — so people
 * started freeform workers believing they had started a Task run with checks.
 *
 * Asserting navigation would not have caught that, and would not catch it
 * coming back. So every test below asserts the **endpoint actually reached**:
 *
 *   "New workflow run"  ->  POST /api/v1/projects/{projectId}/workflows
 *   "New session"       ->  POST /api/v1/orchestrators/delegate
 *
 * The mock is placed at `apiClient`, the lowest layer either path crosses, so
 * the real hooks, the real form state and the real validation all run.
 */

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock },
	apiErrorMessage: (error: unknown) =>
		(error as { message?: string })?.message ?? "request failed",
	apiErrorCode: (error: unknown) => (error as { code?: string })?.code,
	hasTrustedApiBaseUrl: () => true,
}));

vi.mock("@tanstack/react-router", async (importOriginal) => {
	const actual = await importOriginal<typeof import("@tanstack/react-router")>();
	return {
		...actual,
		Link: ({ children, to }: { children: ReactNode; to: string }) => <a href={to}>{children}</a>,
		useNavigate: () => vi.fn(),
	};
});

vi.mock("../stores/ui-store", () => ({
	useUiStore: (selector: (state: { openGlobalSettings: () => void }) => unknown) =>
		selector({ openGlobalSettings: vi.fn() }),
}));

// Telemetry is fire-and-forget on the delegate path; it must not reach the network.
vi.mock("../lib/telemetry", () => ({ captureRendererEvent: vi.fn() }));

import { createAppI18n } from "../i18n/instance";
import { TaskComposer } from "../components/TaskComposer";
import { WorkflowsList } from "./_shell.workflows";

const PROJECT = {
	id: "proj-a",
	name: "Project A",
	path: "/repos/a",
	kind: "single_repo" as const,
	sessionPrefix: "a",
	valid: true,
};

/** Answers every GET either surface makes, by path. */
function stubReads() {
	getMock.mockImplementation((path: string) => {
		switch (path) {
			case "/api/v1/projects":
				return Promise.resolve({ data: { projects: [PROJECT] }, error: undefined });
			case "/api/v1/projects/{id}":
				return Promise.resolve({
					data: { status: "ok", project: { ...PROJECT, agent: "claude-code", config: {} } },
					error: undefined,
				});
			case "/api/v1/execution-policy":
				return Promise.resolve({ data: { policy: { autonomousMode: false } }, error: undefined });
			case "/api/v1/settings":
				return Promise.resolve({
					data: { defaultSessionMode: "tui", chatHarnesses: [], memoryMode: "off" },
					error: undefined,
				});
			case "/api/v1/agents":
				return Promise.resolve({
					data: {
						supported: [{ id: "claude-code", label: "Claude Code" }],
						installed: ["claude-code"],
						authorized: ["claude-code"],
					},
					error: undefined,
				});
			default:
				return Promise.resolve({ data: {}, error: undefined });
		}
	});
}

function wrapper({ children }: { children: ReactNode }) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
	});
	return (
		<QueryClientProvider client={queryClient}>
			<I18nextProvider i18n={createAppI18n("en")}>{children}</I18nextProvider>
		</QueryClientProvider>
	);
}

/** The paths POSTed, in order, so a test can name the endpoint it expects. */
function postedPaths(): string[] {
	return postMock.mock.calls.map((call) => call[0] as string);
}

/**
 * The POSTs that CREATE something, which is the only axis these tests are
 * about. The composer also refreshes the agent inventory on open; that is
 * housekeeping, and letting it into the assertion would make the contract read
 * as incidental ordering.
 */
const CREATION_ENDPOINTS = ["/api/v1/projects/{projectId}/workflows", "/api/v1/orchestrators/delegate"];

function creationPosts(): { path: string; options: { params?: { path?: Record<string, string> }; body: Record<string, unknown> } }[] {
	return postMock.mock.calls
		.filter((call) => CREATION_ENDPOINTS.includes(call[0] as string))
		.map((call) => ({ path: call[0] as string, options: call[1] }));
}

beforeEach(() => {
	vi.clearAllMocks();
	stubReads();
	postMock.mockResolvedValue({ data: { workflow: { id: "wf-new" }, workerId: "ses-new" }, error: undefined });
});

describe("creation surfaces reach the endpoint their name promises", () => {
	it("the workflow form POSTs a workflow run to the project, never a delegate", async () => {
		render(<WorkflowsList initialProjectId="proj-a" />, { wrapper });

		await screen.findByDisplayValue("");
		await userEvent.type(
			screen.getByLabelText(/objective/i),
			"Fix the flaky checkout test",
		);
		// A Task run carries no planner, so the form requires an executable check
		// before it will submit. Supplying one is part of the contract.
		await userEvent.click(screen.getByRole("radio", { name: /^Task/ }));
		await userEvent.type(screen.getAllByLabelText(/command/i)[0], "npm");

		await userEvent.click(screen.getByRole("button", { name: /create workflow run/i }));

		await waitFor(() => expect(creationPosts()).toHaveLength(1));
		const [created] = creationPosts();
		expect(created.path).toBe("/api/v1/projects/{projectId}/workflows");
		expect(postedPaths()).not.toContain("/api/v1/orchestrators/delegate");

		const options = created.options;
		expect(options.params?.path?.projectId).toBe("proj-a");
		expect(options.body.strategy).toBe("task");
		expect(options.body.objective).toBe("Fix the flaky checkout test");
		// The verify plan travels with the create call, in the same transaction
		// that makes the run -- a task cannot durably exist without its checks.
		expect((options.body.verification as { commands: { command: string }[] }).commands[0].command).toBe("npm");
	});

	it("the session composer POSTs a delegate, never a workflow run", async () => {
		render(<TaskComposer projectId="proj-a" onCreated={vi.fn()} />, { wrapper });

		await userEvent.type(screen.getByLabelText(/instructions/i), "Poke at the checkout flow");
		await userEvent.click(screen.getByRole("button", { name: /start worker/i }));

		await waitFor(() => expect(creationPosts()).toHaveLength(1));
		const [created] = creationPosts();
		expect(created.path).toBe("/api/v1/orchestrators/delegate");
		expect(postedPaths()).not.toContain("/api/v1/projects/{projectId}/workflows");

		const options = created.options;
		expect(options.body.projectId).toBe("proj-a");
		// No strategy, no verification: this object has neither, and inventing
		// them here is exactly the conflation the two surfaces must not have.
		expect(options.body.strategy).toBeUndefined();
		expect(options.body.verification).toBeUndefined();
	});

	it("names the two surfaces differently, and says which object each one makes", () => {
		render(<TaskComposer projectId="proj-a" onCreated={vi.fn()} />, { wrapper });
		// The field that used to be labelled "Task" inside a dialog that makes a
		// session. If this ever reads "Task" again, the original confusion is back.
		expect(screen.getByLabelText(/instructions/i)).toBeInTheDocument();
		expect(screen.queryByLabelText(/^task$/i)).not.toBeInTheDocument();
	});
});

describe("the workflow form refuses a run it knows would be doomed", () => {
	it("will not create a Task with no executable check", async () => {
		render(<WorkflowsList initialProjectId="proj-a" />, { wrapper });

		await userEvent.type(screen.getByLabelText(/objective/i), "Fix the flaky checkout test");
		await userEvent.click(screen.getByRole("radio", { name: /^Task/ }));
		await userEvent.click(screen.getByRole("button", { name: /create workflow run/i }));

		// Nothing is POSTed at all: the refusal happens before a run exists,
		// rather than after a worker has already done the job.
		expect(postMock).not.toHaveBeenCalled();
	});

	it("surfaces a daemon refusal instead of reporting success", async () => {
		postMock.mockResolvedValue({ data: undefined, error: { message: "verification is required" } });
		render(<WorkflowsList initialProjectId="proj-a" />, { wrapper });

		await userEvent.type(screen.getByLabelText(/objective/i), "Fix the flaky checkout test");
		await userEvent.click(screen.getByRole("radio", { name: /^Task/ }));
		await userEvent.type(screen.getAllByLabelText(/command/i)[0], "npm");
		await userEvent.click(screen.getByRole("button", { name: /create workflow run/i }));

		expect(await screen.findByText(/verification is required/i)).toBeInTheDocument();
	});
});

describe("a refusal from the daemon is reported, never swallowed", () => {
	// 401/403 are the two the user is most likely to meet on a fresh install or
	// a revoked session, and the failure mode that matters is the silent one:
	// the form appearing to succeed, or clearing itself, when nothing was made.
	it.each([
		["unauthenticated", { status: 401, code: "UNAUTHORIZED", message: "Sign in to continue" }],
		["forbidden", { status: 403, code: "FORBIDDEN", message: "You cannot create runs in this project" }],
	])("shows the daemon's %s refusal and keeps the objective for correction", async (_label, apiError) => {
		postMock.mockImplementation((path: string) =>
			path === "/api/v1/projects/{projectId}/workflows"
				? Promise.resolve({ data: undefined, error: apiError })
				: Promise.resolve({ data: {}, error: undefined }),
		);
		render(<WorkflowsList initialProjectId="proj-a" />, { wrapper });

		await userEvent.type(screen.getByLabelText(/objective/i), "Fix the flaky checkout test");
		await userEvent.click(screen.getByRole("radio", { name: /^Task/ }));
		await userEvent.type(screen.getAllByLabelText(/command/i)[0], "npm");
		await userEvent.click(screen.getByRole("button", { name: /create workflow run/i }));

		expect(await screen.findByText(apiError.message)).toBeInTheDocument();
		// The work the user typed survives a refusal they have to act on.
		expect(screen.getByLabelText(/objective/i)).toHaveValue("Fix the flaky checkout test");
	});

	it("reports a refused delegate on the session surface too", async () => {
		postMock.mockImplementation((path: string) =>
			path === "/api/v1/orchestrators/delegate"
				? Promise.resolve({ data: undefined, error: { status: 403, message: "Agent is not authorized" } })
				: Promise.resolve({ data: {}, error: undefined }),
		);
		const onCreated = vi.fn();
		render(<TaskComposer projectId="proj-a" onCreated={onCreated} />, { wrapper });

		await userEvent.type(screen.getByLabelText(/instructions/i), "Poke at the checkout flow");
		await userEvent.click(screen.getByRole("button", { name: /start worker/i }));

		expect(await screen.findByText("Agent is not authorized")).toBeInTheDocument();
		// Never report a session that does not exist.
		expect(onCreated).not.toHaveBeenCalled();
	});
});

describe("the run's frozen axes travel with the create call", () => {
	// Placement, strategy and review depth are frozen at creation by the daemon.
	// Sending them afterwards would race the autonomous kickoff -- which is how
	// an explicit "current branch" once became a worktree nobody asked for -- so
	// the contract is that they are in the create body itself.
	it("sends strategy, placement, approval, repair and review depth on create", async () => {
		render(<WorkflowsList initialProjectId="proj-a" />, { wrapper });

		await userEvent.type(screen.getByLabelText(/objective/i), "Ship it");
		await userEvent.click(screen.getByRole("radio", { name: /^Task/ }));
		await userEvent.type(screen.getAllByLabelText(/command/i)[0], "npm");
		await userEvent.click(screen.getByRole("button", { name: /create workflow run/i }));

		await waitFor(() => expect(creationPosts()).toHaveLength(1));
		const body = creationPosts()[0].options.body;
		expect(body.strategy).toBe("task");
		expect(body.placement).toBe("auto");
		expect(body.approvalPolicy).toBe("manual");
		expect(body.repairPolicy).toBe("suggest");
		// A bounded Task asks for no review by default; the daemon still clamps
		// upward for a risky change, which is not expressible here.
		expect(body.reviewDepth).toBe("none");
	});
});

describe("the project preselect survives the desktop app's hash history", () => {
	// The Board hands the form a project through `?projectId=`. The form used to
	// read it off window.location.search, which is EMPTY under hash history --
	// the history the Electron app uses -- so the preselect silently did nothing
	// in the build almost everybody runs. Reading it as a prop from the router's
	// validated search is what makes it work in both.
	it("preselects the project it was handed, with no window.location involved", async () => {
		render(<WorkflowsList initialProjectId="proj-a" />, { wrapper });
		expect(window.location.search).toBe("");
		// The Select trigger specifically: the name also appears in the hidden
		// native select Radix renders for form compatibility.
		// The trigger exists from first paint and fills in once the project list
		// resolves, so this waits for the content rather than for the element.
		await waitFor(() =>
			expect(screen.getByTestId("workflow-project-trigger")).toHaveTextContent("Project A"),
		);
	});

	it("creates against the preselected project without the user re-picking it", async () => {
		render(<WorkflowsList initialProjectId="proj-a" />, { wrapper });

		await userEvent.type(screen.getByLabelText(/objective/i), "Ship it");
		await userEvent.click(screen.getByRole("radio", { name: /^Task/ }));
		await userEvent.type(screen.getAllByLabelText(/command/i)[0], "npm");
		await userEvent.click(screen.getByRole("button", { name: /create workflow run/i }));

		await waitFor(() => expect(creationPosts()).toHaveLength(1));
		expect(creationPosts()[0].options.params?.path?.projectId).toBe("proj-a");
	});
});
