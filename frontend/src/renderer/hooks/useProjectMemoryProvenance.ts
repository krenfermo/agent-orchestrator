import { useQuery } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type MemoryProvenance = components["schemas"]["ControllersProjectMemoryProvenanceResponse"];

/**
 * The evidence behind one durable fact, fetched only when somebody asks to see it.
 *
 * The list read (useProjectMemoryItems) deliberately ships no bodies: two
 * hundred facts with their bodies would be two orders of magnitude more
 * payload to render text nothing is showing. This is the other half of that
 * decision — the body, the full source-path list, and the relations that say
 * how the fact became the project's knowledge, for the one row a person
 * expanded.
 *
 * It is what makes an inference checkable rather than merely labelled. A fact
 * AO derived from directory naming carries its own "how this was determined"
 * note in that body, and a reader who cannot see it has only AO's word.
 */
export function useProjectMemoryProvenance(projectId: string, itemId: string | undefined) {
	const query = useQuery({
		queryKey: ["project-memory-provenance", projectId, itemId ?? ""],
		enabled: hasTrustedApiBaseUrl() && Boolean(projectId) && Boolean(itemId),
		queryFn: async () => {
			// The project id travels in the path because the route is
			// project-scoped and guarded on it: a fact is only readable by
			// somebody who may read that project's memory, and dropping the
			// segment would be asking the server to look one up for us.
			const { data, error } = await apiClient.GET("/api/v1/projects/{id}/memory/provenance/{itemId}", {
				params: { path: { id: projectId, itemId: itemId as string } },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
	});
	return {
		provenance: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}
