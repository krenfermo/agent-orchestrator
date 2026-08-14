package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// harnessAwareSpawner fails for one specific harness (simulating a Codex
// provider/capacity failure) and succeeds for every other harness — the
// Checkpoint 8H failure-injection shape: the signal enters through the exact
// same Spawner.Spawn call site production dispatch uses, never by mutating
// tables directly.
type harnessAwareSpawner struct {
	failHarness domain.AgentHarness
	failErr     error
	calls       []domain.AgentHarness
}

func (f *harnessAwareSpawner) Spawn(_ context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	f.calls = append(f.calls, cfg.Harness)
	if cfg.Harness == f.failHarness {
		return domain.SessionRecord{}, 0, 0, f.failErr
	}
	rec := domain.SessionRecord{
		ID:        domain.SessionID(fmt.Sprintf("sess-%d", len(f.calls))),
		ProjectID: cfg.ProjectID,
		Harness:   cfg.Harness,
		Kind:      cfg.Kind,
		IssueID:   cfg.IssueID,
		Metadata:  domain.SessionMetadata{Branch: "wf/step", WorkspacePath: "/tmp/wf-worktree"},
	}
	return rec, len(cfg.Prompt), 0, nil
}

// fakeSwitcher is a minimal workflowcore.AgentSwitcher fake that records
// calls (for idempotency assertions) and returns a fixed, completed
// AgentSwitch — proving the failover path calls through the reused
// session_manager saga interface, without needing a real session manager.
type fakeSwitcher struct {
	calls  []workflowcore.AgentSwitchRequest
	result domain.AgentSwitch
	err    error
	byKey  map[string]domain.AgentSwitch
}

func newFakeSwitcher(target domain.AgentHarness) *fakeSwitcher {
	return &fakeSwitcher{
		result: domain.AgentSwitch{ID: "asw-1", TargetHarness: target, State: domain.AgentSwitchCompleted},
		byKey:  map[string]domain.AgentSwitch{},
	}
}

func (f *fakeSwitcher) SwitchAgent(_ context.Context, id domain.SessionID, cfg workflowcore.AgentSwitchRequest) (domain.AgentSwitch, error) {
	f.calls = append(f.calls, cfg)
	if existing, ok := f.byKey[cfg.IdempotencyKey]; ok {
		return existing, nil
	}
	if f.err != nil {
		return domain.AgentSwitch{}, f.err
	}
	result := f.result
	result.SessionID = id
	f.byKey[cfg.IdempotencyKey] = result
	return result, nil
}

func newCoordinatorWithSwitcher(spawner workflowcore.Spawner, switcher workflowcore.AgentSwitcher) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:    store,
		Spawner:  spawner,
		Switcher: switcher,
		Clock:    clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

var errRateLimited = errors.New("codex: 429 Too Many Requests, rate limit exceeded")

// TestWorkDispatch_CodexRateLimitFailsOverToClaude covers Checkpoint 8H test
// requirement #4: a Codex eligible failure selects Claude, in one
// synchronous dispatch — the Codex attempt is recorded failed (never
// deleted), a second attempt is recorded for claude-code, and the step ends
// up with a live session on the fallback harness.
func TestWorkDispatch_CodexRateLimitFailsOverToClaude(t *testing.T) {
	spawner := &harnessAwareSpawner{failHarness: domain.HarnessCodex, failErr: errRateLimited}
	c, store, _ := newCoordinatorWithSwitcher(spawner, nil)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	if work.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("work step state = %q, want running (fallback succeeded)", work.Step.State)
	}
	if work.Step.SessionID == nil {
		t.Fatalf("work step has no session after fallback success")
	}
	if detail.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run went to needs_attention despite a successful fallback")
	}

	attempts, err := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %+v, want exactly 2 (codex failed, claude-code succeeded)", attempts)
	}
	if attempts[0].Harness != "codex" || attempts[0].Outcome != domain.WorkflowAttemptFailed || attempts[0].ErrorClass != domain.WorkflowErrorRateLimited {
		t.Fatalf("attempt 1 = %+v, want codex/failed/rate_limited", attempts[0])
	}
	if attempts[1].Harness != "claude-code" || attempts[1].Outcome != "" {
		t.Fatalf("attempt 2 = %+v, want claude-code/running(no outcome yet)", attempts[1])
	}
	if len(spawner.calls) != 2 || spawner.calls[0] != domain.HarnessCodex || spawner.calls[1] != domain.HarnessClaudeCode {
		t.Fatalf("spawner calls = %v, want [codex claude-code]", spawner.calls)
	}
}

