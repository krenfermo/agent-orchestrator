package workflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeReviewRuns is a hand-rolled fake for workflowcore.ReviewRuns: no real
// sqlite review_store call in unit tests, but the same natural-key dedupe
// semantics (unique on session+prUrl+targetSha+harness).
type fakeReviewRuns struct {
	reviews map[string]domain.Review    // by "sessionID|harness"
	runs    map[string]domain.ReviewRun // by run id
	seq     int

	// insertCalls counts InsertReviewRun invocations, used to assert a
	// review is created exactly once across repeated dispatch calls.
	insertCalls int
	// forceDuplicate makes the next InsertReviewRun return
	// domain.ErrDuplicateReviewRun instead of inserting, so tests can drive
	// the dedupe-fallback path deterministically.
	forceDuplicate bool
}

func newFakeReviewRuns() *fakeReviewRuns {
	return &fakeReviewRuns{
		reviews: map[string]domain.Review{},
		runs:    map[string]domain.ReviewRun{},
	}
}

func reviewKey(id domain.SessionID, harness domain.ReviewerHarness) string {
	return string(id) + "|" + string(harness)
}

func naturalKey(id domain.SessionID, prURL, targetSHA string, harness domain.ReviewerHarness) string {
	return string(id) + "|" + prURL + "|" + targetSHA + "|" + string(harness)
}

func (f *fakeReviewRuns) GetReviewRun(_ context.Context, id string) (domain.ReviewRun, bool, error) {
	r, ok := f.runs[id]
	return r, ok, nil
}

func (f *fakeReviewRuns) GetReviewBySessionAndHarness(_ context.Context, id domain.SessionID, harness domain.ReviewerHarness) (domain.Review, bool, error) {
	r, ok := f.reviews[reviewKey(id, harness)]
	return r, ok, nil
}

func (f *fakeReviewRuns) UpsertReview(_ context.Context, r domain.Review) error {
	f.reviews[reviewKey(r.SessionID, r.Harness)] = r
	return nil
}

func (f *fakeReviewRuns) InsertReviewRun(_ context.Context, run domain.ReviewRun) error {
	f.insertCalls++
	if f.forceDuplicate {
		f.forceDuplicate = false
		return domain.ErrDuplicateReviewRun
	}
	key := naturalKey(run.SessionID, run.PRURL, run.TargetSHA, run.Harness)
	if _, exists := f.byNaturalKey(key); exists {
		return domain.ErrDuplicateReviewRun
	}
	f.runs[run.ID] = run
	return nil
}

func (f *fakeReviewRuns) byNaturalKey(key string) (domain.ReviewRun, bool) {
	for _, r := range f.runs {
		if naturalKey(r.SessionID, r.PRURL, r.TargetSHA, r.Harness) == key {
			return r, true
		}
	}
	return domain.ReviewRun{}, false
}

func (f *fakeReviewRuns) GetReviewRunBySessionPRSHAAndHarness(_ context.Context, id domain.SessionID, prURL, targetSHA string, harness domain.ReviewerHarness) (domain.ReviewRun, bool, error) {
	r, ok := f.byNaturalKey(naturalKey(id, prURL, targetSHA, harness))
	return r, ok, nil
}

