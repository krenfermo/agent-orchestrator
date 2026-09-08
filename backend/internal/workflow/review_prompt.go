package workflow

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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

	// Depth is P5-A's review depth for this cycle. The zero value, `none` and
	// `deep` all produce the full independent review prompt AO has always
	// built, byte for byte; only `light` produces the bounded one. That is
	// deliberate: nothing that runs deep may have changed, and a depth AO
	// cannot read must not buy a cheaper review.
	Depth domain.ReviewDepth
	// RiskTier and RiskReasons are the deterministic risk classification this
	// cycle's depth was resolved from. Rendered only in the light prompt, where
	// they tell a bounded reviewer which parts of the change AO already knows
	// are the sensitive ones.
	RiskTier    domain.ReviewRiskTier
	RiskReasons []string
	// ChangedPaths and ChangedFileCount are AO's OWN observation of the
	// delivered change (ObserveWorkspace, via the persisted
	// review_policy_decision), not the worker's account of it. The list is
	// bounded by lightReviewMaxChangedPaths; the count is not, so a reader is
	// never told a smaller number than the truth.
	ChangedPaths     []string
	ChangedFileCount int
	// VerifyCommands and VerifyFileChecks are the deterministic verification AO
	// will run itself after this review. Telling the reviewer means it does not
	// spend a bounded pass proving what AO is about to prove.
	VerifyCommands   []string
	VerifyFileChecks []string
	// PriorWorkerAttempts is how many provider attempts the work step took. More
	// than one means a retry or a failover happened before the code reached
	// review, which is a thing a reviewer should know and cannot see in a diff.
	PriorWorkerAttempts int
}

// lightReviewMaxChangedPaths bounds the changed-path list rendered into a light
// prompt. A bounded review whose evidence pack is unbounded is not bounded; a
// change with more paths than this has already been classified
// large_or_multi_module by ReviewPolicy, so the tail adds risk information the
// tier has already carried.
const lightReviewMaxChangedPaths = 50

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
	if in.Depth == domain.ReviewDepthLight {
		return buildLightReviewPrompt(in)
	}
	return buildDeepReviewPrompt(in)
}

