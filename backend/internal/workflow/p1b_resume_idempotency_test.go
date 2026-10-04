package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1b_resume_idempotency_test.go — matrix 2/3/4/5, against the real dispatch
// stack rather than the classifier.
//
// The classifier tests (p1b_recovery_internal_test.go) prove Resume NAMES the
// right obligation. These prove the thing that actually matters: naming it and
// then discharging it repeatedly, across restarts, produces exactly one
// worker, one reviewer, one fix prompt and one verification authority.
//
// The observable surface is the fixture's own side-effect counters -- Spawn,
// reviewer launch, InsertReviewRun, Send -- plus the durable attempt rows. If
// none of them move across repeated resumes and repeated daemon restarts,
// nothing was duplicated. That is the same instrument P0-D's ABA suite uses,
// pointed at P1-B's new entry point.

// p1bResumeLoop resumes runID resumeCount times, restarting the coordinator
// before each one so every resume is a fresh daemon reading the same rows.
// It returns the obligation each resume reported.
func p1bResumeLoop(t *testing.T, f *fixRecoveryFixture, resumeCount int) []workflowcore.ResumeObligationKind {
	t.Helper()
	ctx := context.Background()
	out := make([]workflowcore.ResumeObligationKind, 0, resumeCount)
	for i := 0; i < resumeCount; i++ {
		c := f.restart()
		_, report, err := c.ResumeRun(ctx, f.runID)
		if err != nil {
			t.Fatalf("resume %d: %v", i+1, err)
		}
		out = append(out, report.Obligation.Kind)
		f.clk.Advance(11 * time.Second)
	}
	return out
}

func p1bAssertSameObligation(t *testing.T, got []workflowcore.ResumeObligationKind, want workflowcore.ResumeObligationKind) {
	t.Helper()
	for i, kind := range got {
		if kind != want {
			t.Fatalf("resume %d reported obligation %q, want %q on every pass", i+1, kind, want)
		}
	}
}

// Matrix 2: a running worker is OBSERVED. Five resumes across five restarts
// must never produce a second Spawn or a second work attempt.
func TestResumeRunningWorkerNeverDuplicatesTheLaunch(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	detail, err := f.c.StartRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	if work.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("work step = %q, want running: the fixture never reached the state under test", work.Step.State)
	}
	// A live, evidenced worker: the session exists, is not terminated, and is
	// working. Every resume below must adopt exactly this one.
	f.sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityActive}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})

	before := f.p0dSnapshot(t, work.Step.ID)
	kinds := p1bResumeLoop(t, f, 5)
	p1bAssertSameObligation(t, kinds, workflowcore.ResumeObligationWorkObservation)

	after := f.p0dSnapshot(t, work.Step.ID)
	if after != before {
		t.Fatalf("five resumes over one running worker moved the side-effect counters\n  before: %+v\n  after:  %+v", before, after)
	}
	if f.spawner.calls != 1 {
		t.Fatalf("Spawn called %d times, want exactly 1", f.spawner.calls)
	}
}

// Matrix 3: a review in flight is OBSERVED. Repeated resumes must not insert a
// second review run or launch a second reviewer.
func TestResumeRunningReviewNeverDuplicatesTheReviewer(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	completeWorkStep(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.runID)
	detail, err := f.c.ContinueRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(detail)
	if review.Step.State != domain.WorkflowStepRunning || review.Step.ReviewRunID == nil {
		t.Fatalf("review step = %q reviewRun=%v, want a running review", review.Step.State, review.Step.ReviewRunID)
	}
	reviewRunID := *review.Step.ReviewRunID

	before := f.p0dSnapshot(t, review.Step.ID)
	kinds := p1bResumeLoop(t, f, 5)
	p1bAssertSameObligation(t, kinds, workflowcore.ResumeObligationReviewObservation)

	after := f.p0dSnapshot(t, review.Step.ID)
	if after != before {
		t.Fatalf("five resumes over one running review moved the side-effect counters\n  before: %+v\n  after:  %+v", before, after)
	}
	if f.reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun called %d times, want exactly 1", f.reviewRuns.insertCalls)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launched %d times, want exactly 1", f.launcher.launchCalls)
	}
	// The review the run is bound to must still be the same one: a new review
	// run id would be a second reviewer generation even if the counters
	// happened to agree.
	final, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewStepFrom(final).Step.ReviewRunID; got == nil || *got != reviewRunID {
		t.Fatalf("review run id moved from %s to %v across resumes", reviewRunID, got)
	}
}

