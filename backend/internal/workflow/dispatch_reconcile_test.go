package workflow_test

// Crash/restart reconciliation of the dispatch state machine.
//
// The phased dispatch made every step of a launch durable in order. These tests
// are about the other half of that bargain: after a crash, AO has to be able to
// READ those rows and say which phase actually completed — and then do exactly
// one correct thing about it.
//
// Six contradictions, one per test, each built by writing the durable rows a
// crash at that exact instant leaves behind, and each resolved through fake
// launcher/ownership fixtures. No process is started, no terminal is opened, no
// test sleeps, and nothing waits for a timer: the clock is a value.
//
// The seventh property, and the one that matters most, is spread across all of
// them: reconciliation never launches anything. The fake registry below counts
// every launch it is asked for, and every test asserts that count is zero.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// ---- the fake ownership registry ---------------------------------------------

// fakeOwnedWorker is one worker the registry knows about, and how readable it
// is. The three not-live shapes are deliberately distinct, because
// reconciliation is required to treat them differently: a session that is GONE
// is a fact it may act on, and a session it could not READ is not.
type fakeOwnedWorker struct {
	sessionID   domain.SessionID
	dispatchKey string
	// live is the registry's own truth: is this worker running right now.
	live bool
	// missing models a read that answered "there is no such session".
	missing bool
	// unreadable models a read that FAILED. Never the same as missing.
	unreadable string
	// livenessKnown models whether the runtime probe answers at all for this
	// session. False is "no reading was taken", never "not alive".
	livenessKnown bool
}

// fakeOwnershipRegistry is the launcher, the ownership prober and the liveness
// probe in one object, because the property under test spans all three: a
// launch creates ownership, ownership is what reconciliation reads, and the
// count of live workers under one dispatch key is the invariant.
type fakeOwnershipRegistry struct {
	mu       sync.Mutex
	workers  map[domain.SessionID]*fakeOwnedWorker
	launches int
}

func newFakeOwnershipRegistry() *fakeOwnershipRegistry {
	return &fakeOwnershipRegistry{workers: map[domain.SessionID]*fakeOwnedWorker{}}
}

// register places a worker under a dispatch key without launching it — the
// state a crashed daemon leaves behind, where the worker exists and AO's own
// records do not yet say so.
func (r *fakeOwnershipRegistry) register(w *fakeOwnedWorker) *fakeOwnedWorker {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers[w.sessionID] = w
	return w
}

// liveUnder counts the live workers for one dispatch key. This is the number
// that must never reach two.
func (r *fakeOwnershipRegistry) liveUnder(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, w := range r.workers {
		if w.dispatchKey == key && w.live {
			n++
		}
	}
	return n
}

// LaunchWorker is workflowcore.WorkerLauncher. Reconciliation must never reach
// it; every test asserts launches == 0.
func (r *fakeOwnershipRegistry) LaunchWorker(
	_ context.Context,
	req workflowcore.WorkerLaunchRequest,
) (workflowcore.WorkerLaunchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.launches++
	id := domain.SessionID(fmt.Sprintf("sess-launched-%d", r.launches))
	r.workers[id] = &fakeOwnedWorker{
		sessionID:     id,
		dispatchKey:   "workflow-step-spawn:" + req.StepID,
		live:          true,
		livenessKnown: true,
	}
	return workflowcore.WorkerLaunchResult{
		Session: domain.SessionRecord{ID: id, ProjectID: req.ProjectID, Harness: req.Harness},
	}, nil
}

// ObserveSessionOwnership is workflowcore.SessionOwnership.
func (r *fakeOwnershipRegistry) ObserveSessionOwnership(
	_ context.Context,
	id domain.SessionID,
) workflowcore.SessionOwnershipEvidence {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[id]
	switch {
	case !ok:
		return workflowcore.SessionOwnershipEvidence{
			SessionID: id, Missing: true,
			Unavailable: "the session row could not be found",
		}
	case w.unreadable != "":
		return workflowcore.SessionOwnershipEvidence{SessionID: id, Unavailable: w.unreadable}
	case w.missing:
		return workflowcore.SessionOwnershipEvidence{
			SessionID: id, Missing: true,
			Unavailable: "the session row could not be found",
		}
	}
	return workflowcore.SessionOwnershipEvidence{
		SessionID:       id,
		Observed:        true,
		RuntimeHandleID: "rt-" + string(id),
		RuntimeLaunchID: "gen-1",
		AgentSessionID:  "agent-" + string(id),
		Branch:          "feat/reconcile",
		WorktreePath:    "/tmp/wt/" + string(id),
		BaseSHA:         "base-sha",
	}
}

// SessionAlive is workflowcore.WorkerLivenessProbe. An unregistered session, or
// one whose probe is deliberately silent, answers known=false — which
// reconciliation must never read as death.
func (r *fakeOwnershipRegistry) SessionAlive(_ context.Context, id domain.SessionID) (bool, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[id]
	if !ok || !w.livenessKnown {
		return false, false, nil
	}
	return w.live, true, nil
}

// ---- the fixture --------------------------------------------------------------

