import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const apiGET = vi.fn();
const apiPOST = vi.fn();
const apiDELETE = vi.fn();

vi.mock("../lib/api-client", async () => {
	const actual = await vi.importActual<typeof import("../lib/api-client")>("../lib/api-client");
	return {
		...actual,
		apiClient: {
			GET: (...args: unknown[]) => apiGET(...args),
			POST: (...args: unknown[]) => apiPOST(...args),
			DELETE: (...args: unknown[]) => apiDELETE(...args),
		},
	};
});

import { setApiBaseUrl } from "../lib/api-client";
import { WorkItemLinkPanel } from "./workitem-link-panel";

// workitem-link-panel.test.tsx — the run/task planning panel (P4-E §6, §9).
//
// The states worth pinning are the ones that are easy to get subtly wrong: a
// project with no connection must render NOTHING (an integration nobody set up
// should not occupy a line saying so), a denied project must be
// indistinguishable from an unconfigured one, and a provider outage must show
// the cached item rather than an empty panel that reads as lost data.

function wrapper({ children }: { children: ReactNode }) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const CONNECTED_HEALTH = {
	configured: true,
	enabled: true,
	connected: true,
	degraded: false,
	pending: 0,
	failed: 0,
	links: 1,
};

const LINK = {
	id: "l1",
	projectId: "p1",
	scope: "run",
	scopeId: "run-1",
	provider: "plane",
	workspace: "acme",
	externalProjectId: "plane-proj",
	externalItemId: "item-1",
	externalItemKey: "ACME-7",
	url: "https://app.plane.so/acme/projects/plane-proj/issues/item-1",
	origin: "manual",
	syncEnabled: true,
	title: "Fix the login redirect",
	state: "started",
	stateName: "In Progress",
	stale: false,
	readiness: "ready",
	createdAt: new Date().toISOString(),
};

function answer(map: Record<string, unknown>) {
	apiGET.mockImplementation((path: string) => {
		for (const [key, data] of Object.entries(map)) {
			if (path === key) return Promise.resolve({ data });
		}
		return Promise.resolve({ data: {} });
	});
}

function renderPanel(scope: "run" | "task" = "run", scopeId = "run-1") {
	return render(<WorkItemLinkPanel projectId="p1" scope={scope} scopeId={scopeId} />, { wrapper });
}

