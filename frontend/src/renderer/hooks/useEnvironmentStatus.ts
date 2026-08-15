import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type EnvironmentStatus = components["schemas"]["EnvironmentStatus"];
export type GitHubStatus = components["schemas"]["EnvironmentGitHubStatus"];

export const environmentStatusQueryKey = ["environment-status"] as const;

async function fetchEnvironmentStatus(): Promise<EnvironmentStatus> {
	const { data, error } = await apiClient.GET("/api/v1/environment/status");
	if (error) throw new Error(apiErrorMessage(error));
	return data;
}

/**
 * Settings' "Environment readiness" surface and everything that gates on it
 * (the Workflows create form) read this. Every field comes from a real local
 * probe run by the daemon — never invented — so a stale query just means the
 * daemon hasn't been asked recently, not that the underlying fact is wrong.
 */
export function useEnvironmentStatus() {
	const queryClient = useQueryClient();
	const query = useQuery({
		queryKey: environmentStatusQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchEnvironmentStatus,
		staleTime: 60 * 1000,
	});

	const testGitHub = useMutation({
		mutationFn: async (): Promise<GitHubStatus> => {
			const { data, error } = await apiClient.POST("/api/v1/environment/github/test");
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
		onSuccess: () => {
			void queryClient.invalidateQueries({ queryKey: environmentStatusQueryKey });
		},
	});

	return {
		status: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		refetch: query.refetch,
		testGitHub: testGitHub.mutateAsync,
		testingGitHub: testGitHub.isPending,
	};
}
