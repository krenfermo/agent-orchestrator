import { beforeEach, describe, expect, it, vi } from "vitest";

const apiGET = vi.fn();
const apiPOST = vi.fn();

const ssoSignIn = vi.fn();
const openExternal = vi.fn();

vi.mock("../lib/bridge", () => ({
	aoBridge: {
		auth: { ssoSignIn: (...args: unknown[]) => ssoSignIn(...args) },
		app: { openExternal: (...args: unknown[]) => openExternal(...args) },
	},
}));

vi.mock("../lib/api-client", async () => {
	const actual = await vi.importActual<typeof import("../lib/api-client")>("../lib/api-client");
	return {
		...actual,
		apiClient: { GET: (...args: unknown[]) => apiGET(...args), POST: (...args: unknown[]) => apiPOST(...args) },
	};
});

import { setApiBaseUrl } from "../lib/api-client";
import { useAuthStore } from "./auth-store";

describe("auth-store", () => {
	beforeEach(() => {
		apiGET.mockReset();
		apiPOST.mockReset();
		ssoSignIn.mockReset();
		openExternal.mockReset();
		openExternal.mockResolvedValue(undefined);
		useAuthStore.setState({
			user: null,
			status: "loading",
			error: null,
			setupRequired: null,
			authMethod: null,
			issuer: null,
			providers: null,
			providersStatus: "idle",
			ssoError: null,
			permissions: [],
			ssoPending: false,
		});
		// The store's calls are gated on a trusted daemon URL, so the default for
		// these cases is "daemon is ready". The not-ready gate has its own block.
		setApiBaseUrl("http://127.0.0.1:3001");
	});

	// P4-A -------------------------------------------------------------------

	describe("loadProviders", () => {
		it("records the provider the backend advertises", async () => {
			apiGET.mockResolvedValue({
				data: { mode: "oidc", passwordEnabled: true, oidc: { displayName: "Okta", startPath: "/api/v1/auth/oidc/start" } },
			});
			await useAuthStore.getState().loadProviders();
			expect(useAuthStore.getState().providers?.oidc?.displayName).toBe("Okta");
		});

		it("leaves the sign-in screen password-only when the call fails", async () => {
			apiGET.mockRejectedValue(new Error("network down"));
			await useAuthStore.getState().loadProviders();
			expect(useAuthStore.getState().providers).toBeNull();
		});

		it("reports a reachable daemon's provider failure as an SSO error, not a daemon one", async () => {
			apiGET.mockResolvedValue({ error: { message: "provider discovery failed", code: "oidc_discovery" } });

			await useAuthStore.getState().loadProviders();

			expect(useAuthStore.getState().providersStatus).toBe("error");
			expect(useAuthStore.getState().ssoError).toContain("provider discovery failed");
			// Password sign-in is unaffected and shows nothing of this.
			expect(useAuthStore.getState().error).toBeNull();
		});

		it("marks providers loaded so the screen can tell 'no SSO' from 'not asked yet'", async () => {
			apiGET.mockResolvedValue({ data: { mode: "password", passwordEnabled: true } });

			await useAuthStore.getState().loadProviders();

			expect(useAuthStore.getState().providersStatus).toBe("loaded");
			expect(useAuthStore.getState().providers?.oidc).toBeUndefined();
		});

		it("de-duplicates concurrent loads into one request", async () => {
			apiGET.mockResolvedValue({ data: { mode: "oidc", passwordEnabled: true, oidc: { displayName: "Google", startPath: "/api/v1/auth/oidc/start" } } });

			await Promise.all([useAuthStore.getState().loadProviders(), useAuthStore.getState().loadProviders()]);

			expect(apiGET).toHaveBeenCalledTimes(1);
		});
	});

	// The cold-start race this fix exists for: Electron's renderer boots before
	// the daemon binds its port, api-client short-circuits every request into a
	// synthetic 503 carrying the daemon-startup text, and nothing used to run
	// again once the daemon came up.
	describe("daemon readiness", () => {
		beforeEach(() => {
			setApiBaseUrl(null);
		});

		it("does not ask anything while no daemon URL is trusted", async () => {
			await useAuthStore.getState().load();
			await useAuthStore.getState().checkSetup();
			await useAuthStore.getState().loadProviders();

			expect(apiGET).not.toHaveBeenCalled();
		});

		it("stays in loading rather than claiming the person is signed out", async () => {
			await useAuthStore.getState().load();

			// "unauthenticated" here is what rendered LoginScreen before the daemon
			// had answered anything at all.
			expect(useAuthStore.getState().status).toBe("loading");
			expect(useAuthStore.getState().error).toBeNull();
		});

		it("never puts the daemon-startup message where the login form shows errors", async () => {
			await useAuthStore.getState().load();

			expect(useAuthStore.getState().error).toBeNull();
		});

		it("leaves setupRequired undecided instead of guessing", async () => {
			await useAuthStore.getState().checkSetup();

			expect(useAuthStore.getState().setupRequired).toBeNull();
		});

		it("keeps providers idle, not errored, so the retry is expected", async () => {
			await useAuthStore.getState().loadProviders();

			expect(useAuthStore.getState().providersStatus).toBe("idle");
			expect(useAuthStore.getState().ssoError).toBeNull();
		});

		it("loads the identity, setup state and providers once the daemon is ready", async () => {
			apiGET.mockImplementation((path: string) => {
				if (path === "/api/v1/auth/providers") {
					return Promise.resolve({
						data: { mode: "oidc", passwordEnabled: true, oidc: { displayName: "Google", startPath: "/api/v1/auth/oidc/start" } },
					});
				}
				if (path === "/api/v1/auth/setup-status") return Promise.resolve({ data: { setupRequired: false } });
				return Promise.resolve({ error: { message: "unauthorized" }, response: { status: 401 } });
			});

			setApiBaseUrl("http://127.0.0.1:3002");
			await useAuthStore.getState().refreshForDaemonReady();

			// providers 200 + /me 401 is a login-ready daemon, not a starting one.
			expect(useAuthStore.getState().status).toBe("unauthenticated");
			expect(useAuthStore.getState().error).toBeNull();
			expect(useAuthStore.getState().setupRequired).toBe(false);
			expect(useAuthStore.getState().providers?.oidc?.displayName).toBe("Google");
			expect(useAuthStore.getState().providersStatus).toBe("loaded");
		});
	});

	describe("startSso", () => {
		it("reloads the identity once the desktop supervisor completes sign-in", async () => {
			ssoSignIn.mockResolvedValue({ status: "complete", user: { id: "u1", displayName: "Ada", email: "ada@example.com" } });
			apiGET.mockResolvedValue({
				data: {
					status: "authenticated",
					authMethod: "oidc",
					issuer: "https://idp.example",
					user: { id: "u1", displayName: "Ada", email: "ada@example.com", username: "ada", status: "active", role: "owner" },
				},
			});

			await useAuthStore.getState().startSso();

			expect(useAuthStore.getState().status).toBe("authenticated");
			expect(useAuthStore.getState().authMethod).toBe("oidc");
			expect(useAuthStore.getState().issuer).toBe("https://idp.example");
			// The renderer never asks the daemon to start a browser flow when
			// the supervisor already finished one.
			expect(apiPOST).not.toHaveBeenCalled();
		});

		it("surfaces the supervisor's failure without starting a second flow", async () => {
			ssoSignIn.mockRejectedValue(new Error("Single sign-on could not be completed."));

			await useAuthStore.getState().startSso();

			expect(useAuthStore.getState().ssoError).toBe("Single sign-on could not be completed.");
			// Never in `error`: that slot is the password form's.
			expect(useAuthStore.getState().error).toBeNull();
			expect(useAuthStore.getState().ssoPending).toBe(false);
			expect(apiPOST).not.toHaveBeenCalled();
		});

		it("falls back to a browser navigation when there is no supervisor", async () => {
			ssoSignIn.mockResolvedValue({ status: "cancelled" });
			apiPOST.mockResolvedValue({ data: { authorizationUrl: "https://idp.example/authorize?x=1", flowId: "f1" } });
			const assign = vi.fn();
			vi.stubGlobal("window", { location: { pathname: "/board", search: "", assign } });

			await useAuthStore.getState().startSso();

			expect(apiPOST).toHaveBeenCalledWith(
				"/api/v1/auth/oidc/start",
				expect.objectContaining({ body: { returnTo: "/board", clientKind: "browser" } }),
			);
			expect(assign).toHaveBeenCalledWith("https://idp.example/authorize?x=1");
			vi.unstubAllGlobals();
		});
	});

	describe("logout", () => {
		it("offers the provider's own sign-out when the backend returns one", async () => {
			apiPOST.mockResolvedValue({ data: { ok: true, providerEndSessionUrl: "https://idp.example/logout" } });
			apiGET.mockResolvedValue({ data: { status: "no_user" } });

			await useAuthStore.getState().logout();

			expect(openExternal).toHaveBeenCalledWith("https://idp.example/logout");
			expect(useAuthStore.getState().authMethod).toBeNull();
		});

		it("does not invent a provider sign-out when the backend offers none", async () => {
			apiPOST.mockResolvedValue({ data: { ok: true } });
			apiGET.mockResolvedValue({ data: { status: "no_user" } });

			await useAuthStore.getState().logout();

			expect(openExternal).not.toHaveBeenCalled();
		});
	});

	describe("checkSetup", () => {
		it("sets setupRequired true when the backend reports zero users", async () => {
			apiGET.mockResolvedValue({ data: { setupRequired: true } });
			await useAuthStore.getState().checkSetup();
			expect(useAuthStore.getState().setupRequired).toBe(true);
		});

		it("sets setupRequired false once an owner exists", async () => {
			apiGET.mockResolvedValue({ data: { setupRequired: false } });
			await useAuthStore.getState().checkSetup();
			expect(useAuthStore.getState().setupRequired).toBe(false);
		});

		it("defaults to false (never offers signup) when the daemon is unreachable", async () => {
			apiGET.mockRejectedValue(new Error("network down"));
			await useAuthStore.getState().checkSetup();
			expect(useAuthStore.getState().setupRequired).toBe(false);
		});
	});

	describe("register", () => {
		it("signs the new owner in on success", async () => {
			const user = { id: "u1", displayName: "Owner", email: "owner@example.com", username: "owner@example.com", status: "active", role: "owner" };
			apiPOST.mockResolvedValue({ data: { user } });

			const ok = await useAuthStore.getState().register("Owner", "owner@example.com", "supersecret1");

			expect(ok).toBe(true);
			expect(useAuthStore.getState()).toMatchObject({ user, status: "authenticated", setupRequired: false, error: null });
		});

		it("surfaces a conflict error and does not authenticate on a second registration", async () => {
			apiPOST.mockResolvedValue({
				error: { code: "SETUP_ALREADY_COMPLETED", message: "this installation already has an owner account" },
			});

			const ok = await useAuthStore.getState().register("Someone Else", "someone@example.com", "supersecret2");

			expect(ok).toBe(false);
			expect(useAuthStore.getState().status).not.toBe("authenticated");
			expect(useAuthStore.getState().error).toBeTruthy();
		});
	});

	// P4-B -------------------------------------------------------------------

	describe("capabilities", () => {
		// The renderer takes the daemon's word for what this identity may do.
		// Deriving it from `user.role` in React would be a second authorization
		// implementation, and the one that decides lives in the daemon.
		it("stores the permissions /auth/me reports", async () => {
			apiGET.mockResolvedValue({
				data: {
					status: "authenticated",
					user: { id: "u1", displayName: "Ada", email: "ada@example.com", username: "ada", status: "active", role: "admin" },
					authMethod: "password",
					permissions: ["users.read", "users.manage", "settings.read"],
				},
			});

			await useAuthStore.getState().load();

			expect(useAuthStore.getState().permissions).toEqual(["users.read", "users.manage", "settings.read"]);
		});

		// A capability list that cannot be fetched must render an app with no
		// administration surfaces, never one with all of them.
		it("reports no capabilities when the identity cannot be resolved", async () => {
			useAuthStore.setState({ permissions: ["users.manage"] });
			apiGET.mockResolvedValue({
				error: { error: { code: "NOT_AUTHENTICATED", message: "authentication required" } },
				response: { status: 401 },
			});

			await useAuthStore.getState().load();

			expect(useAuthStore.getState().permissions).toEqual([]);
		});

		it("clears capabilities on sign-out", async () => {
			useAuthStore.setState({ permissions: ["users.manage"] });
			apiPOST.mockResolvedValue({ data: { ok: true } });
			apiGET.mockResolvedValue({ data: { status: "no_user", permissions: [] } });

			await useAuthStore.getState().logout();

			expect(useAuthStore.getState().permissions).toEqual([]);
		});
	});
});
