import { beforeEach, describe, expect, it, vi } from "vitest";

const apiGET = vi.fn();

vi.mock("../lib/api-client", async () => {
	const actual = await vi.importActual<typeof import("../lib/api-client")>("../lib/api-client");
	return {
		...actual,
		apiClient: { GET: (...args: unknown[]) => apiGET(...args) },
	};
});

import { setApiBaseUrl } from "../lib/api-client";
import { useTenantStore } from "./tenant-store";

const acme = { id: "tnt-a", name: "Acme", slug: "acme", description: "", status: "active", role: "member" };
const umbrella = { id: "tnt-b", name: "Umbrella", slug: "umbrella", description: "", status: "active", role: "admin" };

function answerWith(tenants: unknown[]) {
	apiGET.mockResolvedValue({ data: { tenants } });
}

describe("tenant-store", () => {
	beforeEach(() => {
		apiGET.mockReset();
		localStorage.clear();
		useTenantStore.getState().reset();
		setApiBaseUrl("http://127.0.0.1:4000");
	});

	it("selects the only organization without asking anybody to choose", async () => {
		answerWith([acme]);
		await useTenantStore.getState().load();

		const state = useTenantStore.getState();
		expect(state.status).toBe("loaded");
		expect(state.tenants).toHaveLength(1);
		// currentTenantId is set, but every consumer reads it through
		// useCurrentTenantID, which reports null while there is nothing to
		// choose between -- so nothing filters and no selector renders.
		expect(state.currentTenantId).toBe("tnt-a");
	});

	it("remembers the chosen organization across a restart", async () => {
		answerWith([acme, umbrella]);
		await useTenantStore.getState().load();
		useTenantStore.getState().setCurrentTenant("tnt-b");
		expect(localStorage.getItem("ao.currentTenantId")).toBe("tnt-b");

		// A restart: nothing in memory, the choice still on disk. Modelled by
		// clearing the store WITHOUT clearing storage, which is what a fresh
		// renderer process looks like -- reset() is sign-out, and deliberately
		// forgets both.
		useTenantStore.setState({ tenants: [], currentTenantId: null, status: "idle", error: null });
		answerWith([acme, umbrella]);
		await useTenantStore.getState().load();

		expect(useTenantStore.getState().currentTenantId).toBe("tnt-b");
	});

	it("forgets the choice on sign-out, not just the list", async () => {
		answerWith([acme, umbrella]);
		await useTenantStore.getState().load();
		useTenantStore.getState().setCurrentTenant("tnt-b");

		useTenantStore.getState().reset();

		expect(localStorage.getItem("ao.currentTenantId")).toBeNull();
	});

	it("drops a remembered organization the account no longer belongs to", async () => {
		answerWith([acme, umbrella]);
		await useTenantStore.getState().load();
		useTenantStore.getState().setCurrentTenant("tnt-b");

		// Access to Umbrella is revoked; the daemon stops listing it.
		useTenantStore.getState().reset();
		answerWith([acme]);
		await useTenantStore.getState().load();

		// Keeping the stale choice would filter every list down to nothing and
		// look exactly like an empty installation.
		expect(useTenantStore.getState().currentTenantId).toBe("tnt-a");
	});

	it("refuses to select an organization the account does not belong to", async () => {
		answerWith([acme]);
		await useTenantStore.getState().load();

		useTenantStore.getState().setCurrentTenant("tnt-somebody-elses");

		expect(useTenantStore.getState().currentTenantId).toBe("tnt-a");
	});

	it("reports a failed load rather than an empty organization list", async () => {
		apiGET.mockResolvedValue({ error: { message: "daemon is down" } });
		await useTenantStore.getState().load();

		const state = useTenantStore.getState();
		expect(state.status).toBe("error");
		expect(state.error).toBeTruthy();
		// An empty list would be indistinguishable from "you belong to
		// nothing", which is a very different thing to tell somebody.
		expect(state.tenants).toEqual([]);
	});

	it("de-duplicates concurrent loads", async () => {
		answerWith([acme]);
		await Promise.all([useTenantStore.getState().load(), useTenantStore.getState().load()]);
		expect(apiGET).toHaveBeenCalledTimes(1);
	});

	it("forgets everything on reset, so a sign-out leaves nothing behind", async () => {
		answerWith([acme, umbrella]);
		await useTenantStore.getState().load();
		useTenantStore.getState().reset();

		const state = useTenantStore.getState();
		expect(state.tenants).toEqual([]);
		expect(state.currentTenantId).toBeNull();
		expect(state.status).toBe("idle");
	});
});
