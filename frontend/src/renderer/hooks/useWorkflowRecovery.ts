import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import { workflowRunQueryKey } from "./useWorkflowRun";
import { workflowRunsQueryKey } from "./useWorkflowRuns";

export type WorkflowRecoveryView = components["schemas"]["WorkflowRecoveryView"];
export type WorkflowRepairPlanView = components["schemas"]["WorkflowRepairPlanView"];
export type RecoveryAction = WorkflowRecoveryView["recommendedAction"];

/**
 * The four operations the recovery panel can offer. They map one-to-one onto
 * the named backend routes; there is no fifth thing the UI may do.
 */
export const RECOVERY_OPERATIONS = ["resume", "reuse_plan", "regenerate_plan", "repair"] as const;
export type RecoveryOperation = (typeof RECOVERY_OPERATIONS)[number];

export function workflowRecoveryQueryKey(workflowId: string) {
	return ["workflow-recovery", workflowId] as const;
}

/**
 * Reads the backend's deterministic recovery assessment for one run, and
 * exposes the four recovery operations.
 *
 * The contract this hook exists to hold: **the renderer never decides whether
 * a recovery action is safe or available.** Every such answer —
 * `recommendedAction`, `automaticAllowed`, `planReusable`, `repairAvailable`,
 * `repairEligibility` — is computed by the daemon from durable facts
 * (workflow.AssessRecovery) and is only rendered here. A second copy of those
 * rules in React would drift from the backend's the first time a stop reason
 * is added, and would drift silently, which is the failure mode the whole
 * assessment model exists to remove.
 *
 * It is polled alongside the run rather than derived from it, because the
 * assessment probes the project's planner context and the backend deliberately
 * keeps that off the run-detail read path.
 */
export function useWorkflowRecovery(workflowId: string | undefined, enabled: boolean) {
	const queryClient = useQueryClient();

	const query = useQuery({
		queryKey: workflowRecoveryQueryKey(workflowId ?? ""),
		enabled: Boolean(workflowId && enabled && hasTrustedApiBaseUrl()),
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/workflows/{workflowId}/recovery", {
				params: { path: { workflowId: workflowId as string } },
			});
			if (error) throw error;
			return data;
		},
		refetchInterval: 5_000,
	});

	// Every operation refreshes the run AND the assessment: after acting, the
	// answer has changed, and rendering the pre-action assessment would offer
	// the user a decision about a state that no longer exists.
	const invalidate = () => {
		if (!workflowId) return;
		void queryClient.invalidateQueries({ queryKey: workflowRunQueryKey(workflowId) });
		void queryClient.invalidateQueries({ queryKey: workflowRecoveryQueryKey(workflowId) });
		void queryClient.invalidateQueries({ queryKey: workflowRunsQueryKey() });
	};

	const operation = (path:
		| "/api/v1/workflows/{workflowId}/resume"
		| "/api/v1/workflows/{workflowId}/plan/reuse"
		| "/api/v1/workflows/{workflowId}/plan/regenerate") =>
		useMutation({
			mutationFn: async () => {
				if (!workflowId) throw new Error("workflow id is required");
				const { data, error } = await apiClient.POST(path, { params: { path: { workflowId } } });
				// A backend refusal must stay a refusal: throwing is what puts it
				// in the mutation's error state and in front of the user, instead
				// of a silent no-op the panel would render as success.
				if (error) throw error;
				return data.workflow;
			},
			onSuccess: invalidate,
		});

	const resume = operation("/api/v1/workflows/{workflowId}/resume");
	const reusePlan = operation("/api/v1/workflows/{workflowId}/plan/reuse");
	const regeneratePlan = operation("/api/v1/workflows/{workflowId}/plan/regenerate");

	const repair = useMutation({
		mutationFn: async () => {
			if (!workflowId) throw new Error("workflow id is required");
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/repair", {
				params: { path: { workflowId } },
			});
			if (error) throw error;
			return data;
		},
		onSuccess: invalidate,
	});

	const mutations = { resume, reuse_plan: reusePlan, regenerate_plan: regeneratePlan, repair } as const;
	const pending = Object.values(mutations).some((m) => m.isPending);
	const failed = Object.values(mutations).find((m) => m.error);

	return {
		recovery: query.data?.recovery,
		repairPlan: query.data?.repair,
		// P3-D: the recovery projection rides on the same response, so the
		// panel never has to ask a second time and see a different moment.
		recoveryStatus: query.data?.status,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		run: (op: RecoveryOperation) => {
			// One in-flight operation at a time. A keyboard repeat or a double
			// click can outrun a re-render, so the guard is here as well as on
			// the buttons' disabled state.
			if (pending) return;
			mutations[op].mutate();
		},
		pending,
		pendingOperation: (RECOVERY_OPERATIONS.find((op) => mutations[op].isPending) ?? undefined) as
			| RecoveryOperation
			| undefined,
		actionError: failed?.error ? apiErrorMessage(failed.error) : undefined,
	};
}
