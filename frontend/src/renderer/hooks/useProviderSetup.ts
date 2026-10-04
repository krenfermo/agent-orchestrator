// Checkpoint 8P-E.8.4: zero-terminal provider onboarding. Starting a setup
// session opens a server-owned PTY (see backend/internal/service/providersetup)
// running the provider CLI's own login flow inside the profile owner's
// isolated runtime-home, and returns a handleId the caller attaches to over
// the existing terminal WebSocket mux (the same one shell terminals use).
// This hook never learns the launch env/argv -- the daemon decides both --
// it only starts/stops the session and polls the existing Test Connection
// endpoint to notice when login succeeds, so the user is never required to
// click Test Connection themselves.

import { useCallback, useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { providerProfilesQueryKey } from "./useProviderProfiles";

export type ProviderSetupPhase = "idle" | "starting" | "waiting" | "timed_out" | "error";

/** How often to re-probe auth state while a setup terminal is open. */
const POLL_INTERVAL_MS = 3000;
/** How long to keep polling before giving up and asking the user to retry. */
const TIMEOUT_MS = 10 * 60 * 1000;

export type ProviderSetupState = {
	phase: ProviderSetupPhase;
	handleId: string | undefined;
	instructions: string | undefined;
	error: string | undefined;
	/** Starts a fresh setup terminal, replacing any live one for this profile. */
	start: () => Promise<void>;
	/** Stops the live setup terminal, if any, and resets to idle. */
	stop: () => Promise<void>;
};

/**
 * Drives one provider profile's guided setup session: start opens the
 * terminal and begins polling; the poll auto-stops (and closes the terminal)
 * the instant Test Connection reports authenticated, invalidating the
 * profiles query so the card flips to Ready without another click.
 */
export function useProviderSetup(profileId: string | undefined): ProviderSetupState {
	const queryClient = useQueryClient();
	const [phase, setPhase] = useState<ProviderSetupPhase>("idle");
	const [handleId, setHandleId] = useState<string | undefined>(undefined);
	const [instructions, setInstructions] = useState<string | undefined>(undefined);
	const [error, setError] = useState<string | undefined>(undefined);
	const pollRef = useRef<ReturnType<typeof setInterval> | undefined>(undefined);
	const timeoutRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

	const clearTimers = useCallback(() => {
		if (pollRef.current !== undefined) clearInterval(pollRef.current);
		if (timeoutRef.current !== undefined) clearTimeout(timeoutRef.current);
		pollRef.current = undefined;
		timeoutRef.current = undefined;
	}, []);

	const startMutation = useMutation({
		mutationFn: async (id: string) => {
			const { data, error } = await apiClient.POST("/api/v1/provider-profiles/{id}/setup", { params: { path: { id } } });
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
	});

	const stopMutation = useMutation({
		mutationFn: async (id: string) => {
			const { error } = await apiClient.DELETE("/api/v1/provider-profiles/{id}/setup", { params: { path: { id } } });
			if (error) throw new Error(apiErrorMessage(error));
		},
	});

	const stop = useCallback(async () => {
		clearTimers();
		setPhase("idle");
		setHandleId(undefined);
		setInstructions(undefined);
		if (profileId) {
			// Best-effort: the terminal may already be gone (user exited the
			// CLI by hand, or a previous stop already ran) -- that is not a
			// failure the caller needs to see.
			await stopMutation.mutateAsync(profileId).catch(() => undefined);
		}
	}, [profileId, clearTimers, stopMutation]);

	const start = useCallback(async () => {
		if (!profileId) return;
		clearTimers();
		setError(undefined);
		setPhase("starting");
		try {
			const data = await startMutation.mutateAsync(profileId);
			setHandleId(data?.handleId);
			setInstructions(data?.instructions);
			setPhase("waiting");

			pollRef.current = setInterval(() => {
				void (async () => {
					const { data: testData, error: testError } = await apiClient.POST("/api/v1/provider-profiles/{id}/test", {
						params: { path: { id: profileId } },
					});
					// A transient poll failure is not evidence of anything --
					// keep waiting and let the next tick retry.
					if (testError || !testData?.ok) return;
					clearTimers();
					setPhase("idle");
					setHandleId(undefined);
					setInstructions(undefined);
					void queryClient.invalidateQueries({ queryKey: providerProfilesQueryKey });
					await stopMutation.mutateAsync(profileId).catch(() => undefined);
				})();
			}, POLL_INTERVAL_MS);

			timeoutRef.current = setTimeout(() => {
				clearTimers();
				setPhase("timed_out");
			}, TIMEOUT_MS);
		} catch (e) {
			setPhase("error");
			setError(e instanceof Error ? e.message : String(e));
		}
	}, [profileId, clearTimers, startMutation, stopMutation, queryClient]);

	// Unmount (dialog closed, navigated away) must not leave a poll/timeout
	// running against a component that no longer exists.
	useEffect(() => clearTimers, [clearTimers]);

	return { phase, handleId, instructions, error, start, stop };
}
