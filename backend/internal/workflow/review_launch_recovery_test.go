package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// These tests reproduce the wf-6d290889-3a7f-41e1-9aa7-b2b265b2ad13 incident and
// pin the recovery lifecycle that replaced it: work completed, review was
// required, the reviewer harness was selected, review + review_run rows and the
// trigger_review outbox entry were created — and then the reviewer launch failed
// before any reviewer session existed.
//
// The properties asserted throughout, in the incident's own terms:
//
//   - the review_run never stays "running" behind a reviewer that never started;
//   - the deep/root error is durable, not just the flat "reviewer_launch_failed";
//   - a transient cause retries automatically, bounded, with a durable wake, and
//     asks nobody for anything;
//   - a permanent cause (auth, missing binary) stops with a named reason AND an
//     action;
//   - every retry path is idempotent: one review row, one outbox entry, one
//     reviewer session, restart-safe.

// newCoordinatorWithReviewAndWakes is newCoordinatorWithReview plus the durable
// wake scheduler, which the transient-retry path depends on.
func newCoordinatorWithReviewAndWakes(
	spawner workflowcore.Spawner,
	sessionFacts workflowcore.SessionFacts,
	workspaceFacts workflowcore.WorkspaceFacts,
	reviewRuns *fakeReviewRuns,
	launcher *fakeReviewerLauncher,
) (*workflowcore.Coordinator, *fakeStore, *fakeClock, *fakeWakeScheduler) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	wakes := newFakeWakeScheduler()
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: launcher,
		WakeScheduler:    wakes,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk, wakes
}

// launchFailureFixture drives a run to "work completed, review dispatch
// attempted, reviewer launch failed" with the given launch error.
type launchFailureFixture struct {
	c        *workflowcore.Coordinator
	store    *fakeStore
	clk      *fakeClock
	wakes    *fakeWakeScheduler
	reviews  *fakeReviewRuns
	launcher *fakeReviewerLauncher
	runID    string
}

func newLaunchFailureFixture(t *testing.T, launchErr, preflightErr error) launchFailureFixture {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviews := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{launchErr: launchErr, preflightErr: preflightErr}
	c, store, clk, wakes := newCoordinatorWithReviewAndWakes(spawner, sessionFacts, workspaceFacts, reviews, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	// ContinueRun is the explicit unblock of review cycle 1 — the exact call
	// the incident's child workflow made before its reviewer launch failed.
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	return launchFailureFixture{c: c, store: store, clk: clk, wakes: wakes, reviews: reviews, launcher: launcher, runID: created.Run.ID}
}

func (f launchFailureFixture) reviewStepID(t *testing.T) string {
	t.Helper()
	steps, _ := f.store.ListWorkflowSteps(context.Background(), f.runID)
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepReview {
			return s.ID
		}
	}
	t.Fatalf("no review step for run %s", f.runID)
	return ""
}

// runningReviewRuns is the orphan detector: how many review_run rows are still
// "running". After a failed launch this must be zero, forever.
func (f launchFailureFixture) runningReviewRuns() int {
	n := 0
	for _, r := range f.reviews.runs {
		if r.Status == domain.ReviewRunRunning {
			n++
		}
	}
	return n
}

func (f launchFailureFixture) reviewOutboxEntries() []domain.WorkflowOutboxEntry {
	var out []domain.WorkflowOutboxEntry
	for key, e := range f.store.outbox {
		if strings.HasPrefix(key, "workflow-step-review:") {
			out = append(out, e)
		}
	}
	return out
}

func (f launchFailureFixture) checkpointPhases() []string {
	var out []string
	for _, cp := range f.store.checkpoints[f.runID] {
		out = append(out, cp.DurablePhase)
	}
	return out
}

func (f launchFailureFixture) checkpointWithPhase(phase string) (domain.WorkflowCheckpoint, bool) {
	var found domain.WorkflowCheckpoint
	ok := false
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase == phase {
			found, ok = cp, true
		}
	}
	return found, ok
}

