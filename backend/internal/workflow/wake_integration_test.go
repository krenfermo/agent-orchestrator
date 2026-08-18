package workflow_test

// Checkpoint 8N.1 integration tests: proving, against the real production
// dispatch paths (not wake.Scheduler in isolation), that worker/reviewer/
// decision-resolver capacity waits each produce a durable wake row, and that
// the daemon poller — never a browser GET/ContinueRun call — is what fires
// it and resumes the run. All tests here drive progress purely by advancing
// a fake clock and calling wakepoller.Poller.RunDueOnce, the same primitive
// the daemon's own 20s ticker calls in production (see wakepoller/poller.go).

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowports "github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wakepoller"
)

// wakeTestSpawner is a Spawner that, unlike dispatch_test.go's fakes, writes
// a real row into a real *sqlite.Store on every call — required for these
// tests because a real store enforces the FOREIGN KEY from
// workflow_steps.session_id to sessions.id that an in-memory fakeStore does
// not, and these tests need a real store anyway to wire a real
// wake.Scheduler (which requires the sqlite Store interface, not the
// package's hand-rolled fakeStore).
type wakeTestSpawner struct {
	store *sqlite.Store
	calls []domain.AgentHarness
}

func (s *wakeTestSpawner) Spawn(ctx context.Context, cfg workflowports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.calls = append(s.calls, cfg.Harness)
	rec := domain.SessionRecord{
		ID:        domain.SessionID("sess-" + strconv.Itoa(len(s.calls))),
		ProjectID: cfg.ProjectID,
		Kind:      cfg.Kind,
		Harness:   cfg.Harness,
		IssueID:   cfg.IssueID,
		Activity:  domain.Activity{State: domain.ActivityIdle},
		Metadata:  domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	created, err := s.store.CreateSession(ctx, rec)
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	return created, len(cfg.Prompt), 0, nil
}

func wakeIntIDSeq(prefix string) func() string {
	n := 0
	return func() string {
		n++
		return prefix + string(rune('0'+n))
	}
}

// TestWorkerCapacityWake_HeadlessPollerRedispatchesExactlyOnceAndRunGoesRunning
// covers Checkpoint 8N.1's central end-to-end claim for the worker role:
// capacity unavailable -> durable wait -> scheduled wake -> daemon poller ->
// capacity re-evaluated -> exactly one real dispatch -> run visibly Running
// immediately (test-matrix item K), with zero GetRun/ContinueRun calls
// driving progress (item L).
func TestWorkerCapacityWake_HeadlessPollerRedispatchesExactlyOnceAndRunGoesRunning(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	spawner := &wakeTestSpawner{store: store}
	wakeSched := wake.New(store, clk.Now, wakeIntIDSeq("wk"), wake.Config{})
	coord := workflowcore.New(workflowcore.Deps{
		Store:         store,
		Projects:      store,
		Spawner:       spawner,
		SessionFacts:  store,
		WakeScheduler: wakeSched,
		Clock:         clk.Now,
		NewID:         wakeIntIDSeq("id"),
	})

	// Both harnesses in cooldown with a real, known reset time.
	reset := clk.Now().Add(45 * time.Minute)
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
			ID: "ahe-" + string(h), Harness: h, State: domain.AgentHealthCooldown,
			Reason: "test", CooldownUntil: &reset, CreatedAt: clk.Now(),
		}); err != nil {
			t.Fatalf("RecordAgentHealthEvent(%s): %v", h, err)
		}
	}

	created, err := coord.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := coord.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting", detail.Run.State)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawner calls = %v, want zero before capacity recovers", spawner.calls)
	}

	// A durable wake row must exist for this run — never just an in-memory
	// wait — proving the real dispatchFromPending/markRunWaitingForCapacity
	// path (not a hand-rolled test double) actually calls Schedule.
	next, err := wakeSched.NextForRun(ctx, domain.WorkflowRunID(created.Run.ID))
	if err != nil || next == nil {
		t.Fatalf("expected a durable wake scheduled for the run, got %+v err=%v", next, err)
	}
	if next.Reason != wake.ReasonWorkerCapacity {
		t.Fatalf("wake reason = %q, want worker_capacity", next.Reason)
	}

	// Claude Code recovers by the known reset time — the production fact
	// source (AgentHealthEvent), never a manually forced run state.
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered", Harness: domain.HarnessClaudeCode, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}

	// Advance the fake clock to the known-reset wake and drive progress
	// PURELY via the poller's RunDueOnce — no GetRun, no ContinueRun call
	// from this test. This is the headless/no-browser-GET proof (item L).
	clk.Advance(46 * time.Minute)
	poller := wakepoller.New(wakeSched, coord, wakepoller.Config{Clock: clk.Now})
	n, err := poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("claimed = %d, want exactly 1", n)
	}

	if len(spawner.calls) != 1 {
		t.Fatalf("spawner calls = %v, want exactly 1 (no duplicate dispatch)", spawner.calls)
	}
	if spawner.calls[0] != domain.HarnessClaudeCode {
		t.Fatalf("spawned harness = %q, want claude-code", spawner.calls[0])
	}

	// The Waiting -> Running fix: run state must already read Running right
	// after the poller's single RunDueOnce call, with no further read-time
	// pass needed to "notice" the redispatch.
	got, _, err := store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if got.State != domain.WorkflowRunRunning {
		t.Fatalf("run state = %q, want running immediately after successful redispatch", got.State)
	}

	// The wake must be closed out, not left dangling or re-firing.
	clk.Advance(24 * time.Hour)
	n, err = poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce after completion: %v", err)
	}
	if n != 0 {
		t.Fatalf("claimed after completion = %d, want 0", n)
	}

	// Checkpoint 8N.1 §A: the wake-driven redispatch must have gone through
	// SessionLifecyclePolicy explicitly (applyWorkLifecycleDecision), not
	// spawned a session just because a wake happened to fire — assert the
	// durable session_lifecycle_decision checkpoint exists and says
	// NEW_SESSION for the correct reason (no session existed yet for this
	// step; that is DecideSessionLifecycle's own first, most certain rule,
	// not something invented here).
	cps, err := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	foundLifecycleDecision := false
	for _, cp := range cps {
		if cp.DurablePhase != "session_lifecycle_decision" {
			continue
		}
		decision, _, ok := workflowcore.DecodeSessionLifecycleDecisionForTest(cp.RetryState)
		if !ok {
			t.Fatalf("session_lifecycle_decision checkpoint did not decode: %+v", cp)
		}
		if decision.Role == domain.WorkflowRoleWorker && decision.Action == domain.LifecycleNewSession {
			foundLifecycleDecision = true
		}
	}
	if !foundLifecycleDecision {
		t.Fatalf("expected an explicit worker NEW_SESSION session_lifecycle_decision checkpoint, found none among %d checkpoints", len(cps))
	}

	// Idempotency: reconciling again after the wake already resumed the run
	// must never spawn a second session (dispatchWorkStep's SessionID-nil
	// guard makes this a structural no-op, not a race we got lucky on).
	if err := coord.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after resume: %v", err)
	}
	if err := coord.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawner calls after repeated Reconcile = %v, want still exactly 1 (no duplicate session)", spawner.calls)
	}
}