// TestWorkDispatch_NonEligibleFailureDoesNotFailOver covers test requirement
// #3: a failure that is not a provider/capacity failure (here, an
// unclassified generic error) must never trigger automatic failover — only
// one Spawn call, step fails, run needs attention.
func TestWorkDispatch_NonEligibleFailureDoesNotFailOver(t *testing.T) {
	spawner := &harnessAwareSpawner{failHarness: domain.HarnessCodex, failErr: errors.New("some internal bug")}
	c, store, _ := newCoordinatorWithSwitcher(spawner, nil)
	ctx := context.Background()

	created, _ := c.CreateRun(ctx, "proj-1", "ship the thing")
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", detail.Run.State)
	}
	work := workStepFrom(detail)
	if work.Step.State != domain.WorkflowStepFailed {
		t.Fatalf("work step state = %q, want failed", work.Step.State)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawner calls = %v, want exactly 1 (no failover attempted)", spawner.calls)
	}
	attempts, _ := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if len(attempts) != 1 || attempts[0].ErrorClass != domain.WorkflowErrorAgentStartFailed {
		t.Fatalf("attempts = %+v, want exactly 1 agent_start_failed", attempts)
	}
}

// TestWorkDispatch_BudgetExhaustionStopsFailover covers test requirements
// #10/#11: with MaxWorkProviderAttempts=1, a Codex failure must not attempt
// any fallback even though one would otherwise be eligible — the budget is
// enforced before the fallback harness is even chosen.
func TestWorkDispatch_BudgetExhaustionStopsFailover(t *testing.T) {
	spawner := &harnessAwareSpawner{failHarness: domain.HarnessCodex, failErr: errRateLimited}
	c, store, _ := newCoordinatorWithSwitcher(spawner, nil)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing", workflowcore.VerificationPlan{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Overwrite the policy snapshot with a budget of exactly 1 attempt.
	run, ok, err := store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v, ok=%v", err, ok)
	}
	run.PolicySnapshot = `{"version":"v1","maxFixCycles":3,"maxWorkProviderAttempts":1,"maxReviewProviderAttempts":3}`
	store.runs[run.ID] = run

	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention (budget exhausted)", detail.Run.State)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawner calls = %v, want exactly 1 (budget=1 forbids fallback)", spawner.calls)
	}
	work := workStepFrom(detail)
	attempts, _ := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if len(attempts) != 1 || attempts[0].Harness != "codex" {
		t.Fatalf("attempts = %+v, want exactly 1 codex attempt", attempts)
	}
}

// TestReportWorkStepProviderFailure_LiveSessionSwitchesAgent covers test
// requirements #8/#9/#18: a live-session provider failure uses the reused
// session_manager.SwitchAgent path (via the fakeSwitcher), the Codex attempt
// is preserved (not deleted), a new attempt is opened for Claude, and the
// same session id is reused (worktree/branch/session identity preserved —
// no second session is spawned).
func TestReportWorkStepProviderFailure_LiveSessionSwitchesAgent(t *testing.T) {
	spawner := &harnessAwareSpawner{}
	switcher := newFakeSwitcher(domain.HarnessClaudeCode)
	c, store, clk := newCoordinatorWithSwitcher(spawner, switcher)
	ctx := context.Background()

	created, _ := c.CreateRun(ctx, "proj-1", "ship the thing")
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	if work.Step.SessionID == nil {
		t.Fatalf("expected a live session after a successful codex spawn")
	}
	liveSessionID := *work.Step.SessionID

	clk.Advance(time.Minute)
	updated, err := c.ReportWorkStepProviderFailure(ctx, created.Run.ID, work.Step.ID, errRateLimited)
	if err != nil {
		t.Fatalf("ReportWorkStepProviderFailure: %v", err)
	}
	if updated.SessionID == nil || *updated.SessionID != liveSessionID {
		t.Fatalf("session id changed after failover: got %v, want unchanged %q (worktree/session identity must be preserved)", updated.SessionID, liveSessionID)
	}
	if len(switcher.calls) != 1 || switcher.calls[0].TargetHarness != domain.HarnessClaudeCode {
		t.Fatalf("switcher calls = %+v, want exactly one call targeting claude-code", switcher.calls)
	}

	attempts, err := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %+v, want exactly 2 (codex preserved, claude-code opened)", attempts)
	}
	if attempts[0].Harness != "codex" || attempts[0].Outcome != domain.WorkflowAttemptFailed || attempts[0].ErrorClass != domain.WorkflowErrorRateLimited {
		t.Fatalf("attempt 1 (must be preserved) = %+v", attempts[0])
	}
	if attempts[1].Harness != "claude-code" {
		t.Fatalf("attempt 2 = %+v, want harness claude-code", attempts[1])
	}
}

