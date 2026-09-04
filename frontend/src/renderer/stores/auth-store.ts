import { create } from "zustand";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { aoBridge } from "../lib/bridge";
import type { components } from "../../api/schema";

type UserView = components["schemas"]["ControllersUserView"];
type AuthProviders = components["schemas"]["ControllersAuthProvidersResponse"];

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

// P4-A: how the current identity was established. Distinct from `status`,
// which says WHETHER one resolved. Sign-out reads this: a federated session
// may additionally be offered the provider's own logout.
export type AuthMethod = "trusted_local" | "password" | "oidc" | null;

// P4-B: the installation-wide permissions the BACKEND says this identity
// holds. The renderer never derives authority from `user.role` — a second
// authorization implementation in React is one that can disagree with the real
// one, and the one in the daemon is the one that decides. Hiding a control from
// this list is a convenience; the route behind it is enforced regardless.
export type Permission =
	| "project.create"
	| "provider.read"
	| "provider.manage"
	| "settings.read"
	| "settings.manage"
	| "users.read"
	| "users.manage"
	| "teams.read"
	| "teams.manage"
	| "audit.read";

type AuthState = {
	user: UserView | null;
	status: AuthStatus;
	error: string | null;
	// P4-A: how the resolved identity authenticated, and (federated only) the
	// provider behind it.
	authMethod: AuthMethod;
	issuer: string | null;
	// providers is null until loadProviders() resolves. It is the ONLY thing
	// the renderer knows about the installation's SSO configuration: a label
	// and a start path. Issuer, client id and every secret stay server-side.
	providers: AuthProviders | null;
	// permissions is what this identity may do installation-wide. Empty until
	// load() resolves, and empty on any failure — an app that renders nothing
	// it cannot prove is allowed is degraded; one that renders everything on a
	// failed lookup is lying about authority.
	permissions: Permission[];
	// ssoPending is true while a sign-in is waiting on the system browser.
	ssoPending: boolean;
	// Checkpoint 8P-E.8: null until checkSetup() resolves. Orthogonal to
	// `status` — "unauthenticated" alone doesn't distinguish "no account
	// exists yet, show Create your account" from "an account exists, show
	// Sign in"; setupRequired is what makes that call.
	setupRequired: boolean | null;
	load: () => Promise<void>;
	checkSetup: () => Promise<void>;
	loadProviders: () => Promise<void>;
	// startSso begins single sign-on. In the desktop app the main process
	// drives the system browser and installs the resulting session cookie;
	// in a plain browser the page navigates to the provider itself.
	startSso: () => Promise<void>;
	login: (usernameOrEmail: string, password: string) => Promise<boolean>;
	register: (displayName: string, email: string, password: string) => Promise<boolean>;
	logout: () => Promise<void>;
};

let pendingLoad: Promise<void> | undefined;

// signedOutState is the one definition of "nobody is signed in". Spelling it
// out at each call site is how a new field (permissions, say) ends up cleared
// in three places and left stale in a fourth.
const signedOutState = {
	user: null,
	status: "unauthenticated" as AuthStatus,
	authMethod: null,
	issuer: null,
	permissions: [] as Permission[],
};

export const useAuthStore = create<AuthState>((set, get) => ({
	user: null,
	status: "loading",
	error: null,
	authMethod: null,
	issuer: null,
	providers: null,
	permissions: [],
	ssoPending: false,
	setupRequired: null,
	load: async () => {
		if (pendingLoad) return pendingLoad;
		pendingLoad = (async () => {
			try {
				const { data, error, response } = await apiClient.GET("/api/v1/auth/me", { credentials: "include" });
				if (error) {
					if (response?.status === 401) {
						set({ ...signedOutState, error: null });
						return;
					}
					set({
						...signedOutState,
						error: apiErrorMessage(error, "Could not resolve current user"),
					});
					return;
				}
				if (!data) {
					set({ ...signedOutState, error: null });
					return;
				}
				set({
					user: data.user ?? null,
					status: data.status as AuthStatus,
					error: null,
					authMethod: (data.authMethod as AuthMethod) ?? null,
					issuer: data.issuer ?? null,
					permissions: (data.permissions ?? []) as Permission[],
				});
			} catch {
				// A missing/unreachable daemon must not crash the shell; the daemon
				// status banner already surfaces that separately.
				set({ ...signedOutState, error: null });
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
			set({ user: data.user, status: "authenticated", error: null, authMethod: "password", issuer: null });
			// The login response carries the identity, not the capabilities;
			// /auth/me is the one place that answers what this identity may do.
			void get().load();
			return true;
		} catch {
			set({ error: "Could not reach the daemon" });
			return false;
		}
	},
	loadProviders: async () => {
		try {
			const { data, error } = await apiClient.GET("/api/v1/auth/providers", { credentials: "include" });
			if (error || !data) return;
			set({ providers: data });
		} catch {
			// A provider list that cannot be fetched simply leaves the sign-in
			// screen offering passwords only, which always works.
		}
	},
	startSso: async () => {
		set({ error: null, ssoPending: true });
		try {
			// Desktop: the main process opens the system browser and installs
			// the session cookie itself, so nothing sensitive crosses into the
			// renderer. It resolves "cancelled" outside Electron.
			const outcome = await aoBridge.auth.ssoSignIn();
			if (outcome.status === "complete") {
				await get().load();
				return;
			}
		} catch (bridgeError) {
			set({ error: bridgeError instanceof Error ? bridgeError.message : "Single sign-on could not be completed." });
			set({ ssoPending: false });
			return;
		}

		// Plain browser: ask the daemon for the authorization URL and navigate
		// there. `returnTo` is validated server-side to a same-origin path, so
		// nothing here can turn into an open redirect.
		try {
			const returnTo = typeof window === "undefined" ? "/" : window.location.pathname + window.location.search;
			const { data, error } = await apiClient.POST("/api/v1/auth/oidc/start", {
				credentials: "include",
				body: { returnTo, clientKind: "browser" },
			});
			if (error || !data) {
				set({ error: apiErrorMessage(error, "Single sign-on could not be started."), ssoPending: false });
				return;
			}
			if (typeof window !== "undefined") {
				window.location.assign(data.authorizationUrl);
				return;
			}
			set({ ssoPending: false });
		} catch {
			set({ error: "Could not reach the daemon", ssoPending: false });
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
		let providerEndSessionUrl: string | undefined;
		try {
			const { data } = await apiClient.POST("/api/v1/auth/logout", { credentials: "include" });
			providerEndSessionUrl = data?.providerEndSessionUrl;
		} catch {
			// Best-effort: clear local state regardless of network outcome.
		}
		set({ ...signedOutState, error: null });
		void get().load();
		// The AO session is already gone. This only OFFERS the provider's own
		// sign-out; a provider that advertises none returns nothing here, and
		// AO never claims to have ended a session it cannot end.
		if (providerEndSessionUrl) {
			void aoBridge.app.openExternal(providerEndSessionUrl).catch(() => undefined);
		}
	},
}));

/**
 * useCan reports whether the signed-in identity holds a permission, according
 * to the backend. Use it to hide navigation and controls a person cannot use —
 * never to decide whether an action is safe. The daemon enforces every one of
 * these again, and a direct API call from a hidden button is refused exactly as
 * a visible one would be.
 */
export function useCan(permission: Permission): boolean {
	return useAuthStore((state) => state.permissions.includes(permission));
}

/** useCanAny is useCan for a group of related surfaces (an admin nav entry). */
export function useCanAny(...permissions: Permission[]): boolean {
	return useAuthStore((state) => permissions.some((p) => state.permissions.includes(p)));
}
