package workflow

import (
	"fmt"
	"strings"
)

// ReviewPromptInput is everything BuildReviewPrompt needs to build the
// workflow-owned review task text. It deliberately carries only plain values
// (no domain/ports types) so the function stays pure and trivially testable,
// mirroring plan.go's BuildWorkStepPrompt.
type ReviewPromptInput struct {
	Objective          string
	AcceptanceCriteria []string
	// EffectiveSpec is RenderEffectiveSpecification's output: the approved
	// amendments that reconcile the original objective with the criteria in
	// force. Empty when the task has none.
	EffectiveSpec   string
	WorkerSessionID string
	Branch          string
	WorktreePath    string
	BaseSHA         string
	HeadSHA         string
	ReviewRunID     string
	// AvailableDependencies names the plan's already-delivered tasks — the work
	// this reviewer may assume exists.
	AvailableDependencies []string
	// FuturePlannedTasks names the plan's tasks that are NOT this one and have
	// not been delivered yet. Their absence from the worktree is expected, and
	// telling the reviewer so is the whole point: see ReviewTaskScope.
	FuturePlannedTasks []string
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
	scope := reviewScopeSection(in)

	commitNote := "The reviewed target above is AO's content-aware SHA-256 workspace " +
		"fingerprint, not a Git object id or a claim that a commit landed. The worker was " +
		"instructed not to commit, so dirty/staged/untracked changes with Git HEAD still at " +
		"the base commit are expected and are not themselves a review problem."

	return fmt.Sprintf(`You are the automatic reviewer for one step of an AO-managed workflow run.

Objective of the task you are reviewing: %s

Acceptance criteria for the work you are reviewing:
%s%s%s
Worker session under review: %s
Branch: %s
Worktree path (already your current checkout — do not clone or fetch elsewhere): %s
Base commit: %s
Reviewed workspace fingerprint: %s

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

Evaluate, and judge ONLY against this task's own objective and acceptance criteria above:
- Whether the change actually satisfies this task's objective and acceptance criteria.
- Regressions or functional errors introduced by the change.
- Whether relevant tests exist/pass for the change (read-only: you may run a read-only
  test command if one is obviously available, but do not install dependencies or modify
  anything to make tests pass).
- Inconsistencies with the existing codebase's architecture/conventions.
- Any out-of-scope changes (files touched that have nothing to do with the objective).
- Any clear risk (security, data loss, breaking behavior) even if the stated objective is met.

Verdict rule:
- changes_requested is for this task's OWN acceptance criteria being unmet, or for the
  change as delivered being incorrect, unsafe, or breaking existing behavior. Judge those
  as strictly as you always would — none of the above lowers that bar.
- Anything whose implementation belongs to another planned task is NOT a reason to
  request changes. Record it as future-scope context instead (see above) and approve.

When you are done, submit your verdict with exactly one of the following commands (do not
use any other review-submission form):

  ao review submit %s --run %s --verdict approved [--body <path>]

or, if the change needs work before it is acceptable:

  ao review submit %s --run %s --verdict changes_requested --body <path>

Where <path> is a path to a file containing your findings, or - to pipe them in on stdin
(never write your findings into a file inside the worktree). --body is required for
changes_requested and must contain your findings (what's wrong and what should change).
On an approval --body is optional and is where your non-blocking notes go.
This is the ONLY way to record your verdict — AO reads it back from this review run, not
from anything else you output.`,
		in.Objective, criteria, in.EffectiveSpec, scope, in.WorkerSessionID, in.Branch, in.WorktreePath,
		in.BaseSHA, in.HeadSHA, commitNote,
		in.WorkerSessionID, in.ReviewRunID,
		in.WorkerSessionID, in.ReviewRunID,
	)
}

// reviewScopeSection renders the plan-scope boundary, and renders nothing at
// all when there is no plan context to state. A standalone run has no siblings,
// and inventing an empty "future tasks: none" section for it would only invite
// the reviewer to reason about a plan that does not exist.
func reviewScopeSection(in ReviewPromptInput) string {
	if len(in.AvailableDependencies) == 0 && len(in.FuturePlannedTasks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nScope of this review — you are reviewing ONE task of a larger plan:\n")
	if len(in.AvailableDependencies) > 0 {
		b.WriteString("\nAlready delivered by earlier tasks (you may assume this exists):\n")
		for _, d := range in.AvailableDependencies {
			b.WriteString("- " + d + "\n")
		}
	}
	if len(in.FuturePlannedTasks) > 0 {
		b.WriteString("\nAssigned to OTHER planned tasks that have not run yet. This work is deliberately\n" +
			"absent from the worktree, is NOT part of the task you are reviewing, and its absence\n" +
			"is never a defect in this task:\n")
		for _, t := range in.FuturePlannedTasks {
			b.WriteString("- " + t + "\n")
		}
		b.WriteString("\nIf you find something that belongs to one of those tasks — for example a new component\n" +
			"that is not yet called from the code that will eventually call it — do NOT return\n" +
			"changes_requested for it. Approve the task and record the observation as non-blocking\n" +
			"under a \"Future-scope notes (non-blocking)\" heading in an optional --body file on the\n" +
			"approval. AO carries those notes forward to the task that owns the work.\n")
	}
	return b.String()
}
