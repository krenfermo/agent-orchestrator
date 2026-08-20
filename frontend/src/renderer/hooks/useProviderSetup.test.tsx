import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { postMock, deleteMock } = vi.hoisted(() => ({ postMock: vi.fn(), deleteMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { POST: postMock, DELETE: deleteMock },
	apiErrorMessage: (err: unknown) => (err instanceof Error ? err.message : String(err)),
}));

import { useProviderSetup } from "./useProviderSetup";

function wrapper({ children }: { children: ReactNode }) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
	postMock.mockReset();
	deleteMock.mockReset().mockResolvedValue({ error: undefined });
	vi.useFakeTimers();
});

afterEach(() => {
	vi.useRealTimers();
});

describe("useProviderSetup", () => {
	it("starts a setup session and moves to waiting with the returned handle/instructions", async () => {
		postMock.mockResolvedValueOnce({ data: { handleId: "h-1", instructions: "Run /login." }, error: undefined });

		const { result } = renderHook(() => useProviderSetup("prof-1"), { wrapper });
		await act(async () => {
			await result.current.start();
		});

		expect(result.current.phase).toBe("waiting");
		expect(result.current.handleId).toBe("h-1");
		expect(result.current.instructions).toBe("Run /login.");
		expect(postMock).toHaveBeenCalledWith("/api/v1/provider-profiles/{id}/setup", { params: { path: { id: "prof-1" } } });
	});

	it("polls Test Connection and auto-stops once authenticated, without a manual click", async () => {
		postMock
			.mockResolvedValueOnce({ data: { handleId: "h-1", instructions: "Run /login." }, error: undefined }) // start
			.mockResolvedValueOnce({ data: { ok: false }, error: undefined }) // first poll: still waiting
			.mockResolvedValueOnce({ data: { ok: true }, error: undefined }); // second poll: authenticated

		const { result } = renderHook(() => useProviderSetup("prof-1"), { wrapper });
		await act(async () => {
			await result.current.start();
		});
		expect(result.current.phase).toBe("waiting");

		await act(async () => {
			await vi.advanceTimersByTimeAsync(3000);
		});
		expect(result.current.phase).toBe("waiting");

		await act(async () => {
			await vi.advanceTimersByTimeAsync(3000);
		});
		expect(result.current.phase).toBe("idle");
		expect(result.current.handleId).toBeUndefined();
		// The dialog must never require the user to click Test Connection
		// themselves -- the poll's own success closes the terminal.
		expect(deleteMock).toHaveBeenCalledWith("/api/v1/provider-profiles/{id}/setup", { params: { path: { id: "prof-1" } } });
	});

	it("times out after the bound and stops polling instead of running forever", async () => {
		postMock.mockResolvedValueOnce({ data: { handleId: "h-1", instructions: "Run /login." }, error: undefined });
		postMock.mockResolvedValue({ data: { ok: false }, error: undefined });

		const { result } = renderHook(() => useProviderSetup("prof-1"), { wrapper });
		await act(async () => {
			await result.current.start();
		});

		await act(async () => {
			await vi.advanceTimersByTimeAsync(10 * 60 * 1000 + 1000);
		});
		expect(result.current.phase).toBe("timed_out");

		const pollCallsAtTimeout = postMock.mock.calls.length;
		await act(async () => {
			await vi.advanceTimersByTimeAsync(30_000);
		});
		expect(postMock.mock.calls.length).toBe(pollCallsAtTimeout);
	});

	it("stop() closes the terminal and resets to idle (explicit Cancel)", async () => {
		postMock.mockResolvedValueOnce({ data: { handleId: "h-1", instructions: "Run /login." }, error: undefined });

		const { result } = renderHook(() => useProviderSetup("prof-1"), { wrapper });
		await act(async () => {
			await result.current.start();
		});
		await act(async () => {
			await result.current.stop();
		});

		expect(result.current.phase).toBe("idle");
		expect(result.current.handleId).toBeUndefined();
		expect(deleteMock).toHaveBeenCalledWith("/api/v1/provider-profiles/{id}/setup", { params: { path: { id: "prof-1" } } });
	});

	it("surfaces a start failure as phase error without leaving a dangling poll", async () => {
		postMock.mockResolvedValueOnce({ data: undefined, error: { message: "PROVIDER_CLI_NOT_INSTALLED" } });

		const { result } = renderHook(() => useProviderSetup("prof-1"), { wrapper });
		await act(async () => {
			await result.current.start();
		});

		expect(result.current.phase).toBe("error");
		expect(result.current.handleId).toBeUndefined();
		await act(async () => {
			await vi.advanceTimersByTimeAsync(10_000);
		});
		// Only the failed start call -- no poll ever began.
		expect(postMock).toHaveBeenCalledTimes(1);
	});
});
