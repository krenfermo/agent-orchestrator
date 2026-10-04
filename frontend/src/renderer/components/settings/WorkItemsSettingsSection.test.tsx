import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const apiGET = vi.fn();
const apiPUT = vi.fn();
const apiPOST = vi.fn();
const apiDELETE = vi.fn();

vi.mock("../../lib/api-client", async () => {
	const actual = await vi.importActual<typeof import("../../lib/api-client")>("../../lib/api-client");
	return {
		...actual,
		apiClient: {
			GET: (...args: unknown[]) => apiGET(...args),
			PUT: (...args: unknown[]) => apiPUT(...args),
			POST: (...args: unknown[]) => apiPOST(...args),
			DELETE: (...args: unknown[]) => apiDELETE(...args),
		},
	};
});

import { WorkItemsSettingsSection } from "./WorkItemsSettingsSection";

// WorkItemsSettingsSection.test.tsx — the states a person actually meets
// (P4-E §17).
//
// The interesting ones are the honest-but-awkward states: not connected,
// connected-but-degraded, and a link the provider could not answer for. A panel
// that renders the happy path and quietly mis-renders those is worse than none,
// because it makes an outage look like missing data.

function wrapper({ children }: { children: ReactNode }) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const DISCONNECTED = {
	projectId: "p1",
	provider: "plane",
	tokenConfigured: false,
	tokenFromEnv: false,
	enabled: false,
	syncStates: true,
	syncComments: true,
	connected: false,
	degraded: false,
};

// The state MEDUSA was actually stuck in: a base URL, a workspace and a stored
// token, no project mapping, and the connection off. Every field here is the
// shape of the real row (work_item_configs), not an invented one.
const HALF_CONFIGURED = {
	...DISCONNECTED,
	baseUrl: "http://plane.example.test",
	workspace: "acme-dev",
	tokenConfigured: true,
};

const CONNECTED = {
	...DISCONNECTED,
	workspace: "acme",
	externalProjectId: "plane-proj",
	externalProjectName: "Acme Web",
	externalProjectKey: "ACME",
	tokenConfigured: true,
	enabled: true,
	connected: true,
};

function answer(map: Record<string, unknown>) {
	apiGET.mockImplementation((path: string) => {
		for (const [key, data] of Object.entries(map)) {
			if (path === key) return Promise.resolve({ data });
		}
		return Promise.resolve({ data: {} });
	});
}

