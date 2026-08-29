import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type WorkflowRunView = components["schemas"]["WorkflowRunView"];

/**
 * The canonical execution strategies a run may be created under. "auto" is a
 * valid API request value too, but the create form always states a choice, so
 * it is deliberately not offered here.
 */
export const EXECUTION_STRATEGIES = ["task", "autonomous", "master"] as const;
export type ExecutionStrategy = (typeof EXECUTION_STRATEGIES)[number];

/** Approval is a separate axis from strategy: who approves and drives the run. */
export const APPROVAL_POLICIES = ["automatic", "manual"] as const;
export type ApprovalPolicy = (typeof APPROVAL_POLICIES)[number];

/**
 * P1-B: the run's frozen auto-repair policy. A third independent axis — it
 * decides what AO may do to a run unattended when a repairable technical stop
 * happens, and it is frozen at creation like the other two.
 */
export const REPAIR_POLICIES = ["disabled", "suggest", "automatic"] as const;
export type RepairPolicy = (typeof REPAIR_POLICIES)[number];

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
		mutationFn: async (input: {
			projectId: string;
			objective: string;
			strategy: ExecutionStrategy;
			approvalPolicy: ApprovalPolicy;
			repairPolicy: RepairPolicy;
		}) => {
			const { data, error } = await apiClient.POST("/api/v1/projects/{projectId}/workflows", {
				params: { path: { projectId: input.projectId } },
				body: {
					objective: input.objective,
					// P1-A: strategy and approval are two independent axes, and the
					// daemon owns both decisions. The renderer no longer derives
					// masterPlan/planApprovalMode/autonomous itself — those were the
					// implicit flags the execution-strategy model replaces.
					strategy: input.strategy,
					approvalPolicy: input.approvalPolicy,
					repairPolicy: input.repairPolicy,
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