// TestReportWorkStepProviderFailure_IdempotentAcrossDuplicateReports covers
// test requirement #7: reconciling/reporting the same failure twice must not
// produce two switches or two attempt rows — SwitchAgent's own idempotency
// key does the work; the workflow layer must derive the same key both times
// and must not blindly re-run its own bookkeeping either.
func TestReportWorkStepProviderFailure_IdempotentAcrossDuplicateReports(t *testing.T) {
	spawner := &harnessAwareSpawner{}
	switcher := newFakeSwitcher(domain.HarnessClaudeCode)
	c, store, _ := newCoordinatorWithSwitcher(spawner, switcher)
	ctx := context.Background()

	created, _ := c.CreateRun(ctx, "proj-1", "ship the thing")
	detail, _ := c.StartRun(ctx, created.Run.ID)
	work := workStepFrom(detail)

	if _, err := c.ReportWorkStepProviderFailure(ctx, created.Run.ID, work.Step.ID, errRateLimited); err != nil {
		t.Fatalf("first report: %v", err)
	}
	afterFirst, _ := store.ListWorkflowAttempts(ctx, work.Step.ID)

	// Second report of the exact same failure for the same (now-terminal)
	// attempt must be a no-op: the attempt already has a terminal outcome.
	if _, err := c.ReportWorkStepProviderFailure(ctx, created.Run.ID, work.Step.ID, errRateLimited); err != nil {
		t.Fatalf("second report: %v", err)
	}
	afterSecond, _ := store.ListWorkflowAttempts(ctx, work.Step.ID)

	if len(afterFirst) != len(afterSecond) {
		t.Fatalf("attempt count changed on duplicate report: %d -> %d, want no change", len(afterFirst), len(afterSecond))
	}
	if len(switcher.calls) != 1 {
		t.Fatalf("switcher calls = %d, want exactly 1 despite two reports (no duplicate switch)", len(switcher.calls))
	}
}

// TestReportWorkStepProviderFailure_CancelledRunPreventsFailover covers test
// requirement #12: a cancelled run must never fail over, even if a failure
// signal arrives afterward.
func TestReportWorkStepProviderFailure_CancelledRunPreventsFailover(t *testing.T) {
	spawner := &harnessAwareSpawner{}
	switcher := newFakeSwitcher(domain.HarnessClaudeCode)
	c, _, _ := newCoordinatorWithSwitcher(spawner, switcher)
	ctx := context.Background()

	created, _ := c.CreateRun(ctx, "proj-1", "ship the thing")
	detail, _ := c.StartRun(ctx, created.Run.ID)
	work := workStepFrom(detail)

	if _, err := c.CancelRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if _, err := c.ReportWorkStepProviderFailure(ctx, created.Run.ID, work.Step.ID, errRateLimited); err != nil {
		t.Fatalf("ReportWorkStepProviderFailure after cancel: %v", err)
	}
	if len(switcher.calls) != 0 {
		t.Fatalf("switcher calls = %d, want 0: cancellation must prevent failover", len(switcher.calls))
	}
}

// TestReportWorkStepProviderFailure_SwitchRejectedNeedsAttention covers a
// SwitchAgent-level rejection (e.g. session blocked/ambiguous): workflow must
// never retry blindly and must surface needs_attention instead.
func TestReportWorkStepProviderFailure_SwitchRejectedNeedsAttention(t *testing.T) {
	spawner := &harnessAwareSpawner{}
	switcher := newFakeSwitcher(domain.HarnessClaudeCode)
	switcher.err = errors.New("switch agent: source session blocked on approval")
	c, store, _ := newCoordinatorWithSwitcher(spawner, switcher)
	ctx := context.Background()

	created, _ := c.CreateRun(ctx, "proj-1", "ship the thing")
	detail, _ := c.StartRun(ctx, created.Run.ID)
	work := workStepFrom(detail)

	updated, err := c.ReportWorkStepProviderFailure(ctx, created.Run.ID, work.Step.ID, errRateLimited)
	if err != nil {
		t.Fatalf("ReportWorkStepProviderFailure: %v", err)
	}
	if updated.State != domain.WorkflowStepWaiting {
		t.Fatalf("step state = %q, want waiting", updated.State)
	}
	run, _, _ := store.GetWorkflowRun(ctx, created.Run.ID)
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", run.State)
	}
}
