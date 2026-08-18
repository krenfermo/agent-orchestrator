import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type WorkflowRunView = components["schemas"]["WorkflowRunView"];

export function workflowRunsQueryKey(projectId?: string) {
	return ["workflow-runs", projectId ?? ""] as const;
}

/**
 * Lists workflow run summaries, optionally filtered by project, and exposes a
 * create mutation. Checkpoint 8A: this is structure only — creating a run
 * seeds its initial steps but nothing executes them yet.
 */
export function useWorkflowRuns(projectId?: string) {
	const queryClient = useQueryClient();
	const query = useQuery({
		queryKey: workflowRunsQueryKey(projectId),
		enabled: hasTrustedApiBaseUrl(),
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/workflows", {
				params: { query: projectId ? { projectId } : {} },
			});
			if (error) throw error;
			return data.workflows;
		},
	});

	const create = useMutation({
		mutationFn: async (input: { projectId: string; objective: string; autonomous: boolean }) => {
			const { data, error } = await apiClient.POST("/api/v1/projects/{projectId}/workflows", {
				params: { path: { projectId: input.projectId } },
				body: {
					objective: input.objective,
					masterPlan: true,
					planApprovalMode: input.autonomous ? "auto" : "manual",
					autonomous: input.autonomous,
				},
			});
			if (error) throw error;
			return data.workflow;
		},
		onSuccess: () => {
			void queryClient.invalidateQueries({ queryKey: workflowRunsQueryKey(projectId) });
		},
	});

	return {
		runs: query.data ?? [],
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		createRun: create.mutateAsync,
		creating: create.isPending,
		createError: create.error ? apiErrorMessage(create.error) : undefined,
		resetCreateError: create.reset,
	};
}