// TestReviewerCapacityWake_HeadlessPollerLaunchesReviewerExactlyOnce proves
// the same loop for the reviewer role, sharing scheduleCapacityWake's
// production code path with the worker test above but exercised through
// review dispatch specifically (ReasonReviewerCapacity, not
// ReasonWorkerCapacity).
func TestReviewerCapacityWake_HeadlessPollerLaunchesReviewerExactlyOnce(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	spawner := &wakeTestSpawner{store: store}
	workspaceFacts := &fakeWorkspaceFacts{}
	launcher := &fakeReviewerLauncher{}
	wakeSched := wake.New(store, clk.Now, wakeIntIDSeq("wk"), wake.Config{})
	coord := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Projects:         store,
		Spawner:          spawner,
		SessionFacts:     store,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       store,
		ReviewerLauncher: launcher,
		WakeScheduler:    wakeSched,
		Clock:            clk.Now,
		NewID:            wakeIntIDSeq("id"),
	})

	created, err := coord.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := coord.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	if work.Step.SessionID == nil {
		t.Fatalf("expected the work step to dispatch (no cooldown on worker harnesses)")
	}
	// wakeTestSpawner already spawns the session as Idle, the fact
	// observeWorkStep needs alongside a dirty workspace to conclude the work
	// step is done.
	workspaceFacts.obs.Dirty = true

	// Whichever harness the reviewer would need is put in cooldown BEFORE
	// the work step is observed as complete, so review dispatch itself
	// parks on capacity rather than launching.
	reset := clk.Now().Add(20 * time.Minute)
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
			ID: "ahe-" + string(h), Harness: h, State: domain.AgentHealthCooldown,
			Reason: "test", CooldownUntil: &reset, CreatedAt: clk.Now(),
		}); err != nil {
			t.Fatalf("RecordAgentHealthEvent(%s): %v", h, err)
		}
	}

	clk.Advance(10 * time.Second)
	// ContinueRun, not a plain GetRun: GetRun's own opportunistic review
	// dispatch deliberately excludes cycle 1 (see workflow.go's
	// includeCycle1Unblock=false at its own advanceReviewFixCycle call) —
	// only ContinueRun/Reconcile ever attempt the first review dispatch.
	// This single call is the equivalent of whatever real trigger notices
	// the worker went idle; everything after it must run through the
	// poller alone, matching the worker test's use of StartRun as its one
	// non-poller trigger.
	got, err := coord.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", workStepFrom(got).Step.State)
	}
	if launcher.launchCalls != 0 {
		t.Fatalf("launcher calls = %d, want 0 before capacity recovers", launcher.launchCalls)
	}

	next, err := wakeSched.NextForRun(ctx, domain.WorkflowRunID(created.Run.ID))
	if err != nil || next == nil {
		t.Fatalf("expected a durable wake scheduled for the run, got %+v err=%v", next, err)
	}
	if next.Reason != wake.ReasonReviewerCapacity {
		t.Fatalf("wake reason = %q, want reviewer_capacity", next.Reason)
	}

	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered", Harness: domain.HarnessCodex, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered-2", Harness: domain.HarnessClaudeCode, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}

	clk.Advance(21 * time.Minute)
	poller := wakepoller.New(wakeSched, coord, wakepoller.Config{Clock: clk.Now})
	n, err := poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("claimed = %d, want exactly 1", n)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1 (no duplicate dispatch)", launcher.launchCalls)
	}
}