func (f launchFailureFixture) runState(t *testing.T) domain.WorkflowRunState {
	t.Helper()
	run, ok, err := f.store.GetWorkflowRun(context.Background(), f.runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	return run.State
}

// TestReviewerLaunchTransientFailureRecoversWithoutHuman is the whole incident,
// end to end, with a transient cause: the review row and review_run are created,
// the launch fails before any reviewer session exists, and the run must NOT be
// left with an orphan running review_run, a terminal review step, or a request
// for human attention — it must retry itself and, once the retry succeeds,
// continue the review normally.
func TestReviewerLaunchTransientFailureRecoversWithoutHuman(t *testing.T) {
	transient := errors.New("tmux: resource temporarily unavailable")
	f := newLaunchFailureFixture(t, transient, nil)
	ctx := context.Background()

	if f.launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1", f.launcher.launchCalls)
	}

	// 1. The review_run created for the failed launch is closed out, not left
	// running. This is the partial-state bug the incident exposed.
	if got := f.runningReviewRuns(); got != 0 {
		t.Fatalf("review runs still running after a failed launch = %d, want 0", got)
	}
	if f.reviews.insertCalls != 1 {
		t.Fatalf("review run inserts = %d, want 1", f.reviews.insertCalls)
	}

	// 2. The deep/root error is durable, alongside a real classification —
	// not just the flat "reviewer_launch_failed" class.
	rec, ok := f.checkpointWithPhase("reviewer_launch_error")
	if !ok {
		t.Fatalf("no reviewer_launch_error checkpoint; phases = %v", f.checkpointPhases())
	}
	if !strings.Contains(rec.NextAction, transient.Error()) {
		t.Fatalf("deep launch error not persisted; next_action = %q", rec.NextAction)
	}
	if !strings.Contains(rec.RetryState, `"class":"transient"`) || !strings.Contains(rec.RetryState, `"retryable":true`) {
		t.Fatalf("classification not persisted; retry_state = %q", rec.RetryState)
	}
	if !strings.Contains(rec.RetryState, `"stage":"launch"`) {
		t.Fatalf("launch stage not persisted; retry_state = %q", rec.RetryState)
	}

	// 3. The step is left resumable and the run is not billed to a human.
	detail, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := reviewStepFrom(detail).Step.State; got == domain.WorkflowStepFailed {
		t.Fatalf("review step is terminal (failed) after a transient launch failure — nothing could ever resume it")
	}
	if got := f.runState(t); got == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run parked in needs_attention for a transient, self-remediable launch failure")
	}
	verdict := workflowcore.ClassifyAttention(detail, nil, workflowcore.PhaseNeedsAttention)
	if verdict.Attention == workflowcore.AttentionHuman {
		t.Fatalf("transient reviewer launch failure asked a human for a decision: %+v", verdict)
	}

	// 4. A durable wake was scheduled, so recovery does not depend on a poll.
	sawRetryWake := false
	for _, r := range f.wakes.reasons {
		if r == wake.ReasonTransientRetry {
			sawRetryWake = true
		}
	}
	if !sawRetryWake {
		t.Fatalf("no transient_retry wake scheduled; reasons = %v", f.wakes.reasons)
	}

	// 5. The retry succeeds (the transient condition cleared) and the review
	// proceeds — with no human action anywhere in this sequence.
	f.launcher.launchErr = nil
	f.clk.Advance(time.Minute)
	detail, err = f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun after retry: %v", err)
	}
	if f.launcher.launchCalls != 2 {
		t.Fatalf("launch calls after retry = %d, want 2", f.launcher.launchCalls)
	}
	reviewStep := reviewStepFrom(detail).Step
	if reviewStep.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after a successful retry")
	}
	if reviewStep.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state after successful retry = %q, want running", reviewStep.State)
	}
	live, ok, _ := f.reviews.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if !ok || live.Status != domain.ReviewRunRunning {
		t.Fatalf("attached review run = %+v (found=%v), want a running one", live, ok)
	}
	if f.runningReviewRuns() != 1 {
		t.Fatalf("running review runs after retry = %d, want exactly 1 (no duplicate reviewer)", f.runningReviewRuns())
	}

	// 6. Idempotency: one review row, one outbox entry for this cycle.
	if len(f.reviews.reviews) != 1 {
		t.Fatalf("review rows = %d, want 1", len(f.reviews.reviews))
	}
	if entries := f.reviewOutboxEntries(); len(entries) != 1 {
		t.Fatalf("review outbox entries = %d, want exactly 1 (no duplicate dispatch)", len(entries))
	}

	// 7. Repeated polls after recovery never launch a second reviewer.
	for i := 0; i < 3; i++ {
		if _, err := f.c.GetRun(ctx, f.runID); err != nil {
			t.Fatalf("GetRun poll %d: %v", i, err)
		}
	}
	if f.launcher.launchCalls != 2 {
		t.Fatalf("launch calls after redundant polls = %d, want still 2", f.launcher.launchCalls)
	}
}

