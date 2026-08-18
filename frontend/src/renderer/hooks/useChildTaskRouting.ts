import { useQueries } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, hasTrustedApiBaseUrl } from "../lib/api-client";
import { workflowRunQueryKey } from "./useWorkflowRun";

export type ChildTaskRoutingSummary = {
	selectedLabel: string;
	fallbackUsed: boolean;
};

/**
 * Checkpoint 8P-C.1 §17: a compact "which provider did this task actually
 * use" summary per master-plan task row, read from the SAME persisted
 * routing decision the parent workflow detail page already surfaces (via
 * GET /api/v1/workflows/{id}) -- never a new endpoint, never a
 * recomputation. Shares the run-detail query cache/key with
 * useWorkflowRun, so opening a child run afterward is an instant
 * cache-hit, not a second fetch.
 */
export function useChildTaskRouting(executionWorkflowIds: string[]) {
	const queries = useQueries({
		queries: executionWorkflowIds.map((id) => ({
			queryKey: workflowRunQueryKey(id),
			enabled: hasTrustedApiBaseUrl(),
			staleTime: 30_000,
			queryFn: async () => {
				const { data, error } = await apiClient.GET("/api/v1/workflows/{workflowId}", { params: { path: { workflowId: id } } });
				if (error) throw error;
				return data.workflow;
			},
		})),
	});

	const summaries: Record<string, ChildTaskRoutingSummary | undefined> = {};
	executionWorkflowIds.forEach((id, index) => {
		const workflow = queries[index]?.data as components["schemas"]["WorkflowRunDetailView"] | undefined;
		const workStep = workflow?.steps.find((s) => s.kind === "work");
		const routing = workStep?.routing;
		if (!routing) return;
		const selectedLabel = routing.selectedProfile?.displayName || routing.selectedHarness;
		if (!selectedLabel) return;
		summaries[id] = { selectedLabel, fallbackUsed: routing.fallbackUsed };
	});
	return summaries;
}
