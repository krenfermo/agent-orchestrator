import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const apiGET = vi.fn();
const apiPOST = vi.fn();

vi.mock("../../lib/api-client", async () => {
	const actual = await vi.importActual<typeof import("../../lib/api-client")>("../../lib/api-client");
	return {
		...actual,
		apiClient: {
			GET: (...args: unknown[]) => apiGET(...args),
			POST: (...args: unknown[]) => apiPOST(...args),
		},
	};
});

import { setApiBaseUrl } from "../../lib/api-client";
import { IntelligenceContext } from "./IntelligenceContext";
import { IntelligenceGraph } from "./IntelligenceGraph";
import { IntelligenceMemory } from "./IntelligenceMemory";
import { IntelligenceOverview } from "./IntelligenceOverview";
import { IntelligenceSearch } from "./IntelligenceSearch";
import { ProjectIntelligenceView } from "./ProjectIntelligenceView";

// ProjectIntelligence.test.tsx — the states a person actually meets.
//
// The ones worth asserting are the honest-but-awkward ones: a project nobody
// has indexed, a graph that has fallen behind the checkout, a walk that hit its
// ceiling, and a context preview that must never call its measurements
// "savings". A screen that renders the happy path correctly and quietly
// mis-renders those is worse than no screen.

function wrapper({ children }: { children: ReactNode }) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const READY_REPO = {
	repoId: "repo_1",
	repoPath: "/src/ao",
	backend: "local",
	state: "ready",
	indexedCommit: "abcdef1234567890",
	headCommit: "abcdef1234567890",
	generation: 3,
	files: 120,
	symbols: 900,
	edges: 2400,
	lastSyncKind: "incremental",
	filesParsed: 1,
	filesReused: 119,
	filesRemoved: 0,
	lastMillis: 850,
	memoryItems: 42,
};

function answer(map: Record<string, unknown>) {
	apiGET.mockImplementation((path: string) => {
		for (const [key, data] of Object.entries(map)) {
			if (path === key) return Promise.resolve({ data });
		}
		return Promise.resolve({ data: {} });
	});
}

