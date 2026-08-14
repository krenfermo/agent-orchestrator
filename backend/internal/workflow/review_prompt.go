package workflow

import "fmt"

// ReviewPromptInput is everything BuildReviewPrompt needs to build the
// workflow-owned review task text. It deliberately carries only plain values
// (no domain/ports types) so the function stays pure and trivially testable,
// mirroring plan.go's BuildWorkStepPrompt.
type ReviewPromptInput struct {
	Objective          string
	AcceptanceCriteria []string
	WorkerSessionID    string
	Branch             string
	WorktreePath       string
	BaseSHA            string
	HeadSHA            string
	ReviewRunID        string
}

// BuildReviewPrompt deterministically builds the text handed to the real
// Claude Code reviewer process for a workflow-triggered review pass
// (Checkpoint 8C). Pure and deterministic: no IO, no model call.
//
// This is workflow's OWN prompt — it does not reuse internal/review's shared
// prompt builder (review/prompt.go's reviewTexts). That builder unconditionally
// instructs the reviewer to post a review to a GitHub pull request via
// `gh api .../pulls/{number}/reviews` and diff against the PR's base branch.
// Checkpoint 8B's worker guardrail prompt (plan.go's BuildWorkStepPrompt)
// explicitly forbids the worker from pushing, merging, or opening a PR, so a
// completed work step commonly has no PR and sometimes no new commit at all —
// just a dirty/untracked worktree. Reusing the PR-centric prompt here would
// either be nonsensical (empty PR URL) or require AO to silently open/push a
// PR itself, which Checkpoint 8C explicitly forbids. So this function
// instructs the reviewer to inspect the live worktree via `git status`/
// `git diff` and submit its verdict with the single-run `ao review submit`
// form instead — everything else (the read-only tool allowlist, the actual
// reviewer process/binary) still comes from the unmodified Claude Code
// reviewer adapter (adapters/reviewer/claudecode/claudecode.go).
func BuildReviewPrompt(in ReviewPromptInput) string {
	var criteria string
	for _, c := range in.AcceptanceCriteria {
		criteria += "- " + c + "\n"
	}
	if criteria == "" {
		criteria = "- (none recorded)\n"
	}

	commitNote := "The worker was instructed not to commit, so most likely the change is " +
		"still uncommitted in the worktree (dirty/staged/untracked files) and HEAD may be " +
		"unchanged from the base commit. That is expected and not itself a problem."
	if in.HeadSHA != "" && in.HeadSHA != in.BaseSHA {
		commitNote = fmt.Sprintf(
			"A commit did land on this branch (HEAD moved from %s to %s). As a secondary check, "+
				"also run `git diff %s..%s` to see the committed diff, in addition to `git status`/"+
				"`git diff` for any further uncommitted changes on top of it.",
			in.BaseSHA, in.HeadSHA, in.BaseSHA, in.HeadSHA)
	}

	return fmt.Sprintf(`You are the automatic reviewer for one step of an AO-managed workflow run.

Objective of the overall run: %s

Acceptance criteria for the work you are reviewing:
%s
Worker session under review: %s
Branch: %s
Worktree path (already your current checkout — do not clone or fetch elsewhere): %s
Base commit: %s
Head commit: %s

%s

Inspect the change by running, directly in this worktree:
- git status
- git diff

Do NOT diff against any pull request's base branch — there is no pull request for this
review. Do NOT run the gh command or interact with any PR in any way.

Review-only guardrails (follow all of these):
- Do NOT modify any file in this worktree.
- Do NOT stage, commit, push, merge, rebase, or switch branches.
- Do NOT open, create, or otherwise interact with a pull request.

Evaluate:
- Whether the change actually satisfies the objective and acceptance criteria above.
- Regressions or functional errors introduced by the change.
- Whether relevant tests exist/pass for the change (read-only: you may run a read-only
  test command if one is obviously available, but do not install dependencies or modify
  anything to make tests pass).
- Inconsistencies with the existing codebase's architecture/conventions.
- Any out-of-scope changes (files touched that have nothing to do with the objective).
- Any clear risk (security, data loss, breaking behavior) even if the stated objective is met.

When you are done, submit your verdict with exactly one of the following commands (do not
use any other review-submission form):

  ao review submit %s --run %s --verdict approved

or, if the change needs work before it is acceptable:

  ao review submit %s --run %s --verdict changes_requested --body <path>

Where <path> is a path to a file containing your findings, or - to pipe them in on stdin
(never write your findings into a file inside the worktree). --body is required for
changes_requested and must contain your findings (what's wrong and what should change).
This is the ONLY way to record your verdict — AO reads it back from this review run, not
from anything else you output.`,
		in.Objective, criteria, in.WorkerSessionID, in.Branch, in.WorktreePath,
		in.BaseSHA, in.HeadSHA, commitNote,
		in.WorkerSessionID, in.ReviewRunID,
		in.WorkerSessionID, in.ReviewRunID,
	)
}
