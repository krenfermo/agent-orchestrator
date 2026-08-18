import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { getMock } = vi.hoisted(() => ({ getMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: vi.fn() },
	apiErrorMessage: () => "request failed",
	hasTrustedApiBaseUrl: () => true,
}));

vi.mock("@tanstack/react-router", async (importOriginal) => {
	const actual = await importOriginal<typeof import("@tanstack/react-router")>();
	return {
		...actual,
		Link: ({ children, to }: { children: React.ReactNode; to: string }) => <a href={to}>{children}</a>,
	};
});

import { WorkflowRunView } from "./_shell.workflows.$workflowId";

function wrapper({ children }: { children: ReactNode }) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
	});
	return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
	getMock.mockReset();
});

describe("WorkflowRunView", () => {
	// Checkpoint 8P-D.1: reproduces the real-browser "Something went wrong!"
	// crash (minified React error #310) directly, by letting the query
	// actually resolve asynchronously -- a component-level mock that returns
	// data synchronously would never expose a Rules-of-Hooks violation
	// between the "no data yet" and "data arrived" renders the way a real
	// network round-trip does. If a future change moves a hook back below an
	// early return, this test throws again exactly like the live app did.
	it("renders through the loading-to-loaded transition without crashing (regression: React error #310)", async () => {
		let resolveGet!: (value: unknown) => void;
		getMock.mockReturnValue(
			new Promise((resolve) => {
				resolveGet = resolve;
			}),
		);

		render(<WorkflowRunView workflowId="wf-pending" />, { wrapper });
		expect(screen.getByText(/loading/i)).toBeInTheDocument();

		resolveGet({
			data: {
				workflow: {
					run: {
						id: "wf-pending",
						projectId: "proj-1",
						objective: "Create a file named status.txt",
						state: "pending",
						createdAt: "2026-08-17T18:54:01.866Z",
						updatedAt: "2026-08-17T18:54:01.866Z",
						executionMode: "manual",
					},
					steps: [{ id: "wfs-1", kind: "plan", ordinal: 1, state: "ready", createdAt: "2026-08-17T18:54:01.866Z", updatedAt: "2026-08-17T18:54:01.866Z", attempts: [] }],
					plan: { status: "pending", approvalMode: "manual", promptContextVersion: "v1" },
				},
			},
			error: undefined,
		});

		await waitFor(() => expect(screen.getByText("Create a file named status.txt")).toBeInTheDocument());
		expect(screen.queryByText(/something went wrong/i)).not.toBeInTheDocument();
	});

	it("renders a legacy/pre-8P-D workflow record missing plan, tasks, integrationState, questions, and usage", async () => {
		getMock.mockResolvedValue({
			data: {
				workflow: {
					run: {
						id: "wf-legacy",
						projectId: "proj-1",
						objective: "Legacy run from before Checkpoint 8P-D",
						state: "completed",
						createdAt: "2026-01-01T00:00:00Z",
						updatedAt: "2026-01-01T00:00:00Z",
						executionMode: "manual",
					},
					steps: [],
				},
			},
			error: undefined,
		});

		render(<WorkflowRunView workflowId="wf-legacy" />, { wrapper });

		await waitFor(() => expect(screen.getByText("Legacy run from before Checkpoint 8P-D")).toBeInTheDocument());
		expect(screen.queryByText(/something went wrong/i)).not.toBeInTheDocument();
	});

	it("renders a newly created autonomous workflow with master-plan tasks and no crash", async () => {
		getMock.mockResolvedValue({
			data: {
				workflow: {
					run: {
						id: "wf-auto",
						projectId: "proj-1",
						objective: "Build a small Go products library",
						state: "running",
						createdAt: "2026-08-18T12:00:00Z",
						updatedAt: "2026-08-18T12:00:00Z",
						executionMode: "autonomous",
					},
					steps: [{ id: "wfs-1", kind: "plan", ordinal: 1, state: "completed", createdAt: "2026-08-18T12:00:00Z", updatedAt: "2026-08-18T12:00:00Z", attempts: [] }],
					plan: { status: "approved", approvalMode: "auto", promptContextVersion: "v1" },
					tasks: [
						{
							id: "task-1",
							number: 1,
							title: "Define Product model",
							description: "Add validation",
							state: "running",
							dependencies: [],
							acceptanceCriteria: [],
							verify: {},
							executionWorkflowId: "wf-child-1",
						},
					],
				},
			},
			error: undefined,
		});

		render(<WorkflowRunView workflowId="wf-auto" />, { wrapper });

		await waitFor(() => expect(screen.getByText("Build a small Go products library")).toBeInTheDocument());
		expect(screen.getByText(/Define Product model/)).toBeInTheDocument();
		expect(screen.queryByText(/something went wrong/i)).not.toBeInTheDocument();
	});
});
