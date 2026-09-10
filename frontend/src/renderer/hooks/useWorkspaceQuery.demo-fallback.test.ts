import { afterEach, describe, expect, it, vi } from "vitest";

/**
 * The behavioural half of the same regression: with the daemon reachable and
 * demo mode off, the workspace query must read the API — and when the daemon
 * is NOT reachable it must fail loudly rather than quietly substituting
 * invented workspaces.
 */
afterEach(() => {
	vi.resetModules();
	vi.unstubAllEnvs();
	vi.restoreAllMocks();
	delete (window as unknown as { __aoFakeAgent?: unknown }).__aoFakeAgent;
});

async function loadQuery(opts: { trustedBase: boolean }) {
	vi.doMock("../lib/api-client", () => ({
		apiClient: {
			GET: vi.fn(async (path: string) =>
				path === "/api/v1/projects"
					? { data: { projects: [{ id: "real-project", name: "Real", path: "/tmp/real" }] } }
					: { data: { sessions: [] } },
			),
		},
		hasTrustedApiBaseUrl: () => opts.trustedBase,
	}));
	return import("./useWorkspaceQuery");
}

describe("workspace data source", () => {
	it("reads the daemon in the browser build when demo data was not requested", async () => {
		vi.stubEnv("VITE_NO_ELECTRON", "1");
		const mod = await loadQuery({ trustedBase: true });
		const rows = await mod.workspaceQueryOptions.queryFn!();
		expect(Array.isArray(rows)).toBe(true);
		// The real project, not the demo dataset.
		expect(JSON.stringify(rows)).toContain("real-project");
		expect(JSON.stringify(rows)).not.toContain("ao-demo");
	});

	it("throws instead of showing demo workspaces when the daemon is unreachable", async () => {
		vi.stubEnv("VITE_NO_ELECTRON", "1");
		const mod = await loadQuery({ trustedBase: false });
		await expect(mod.workspaceQueryOptions.queryFn!()).rejects.toThrow(
			/daemon API is not ready/i,
		);
	});

	it("uses the demo dataset only when explicitly asked", async () => {
		vi.stubEnv("VITE_NO_ELECTRON", "1");
		vi.stubEnv("VITE_AO_DEMO_DATA", "1");
		const mod = await loadQuery({ trustedBase: false });
		const rows = await mod.workspaceQueryOptions.queryFn!();
		expect(JSON.stringify(rows)).toContain("ao-demo");
	});
});
