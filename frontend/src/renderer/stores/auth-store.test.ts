import { beforeEach, describe, expect, it, vi } from "vitest";

const apiGET = vi.fn();
const apiPOST = vi.fn();

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
		useAuthStore.setState({ user: null, status: "loading", error: null, setupRequired: null });
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
