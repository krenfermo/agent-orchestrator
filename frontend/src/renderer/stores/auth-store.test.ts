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
			ssoPending: false,
		});
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

			expect(useAuthStore.getState().error).toBe("Single sign-on could not be completed.");
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
});
