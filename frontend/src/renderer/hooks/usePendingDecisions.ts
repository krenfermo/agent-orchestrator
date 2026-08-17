import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type PendingDecisionQuestion = components["schemas"]["WorkflowQuestionResponse"];

export const pendingDecisionsQueryKey = ["pending-decisions"] as const;

async function fetchPendingDecisions(): Promise<PendingDecisionQuestion[]> {
	const { data, error } = await apiClient.GET("/api/v1/questions/pending");
	if (error) throw new Error(apiErrorMessage(error));
	return data.questions;
}

/**
 * Checkpoint 8K-B pass 3's global "Pending Decisions" inbox: every open/
 * in-flight question across ALL workflow runs (human_required, resolving,
 * and — read from the same "resolving" persisted state — waiting_for_
 * capacity). Read-only; answering still happens on the per-run questions
 * section this reuses QuestionCard from.
 */
export function usePendingDecisions() {
	const queryClient = useQueryClient();
	const query = useQuery({
		queryKey: pendingDecisionsQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchPendingDecisions,
		staleTime: 15 * 1000,
		refetchInterval: 15 * 1000,
	});

	// Answering from the global inbox hits the same per-run answer route
	// the run-detail page uses (Checkpoint 8K-A) — this hook just needs to
	// know which run a given question belongs to, which the pending list
	// response already carries (workflowRunId).
	const answerQuestion = useMutation({
		mutationFn: async ({
			workflowRunId,
			questionId,
			choiceId,
			customText,
		}: {
			workflowRunId: string;
			questionId: string;
			choiceId?: string;
			customText?: string;
		}) => {
			const { data, error } = await apiClient.POST("/api/v1/workflows/{workflowId}/questions/{questionId}/answer", {
				params: { path: { workflowId: workflowRunId, questionId } },
				body: { choiceId, customText },
			});
			if (error) throw error;
			return data.question;
		},
		onSuccess: () => {
			void queryClient.invalidateQueries({ queryKey: pendingDecisionsQueryKey });
		},
	});

	return {
		questions: query.data ?? [],
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		answerQuestion: answerQuestion.mutateAsync,
		answeringQuestion: answerQuestion.isPending,
		answerQuestionError: answerQuestion.error ? apiErrorMessage(answerQuestion.error) : undefined,
	};
}
