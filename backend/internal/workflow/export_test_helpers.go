package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// export_test_helpers.go — narrow windows onto production functions whose inputs
// a unit fixture cannot realistically stage.
//
// Everything here calls the real implementation; none of it reimplements one.

// ReviewCycleOfKeyForTest exposes the key-parsing fallback so a test can assert
// what it does NOT tell us about a replacement claim: those keys carry no cycle,
// so parsing one yields zero, and a budget recorded under a real cycle would
// read as untouched if recovery relied on it.
func ReviewCycleOfKeyForTest(key string) int {
	return reviewCycleOf(domain.WorkflowOutboxEntry{IdempotencyKey: key})
}

// ReviewLaunchBudgetRemainsForTest exercises the gate every claim-release path
// consults.
//
// Driving a REPLACEMENT claim into that gate through a full dispatch needs the
// whole replacement-authorization flow staged around it; the property under test
// — that the cycle comes from the durable record rather than from the claim's
// key — lives entirely inside this function, so it is exercised here directly.
func (c *Coordinator) ReviewLaunchBudgetRemainsForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
) (bool, string, error) {
	return c.reviewLaunchBudgetRemains(ctx, run, step, entry)
}

// ReleaseReviewDispatchClaimForTest exercises the single choke point for
// dispatched -> pending, which applies the retry-budget gate itself.
//
// Reaching it with an exhausted budget through a full dispatch is not possible:
// claim-time allocation refuses the dispatch first, which is correct but hides
// what this gate independently guarantees — that a claim nothing will retry is
// not left advertising a retry.
func (c *Coordinator) ReleaseReviewDispatchClaimForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, why string,
) (domain.WorkflowStep, error) {
	return c.releaseReviewDispatchClaim(ctx, run, step, entry, why)
}

// ResumeReviewLaunchAfterFailureForTest exercises the human-resume protocol
// directly, so a test can place a ledger failure on the resume's OWN history
// read.
//
// Driving it through ContinueRun cannot isolate that read: an earlier
// checkpoint read inside the same call fails closed first, which is correct but
// masks whether this one does.
func (c *Coordinator) ResumeReviewLaunchAfterFailureForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
) bool {
	observed, ok := c.observeFailedReviewLaunchGeneration(ctx, run.ID, step.ID, entry)
	if !ok {
		return false
	}
	return c.resumeReviewLaunchAfterFailure(ctx, run, step, entry, observed)
}

// ReviewLaunchGenerationForTest is one observed failed outbox generation, held
// opaquely so a test can capture what a caller SAW at one moment and hand it to
// a resume that runs much later — which is the whole shape of the race the
// binding exists to refuse.
type ReviewLaunchGenerationForTest struct {
	gen reviewLaunchGeneration
}

// RecordIDForTest is the launch-failure record this observation names.
func (g ReviewLaunchGenerationForTest) RecordIDForTest() string { return g.gen.RecordID }

// EpochForTest is the budget epoch the observed failure was produced in.
func (g ReviewLaunchGenerationForTest) EpochForTest() int { return g.gen.Epoch }

// ObserveFailedReviewLaunchGenerationForTest captures the failed generation a
// caller is looking at right now.
func (c *Coordinator) ObserveFailedReviewLaunchGenerationForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
) (ReviewLaunchGenerationForTest, bool) {
	gen, ok := c.observeFailedReviewLaunchGeneration(ctx, run.ID, step.ID, entry)
	return ReviewLaunchGenerationForTest{gen: gen}, ok
}

// ResumeReviewLaunchFromGenerationForTest resumes a launch failure against an
// observation taken EARLIER — a resume that was delayed while the world moved
// on. Nothing else can express the Codex race: the point is precisely that the
// observation and the resume are separated in time.
func (c *Coordinator) ResumeReviewLaunchFromGenerationForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, observed ReviewLaunchGenerationForTest,
) bool {
	return c.resumeReviewLaunchAfterFailure(ctx, run, step, entry, observed.gen)
}

// AllocateReviewLaunchAttemptForTest exposes claim-time allocation so a test can
// assert the NUMBER a fresh budget epoch hands out, not merely that one was
// handed out. Driving it through a dispatch reports only success or refusal,
// which cannot distinguish "the new epoch started over" from "it continued the
// superseded epoch's numbering and happened to be under the limit".
func (c *Coordinator) AllocateReviewLaunchAttemptForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, cycleNumber int,
) (int, error) {
	return c.allocateReviewLaunchAttempt(ctx, run, step, entry, cycleNumber)
}

// MaxReviewerLaunchAttemptsForTest is the retry budget the production gate
// applies, so a test asserts against the real limit rather than a copy of it
// that would keep passing if the limit changed.
const MaxReviewerLaunchAttemptsForTest = maxReviewerLaunchAttempts
