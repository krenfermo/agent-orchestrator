import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import { useCurrentTenantID } from "../stores/tenant-store";

export type ProjectSummary = components["schemas"]["ProjectSummary"];

export const projectsListQueryKey = ["projects-list"] as const;

/**
 * The cache key for one organization's project list.
 *
 * The organization id is PART of the key rather than a filter applied to a
 * shared cache entry, which is what makes switching organizations show the
 * right list immediately instead of the previous organization's projects until
 * a refetch lands. React Query cannot serve one key's data for another key, so
 * "no stale cross-organization data" is a property of the key rather than a
 * discipline about invalidation.
 */
export function projectsListKeyFor(tenantId: string | null) {
	return tenantId ? ([...projectsListQueryKey, tenantId] as const) : projectsListQueryKey;
}

async function fetchProjects(): Promise<ProjectSummary[]> {
	const { data, error } = await apiClient.GET("/api/v1/projects");
	if (error) throw new Error(apiErrorMessage(error));
	return data.projects;
}

/**
 * Registered-project list shared by the Workflows "New workflow" picker and
 * Settings -> Projects. A single query key keeps both surfaces in sync: a
 * project registered or cloned from Settings appears in the Workflows
 * dropdown without a manual refresh.
 *
 * P4-C: the daemon has ALREADY removed every project the caller cannot reach,
 * in any organization, before this ever sees the response -- the filter below
 * narrows a list the caller is entitled to, from "every organization I belong
 * to" down to "the one I am looking at". It is a view concern, and it does
 * nothing at all on an installation with a single organization.
 */
export function useProjectsList() {
	const tenantId = useCurrentTenantID();
	const query = useQuery({
		queryKey: projectsListKeyFor(tenantId),
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchProjects,
	});

	const projects = query.data ?? [];
	return {
		projects: tenantId ? projects.filter((p) => p.tenantId === tenantId) : projects,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}

/**
 * Imperative invalidation for mutations defined outside this hook
 * (registration, clone). It invalidates every organization's list, not just
 * the one on screen: a project can be registered into an organization other
 * than the one currently being viewed, and leaving that list stale would hide
 * the project the person just created.
 */
export function useInvalidateProjectsList() {
	const queryClient = useQueryClient();
	return () => queryClient.invalidateQueries({ queryKey: projectsListQueryKey });
}