// seedRunWithPolicyAndReviewStep is seedRunWithPolicy
// (decision_resolver_wiring_test.go) plus a pending review step: ContinueRun
// (workflow.go) requires both a work and a review step to exist on any run
// it's asked to resume, which every real Coordinator.CreateRun-created run
// always has — seedRunWithPolicy's own minimal single-step fixture is fine
// for tests that only ever call GetRun, but the wake-integration tests here
// resume via the poller's ContinueRun call, so they need the second step too.
func seedRunWithPolicyAndReviewStep(t *testing.T, ctx context.Context, store *sqlite.Store, policyJSON string) (domain.WorkflowRun, string) {
	t.Helper()
	now := time.Now().UTC()
	runID := "wf-" + t.Name()
	workStepID := "step-work-" + t.Name()
	reviewStepID := "step-review-" + t.Name()
	run := domain.WorkflowRun{
		ID:             runID,
		ProjectID:      "p",
		Objective:      "objective",
		State:          domain.WorkflowRunRunning,
		PolicyVersion:  "v1",
		PolicySnapshot: policyJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	steps := []domain.WorkflowStep{
		{
			ID: workStepID, WorkflowRunID: runID, Kind: domain.WorkflowStepWork,
			Ordinal: 1, State: domain.WorkflowStepRunning, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: reviewStepID, WorkflowRunID: runID, Kind: domain.WorkflowStepReview,
			Ordinal: 2, State: domain.WorkflowStepPending, CreatedAt: now, UpdatedAt: now,
		},
	}
	inserted, _, err := store.CreateWorkflowRun(ctx, run, steps)
	if err != nil {
		t.Fatalf("CreateWorkflowRun: %v", err)
	}
	return inserted, workStepID
}

// TestDecisionResolverCapacityWake_HeadlessPollerRetriesExactlyOnce proves
// the fix this checkpoint made to decision_resolver_wiring.go: before it, a
// resolver capacity wait was purely read-time-derived (only ever retried by
// another GetRun call, i.e. a browser polling the UI) with no durable wake
// at all. This test proves a wake now gets scheduled and the poller (not a
// second GetRun from this test) is what drives the retry.
func TestDecisionResolverCapacityWake_HeadlessPollerRetriesExactlyOnce(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	clk := &fakeClock{t: time.Date(2026, time.August, 16, 9, 0, 0, 0, time.UTC)}
	launcher := &fakeDecisionResolverLauncher{}
	wakeSched := wake.New(store, clk.Now, wakeIntIDSeq("wk"), wake.Config{})
	coord := workflowcore.New(workflowcore.Deps{
		Store:                    store,
		Projects:                 store,
		SessionFacts:             newFakeSessionFacts(),
		QuestionsStore:           store,
		DecisionResolverLauncher: launcher,
		WakeScheduler:            wakeSched,
		Clock:                    clk.Now,
	})

	run, stepID := seedRunWithPolicyAndReviewStep(t, ctx, store, `{"version":"v1","maxFixCycles":3}`)
	seedResolvingQuestion(t, ctx, store, run.ID, stepID, domain.HarnessClaudeCode)

	// Codex (the only cross-provider resolver option, same-provider not
	// allowed by default policy) is unavailable.
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-1", Harness: domain.HarnessCodex, State: domain.AgentHealthUnavailable, Reason: "test", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent: %v", err)
	}

	if _, err := coord.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(launcher.calls) != 0 {
		t.Fatalf("launcher calls = %d, want 0 before capacity recovers", len(launcher.calls))
	}

	next, err := wakeSched.NextForRun(ctx, domain.WorkflowRunID(run.ID))
	if err != nil || next == nil {
		t.Fatalf("expected a durable wake scheduled for the resolver wait, got %+v err=%v", next, err)
	}
	if next.Reason != wake.ReasonQuestionResolverCapacity {
		t.Fatalf("wake reason = %q, want question_resolver_capacity", next.Reason)
	}

	// Codex recovers.
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered", Harness: domain.HarnessCodex, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}

	next, err = wakeSched.NextForRun(ctx, domain.WorkflowRunID(run.ID))
	if err != nil || next == nil {
		t.Fatalf("expected the wake still open, got %+v err=%v", next, err)
	}
	clk.Advance(next.ScheduledAt.Sub(clk.Now()) + time.Second)

	poller := wakepoller.New(wakeSched, coord, wakepoller.Config{Clock: clk.Now})
	n, err := poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("claimed = %d, want exactly 1", n)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1 (no duplicate dispatch)", len(launcher.calls))
	}
}

