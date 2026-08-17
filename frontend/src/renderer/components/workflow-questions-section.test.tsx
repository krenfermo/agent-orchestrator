import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { WorkflowQuestionsSection } from "./workflow-questions-section";
import type { components } from "../../api/schema";

type WorkflowQuestionResponse = components["schemas"]["WorkflowQuestionResponse"];

function baseQuestion(overrides: Partial<WorkflowQuestionResponse>): WorkflowQuestionResponse {
	return {
		id: "q-1",
		workflowRunId: "wf-1",
		questionText: "Should I push directly to main?",
		certainty: "inferred",
		classification: "human_required",
		state: "human_required",
		createdAt: new Date().toISOString(),
		delivered: false,
		...overrides,
	};
}

describe("WorkflowQuestionsSection", () => {
	it("renders nothing when there are no questions", () => {
		const { container } = render(<WorkflowQuestionsSection questions={[]} onAnswer={vi.fn()} answering={false} />);
		expect(container).toBeEmptyDOMElement();
	});

	it("renders a classification badge and choice buttons for a normal question", () => {
		const question = baseQuestion({
			structuredChoices: [
				{ id: "yes", label: "Yes" },
				{ id: "no", label: "No" },
			],
		});
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={vi.fn()} answering={false} />);

		expect(screen.getByText("Should I push directly to main?")).toBeInTheDocument();
		expect(screen.getAllByText("Needs your input").length).toBeGreaterThan(0);
		expect(screen.getByRole("button", { name: "Yes" })).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "No" })).toBeInTheDocument();
	});

	it("renders the exact fallback string when question text could not be reconstructed reliably", () => {
		const question = baseQuestion({ questionText: "", certainty: "unknown", classification: "human_required" });
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={vi.fn()} answering={false} />);

		expect(
			screen.getByText("Agent is waiting for input. The question text could not be reconstructed reliably."),
		).toBeInTheDocument();
	});

	it("calls the submit mutation with the selected choice on click", async () => {
		const onAnswer = vi.fn().mockResolvedValue(undefined);
		const question = baseQuestion({
			structuredChoices: [{ id: "yes", label: "Yes" }],
		});
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={onAnswer} answering={false} />);

		fireEvent.click(screen.getByRole("button", { name: "Yes" }));

		expect(onAnswer).toHaveBeenCalledWith({ questionId: "q-1", choiceId: "yes" });
	});

	it("does not render answer controls for an already-answered question", () => {
		const question = baseQuestion({
			state: "answered",
			classification: "policy_resolvable",
			answerText: "No — use branch feature/x.",
			answerSource: "policy",
			structuredChoices: [{ id: "yes", label: "Yes" }],
		});
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={vi.fn()} answering={false} />);

		expect(screen.queryByRole("button", { name: "Yes" })).not.toBeInTheDocument();
		expect(screen.getByText("No — use branch feature/x.")).toBeInTheDocument();
	});

	it("renders the auto_resolvable classification badge", () => {
		const question = baseQuestion({ classification: "auto_resolvable", state: "resolving" });
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={vi.fn()} answering={false} />);

		expect(screen.getByText("Auto-resolvable")).toBeInTheDocument();
	});

	it("renders 'Resolving' when a resolver attempt is dispatched (state=resolving with a resolverHarness)", () => {
		const question = baseQuestion({
			classification: "auto_resolvable",
			state: "resolving",
			resolverHarness: "codex",
			resolverProvider: "openai",
		});
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={vi.fn()} answering={false} />);

		expect(screen.getByText("Resolving")).toBeInTheDocument();
	});

	it("renders 'Waiting for capacity' when resolving with no resolver dispatched yet", () => {
		const question = baseQuestion({ classification: "auto_resolvable", state: "resolving" });
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={vi.fn()} answering={false} />);

		expect(screen.getByText("Waiting for capacity")).toBeInTheDocument();
	});

	it("renders the resolver advisory in a visually distinct block, never as a delivered answer", () => {
		const question = baseQuestion({
			classification: "auto_resolvable",
			state: "human_required",
			resolverHarness: "codex",
			resolverProvider: "openai",
			resolverReasonSummary: "could not verify against the test suite",
			resolverAdvisoryAnswer: "Probably 8s, but unverified.",
		});
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={vi.fn()} answering={false} />);

		// The advisory disclaimer and text render...
		expect(screen.getByText("Resolver could not determine safely")).toBeInTheDocument();
		expect(screen.getByText("Probably 8s, but unverified.")).toBeInTheDocument();
		// ...but this question was never actually answered, so no "Answered"
		// block (and no answerText) exists anywhere in the DOM.
		expect(screen.queryByText("Answered")).not.toBeInTheDocument();
	});

	it("does not render the advisory block for a question the resolver answered successfully (requiresHuman=false)", () => {
		const question = baseQuestion({
			classification: "auto_resolvable",
			state: "answered",
			answerSource: "resolver",
			answerText: "Use the existing formatHelper in internal/foo.",
			resolverHarness: "codex",
			resolverProvider: "openai",
		});
		render(<WorkflowQuestionsSection questions={[question]} onAnswer={vi.fn()} answering={false} />);

		// "Answered" appears twice by design (the state badge and the answer
		// block's own heading) — assert both, plus the delivered answer text.
		expect(screen.getAllByText("Answered").length).toBe(2);
		expect(screen.getByText("Use the existing formatHelper in internal/foo.")).toBeInTheDocument();
		expect(screen.queryByText("Resolver could not determine safely")).not.toBeInTheDocument();
	});
});