describe("Planning settings", () => {
	beforeEach(() => {
		apiGET.mockReset();
		apiPUT.mockReset();
		apiPOST.mockReset();
		apiDELETE.mockReset();
	});

	it("shows a project with no connection as not connected", async () => {
		answer({ "/api/v1/projects/{id}/workitems": DISCONNECTED });
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

		const status = await screen.findByTestId("workitems-status");
		expect(within(status).getByText("Not connected")).toBeInTheDocument();
		// And it does not pretend there is a credential.
		expect(screen.getByLabelText(/api token/i)).toHaveAttribute("placeholder", "Personal access token");
	});

	it("shows a working connection with the mapped project", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": CONNECTED,
			"/api/v1/projects/{id}/workitems/links": { links: [] },
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
		});
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

		const status = await screen.findByTestId("workitems-status");
		expect(within(status).getByText("Connected")).toBeInTheDocument();
		expect(within(status).getByText(/Acme Web/)).toBeInTheDocument();
		// A stored credential is reported as stored, never rendered.
		expect(screen.getByLabelText(/api token/i)).toHaveAttribute("placeholder", "A token is stored");
		expect(screen.getByLabelText(/api token/i)).toHaveValue("");
	});

	// Section 13: switched on and not working says so, rather than showing
	// green or hiding the panel.
	it("shows a degraded connection as degraded with the reason", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": {
				...CONNECTED,
				connected: false,
				degraded: true,
				lastCheckError: "Plane rejected the API token",
			},
			"/api/v1/projects/{id}/workitems/links": { links: [] },
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
		});
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

		const status = await screen.findByTestId("workitems-status");
		expect(within(status).getByText("Sync degraded")).toBeInTheDocument();
		expect(screen.getByTestId("workitems-last-error")).toHaveTextContent("Plane rejected the API token");
	});

	it("says when the credential comes from the environment", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": { ...CONNECTED, tokenFromEnv: true },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
		});
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });
		expect(await screen.findByText(/AO_PLANE_API_TOKEN/)).toBeInTheDocument();
	});

	it("lists linked items with their state and a way out to Plane", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": CONNECTED,
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			"/api/v1/projects/{id}/workitems/links": {
				links: [
					{
						id: "l1", projectId: "p1", scope: "run", scopeId: "r1",
						provider: "plane", workspace: "acme",
						externalProjectId: "plane-proj", externalItemId: "item-1",
						externalItemKey: "ACME-7",
						url: "https://app.plane.so/acme/projects/plane-proj/issues/item-1",
						origin: "manual", syncEnabled: true,
						title: "Fix the login redirect", state: "started", stateName: "In Progress",
						stale: false, readiness: "ready", createdAt: new Date().toISOString(),
					},
				],
			},
		});
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

		const link = await screen.findByTestId("workitems-link");
		expect(within(link).getByText("ACME-7")).toBeInTheDocument();
		expect(within(link).getByText("Fix the login redirect")).toBeInTheDocument();
		// The provider's own state NAME is what a person recognises.
		expect(within(link).getByText("In Progress")).toBeInTheDocument();
		expect(within(link).getByRole("link", { name: /open in plane/i })).toHaveAttribute(
			"href",
			"https://app.plane.so/acme/projects/plane-proj/issues/item-1",
		);
	});

	// Section 13: a link the provider could not answer for renders from the
	// cache and says so, rather than vanishing.
	it("renders an unreachable item from the cache and marks it stale", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": CONNECTED,
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			"/api/v1/projects/{id}/workitems/links": {
				links: [
					{
						id: "l1", projectId: "p1", scope: "task", scopeId: "t1",
						provider: "plane", workspace: "acme",
						externalProjectId: "plane-proj", externalItemId: "item-1",
						externalItemKey: "ACME-7", origin: "manual", syncEnabled: true,
						title: "Fix the login redirect", state: "started",
						stale: true, liveError: "Plane could not be reached",
						createdAt: new Date().toISOString(),
					},
				],
			},
		});
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

		const link = await screen.findByTestId("workitems-link");
		expect(within(link).getByText("cached")).toBeInTheDocument();
		expect(within(link).getByText("Plane could not be reached")).toBeInTheDocument();
		// The cached title is still there — the panel did not go blank.
		expect(within(link).getByText("Fix the login redirect")).toBeInTheDocument();
	});

	it("shows a muted link as not syncing", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": CONNECTED,
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			"/api/v1/projects/{id}/workitems/links": {
				links: [
					{
						id: "l1", projectId: "p1", scope: "run", scopeId: "r1",
						provider: "plane", workspace: "acme",
						externalProjectId: "plane-proj", externalItemId: "item-1",
						externalItemKey: "ACME-7", origin: "manual", syncEnabled: false,
						title: "Fix the login redirect", stale: false,
						createdAt: new Date().toISOString(),
					},
				],
			},
		});
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });
		const link = await screen.findByTestId("workitems-link");
		expect(within(link).getByText("sync off")).toBeInTheDocument();
	});

	it("shows the empty state when a connected project has no links", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": CONNECTED,
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
		});
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });
		expect(await screen.findByTestId("workitems-links-empty")).toBeInTheDocument();
	});

	// Saving the form with the token field blank must not clear the stored
	// credential — the request simply omits it.
	it("omits the token from a save when the field is blank", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": CONNECTED,
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
		});
		apiPUT.mockResolvedValue({ data: CONNECTED });
		const user = userEvent.setup();
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

		await user.click(await screen.findByRole("button", { name: "Save" }));
		expect(apiPUT).toHaveBeenCalledTimes(1);
		const body = apiPUT.mock.calls[0][1].body;
		expect(body).not.toHaveProperty("apiToken");
		expect(body.workspace).toBe("acme");
	});

	it("sends the token when one was typed", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": CONNECTED,
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
		});
		apiPUT.mockResolvedValue({ data: CONNECTED });
		const user = userEvent.setup();
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

		await user.type(await screen.findByLabelText(/api token/i), "plane_abc123");
		await user.click(screen.getByRole("button", { name: "Save" }));
		expect(apiPUT.mock.calls[0][1].body.apiToken).toBe("plane_abc123");
	});

	// An unauthorized caller gets the daemon's error rendered, not a blank
	// panel that looks like the feature is missing.
	it("renders an authorization failure rather than an empty panel", async () => {
		apiGET.mockResolvedValue({ error: { code: "PROJECT_NOT_FOUND", message: "project not found" } });
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });
		expect(await screen.findByText(/project not found/i)).toBeInTheDocument();
	});

	it("reports a failed connection test without clearing the form", async () => {
		answer({
			"/api/v1/projects/{id}/workitems": CONNECTED,
			"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
		});
		apiPOST.mockResolvedValue({ error: { code: "PLANE_AUTH_FAILED", message: "Plane rejected the API token" } });
		const user = userEvent.setup();
		render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

		await user.click(await screen.findByRole("button", { name: /test connection/i }));
		expect(await screen.findByTestId("workitems-error")).toHaveTextContent("Plane rejected the API token");
		expect(screen.getByLabelText(/workspace slug/i)).toHaveValue("acme");
	});

	// The deadlock, at the widget. A stored token plus a workspace and no
	// mapping used to be a terminal state: the project id was settable ONLY
	// through the picker, and the picker could not load until a project was
	// already mapped.
	describe("a configuration with no project mapping", () => {
		it("offers a project id field so the mapping can be typed", async () => {
			answer({
				"/api/v1/projects/{id}/workitems": HALF_CONFIGURED,
				"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			});
			render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

			const field = await screen.findByLabelText(/plane project id/i);
			expect(field).toHaveValue("");
			// The picker is not rendered — there is nothing to pick from — and
			// that must not be the only way in.
			expect(screen.queryByLabelText("Plane project")).not.toBeInTheDocument();
		});

		it("saves the typed project id alongside the rest, without erasing the stored token", async () => {
			answer({
				"/api/v1/projects/{id}/workitems": HALF_CONFIGURED,
				"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			});
			apiPUT.mockResolvedValue({ data: { ...HALF_CONFIGURED, externalProjectId: "uuid-1" } });
			render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

			await userEvent.type(await screen.findByLabelText(/plane project id/i), "uuid-1");
			await userEvent.click(screen.getByRole("button", { name: "Save" }));

			const body = apiPUT.mock.calls[0][1].body;
            expect(body.externalProjectId).toBe("uuid-1");
			expect(body.workspace).toBe("acme-dev");
			expect(body.baseUrl).toBe("http://plane.example.test");
			// The credential field was untouched, so it must not travel at all.
			expect(body).not.toHaveProperty("apiToken");
		});

		it("says why the connection cannot be switched on yet", async () => {
			answer({
				"/api/v1/projects/{id}/workitems": HALF_CONFIGURED,
				"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			});
			render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

			expect(await screen.findByTestId("workitems-enable-blocked")).toHaveTextContent(
				"A workspace, a project id and an API token are all required",
			);

			// Typing is not saving, and the toggle says so rather than looking
			// broken.
			await userEvent.type(screen.getByLabelText(/plane project id/i), "uuid-1");
			expect(screen.getByTestId("workitems-enable-blocked")).toHaveTextContent(
				"Save the project id before switching the connection on",
			);
		});

		it("can still test the connection: a preflight needs a workspace and a token, not a mapping", async () => {
			answer({
				"/api/v1/projects/{id}/workitems": HALF_CONFIGURED,
				"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			});
			apiPOST.mockResolvedValue({ data: { provider: "plane", workspace: "acme-dev", projects: 4 } });
			render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

			const testButton = await screen.findByRole("button", { name: "Test connection" });
			expect(testButton).toBeEnabled();
			await userEvent.click(testButton);

			expect(apiPOST).toHaveBeenCalledWith(
				"/api/v1/projects/{id}/workitems/test",
				expect.objectContaining({ params: { path: { id: "p1" } } }),
			);
			expect(await screen.findByText(/Connected to acme-dev/)).toBeInTheDocument();
		});

		it("enables the toggle once the daemon has the mapping stored", async () => {
			answer({
				"/api/v1/projects/{id}/workitems": { ...HALF_CONFIGURED, externalProjectId: "uuid-1" },
				"/api/v1/projects/{id}/workitems/projects": { projects: [] },
			});
			render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

			await screen.findByLabelText(/plane project id/i);
			expect(screen.queryByTestId("workitems-enable-blocked")).not.toBeInTheDocument();
			expect(screen.getByRole("checkbox", { name: /Connect this project/ })).toBeEnabled();
		});

		it("keeps the picker as the shortcut it was meant to be", async () => {
			answer({
				"/api/v1/projects/{id}/workitems": HALF_CONFIGURED,
				"/api/v1/projects/{id}/workitems/projects": {
					projects: [{ id: "uuid-9", name: "Medusa", identifier: "MED" }],
				},
			});
			apiPUT.mockResolvedValue({ data: { ...HALF_CONFIGURED, externalProjectId: "uuid-9" } });
			render(<WorkItemsSettingsSection projectId="p1" />, { wrapper });

			await userEvent.click(await screen.findByLabelText("Plane project"));
			await userEvent.click(await screen.findByText("Medusa (MED)"));

			expect(apiPUT.mock.calls[0][1].body).toEqual({ externalProjectId: "uuid-9" });
			// And the choice lands in the field, so the two controls agree.
			expect(screen.getByLabelText(/plane project id/i)).toHaveValue("uuid-9");
		});
	});
});
