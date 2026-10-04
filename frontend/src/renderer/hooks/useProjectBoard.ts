import { useQuery } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type BoardWorkflow = components["schemas"]["WorkflowBoardEntryView"];
export type BoardWorkflowTask = components["schemas"]["WorkflowBoardTaskView"];
export type BoardStepProgress = components["schemas"]["WorkflowStepProgressView"];
export type BoardBranchWait = components["schemas"]["WorkflowBranchWaitView"];
export type BoardPhase = BoardWorkflow["phase"];
export type BoardRepair = components["schemas"]["WorkflowBoardRepairView"];
export type BoardCounts = components["schemas"]["WorkflowBoardCountsView"];

/**
 * The four views the Board offers (P3-B §14). They are filters over ONE
 * projection, applied by the daemon: "Needs attention" is not a different card,
 * it is the same card selected by the same `requiresHuman` the card renders.
 */
export type BoardView = "active" | "attention" | "completed" | "archived";

export function projectBoardQueryKey(projectId: string, view: BoardView = "active") {
	return ["project-board", projectId, view] as const;
}

/** The empty counts a board that has not loaded yet reports. */
export const EMPTY_BOARD_COUNTS: BoardCounts = {
	active: 0,
	working: 0,
	waiting: 0,
	needsAttention: 0,
	completed: 0,
	archived: 0,
};

/** States after which nothing more will ever happen to a run. */
const TERMINAL_STATES: ReadonlySet<string> = new Set(["completed", "failed", "cancelled"]);

export function isTerminalPhase(phase: string | undefined): boolean {
	return TERMINAL_STATES.has(phase ?? "");
}

/**
 * The stage vocabulary's terminal members. It is the same set as the phase
 * one — the two vocabularies agree about what "over" means, which is a
 * property of the daemon's projection rather than a coincidence.
 */
export function isTerminalStage(stage: string | undefined): boolean {
	return TERMINAL_STATES.has(stage ?? "");
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
/**
 * The query the daemon applies for each view. Nothing is filtered in React:
 * the daemon owns the projection, so it owns the selection over it — which is
 * also what keeps the counts and the cards from being computed by two
 * different rules.
 */
function boardQueryFor(view: BoardView): Record<string, string> {
	switch (view) {
		case "attention":
			return { requiresHuman: "true" };
		case "completed":
			return { stage: "completed" };
		default:
			return {};
	}
}

export function useProjectBoard(projectId: string | undefined, view: BoardView = "active") {
	const query = useQuery({
		queryKey: projectBoardQueryKey(projectId ?? "", view),
		enabled: Boolean(projectId) && hasTrustedApiBaseUrl() && view !== "archived",
		refetchInterval: (query) => {
			const workflows = query.state.data?.workflows;
			if (!workflows || workflows.length === 0) return false;
			return workflows.some((workflow) => !isTerminalStage(workflow.stage)) ? ACTIVE_REFETCH_MS : false;
		},
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects/{projectId}/board", {
				params: { path: { projectId: projectId as string }, query: boardQueryFor(view) },
			});
			if (error) throw error;
			return data;
		},
	});

	return {
		workflows: query.data?.workflows ?? [],
		counts: query.data?.counts ?? EMPTY_BOARD_COUNTS,
		matched: query.data?.matched ?? 0,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}
