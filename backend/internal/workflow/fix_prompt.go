package workflow

import "fmt"

// FixPromptInput is everything BuildFixPrompt needs to build the workflow-
// owned fix task text. Pure values only (no domain/ports types), mirroring
// plan.go's BuildWorkStepPrompt and review_prompt.go's ReviewPromptInput.
type FixPromptInput struct {
	Objective          string
	AcceptanceCriteria []string
	// EffectiveSpec is RenderEffectiveSpecification's output: the approved
	// amendments that reconcile the original objective with the criteria in
	// force. Empty when the task has none, which renders the identical prompt
	// this builder produced before amendments existed.
	EffectiveSpec string
	ReviewRunID   string
	// Findings is the review_run's own Body text, fetched live from the
	// ReviewRuns port at dispatch time. It is referenced here, never copied
	// into any workflow table — workflow_checkpoints only ever store the
	// review_run_id reference (see fix_dispatch.go).
	Findings    string
	CycleNumber int
}

// BuildFixPrompt deterministically builds the text delivered to the SAME
// Codex worker session that did the original work (Checkpoint 8D §5). Pure
// and deterministic: no IO, no model call. Delivered via MessageSender.Send
// into the existing session, never a new Spawn.
func BuildFixPrompt(in FixPromptInput) string {
	var criteria string
	for _, c := range in.AcceptanceCriteria {
		criteria += "- " + c + "\n"
	}
	if criteria == "" {
		criteria = "- (none recorded)\n"
	}
	findings := in.Findings
	if findings == "" {
		findings = "(no findings text was recorded on the review run)"
	}

	return fmt.Sprintf(`A reviewer requested changes on your work for this AO-managed workflow run (fix cycle %d).

Original objective: %s

Acceptance criteria:
%s%s
The reviewer's findings (review run %s), verbatim:
---
%s
---

Your task now: fix ONLY the issues raised in the findings above. Do not rework
parts of the change the reviewer did not flag — preserve already-correct
work. After making the fix, run any reasonable/available test suite for this
project before considering the fix done.

Guardrails (same as your original task, still in force):
- Work only inside the current worktree. Do not touch files outside it.
- Do NOT commit, push, merge, or modify any branch other than the current one.
- Do NOT open, request, or interact with any pull request.

When you are done (or if you get stuck), report the outcome clearly in your
final message: what you changed and what you tested. This report is
informational only — AO verifies your work independently from the actual
state of the worktree, not from what you say here, so be honest about
partial progress or failures.`, in.CycleNumber, in.Objective, criteria, in.EffectiveSpec, in.ReviewRunID, findings)
}
