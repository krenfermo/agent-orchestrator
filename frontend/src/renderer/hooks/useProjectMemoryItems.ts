import { useQuery } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type MemoryItem = components["schemas"]["ControllersProjectMemoryItemResponse"];

/**
 * The durable facts AO holds about a project.
 *
 * Note what this endpoint returns and what it does not: a summary, a type, a
 * scope, a state and a provenance chain — never the raw body, never a
 * transcript, never anything a model produced while thinking. That is a
 * property of the API rather than of this hook, which is the right place for
 * it: a renderer that had to remember to withhold something would eventually
 * forget.
 */
export function useProjectMemoryItems(projectId: string, repoPath?: string) {
	const query = useQuery({
		queryKey: ["project-memory-items", projectId, repoPath ?? ""],
		enabled: hasTrustedApiBaseUrl() && Boolean(projectId),
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects/{id}/memory/items", {
				params: { path: { id: projectId }, query: repoPath ? { repoPath } : {} },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
	});
	return {
		items: query.data?.items ?? [],
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}