type dispatchReconcileFixture struct {
	t        *testing.T
	ctx      context.Context
	c        *workflowcore.Coordinator
	store    *fakeStore
	facts    *fakeSessionFacts
	registry *fakeOwnershipRegistry
	wakes    *fakeWakeScheduler
	clk      *fakeClock

	runID  string
	stepID string
}

func newDispatchReconcileFixture(t *testing.T) *dispatchReconcileFixture {
	t.Helper()
	store := newFakeStore()
	facts := newFakeSessionFacts()
	registry := newFakeOwnershipRegistry()
	wakes := newFakeWakeScheduler()
	clk := &fakeClock{t: time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		WorkerLauncher:   registry,
		SessionOwnership: registry,
		WorkerLiveness:   registry,
		SessionFacts:     facts,
		WorkspaceFacts:   &fakeWorkspaceFacts{},
		WakeScheduler:    wakes,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	f := &dispatchReconcileFixture{t: t, ctx: context.Background(), c: c, store: store,
		facts: facts, registry: registry, wakes: wakes, clk: clk}

	created, err := c.CreateRun(f.ctx, "proj-1", "reconcile the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	f.stepID = workStepIDOf(t, store, created.Run.ID)
	return f
}

func (f *dispatchReconcileFixture) dispatchKey() string { return "workflow-step-spawn:" + f.stepID }
func (f *dispatchReconcileFixture) issueID() domain.IssueID {
	return domain.IssueID("workflow-step:" + f.stepID)
}

func (f *dispatchReconcileFixture) run() domain.WorkflowRun {
	f.t.Helper()
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	return run
}

func (f *dispatchReconcileFixture) step() domain.WorkflowStep {
	f.t.Helper()
	return stepByID(f.t, f.store, f.runID, f.stepID)
}

func (f *dispatchReconcileFixture) attempts() []domain.WorkflowAttempt {
	f.t.Helper()
	got, err := f.store.ListWorkflowAttempts(f.ctx, f.stepID)
	if err != nil {
		f.t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	return got
}

// ---- seeding the durable state a crash leaves behind --------------------------

func (f *dispatchReconcileFixture) seedRunState(next domain.WorkflowRunState) {
	f.t.Helper()
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID, f.run().State, next, f.clk.Now()); err != nil {
		f.t.Fatalf("seed run -> %s: %v", next, err)
	}
}

func (f *dispatchReconcileFixture) seedStepState(next domain.WorkflowStepState) {
	f.t.Helper()
	if _, err := f.store.UpdateWorkflowStepState(f.ctx, f.stepID, f.step().State, next, f.clk.Now()); err != nil {
		f.t.Fatalf("seed step -> %s: %v", next, err)
	}
}

func (f *dispatchReconcileFixture) seedStepSession(id domain.SessionID) {
	f.t.Helper()
	if _, err := f.store.UpdateWorkflowStepSession(f.ctx, f.stepID, string(id), f.clk.Now()); err != nil {
		f.t.Fatalf("seed step session: %v", err)
	}
}

func (f *dispatchReconcileFixture) seedOutbox(status domain.WorkflowOutboxStatus) domain.WorkflowOutboxEntry {
	f.t.Helper()
	stepID := f.stepID
	entry, _, err := f.store.EnqueueWorkflowOutboxEntry(f.ctx, domain.WorkflowOutboxEntry{
		ID: "wfo-seed", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		IdempotencyKey: f.dispatchKey(),
		CommandType:    domain.WorkflowOutboxSpawnWorkerSession,
		Payload:        `{"projectId":"proj-1"}`,
		CreatedAt:      f.clk.Now(),
	})
	if err != nil {
		f.t.Fatalf("seed outbox: %v", err)
	}
	if status != entry.Status {
		if _, err := f.store.UpdateWorkflowOutboxStatus(
			f.ctx, entry.ID, entry.Status, status, f.clk.Now(), ""); err != nil {
			f.t.Fatalf("seed outbox -> %s: %v", status, err)
		}
		entry.Status = status
	}
	return entry
}

func (f *dispatchReconcileFixture) outbox() domain.WorkflowOutboxEntry {
	f.t.Helper()
	entries, err := f.store.ListWorkflowOutboxByRun(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowOutboxByRun: %v", err)
	}
	for _, e := range entries {
		if e.IdempotencyKey == f.dispatchKey() {
			return e
		}
	}
	f.t.Fatalf("no outbox entry under the dispatch key %s", f.dispatchKey())
	return domain.WorkflowOutboxEntry{}
}

func (f *dispatchReconcileFixture) seedOpenAttempt(harness string) domain.WorkflowAttempt {
	f.t.Helper()
	attempt, err := f.store.CreateWorkflowAttempt(
		f.ctx, "wfa-seed", f.stepID, harness, "", f.clk.Now())
	if err != nil {
		f.t.Fatalf("seed attempt: %v", err)
	}
	return attempt
}

// seedBoundary appends one dispatch record exactly as the state machine would
// have written it at the moment the daemon died.
func (f *dispatchReconcileFixture) seedBoundary(
	phase domain.WorkflowDispatchPhase,
	stage domain.WorkflowLaunchStage,
	outcome domain.WorkflowLaunchOutcome,
	attemptID, sessionID string,
) {
	f.t.Helper()
	stepID := f.stepID
	cp := domain.WorkflowDispatchCheckpoint{
		ID: "wfd-seed-" + string(outcome), WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		Phase: phase, LaunchStage: stage, LaunchOutcome: outcome,
		IdempotencyKey: f.dispatchKey(), Harness: "codex",
		EvidenceJSON: "{}", CreatedAt: f.clk.Now(),
	}
	if attemptID != "" {
		cp.AttemptID = &attemptID
	}
	if sessionID != "" {
		cp.SessionID = &sessionID
	}
	if _, err := f.store.CreateWorkflowDispatchCheckpoint(f.ctx, cp); err != nil {
		f.t.Fatalf("seed dispatch boundary: %v", err)
	}
}

// seedLiveWorker puts a worker under this step's dispatch key: live in the
// ownership registry AND visible through the natural key, which is exactly what
// a launch that outlived the daemon recording it looks like.
func (f *dispatchReconcileFixture) seedLiveWorker(id domain.SessionID) *fakeOwnedWorker {
	f.t.Helper()
	f.facts.put(domain.SessionRecord{
		ID: id, ProjectID: "proj-1", Harness: domain.HarnessCodex,
		Kind: domain.KindWorker, IssueID: f.issueID(),
		Activity: domain.Activity{State: domain.ActivityActive},
		Metadata: domain.SessionMetadata{Branch: "feat/reconcile", WorkspacePath: "/tmp/wt/" + string(id)},
	})
	return f.registry.register(&fakeOwnedWorker{
		sessionID: id, dispatchKey: f.dispatchKey(), live: true, livenessKnown: true,
	})
}

// seedDeadWorker puts a worker under the key whose session row is terminated
// and whose probe says so. Positive evidence, not silence.
func (f *dispatchReconcileFixture) seedDeadWorker(id domain.SessionID) {
	f.t.Helper()
	f.facts.put(domain.SessionRecord{
		ID: id, ProjectID: "proj-1", Harness: domain.HarnessCodex,
		Kind: domain.KindWorker, IssueID: f.issueID(),
		IsTerminated: true,
		Activity:     domain.Activity{State: domain.ActivityExited},
	})
	f.registry.register(&fakeOwnedWorker{
		sessionID: id, dispatchKey: f.dispatchKey(), live: false, livenessKnown: true,
	})
}

func (f *dispatchReconcileFixture) reconcile() workflowcore.DispatchReconciliation {
	f.t.Helper()
	result, _, err := f.c.ReconcileWorkStepDispatch(f.ctx, f.run(), f.step())
	if err != nil {
		f.t.Fatalf("ReconcileWorkStepDispatch: %v", err)
	}
	return result
}

func (f *dispatchReconcileFixture) assertNothingLaunched() {
	f.t.Helper()
	if f.registry.launches != 0 {
		f.t.Fatalf("reconciliation launched %d workers; it must never launch anything", f.registry.launches)
	}
}

func (f *dispatchReconcileFixture) checkpointPhases() []string {
	f.t.Helper()
	return ledgerPhases(f.t, f.store, f.runID)
}

func (f *dispatchReconcileFixture) hasCheckpointPhase(phase string) bool {
	f.t.Helper()
	for _, p := range f.checkpointPhases() {
		if p == phase {
			return true
		}
	}
	return false
}

// ---- (a) an attempt created, and a launch that never happened -----------------

func TestReconcileClosesAnAttemptWhoseDispatchIntentNeverLaunched(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("codex")
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchIntent, domain.LaunchStageIntent,
		domain.LaunchOutcomeIntended, attempt.ID, "")
	// Past the in-flight settle window: an intent this old is not a launch
	// happening right now.
	f.clk.Advance(5 * time.Minute)

	got := f.reconcile()
	if got.Contradiction != workflowcore.ContradictionIntentNeverLaunched {
		t.Fatalf("contradiction = %q, want intent_never_launched (detail: %s)", got.Contradiction, got.Detail)
	}
	if got.Action != workflowcore.DispatchReconcileRetryScheduled {
		t.Fatalf("action = %q, want retry_scheduled (detail: %s)", got.Action, got.Detail)
	}
	f.assertNothingLaunched()

	// The attempt row is what claimed work was in flight. It is closed.
	attempts := f.attempts()
	if len(attempts) != 1 || attempts[0].Outcome != domain.WorkflowAttemptFailed {
		t.Fatalf("attempts = %+v, want the seeded attempt concluded failed", attempts)
	}
	// The retry is handed to the bounded policy under the SAME dispatch key:
	// the outbox goes back to pending and a durable wake carries it.
	if got := f.outbox(); got.Status != domain.WorkflowOutboxPending {
		t.Fatalf("outbox status = %q, want pending so the bounded retry can re-enter dispatch", got.Status)
	}
	if len(f.wakes.reasons) == 0 || f.wakes.reasons[len(f.wakes.reasons)-1] != wake.ReasonTransientRetry {
		t.Fatalf("wake reasons = %v, want a transient_retry wake to carry the retry", f.wakes.reasons)
	}
	// And the reconciliation itself is durable, in the dispatch history it
	// belongs to rather than in a model of its own.
	records := dispatchRecordsFor(t, f.store, f.stepID)
	latest := records[len(records)-1]
	if latest.Phase != domain.DispatchPhaseWorkerDispatchReconciled {
		t.Fatalf("newest dispatch record = %q, want the reconciliation boundary", latest.Phase)
	}
	if !strings.Contains(latest.EvidenceJSON, string(workflowcore.ContradictionIntentNeverLaunched)) {
		t.Fatalf("reconciliation evidence = %s, want it to name the contradiction", latest.EvidenceJSON)
	}
}

// ---- (b) an intent that was persisted, and a launch that failed ---------------

func TestReconcileRoutesAProvenLaunchFailureThatWasNeverRouted(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("codex")
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchIntent, domain.LaunchStageIntent,
		domain.LaunchOutcomeIntended, attempt.ID, "")
	// The launcher failed and the daemon died before its failure reached the
	// retry policy: the boundary is on disk and nothing answered it.
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchError, domain.LaunchStageSpawn,
		domain.LaunchOutcomeFailed, attempt.ID, "")

	got := f.reconcile()
	if got.Contradiction != workflowcore.ContradictionLaunchFailed {
		t.Fatalf("contradiction = %q, want launch_failed (detail: %s)", got.Contradiction, got.Detail)
	}
	if got.Action != workflowcore.DispatchReconcileRetryScheduled {
		t.Fatalf("action = %q, want retry_scheduled (detail: %s)", got.Action, got.Detail)
	}
	f.assertNothingLaunched()
	if !f.hasCheckpointPhase("worker_launch_error") {
		t.Fatalf("the failure was never durably classified; phases = %v", f.checkpointPhases())
	}
	if got := f.outbox(); got.Status != domain.WorkflowOutboxPending {
		t.Fatalf("outbox status = %q, want pending", got.Status)
	}
}

