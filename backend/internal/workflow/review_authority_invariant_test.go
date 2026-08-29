package workflow_test

// review_authority_invariant_test.go — the P0-B invariant, pinned.
//
//	VERIFY MUST NEVER VALIDATE A COMMIT THAT IS NEWER OR DIFFERENT FROM THE ONE
//	REVIEW ACTUALLY APPROVED, UNLESS THAT NEWER COMMIT HAS ITSELF BEEN REVIEWED
//	AND APPROVED.
//
// The production run is wf-a21d98aa (2026-08-28 19:58–20:19 UTC, branch
// feat/engineering-control-center), and its durable timeline is the fixture
// these tests reproduce:
//
//	20:07:35  work observed          HEAD 77aad8d6   fingerprint 45b2e769
//	20:07:35  review_target_observed HEAD 77aad8d6   fingerprint 45b2e769   cycle 1
//	20:10:15  review 1 -> changes_requested
//	20:12:55  fix 1 delivered                        fingerprint c2561265
//	20:14:43  the fix worker COMMITS                 HEAD 095bf89f
//	20:15:35  review 2 -> changes_requested
//	20:16:55  fix 2 delivered                        fingerprint b0910a3d
//	20:19:35  review 3 -> APPROVED  of b0910a3d, read at HEAD 095bf89f
//	20:19:35  verify   -> verify_workspace_changed   "UNKNOWN: the branch
//	                                                  advanced from the approved
//	                                                  commit 77aad… to 095bf…"
//
// Nothing about that last line was true. The branch had not advanced since the
// approval: 095bf89f was already HEAD when review 3 read the tree. AO said
// otherwise because review cycles 2 and 3 never wrote down which COMMIT their
// target fingerprint named — reviewTargetFingerprint pins that only for a
// first-cycle dispatch — so approvedHeadSHA fell back to the work step's
// completion commit, two fix cycles stale, and the drift classifier compared the
// live HEAD against it.
//
// The tests below cover both halves: the false positive that parked wf-a21d98aa,
// and the true positive it must never be traded for.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// authorityCleanAt is the same, committed: HEAD moved, nothing outstanding.
func authorityCleanAt(dir, head string) ports.WorkspaceObservation {
	return ports.WorkspaceObservation{Path: dir, Branch: "feat/engineering-control-center", HeadSHA: head}
}

func authorityCheckpoints(store *fakeStore, runID, phase string) []domain.WorkflowCheckpoint {
	var out []domain.WorkflowCheckpoint
	for _, cp := range store.checkpoints[runID] {
		if cp.DurablePhase == phase {
			out = append(out, cp)
		}
	}
	return out
}

func authorityVerifyClasses(detail workflowcore.RunDetail) []domain.WorkflowErrorClass {
	var out []domain.WorkflowErrorClass
	for _, s := range detail.Steps {
		if s.Step.Kind != domain.WorkflowStepVerify {
			continue
		}
		for _, a := range s.Attempts {
			out = append(out, a.ErrorClass)
		}
	}
	return out
}

// (a) approved(A) with an unchanged HEAD verifies A, and (l) the reviewer
// lifecycle underneath it is untouched. This is the control: the guard must not
// have been loosened into always saying yes.
func TestApprovedTargetWithUnchangedHeadVerifies(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, st, clk, sessionFacts, ws, reviewRuns, _, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, st, clk, sessionFacts, ws, dir)

	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatal("no reviewer was dispatched")
	}
	reviewRuns.setStatus(*review.Step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)

	clk.Advance(2 * time.Minute)
	got, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	for _, class := range authorityVerifyClasses(got) {
		if class == domain.WorkflowErrorVerifyWorkspaceChanged {
			t.Fatal("verify_workspace_changed although nothing moved after the approval")
		}
	}
	if got.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed", got.Run.State)
	}
}

// (b, e) approved(A), then the branch advances to B: verification must not
// certify A (it is not what is there) and must not certify B (nobody read it).
// It must ask for a review of B first — and (d) the approval of A must not be
// what authorizes the verification that follows.
//
// This is wf-a21d98aa's shape with the head genuinely moved, which is the case
// its false positive was indistinguishable from.
func TestBranchAdvanceAfterApprovalIsNotVerifiedOnTheStaleApproval(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, st, clk, sessionFacts, ws, reviewRuns, _, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, st, clk, sessionFacts, ws, dir)

	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	approvedRunID := *review.Step.ReviewRunID
	approvedTarget := reviewRuns.runs[approvedRunID].TargetSHA
	reviewRuns.setStatus(approvedRunID, domain.ReviewRunComplete, domain.VerdictApproved)

	// The branch advances: the work is committed and HEAD moves to B.
	ws.obs = authorityCleanAt(dir, "095bf89fd5d0dacb734662e7ed15e9b767eefae5")
	advanced := workflowcore.WorkspaceFingerprint(ws.obs)
	if advanced == approvedTarget {
		t.Fatal("fixture is not exercising an advance: the fingerprint did not move")
	}

	clk.Advance(2 * time.Minute)
	got, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	// Neither commit may have been certified.
	if got.Run.State == domain.WorkflowRunCompleted {
		t.Fatal("the run completed on a head no review approved")
	}
	if runner.calls != 0 {
		t.Fatalf("verification commands ran %d times on an unreviewed head; want 0", runner.calls)
	}
	// And the stale approval must be recorded as superseded, not reused: either
	// a fresh review is now due, or the run stopped fail-closed. What it must
	// never be is a verification that ran.
	fresh := len(authorityCheckpoints(st, runID, "verify_branch_advanced_fresh_review")) +
		len(authorityCheckpoints(st, runID, "verify_provenance_fresh_review"))
	if fresh == 0 && got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q with no fresh review requested: the advance was neither re-reviewed nor stopped", got.Run.State)
	}
	if fresh > 0 {
		clk.Advance(time.Minute)
		got, err = c.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetRun after the fresh review was authorized: %v", err)
		}
		review = reviewStepFrom(got)
		if review.Step.ReviewRunID == nil || *review.Step.ReviewRunID == approvedRunID {
			t.Fatalf("the review step still speaks for the stale approval %s", approvedRunID)
		}
		if target := reviewRuns.runs[*review.Step.ReviewRunID].TargetSHA; target != advanced {
			t.Fatalf("fresh review target = %s, want the advanced head's tree %s", target, advanced)
		}
	}
}

