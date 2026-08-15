import { useMutation } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient } from "../lib/api-client";
import { describeProjectApiError } from "../lib/project-error-messages";
import { useInvalidateProjectsList } from "./useProjectsList";

export type BrowseEntry = components["schemas"]["ProjectBrowseEntry"];
export type BrowseResult = components["schemas"]["ProjectBrowseResult"];

async function browseAllowedRoot(path: string): Promise<BrowseResult> {
	const { data, error } = await apiClient.GET("/api/v1/projects/browse", {
		params: { query: path ? { path } : {} },
	});
	if (error) throw error;
	return data;
}

/**
 * Mutations for the web "register existing repository" / "clone from GitHub"
 * flows in Settings → Projects. Both write through the server-side
 * allowedProjectRoots confinement; the frontend never sends an arbitrary
 * filesystem path outside what the user picked from a browse() listing.
 */
export function useProjectRegistration() {
	const invalidateProjects = useInvalidateProjectsList();

	const register = useMutation({
		mutationFn: async (input: { path: string; name?: string }) => {
			const { data, error } = await apiClient.POST("/api/v1/projects", {
				body: { path: input.path, name: input.name },
			});
			if (error) throw error;
			return data.project;
		},
		onSuccess: () => {
			void invalidateProjects();
		},
	});

	const clone = useMutation({
		mutationFn: async (input: { repo: string; destinationName?: string }) => {
			const { data, error } = await apiClient.POST("/api/v1/projects/clone", {
				body: { repo: input.repo, destinationName: input.destinationName },
			});
			if (error) throw error;
			return data.project;
		},
		onSuccess: () => {
			void invalidateProjects();
		},
	});

	return {
		browse: browseAllowedRoot,
		register: register.mutateAsync,
		registering: register.isPending,
		registerError: register.error ? describeProjectApiError(register.error) : undefined,
		resetRegisterError: register.reset,
		clone: clone.mutateAsync,
		cloning: clone.isPending,
		cloneError: clone.error ? describeProjectApiError(clone.error) : undefined,
		resetCloneError: clone.reset,
	};
}
