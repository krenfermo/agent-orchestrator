package integration

import (
	"context"
	"strings"
	"time"
)

// Strategy is how one task's work was put onto the target branch. The order
// they are declared in is the order the coordinator prefers them: each one
// rewrites or invents strictly more history than the one above it, so the
// first applicable strategy is by construction the safest applicable one.
type Strategy string

const (
	// StrategyFastForward moves the target ref forward onto a commit that
	// already contains it. No commit is created, no history is rewritten, and
	// nothing can be lost -- which is why it is tried first and why it is the
	// only strategy that needs no re-verification.
	StrategyFastForward Strategy = "fast_forward"
	// StrategyRebaseFastForward replays the task's own commits onto the moved
	// target and then fast-forwards. The task branch is rewritten; the target
	// history stays linear and every task commit survives individually.
	StrategyRebaseFastForward Strategy = "rebase_fast_forward"
	// StrategyCherryPick replays the task's commits onto a detached checkout of
	// the target, leaving the task branch alone. It is what remains when the
	// task's history contains a merge, which rebase would silently flatten.
	StrategyCherryPick Strategy = "cherry_pick"
	// StrategyMergeCommit records both histories and invents a commit that has
	// never been verified as a whole by anything but this integration. It is
	// only ever chosen when Policy.AllowMergeCommit says so explicitly.
	StrategyMergeCommit Strategy = "merge_commit"
)

// Outcome is what happened to one integration attempt, and it has exactly
// three shapes:
//
//	Integrated                -> the target moved; Record describes how.
//	Attention != nil          -> a human has to act; the target did not move.
//	neither, with a nil error -> impossible; Integrate never returns it.
type Outcome struct {
	// Integrated is true only when the target branch actually moved (or was
	// already at the source, which is the no-op fast-forward).
	Integrated bool
	Record     Record
	// Attention is set when the attempt stopped on something only a person can
	// decide. It is deliberately NOT an error: an unresolvable conflict is a
	// normal outcome of integrating concurrent work, and it must not look like
	// a broken coordinator to the caller, nor stop any other task.
	Attention *Attention
}

// Record is the durable account of one integration attempt. Every field it
// carries is one a later reader cannot re-derive: the target SHAs are gone the
// moment the branch moves again, and the strategy is a decision rather than an
// observation.
type Record struct {
	TaskID        string
	WorkflowRunID string
	ProjectID     string
	RepoPath      string
	TargetBranch  string
	SourceBranch  string

	Strategy Strategy
	// SourceSHA is the commit that was integrated -- after any rebase, so it is
	// the commit whose content actually reached the target, not the one the
	// task last reported.
	SourceSHA string
	// TargetBeforeSHA and TargetAfterSHA bracket the ref update. They are equal
	// only for the no-op case where the target already contained the source.
	TargetBeforeSHA string
	TargetAfterSHA  string
	// BaseSHA is the target commit the task's work was built on. It differs
	// from TargetBeforeSHA exactly when the target moved while the task ran,
	// which is the condition that forces a replay and a re-verification.
	BaseSHA string

	// Replayed reports that the task's work had to be moved onto a target that
	// had advanced, and therefore that Verification describes a run against
	// content that did not exist when the task finished.
	Replayed bool
	// AutoResolvedPaths are the conflicted files the coordinator resolved by
	// itself, under the one deterministic rule in conflict.go. Empty for the
	// overwhelming majority of integrations.
	AutoResolvedPaths []string
	Verification      Verification

	Outcome      RecordOutcome
	Attention    *Attention
	IntegratedAt time.Time
}

// RecordOutcome is the one-word summary of a Record, so a ledger can be
// filtered without re-reading the rest of the row.
type RecordOutcome string

// The two outcomes an attempt can have. There is no "failed": an attempt that
// could not run at all returns an error and is not recorded, because nothing
// happened to the target branch for a ledger to describe.
const (
	OutcomeIntegrated     RecordOutcome = "integrated"
	OutcomeNeedsAttention RecordOutcome = "needs_attention"
)

// Verification is the result of re-running the task's verification after its
// work was replayed onto a target that had moved. Ran is false for a plain
// fast-forward: nothing changed, so the verification the task already passed
// still describes the exact content being integrated, and re-running it would
// be an expensive way to get the same answer.
type Verification struct {
	Ran    bool
	Passed bool
	// Summary is a short human-readable account of what ran and what failed.
	Summary string
}

// Verifier re-runs a task's verification against the worktree in its current
// state. It is a port rather than a direct call into internal/workflow so that
// this package stays importable BY the workflow coordinator; the workflow side
// supplies an adapter over the verification infrastructure it already owns.
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (Verification, error)
}

// VerifyRequest names the exact state to verify. HeadSHA is the replayed
// commit, so a verifier that caches by content has something to key on.
type VerifyRequest struct {
	TaskID        string
	WorkflowRunID string
	ProjectID     string
	WorktreePath  string
	HeadSHA       string
	// TargetSHA is what the work was replayed onto, which is the other half of
	// the content identity a cache would need.
	TargetSHA string
}

