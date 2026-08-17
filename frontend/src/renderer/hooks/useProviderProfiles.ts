import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type ProviderDescriptor = components["schemas"]["ControllersProviderDescriptorView"];
export type ProviderProfile = components["schemas"]["ControllersProviderProfileView"];

export const providerRegistryQueryKey = ["provider-registry"] as const;
export const providerProfilesQueryKey = ["provider-profiles"] as const;

async function fetchRegistry(): Promise<ProviderDescriptor[]> {
	const { data, error } = await apiClient.GET("/api/v1/providers/registry");
	if (error) throw new Error(apiErrorMessage(error));
	return data.providers;
}

async function fetchProfiles(): Promise<ProviderProfile[]> {
	const { data, error } = await apiClient.GET("/api/v1/provider-profiles");
	if (error) throw new Error(apiErrorMessage(error));
	return data.profiles;
}

/**
 * Checkpoint 8P-B: Settings → Agents & Models drives its cards from the
 * provider registry (what AO's code can support) joined against the current
 * user's own provider profiles (what THIS user has actually connected) —
 * never a hardcoded Claude/Codex pair. A provider with no matching profile
 * yet renders as "not connected", not omitted.
 */
export function useProviderProfiles() {
	const queryClient = useQueryClient();

	const registry = useQuery({
		queryKey: providerRegistryQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchRegistry,
		staleTime: 5 * 60 * 1000,
	});
	const profiles = useQuery({
		queryKey: providerProfilesQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchProfiles,
		staleTime: 30 * 1000,
	});

	const invalidateProfiles = () => void queryClient.invalidateQueries({ queryKey: providerProfilesQueryKey });

	const create = useMutation({
		mutationFn: async (input: { provider: string; harness: string; displayName: string }) => {
			const { data, error } = await apiClient.POST("/api/v1/provider-profiles", { body: input });
			if (error) throw new Error(apiErrorMessage(error));
			return data.profile;
		},
		onSuccess: invalidateProfiles,
	});

	const connect = useMutation({
		mutationFn: async (id: string) => {
			const { data, error } = await apiClient.POST("/api/v1/provider-profiles/{id}/connect", { params: { path: { id } } });
			if (error) throw new Error(apiErrorMessage(error));
			return data.profile;
		},
		onSuccess: invalidateProfiles,
	});

	const disconnect = useMutation({
		mutationFn: async (id: string) => {
			const { data, error } = await apiClient.POST("/api/v1/provider-profiles/{id}/disconnect", { params: { path: { id } } });
			if (error) throw new Error(apiErrorMessage(error));
			return data.profile;
		},
		onSuccess: invalidateProfiles,
	});

	const test = useMutation({
		mutationFn: async (id: string) => {
			const { data, error } = await apiClient.POST("/api/v1/provider-profiles/{id}/test", { params: { path: { id } } });
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
		onSuccess: invalidateProfiles,
	});

	const setEnabled = useMutation({
		mutationFn: async (input: { id: string; profile: ProviderProfile; enabled: boolean }) => {
			const { data, error } = await apiClient.PATCH("/api/v1/provider-profiles/{id}", {
				params: { path: { id: input.id } },
				body: { displayName: input.profile.displayName, enabled: input.enabled, defaultModel: input.profile.defaultModel },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data.profile;
		},
		onSuccess: invalidateProfiles,
	});

	return {
		registry: registry.data,
		profiles: profiles.data,
		isLoading: registry.isLoading || profiles.isLoading,
		error: registry.error ? apiErrorMessage(registry.error) : profiles.error ? apiErrorMessage(profiles.error) : undefined,
		createProfile: create.mutateAsync,
		connect: connect.mutateAsync,
		disconnect: disconnect.mutateAsync,
		test: test.mutateAsync,
		setEnabled: setEnabled.mutateAsync,
	};
}