// A launch failure the retry policy ALREADY answered belongs to the wake that
// owns it. Reconciliation must not burn its budget re-answering it.
func TestReconcileLeavesAnAlreadyRoutedLaunchFailureAlone(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("codex")
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchError, domain.LaunchStageSpawn,
		domain.LaunchOutcomeFailed, attempt.ID, "")
	stepID := f.stepID
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-routed", WorkflowRunID: f.runID, WorkflowStepID: &stepID, ProjectID: "proj-1",
		DurablePhase: "worker_launch_error", PayloadVersion: "v1",
		RetryState: `{"attempt":1,"class":"runtime_failed","retryable":true}`,
		CreatedAt:  f.clk.Now(),
	}); err != nil {
		t.Fatalf("seed launch record: %v", err)
	}

	before := len(dispatchRecordsFor(t, f.store, f.stepID))
	got := f.reconcile()
	if got.Action != workflowcore.DispatchReconcileNoop {
		t.Fatalf("action = %q, want noop: the retry policy already owns this step", got.Action)
	}
	if after := len(dispatchRecordsFor(t, f.store, f.stepID)); after != before {
		t.Fatalf("dispatch records %d -> %d: a no-op wrote a boundary", before, after)
	}
	f.assertNothingLaunched()
}

// ---- (c) a launch that succeeded and could not be confirmed -------------------