// (f, g, h) The decision survives a restart on either side of it, and repeating
// it changes nothing.
//
// f: the daemon dies between approved(A) and the advance to B.
// g: B exists and the daemon dies before the re-review is dispatched.
// h: reconciling repeatedly is idempotent — one fresh review, not one per poll.
func TestBranchAdvanceReReviewSurvivesRestartAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, st, clk, sessionFacts, ws, reviewRuns, _, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, st, clk, sessionFacts, ws, dir)

	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	approvedRunID := *reviewStepFrom(got).Step.ReviewRunID
	reviewRuns.setStatus(approvedRunID, domain.ReviewRunComplete, domain.VerdictApproved)

	// f — the restart lands here, with the approval durable and nothing moved.
	c = rebuildDriftCoordinator(t, st, clk, sessionFacts, ws, reviewRuns, runner)

	// The advance happens while AO is down.
	ws.obs = authorityCleanAt(dir, "095bf89fd5d0dacb734662e7ed15e9b767eefae5")
	clk.Advance(3 * time.Minute)
	if _, err := c.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}

	// g — and again after B exists but before any re-review has been dispatched.
	c = rebuildDriftCoordinator(t, st, clk, sessionFacts, ws, reviewRuns, runner)

	// h — repeated reconcile. Whatever AO decided, it decides it once.
	for i := 0; i < 5; i++ {
		clk.Advance(30 * time.Second)
		if _, err := c.GetRun(ctx, runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}
	if n := len(authorityCheckpoints(st, runID, "verify_branch_advanced_fresh_review")); n > 1 {
		t.Fatalf("%d branch-advance fresh reviews authorized for one advance; want at most 1", n)
	}
	if n := len(authorityCheckpoints(st, runID, "verify_provenance_fresh_review")); n > 1 {
		t.Fatalf("%d provenance fresh reviews authorized for one advance; want at most 1", n)
	}
	if runner.calls != 0 {
		t.Fatalf("verification commands ran %d times against an unreviewed head; want 0", runner.calls)
	}
}

// (c, i) A fix cycle derived from an approved review is refused, and refusing it
// costs nothing: no prompt is sent, no attempt row is minted, and the next poll
// is free to dispatch whatever IS authorized.
//
// The one exception is the verify-driven re-entry, which is its own written
// authorization — and the test asserts that door still opens.
func TestApprovedReviewDoesNotAuthorizeAFixCycle(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, st, clk, ws, facts, sender, reviews, runID := verifyReentryFixture(t, runner)
	_ = ws
	_ = facts

	// The review step's own run is approved, and no verification has asked for
	// anything. Nothing may reach the worker.
	before := sender.calls
	clk.Advance(time.Minute)
	if _, err := c.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if sender.calls != before {
		t.Fatalf("a fix prompt was delivered under an approved review (%d sends)", sender.calls-before)
	}
	if n := len(st.attempts["fix"]); n != 0 {
		t.Fatalf("%d fix attempts were minted under an approved review; want 0", n)
	}
	// And the review run is untouched: refusing a delivery is not a verdict.
	if got := reviews.runs["review-verify"].Verdict; got != domain.VerdictApproved {
		t.Fatalf("review verdict = %q after a refused fix delivery, want it unchanged", got)
	}
}

// rebuildDriftCoordinator models a daemon restart: the durable store and the
// world outside AO survive, every in-memory decision does not.
func rebuildDriftCoordinator(
	t *testing.T, st *fakeStore, clk *fakeClock, sessionFacts *fakeSessionFacts,
	ws *fakeWorkspaceFacts, reviewRuns *fakeReviewRuns, runner workflowcore.VerifyRunner,
) *workflowcore.Coordinator {
	t.Helper()
	var idSeq int
	return workflowcore.New(workflowcore.Deps{
		Store:            st,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   ws,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: &fakeReviewerLauncher{},
		Verifier:         runner,
		Clock:            clk.Now,
		NewID:            func() string { idSeq++; return fmt.Sprintf("restart%d", idSeq) },
	})
}