// TestReviewerLaunchTransientRetryBudgetEndsInNeedsAttention pins the bound: a
// failure that keeps looking transient must not retry forever. After
// maxReviewerLaunchAttempts the run stops with a named reason and an action —
// and still without an orphan review_run or a duplicate outbox entry.
func TestReviewerLaunchTransientRetryBudgetEndsInNeedsAttention(t *testing.T) {
	f := newLaunchFailureFixture(t, errors.New("tmux: resource temporarily unavailable"), nil)
	ctx := context.Background()

	// Redundant polls/reconciles: the retry budget must be counted durably, not
	// per call, so this converges rather than looping.
	for i := 0; i < 6; i++ {
		f.clk.Advance(time.Minute)
		if _, err := f.c.GetRun(ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}
	if f.launcher.launchCalls != 3 {
		t.Fatalf("launch calls = %d, want exactly 3 (bounded automatic retry)", f.launcher.launchCalls)
	}
	if got := f.runState(t); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state after exhausting the retry budget = %q, want needs_attention", got)
	}
	if got := f.runningReviewRuns(); got != 0 {
		t.Fatalf("orphan running review runs = %d, want 0", got)
	}
	if entries := f.reviewOutboxEntries(); len(entries) != 1 {
		t.Fatalf("review outbox entries = %d, want exactly 1", len(entries))
	}

	detail, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	verdict := workflowcore.ClassifyAttention(detail, nil, workflowcore.PhaseNeedsAttention)
	if verdict.Reason != workflowcore.ReasonReviewerLaunchRetriesExhausted {
		t.Fatalf("attention reason = %q, want %q", verdict.Reason, workflowcore.ReasonReviewerLaunchRetriesExhausted)
	}
	if verdict.Attention != workflowcore.AttentionHuman || verdict.Action == "" {
		t.Fatalf("exhausted retries must reach a human with a concrete action; got %+v", verdict)
	}
	// And it stays converged: no further launches once it is a human's call.
	f.clk.Advance(time.Hour)
	if _, err := f.c.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun after stop: %v", err)
	}
	if f.launcher.launchCalls != 3 {
		t.Fatalf("launch calls after the stop = %d, want still 3", f.launcher.launchCalls)
	}
}

// TestReviewerLaunchAuthFailureNeedsAttentionImmediately: an invalid credential
// is not something AO may retry — it stops at once, with the precise reason and
// action, and still cleans up its partial review_run.
func TestReviewerLaunchAuthFailureNeedsAttentionImmediately(t *testing.T) {
	f := newLaunchFailureFixture(t, fmt.Errorf("start reviewer pane: %w", ports.ErrChatAuthRequired), nil)
	ctx := context.Background()

	if f.launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1 (auth must not auto-retry)", f.launcher.launchCalls)
	}
	if got := f.runState(t); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	if got := f.runningReviewRuns(); got != 0 {
		t.Fatalf("orphan running review runs = %d, want 0", got)
	}
	rec, ok := f.checkpointWithPhase("reviewer_launch_error")
	if !ok || !strings.Contains(rec.RetryState, `"class":"auth"`) {
		t.Fatalf("auth class not persisted; found=%v retry_state=%q", ok, rec.RetryState)
	}
	if !strings.Contains(rec.RetryState, `"retryable":false`) {
		t.Fatalf("auth failure recorded as retryable; retry_state = %q", rec.RetryState)
	}

	detail, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	verdict := workflowcore.ClassifyAttention(detail, nil, workflowcore.PhaseNeedsAttention)
	if verdict.Reason != workflowcore.ReasonReviewerAuthInvalid || verdict.Attention != workflowcore.AttentionHuman || verdict.Action == "" {
		t.Fatalf("auth stop verdict = %+v, want a human decision named %q with an action", verdict, workflowcore.ReasonReviewerAuthInvalid)
	}

	// Polls must not retry a permanent failure.
	for i := 0; i < 3; i++ {
		f.clk.Advance(time.Minute)
		if _, err := f.c.GetRun(ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("launch calls after polls = %d, want still 1", f.launcher.launchCalls)
	}

	// The human fixes the credential and continues: the same cycle resumes,
	// the run un-parks, and the review proceeds — no duplicate rows anywhere.
	f.launcher.launchErr = nil
	if _, err := f.c.ContinueRun(ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun after fixing auth: %v", err)
	}
	if f.launcher.launchCalls != 2 {
		t.Fatalf("launch calls after human continue = %d, want 2", f.launcher.launchCalls)
	}
	if got := f.runState(t); got != domain.WorkflowRunRunning {
		t.Fatalf("run state after a successful human-driven retry = %q, want running", got)
	}
	detail, err = f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun after resume: %v", err)
	}
	if reviewStepFrom(detail).Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after the human-driven retry")
	}
	if len(f.reviews.reviews) != 1 {
		t.Fatalf("review rows = %d, want 1", len(f.reviews.reviews))
	}
	if entries := f.reviewOutboxEntries(); len(entries) != 1 {
		t.Fatalf("review outbox entries = %d, want exactly 1", len(entries))
	}
	if f.runningReviewRuns() != 1 {
		t.Fatalf("running review runs = %d, want exactly 1", f.runningReviewRuns())
	}
}

