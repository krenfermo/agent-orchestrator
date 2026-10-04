import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { workflowRunQueryKey } from "./useWorkflowRun";

export type WorkflowPendingChanges = components["schemas"]["WorkflowPendingChangesResponse"];
export type WorkflowCommitOutcome = components["schemas"]["CommitPendingChangesResponse"];

export function workflowPendingChangesQueryKey(workflowId: string) {
	return ["workflow-pending-changes", workflowId] as const;
}

/**
 * P3-A §17's commit-and-continue flow, renderer side.
 *
 * The read and the write are two operations here for the same reason they are
 * two routes in the daemon: seeing what is pending must not require the ability
 * to commit it. The query is enabled only while the dialog is open, because
 * `git status` on somebody's repository is a real side effect of looking at a
 * page and should happen when they asked to look.
 *
 * Nothing in this hook decides anything. It does not compute a commit message
 * (the daemon proposes one and the user edits it), it does not decide whether
 * the tree is clean (the daemon re-probes), and it does not resume the run (the
 * daemon does, and only after proving the tree is clean).
 */
export function useWorkflowPendingChanges(workflowId: string, enabled: boolean) {
	const queryClient = useQueryClient();
	const query = useQuery({
		queryKey: workflowPendingChangesQueryKey(workflowId),
		enabled,
		// Always refetched when the dialog opens: a person is about to commit
		// what this list shows, and showing them a cached list from five minutes
		// ago would be showing them the wrong thing to approve.
		staleTime: 0,
		gcTime: 0,
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/workflows/{workflowId}/pending-changes", {
				params: { path: { workflowId } },
			});
			if (error) throw error;
			return data;
		},
	});

	const commit = useMutation({
		mutationFn: async (message: string) => {
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/pending-changes/commit", {
				params: { path: { workflowId } },
				body: { message },
			});
			if (error) throw error;
			return data;
		},
		onSuccess: () => {
			// The run's own state may have moved (committed, re-probed, resumed),
			// and the pending list certainly has.
			void queryClient.invalidateQueries({ queryKey: workflowRunQueryKey(workflowId) });
			void queryClient.invalidateQueries({ queryKey: workflowPendingChangesQueryKey(workflowId) });
		},
	});

	return {
		pending: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		commit: commit.mutateAsync,
		committing: commit.isPending,
		commitError: commit.error ? apiErrorMessage(commit.error) : undefined,
		commitOutcome: commit.data,
		reset: commit.reset,
	};
}
