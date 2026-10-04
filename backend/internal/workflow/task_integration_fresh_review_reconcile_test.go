package workflow_test

// Closing an integration fresh-review request that a review already answered
// (task_integration_fresh_review_reconcile.go).
//
// The request is normally closed at integration. A run that stops between the
// verdict and integration therefore keeps it open forever — and an open request
// is consulted before every other reason a review step can rest at `waiting`,
// so it shadows them all. These tests pin both halves: the evidence that closes
// a request, and the far more important set of situations that must NOT.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type reconcileFixture struct {
	t       *testing.T
	coord   *workflowcore.Coordinator
	store   *fakeStore
	clk     *fakeClock
	reviews *fakeReviewRuns
	runID   string
	sid     string
	now     time.Time
	// wanted is the fingerprint the request asked to have reviewed.
	wanted string
	// priorReviewID is the stale approval the request was raised about.
	priorReviewID string
}

const (
	reconcileWantedFingerprint = "fp-current-8943c916"
	reconcileStaleFingerprint  = "fp-approved-fec959f9"
)

// newReconcileFixture reproduces Task 8's shape: an integration fresh-review
// request, no answered row, and the run stopped after the verdict but before
// integration.
func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()
	now := time.Date(2026, 8, 24, 13, 30, 0, 0, time.UTC)
	runID := "wf-reconcile"
	fx := &reconcileFixture{
		t: t, store: newFakeStore(), clk: &fakeClock{t: now}, runID: runID,
		sid: "sess-reconcile", now: now,
		wanted: reconcileWantedFingerprint, priorReviewID: "review-stale",
	}

	fx.store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &fx.sid},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepWaiting},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepWaiting},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepFailed},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending},
	}
	fx.store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "project-1", Objective: "reconcile objective",
		State: domain.WorkflowRunNeedsAttention, PolicyVersion: "v1",
		PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now.Add(-8 * time.Hour), UpdatedAt: now,
	}

	workStepID, reviewStepID := "work", "review"
	requestedAt := now.Add(-20 * time.Minute)
	payload, err := json.Marshal(map[string]any{
		"taskId":              "wft-reconcile",
		"masterRunId":         "wf-master",
		"approvedFingerprint": reconcileStaleFingerprint,
		"currentFingerprint":  fx.wanted,
		"reviewStepId":        reviewStepID,
		"priorReviewRunId":    fx.priorReviewID,
		"attempt":             1,
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.store.checkpoints[runID] = []domain.WorkflowCheckpoint{
		{
			ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "project-1",
			SessionID: &fx.sid, Branch: "main", WorktreePath: "/tmp/reconcile",
			DurablePhase: "worker_observed", CreatedAt: now.Add(-8 * time.Hour),
		},
		{
			ID: "req-cp", WorkflowRunID: runID, WorkflowStepID: &reviewStepID, ProjectID: "project-1",
			RetryState: string(payload), DurablePhase: "integration_fresh_review_required",
			FingerprintBefore: reconcileStaleFingerprint, FingerprintAfter: fx.wanted,
			CreatedAt: requestedAt,
		},
	}

	fx.reviews = newFakeReviewRuns()
	// The stale approval the request was raised about.
	fx.reviews.runs[fx.priorReviewID] = domain.ReviewRun{
		ID: fx.priorReviewID, SessionID: domain.SessionID(fx.sid), Harness: domain.ReviewerClaudeCode,
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		TargetSHA: reconcileStaleFingerprint, CreatedAt: requestedAt.Add(-time.Hour),
	}
	fx.coord = fx.newCoordinator()
	return fx
}

func (fx *reconcileFixture) newCoordinator() *workflowcore.Coordinator {
	ids := 0
	return workflowcore.New(workflowcore.Deps{
		Store: fx.store, ReviewRuns: fx.reviews, SessionFacts: newFakeSessionFacts(),
		MessageSender: &fakeMessageSender{}, Clock: fx.clk.Now,
		NewID: func() string { ids++; return "rec-id" },
	})
}

// answeringReview adds a review that satisfies every proof unless a field is
// overridden by the caller.
func (fx *reconcileFixture) answeringReview(id string, mutate func(*domain.ReviewRun)) {
	fx.t.Helper()
	r := domain.ReviewRun{
		ID: id, SessionID: domain.SessionID(fx.sid), Harness: domain.ReviewerClaudeCode,
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		TargetSHA: fx.wanted, CreatedAt: fx.now.Add(-15 * time.Minute),
	}
	if mutate != nil {
		mutate(&r)
	}
	fx.reviews.runs[r.ID] = r
}

func (fx *reconcileFixture) continueRun() {
	fx.t.Helper()
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
		fx.t.Fatalf("ContinueRun: %v", err)
	}
}

func (fx *reconcileFixture) answeredRows() []domain.WorkflowCheckpoint {
	fx.t.Helper()
	return checkpointsByPhase(fx.store, fx.runID, "integration_fresh_review_answered")
}

// stillBlocks reports whether the request is still shadowing the dispatcher.
func (fx *reconcileFixture) stillBlocks() bool {
	fx.t.Helper()
	_, ok := fx.coord.PendingIntegrationFreshReviewForTest(context.Background(), fx.runID, "review")
	return ok
}

// ---- 1. an approved review closes it ---------------------------------------