// Attention is the exact detail a person needs to finish an integration the
// coordinator would not finish by itself. Every field is populated on every
// attention outcome: an attention record that says "conflict" without naming
// the files or the commits is the failure mode this type exists to prevent.
type Attention struct {
	// Reason is a stable machine-checkable code.
	Reason AttentionReason
	// ConflictFiles are the exact repository-relative paths git reported as
	// unmerged, in git's order. Empty for reasons that are not conflicts.
	ConflictFiles []string
	// BaseSHA, TargetSHA and SourceSHA are the three commits that describe the
	// situation completely: what the task built on, where the target actually
	// is, and what is trying to land.
	BaseSHA   string
	TargetSHA string
	SourceSHA string
	Strategy  Strategy
	Detail    string
}

// AttentionReason is the canonical set of reasons an integration stops for a
// person.
type AttentionReason string

const (
	// ReasonMergeConflict means the replay hit a conflict that the automatic
	// rule refused to resolve, i.e. one where two changes genuinely overlap.
	ReasonMergeConflict AttentionReason = "integration_merge_conflict"
	// ReasonVerificationFailed means the replay succeeded but the task's own
	// verification no longer passes against the moved target. The work is
	// correct against the base it was written on and wrong against the target,
	// which is a fact only its author can act on.
	ReasonVerificationFailed AttentionReason = "integration_verification_failed"
	// ReasonNoApplicableStrategy means the source and the target share no
	// common ancestor, so none of the strategies -- every one of which replays
	// a change relative to a base -- has anything to work with.
	ReasonNoApplicableStrategy AttentionReason = "integration_no_applicable_strategy"
	// ReasonTargetMoved means the target branch changed between the read and
	// the compare-and-set despite the lock -- something outside AO wrote it.
	ReasonTargetMoved AttentionReason = "integration_target_moved"
)

// Recorder persists one attempt's Record. It is called for BOTH outcomes: an
// integration that stopped for a person is exactly as much a fact about the
// target branch as one that landed, and the SHAs it names stop being
// recoverable the moment anything else moves.
type Recorder interface {
	RecordIntegration(ctx context.Context, rec Record) error
}

// RecorderFunc adapts a function to Recorder.
type RecorderFunc func(ctx context.Context, rec Record) error

// RecordIntegration calls f.
func (f RecorderFunc) RecordIntegration(ctx context.Context, rec Record) error { return f(ctx, rec) }

// Policy is the only place a project widens what the coordinator may do.
//
// Its zero value is the intended default rather than an inert one, which is
// why the two fields are polarised the way they are: a caller that constructs
// Policy{} gets fast-forward, rebase and cherry-pick (all of which keep the
// target's history linear and none of which can invent a commit), plus the one
// automatic conflict resolution that is provably deterministic.
type Policy struct {
	// AllowMergeCommit permits StrategyMergeCommit. Off by default because a
	// merge commit is the one integration whose result no verification has
	// ever seen as a unit unless this coordinator re-runs it.
	AllowMergeCommit bool
	// DisableAutoResolve turns off conflict.go's append-only rule, so every
	// conflict at all becomes a Needs attention. It exists for projects that
	// would rather look at every conflict themselves.
	DisableAutoResolve bool
}

// ReviewState is how the task's review ended. The coordinator only reads it;
// it never derives it.
type ReviewState string

// The review states. Only the first two let a task be integrated.
const (
	ReviewApproved         ReviewState = "approved"
	ReviewSkipped          ReviewState = "skipped"
	ReviewChangesRequested ReviewState = "changes_requested"
	ReviewFailed           ReviewState = "failed"
	ReviewPending          ReviewState = "pending"
)

// VerifyState is how the task's verification ended.
type VerifyState string

// The verification states. Only the first two let a task be integrated;
// VerifySkipped means the plan asked for no verification, not that one was
// skipped after being asked for.
const (
	VerifyPassed  VerifyState = "passed"
	VerifySkipped VerifyState = "skipped"
	VerifyFailed  VerifyState = "failed"
	VerifyPending VerifyState = "pending"
)

// Readiness is the caller's proof that a task may be integrated at all. It
// mirrors the gate internal/workflow already enforces before completing a
// task's execution run -- review approved or skipped, verification passed or
// not planned -- and restates it here so that the gate is enforced at the one
// place that can actually move the target branch, rather than only at the
// place that decides to call it.
type Readiness struct {
	Review ReviewState
	Verify VerifyState
}

// Ready reports whether the task may be integrated, and why not when it may
// not. Anything other than the two accepting values of each field is a
// refusal, including the empty string: a caller that forgot to populate
// Readiness has not shown that review and verification passed, and "not shown"
// and "failed" have to be treated identically here or the gate is decorative.
func (r Readiness) Ready() (bool, string) {
	switch r.Review {
	case ReviewApproved, ReviewSkipped:
	case "":
		return false, "review state is unknown"
	default:
		return false, "review is " + string(r.Review)
	}
	switch r.Verify {
	case VerifyPassed, VerifySkipped:
	case "":
		return false, "verification state is unknown"
	default:
		return false, "verification is " + string(r.Verify)
	}
	return true, ""
}

func trimmed(values ...*string) {
	for _, v := range values {
		*v = strings.TrimSpace(*v)
	}
}
