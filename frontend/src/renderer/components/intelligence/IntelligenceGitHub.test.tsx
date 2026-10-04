import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const apiGET = vi.fn();

vi.mock("../../lib/api-client", async () => {
	const actual = await vi.importActual<typeof import("../../lib/api-client")>("../../lib/api-client");
	return {
		...actual,
		apiClient: { GET: (...args: unknown[]) => apiGET(...args) },
	};
});

import { setApiBaseUrl } from "../../lib/api-client";
import { aoBridge } from "../../lib/bridge";
import { IntelligenceGitHub } from "./IntelligenceGitHub";

// IntelligenceGitHub.test.tsx — the states a person actually meets on a panel
// that reads somebody else's system.
//
// The happy path is the least interesting one. What has to be right is the
// degraded family: no token, a rate limit, an outage serving the last known
// data. Each of those is a different problem with a different fix, and a panel
// that renders them all as an empty box teaches people to stop opening it.

function wrapper({ children }: { children: ReactNode }) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const REPO = {
	provider: "github",
	host: "github.com",
	repo: "DarkaMX/MEDUSASASBACK",
	url: "https://github.com/DarkaMX/MEDUSASASBACK",
	originUrl: "git@github-nuevo:DarkaMX/MEDUSASASBACK.git",
	aliasResolved: true,
	defaultBranch: "main",
	currentBranch: "feat/thing",
	headSha: "aaaaaaaaaaaaaaaa",
	remoteHeadSha: "bbbbbbbbbbbbbbbb",
	remoteHeadRef: "feat/thing",
	upstreamRef: "origin/feat/thing",
	ahead: 2,
	behind: 1,
	dirty: true,
	recentCommits: [{ sha: "aaaaaaaaaaaa", subject: "do the thing", author: "ana" }],
};

const CURRENT_PR = {
	number: 12,
	title: "Do the thing",
	url: "https://github.com/DarkaMX/MEDUSASASBACK/pull/12",
	state: "open",
	baseBranch: "main",
	headBranch: "feat/thing",
	reviewDecision: "changes_requested",
	checksSummary: "failing",
	checksPassed: 3,
	checksFailed: 1,
	checksPending: 0,
	failingChecks: ["backend-tests"],
	mergeable: "blocked",
	additions: 40,
	deletions: 5,
	changedFiles: 3,
};

function ready(overrides: Record<string, unknown> = {}) {
	return {
		data: {
			projectId: "proj-1",
			availability: "ready",
			authenticated: true,
			repository: REPO,
			pullRequests: [CURRENT_PR, { number: 44, title: "Other work", url: "u44", state: "open", headBranch: "other" }],
			currentPr: CURRENT_PR,
			issues: [
				{
					number: 7,
					title: "Login is broken",
					state: "open",
					url: "https://github.com/DarkaMX/MEDUSASASBACK/issues/7",
					labels: ["bug"],
					milestone: "v2",
					assignees: ["ana"],
					linkedPrs: [{ number: 3, title: "Fix login", state: "open" }],
				},
			],
			observedAt: "2026-01-02T03:04:05Z",
			...overrides,
		},
	};
}