// TestReviewerLaunchBinaryMissingNeedsAttention: a missing reviewer CLI is a
// permanent, human-owned stop with its own action — and the preflight failure
// happens after the review_run insert, so the cleanup path is exercised too.
func TestReviewerLaunchBinaryMissingNeedsAttention(t *testing.T) {
	f := newLaunchFailureFixture(t, nil, fmt.Errorf("resolve reviewer binary: %w", ports.ErrAgentBinaryNotFound))
	ctx := context.Background()

	if f.launcher.launchCalls != 0 {
		t.Fatalf("launch calls = %d, want 0 (preflight failed first)", f.launcher.launchCalls)
	}
	if got := f.runningReviewRuns(); got != 0 {
		t.Fatalf("orphan running review runs = %d, want 0", got)
	}
	if got := f.runState(t); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	rec, ok := f.checkpointWithPhase("reviewer_launch_error")
	if !ok || !strings.Contains(rec.RetryState, `"stage":"preflight"`) {
		t.Fatalf("preflight stage not persisted; found=%v retry_state=%q", ok, rec.RetryState)
	}
	detail, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	verdict := workflowcore.ClassifyAttention(detail, nil, workflowcore.PhaseNeedsAttention)
	if verdict.Reason != workflowcore.ReasonReviewerBinaryMissing || verdict.Action == "" {
		t.Fatalf("binary-missing verdict = %+v, want %q with an action", verdict, workflowcore.ReasonReviewerBinaryMissing)
	}
	// Preflight is not retried automatically either.
	f.clk.Advance(time.Minute)
	if _, err := f.c.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if f.launcher.preflightCalls != 1 {
		t.Fatalf("preflight calls = %d, want still 1", f.launcher.preflightCalls)
	}
}

// TestReviewerLaunchRetrySurvivesRestart: the daemon restarts between the failed
// launch and its retry. Boot recovery (Reconcile) must resume the SAME cycle —
// one reviewer, one review row, one outbox entry — not start a second one.
func TestReviewerLaunchRetrySurvivesRestart(t *testing.T) {
	f := newLaunchFailureFixture(t, errors.New("spawn reviewer: connection refused"), nil)
	ctx := context.Background()

	if f.runningReviewRuns() != 0 {
		t.Fatalf("orphan running review runs before restart = %d, want 0", f.runningReviewRuns())
	}

	// Restart: no in-memory state carries over, only the durable rows.
	f.launcher.launchErr = nil
	f.clk.Advance(2 * time.Minute)
	if err := f.c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.launcher.launchCalls != 2 {
		t.Fatalf("launch calls after restart = %d, want 2 (one original, one retry)", f.launcher.launchCalls)
	}
	// A second reconcile (a second restart, or a duplicate boot pass) is a
	// no-op: the reviewer is attached now.
	if err := f.c.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if f.launcher.launchCalls != 2 {
		t.Fatalf("launch calls after a second reconcile = %d, want still 2", f.launcher.launchCalls)
	}
	if f.runningReviewRuns() != 1 {
		t.Fatalf("running review runs = %d, want exactly 1", f.runningReviewRuns())
	}
	if len(f.reviews.reviews) != 1 {
		t.Fatalf("review rows = %d, want 1", len(f.reviews.reviews))
	}
	if entries := f.reviewOutboxEntries(); len(entries) != 1 {
		t.Fatalf("review outbox entries = %d, want exactly 1", len(entries))
	}
	detail, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	step := reviewStepFrom(detail).Step
	if step.ReviewRunID == nil || step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step after restart recovery = %+v, want running with a review run", step)
	}
}

// TestReviewerLaunchRetryProceedsToVerdict proves the recovered cycle is a real
// review, not just a state repair: the retried reviewer's approved verdict
// drives the run forward exactly as an uninterrupted one would.
func TestReviewerLaunchRetryProceedsToVerdict(t *testing.T) {
	f := newLaunchFailureFixture(t, errors.New("runtime unavailable"), nil)
	ctx := context.Background()

	f.launcher.launchErr = nil
	f.clk.Advance(time.Minute)
	detail, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	step := reviewStepFrom(detail).Step
	if step.ReviewRunID == nil {
		t.Fatalf("no review run attached after the automatic retry")
	}

	// The reviewer submits (as the real `ao review submit` would, out of band).
	f.reviews.setStatus(*step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)
	f.clk.Advance(time.Minute)
	detail, err = f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun after verdict: %v", err)
	}
	if got := reviewStepFrom(detail).Step.State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step state after an approved verdict = %q, want completed", got)
	}
}
