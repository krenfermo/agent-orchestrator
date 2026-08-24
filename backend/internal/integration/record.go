package integration

import (
	"context"
	"fmt"
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
	// StrategyNoOp is an integration with no git operation at all: the work is
	// already on the target ref and all that had to happen was proving, under
	// the lane, that it still is.
	//
	// It exists so direct-branch execution can take the same road as every
	// other mode instead of a parallel one. Calling it a fast-forward would be
	// the small lie that made the parallel road look justified — nothing was
	// forwarded — and an integration that performs no git operation still has
	// a lane to take, a readiness gate to pass, a precondition to satisfy and
	// an audit row to write. Naming the no-op is what lets it have all four.
	StrategyNoOp Strategy = "no_op"
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
	// TargetRef is the ref that actually moved -- refs/heads/<TargetBranch>
	// for a real branch, or the AO-owned ref a master run accumulates on.
	TargetRef    string
	SourceBranch string

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

// The outcomes an attempt can have. There is no "failed": an attempt that could
// not run at all returns an error and is not recorded, because nothing happened
// to the target for a ledger to describe.
//
// OutcomeAttempting is the reason a landed target can never lack an audit
// record. The ref update and the record of it are two writes to two different
// stores, so no ordering of them alone is safe: recording afterwards loses the
// record if the process dies in between, and recording only beforehand cannot
// say what happened. Writing the intent first and the result after means the
// worst a crash can leave behind is a row that names exactly which commit was
// about to move which ref, from where -- which is enough to see what happened
// and reconcile it. Both rows are written while the lane is still held.
const (
	OutcomeAttempting     RecordOutcome = "attempting"
	OutcomeIntegrated     RecordOutcome = "integrated"
	OutcomeNeedsAttention RecordOutcome = "needs_attention"
)

// Verification is the verification that authorized one integration.
//
// It is NOT only "did this package re-run the checks". Every integration is
// authorized by some verification — a plain fast-forward and a direct-branch
// proof are authorized by the one the task itself already passed, and only a
// replay is authorized by one this package ran. Recording Ran=false for the
// first two was the defect: an audit row that says "verificationRan: false" for
// work that a durable, successful verify step had in fact approved describes
// the coordinator's own activity instead of the fact a reader needs, which is
// what authorized moving the ref.
//
// So the zero value means "no verification is claimed at all", and anything
// else carries the evidence: which durable record the verdict comes from, and
// which content identity it describes. A verdict whose Fingerprint no longer
// matches what is being integrated is never reused (see Request.Verified).
type Verification struct {
	Ran    bool
	Passed bool
	// Summary is a short human-readable account of what ran and what failed.
	Summary string
	// Source says where this verdict came from, so a reader can tell a reused
	// verdict from one this integration produced.
	Source VerificationSource
	// StepID and EvidenceID point at the durable records behind the verdict —
	// the workflow step that verified, and the specific result row. Both are
	// links into evidence that outlives this record, which is the difference
	// between an auditable claim and an assertion.
	StepID     string
	EvidenceID string
	// Fingerprint is the content identity this verdict describes: the thing
	// that was verified, not the thing that happened to be integrated. When it
	// stops matching what is about to land, the verdict is stale and this
	// package refuses to reuse it.
	Fingerprint string
}

// VerificationSource is where a Verification's verdict came from.
type VerificationSource string

const (
	// SourceTaskVerification is the task's own durable verify step: the
	// verification that authorized the work in the first place, reused because
	// the content being integrated is byte-for-byte the content it judged.
	SourceTaskVerification VerificationSource = "task_verification"
	// SourcePostReplay is a verification this coordinator ran after replaying
	// the work onto a target that had moved, i.e. against content no earlier
	// verification had ever seen.
	SourcePostReplay VerificationSource = "post_replay"
	// SourceRevalidated is a verification this coordinator ran because the
	// task's own verdict had gone stale — the work changed after it was
	// verified, without any replay by this package.
	SourceRevalidated VerificationSource = "revalidated"
	// SourceNotPlanned means the task's plan declared no verification at all.
	// It is not a pass; it is the absence of anything to pass.
	SourceNotPlanned VerificationSource = "not_planned"
)

// authorizes reports whether this verdict may authorize integrating content
// whose identity is fingerprint, and why not when it may not.
//
// The empty-fingerprint cases are refusals rather than passes on purpose. A
// verdict that cannot say what it describes, or a caller that cannot say what
// it is integrating, has not shown the two are the same thing — and "not shown"
// has to be treated exactly as "different" here, or the check is decorative.
func (v Verification) authorizes(fingerprint string) (bool, string) {
	switch {
	case !v.Ran:
		return false, "no verification is claimed"
	case !v.Passed:
		return false, "the verification it claims did not pass"
	case v.Fingerprint == "":
		return false, "the verification does not say what content it describes"
	case fingerprint == "":
		return false, "the content being integrated has no fingerprint to compare against"
	case v.Fingerprint != fingerprint:
		return false, fmt.Sprintf("it verified %s and the content being integrated is %s", v.Fingerprint, fingerprint)
	}
	return true, ""
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
	// ReasonPreconditionFailed is a caller's own freshness check refusing under
	// the lane — the target is not what the task's work was judged against.
	ReasonPreconditionFailed AttentionReason = "integration_precondition_failed"
	// ReasonTargetMovedAfterVerification is a no-replay integration whose
	// target no longer contains the verified work. It is distinct from a
	// conflict: nothing is wrong with the work, something moved the branch.
	ReasonTargetMovedAfterVerification AttentionReason = "integration_target_moved_after_verification"
	// ReasonTargetMoved means the target branch changed between the read and
	// the compare-and-set despite the lock -- something outside AO wrote it.
	ReasonTargetMoved AttentionReason = "integration_target_moved"
	// ReasonStaleVerification means the verification the caller offered no
	// longer describes the content being integrated, and nothing was available
	// to produce a fresh one. Integrating anyway would record an authorization
	// that did not authorize this — the exact fiction the evidence fields
	// exist to prevent — so it stops instead.
	ReasonStaleVerification AttentionReason = "integration_stale_verification"
)

// Recorder persists one attempt's Record. It is required, not optional: an
// integration nobody can account for afterwards is worse than one that did not
// happen, so a Coordinator cannot be built without one.
//
// It is called for EVERY outcome. An integration that stopped for a person is
// exactly as much a fact about the target as one that landed, and the SHAs it
// names stop being recoverable the moment anything else moves.
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