func TestReconcileAdoptsALaunchWhoseConfirmationWasNeverPersisted(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("codex")
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchIntent, domain.LaunchStageIntent,
		domain.LaunchOutcomeIntended, attempt.ID, "")
	// The launcher answered, and the confirmation write did not land.
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchUnconfirmed, domain.LaunchStageConfirm,
		domain.LaunchOutcomeUnconfirmed, attempt.ID, "sess-live")
	f.seedLiveWorker("sess-live")

	got := f.reconcile()
	if got.Contradiction != workflowcore.ContradictionLaunchUnconfirmed {
		t.Fatalf("contradiction = %q, want launch_unconfirmed (detail: %s)", got.Contradiction, got.Detail)
	}
	if got.Action != workflowcore.DispatchReconcileAdopted {
		t.Fatalf("action = %q, want adopted (detail: %s)", got.Action, got.Detail)
	}
	// Adopted means CONFIRMED — not relaunched. One worker, and the same one.
	f.assertNothingLaunched()
	if n := f.registry.liveUnder(f.dispatchKey()); n != 1 {
		t.Fatalf("live workers under the dispatch key = %d, want exactly 1", n)
	}
	step := f.step()
	if step.SessionID == nil || *step.SessionID != "sess-live" {
		t.Fatalf("step session = %v, want the adopted session on the step", step.SessionID)
	}
	if step.State != domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want running: the confirmation is durable now", step.State)
	}
	if !f.c.WorkerAttemptRunning(f.ctx, f.runID, step, f.attempts()[0]) {
		t.Fatal("the adopted attempt is still not RUNNING after its confirmation was made durable")
	}
	// The confirmation is a real dispatch confirmation, in the same table and
	// the same shape a fresh launch would have written.
	records := dispatchRecordsFor(t, f.store, f.stepID)
	confirmed := false
	for _, r := range records {
		if r.LaunchOutcome == domain.LaunchOutcomeDispatched && deref(r.SessionID) == "sess-live" {
			confirmed = true
		}
	}
	if !confirmed {
		t.Fatalf("no dispatch confirmation was recorded for the adopted session; records = %+v", records)
	}
}

// ---- (d) a step that says RUNNING over nothing at all -------------------------