// flakyPlanner fails with a rate-limit-shaped error the first N calls, then
// succeeds — the planner-side equivalent of harnessAwareSpawner's controlled
// capacity toggling.
type flakyPlanner struct {
	failCount int
	plan      workflowcore.MasterPlan
	calls     int
}

func (p *flakyPlanner) Generate(context.Context, workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	p.calls++
	if p.calls <= p.failCount {
		return workflowcore.PlannerResponse{}, errors.New("planner: 429 rate limit exceeded, please retry later")
	}
	return workflowcore.PlannerResponse{Plan: p.plan, Provider: "fake", Model: "fake-v1"}, nil
}
func (p *flakyPlanner) Descriptor() (string, string) { return "fake", "fake-v1" }

// TestPlannerCapacityWake_HeadlessPollerRetriesPlanningExactlyUntilSuccess
// proves Checkpoint 8N.1's §B claim: a capacity-shaped planner failure must
// park durably (never needs_attention immediately, never a permanently
// invalidated plan) and the daemon poller — not a human re-triggering
// GeneratePlan — must be what successfully retries it once capacity is back.
func TestPlannerCapacityWake_HeadlessPollerRetriesPlanningExactlyUntilSuccess(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	project := domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	planner := &flakyPlanner{failCount: 2, plan: validMasterPlan()}
	wakeSched := wake.New(store, clk.Now, wakeIntIDSeq("wk"), wake.Config{})
	coord := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Planner: planner, PlannerContextBuilder: staticContext{},
		WakeScheduler: wakeSched, Clock: clk.Now,
		NewID: wakeIntIDSeq("id"),
	})

	created, err := coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}

	// First attempt: planner rate-limited.
	if _, err := coord.GeneratePlan(ctx, created.Run.ID); err != nil {
		t.Fatalf("GeneratePlan (1st, capacity-parked): %v", err)
	}
	if planner.calls != 1 {
		t.Fatalf("planner calls = %d, want 1", planner.calls)
	}

	next, err := wakeSched.NextForRun(ctx, domain.WorkflowRunID(created.Run.ID))
	if err != nil || next == nil {
		t.Fatalf("expected a durable planner_capacity wake, got %+v err=%v", next, err)
	}
	if next.Reason != wake.ReasonPlannerCapacity {
		t.Fatalf("wake reason = %q, want planner_capacity", next.Reason)
	}

	poller := wakepoller.New(wakeSched, coord, wakepoller.Config{Clock: clk.Now})

	// Second cycle: still rate-limited (failCount=2) -> reschedule, not give up.
	clk.Advance(next.ScheduledAt.Sub(clk.Now()) + time.Second)
	if n, err := poller.RunDueOnce(ctx); err != nil || n != 1 {
		t.Fatalf("cycle 2 RunDueOnce: n=%d err=%v", n, err)
	}
	if planner.calls != 2 {
		t.Fatalf("planner calls after cycle 2 = %d, want 2", planner.calls)
	}

	next, err = wakeSched.NextForRun(ctx, domain.WorkflowRunID(created.Run.ID))
	if err != nil || next == nil {
		t.Fatalf("expected the wake still open after 2nd failure, got %+v err=%v", next, err)
	}

	// Third cycle: capacity is back -> planning succeeds, exactly once more.
	clk.Advance(next.ScheduledAt.Sub(clk.Now()) + time.Second)
	if n, err := poller.RunDueOnce(ctx); err != nil || n != 1 {
		t.Fatalf("cycle 3 RunDueOnce: n=%d err=%v", n, err)
	}
	if planner.calls != 3 {
		t.Fatalf("planner calls after recovery = %d, want exactly 3 (no duplicate generation)", planner.calls)
	}

	plan, isMaster, err := store.GetWorkflowPlan(ctx, created.Run.ID)
	if err != nil || !isMaster {
		t.Fatalf("GetWorkflowPlan: plan=%+v isMaster=%v err=%v", plan, isMaster, err)
	}
	if plan.Status != domain.WorkflowPlanValidated {
		t.Fatalf("plan status = %q, want validated", plan.Status)
	}

	// The wake must be closed out, not left re-firing forever.
	clk.Advance(24 * time.Hour)
	if n, err := poller.RunDueOnce(ctx); err != nil || n != 0 {
		t.Fatalf("RunDueOnce after success: n=%d err=%v", n, err)
	}
}

