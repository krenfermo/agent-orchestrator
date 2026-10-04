import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: vi.fn(), POST: postMock },
	apiErrorMessage: () => "request failed",
	hasTrustedApiBaseUrl: () => true,
}));

// The child link is the only thing this component needs from the router, and a
// real router would drag the whole route tree into a unit test.
vi.mock("@tanstack/react-router", () => ({
	Link: ({ children, params }: { children: ReactNode; params: { workflowId: string } }) => (
		<a data-testid="child-link" href={`/workflows/${params.workflowId}`}>
			{children}
		</a>
	),
}));

import { WorkflowResumeButton } from "./workflow-resume-button";

function wrapper({ children }: { children: ReactNode }) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
	});
	return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
	postMock.mockReset();
});

describe("WorkflowResumeButton", () => {
	it("continues the run itself when the stop is its own", async () => {
		const continueRun = vi.fn().mockResolvedValue({});
		render(<WorkflowResumeButton continueRun={continueRun} continuing={false} />, { wrapper });

		await userEvent.click(screen.getByRole("button", { name: "Resume" }));
		expect(continueRun).toHaveBeenCalledTimes(1);
		expect(postMock).not.toHaveBeenCalled();
	});

	// A master run stopped on child_needs_attention is reporting somebody
	// else's problem. Continuing the master does nothing for the task that
	// actually stopped, so the POST must name the exact child.
	it("continues the exact child run when the stop mirrors one", async () => {
		postMock.mockResolvedValue({ data: { workflow: { id: "wf-child" } }, error: undefined });
		const continueRun = vi.fn().mockResolvedValue({});
		render(
			<WorkflowResumeButton attentionWorkflowId="wf-child" continueRun={continueRun} continuing={false} />,
			{ wrapper },
		);

		await userEvent.click(screen.getByRole("button", { name: "Resume" }));

		await waitFor(() => expect(postMock).toHaveBeenCalledTimes(1));
		expect(postMock).toHaveBeenCalledWith("/api/v1/workflows/{workflowId}/continue", {
			params: { path: { workflowId: "wf-child" } },
		});
		expect(continueRun).not.toHaveBeenCalled();
		expect(screen.getByTestId("child-link")).toHaveAttribute("href", "/workflows/wf-child");
	});

	it("is inert while a submit is already in flight, so duplicate clicks are safe", async () => {
		let release: ((value: unknown) => void) | undefined;
		postMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = resolve;
				}),
		);
		render(<WorkflowResumeButton attentionWorkflowId="wf-child" continueRun={vi.fn()} continuing={false} />, {
			wrapper,
		});

		const button = screen.getByRole("button");
		await userEvent.click(button);
		await waitFor(() => expect(button).toBeDisabled());
		await userEvent.click(button);
		await userEvent.click(button);

		expect(postMock).toHaveBeenCalledTimes(1);
		release?.({ data: { workflow: {} }, error: undefined });
	});

	it("shows the error and stays clickable when a continue fails", async () => {
		postMock.mockResolvedValue({ data: undefined, error: { message: "boom" } });
		render(<WorkflowResumeButton attentionWorkflowId="wf-child" continueRun={vi.fn()} continuing={false} />, {
			wrapper,
		});

		await userEvent.click(screen.getByRole("button"));
		expect(await screen.findByText("request failed")).toBeInTheDocument();
		expect(screen.getByRole("button")).toBeEnabled();
	});
});