func TestReconcileClosesARunningStepWithNoDispatchEvidenceAtAll(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedStepState(domain.WorkflowStepRunning)
	attempt := f.seedOpenAttempt("codex")
	_ = attempt
	// No outbox command, no dispatch boundary, no session: nothing was ever
	// launched, or even commanded, and the step says it is running.

	got := f.reconcile()
	if got.Contradiction != workflowcore.ContradictionRunningWithoutEvidence {
		t.Fatalf("contradiction = %q, want running_without_evidence (detail: %s)", got.Contradiction, got.Detail)
	}
	if got.Action != workflowcore.DispatchReconcileRetryScheduled {
		t.Fatalf("action = %q, want retry_scheduled (detail: %s)", got.Action, got.Detail)
	}
	f.assertNothingLaunched()
	if attempts := f.attempts(); attempts[0].Outcome != domain.WorkflowAttemptFailed {
		t.Fatalf("attempt outcome = %q, want the phantom attempt closed", attempts[0].Outcome)
	}
}

// A dispatched command with no boundary under it is the pre-state-machine
// ambiguity, and it belongs to dispatch adoption — which adopts on evidence and
// escalates otherwise. Reconciliation must not take it, because taking it would
// mean answering a question the rows genuinely do not answer.
func TestReconcileDefersADispatchedCommandWithNoBoundaryToDispatchAdoption(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedStepState(domain.WorkflowStepRunning)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	f.seedOpenAttempt("codex")

	got := f.reconcile()
	if got.Action != workflowcore.DispatchReconcileNoop {
		t.Fatalf("action = %q, want noop: dispatch adoption owns this shape (detail: %s)", got.Action, got.Detail)
	}
	if len(dispatchRecordsFor(t, f.store, f.stepID)) != 0 {
		t.Fatal("a deferred shape wrote a reconciliation boundary")
	}
	f.assertNothingLaunched()
}

// ---- (e) a stale running attempt with no live owned execution -----------------

func TestReconcileClosesAStaleRunningAttemptWhoseExecutionIsGone(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedStepState(domain.WorkflowStepRunning)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("codex")
	// The confirmation landed; the transition after it (session onto the step)
	// did not. So the attempt claims to be in flight and nothing tracks what
	// over — and the execution it named is provably gone.
	f.seedBoundary(domain.DispatchPhaseWorkerDispatched, domain.LaunchStageConfirm,
		domain.LaunchOutcomeDispatched, attempt.ID, "sess-dead")
	f.seedDeadWorker("sess-dead")

	got := f.reconcile()
	if got.Contradiction != workflowcore.ContradictionStaleRunning {
		t.Fatalf("contradiction = %q, want stale_running (detail: %s)", got.Contradiction, got.Detail)
	}
	if got.Action != workflowcore.DispatchReconcileRetryScheduled {
		t.Fatalf("action = %q, want retry_scheduled (detail: %s)", got.Action, got.Detail)
	}
	f.assertNothingLaunched()
	if attempts := f.attempts(); attempts[0].Outcome != domain.WorkflowAttemptFailed {
		t.Fatalf("attempt outcome = %q, want the stale attempt closed", attempts[0].Outcome)
	}
	if n := f.registry.liveUnder(f.dispatchKey()); n != 0 {
		t.Fatalf("live workers under the dispatch key = %d, want 0", n)
	}
}

// ---- (f) a live, evidenced worker: untouched ----------------------------------

func TestReconcileLeavesALiveEvidencedWorkerCompletelyUntouched(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedStepState(domain.WorkflowStepRunning)
	f.seedOutbox(domain.WorkflowOutboxAcknowledged)
	attempt := f.seedOpenAttempt("codex")
	f.seedBoundary(domain.DispatchPhaseWorkerDispatched, domain.LaunchStageConfirm,
		domain.LaunchOutcomeDispatched, attempt.ID, "sess-live")
	f.seedLiveWorker("sess-live")
	f.seedStepSession("sess-live")

	beforeDispatch := len(dispatchRecordsFor(t, f.store, f.stepID))
	beforeLedger := len(f.checkpointPhases())
	beforeRun, beforeStep := f.run(), f.step()

	got := f.reconcile()
	if got.Action != workflowcore.DispatchReconcileProtected {
		t.Fatalf("action = %q, want protected: a live evidenced worker is never touched (detail: %s)",
			got.Action, got.Detail)
	}
	if got.Contradiction != workflowcore.ContradictionNone {
		t.Fatalf("contradiction = %q, want none", got.Contradiction)
	}
	f.assertNothingLaunched()
	if n := f.registry.liveUnder(f.dispatchKey()); n != 1 {
		t.Fatalf("live workers under the dispatch key = %d, want the one live worker still live", n)
	}
	// Untouched means untouched: not retried, not stopped, not killed, and not
	// written about.
	if after := len(dispatchRecordsFor(t, f.store, f.stepID)); after != beforeDispatch {
		t.Fatalf("dispatch records %d -> %d over a live worker", beforeDispatch, after)
	}
	if after := len(f.checkpointPhases()); after != beforeLedger {
		t.Fatalf("ledger rows %d -> %d over a live worker: %v", beforeLedger, after, f.checkpointPhases())
	}
	if f.run().State != beforeRun.State {
		t.Fatalf("run state %q -> %q over a live worker", beforeRun.State, f.run().State)
	}
	if f.step().State != beforeStep.State {
		t.Fatalf("step state %q -> %q over a live worker", beforeStep.State, f.step().State)
	}
	if attempts := f.attempts(); attempts[0].Outcome != "" {
		t.Fatalf("attempt outcome = %q, want it still open over a live worker", attempts[0].Outcome)
	}
	if f.wakes.scheduled[f.runID] {
		t.Fatal("a wake was scheduled over a live worker")
	}
}