// TestHumanRequiredQuestion_NeverSchedulesWake proves test-matrix item 15:
// a question already forced to human_required must never produce a durable
// wake, no matter how many times the run is read/resumed — only an
// AUTO_RESOLVABLE question still at state=resolving ever reaches
// dispatchDecisionResolver (see reconcileDecisionResolvers's state filter),
// so this is really a proof that the filter itself is airtight, not a
// separate suppression rule that could regress independently.
func TestHumanRequiredQuestion_NeverSchedulesWake(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	clk := &fakeClock{t: time.Date(2026, time.August, 16, 9, 0, 0, 0, time.UTC)}
	wakeSched := wake.New(store, clk.Now, wakeIntIDSeq("wk"), wake.Config{})
	coord := workflowcore.New(workflowcore.Deps{
		Store:          store,
		Projects:       store,
		SessionFacts:   newFakeSessionFacts(),
		QuestionsStore: store,
		WakeScheduler:  wakeSched,
		Clock:          clk.Now,
	})

	run, stepID := seedRunWithPolicyAndReviewStep(t, ctx, store, `{"version":"v1","maxFixCycles":3}`)
	q := seedResolvingQuestion(t, ctx, store, run.ID, stepID, domain.HarnessClaudeCode)
	if _, err := store.TransitionWorkflowQuestionState(ctx, string(q.ID), domain.QuestionStateResolving, domain.QuestionStateHumanRequired, "test forced", clk.Now()); err != nil {
		t.Fatalf("force human_required: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := coord.GetRun(ctx, run.ID); err != nil {
			t.Fatalf("GetRun[%d]: %v", i, err)
		}
		clk.Advance(time.Hour)
	}

	next, err := wakeSched.NextForRun(ctx, domain.WorkflowRunID(run.ID))
	if err != nil {
		t.Fatalf("NextForRun: %v", err)
	}
	if next != nil {
		t.Fatalf("expected zero wakes for a human_required question, got %+v", next)
	}
}

