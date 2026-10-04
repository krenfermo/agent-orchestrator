import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { BrowseResult } from "../hooks/useProjectRegistration";
import { CreateProjectFlow } from "./CreateProjectFlow";

const { chooseDirectoryMock, scanImportFolderMock, browseMock } = vi.hoisted(() => ({
	chooseDirectoryMock: vi.fn(),
	scanImportFolderMock: vi.fn(),
	browseMock: vi.fn(),
}));

vi.mock("../lib/bridge", () => ({
	aoBridge: {
		app: {
			chooseDirectory: chooseDirectoryMock,
			scanImportFolder: scanImportFolderMock,
			checkAncestorRepo: vi.fn().mockResolvedValue(undefined),
		},
	},
}));

// The graphical server-side folder browser (Checkpoint 8P-E.4) is what the
// web (non-Electron) folder step now shows by default; stub the one network
// call it makes so these component tests never depend on a real daemon.
vi.mock("../hooks/useProjectRegistration", () => ({
	useProjectRegistration: () => ({
		browse: browseMock,
		register: vi.fn(),
		registering: false,
		registerError: undefined,
		resetRegisterError: vi.fn(),
		clone: vi.fn(),
		cloning: false,
		cloneError: undefined,
		resetCloneError: vi.fn(),
	}),
}));

// Stand in for the real agent-selection sheet: it needs a live agents query,
// which is unrelated to the fallback behavior under test here. Exposes the
// path it was opened with and a one-click submit so tests can drive the rest
// of the create-project flow (CreateProjectFlow.tsx:236) without it.
vi.mock("./CreateProjectAgentSheet", () => ({
	CreateProjectAgentSheet: ({
		onSubmit,
		open,
		path,
	}: {
		onSubmit: (selection: { orchestratorAgent: string; workerAgent: string }) => Promise<void>;
		open: boolean;
		path: string | null;
	}) =>
		open ? (
			<div role="dialog" aria-label="agent-sheet-stub">
				<span data-testid="agent-sheet-path">{path}</span>
				<button
					type="button"
					onClick={() => void onSubmit({ orchestratorAgent: "claude-code", workerAgent: "claude-code" })}
				>
					Confirm create
				</button>
			</div>
		) : null,
}));

function topLevel(entries: BrowseResult["entries"]): BrowseResult {
	return { path: "/srv/repos", entries };
}