// ---- duplicate wakes ----------------------------------------------------------

// The invariant the whole file exists for: however many times reconciliation
// runs against one dispatch key, there is never a second live worker under it.
//
// Both halves are exercised — the key that HAS a live worker (which must be
// adopted once and then left alone) and the key that has none (which must be
// handed to the retry policy once and then left alone).
func TestReconcilingTwiceUnderOneDispatchKeyNeverProducesASecondWorker(t *testing.T) {
	t.Run("a live worker is adopted once", func(t *testing.T) {
		f := newDispatchReconcileFixture(t)
		f.seedRunState(domain.WorkflowRunRunning)
		f.seedStepState(domain.WorkflowStepReady)
		f.seedOutbox(domain.WorkflowOutboxDispatched)
		attempt := f.seedOpenAttempt("codex")
		f.seedBoundary(domain.DispatchPhaseWorkerLaunchUnconfirmed, domain.LaunchStageConfirm,
			domain.LaunchOutcomeUnconfirmed, attempt.ID, "sess-live")
		f.seedLiveWorker("sess-live")

		first := f.reconcile()
		second := f.reconcile()
		third := f.reconcile()

		if first.Action != workflowcore.DispatchReconcileAdopted {
			t.Fatalf("first action = %q, want adopted", first.Action)
		}
		for i, got := range []workflowcore.DispatchReconciliation{second, third} {
			if got.Action != workflowcore.DispatchReconcileProtected {
				t.Fatalf("duplicate wake %d action = %q, want protected (detail: %s)", i+2, got.Action, got.Detail)
			}
		}
		f.assertNothingLaunched()
		if n := f.registry.liveUnder(f.dispatchKey()); n != 1 {
			t.Fatalf("live workers under the dispatch key after three passes = %d, want exactly 1", n)
		}
		confirmations := 0
		for _, r := range dispatchRecordsFor(t, f.store, f.stepID) {
			if r.LaunchOutcome == domain.LaunchOutcomeDispatched {
				confirmations++
			}
		}
		if confirmations != 1 {
			t.Fatalf("dispatch confirmations = %d, want exactly one for one adoption", confirmations)
		}
	})

	t.Run("a key with no worker is retried once", func(t *testing.T) {
		f := newDispatchReconcileFixture(t)
		f.seedRunState(domain.WorkflowRunRunning)
		f.seedStepState(domain.WorkflowStepReady)
		f.seedOutbox(domain.WorkflowOutboxDispatched)
		attempt := f.seedOpenAttempt("codex")
		f.seedBoundary(domain.DispatchPhaseWorkerLaunchIntent, domain.LaunchStageIntent,
			domain.LaunchOutcomeIntended, attempt.ID, "")
		f.clk.Advance(5 * time.Minute)

		first := f.reconcile()
		second := f.reconcile()
		if first.Action != workflowcore.DispatchReconcileRetryScheduled {
			t.Fatalf("first action = %q, want retry_scheduled", first.Action)
		}
		if second.Action != workflowcore.DispatchReconcileNoop {
			t.Fatalf("duplicate wake action = %q, want noop: the contradiction was already answered (detail: %s)",
				second.Action, second.Detail)
		}
		f.assertNothingLaunched()
		if n := f.registry.liveUnder(f.dispatchKey()); n != 0 {
			t.Fatalf("live workers under the dispatch key = %d, want 0", n)
		}
		reconciliations := 0
		for _, r := range dispatchRecordsFor(t, f.store, f.stepID) {
			if r.Phase == domain.DispatchPhaseWorkerDispatchReconciled {
				reconciliations++
			}
		}
		if reconciliations != 1 {
			t.Fatalf("reconciliation boundaries = %d, want exactly one for one contradiction", reconciliations)
		}
	})
}

