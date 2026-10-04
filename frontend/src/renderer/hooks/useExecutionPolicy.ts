import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type ExecutionPolicy = components["schemas"]["ControllersExecutionPolicyView"];
export type FallbackBehavior = ExecutionPolicy["fallbackBehavior"];
export type ReviewIndependence = ExecutionPolicy["reviewIndependence"];

export const executionPolicyQueryKey = ["execution-policy"] as const;

async function fetchExecutionPolicy(): Promise<ExecutionPolicy> {
	const { data, error } = await apiClient.GET("/api/v1/execution-policy");
	if (error) throw new Error(apiErrorMessage(error));
	return data.policy;
}

/**
 * Checkpoint 8P-C: Settings → Execution Policy. Server always derives the
 * current user from the authenticated request -- this hook never sends a
 * userId. GET returns the caller's stored policy, or the documented
 * bootstrap default (built from their own connected profiles) when none has
 * been saved yet, so the UI always has something concrete to render/edit.
 */
export function useExecutionPolicy() {
	const queryClient = useQueryClient();

	const policy = useQuery({
		queryKey: executionPolicyQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchExecutionPolicy,
		staleTime: 30 * 1000,
	});

	const save = useMutation({
		mutationFn: async (input: {
			autonomousMode: boolean;
			plannerPriority: string[];
			workerPriority: string[];
			reviewerPriority: string[];
			decisionResolverPriority: string[];
			fallbackBehavior: FallbackBehavior;
			reviewIndependence: ReviewIndependence;
		}) => {
			const { data, error } = await apiClient.PUT("/api/v1/execution-policy", { body: input });
			if (error) throw new Error(apiErrorMessage(error));
			return data.policy;
		},
		onSuccess: (saved) => queryClient.setQueryData(executionPolicyQueryKey, saved),
	});

	return {
		policy: policy.data,
		isLoading: policy.isLoading,
		error: policy.error ? apiErrorMessage(policy.error) : undefined,
		save: save.mutateAsync,
		isSaving: save.isPending,
	};
}