// ListReviewRunsBySession backs Checkpoint 8D's cycle-number derivation.
func (f *fakeReviewRuns) ListReviewRunsBySession(_ context.Context, id domain.SessionID) ([]domain.ReviewRun, error) {
	var out []domain.ReviewRun
	for _, r := range f.runs {
		if r.SessionID == id {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeMessageSender is a hand-rolled fake for workflowcore.MessageSender (no
// real session_manager.Send in unit tests).
type fakeMessageSender struct {
	calls   int
	lastID  domain.SessionID
	lastMsg string
	err     error
}

func (f *fakeMessageSender) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	f.calls++
	f.lastID = id
	f.lastMsg = message
	return f.err
}

// setStatus lets tests simulate the real `ao review submit` CLI call landing
// out-of-band (as it does for real: Claude's own process calls the daemon's
// HTTP endpoint directly, never through workflow's own write path).
func (f *fakeReviewRuns) setStatus(id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict) {
	r := f.runs[id]
	r.Status = status
	r.Verdict = verdict
	f.runs[id] = r
}

// fakeReviewerLauncher is a hand-rolled fake for workflowcore.ReviewerLauncher
// (no real Claude Code process/pane in unit tests).
type fakeReviewerLauncher struct {
	preflightCalls int
	launchCalls    int
	preflightErr   error
	launchErr      error
	lastPrompt     string
	lastReq        workflowcore.ReviewerLaunchRequest
}

func (f *fakeReviewerLauncher) Preflight(_ context.Context, _ domain.ReviewerHarness, _ string) error {
	f.preflightCalls++
	return f.preflightErr
}

func (f *fakeReviewerLauncher) Launch(_ context.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	f.launchCalls++
	f.lastPrompt = req.Prompt
	f.lastReq = req
	if f.launchErr != nil {
		return workflowcore.ReviewerLaunchResult{}, f.launchErr
	}
	return workflowcore.ReviewerLaunchResult{HandleID: fmt.Sprintf("handle-%d", f.launchCalls)}, nil
}

// newCoordinatorWithReview wires a coordinator with 8B's work-step deps plus
// 8C's review deps, all fakes.
func newCoordinatorWithReview(spawner workflowcore.Spawner, sessionFacts workflowcore.SessionFacts, workspaceFacts workflowcore.WorkspaceFacts, reviewRuns *fakeReviewRuns, launcher *fakeReviewerLauncher) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: launcher,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

// completeWorkStep drives a run all the way through StartRun and a
// successful work-step completion (idle worker + dirty worktree evidence),
// returning the run detail once the work step is completed and the review
// step is ready/pending. Shared setup for the review-dispatch tests below.
func completeWorkStep(t *testing.T, c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, sessionFacts *fakeSessionFacts, workspaceFacts *fakeWorkspaceFacts, runID string) workflowcore.RunDetail {
	t.Helper()
	ctx := context.Background()
	detail, err := c.StartRun(ctx, runID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	workspaceFacts.obs.Dirty = true
	clk.Advance(10 * time.Second)
	got, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", workStepFrom(got).Step.State)
	}
	_ = store
	return got
}

// Test: work completed enables review dispatch; before completion,
// ContinueRun is a no-op that never creates a review run.
func TestContinueRunNoOpBeforeWorkCompletes(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, _, _ := newCoordinatorWithReview(spawner, sessionFacts, &fakeWorkspaceFacts{}, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if reviewRuns.insertCalls != 0 {
		t.Fatalf("InsertReviewRun calls = %d, want 0 before work completes", reviewRuns.insertCalls)
	}
	if launcher.launchCalls != 0 {
		t.Fatalf("launch calls = %d, want 0 before work completes", launcher.launchCalls)
	}
	if reviewStepFrom(got).Step.State != domain.WorkflowStepPending {
		t.Fatalf("review step state = %q, want pending", reviewStepFrom(got).Step.State)
	}
}

// Test: review is created exactly once, harness is claude-code, and repeated
// ContinueRun calls are idempotent (no duplicate review_run, no duplicate
// reviewer launch).
func TestContinueRunDispatchesReviewExactlyOnceAndIdempotent(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want running", review.Step.State)
	}
	if review.Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after dispatch")
	}
	if reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls = %d, want 1", reviewRuns.insertCalls)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1", launcher.launchCalls)
	}
	run, ok := reviewRuns.runs[*review.Step.ReviewRunID]
	if !ok {
		t.Fatalf("review run %s not found", *review.Step.ReviewRunID)
	}
	if run.Harness != domain.ReviewerClaudeCode {
		t.Fatalf("review run harness = %q, want claude-code", run.Harness)
	}
	if run.AutoInjectReview {
		t.Fatalf("review run AutoInjectReview = true, want false (no-fix-loop guardrail)")
	}

	// Repeated ContinueRun calls must not create a second review or launch.
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("second ContinueRun: %v", err)
	}
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("third ContinueRun: %v", err)
	}
	if reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls after repeated ContinueRun = %d, want still 1", reviewRuns.insertCalls)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls after repeated ContinueRun = %d, want still 1", launcher.launchCalls)
	}
}

