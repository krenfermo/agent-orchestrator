import { afterEach, describe, expect, it, vi } from "vitest";
import { usesDemoWorkspaceData } from "./preview-mode";

/**
 * The regression this file exists for.
 *
 * `dev:web` used to serve invented workspaces purely because Electron was
 * absent. Pointed at a live daemon it therefore showed projects that do not
 * exist in it, beside API calls succeeding against the real thing — a preview
 * that silently shows fixture data, which is the one failure mode a preview
 * cannot have, because everything about it looks like a verification.
 *
 * Demo data is now opt-in. Being the browser build is not opting in.
 */
afterEach(() => {
	vi.unstubAllEnvs();
	delete (window as unknown as { __aoFakeAgent?: unknown }).__aoFakeAgent;
});

describe("usesDemoWorkspaceData", () => {
	it("is false for the plain browser build, so dev:web shows the real daemon", () => {
		vi.stubEnv("VITE_NO_ELECTRON", "1");
		expect(usesDemoWorkspaceData()).toBe(false);
	});

	it("is true only when demo data is explicitly requested", () => {
		vi.stubEnv("VITE_AO_DEMO_DATA", "1");
		expect(usesDemoWorkspaceData()).toBe(true);
	});

	it("is true when the e2e harness has injected its own timeline", () => {
		(window as unknown as { __aoFakeAgent?: unknown }).__aoFakeAgent = { snapshot: () => [] };
		expect(usesDemoWorkspaceData()).toBe(true);
	});

	it("does not turn on for any other value of the demo flag", () => {
		for (const value of ["0", "", "true", "yes"]) {
			vi.stubEnv("VITE_AO_DEMO_DATA", value);
			expect(usesDemoWorkspaceData()).toBe(false);
		}
	});
});
