import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import { type BoardWorkflow, projectBoardQueryKey } from "./useProjectBoard";

export const cancelAndArchiveWorkflowMutationKey = ["cancel-archive-workflow"] as const;

export function projectBoardHistoryQueryKey(projectId: string) {
	return ["project-board-history", projectId] as const;
}

/**
 * Workflow states a user may cancel and archive.
 *
 * Deliberately not "everything": a workflow AO is actively driving
 * (planning/running/reviewing/fixing/verifying) is not stale, and the button
 * for it is Cancel, not Cancel-and-archive. What belongs here is the set the
 * Board could otherwise show forever — a run parked for a human, one queued on
 * something that never came, and one already finished whose card the user wants
 * out of the way.
 */
const ARCHIVABLE_STATES: ReadonlySet<string> = new Set([
	"needs_attention",
	"waiting",
	"failed",
	"cancelled",
	"completed",
	"pending",
]);

export function canCancelAndArchive(workflow: Pick<BoardWorkflow, "state">): boolean {
	return ARCHIVABLE_STATES.has(workflow.state);
}

/**
 * Cancels a workflow and moves it off the active Board.
 *
 * The daemon does the actual work — cascade to child runs, branch-lock release
 * through the branch-lock lifecycle, wake cancellation, audit checkpoints. This
 * hook never hides anything client-side: the card disappears because the
 * refetched board no longer contains it, so a workflow that is genuinely still
 * active cannot be made to vanish by pressing the button.
 *
 * The board query is both updated in place and invalidated: the in-place update
 * is what makes the card go away in the same frame (no application restart, no
 * waiting for the 2s poll), and the invalidation is what makes the daemon —
 * not the renderer — the final word on what the board contains.
 */
export function useCancelAndArchiveWorkflow(projectId: string | undefined) {
	const queryClient = useQueryClient();
	return useMutation({
		mutationKey: cancelAndArchiveWorkflowMutationKey,
		mutationFn: async (workflowId: string) => {
			const { data, error, response } = await apiClient.POST("/api/v1/workflows/{workflowId}/cancel-archive", {
				params: { path: { workflowId } },
			});
			if (error) {
				const fallback = response
					? `Failed to cancel and archive workflow (${response.status})`
					: "Failed to cancel and archive workflow";
				throw new Error(apiErrorMessage(error, fallback));
			}
			return data;
		},
		onSuccess: async (_data, workflowId) => {
			if (!projectId) return;
			queryClient.setQueryData<BoardWorkflow[]>(projectBoardQueryKey(projectId), (current) =>
				current ? current.filter((workflow) => workflow.workflowId !== workflowId) : current,
			);
			await Promise.all([
				queryClient.invalidateQueries({ queryKey: projectBoardQueryKey(projectId) }),
				queryClient.invalidateQueries({ queryKey: projectBoardHistoryQueryKey(projectId) }),
			]);
		},
	});
}

/**
 * Reads the archived workflows of a project ("Mostrar archivados").
 *
 * Only fetched while the history is actually open, and never polled: an
 * archived workflow is finished by definition, so there is nothing to watch.
 */
export function useProjectBoardHistory(projectId: string | undefined, enabled: boolean) {
	const query = useQuery({
		queryKey: projectBoardHistoryQueryKey(projectId ?? ""),
		enabled: Boolean(projectId) && enabled && hasTrustedApiBaseUrl(),
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects/{projectId}/board/history", {
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
