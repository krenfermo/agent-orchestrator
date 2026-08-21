import { useQuery } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type BoardWorkflow = components["schemas"]["WorkflowBoardEntryView"];
export type BoardWorkflowTask = components["schemas"]["WorkflowBoardTaskView"];
export type BoardStepProgress = components["schemas"]["WorkflowStepProgressView"];
export type BoardPhase = BoardWorkflow["phase"];

export function projectBoardQueryKey(projectId: string) {
	return ["project-board", projectId] as const;
}

/** Phases after which nothing more will ever happen to a run. */
const TERMINAL_PHASES: ReadonlySet<string> = new Set(["completed", "failed", "cancelled"]);

export function isTerminalPhase(phase: string | undefined): boolean {
	return TERMINAL_PHASES.has(phase ?? "");
}

/**
 * True when this workflow genuinely cannot proceed without the user.
 *
 * The backend draws this line, not the renderer: `attention: "ao_internal"`
 * means AO has noticed a problem and is still handling it (a review that asked
 * for changes, a capacity wait with a scheduled retry), and must never render
 * as a request for help. Only `human_decision` may.
 */
export function needsHumanDecision(workflow: BoardWorkflow): boolean {
	return workflow.attention === "human_decision";
}

/** Board refresh cadence while anything is still moving. Matches the workflow detail page. */
const ACTIVE_REFETCH_MS = 2_000;

/**
 * Reads the project Board projection: every top-level workflow run of a
 * project, already projected onto the lifecycle vocabulary by the daemon.
 *
 * It polls while any workflow is non-terminal and stops once they all are, so
 * a finished board costs nothing. There is deliberately no CDC path here —
 * workflow tables emit no change_log events (see the lifecycle mapping doc),
 * so polling is the architecture, not a shortcut.
 */
export function useProjectBoard(projectId: string | undefined) {
	const query = useQuery({
		queryKey: projectBoardQueryKey(projectId ?? ""),
		enabled: Boolean(projectId) && hasTrustedApiBaseUrl(),
		refetchInterval: (query) => {
			const workflows = query.state.data;
			if (!workflows || workflows.length === 0) return false;
			return workflows.some((workflow) => !isTerminalPhase(workflow.phase)) ? ACTIVE_REFETCH_MS : false;
		},
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects/{projectId}/board", {
				params: { path: { projectId: projectId as string } },
			});
			if (error) throw error;
			return data.workflows;
		},
	});

	return {
		workflows: query.data ?? [],
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}
