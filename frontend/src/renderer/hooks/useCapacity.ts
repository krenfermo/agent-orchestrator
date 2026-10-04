import { useQuery } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type CapacitySnapshot = components["schemas"]["ControllersCapacitySnapshotResponse"];

export const capacityQueryKey = ["capacity"] as const;

async function fetchCapacity(): Promise<CapacitySnapshot[]> {
	const { data, error } = await apiClient.GET("/api/v1/capacity");
	if (error) throw new Error(apiErrorMessage(error));
	return data.capacity;
}

/**
 * Checkpoint 8J's read-only capacity view (Settings → Development Agents).
 * Every field is derived from a real Checkpoint 8H agent_health_events row;
 * a harness with no recorded event reports state "unknown", not a guess.
 */
export function useCapacity() {
	const query = useQuery({
		queryKey: capacityQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchCapacity,
		staleTime: 30 * 1000,
	});
	return {
		capacity: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}