// A stop taken because a session could not be READ must not become permanent.
// If that session later reads back alive, the worker is running and AO has to
// recognise it — reconciliation reopens on liveness, and on nothing else.
func TestAReconciliationStopReopensWhenTheWorkerReadsBackAlive(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("codex")
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchUnconfirmed, domain.LaunchStageConfirm,
		domain.LaunchOutcomeUnconfirmed, attempt.ID, "sess-flaky")
	worker := f.registry.register(&fakeOwnedWorker{
		sessionID: "sess-flaky", dispatchKey: f.dispatchKey(),
		unreadable: "the runtime could not be reached",
	})

	if got := f.reconcile(); got.Action != workflowcore.DispatchReconcileNeedsAttention {
		t.Fatalf("first action = %q, want needs_attention (detail: %s)", got.Action, got.Detail)
	}
	// The same unreadable session, twice: still a stop, and not a second one.
	if got := f.reconcile(); got.Action != workflowcore.DispatchReconcileNoop {
		t.Fatalf("second action = %q, want noop while nothing has changed (detail: %s)", got.Action, got.Detail)
	}

	// The runtime comes back, and the worker is there.
	worker.unreadable = ""
	worker.live = true
	worker.livenessKnown = true
	f.facts.put(domain.SessionRecord{
		ID: "sess-flaky", ProjectID: "proj-1", Harness: domain.HarnessCodex,
		Kind: domain.KindWorker, IssueID: f.issueID(),
		Activity: domain.Activity{State: domain.ActivityActive},
	})

	got := f.reconcile()
	if got.Action != workflowcore.DispatchReconcileAdopted {
		t.Fatalf("action after the worker read back alive = %q, want adopted (detail: %s)", got.Action, got.Detail)
	}
	f.assertNothingLaunched()
	if n := f.registry.liveUnder(f.dispatchKey()); n != 1 {
		t.Fatalf("live workers under the dispatch key = %d, want exactly 1", n)
	}
	step := f.step()
	if step.SessionID == nil || *step.SessionID != "sess-flaky" {
		t.Fatalf("step session = %v, want the adopted session", step.SessionID)
	}
	if step.State != domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want running once its live worker was recognised", step.State)
	}
	if got := f.run().State; got != domain.WorkflowRunRunning {
		t.Fatalf("run state = %q, want running: its child really is running", got)
	}
}

// ---- the unprovable case, and the evidence it owes ----------------------------

// What AO cannot prove, it does not guess at — and what it stops for, it
// evidences. This is the acceptance test for both halves at once: an ownership
// read that FAILED is never read as a dead worker, and the stop it produces
// carries the bounded snapshot, written through the package's one evidence gate.
func TestReconcileStopsWithTheEvidenceSnapshotWhenOwnershipCannotBeRead(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("codex")
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchUnconfirmed, domain.LaunchStageConfirm,
		domain.LaunchOutcomeUnconfirmed, attempt.ID, "sess-unreadable")
	// The read FAILED. That is not "there is no session".
	f.registry.register(&fakeOwnedWorker{
		sessionID: "sess-unreadable", dispatchKey: f.dispatchKey(),
		unreadable: "the runtime could not be reached",
	})

	got := f.reconcile()
	if got.Contradiction != workflowcore.ContradictionUnprovable {
		t.Fatalf("contradiction = %q, want unprovable (detail: %s)", got.Contradiction, got.Detail)
	}
	if got.Action != workflowcore.DispatchReconcileNeedsAttention {
		t.Fatalf("action = %q, want needs_attention (detail: %s)", got.Action, got.Detail)
	}
	f.assertNothingLaunched()

	// The stop is evidenced, through Task 2's writer and no other.
	snapshotRow, ok := latestCheckpointOfPhase(t, f.store, f.runID, workflowcore.AmbiguousWorkerStateEvidencePhase)
	if !ok {
		t.Fatalf("no evidence snapshot was recorded for the stop; phases = %v", f.checkpointPhases())
	}
	snap, decoded := workflowcore.DecodeWorkerEvidenceSnapshot(snapshotRow.RetryState)
	if !decoded {
		t.Fatalf("the recorded snapshot does not decode: %s", snapshotRow.RetryState)
	}
	if snap.StepID != f.stepID {
		t.Fatalf("snapshot step = %q, want %q", snap.StepID, f.stepID)
	}
	// And it carries the LAUNCH evidence — automatically, because the collector
	// already reads the dispatch table this reconciliation writes to.
	for _, key := range []string{"launch.latest", "launch.outcome", "launch.history", "launch.idempotencyKey"} {
		field, found := snap.Field(key)
		if !found {
			t.Fatalf("the snapshot carries no %s field; a reconciliation stop must carry its launch evidence", key)
		}
		if !field.Available() {
			t.Fatalf("%s = %q (%s), want an observed fact", key, field.Value, field.Status)
		}
	}
	if field, _ := snap.Field("launch.latest"); field.Value != string(domain.DispatchPhaseWorkerDispatchReconciled) {
		t.Fatalf("launch.latest = %q, want the reconciliation boundary that caused the stop", field.Value)
	}

	// The stop itself: the attempt concluded under the evidenced class, the step
	// parked, the run stopped, and the reason named.
	attempts := f.attempts()
	if attempts[0].Outcome != domain.WorkflowAttemptFailed {
		t.Fatalf("attempt outcome = %q, want failed", attempts[0].Outcome)
	}
	if attempts[0].ErrorClass != domain.WorkflowErrorAmbiguousWorkerState {
		t.Fatalf("attempt error class = %q, want the class the evidence gate produced", attempts[0].ErrorClass)
	}
	if !f.hasCheckpointPhase(workflowcore.ReasonWorkerDispatchAmbiguous) {
		t.Fatalf("the stop was not named; phases = %v", f.checkpointPhases())
	}
	if got := f.step().State; got != domain.WorkflowStepWaiting {
		t.Fatalf("step state = %q, want waiting", got)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
}

