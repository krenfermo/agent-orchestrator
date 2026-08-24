import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, hasTrustedApiBaseUrl } from "../lib/api-client";
import { workflowRunQueryKey } from "./useWorkflowRun";

// The generated names carry the controllers-package prefix the spec builder
// applies to every controller type; aliasing them here keeps that detail out of
// the components that consume them.
export type IncidentView = components["schemas"]["ControllersIncidentView"];
export type IncidentDiagnosisView = components["schemas"]["ControllersIncidentDiagnosisView"];
export type IncidentActionView = components["schemas"]["ControllersIncidentActionView"];

export function workflowIncidentQueryKey(workflowId: string) {
	return ["workflow-incident", workflowId] as const;
}

/**
 * Reads the incident behind a stopped run, and exposes the two mutations the
 * "¿Qué hago?" modal needs.
 *
 * Everything this hook returns is decided by the backend. It does not compute
 * whether an action needs approval, whether it is executable, what it will do,
 * or what the risk is — those come down the wire as fields, because the same
 * policy governs the executor and a second copy here would drift from it. The
 * failure mode of that drift is a control that looks safe and is not, which is
 * exactly what the Advisor exists to prevent.
 *
 * The query only polls while a diagnosis is actually in flight. An incident
 * that is open, diagnosed or terminal does not change on its own, and polling
 * one would be a request every two seconds for an unchanging answer.
 */
export function useWorkflowIncident(workflowId: string | undefined, enabled: boolean) {
	const queryClient = useQueryClient();

	const query = useQuery({
		queryKey: workflowIncidentQueryKey(workflowId ?? ""),
		enabled: Boolean(workflowId && enabled && hasTrustedApiBaseUrl()),
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/workflows/{workflowId}/incident", {
				params: { path: { workflowId: workflowId as string } },
			});
			if (error) throw error;
			return data.incident;
		},
		// Poll while something is genuinely moving, and stop when it is not. The
		// set is the backend's own progress vocabulary rather than a guess about
		// which incident states are busy, so a new state cannot silently become
		// un-polled.
		refetchInterval: (state) => {
			const progress = state.state.data?.progress;
			const moving = new Set(["diagnosing", "waiting_capacity", "repairing", "reviewing", "verifying"]);
			return progress && moving.has(progress) ? 3_000 : false;
		},
	});

	const invalidate = () => {
		if (!workflowId) return;
		void queryClient.invalidateQueries({ queryKey: workflowIncidentQueryKey(workflowId) });
		void queryClient.invalidateQueries({ queryKey: workflowRunQueryKey(workflowId) });
	};

	const diagnose = useMutation({
		mutationFn: async () => {
			if (!workflowId) throw new Error("workflow id is required");
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/incident/diagnose", {
				params: { path: { workflowId } },
			});
			if (error) throw error;
			return data.incident;
		},
		onSuccess: invalidate,
	});

	/**
	 * `approve` is passed explicitly rather than inferred from the click. The
	 * backend attributes the approval to the authenticated caller and records
	 * who gave it, so this flag is the UI's half of a durable consent — not a
	 * convenience.
	 */
	const execute = useMutation({
		mutationFn: async ({ incidentId, approve }: { incidentId: string; approve: boolean }) => {
			if (!workflowId) throw new Error("workflow id is required");
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/incident/execute", {
				params: { path: { workflowId } },
				body: { incidentId, approve },
			});
			if (error) throw error;
			return data.incident;
		},
		onSuccess: invalidate,
	});

	return { query, diagnose, execute };
}
