import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { PendingDecisionsRoute } from "./_shell.decisions";

const { usePendingDecisionsMock } = vi.hoisted(() => ({
	usePendingDecisionsMock: vi.fn(),
}));

vi.mock("@tanstack/react-router", async (importOriginal) => {
	const actual = await importOriginal<typeof import("@tanstack/react-router")>();
	return {
		...actual,
		Link: ({ children, params }: { children: React.ReactNode; params?: { workflowId?: string } }) => (
			<a href={`/workflows/${params?.workflowId ?? ""}`}>{children}</a>
		),
	};
});

vi.mock("../hooks/usePendingDecisions", () => ({
	usePendingDecisions: usePendingDecisionsMock,
}));

function baseQuestion(overrides: Partial<Record<string, unknown>> = {}) {
	return {
		id: "q-1",
		workflowRunId: "wf-1",
		questionText: "Which retry backoff should the fix worker use?",
		certainty: "inferred",
		classification: "human_required",
		state: "human_required",
		createdAt: new Date().toISOString(),
		delivered: false,
		...overrides,
	};
}

describe("PendingDecisionsRoute", () => {
	it("renders pending items spanning multiple runs, each linking to its own run", () => {
		usePendingDecisionsMock.mockReturnValue({
			questions: [
				baseQuestion({
					id: "q-1",
					workflowRunId: "wf-1",
					questionText: "Question from run 1",
					createdAt: "2026-08-16T10:00:00.000Z",
				}),
				baseQuestion({
					id: "q-2",
					workflowRunId: "wf-2",
					questionText: "Question from run 2",
					state: "resolving",
					classification: "auto_resolvable",
					createdAt: "2026-08-16T11:00:00.000Z",
				}),
			],
			isLoading: false,
			error: undefined,
			answerQuestion: vi.fn(),
			answeringQuestion: false,
		});

		render(<PendingDecisionsRoute />);

		expect(screen.getByText("Question from run 1")).toBeInTheDocument();
		expect(screen.getByText("Question from run 2")).toBeInTheDocument();
		const links = screen.getAllByRole("link", { name: /view run/i });
		expect(links).toHaveLength(2);
		expect(links[0]).toHaveAttribute("href", expect.stringContaining("wf-2")); // most-recent-first sort
	});

	it("shows the empty state when there are no pending decisions", () => {
		usePendingDecisionsMock.mockReturnValue({
			questions: [],
			isLoading: false,
			error: undefined,
			answerQuestion: vi.fn(),
			answeringQuestion: false,
		});

		render(<PendingDecisionsRoute />);

		expect(screen.getByText(/no pending decisions/i)).toBeInTheDocument();
	});

	it("never renders a wide table — each question is a single stacked card", () => {
		usePendingDecisionsMock.mockReturnValue({
			questions: [baseQuestion()],
			isLoading: false,
			error: undefined,
			answerQuestion: vi.fn(),
			answeringQuestion: false,
		});

		const { container } = render(<PendingDecisionsRoute />);

		expect(container.querySelector("table")).not.toBeInTheDocument();
	});
});
