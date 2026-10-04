import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { I18nextProvider } from "react-i18next";
import { createAppI18n } from "../i18n/instance";

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock },
	apiErrorMessage: () => "request failed",
	hasTrustedApiBaseUrl: () => true,
}));

import { WorkflowCommitDialog } from "./workflow-commit-dialog";

/**
 * workflow-commit-dialog.test.tsx — P3-A §17.
 *
 * The properties under test are the ones that make this flow acceptable at all:
 * the user SEES the files before agreeing, the message is theirs, the commit is
 * the only write, and a commit that did not resume the run is reported as a
 * commit that did not resume the run.
 */

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

beforeEach(() => {
	getMock.mockReset();
	postMock.mockReset();
});

function pendingResponse(overrides: Record<string, unknown> = {}) {
	return {
		data: {
			available: true,
			repoPath: "/repo",
			branch: "feat/x",
			dirty: true,
			changes: [
				{ path: "src/a.ts", status: " M" },
				{ path: "src/new.ts", status: "??" },
			],
			proposedMessage: "wip: ship the thing",
			...overrides,
		},
		error: undefined,
	};
}

describe("commit and continue", () => {
	it("shows every pending file and the proposed message before anything is written", async () => {
		getMock.mockResolvedValue(pendingResponse());
		render(<WorkflowCommitDialog onOpenChange={vi.fn()} open workflowId="wf-1" />, { wrapper });

		await waitFor(() => expect(screen.getByTestId("workflow-commit-files")).toBeInTheDocument());
		const files = screen.getByTestId("workflow-commit-files");
		expect(files).toHaveTextContent("src/a.ts");
		expect(files).toHaveTextContent("src/new.ts");
		expect(screen.getByTestId("workflow-commit-message")).toHaveValue("wip: ship the thing");
		// Opening the dialog reads. It must not have written anything.
		expect(postMock).not.toHaveBeenCalled();
	});

	it("commits the user's own message, and only when they press the button", async () => {
		getMock.mockResolvedValue(pendingResponse());
		postMock.mockResolvedValue({ data: { committed: true, commitSha: "abc123", clean: true, resumed: true }, error: undefined });
		const onOpenChange = vi.fn();
		render(<WorkflowCommitDialog onOpenChange={onOpenChange} open workflowId="wf-1" />, { wrapper });

		await waitFor(() => expect(screen.getByTestId("workflow-commit-message")).toBeInTheDocument());
		const box = screen.getByTestId("workflow-commit-message");
		// Replacing the proposal outright, which is the case that matters: the
		// message AO proposed must stop being re-seeded the moment the user has
		// written their own, or every refetch would silently overwrite it.
		fireEvent.change(box, { target: { value: "chore: save my work" } });
		await waitFor(() => expect(box).toHaveValue("chore: save my work"));
		expect(postMock).not.toHaveBeenCalled();

		await userEvent.click(screen.getByTestId("workflow-commit-confirm"));
		await waitFor(() => expect(postMock).toHaveBeenCalledOnce());
		expect(postMock.mock.calls[0][1]).toMatchObject({ body: { message: "chore: save my work" } });
		// The daemon resumed the run, so the dialog's work is done.
		await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
	});

	// A commit AO could not follow with a resume is reported as exactly that.
	// Saying "done" over it would send the user away believing the run is
	// moving when the daemon deliberately left it stopped.
	it("does not claim the run resumed when the daemon says it did not", async () => {
		getMock.mockResolvedValue(pendingResponse());
		postMock.mockResolvedValue({
			data: {
				committed: true,
				commitSha: "abc123",
				clean: false,
				resumed: false,
				detail: "the repository still has pending changes",
			},
			error: undefined,
		});
		render(<WorkflowCommitDialog onOpenChange={vi.fn()} open workflowId="wf-1" />, { wrapper });
		await waitFor(() => expect(screen.getByTestId("workflow-commit-confirm")).toBeEnabled());
		await userEvent.click(screen.getByTestId("workflow-commit-confirm"));
		await waitFor(() => expect(screen.getByTestId("workflow-commit-not-resumed")).toBeInTheDocument());
		expect(screen.getByTestId("workflow-commit-not-resumed")).toHaveTextContent(
			"the repository still has pending changes",
		);
	});

	// An unreadable repository is UNKNOWN, never "clean". The dialog refuses to
	// offer a commit over an answer AO does not have.
	it("refuses to commit when AO could not read the repository", async () => {
		getMock.mockResolvedValue(
			pendingResponse({ available: false, dirty: false, changes: undefined, unavailable: "no repository probe is wired" }),
		);
		render(<WorkflowCommitDialog onOpenChange={vi.fn()} open workflowId="wf-1" />, { wrapper });
		await waitFor(() => expect(screen.getByTestId("workflow-commit-unavailable")).toBeInTheDocument());
		expect(screen.getByTestId("workflow-commit-confirm")).toBeDisabled();
	});
});
