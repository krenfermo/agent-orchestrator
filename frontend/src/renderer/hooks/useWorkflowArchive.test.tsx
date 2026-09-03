import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

const { postMock, getMock } = vi.hoisted(() => ({ postMock: vi.fn(), getMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { POST: postMock, GET: getMock },
	apiErrorMessage: (_error: unknown, fallback?: string) => fallback ?? "error",
	hasTrustedApiBaseUrl: () => true,
}));

import { projectBoardQueryKey, type BoardWorkflow } from "./useProjectBoard";
import {
	canCancelAndArchive,
	projectBoardHistoryQueryKey,
	useCancelAndArchiveWorkflow,
	useProjectBoardHistory,
} from "./useWorkflowArchive";

function boardWorkflow(id: string, state: BoardWorkflow["state"] = "needs_attention"): BoardWorkflow {
	return {
		workflowId: id,
		projectId: "proj",
		objective: "an objective",
		state,
		phase: "needs_attention",
		stage: "needs_attention",
		requiresHuman: true,
		automaticActionActive: false,
		executionMode: "autonomous",
		lastActivityAt: new Date().toISOString(),
		lastMeaningfulActivityAt: new Date().toISOString(),
		reviewCycles: 0,
		tasksTotal: 0,
		tasksCompleted: 0,
		tasksRunning: 0,
		tasksBlocked: 0,
		tasksEligible: 0,
		tasksFailed: 0,
		tasksNeedsAttention: 0,
	};
}

function wrapperFor(client: QueryClient) {
	return function Wrapper({ children }: { children: ReactNode }) {
		return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	};
}

describe("canCancelAndArchive", () => {
	// The safety rule, stated as a test: the renderer must never offer to
	// retire a workflow AO is actively driving.
	it("offers the action for stale states and withholds it for a running one", () => {
		for (const state of ["needs_attention", "waiting", "failed", "cancelled", "completed"] as const) {
			expect(canCancelAndArchive({ state })).toBe(true);
		}
		expect(canCancelAndArchive({ state: "running" })).toBe(false);
	});
});

describe("useCancelAndArchiveWorkflow", () => {
	// Requirement: the card leaves the active Board immediately, without an
	// application restart and without waiting for the next poll.
	// P3-B §10: the renderer holds no optimistic board state. Archiving asks the
	// daemon and then re-reads it; the card goes away because the daemon's next
	// board does not contain it. Removing it locally would make a workflow the
	// daemon refused to retire vanish anyway, and would not survive a restart.
	it("re-reads the board from the daemon instead of editing it locally", async () => {
		postMock.mockResolvedValue({ data: { workflow: { run: { id: "wf-stale" } } } });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const invalidated: unknown[] = [];
		const realInvalidate = client.invalidateQueries.bind(client);
		vi.spyOn(client, "invalidateQueries").mockImplementation((filters) => {
			invalidated.push((filters as { queryKey?: unknown })?.queryKey);
			return realInvalidate(filters);
		});

		const { result } = renderHook(() => useCancelAndArchiveWorkflow("proj"), { wrapper: wrapperFor(client) });
		await result.current.mutateAsync("wf-stale");

		await waitFor(() => expect(invalidated).toContainEqual(["project-board", "proj"]));
		expect(invalidated).toContainEqual(projectBoardHistoryQueryKey("proj"));
		expect(postMock).toHaveBeenCalledWith("/api/v1/workflows/{workflowId}/cancel-archive", {
			params: { path: { workflowId: "wf-stale" } },
		});
	});

	// Nothing is hidden client-side on failure: a workflow that is still
	// running must stay visible if the daemon refused to retire it.
	it("leaves the board untouched when the daemon rejects the request", async () => {
		postMock.mockResolvedValue({ error: { detail: "nope" }, response: { status: 422 } });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
		client.setQueryData(projectBoardQueryKey("proj"), [boardWorkflow("wf-stale")]);

		const { result } = renderHook(() => useCancelAndArchiveWorkflow("proj"), { wrapper: wrapperFor(client) });
		await expect(result.current.mutateAsync("wf-stale")).rejects.toThrow();

		expect(client.getQueryData<BoardWorkflow[]>(projectBoardQueryKey("proj"))?.map((w) => w.workflowId)).toEqual([
			"wf-stale",
		]);
	});
});

describe("useProjectBoardHistory", () => {
	it("does not fetch until the archived view is opened", async () => {
		getMock.mockResolvedValue({ data: { workflows: [boardWorkflow("wf-archived", "cancelled")] } });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

		const closed = renderHook(() => useProjectBoardHistory("proj", false), { wrapper: wrapperFor(client) });
		expect(closed.result.current.workflows).toEqual([]);
		expect(getMock).not.toHaveBeenCalled();

		const open = renderHook(() => useProjectBoardHistory("proj", true), { wrapper: wrapperFor(client) });
		await waitFor(() => expect(open.result.current.workflows).toHaveLength(1));
		expect(getMock).toHaveBeenCalledWith("/api/v1/projects/{projectId}/board/history", {
			params: { path: { projectId: "proj" }, query: {} },
		});
		expect(client.getQueryData(projectBoardHistoryQueryKey("proj"))).toBeDefined();
	});
});
