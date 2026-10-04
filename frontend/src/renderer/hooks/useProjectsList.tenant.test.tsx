import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
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
import { useTenantStore } from "../stores/tenant-store";
import { useProjectsList } from "./useProjectsList";

const acme = { id: "tnt-a", name: "Acme", slug: "acme", description: "", status: "active", role: "member" };
const umbrella = { id: "tnt-b", name: "Umbrella", slug: "umbrella", description: "", status: "active", role: "member" };

// Both projects come back from the daemon, because this account belongs to
// both organizations. The filter below is a VIEW concern: it narrows a list the
// caller is entitled to down to the one organization on screen.
const projects = [
	{ id: "project-a", name: "project-a", path: "/tmp/a", tenantId: "tnt-a" },
	{ id: "project-b", name: "project-b", path: "/tmp/b", tenantId: "tnt-b" },
];

function wrapper({ children }: { children: ReactNode }) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

describe("useProjectsList with organizations", () => {
	beforeEach(() => {
		apiGET.mockReset();
		localStorage.clear();
		useTenantStore.getState().reset();
		setApiBaseUrl("http://127.0.0.1:4000");
		apiGET.mockImplementation((path: string) => {
			if (path === "/api/v1/tenants") return Promise.resolve({ data: { tenants: [acme, umbrella] } });
			return Promise.resolve({ data: { projects } });
		});
	});

	it("shows only the current organization's projects when there is more than one", async () => {
		await useTenantStore.getState().load();
		useTenantStore.getState().setCurrentTenant("tnt-a");

		const { result } = renderHook(() => useProjectsList(), { wrapper });
		await waitFor(() => expect(result.current.projects).toHaveLength(1));
		expect(result.current.projects[0].id).toBe("project-a");
	});

	it("shows every project when the account belongs to a single organization", async () => {
		apiGET.mockImplementation((path: string) => {
			if (path === "/api/v1/tenants") return Promise.resolve({ data: { tenants: [acme] } });
			return Promise.resolve({ data: { projects } });
		});
		await useTenantStore.getState().load();

		// With one organization there is nothing to choose between, so nothing
		// filters -- a single-organization installation behaves exactly as it
		// did before P4-C, including for a project whose tenantId the renderer
		// has never heard of.
		const { result } = renderHook(() => useProjectsList(), { wrapper });
		await waitFor(() => expect(result.current.projects).toHaveLength(2));
	});

	it("does not serve one organization's cached list for another", async () => {
		await useTenantStore.getState().load();
		useTenantStore.getState().setCurrentTenant("tnt-a");

		const { result, rerender } = renderHook(() => useProjectsList(), { wrapper });
		await waitFor(() => expect(result.current.projects).toHaveLength(1));
		expect(result.current.projects[0].id).toBe("project-a");

		useTenantStore.getState().setCurrentTenant("tnt-b");
		rerender();

		// The cache key carries the organization id, so the previous
		// organization's rows can never be handed to the new one -- not even
		// for the instant before a refetch lands.
		await waitFor(() => {
			expect(result.current.projects.map((p) => p.id)).toEqual(["project-b"]);
		});
	});
});