// TestReviewerCapacityStall_MidSessionDetectsAndRecovers is Checkpoint
// 8P-D.3's regression for the real failure Checkpoint 8P-D.2 surfaced: a
// reviewer session that dispatches fine, then goes idle (its own CLI turn
// ends) without ever calling `ao review submit` — the exact signature a
// mid-review provider usage-limit hit leaves behind (confirmed from the real
// Codex transcript: a text "approved" opinion, but the submit tool call
// itself was blocked by the exhausted quota, so review_run.status never left
// "running"). This proves: (1) it is detected promptly (grace-window
// seconds, never reviewStalenessThreshold's 30 minutes), (2) the stalled
// review_run is durably closed out with its verdict still empty — never
// fabricated as approved from the model's own prose, (3) a scoped
// AgentHealthEvent lands the harness in cooldown, (4) with no eligible
// independent fallback (only codex/claude-code exist, claude-code is the
// implementer), the run parks in Waiting behind a durable reviewer_capacity
// wake rather than needs_attention, and (5) once capacity recovers, the
// headless poller alone (no GetRun/ContinueRun) redispatches exactly once
// more — a fresh review_run, not a duplicate/zombie reviewer.
func TestReviewerCapacityStall_MidSessionDetectsAndRecovers(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	spawner := &wakeTestSpawner{store: store}
	workspaceFacts := &fakeWorkspaceFacts{}
	launcher := &fakeReviewerLauncher{}
	wakeSched := wake.New(store, clk.Now, wakeIntIDSeq("wk"), wake.Config{})
	coord := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Projects:         store,
		Spawner:          spawner,
		SessionFacts:     store,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       store,
		ReviewerLauncher: launcher,
		WakeScheduler:    wakeSched,
		Clock:            clk.Now,
		NewID:            wakeIntIDSeq("id"),
	})

	created, err := coord.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := coord.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	if work.Step.SessionID == nil {
		t.Fatalf("expected the work step to dispatch")
	}
	workerSessionID := domain.SessionID(*work.Step.SessionID)
	workspaceFacts.obs.Dirty = true

	clk.Advance(10 * time.Second)
	got, err := coord.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want running", review.Step.State)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1 (real dispatch, no pre-existing cooldown)", launcher.launchCalls)
	}
	if review.Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after dispatch")
	}
	originalRunID := *review.Step.ReviewRunID
	originalRun, ok, err := store.GetReviewRun(ctx, originalRunID)
	if err != nil || !ok {
		t.Fatalf("GetReviewRun(%s): ok=%v err=%v", originalRunID, ok, err)
	}
	if originalRun.Harness != domain.ReviewerCodex {
		t.Fatalf("reviewer harness = %q, want codex (cross-provider from the claude-code worker)", originalRun.Harness)
	}

	// Simulate the reviewer's own session going idle with no verdict, the
	// real signature a usage-limit hit leaves behind — its Stop hook fires
	// (turn ended), but `ao review submit` never lands. Fetch-then-write
	// through the real activity-signal path (not a raw column poke) so this
	// exercises the exact same CAS-guarded update production hooks use.
	sess, found, err := store.GetSession(ctx, workerSessionID)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): found=%v err=%v", workerSessionID, found, err)
	}
	clk.Advance(1 * time.Second)
	sess.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: clk.Now()}
	sess.UpdatedAt = clk.Now()
	if _, err := store.UpdateSessionFromActivitySignal(ctx, sess); err != nil {
		t.Fatalf("UpdateSessionFromActivitySignal: %v", err)
	}

	// Well past reviewerStallGrace, nowhere near reviewStalenessThreshold.
	clk.Advance(25 * time.Second)
	got, err = coord.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	// The original review_run must be closed out, verdict still empty —
	// never fabricated as approved from the model's own text.
	closed, ok, err := store.GetReviewRun(ctx, originalRunID)
	if err != nil || !ok {
		t.Fatalf("GetReviewRun(%s) after stall: ok=%v err=%v", originalRunID, ok, err)
	}
	if closed.Status != domain.ReviewRunCancelled {
		t.Fatalf("stalled review_run status = %q, want cancelled", closed.Status)
	}
	if closed.Verdict != domain.VerdictNone {
		t.Fatalf("stalled review_run verdict = %q, want empty (never fabricated)", closed.Verdict)
	}

	// A scoped capacity signal landed for codex, in cooldown.
	health, ok, err := store.GetAgentHealth(ctx, domain.HarnessCodex)
	if err != nil || !ok {
		t.Fatalf("GetAgentHealth(codex): ok=%v err=%v", ok, err)
	}
	if health.State != domain.AgentHealthCooldown {
		t.Fatalf("codex health state = %q, want cooldown", health.State)
	}
	if health.FailureClass != domain.WorkflowErrorCapacityExhausted {
		t.Fatalf("codex health failure class = %q, want capacity_exhausted", health.FailureClass)
	}

	// No eligible independent fallback exists (claude-code is the
	// implementer's own provider) — the run parks in Waiting, never
	// needs_attention, and never the 30-minute blind wait.
	if got.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting (never needs_attention for a capacity signal)", got.Run.State)
	}
	if reviewStepFrom(got).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("review step state = %q, want waiting", reviewStepFrom(got).Step.State)
	}

	next, err := wakeSched.NextForRun(ctx, domain.WorkflowRunID(created.Run.ID))
	if err != nil || next == nil {
		t.Fatalf("expected a durable reviewer_capacity wake, got %+v err=%v", next, err)
	}
	if next.Reason != wake.ReasonReviewerCapacity {
		t.Fatalf("wake reason = %q, want reviewer_capacity", next.Reason)
	}

	// Capacity recovers; only the headless poller (no GetRun/ContinueRun)
	// drives the retry from here.
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered", Harness: domain.HarnessCodex, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}

	policy := domain.DefaultWakePolicy()
	clk.Advance(time.Duration(policy.InitialBackoffSeconds+policy.JitterSeconds+5) * time.Second)
	poller := wakepoller.New(wakeSched, coord, wakepoller.Config{Clock: clk.Now})
	n, err := poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	t.Logf("RunDueOnce claimed %d wake(s)", n)
	if launcher.launchCalls != 2 {
		t.Fatalf("launcher calls = %d, want exactly 2 (one real retry, no duplicate/zombie reviewer)", launcher.launchCalls)
	}

	runs, err := store.ListReviewRunsBySession(ctx, workerSessionID)
	if err != nil {
		t.Fatalf("ListReviewRunsBySession: %v", err)
	}
	var running, cancelled int
	for _, r := range runs {
		switch r.Status {
		case domain.ReviewRunRunning:
			running++
		case domain.ReviewRunCancelled:
			cancelled++
		}
	}
	if running != 1 || cancelled != 1 {
		t.Fatalf("review_run rows: running=%d cancelled=%d, want exactly 1 and 1 (no duplicate live reviewer)", running, cancelled)
	}

	final, err := coord.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun (final): %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want running again after recovery", reviewStepFrom(final).Step.State)
	}
	if final.Run.State != domain.WorkflowRunRunning {
		t.Fatalf("run state = %q, want running again after recovery", final.Run.State)
	}
}
