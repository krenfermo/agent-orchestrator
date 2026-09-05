import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

// useProjectIntelligence.ts -- the data layer for Project → Intelligence.
//
// Every read here is a plain query against an endpoint the daemon already
// authorizes per project, so there is nothing to decide in the renderer: a
// project the caller cannot reach answers 404 and the tab renders that, the
// same as a project that does not exist. The renderer never filters for
// access, because a second opinion about access is one that can disagree with
// the first.

export type IntelligenceOverview = components["schemas"]["ProjectIntelligenceOverview"];
export type IntelligenceRepoStatus = components["schemas"]["ProjectIntelligenceRepoStatus"];
export type IntelligenceArchitecture = components["schemas"]["ProjectIntelligenceArchitecture"];
export type IntelligenceSubgraph = components["schemas"]["ProjectIntelligenceSubgraph"];
export type IntelligenceSubgraphNode = components["schemas"]["ProjectIntelligenceSubgraphNode"];
export type IntelligenceSubgraphEdge = components["schemas"]["ProjectIntelligenceSubgraphEdge"];
export type IntelligenceSearchResult = components["schemas"]["ProjectIntelligenceSearchResult"];
export type IntelligenceSearchHit = components["schemas"]["ProjectIntelligenceSearchHit"];
export type IntelligenceContextPreview = components["schemas"]["ProjectIntelligenceContextPreview"];

/** The derived lifecycle a repository's intelligence is in. */
export type IntelligenceState = "pending" | "indexing" | "ready" | "stale" | "failed";

export type ContextRole = "planner" | "worker" | "reviewer" | "repair";

export const intelligenceQueryKey = (projectId: string) => ["project-intelligence", projectId] as const;

function unwrap<T>(result: { data?: T; error?: unknown }): T {
	if (result.error) throw new Error(apiErrorMessage(result.error));
	return result.data as T;
}

/**
 * The Overview tab's data. Polled while anything is indexing, because an index
 * finishing is the one state change nobody triggers from this screen — the
 * reconciler does it — and a status page that needs a manual refresh to notice
 * is a status page people stop trusting.
 */
export function useIntelligenceOverview(projectId: string) {
	const query = useQuery({
		queryKey: intelligenceQueryKey(projectId),
		enabled: hasTrustedApiBaseUrl() && Boolean(projectId),
		queryFn: async () =>
			unwrap(
				await apiClient.GET("/api/v1/projects/{id}/intelligence", {
					params: { path: { id: projectId } },
				}),
			),
		refetchInterval: (query) => {
			const repos = query.state.data?.repos ?? [];
			return repos.some((repo) => repo.state === "indexing") ? 2000 : false;
		},
	});
	return {
		overview: query.data,
		repos: query.data?.repos ?? [],
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		refetch: query.refetch,
	};
}

export function useIntelligenceArchitecture(projectId: string, repoPath?: string) {
	const query = useQuery({
		queryKey: [...intelligenceQueryKey(projectId), "architecture", repoPath ?? ""],
		enabled: hasTrustedApiBaseUrl() && Boolean(projectId),
		queryFn: async () =>
			unwrap(
				await apiClient.GET("/api/v1/projects/{id}/intelligence/architecture", {
					params: { path: { id: projectId }, query: repoPath ? { repoPath } : {} },
				}),
			),
	});
	return {
		architecture: query.data?.architecture as Record<string, unknown> | undefined,
		rendered: query.data?.rendered,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}

export type SubgraphParams = {
	symbol?: string;
	path?: string;
	depth?: number;
	nodeKinds?: string;
	edgeKinds?: string;
	repoPath?: string;
};

/**
 * A bounded neighbourhood. Disabled until there is a seed: the endpoint refuses
 * a seedless walk, and asking anyway would just render an error where an empty
 * state belongs.
 */
export function useIntelligenceSubgraph(projectId: string, params: SubgraphParams) {
	const seeded = Boolean(params.symbol || params.path);
	const query = useQuery({
		queryKey: [...intelligenceQueryKey(projectId), "graph", params],
		enabled: hasTrustedApiBaseUrl() && Boolean(projectId) && seeded,
		queryFn: async () =>
			unwrap(
				await apiClient.GET("/api/v1/projects/{id}/intelligence/graph", {
					params: {
						path: { id: projectId },
						query: {
							symbol: params.symbol || undefined,
							path: params.path || undefined,
							depth: params.depth,
							nodeKinds: params.nodeKinds || undefined,
							edgeKinds: params.edgeKinds || undefined,
							repoPath: params.repoPath || undefined,
						},
					},
				}),
			),
	});
	return {
		subgraph: query.data,
		seeded,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}

export function useIntelligenceSearch(projectId: string, term: string) {
	const query = useQuery({
		queryKey: [...intelligenceQueryKey(projectId), "search", term],
		enabled: hasTrustedApiBaseUrl() && Boolean(projectId) && term.trim().length > 0,
		queryFn: async () =>
			unwrap(
				await apiClient.GET("/api/v1/projects/{id}/intelligence/search", {
					params: { path: { id: projectId }, query: { q: term } },
				}),
			),
	});
	return {
		result: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}

export function useIntelligenceContext(projectId: string, role: ContextRole) {
	const query = useQuery({
		queryKey: [...intelligenceQueryKey(projectId), "context", role],
		enabled: hasTrustedApiBaseUrl() && Boolean(projectId),
		queryFn: async () =>
			unwrap(
				await apiClient.GET("/api/v1/projects/{id}/intelligence/context", {
					params: { path: { id: projectId }, query: { role } },
				}),
			),
	});
	return {
		preview: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}

/**
 * The two write actions. Both invalidate the whole intelligence subtree,
 * because a sync changes the architecture, the graph and the context pack as
 * well as the counts on the Overview.
 */
export function useIntelligenceSync(projectId: string) {
	const queryClient = useQueryClient();
	const invalidate = () =>
		queryClient.invalidateQueries({ queryKey: intelligenceQueryKey(projectId) });

	const sync = useMutation({
		mutationFn: async (repoPath?: string) =>
			unwrap(
				await apiClient.POST("/api/v1/projects/{id}/intelligence/sync", {
					params: { path: { id: projectId }, query: repoPath ? { repoPath } : {} },
				}),
			),
		onSuccess: invalidate,
	});

	const rebuild = useMutation({
		mutationFn: async (repoPath?: string) =>
			unwrap(
				await apiClient.POST("/api/v1/projects/{id}/intelligence/rebuild", {
					params: { path: { id: projectId }, query: repoPath ? { repoPath } : {} },
				}),
			),
		onSuccess: invalidate,
	});

	return {
		sync: (repoPath?: string) => sync.mutateAsync(repoPath),
		rebuild: (repoPath?: string) => rebuild.mutateAsync(repoPath),
		isSyncing: sync.isPending,
		isRebuilding: rebuild.isPending,
		error: sync.error
			? apiErrorMessage(sync.error)
			: rebuild.error
				? apiErrorMessage(rebuild.error)
				: undefined,
	};
}
