import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { WorkflowQuestionsSection } from "./workflow-questions-section";
import type { components } from "../../api/schema";

type WorkflowQuestionResponse = components["schemas"]["WorkflowQuestionResponse"];

/**
 * P3-C §14: the UI must not ask the user while AO is deciding for itself, and
 * an answer AO decided must never read like an answer the user gave.
 *
 * Both are assertions about what a person SEES, which is why they live here
 * rather than beside the backend projection that computes them.
 */

function question(overrides: Partial<WorkflowQuestionResponse>): WorkflowQuestionResponse {
	return {
		id: "q-1",
		workflowRunId: "wf-1",
		questionText: "Should I extract this into a helper in utils.go or inline it here?",
		certainty: "actual",
		classification: "auto_resolvable",
		state: "resolving",
		createdAt: new Date().toISOString(),
		delivered: false,
		...overrides,
	};
}

describe("P3-C autonomous question UX", () => {
	it("offers no answer form while AO is resolving a low-risk decision", () => {
		const onAnswer = vi.fn();
		render(
			<WorkflowQuestionsSection
				questions={[
					question({
						autonomyMode: "auto_decide_low_risk",
						resolverHarness: "codex",
						structuredChoices: [
							{ id: "extract", label: "Extract into utils.go" },
							{ id: "inline", label: "Inline it here" },
						],
					}),
				]}
				onAnswer={onAnswer}
				answering={false}
			/>,
		);

		// The question is visible — a person may always SEE what AO is deciding.
		expect(screen.getByText(/extract this into a helper/i)).toBeInTheDocument();
		// But nothing invites them to answer it: no choice buttons, no free-text
		// submit. Offering either would be asking a question AO is already
		// answering, which is the interview P3-C exists to end.
		expect(screen.queryByRole("button", { name: "Extract into utils.go" })).not.toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Inline it here" })).not.toBeInTheDocument();
		expect(onAnswer).not.toHaveBeenCalled();
	});

	it("labels an autonomous answer as AO's decision, not as a bare source string", () => {
		render(
			<WorkflowQuestionsSection
				questions={[
					question({
						state: "answered",
						answerSource: "autonomous",
						answerText: "Extract into utils.go",
						autonomyMode: "auto_decide_low_risk",
					}),
				]}
				onAnswer={vi.fn()}
				answering={false}
			/>,
		);

		expect(screen.getByText("Extract into utils.go")).toBeInTheDocument();
		expect(screen.getByText(/AO decided this automatically/i)).toBeInTheDocument();
		// And never the generic "Source: x" line a human/policy answer gets:
		// the two carry different authority and must not read alike.
		expect(screen.queryByText(/^Source: /)).not.toBeInTheDocument();
	});

	it("still keeps the generic source line for a human answer", () => {
		render(
			<WorkflowQuestionsSection
				questions={[
					question({
						state: "answered",
						answerSource: "human",
						answerText: "Inline it here",
						classification: "human_required",
					}),
				]}
				onAnswer={vi.fn()}
				answering={false}
			/>,
		);

		expect(screen.getByText("Source: human")).toBeInTheDocument();
		expect(screen.queryByText(/AO decided this automatically/i)).not.toBeInTheDocument();
	});

	it("still asks the user when the question is genuinely theirs", () => {
		render(
			<WorkflowQuestionsSection
				questions={[
					question({
						questionText: "Should I delete the production database rows first?",
						classification: "human_required",
						state: "human_required",
						structuredChoices: [
							{ id: "yes", label: "Yes" },
							{ id: "no", label: "No" },
						],
					}),
				]}
				onAnswer={vi.fn()}
				answering={false}
			/>,
		);

		expect(screen.getByRole("button", { name: "Yes" })).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "No" })).toBeInTheDocument();
	});
});

describe("P3-C decision delivery UX", () => {
	it("distinguishes a decision AO has taken from one the agent has received", () => {
		render(
			<WorkflowQuestionsSection
				questions={[
					question({
						state: "answered",
						answerSource: "autonomous",
						answerText: "pathutil.go",
						autonomyMode: "auto_decide_low_risk",
						delivered: false,
					}),
				]}
				onAnswer={vi.fn()}
				answering={false}
			/>,
		);

		// The decision exists and the agent does not have it yet. Both halves
		// matter: the first is what AO promises, the second is what unblocks the
		// task, and collapsing them into one "Answered" hides the wait.
		expect(screen.getByText(/AO is sending this to the agent/i)).toBeInTheDocument();
		expect(screen.getByText("pathutil.go")).toBeInTheDocument();
	});

	it("reads as plainly answered once the agent has received it", () => {
		render(
			<WorkflowQuestionsSection
				questions={[
					question({
						state: "answered",
						answerSource: "autonomous",
						answerText: "pathutil.go",
						autonomyMode: "auto_decide_low_risk",
						delivered: true,
					}),
				]}
				onAnswer={vi.fn()}
				answering={false}
			/>,
		);

		expect(screen.queryByText(/AO is sending this to the agent/i)).not.toBeInTheDocument();
		// "Answered" appears twice — the state badge and the answer block header —
		// and both are correct once the agent has it.
		expect(screen.getAllByText("Answered").length).toBeGreaterThan(0);
	});
});