describe("Work item link panel", () => {
	beforeEach(() => {
		// The panel's queries are gated on a trusted API base URL, the way every
		// renderer query is; without one they never run and the panel is
		// correctly empty for a reason that has nothing to do with these tests.
		setApiBaseUrl("http://127.0.0.1:4000");
		apiGET.mockReset();
		apiPOST.mockReset();
		apiDELETE.mockReset();
	});

	// The common case: no connection, no panel, no clutter.
	it("renders nothing when the project has no connection", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": { ...CONNECTED_HEALTH, enabled: false, configured: false },
		});
		const { container } = renderPanel();
		await waitFor(() => expect(apiGET).toHaveBeenCalled());
		expect(screen.queryByTestId("workitem-link-panel")).not.toBeInTheDocument();
		expect(container).toBeEmptyDOMElement();
	});

	// §7: a project the caller cannot reach answers 404, and the panel must not
	// hint that anything exists there.
	it("renders nothing for a project the caller cannot reach", async () => {
		apiGET.mockResolvedValue({ error: { code: "PROJECT_NOT_FOUND", message: "project not found" } });
		const { container } = renderPanel();
		await waitFor(() => expect(apiGET).toHaveBeenCalled());
		expect(container).toBeEmptyDOMElement();
	});

	it("offers a link form when the run is not linked yet", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": { ...CONNECTED_HEALTH, links: 0 },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
		});
		renderPanel();
		expect(await screen.findByTestId("workitem-link-panel")).toBeInTheDocument();
		// The form appears once the links read settles; asserting before that
		// would be racing the loading state rather than testing the empty one.
		expect(await screen.findByLabelText(/work item reference/i)).toBeInTheDocument();
		expect(screen.queryByTestId("workitem-linked")).not.toBeInTheDocument();
	});

	it("shows the linked item with its state and a way out to Plane", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": CONNECTED_HEALTH,
			"/api/v1/projects/{id}/workitems/links": { links: [LINK] },
		});
		renderPanel();

		const linked = await screen.findByTestId("workitem-linked");
		expect(within(linked).getByText("ACME-7")).toBeInTheDocument();
		expect(within(linked).getByText("Fix the login redirect")).toBeInTheDocument();
		// The provider's own state NAME, which is what a person recognises.
		expect(within(linked).getByText("In Progress")).toBeInTheDocument();
		expect(within(linked).getByRole("link", { name: /open in plane/i })).toHaveAttribute("href", LINK.url);
	});

	// A run's panel must not show a task's link, or vice versa.
	it("shows only the link for its own scope", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": CONNECTED_HEALTH,
			"/api/v1/projects/{id}/workitems/links": {
				links: [LINK, { ...LINK, id: "l2", scope: "task", scopeId: "task-9", externalItemKey: "ACME-9" }],
			},
		});
		renderPanel("task", "task-9");
		const linked = await screen.findByTestId("workitem-linked");
		expect(within(linked).getByText("ACME-9")).toBeInTheDocument();
		expect(within(linked).queryByText("ACME-7")).not.toBeInTheDocument();
	});

	it("links an existing item by reference", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": { ...CONNECTED_HEALTH, links: 0 },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
		});
		apiPOST.mockResolvedValue({ data: LINK });
		const user = userEvent.setup();
		renderPanel();

		await user.type(await screen.findByLabelText(/work item reference/i), "ACME-7");
		await user.click(screen.getByRole("button", { name: "Link" }));

		expect(apiPOST).toHaveBeenCalledTimes(1);
		const body = apiPOST.mock.calls[0][1].body;
		expect(body).toMatchObject({ scope: "run", scopeId: "run-1", reference: "ACME-7", syncEnabled: true });
	});

	it("unlinks", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": CONNECTED_HEALTH,
			"/api/v1/projects/{id}/workitems/links": { links: [LINK] },
		});
		apiDELETE.mockResolvedValue({});
		const user = userEvent.setup();
		renderPanel();

		await user.click(await screen.findByRole("button", { name: /unlink/i }));
		expect(apiDELETE).toHaveBeenCalledTimes(1);
		expect(apiDELETE.mock.calls[0][1].params.path).toMatchObject({ id: "p1", linkId: "l1" });
	});

	// §8: the four observable states. Degraded and pending are what tell
	// somebody the external side is behind.
	it("shows sync as degraded when the integration is failing", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": { ...CONNECTED_HEALTH, degraded: true, failed: 2 },
			"/api/v1/projects/{id}/workitems/links": { links: [LINK] },
		});
		renderPanel();
		expect(await screen.findByTestId("workitem-panel-degraded")).toHaveTextContent("Sync degraded");
	});

	it("shows how many updates are still queued", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": { ...CONNECTED_HEALTH, pending: 3 },
			"/api/v1/projects/{id}/workitems/links": { links: [LINK] },
		});
		renderPanel();
		expect(await screen.findByTestId("workitem-panel-pending")).toHaveTextContent("3");
	});

	// §6/§13: an unreachable item renders from cache, badged, with the reason —
	// never as an empty panel, which would read as AO having lost the link.
	it("renders an unreachable item from the cache and marks it stale", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": { ...CONNECTED_HEALTH, degraded: true },
			"/api/v1/projects/{id}/workitems/links": {
				links: [{ ...LINK, stale: true, liveError: "Plane could not be reached", stateName: "" }],
			},
		});
		renderPanel();

		const linked = await screen.findByTestId("workitem-linked");
		expect(within(linked).getByText("cached")).toBeInTheDocument();
		expect(within(linked).getByText("Plane could not be reached")).toBeInTheDocument();
		expect(within(linked).getByText("Fix the login redirect")).toBeInTheDocument();
	});

	it("shows a muted link as not syncing", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": CONNECTED_HEALTH,
			"/api/v1/projects/{id}/workitems/links": { links: [{ ...LINK, syncEnabled: false }] },
		});
		renderPanel();
		expect(within(await screen.findByTestId("workitem-linked")).getByText("sync off")).toBeInTheDocument();
	});

	// §6: a caller without workitems.link gets the daemon's own refusal, shown
	// as-is rather than swallowed into a blank form.
	it("shows the daemon's refusal when the caller may not link", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": { ...CONNECTED_HEALTH, links: 0 },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
		});
		apiPOST.mockResolvedValue({
			error: { code: "FORBIDDEN", message: "you do not have permission to link work items" },
		});
		const user = userEvent.setup();
		renderPanel();

		await user.type(await screen.findByLabelText(/work item reference/i), "ACME-7");
		await user.click(screen.getByRole("button", { name: "Link" }));

		expect(await screen.findByTestId("workitem-panel-error")).toHaveTextContent(
			"you do not have permission to link work items",
		);
	});

	it("reports a rejected reference without clearing what was typed", async () => {
		answer({
			"/api/v1/projects/{id}/workitems/health": { ...CONNECTED_HEALTH, links: 0 },
			"/api/v1/projects/{id}/workitems/links": { links: [] },
		});
		apiPOST.mockResolvedValue({
			error: { code: "PLANE_NOT_FOUND", message: "not found in Plane" },
		});
		const user = userEvent.setup();
		renderPanel();

		const input = await screen.findByLabelText(/work item reference/i);
		await user.type(input, "ACME-999");
		await user.click(screen.getByRole("button", { name: "Link" }));

		expect(await screen.findByTestId("workitem-panel-error")).toHaveTextContent("not found in Plane");
		expect(input).toHaveValue("ACME-999");
	});
});