describe("Project Intelligence", () => {
	beforeEach(() => {
		apiGET.mockReset();
		apiPOST.mockReset();
		setApiBaseUrl("http://127.0.0.1:4000");
	});

	it("shows a project nobody has indexed as pending, not as broken", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence": {
				projectId: "p1",
				repos: [{ ...READY_REPO, state: "pending", indexedCommit: "", generation: 0, files: 0, symbols: 0, edges: 0, lastSyncKind: "" }],
			},
		});
		render(<IntelligenceOverview projectId="p1" />, { wrapper });

		expect(await screen.findByTestId("intelligence-state-pending")).toHaveTextContent("Not indexed yet");
	});

	it("shows a graph that has fallen behind as out of date, and says why", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence": {
				projectId: "p1",
				repos: [
					{
						...READY_REPO,
						state: "stale",
						headCommit: "999999999999",
						drift: "the checkout is at 999999999999 and the graph was indexed at abcdef123456; run a sync",
					},
				],
			},
		});
		render(<IntelligenceOverview projectId="p1" />, { wrapper });

		expect(await screen.findByTestId("intelligence-state-stale")).toHaveTextContent("Out of date");
		expect(screen.getByTestId("intelligence-drift")).toHaveTextContent(/run a sync/);
		// Both commits are on screen, because "these differ" is the whole of
		// what stale means and a person should be able to see it.
		expect(screen.getByTitle("abcdef1234567890")).toBeInTheDocument();
		expect(screen.getByTitle("999999999999")).toBeInTheDocument();
	});

	it("surfaces a failed index with its error", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence": {
				projectId: "p1",
				repos: [{ ...READY_REPO, state: "failed", lastError: "permission denied reading /src/ao/vendor" }],
			},
		});
		render(<IntelligenceOverview projectId="p1" />, { wrapper });

		expect(await screen.findByTestId("intelligence-state-failed")).toBeInTheDocument();
		expect(screen.getByTestId("intelligence-last-error")).toHaveTextContent(/permission denied/);
	});

	it("syncs without confirmation and rebuilds only after one", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence": { projectId: "p1", repos: [READY_REPO] },
		});
		apiPOST.mockResolvedValue({ data: {} });
		const user = userEvent.setup();
		render(<IntelligenceOverview projectId="p1" />, { wrapper });

		await user.click(await screen.findByRole("button", { name: /sync now/i }));
		expect(apiPOST).toHaveBeenCalledTimes(1);

		// Rebuild is destructive enough to ask first.
		await user.click(screen.getByRole("button", { name: /^rebuild$/i }));
		expect(apiPOST).toHaveBeenCalledTimes(1);
		expect(await screen.findByText(/Rebuild this project's index\?/i)).toBeInTheDocument();

		const dialog = screen.getByRole("dialog");
		await user.click(within(dialog).getByRole("button", { name: /^rebuild$/i }));
		expect(apiPOST).toHaveBeenCalledTimes(2);
	});

	it("does not draw a graph until something is asked about", async () => {
		render(<IntelligenceGraph projectId="p1" seed="" onSeedChange={() => {}} />, { wrapper });

		expect(screen.getByTestId("graph-needs-seed")).toBeInTheDocument();
		// The critical performance rule: no seed, no request.
		expect(apiGET).not.toHaveBeenCalled();
	});

	it("renders a bounded neighbourhood and says when it was truncated", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence/graph": {
				seeds: ["a.go#Alpha"],
				nodes: [
					{ key: "a.go#Alpha", name: "Alpha", kind: "func", path: "a.go", depth: 0, exported: true, line: 3 },
					{ key: "b.go#Beta", name: "Beta", kind: "func", path: "b.go", depth: 1, exported: true, line: 9 },
				],
				edges: [{ kind: "calls", from: "a.go#Alpha", to: "b.go#Beta", path: "a.go", line: 4 }],
				truncated: true,
				generation: 3,
				indexedCommit: "abcdef1234567890",
			},
		});
		render(<IntelligenceGraph projectId="p1" seed="Alpha" onSeedChange={() => {}} />, { wrapper });

		expect(await screen.findByTestId("graph-counts")).toHaveTextContent("2 nodes · 1 relations");
		expect(screen.getByTestId("graph-truncated")).toHaveTextContent(/more exists than is shown/i);
	});

	it("shows a node's incoming and outgoing relations when it is selected", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence/graph": {
				seeds: ["a.go#Alpha"],
				nodes: [
					{ key: "a.go#Alpha", name: "Alpha", kind: "func", path: "a.go", depth: 0, exported: true, line: 3 },
					{ key: "b.go#Beta", name: "Beta", kind: "func", path: "b.go", depth: 1, exported: true, line: 9 },
				],
				edges: [{ kind: "calls", from: "a.go#Alpha", to: "b.go#Beta", path: "a.go", line: 4 }],
				truncated: false,
				generation: 3,
			},
		});
		const user = userEvent.setup();
		render(<IntelligenceGraph projectId="p1" seed="Alpha" onSeedChange={() => {}} />, { wrapper });

		await screen.findByTestId("graph-counts");
		await user.click(screen.getByText("Alpha"));

		const selection = await screen.findByTestId("graph-selection");
		expect(within(selection).getByText(/Reaches \(1\)/)).toBeInTheDocument();
		expect(within(selection).getByText(/calls → Beta/)).toBeInTheDocument();
	});

	it("groups memory by the kind of claim each fact is", async () => {
		answer({
			"/api/v1/projects/{id}/memory/items": {
				items: [
					{
						id: "m1", summary: "Sessions are derived, never stored", type: "convention",
						scope: "repository", state: "valid", origin: "canonical", authority: "authoritative",
						confidence: 0.9, contentBytes: 120, generation: 2, repoId: "repo_1",
						servable: true, updatedAt: new Date().toISOString(), invalidatedAt: null,
						provenanceKind: "repo_derivation", sourcePaths: ["docs/architecture.md"],
					},
					{
						id: "m2", summary: "The reviewer needs the branch lock", type: "risk",
						scope: "task", state: "stale", origin: "canonical", authority: "authoritative",
						confidence: 0.5, contentBytes: 60, generation: 2, repoId: "repo_1",
						servable: false, updatedAt: new Date().toISOString(), invalidatedAt: null,
						provenanceKind: "task_outcome",
					},
				],
			},
		});
		render(<IntelligenceMemory projectId="p1" />, { wrapper });

		expect(await screen.findByTestId("memory-group-project-facts")).toBeInTheDocument();
		expect(screen.getByTestId("memory-group-workflow")).toBeInTheDocument();
		// A fact AO will not serve says so, rather than looking like the rest.
		expect(screen.getByText(/withheld from dispatch/)).toBeInTheDocument();
	});

	// P4-H: the tab exists to show the high-level facts, and the class of claim
	// each one is. A derived fact rendered like an observed one is how a guess
	// becomes a premise nobody rechecks.
	it("shows the high-level facts with how strong each claim is", async () => {
		answer({
			"/api/v1/projects/{id}/memory/items": {
				items: [
					{
						id: "h1", summary: "Auth model: identity/permission code concentrated in internal/oidc",
						type: "auth_model", scope: "repository", state: "valid", origin: "canonical",
						authority: "authoritative", confidence: 0.55, contentBytes: 400, generation: 2,
						repoId: "repo_1", servable: true, updatedAt: new Date().toISOString(),
						invalidatedAt: null, provenanceKind: "repo_derivation",
						evidenceClass: "derived", sourcePaths: ["internal/oidc/login.go"],
					},
					{
						id: "h2", summary: "Persistence: 116 tables declared by the schema",
						type: "persistence", scope: "repository", state: "valid", origin: "canonical",
						authority: "authoritative", confidence: 0.85, contentBytes: 300, generation: 2,
						repoId: "repo_1", servable: true, updatedAt: new Date().toISOString(),
						invalidatedAt: null, provenanceKind: "repo_derivation",
						evidenceClass: "observed", sourcePaths: ["internal/storage/queries/users.sql"],
					},
				],
			},
		});
		render(<IntelligenceMemory projectId="p1" />, { wrapper });

		const group = await screen.findByTestId("memory-group-shape");
		expect(within(group).getByText(/Auth model/)).toBeInTheDocument();
		expect(within(group).getByText(/116 tables/)).toBeInTheDocument();
		// The two facts are labelled differently, which is the whole point.
		expect(within(group).getByText("inferred")).toBeInTheDocument();
		expect(within(group).getByText("observed")).toBeInTheDocument();
	});

	// Section 8: provenance is expandable. A label a reader cannot check is not
	// much better than no label, and the body is where a derived fact states
	// how it was determined.
	it("expands a fact to the evidence behind it", async () => {
		answer({
			"/api/v1/projects/{id}/memory/items": {
				items: [
					{
						id: "h1", summary: "Auth model: identity/permission code concentrated in internal/oidc",
						type: "auth_model", scope: "repository", state: "valid", origin: "canonical",
						authority: "authoritative", confidence: 0.55, contentBytes: 400, generation: 2,
						repoId: "repo_1", servable: true, updatedAt: new Date().toISOString(),
						invalidatedAt: null, provenanceKind: "repo_derivation", evidenceClass: "derived",
						sourcePaths: ["internal/oidc/login.go"],
					},
				],
			},
			"/api/v1/projects/{id}/memory/provenance/{itemId}": {
				item: {
					id: "h1", summary: "Auth model", type: "auth_model", scope: "repository",
					state: "valid", origin: "canonical", authority: "authoritative", confidence: 0.55,
					contentBytes: 400, generation: 2, repoId: "repo_1", servable: true,
					updatedAt: new Date().toISOString(), invalidatedAt: null,
					content: "Files to read first:\n  - internal/oidc/login.go\n\nHow this was determined: " +
						"directory and file naming during the repository scan. AO has NOT verified that these files " +
						"implement the authorization model.",
					sourcePaths: ["internal/oidc/login.go", "internal/rbac/roles.go"],
					metadata: { derivedBy: "repository_scan", files: "12" },
				},
				servable: true,
				relations: [],
			},
		});
		const user = userEvent.setup();
		render(<IntelligenceMemory projectId="p1" />, { wrapper });

		// Collapsed: the caveat is not on screen.
		expect(await screen.findByTestId("memory-item")).toBeInTheDocument();
		expect(screen.queryByTestId("memory-evidence")).not.toBeInTheDocument();

		await user.click(screen.getByRole("button", { expanded: false }));

		const evidence = await screen.findByTestId("memory-evidence");
		expect(within(evidence).getByText(/AO has NOT verified/)).toBeInTheDocument();
		expect(within(evidence).getByText("internal/rbac/roles.go")).toBeInTheDocument();
	});

	it("labels every search hit with the authority that produced it", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence/search": {
				query: "permisos",
				hits: [
					{ kind: "symbol", title: "MayExport", detail: "func (r *Records) MayExport(role string) bool", path: "internal/records.go", line: 12, symbolKind: "method", score: 6 },
					{ kind: "memory", title: "Supervisor may export", memoryType: "convention", state: "valid", score: 4 },
				],
				memoryHits: 1,
				symbolHits: 1,
				truncated: false,
				generation: 3,
			},
		});
		const user = userEvent.setup();
		render(<IntelligenceSearch projectId="p1" />, { wrapper });

		await user.type(screen.getByLabelText(/ask about this project/i), "permisos");
		await user.click(screen.getByRole("button", { name: /search/i }));

		const hits = await screen.findAllByTestId("search-hit");
		expect(hits).toHaveLength(2);
		expect(within(hits[0]).getByText("Code graph")).toBeInTheDocument();
		expect(within(hits[1]).getByText("Memory")).toBeInTheDocument();
		expect(screen.getByTestId("search-counts")).toHaveTextContent("1 from memory · 1 from the code graph");
	});

	// P4-H: a memory hit also says how strong a claim it is. Two memory rows
	// of different classes must not read as equally settled.
	it("labels a memory hit with how strong its claim is", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence/search": {
				query: "permisos",
				hits: [
					{
						kind: "memory", title: "Auth model: code concentrated in internal/oidc",
						memoryType: "auth_model", state: "valid", evidenceClass: "derived",
						confidence: 0.55, provenance: "repo_derivation", score: 8,
					},
					{
						kind: "memory", title: "AGENTS.md — standing instructions",
						memoryType: "instruction", state: "valid", evidenceClass: "observed",
						confidence: 0.95, provenance: "repo_derivation", score: 4,
					},
				],
				memoryHits: 2, symbolHits: 0, truncated: false, generation: 3,
			},
		});
		const user = userEvent.setup();
		render(<IntelligenceSearch projectId="p1" />, { wrapper });

		await user.type(screen.getByLabelText(/ask about this project/i), "permisos");
		await user.click(screen.getByRole("button", { name: /search/i }));

		const hits = await screen.findAllByTestId("search-hit");
		expect(within(hits[0]).getByTestId("search-hit-evidence")).toHaveTextContent("inferred");
		expect(within(hits[1]).getByTestId("search-hit-evidence")).toHaveTextContent("observed");
	});

	it("reports context as selected and avoided, never as saved", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence/context": {
				role: "planner",
				projectId: "p1",
				repoId: "repo_1",
				sections: [
					{ title: "Conventions", type: "convention", items: [{ summary: "Status is derived", bodyIncluded: true, score: 3, reason: "module match", state: "valid" }] },
				],
				graph: { backend: "local", generation: 3, consideredSymbols: 40, consideredEdges: 90, selectedSymbols: 8, selectedEdges: 12, bytes: 2048, estimatedTokens: 512 },
				candidateItems: 30,
				candidateBytes: 40000,
				selectedItems: 6,
				selectedBytes: 8192,
				estimatedTokens: 2048,
				droppedItems: 24,
				droppedToSummary: 3,
				staleExcluded: 2,
				generation: 3,
				empty: false,
			},
		});
		render(<IntelligenceContext projectId="p1" />, { wrapper });

		const metrics = await screen.findByTestId("context-metrics");
		expect(within(metrics).getByText("6 of 30")).toBeInTheDocument();
		expect(within(metrics).getByText("Context avoided")).toBeInTheDocument();
		// The honesty rule, asserted: nothing on this screen claims a saving.
		expect(screen.queryByText(/saved/i)).not.toBeInTheDocument();
		expect(screen.getByText(/does not claim to have prevented a read/i)).toBeInTheDocument();
	});

	it("rolls the project's headline state up to its worst repository", async () => {
		answer({
			"/api/v1/projects/{id}/intelligence": {
				projectId: "p1",
				repos: [READY_REPO, { ...READY_REPO, repoId: "repo_2", state: "stale" }],
			},
		});
		render(<ProjectIntelligenceView projectId="p1" />, { wrapper });

		// One stale repository makes the project's picture stale. Rolling up to
		// "ready" would be the reassuring answer rather than the true one.
		const headline = await screen.findByTestId("intelligence-headline-state");
		expect(within(headline).getByTestId("intelligence-state-stale")).toBeInTheDocument();
	});

	it("offers all six tabs", async () => {
		answer({ "/api/v1/projects/{id}/intelligence": { projectId: "p1", repos: [READY_REPO] } });
		render(<ProjectIntelligenceView projectId="p1" />, { wrapper });

		for (const label of ["Overview", "Architecture", "Graph", "Memory", "Search", "Context"]) {
			expect(await screen.findByRole("tab", { name: label })).toBeInTheDocument();
		}
	});
});