// buildDeepReviewPrompt is the review prompt AO has always built. Its text is
// unchanged from before review depth existed, and it must stay that way: a
// deep review is the control against which a light one is judged, so any drift
// here would make "master is unaffected" untrue.
func buildDeepReviewPrompt(in ReviewPromptInput) string {
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

// buildLightReviewPrompt is the bounded review pass.
//
// What makes it cheaper is not a shorter instruction list; it is a narrower
// question and a supplied evidence pack. A deep reviewer is asked to form its
// own picture of the change from the repository. A light reviewer is asked to
// judge a diff it is handed against criteria it is handed, using facts AO
// already observed, and to say so rather than guess when that is not enough.
//
// Three things it deliberately keeps from the deep prompt, unchanged:
//
//   - the read-only guardrails and the no-PR/no-gh prohibition;
//   - the plan-scope boundary (ReviewTaskScope), because judging one task of a
//     plan against the whole plan is a defect a bounded review would make more
//     often, not less;
//   - the `ao review submit` verdict protocol, so AO reads a light verdict back
//     through exactly the same durable path as any other.
//
// And one thing it adds: an escalation channel. A bounded reviewer that finds
// the change outside what it can honestly judge must have somewhere to say so
// that is cheaper than approving and safer than guessing.
func buildLightReviewPrompt(in ReviewPromptInput) string {
	var criteria string
	for _, c := range in.AcceptanceCriteria {
		criteria += "- " + c + "\n"
	}
	if criteria == "" {
		criteria = "- (none recorded)\n"
	}
	scope := reviewScopeSection(in)

	return fmt.Sprintf(`You are the automatic reviewer for one step of an AO-managed workflow run.

This is a BOUNDED review. AO classified this change as %s risk and asked for a
proportional pass, not a full audit. Judge the diff you are given against this
task's acceptance criteria, using the evidence below. Do not rebuild your own
picture of the repository.

Objective of the task you are reviewing: %s

Acceptance criteria for the work you are reviewing:
%s%s%s
Worker session under review: %s
Branch: %s
Worktree path (already your current checkout — do not clone or fetch elsewhere): %s
Base commit: %s
Reviewed workspace fingerprint: %s

The reviewed target above is AO's content-aware SHA-256 workspace fingerprint, not a
Git object id or a claim that a commit landed. The worker was instructed not to commit,
so dirty/staged/untracked changes with Git HEAD still at the base commit are expected
and are not themselves a review problem.

%s
What is evidence here:
- The diff is evidence. Read it with git status and git diff, directly in this
  worktree.
- AO's own observations above are evidence.
- The worker's account of what it did is NOT in front of you and is NOT evidence.
  Nothing you approve may rest on it.

Scope discipline for this bounded pass (follow all of these):
- Read the changed files and only what the diff genuinely requires you to open to
  judge it. Do NOT survey the repository, and do NOT audit code this change did not
  touch.
- Do NOT run a full test suite or a repository-wide build. AO runs the verification
  listed above itself, immediately after this review, and a failure there stops the
  run — you do not need to prove what AO is about to prove.
- You MAY run one narrow, obviously-available read-only check covering the changed
  code if the diff's correctness genuinely turns on it. Do not install dependencies
  and do not modify anything to make a check pass.

Review-only guardrails (follow all of these):
- Do NOT modify any file in this worktree.
- Do NOT stage, commit, push, merge, rebase, or switch branches.
- Do NOT open, create, or otherwise interact with a pull request, and do NOT run the
  gh command.
- Do NOT diff against any pull request's base branch — there is no pull request for
  this review.

Judge, and judge ONLY against this task's own objective and acceptance criteria:
- Whether the diff actually satisfies them.
- Regressions, functional errors, or clear risks (security, data loss, breaking
  behavior) visible in the diff.
- Out-of-scope changes: files touched that have nothing to do with the objective.
- Whether the change is consistent with the conventions visible in the files it edits.

Verdict rule:
- changes_requested is for this task's OWN acceptance criteria being unmet, or for the
  change as delivered being incorrect, unsafe, or breaking existing behavior. Judge
  those as strictly as you always would — a bounded review is a narrower question, not
  a lower bar.
- Anything whose implementation belongs to another planned task is NOT a reason to
  request changes. Record it as future-scope context instead and approve.
- If you cannot judge this change honestly within the bounds above — it turns out to
  touch security, authentication, payments, migrations, infrastructure or a public
  contract, it is far larger than the evidence above describes, or the evidence you
  were given is not enough to decide — do NOT approve and do NOT guess. Escalate:
  submit changes_requested with a body whose FIRST LINE is exactly

  %s <one line saying what a deeper review needs to look at>

  followed by whatever you did establish. AO reads that marker and runs the next
  cycle as a full independent review.

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
		lightRiskLabel(in.RiskTier),
		in.Objective, criteria, in.EffectiveSpec, scope,
		in.WorkerSessionID, in.Branch, in.WorktreePath, in.BaseSHA, in.HeadSHA,
		lightEvidenceSection(in),
		domain.ReviewEscalationMarker,
		in.WorkerSessionID, in.ReviewRunID,
		in.WorkerSessionID, in.ReviewRunID,
	)
}

// lightRiskLabel names the tier in the one word the prompt's first paragraph
// needs. An unrecorded tier reads as "unclassified" rather than as "low": a
// reviewer told a change is low risk when AO does not know that is a reviewer
// AO misled.
func lightRiskLabel(tier domain.ReviewRiskTier) string {
	if !tier.Valid() {
		return "unclassified"
	}
	return string(tier)
}

// lightEvidenceSection renders the pack of AO-observed facts a bounded review
// is judged against. Every line is something AO holds durably; nothing here is
// the worker's account of its own work.
func lightEvidenceSection(in ReviewPromptInput) string {
	var b strings.Builder
	b.WriteString("What AO already observed about this change (its own facts, not the worker's report):\n")

	if in.ChangedFileCount > 0 || len(in.ChangedPaths) > 0 {
		b.WriteString("\nChanged files (" + strconv.Itoa(in.ChangedFileCount) + " observed in the worktree):\n")
		shown := in.ChangedPaths
		if len(shown) > lightReviewMaxChangedPaths {
			shown = shown[:lightReviewMaxChangedPaths]
		}
		for _, p := range shown {
			b.WriteString("- " + p + "\n")
		}
		if len(in.ChangedPaths) > len(shown) {
			b.WriteString("- … and " + strconv.Itoa(len(in.ChangedPaths)-len(shown)) +
				" more; git status in this worktree is the complete list.\n")
		}
	} else {
		b.WriteString("\nChanged files: AO could not observe a changed-file list for this target. Treat\n" +
			"git status as the only source and be correspondingly careful.\n")
	}

	if len(in.RiskReasons) > 0 {
		b.WriteString("\nWhy AO classified this change the way it did (deterministic, from the paths and\nthe plan — not a judgement about quality):\n")
		for _, r := range in.RiskReasons {
			b.WriteString("- " + r + "\n")
		}
	}

	if len(in.VerifyCommands) > 0 || len(in.VerifyFileChecks) > 0 {
		b.WriteString("\nVerification AO will run itself after this review (you do not need to run it):\n")
		for _, cmd := range in.VerifyCommands {
			b.WriteString("- command: " + cmd + "\n")
		}
		for _, f := range in.VerifyFileChecks {
			b.WriteString("- file check: " + f + "\n")
		}
	} else {
		b.WriteString("\nVerification AO will run itself after this review: none is declared for this task.\n" +
			"Nothing downstream will catch what you do not, so weigh that in your verdict.\n")
	}

	if in.PriorWorkerAttempts > 1 {
		b.WriteString("\nThe work step took " + strconv.Itoa(in.PriorWorkerAttempts) +
			" provider attempts before reaching review: a retry or a provider failover\nhappened. Partially-applied work from an abandoned attempt is worth looking for.\n")
	}

	return b.String()
}
