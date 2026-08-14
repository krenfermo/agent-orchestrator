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

	return {
		workflow: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		cancel: cancel.mutateAsync,
		cancelling: cancel.isPending,
		cancelError: cancel.error ? apiErrorMessage(cancel.error) : undefined,
	};
}
