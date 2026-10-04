import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock },
	apiErrorMessage: () => "request failed",
	hasTrustedApiBaseUrl: () => true,
}));

import { useContinueWorkflowRun, useStartWorkflowRun, useWorkflowRun, workflowRunIsTerminal } from "./useWorkflowRun";

function wrapper({ children }: { children: ReactNode }) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
	});
	return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
	getMock.mockReset();
	postMock.mockReset();
});

describe("workflowRunIsTerminal", () => {
	it("treats completed/failed/cancelled as terminal and everything else as active", () => {
		expect(workflowRunIsTerminal("completed")).toBe(true);
		expect(workflowRunIsTerminal("failed")).toBe(true);
		expect(workflowRunIsTerminal("cancelled")).toBe(true);
		expect(workflowRunIsTerminal("pending")).toBe(false);
		expect(workflowRunIsTerminal("running")).toBe(false);
		expect(workflowRunIsTerminal(undefined)).toBe(false);
	});
});

describe("useWorkflowRun", () => {
	it("loads a workflow run's detail", async () => {
		getMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-1", projectId: "proj-1", objective: "ship it", state: "pending" },
					steps: [],
				},
			},
			error: undefined,
		});

		const { result } = renderHook(() => useWorkflowRun("wf-1"), { wrapper });

		await waitFor(() => expect(result.current.workflow?.run.id).toBe("wf-1"));
		expect(result.current.workflow?.run.state).toBe("pending");
	});

	it("cancels a run and invalidates its query", async () => {
		getMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-1", projectId: "proj-1", objective: "ship it", state: "running" },
					steps: [],
				},
			},
			error: undefined,
		});
		postMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-1", projectId: "proj-1", objective: "ship it", state: "cancelled" },
					steps: [],
				},
			},
			error: undefined,
		});

		const { result } = renderHook(() => useWorkflowRun("wf-1"), { wrapper });
		await waitFor(() => expect(result.current.workflow?.run.id).toBe("wf-1"));

		await result.current.cancel();
		expect(postMock).toHaveBeenCalledWith(
			"/api/v1/workflows/{workflowId}/cancel",
			expect.objectContaining({ params: { path: { workflowId: "wf-1" } } }),
		);
	});

	it("starts a run and invalidates its query", async () => {
		getMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-1", projectId: "proj-1", objective: "ship it", state: "pending" },
					steps: [],
				},
			},
			error: undefined,
		});
		postMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-1", projectId: "proj-1", objective: "ship it", state: "running" },
					steps: [],
				},
			},
			error: undefined,
		});

		const { result } = renderHook(() => useWorkflowRun("wf-1"), { wrapper });
		await waitFor(() => expect(result.current.workflow?.run.id).toBe("wf-1"));

		await result.current.start();
		expect(postMock).toHaveBeenCalledWith(
			"/api/v1/workflows/{workflowId}/start",
			expect.objectContaining({ params: { path: { workflowId: "wf-1" } } }),
		);
	});

	it("continues a run and invalidates its query", async () => {
		getMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-1", projectId: "proj-1", objective: "ship it", state: "waiting" },
					steps: [],
				},
			},
			error: undefined,
		});
		postMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-1", projectId: "proj-1", objective: "ship it", state: "waiting" },
					steps: [],
				},
			},
			error: undefined,
		});

		const { result } = renderHook(() => useWorkflowRun("wf-1"), { wrapper });
		await waitFor(() => expect(result.current.workflow?.run.id).toBe("wf-1"));

		await result.current.continueRun();
		expect(postMock).toHaveBeenCalledWith(
			"/api/v1/workflows/{workflowId}/continue",
			expect.objectContaining({ params: { path: { workflowId: "wf-1" } } }),
		);
	});
});

describe("useStartWorkflowRun", () => {
	it("calls the start endpoint for the given workflow id", async () => {
		postMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-2", projectId: "proj-1", objective: "ship it", state: "running" },
					steps: [],
				},
			},
			error: undefined,
		});

		const { result } = renderHook(() => useStartWorkflowRun("wf-2"), { wrapper });
		const data = await result.current.mutateAsync();

		expect(postMock).toHaveBeenCalledWith(
			"/api/v1/workflows/{workflowId}/start",
			expect.objectContaining({ params: { path: { workflowId: "wf-2" } } }),
		);
		expect(data.run.state).toBe("running");
	});

	it("rejects when no workflow id is set", async () => {
		const { result } = renderHook(() => useStartWorkflowRun(undefined), { wrapper });
		await expect(result.current.mutateAsync()).rejects.toThrow("workflow id is required");
		expect(postMock).not.toHaveBeenCalled();
	});
});

describe("useContinueWorkflowRun", () => {
	it("calls the continue endpoint for the given workflow id", async () => {
		postMock.mockResolvedValue({
			data: {
				workflow: {
					run: { id: "wf-2", projectId: "proj-1", objective: "ship it", state: "waiting" },
					steps: [],
				},
			},
			error: undefined,
		});

		const { result } = renderHook(() => useContinueWorkflowRun("wf-2"), { wrapper });
		const data = await result.current.mutateAsync();

		expect(postMock).toHaveBeenCalledWith(
			"/api/v1/workflows/{workflowId}/continue",
			expect.objectContaining({ params: { path: { workflowId: "wf-2" } } }),
		);
		expect(data.run.state).toBe("waiting");
	});

	it("rejects when no workflow id is set", async () => {
		const { result } = renderHook(() => useContinueWorkflowRun(undefined), { wrapper });
		await expect(result.current.mutateAsync()).rejects.toThrow("workflow id is required");
		expect(postMock).not.toHaveBeenCalled();
	});
});
