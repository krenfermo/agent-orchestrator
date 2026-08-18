import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CreateProjectFlow } from "./CreateProjectFlow";

const { chooseDirectoryMock, scanImportFolderMock } = vi.hoisted(() => ({
	chooseDirectoryMock: vi.fn(),
	scanImportFolderMock: vi.fn(),
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

describe("CreateProjectFlow without a native folder picker (browser preview / LAN web client)", () => {
	beforeEach(() => {
		delete window.ao;
		chooseDirectoryMock.mockReset();
		scanImportFolderMock.mockReset();
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

	it("advances past the Project card to a manual path field instead of no-oping", async () => {
		renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));

		expect(await screen.findByLabelText("Repository path")).toBeInTheDocument();
		expect(chooseDirectoryMock).not.toHaveBeenCalled();
	});

	it("advances past the Workspace card to a manual path field instead of no-oping", async () => {
		renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Workspace" }));

		expect(await screen.findByLabelText("Workspace root path")).toBeInTheDocument();
		expect(chooseDirectoryMock).not.toHaveBeenCalled();
	});

	it("accepts a typed path and hands it to the create-project flow", async () => {
		const { onCreateProject } = renderFlow();

		await userEvent.click(await screen.findByRole("button", { name: "Project" }));
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