// Test: the launched prompt instructs a plain git status/diff and the exact
// `ao review submit` single-run form, never a PR-centric review.
func TestContinueRunPromptIsWorkflowOwnedNotPRCentric(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if launcher.lastPrompt == "" {
		t.Fatalf("no prompt captured")
	}
	for _, forbidden := range []string{"gh api", "pulls/", "PR base branch", "--reviews"} {
		if contains(launcher.lastPrompt, forbidden) {
			t.Errorf("prompt contains forbidden PR-centric text %q:\n%s", forbidden, launcher.lastPrompt)
		}
	}
	for _, required := range []string{"git status", "git diff", "ao review submit", "--verdict"} {
		if !contains(launcher.lastPrompt, required) {
			t.Errorf("prompt missing required text %q", required)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// Test: approved verdict -> next_action "verify"; changes_requested -> "fix".
// Also asserts zero Spawn calls happen from the changes_requested path (no
// fix loop) and that ContinueRun never dispatches anything toward fix.
func TestReviewVerdictDrivesNextAction(t *testing.T) {
	cases := []struct {
		name       string
		verdict    domain.ReviewVerdict
		status     domain.ReviewRunStatus
		wantStep   domain.WorkflowStepState
		wantRun    domain.WorkflowRunState
		wantAction string
	}{
		{"approved", domain.VerdictApproved, domain.ReviewRunComplete, domain.WorkflowStepCompleted, domain.WorkflowRunWaiting, "verify"},
		// 8C->8D revision: changes_requested now rests the review step at
		// "waiting" (non-terminal), not "completed" — a terminal state would
		// make a second review cycle for the same step impossible. See
		// fix_dispatch_test.go for the full review<->fix cycling coverage.
		{"changes_requested", domain.VerdictChangesRequested, domain.ReviewRunComplete, domain.WorkflowStepWaiting, domain.WorkflowRunWaiting, "fix"},
		{"delivered_approved", domain.VerdictApproved, domain.ReviewRunDelivered, domain.WorkflowStepCompleted, domain.WorkflowRunWaiting, "verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionFacts := newFakeSessionFacts()
			spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
			workspaceFacts := &fakeWorkspaceFacts{}
			reviewRuns := newFakeReviewRuns()
			launcher := &fakeReviewerLauncher{}
			c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
			ctx := context.Background()

			created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
			got, err := c.ContinueRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}
			reviewRunID := *reviewStepFrom(got).Step.ReviewRunID

			// Simulate the real `ao review submit` CLI call landing out of
			// band (as it does for real — this is a live HTTP write into the
			// review_run row, not anything workflow's own code calls).
			reviewRuns.setStatus(reviewRunID, tc.status, tc.verdict)
			clk.Advance(time.Second)

			final, err := c.GetRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			review := reviewStepFrom(final)
			if review.Step.State != tc.wantStep {
				t.Fatalf("review step state = %q, want %q", review.Step.State, tc.wantStep)
			}
			if final.Run.State != tc.wantRun {
				t.Fatalf("run state = %q, want %q", final.Run.State, tc.wantRun)
			}
			if final.NextAction != tc.wantAction {
				t.Fatalf("next action = %q, want %q", final.NextAction, tc.wantAction)
			}
			// No fix loop: zero Spawn calls happened beyond the original work
			// step dispatch (exactly 1, from StartRun).
			if spawner.calls != 1 {
				t.Fatalf("spawner calls = %d, want exactly 1 (no fix-loop spawn)", spawner.calls)
			}
		})
	}
}

// Test: failed/terminal review_run status -> run needs_attention.
func TestReviewRunFailedNeedsAttention(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	reviewRuns.setStatus(reviewRunID, domain.ReviewRunFailed, domain.VerdictNone)
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepFailed {
		t.Fatalf("review step state = %q, want failed", reviewStepFrom(final).Step.State)
	}
	if final.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", final.Run.State)
	}
}

// Test: ambiguous (invalid verdict while complete) -> run needs_attention,
// never silently approved.
func TestReviewRunCompleteWithInvalidVerdictIsAmbiguous(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	reviewRuns.setStatus(reviewRunID, domain.ReviewRunComplete, domain.VerdictNone)
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("review step state = %q, want waiting", reviewStepFrom(final).Step.State)
	}
	if final.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", final.Run.State)
	}
}

// Test: a review run genuinely still running, fresh (under the staleness
// threshold) stays running/waiting — recovery/read must not force it to
// needs_attention prematurely.
func TestReviewStillRunningFreshStaysRunning(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	clk.Advance(time.Minute) // well under the 30-minute staleness threshold

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want still running", reviewStepFrom(final).Step.State)
	}
	if final.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want not needs_attention while review is genuinely fresh", final.Run.State)
	}
}

// Test: a review run stuck at "running" past the staleness threshold ->
// waiting/needs_attention, never silently assumed approved.
func TestReviewStaleRunningNeedsAttention(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	clk.Advance(31 * time.Minute)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("review step state = %q, want waiting", reviewStepFrom(final).Step.State)
	}
	if final.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", final.Run.State)
	}
}