// An exhausted retry budget is a stop, and a stop owes the same evidence as any
// other. It must never be pushed through the failure path unevidenced.
func TestReconcileStopsWithEvidenceWhenTheRetryBudgetIsSpent(t *testing.T) {
	f := newDispatchReconcileFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("codex")
	// A launch that was reported and never confirmed, whose session is provably
	// gone: ordinarily a retry, and here the budget for one is spent.
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchUnconfirmed, domain.LaunchStageConfirm,
		domain.LaunchOutcomeUnconfirmed, attempt.ID, "sess-gone")
	// Three prior launch failures: the automatic budget is gone.
	stepID := f.stepID
	for i := 1; i <= 3; i++ {
		if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
			ID: fmt.Sprintf("wfc-spent-%d", i), WorkflowRunID: f.runID, WorkflowStepID: &stepID,
			ProjectID: "proj-1", DurablePhase: "worker_launch_error", PayloadVersion: "v1",
			RetryState: fmt.Sprintf(`{"attempt":%d,"class":"runtime_failed","retryable":true}`, i),
			CreatedAt:  f.clk.Now(),
		}); err != nil {
			t.Fatalf("seed spent budget: %v", err)
		}
	}
	f.clk.Advance(5 * time.Minute)

	got := f.reconcile()
	if got.Action != workflowcore.DispatchReconcileNeedsAttention {
		t.Fatalf("action = %q, want needs_attention once the budget is spent (detail: %s)", got.Action, got.Detail)
	}
	if _, ok := latestCheckpointOfPhase(t, f.store, f.runID, workflowcore.AmbiguousWorkerStateEvidencePhase); !ok {
		t.Fatalf("a budget-exhausted stop was taken without its evidence snapshot; phases = %v",
			f.checkpointPhases())
	}
	f.assertNothingLaunched()
}

// ---- the parent -----------------------------------------------------------------

// A run is not running because a row says so. After reconciliation resolves its
// only in-flight child, the parent's derived state must say what actually
// happened to that child.
func TestReconciliationSettlesTheParentRunAgainstItsChildsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  func(f *dispatchReconcileFixture)
		want  domain.WorkflowRunState
		state workflowcore.DispatchReconcileAction
	}{
		{
			name: "the only in-flight child was retried: nothing is running",
			seed: func(f *dispatchReconcileFixture) {
				f.seedStepState(domain.WorkflowStepReady)
				f.seedOutbox(domain.WorkflowOutboxDispatched)
				attempt := f.seedOpenAttempt("codex")
				f.seedBoundary(domain.DispatchPhaseWorkerLaunchIntent, domain.LaunchStageIntent,
					domain.LaunchOutcomeIntended, attempt.ID, "")
				f.clk.Advance(5 * time.Minute)
			},
			want:  domain.WorkflowRunWaiting,
			state: workflowcore.DispatchReconcileRetryScheduled,
		},
		{
			name: "the only in-flight child was adopted: it really is running",
			seed: func(f *dispatchReconcileFixture) {
				f.seedStepState(domain.WorkflowStepReady)
				f.seedOutbox(domain.WorkflowOutboxDispatched)
				attempt := f.seedOpenAttempt("codex")
				f.seedBoundary(domain.DispatchPhaseWorkerLaunchUnconfirmed, domain.LaunchStageConfirm,
					domain.LaunchOutcomeUnconfirmed, attempt.ID, "sess-live")
				f.seedLiveWorker("sess-live")
			},
			want:  domain.WorkflowRunRunning,
			state: workflowcore.DispatchReconcileAdopted,
		},
		{
			name: "the only in-flight child stopped: so does the run",
			seed: func(f *dispatchReconcileFixture) {
				f.seedStepState(domain.WorkflowStepReady)
				f.seedOutbox(domain.WorkflowOutboxDispatched)
				attempt := f.seedOpenAttempt("codex")
				f.seedBoundary(domain.DispatchPhaseWorkerLaunchUnconfirmed, domain.LaunchStageConfirm,
					domain.LaunchOutcomeUnconfirmed, attempt.ID, "sess-unreadable")
				f.registry.register(&fakeOwnedWorker{
					sessionID: "sess-unreadable", dispatchKey: f.dispatchKey(),
					unreadable: "the runtime could not be reached",
				})
			},
			want:  domain.WorkflowRunNeedsAttention,
			state: workflowcore.DispatchReconcileNeedsAttention,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDispatchReconcileFixture(t)
			f.seedRunState(domain.WorkflowRunRunning)
			tc.seed(f)

			results, run, err := f.c.ReconcileRunDispatchEvidence(f.ctx, f.run())
			if err != nil {
				t.Fatalf("ReconcileRunDispatchEvidence: %v", err)
			}
			if len(results) != 1 || results[0].Action != tc.state {
				t.Fatalf("results = %+v, want one %s", results, tc.state)
			}
			if run.State != tc.want {
				t.Fatalf("returned run state = %q, want %q", run.State, tc.want)
			}
			// And the read model agrees, which is the only version anybody sees.
			if got := f.run().State; got != tc.want {
				t.Fatalf("durable run state = %q, want %q", got, tc.want)
			}
			f.assertNothingLaunched()
		})
	}
}

// ---- helpers ------------------------------------------------------------------

func latestCheckpointOfPhase(
	t *testing.T, store *fakeStore, runID, phase string,
) (domain.WorkflowCheckpoint, bool) {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			newest, found = cp, true
		}
	}
	return newest, found
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
