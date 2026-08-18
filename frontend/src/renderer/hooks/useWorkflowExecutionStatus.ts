import { useQuery } from "@tanstack/react-query";
import { apiClient, hasTrustedApiBaseUrl } from "../lib/api-client";
import { workflowRunQueryKey, type WorkflowRunDetailView } from "./useWorkflowRun";

export type WorkflowStatusLabel =
	| "planning"
	| "waiting_for_decision"
	| "waiting_for_capacity"
	| "needs_attention"
	| "completed"
	| "reviewing"
	| "applying_fixes"
	| "verifying"
	| { kind: "executing_task"; current: number; total: number }
	| undefined;

/**
 * Checkpoint 8P-D §26: reads the currently in-flight master task's own child
 * workflow (steps only) so the parent status line can distinguish
 * Reviewing/Applying fixes/Verifying from a plain "Executing task X of Y" --
 * shares the same query key/cache useChildTaskRouting already reads, so this
 * is a cache hit whenever that hook has already fetched the same child, and
 * otherwise one small extra GET for just the active task.
 */
function useActiveChildSteps(executionWorkflowId: string | undefined) {
	const query = useQuery({
		queryKey: workflowRunQueryKey(executionWorkflowId ?? ""),
		enabled: Boolean(executionWorkflowId && hasTrustedApiBaseUrl()),
		staleTime: 5_000,
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/workflows/{workflowId}", {
				params: { path: { workflowId: executionWorkflowId as string } },
			});
			if (error) throw error;
			return data.workflow;
		},
	});
	return query.data?.steps;
}

/**
 * Checkpoint 8P-D §26: derives the compact status vocabulary the checkpoint
 * requires (Planning / Executing task X of Y / Reviewing / Applying fixes /
 * Verifying / Waiting for capacity / Waiting for your decision / Needs
 * attention / Completed) purely from fields the API already returns --
 * every fact here already exists in RunDetail/DTO, so this needed no
 * backend telemetry addition. Returns undefined when the run isn't a
 * master-plan run or none of these labels apply (e.g. a plain single-task
 * run, or the brief gap between one task completing and the next becoming
 * eligible), in which case callers should fall back to the existing
 * nextAction text.
 */
export function useWorkflowStatusLabel(workflow: WorkflowRunDetailView | undefined): WorkflowStatusLabel {
	const tasks = workflow?.tasks ?? [];
	const runningTask = tasks.find((task) => task.state === "running");
	const activeChildSteps = useActiveChildSteps(runningTask?.executionWorkflowId);

	if (!workflow) return undefined;

	const hasHumanRequiredQuestion = (workflow.questions ?? []).some((q) => q.state === "human_required");
	if (hasHumanRequiredQuestion) return "waiting_for_decision";
	if (workflow.run.state === "needs_attention") return "needs_attention";
	if (workflow.run.state === "completed") return "completed";
	if (workflow.run.waitReason) return "waiting_for_capacity";
	if (!workflow.plan) return undefined;

	if (workflow.plan.status === "pending" || workflow.plan.status === "running" || workflow.plan.status === "validated") {
		return "planning";
	}
	if (workflow.plan.status !== "approved") return undefined;

	const total = tasks.length;
	const runningIndex = tasks.findIndex((task) => task.state === "running");
	if (runningIndex === -1) return undefined;

	const currentStep = (activeChildSteps ?? []).find((step) => step.state !== "completed" && step.state !== "cancelled" && step.state !== "failed");
	switch (currentStep?.kind) {
		case "review":
			return "reviewing";
		case "fix":
			return "applying_fixes";
		case "verify":
			return "verifying";
		default:
			return { kind: "executing_task", current: runningIndex + 1, total };
	}
}
