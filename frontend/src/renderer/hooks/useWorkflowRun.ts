import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import { workflowRunsQueryKey } from "./useWorkflowRuns";

export type WorkflowRunDetailView = components["schemas"]["WorkflowRunDetailView"];
export type WorkflowRunState = components["schemas"]["WorkflowRunView"]["state"];

const terminalStates = new Set<WorkflowRunState>(["completed", "failed", "cancelled"]);

export function workflowRunIsTerminal(state: WorkflowRunState | undefined): boolean {
	return Boolean(state && terminalStates.has(state));
}

export function workflowRunQueryKey(workflowId: string) {
	return ["workflow-run", workflowId] as const;
}

/**
 * Reads one workflow run with its steps/attempts and exposes a cancel
 * mutation. There is no push channel for workflow rows (see the CDC
 * deviation noted in the migration), so this polls while the run is
 * non-terminal, mirroring useSessionInterfaceTransition.
 */
export function useWorkflowRun(workflowId: string | undefined) {
	const queryClient = useQueryClient();
	const query = useQuery({
		queryKey: workflowRunQueryKey(workflowId ?? ""),
		enabled: Boolean(workflowId && hasTrustedApiBaseUrl()),
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/workflows/{workflowId}", {
				params: { path: { workflowId: workflowId as string } },
			});
			if (error) throw error;
			return data.workflow;
		},
		refetchInterval: (state) => (workflowRunIsTerminal(state.state.data?.run.state) ? false : 2_000),
	});

	const cancel = useMutation({
		mutationFn: async () => {
			if (!workflowId) throw new Error("workflow id is required");
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/cancel", {
				params: { path: { workflowId } },
			});
			if (error) throw error;
			return data.workflow;
		},
		onSuccess: () => {
			if (!workflowId) return;
			void queryClient.invalidateQueries({ queryKey: workflowRunQueryKey(workflowId) });
			void queryClient.invalidateQueries({ queryKey: workflowRunsQueryKey() });
		},
	});

	const start = useMutation({
		mutationFn: async () => {
			if (!workflowId) throw new Error("workflow id is required");
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/start", {
				params: { path: { workflowId } },
			});
			if (error) throw error;
			return data.workflow;
		},
		onSuccess: () => {
			if (!workflowId) return;
			void queryClient.invalidateQueries({ queryKey: workflowRunQueryKey(workflowId) });
			void queryClient.invalidateQueries({ queryKey: workflowRunsQueryKey() });
		},
	});

	const continueRun = useMutation({
		mutationFn: async () => {
			if (!workflowId) throw new Error("workflow id is required");
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/continue", {
				params: { path: { workflowId } },
			});
			if (error) throw error;
			return data.workflow;
		},
		onSuccess: () => {
			if (!workflowId) return;
			void queryClient.invalidateQueries({ queryKey: workflowRunQueryKey(workflowId) });
			void queryClient.invalidateQueries({ queryKey: workflowRunsQueryKey() });
		},
	});

	return {
		workflow: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		cancel: cancel.mutateAsync,
		cancelling: cancel.isPending,
		cancelError: cancel.error ? apiErrorMessage(cancel.error) : undefined,
		start: start.mutateAsync,
		starting: start.isPending,
		startError: start.error ? apiErrorMessage(start.error) : undefined,
		continueRun: continueRun.mutateAsync,
		continuing: continueRun.isPending,
		continueError: continueRun.error ? apiErrorMessage(continueRun.error) : undefined,
	};
}

/**
 * Standalone start mutation, for callers that only need to trigger /start
 * (e.g. a list view) without subscribing to the full run detail query.
 */
export function useStartWorkflowRun(workflowId: string | undefined) {
	const queryClient = useQueryClient();
	return useMutation({
		mutationFn: async () => {
			if (!workflowId) throw new Error("workflow id is required");
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/start", {
				params: { path: { workflowId } },
			});
			if (error) throw error;
			return data.workflow;
		},
		onSuccess: () => {
			if (!workflowId) return;
			void queryClient.invalidateQueries({ queryKey: workflowRunQueryKey(workflowId) });
			void queryClient.invalidateQueries({ queryKey: workflowRunsQueryKey() });
		},
	});
}

/**
 * Standalone continue mutation (Checkpoint 8C), for callers that only need
 * to trigger /continue (e.g. a list view) without subscribing to the full
 * run detail query. Mirrors useStartWorkflowRun exactly.
 */
export function useContinueWorkflowRun(workflowId: string | undefined) {
	const queryClient = useQueryClient();
	return useMutation({
		mutationFn: async () => {
			if (!workflowId) throw new Error("workflow id is required");
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/continue", {
				params: { path: { workflowId } },
			});
			if (error) throw error;
			return data.workflow;
		},
		onSuccess: () => {
			if (!workflowId) return;
			void queryClient.invalidateQueries({ queryKey: workflowRunQueryKey(workflowId) });
			void queryClient.invalidateQueries({ queryKey: workflowRunsQueryKey() });
		},
	});
}
