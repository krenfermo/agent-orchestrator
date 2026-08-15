import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type ProjectSummary = components["schemas"]["ProjectSummary"];

export const projectsListQueryKey = ["projects-list"] as const;

async function fetchProjects(): Promise<ProjectSummary[]> {
	const { data, error } = await apiClient.GET("/api/v1/projects");
	if (error) throw new Error(apiErrorMessage(error));
	return data.projects;
}

/**
 * Registered-project list shared by the Workflows "New workflow" picker and
 * Settings → Projects. A single query key keeps both surfaces in sync: a
 * project registered or cloned from Settings appears in the Workflows
 * dropdown without a manual refresh.
 */
export function useProjectsList() {
	const query = useQuery({
		queryKey: projectsListQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchProjects,
	});

	return {
		projects: query.data ?? [],
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}

/** Imperative invalidation for mutations defined outside this hook (registration, clone). */
export function useInvalidateProjectsList() {
	const queryClient = useQueryClient();
	return () => queryClient.invalidateQueries({ queryKey: projectsListQueryKey });
}
