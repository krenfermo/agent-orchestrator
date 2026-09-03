import { useQuery } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type ProjectUsage = components["schemas"]["ControllersProjectUsageResponse"];
export type ProjectUsagePeriod = "today" | "7d" | "30d" | "all";

export function projectUsageQueryKey(projectId: string, period: ProjectUsagePeriod) {
	return ["project-usage", projectId, period] as const;
}

/**
 * Reads a project's token/cost rollup for one period.
 *
 * The daemon owns every number: the renderer never sums runs to reach a project
 * total, because a session can serve several roles and only the backend knows
 * not to count it twice.
 *
 * It refetches on a slow cadence rather than the Board's two seconds. A spend
 * figure that lags by a minute is fine; a project-wide aggregate polled every
 * two seconds is not, and the query is deliberately cheap ONLY because it is
 * infrequent.
 */
const USAGE_REFETCH_MS = 60_000;

export function useProjectUsage(projectId: string | undefined, period: ProjectUsagePeriod = "7d") {
	const query = useQuery({
		queryKey: projectUsageQueryKey(projectId ?? "", period),
		enabled: Boolean(projectId) && hasTrustedApiBaseUrl(),
		refetchInterval: USAGE_REFETCH_MS,
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects/{projectId}/usage", {
				params: { path: { projectId: projectId as string }, query: { range: period } },
			});
			if (error) throw error;
			return data;
		},
	});
	return {
		usage: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}