describe("CreateProjectFlow without a native folder picker (browser preview / LAN web client)", () => {
	beforeEach(() => {
		delete window.ao;
		chooseDirectoryMock.mockReset();
		scanImportFolderMock.mockReset();
		browseMock.mockReset();
		browseMock.mockResolvedValue(topLevel([{ name: "medusa", path: "/srv/repos/medusa", isGitRepo: true }]));
	});

	afterEach(() => {
		vi.restoreAllMocks();
	});

	function renderFlow(overrides?: { onCreateProject?: () => Promise<void> }) {
		const onCreateProject = overrides?.onCreateProject ?? vi.fn().mockResolvedValue(undefined);
		const onInitializeProject = vi.fn().mockResolvedValue(undefined);
		render(
			<CreateProjectFlow embedded mode="choose" onCreateProject={onCreateProject} onInitializeProject={onInitializeProject}>
				{() => null}
			</CreateProjectFlow>,
		);
		return { onCreateProject, onInitializeProject };
	}

	// -- Project: graphical browser is the primary path --

	it("web Project → Choose folder shows the graphical server-side browser, not a text field", async () => {
		renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));

		expect(await screen.findByText("medusa")).toBeInTheDocument();
		expect(screen.queryByLabelText("Repository path")).not.toBeInTheDocument();
		expect(chooseDirectoryMock).not.toHaveBeenCalled();
		expect(browseMock).toHaveBeenCalledWith("");
	});

	it("web Project: selecting a folder in the browser hands its absolute path to the create-project flow", async () => {
		browseMock.mockImplementation(async (path: string) =>
			path === "" ? topLevel([{ name: "medusa", path: "/srv/repos/medusa", isGitRepo: true }]) : { path, entries: [] },
		);
		const { onCreateProject } = renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));
		await userEvent.click(await screen.findByText("medusa"));
		await userEvent.click(await screen.findByRole("button", { name: "Use this folder" }));

		expect(await screen.findByTestId("agent-sheet-path")).toHaveTextContent("/srv/repos/medusa");
		await userEvent.click(screen.getByRole("button", { name: "Confirm create" }));
		expect(onCreateProject).toHaveBeenCalledWith(expect.objectContaining({ path: "/srv/repos/medusa", asWorkspace: false }));
	});

	// -- Workspace: same graphical browser, asWorkspace: true --

	it("web Workspace → Choose folder shows the graphical server-side browser", async () => {
		renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Workspace" }));

		expect(await screen.findByText("medusa")).toBeInTheDocument();
		expect(screen.queryByLabelText("Workspace root path")).not.toBeInTheDocument();
		expect(chooseDirectoryMock).not.toHaveBeenCalled();
	});

	it("web Workspace: selecting a folder continues into the existing workspace registration flow", async () => {
		browseMock.mockImplementation(async (path: string) =>
			path === "" ? topLevel([{ name: "medusa", path: "/srv/repos/medusa", isGitRepo: true }]) : { path, entries: [] },
		);
		const { onCreateProject } = renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Workspace" }));
		await userEvent.click(await screen.findByText("medusa"));
		await userEvent.click(await screen.findByRole("button", { name: "Use this folder" }));
		await userEvent.click(await screen.findByRole("button", { name: "Confirm create" }));

		// The web path never calls the Electron-only client-side repo scan;
		// workspace repo discovery for a selected folder happens the same way
		// it always has for this fallback (server-side, inside POST
		// /api/v1/projects with asWorkspace: true) — asserting asWorkspace here
		// is what proves that continuation, not a new discovery mechanism.
		expect(scanImportFolderMock).not.toHaveBeenCalled();
		expect(onCreateProject).toHaveBeenCalledWith(expect.objectContaining({ path: "/srv/repos/medusa", asWorkspace: true }));
	});

	// -- Subdirectory navigation --

	it("navigates into a subdirectory and lists its own children", async () => {
		browseMock.mockImplementation(async (path: string) => {
			if (path === "") return topLevel([{ name: "medusa", path: "/srv/repos/medusa", isGitRepo: false }]);
			if (path === "/srv/repos/medusa") {
				return { path: "/srv/repos/medusa", entries: [{ name: "backend", path: "/srv/repos/medusa/backend", isGitRepo: true }] };
			}
			throw new Error(`unexpected browse path ${path}`);
		});
		renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));
		await userEvent.click(await screen.findByText("medusa"));

		expect(await screen.findByText("backend")).toBeInTheDocument();
		expect(screen.queryByText("medusa")).not.toBeInTheDocument();
		expect(browseMock).toHaveBeenCalledWith("/srv/repos/medusa");
	});

	// -- Advanced manual-path fallback still works, now behind an explicit toggle --

	it("keeps the manual path field available as an advanced fallback behind an explicit toggle", async () => {
		renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));
		expect(screen.queryByLabelText("Repository path")).not.toBeInTheDocument();

		await userEvent.click(await screen.findByRole("button", { name: "Enter a path manually instead" }));
		expect(await screen.findByLabelText("Repository path")).toBeInTheDocument();
	});

	it("accepts a typed path and hands it to the create-project flow", async () => {
		const { onCreateProject } = renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));
		await userEvent.click(await screen.findByRole("button", { name: "Enter a path manually instead" }));
		const input = await screen.findByLabelText("Repository path");
		fireEvent.change(input, { target: { value: "/Users/dev/code/my-repo" } });
		await userEvent.click(screen.getByRole("button", { name: "Use this path" }));

		expect(await screen.findByTestId("agent-sheet-path")).toHaveTextContent("/Users/dev/code/my-repo");
		expect(scanImportFolderMock).not.toHaveBeenCalled();

		await userEvent.click(screen.getByRole("button", { name: "Confirm create" }));
		expect(onCreateProject).toHaveBeenCalledWith(
			expect.objectContaining({ path: "/Users/dev/code/my-repo", asWorkspace: false }),
		);
	});

	it("disables submit until a path is typed", async () => {
		renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));
		await userEvent.click(await screen.findByRole("button", { name: "Enter a path manually instead" }));
		expect(await screen.findByRole("button", { name: "Use this path" })).toBeDisabled();
	});

	it("shows the server-side validation error instead of silently failing on an invalid path", async () => {
		const onCreateProject = vi.fn().mockRejectedValue(
			Object.assign(new Error("AO needs a Git repository with an initial commit before it can create agent workspaces."), {
				code: "NOT_A_GIT_REPO",
			}),
		);
		renderFlow({ onCreateProject });

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));
		await userEvent.click(await screen.findByRole("button", { name: "Enter a path manually instead" }));
		const input = await screen.findByLabelText("Repository path");
		fireEvent.change(input, { target: { value: "/tmp/not-a-repo" } });
		await userEvent.click(screen.getByRole("button", { name: "Use this path" }));
		await userEvent.click(await screen.findByRole("button", { name: "Confirm create" }));

		expect(
			await screen.findByText("AO needs a Git repository with an initial commit before it can create agent workspaces."),
		).toBeInTheDocument();
		expect(scanImportFolderMock).not.toHaveBeenCalled();
	});
});

describe("CreateProjectFlow with a native folder picker (Electron)", () => {
	beforeEach(() => {
		window.ao = { app: { chooseDirectory: chooseDirectoryMock } } as unknown as typeof window.ao;
		chooseDirectoryMock.mockReset();
		scanImportFolderMock.mockReset();
		browseMock.mockReset();
	});

	afterEach(() => {
		delete window.ao;
		vi.restoreAllMocks();
	});

	// Checkpoint 8P-E.4 must not touch the Electron native picker path: the
	// graphical server-side browser exists only for the "no window.ao" branch.
	it("still opens the native OS picker directly and never renders the server-side browser", async () => {
		chooseDirectoryMock.mockResolvedValue(null);
		render(
			<CreateProjectFlow
				embedded
				mode="choose"
				onCreateProject={vi.fn().mockResolvedValue(undefined)}
				onInitializeProject={vi.fn().mockResolvedValue(undefined)}
			>
				{() => null}
			</CreateProjectFlow>,
		);

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));

		expect(chooseDirectoryMock).toHaveBeenCalledTimes(1);
		expect(browseMock).not.toHaveBeenCalled();
		expect(screen.queryByRole("button", { name: "Use this folder" })).not.toBeInTheDocument();
	});
});