func TestApprovedReviewClosesTheIntegrationRequest(t *testing.T) {
	fx := newReconcileFixture(t)
	fx.answeringReview("review-answer", nil)
	if !fx.stillBlocks() {
		t.Fatal("precondition: the request should be outstanding before reconciliation")
	}

	fx.continueRun()

	rows := fx.answeredRows()
	if len(rows) != 1 {
		t.Fatalf("answered rows = %d, want exactly 1", len(rows))
	}
	// The reconciliation must be legible as such in the ledger, and must name
	// the review it relied on.
	if !containsAll(rows[0].NextAction, "reconciled", "review-answer") {
		t.Fatalf("answered row does not explain itself: %q", rows[0].NextAction)
	}
	if fx.stillBlocks() {
		t.Fatal("the request still shadows the dispatcher after being answered")
	}
}

// ---- 2. changes_requested closes it too ------------------------------------

// A request asks "review this workspace", not "approve this workspace". A
// changes_requested verdict answers it just as completely as an approval.
func TestChangesRequestedAlsoClosesTheIntegrationRequest(t *testing.T) {
	fx := newReconcileFixture(t)
	fx.answeringReview("review-answer", func(r *domain.ReviewRun) {
		r.Verdict = domain.VerdictChangesRequested
	})

	fx.continueRun()

	if n := len(fx.answeredRows()); n != 1 {
		t.Fatalf("answered rows = %d, want 1: changes_requested answers the request", n)
	}
	if fx.stillBlocks() {
		t.Fatal("a changes_requested verdict left the request outstanding")
	}
}

// ---- 3. the refusals -------------------------------------------------------

// A review of a DIFFERENT workspace answers a different question.
func TestReviewOfADifferentFingerprintDoesNotClose(t *testing.T) {
	fx := newReconcileFixture(t)
	fx.answeringReview("review-elsewhere", func(r *domain.ReviewRun) {
		r.TargetSHA = "fp-some-other-workspace"
	})

	fx.continueRun()

	if n := len(fx.answeredRows()); n != 0 {
		t.Fatalf("answered rows = %d, want 0: a review of another workspace cannot close this request", n)
	}
	if !fx.stillBlocks() {
		t.Fatal("the request stopped blocking without ever being answered")
	}
}

// A verdict that predates the question cannot be its answer.
func TestReviewOlderThanTheRequestDoesNotClose(t *testing.T) {
	fx := newReconcileFixture(t)
	fx.answeringReview("review-too-early", func(r *domain.ReviewRun) {
		r.CreatedAt = fx.now.Add(-45 * time.Minute) // before the request
	})

	fx.continueRun()

	if n := len(fx.answeredRows()); n != 0 {
		t.Fatalf("answered rows = %d, want 0: a review older than the request cannot answer it", n)
	}
	if !fx.stillBlocks() {
		t.Fatal("the request stopped blocking on evidence that predates it")
	}
}

// Two equally-qualified candidates mean AO cannot say which one answered.
func TestTwoAmbiguousReviewsDoNotClose(t *testing.T) {
	fx := newReconcileFixture(t)
	fx.answeringReview("review-a", nil)
	fx.answeringReview("review-b", func(r *domain.ReviewRun) {
		r.CreatedAt = fx.now.Add(-10 * time.Minute)
	})

	fx.continueRun()

	if n := len(fx.answeredRows()); n != 0 {
		t.Fatalf("answered rows = %d, want 0: two candidates is ambiguity, and ambiguity refuses", n)
	}
	if !fx.stillBlocks() {
		t.Fatal("an ambiguous request was closed anyway")
	}
}

// A review still running, or one that was cancelled, answers nothing.
func TestNonTerminalOrCancelledReviewDoesNotClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ReviewRun)
	}{
		{"still running", func(r *domain.ReviewRun) { r.Status = domain.ReviewRunRunning; r.Verdict = "" }},
		{"cancelled", func(r *domain.ReviewRun) { r.Status = domain.ReviewRunCancelled; r.Verdict = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newReconcileFixture(t)
			fx.answeringReview("review-nonterminal", tc.mutate)

			fx.continueRun()

			if n := len(fx.answeredRows()); n != 0 {
				t.Fatalf("answered rows = %d, want 0", n)
			}
			if !fx.stillBlocks() {
				t.Fatal("a review with no usable verdict closed the request")
			}
		})
	}
}

// ---- 4. a genuinely pending request keeps blocking -------------------------

// The property that matters most: this must not become a way to make every
// request disappear. With no answering review at all, the request is still
// open, still outstanding, and still correctly shadowing the dispatcher.
func TestGenuinelyPendingRequestKeepsBlocking(t *testing.T) {
	fx := newReconcileFixture(t)
	// No answering review exists — only the stale approval it was raised about.

	for i := 0; i < 5; i++ {
		fx.clk.Advance(time.Minute)
		fx.continueRun()
	}

	if n := len(fx.answeredRows()); n != 0 {
		t.Fatalf("answered rows = %d, want 0: nothing has answered this request", n)
	}
	if !fx.stillBlocks() {
		t.Fatal("a request nobody answered stopped blocking — this is the failure mode that would let AO integrate unreviewed work")
	}
}

// ---- 5. idempotence across polls and restarts ------------------------------

func TestFiftyPollsAndARestartWriteExactlyOneAnswered(t *testing.T) {
	fx := newReconcileFixture(t)
	fx.answeringReview("review-answer", nil)

	for i := 0; i < 25; i++ {
		fx.clk.Advance(2 * time.Second)
		fx.continueRun()
	}
	// A second Coordinator over the same durable state IS a daemon restart.
	fx.coord = fx.newCoordinator()
	for i := 0; i < 25; i++ {
		fx.clk.Advance(2 * time.Second)
		fx.continueRun()
	}

	if n := len(fx.answeredRows()); n != 1 {
		t.Fatalf("answered rows = %d after 50 polls across a restart, want exactly 1", n)
	}
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
