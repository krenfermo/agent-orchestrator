import { create } from "zustand";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import type { components } from "../../api/schema";

type UserView = components["schemas"]["ControllersUserView"];

// Checkpoint 8P-A: application identity. Mirrors locale-store.ts's shape
// (state + async actions, a de-duplicated in-flight load()) applied to the
// current-user resolution instead of the locale setting.
//
// Status values:
//   "loading"        — load() has not resolved yet.
//   "trusted-local"   — AO_TRUSTED_LOCAL_MODE is on; identity was
//                       synthesized to the bootstrap admin with no login
//                       screen involved (today's zero-friction desktop UX).
//   "no_user"        — trusted-local mode, but no bootstrap admin exists
//                       yet (fresh install with no AO_BOOTSTRAP_ADMIN_*
//                       env set) — a state the UI renders sensibly instead
//                       of crashing on a null user.
//   "authenticated"  — a real session cookie resolved to a user.
//   "unauthenticated" — multi-user mode with no valid session; the login
//                       screen renders in place of the shell.
export type AuthStatus = "loading" | "trusted-local" | "no_user" | "authenticated" | "unauthenticated";

type AuthState = {
	user: UserView | null;
	status: AuthStatus;
	error: string | null;
	// Checkpoint 8P-E.8: null until checkSetup() resolves. Orthogonal to
	// `status` — "unauthenticated" alone doesn't distinguish "no account
	// exists yet, show Create your account" from "an account exists, show
	// Sign in"; setupRequired is what makes that call.
	setupRequired: boolean | null;
	load: () => Promise<void>;
	checkSetup: () => Promise<void>;
	login: (usernameOrEmail: string, password: string) => Promise<boolean>;
	register: (displayName: string, email: string, password: string) => Promise<boolean>;
	logout: () => Promise<void>;
};

let pendingLoad: Promise<void> | undefined;

export const useAuthStore = create<AuthState>((set, get) => ({
	user: null,
	status: "loading",
	error: null,
	setupRequired: null,
	load: async () => {
		if (pendingLoad) return pendingLoad;
		pendingLoad = (async () => {
			try {
				const { data, error, response } = await apiClient.GET("/api/v1/auth/me", { credentials: "include" });
				if (error) {
					if (response?.status === 401) {
						set({ user: null, status: "unauthenticated", error: null });
						return;
					}
					set({ user: null, status: "unauthenticated", error: apiErrorMessage(error, "Could not resolve current user") });
					return;
				}
				if (!data) {
					set({ user: null, status: "unauthenticated", error: null });
					return;
				}
				set({ user: data.user ?? null, status: data.status as AuthStatus, error: null });
			} catch {
				// A missing/unreachable daemon must not crash the shell; the daemon
				// status banner already surfaces that separately.
				set({ user: null, status: "unauthenticated", error: null });
			}
		})();
		try {
			await pendingLoad;
		} finally {
			pendingLoad = undefined;
		}
	},
	checkSetup: async () => {
		try {
			const { data, error } = await apiClient.GET("/api/v1/auth/setup-status", { credentials: "include" });
			if (error || !data) {
				set({ setupRequired: false });
				return;
			}
			set({ setupRequired: data.setupRequired });
		} catch {
			set({ setupRequired: false });
		}
	},
	login: async (usernameOrEmail, password) => {
		set({ error: null });
		try {
			const { data, error } = await apiClient.POST("/api/v1/auth/login", {
				credentials: "include",
				body: { usernameOrEmail, password },
			});
			if (error || !data) {
				set({ error: apiErrorMessage(error, "Invalid username/email or password") });
				return false;
			}
			set({ user: data.user, status: "authenticated", error: null });
			return true;
		} catch {
			set({ error: "Could not reach the daemon" });
			return false;
		}
	},
	register: async (displayName, email, password) => {
		set({ error: null });
		try {
			const { data, error } = await apiClient.POST("/api/v1/auth/register", {
				credentials: "include",
				body: { displayName, email, password },
			});
			if (error || !data) {
				set({ error: apiErrorMessage(error, "Could not create the account") });
				return false;
			}
			set({ user: data.user, status: "authenticated", setupRequired: false, error: null });
			return true;
		} catch {
			set({ error: "Could not reach the daemon" });
			return false;
		}
	},
	logout: async () => {
		try {
			await apiClient.POST("/api/v1/auth/logout", { credentials: "include" });
		} catch {
			// Best-effort: clear local state regardless of network outcome.
		}
		set({ user: null, status: "unauthenticated", error: null });
		void get().load();
	},
}));