describe("IntelligenceGitHub", () => {
	beforeEach(() => {
		apiGET.mockReset();
		setApiBaseUrl("http://127.0.0.1:4319");
	});

	it("shows the local checkout beside the server, without merging them", async () => {
		apiGET.mockResolvedValue(ready());
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });

		expect(await screen.findByText("DarkaMX/MEDUSASASBACK")).toBeInTheDocument();
		// The local half.
		expect(screen.getByText("feat/thing")).toBeInTheDocument();
		expect(screen.getByTestId("github-sync-state")).toHaveTextContent(
			"2 ahead, 1 behind origin/feat/thing",
		);
		expect(screen.getByTestId("github-dirty")).toBeInTheDocument();
		// The server's tip, named as the server's rather than folded into the
		// local numbers.
		expect(screen.getByText("bbbbbbbbbbbb")).toBeInTheDocument();
		expect(screen.getByText("aaaaaaaaaaaa")).toBeInTheDocument();
	});

	it("surfaces the branch's own pull request with its review and check state", async () => {
		apiGET.mockResolvedValue(ready());
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });

		const current = await screen.findByTestId("github-current-pr");
		expect(current).toHaveTextContent("#12");
		expect(current).toHaveTextContent("changes_requested");
		expect(current).toHaveTextContent("Checks failing · 3 passed, 1 failed");
		expect(screen.getByTestId("github-failing-checks")).toHaveTextContent("backend-tests");
		// Other people's pull requests are listed, but not as the highlighted one.
		expect(screen.getAllByTestId("github-pull-request")).toHaveLength(1);
	});

	it("names the SSH alias that made the repository recognizable", async () => {
		apiGET.mockResolvedValue(ready());
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });
		expect(await screen.findByTestId("github-alias-resolved")).toBeInTheDocument();
		expect(screen.getByText("git@github-nuevo:DarkaMX/MEDUSASASBACK.git")).toBeInTheDocument();
	});

	it("opens GitHub in the system browser rather than inside AO", async () => {
		const openExternal = vi.spyOn(aoBridge.app, "openExternal").mockResolvedValue(undefined);
		apiGET.mockResolvedValue(ready());
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });

		await userEvent.click(await screen.findByRole("button", { name: "Open pull request #12 on GitHub" }));
		expect(openExternal).toHaveBeenCalledWith("https://github.com/DarkaMX/MEDUSASASBACK/pull/12");

		await userEvent.click(screen.getByRole("button", { name: "Open issue #7 on GitHub" }));
		expect(openExternal).toHaveBeenCalledWith("https://github.com/DarkaMX/MEDUSASASBACK/issues/7");
		openExternal.mockRestore();
	});

	it("shows issue context a planner uses: milestone, labels and GitHub's own links", async () => {
		apiGET.mockResolvedValue(ready());
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });
		const issue = await screen.findByTestId("github-issue");
		expect(issue).toHaveTextContent("Login is broken");
		expect(issue).toHaveTextContent("v2");
		expect(issue).toHaveTextContent("bug");
		expect(screen.getByTestId("github-linked-prs")).toHaveTextContent("#3");
	});

	// The degraded family. Each says what happened and keeps what is still true.
	it("renders the no-token state without losing the local half", async () => {
		apiGET.mockResolvedValue({
			data: {
				projectId: "proj-1",
				availability: "degraded",
				authenticated: false,
				reason: "NO_CREDENTIALS",
				detail: "No GitHub token is configured, so only local repository state is available.",
				repository: { ...REPO, defaultBranch: "", remoteHeadSha: "", remoteHeadRef: "" },
			},
		});
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });

		expect(await screen.findByTestId("github-unauthenticated")).toBeInTheDocument();
		expect(screen.getByTestId("github-availability")).toHaveTextContent("Partial");
		expect(screen.getByTestId("github-degraded")).toHaveTextContent("No GitHub token is configured");
		// The local half needs no token and must survive.
		expect(screen.getByTestId("github-sync-state")).toHaveTextContent("2 ahead, 1 behind");
		expect(screen.getByTestId("github-repository")).toBeInTheDocument();
	});

	it("labels a snapshot served through an outage as the last known one", async () => {
		apiGET.mockResolvedValue(
			ready({ availability: "degraded", stale: true, reason: "GITHUB_UNREACHABLE", detail: "AO could not reach GitHub." }),
		);
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });
		expect(await screen.findByTestId("github-stale")).toHaveTextContent("Last known");
		expect(screen.getByTestId("github-degraded")).toHaveTextContent("AO could not reach GitHub.");
	});

	it("renders an origin AO does not recognize as unavailable, not as an error", async () => {
		apiGET.mockResolvedValue({
			data: {
				projectId: "proj-1",
				availability: "unavailable",
				authenticated: false,
				reason: "ORIGIN_NOT_GITHUB",
				detail: "This project's origin is not a GitHub repository AO recognizes.",
				repository: {},
			},
		});
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });

		expect(await screen.findByTestId("github-availability")).toHaveTextContent("Unavailable");
		expect(screen.getByTestId("github-degraded")).toHaveTextContent("is not a GitHub repository");
		// Not an error state: nothing is broken, the project simply is not on
		// GitHub, and the panel must not shout about it.
		expect(screen.queryByTestId("github-request-error")).not.toBeInTheDocument();
	});

	// A request that failed is NOT a GitHub outage — the daemon or the caller's
	// access is the problem, and it needs a different fix.
	it("separates a failed request from an unreachable GitHub", async () => {
		apiGET.mockResolvedValue({ error: { code: "PROJECT_NOT_FOUND", message: "project not found" } });
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });
		expect(await screen.findByTestId("github-request-error")).toBeInTheDocument();
		expect(screen.queryByTestId("github-availability")).not.toBeInTheDocument();
	});

	it("refreshes on demand, bypassing the snapshot cache", async () => {
		apiGET.mockResolvedValue(ready());
		render(<IntelligenceGitHub projectId="proj-1" />, { wrapper });
		await screen.findByTestId("github-repository");
		apiGET.mockClear();

		await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
		expect(apiGET).toHaveBeenCalledWith(
			"/api/v1/projects/{id}/github",
			expect.objectContaining({
				params: expect.objectContaining({ query: expect.objectContaining({ refresh: true }) }),
			}),
		);
	});
});