// Matrix 4: a fix cycle in flight is OBSERVED. Repeated resumes must not send
// the findings a second time -- re-sending appends a second copy into the
// worker's composer, which is the exact incident fix delivery was hardened for.
func TestResumeRunningFixNeverDuplicatesThePrompt(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	detail := f.driveToFixDispatch()
	fix := fixStepFrom(detail)
	if fix.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("fix step = %q, want running: the fixture never reached the state under test", fix.Step.State)
	}
	sendsAfterDispatch := f.sender.calls
	if sendsAfterDispatch != 1 {
		t.Fatalf("findings sent %d times at dispatch, want exactly 1", sendsAfterDispatch)
	}

	before := f.p0dSnapshot(t, fix.Step.ID)
	kinds := p1bResumeLoop(t, f, 5)
	p1bAssertSameObligation(t, kinds, workflowcore.ResumeObligationFixObservation)

	after := f.p0dSnapshot(t, fix.Step.ID)
	if after != before {
		t.Fatalf("five resumes over one running fix cycle moved the side-effect counters\n  before: %+v\n  after:  %+v", before, after)
	}
	if f.sender.calls != sendsAfterDispatch {
		t.Fatalf("findings sent %d times after five resumes, want %d", f.sender.calls, sendsAfterDispatch)
	}
	if _, err := f.c.GetRun(ctx, f.runID); err != nil {
		t.Fatal(err)
	}
}

// Matrix 5: resuming at verify must verify the EXACT approved review target,
// and repeated resumes must not promote any other authority into that role.
func TestResumeAtVerifyPreservesTheApprovedAuthority(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	completeWorkStep(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.runID)
	detail, err := f.c.ContinueRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(detail)
	reviewRunID := *review.Step.ReviewRunID
	f.reviewRuns.setStatus(reviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)
	f.clk.Advance(time.Second)

	approved, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStepFrom(approved).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q after approval, want completed", reviewStepFrom(approved).Step.State)
	}
	verifyStepID := ""
	for _, sd := range approved.Steps {
		if sd.Step.Kind == domain.WorkflowStepVerify {
			verifyStepID = sd.Step.ID
		}
	}
	if verifyStepID == "" {
		t.Fatal("no verify step")
	}

	before := f.p0dSnapshot(t, verifyStepID, review.Step.ID)
	kinds := p1bResumeLoop(t, f, 5)
	for i, kind := range kinds {
		// The obligation is either the verification itself or nothing left,
		// depending on what this fixture's verifier can conclude -- but it must
		// never regress to a review or a fix, which would be authority
		// promotion: re-opening a decision that has already been made.
		switch kind {
		case workflowcore.ResumeObligationVerify, workflowcore.ResumeObligationNone:
		default:
			t.Fatalf("resume %d at verify reported obligation %q; the approved review must not be reopened", i+1, kind)
		}
	}

	after := f.p0dSnapshot(t, verifyStepID, review.Step.ID)
	if after != before {
		t.Fatalf("five resumes at verify moved the side-effect counters\n  before: %+v\n  after:  %+v", before, after)
	}
	// The authority itself is unchanged: same review run, still approved, and
	// no second review run was ever created.
	final, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewStepFrom(final).Step.ReviewRunID; got == nil || *got != reviewRunID {
		t.Fatalf("the verified authority moved from review run %s to %v", reviewRunID, got)
	}
	if f.reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun called %d times across five verify resumes, want 1", f.reviewRuns.insertCalls)
	}
}
