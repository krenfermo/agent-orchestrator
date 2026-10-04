package workflow_test

// The phased worker dispatch: intent -> launch -> confirmation -> RUNNING.
//
// Everything here is about one question: what is AO entitled to claim, and
// when. Before this state machine existed, a work step was `running` from the
// moment AO decided to launch — so "running" meant "AO intended to", an attempt
// row meant "AO created one", and neither said anything about whether an agent
// existed. Four durable states now say four different things, and the tests
// below drive each one through an injected launcher, with no process, no
// terminal and no timer anywhere in them:
//
//	intent recorded, nothing launched   -> never RUNNING
//	launcher failed after intent        -> retry per policy, evidence carried
//	launched, confirmation unwritable   -> its own durable state, still not RUNNING
//	launched and confirmed              -> RUNNING, and only then

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- the dispatch table's write side, on the fake store ---------------------

// CreateWorkflowDispatchCheckpoint mirrors the real store's append-only insert.
// dispatchWriteErr lets one specific boundary fail, which is how the two
// write-failure states in this file are reached at all.
func (f *fakeStore) CreateWorkflowDispatchCheckpoint(
	_ context.Context,
	cp domain.WorkflowDispatchCheckpoint,
) (domain.WorkflowDispatchCheckpoint, error) {
	if f.dispatchWriteErr != nil {
		if err := f.dispatchWriteErr(cp); err != nil {
			return domain.WorkflowDispatchCheckpoint{}, err
		}
	}
	if cp.EvidenceJSON == "" {
		cp.EvidenceJSON = "{}"
	}
	f.dispatchCheckpoints = append(f.dispatchCheckpoints, cp)
	return cp, nil
}

// ---- the injected launch boundary -------------------------------------------

// fakeWorkerLauncher is workflowcore.WorkerLauncher. `before` runs at the
// instant the launcher is entered — i.e. with the intent durable and nothing
// launched — which is the only moment at which the "intent-only" state can be
// observed from the outside.
type fakeWorkerLauncher struct {
	calls   int
	session domain.SessionRecord
	err     error
	// noEvidence makes the launcher report success and name no session.
	noEvidence bool
	before     func(req workflowcore.WorkerLaunchRequest)
	// facts, when set, receives the launched session exactly as a real Spawn's
	// session row becomes immediately visible to the same store the observers
	// read from. Without it, the observation pass that follows a successful
	// dispatch would see "session not found" and fail a step that just
	// launched — a fake-decoupling artifact, not behavior.
	facts *fakeSessionFacts
}

func (l *fakeWorkerLauncher) LaunchWorker(
	_ context.Context,
	req workflowcore.WorkerLaunchRequest,
) (workflowcore.WorkerLaunchResult, error) {
	l.calls++
	if l.before != nil {
		l.before(req)
	}
	if l.err != nil {
		return workflowcore.WorkerLaunchResult{}, l.err
	}
	if l.noEvidence {
		return workflowcore.WorkerLaunchResult{}, nil
	}
	rec := l.session
	if rec.ID == "" {
		rec.ID = domain.SessionID(fmt.Sprintf("sess-%d", l.calls))
	}
	rec.ProjectID = req.ProjectID
	rec.Harness = req.Harness
	rec.Kind = domain.KindWorker
	rec.IssueID = req.IssueID
	if l.facts != nil {
		registered := rec
		registered.Activity = domain.Activity{State: domain.ActivityActive}
		registered.IsTerminated = false
		l.facts.put(registered)
	}
	return workflowcore.WorkerLaunchResult{
		Session:    rec,
		LaunchedAt: time.Date(2026, 8, 13, 12, 0, 30, 0, time.UTC),
	}, nil
}

// fakeSessionOwnership is workflowcore.SessionOwnership: the process/session
// ownership proof, faked independently of the launch so a test can have one
// without the other.
type fakeSessionOwnership struct {
	evidence workflowcore.SessionOwnershipEvidence
	calls    int
}

func (o *fakeSessionOwnership) ObserveSessionOwnership(
	_ context.Context,
	id domain.SessionID,
) workflowcore.SessionOwnershipEvidence {
	o.calls++
	ev := o.evidence
	ev.SessionID = id
	return ev
}

func newDispatchMachineCoordinator(
	launcher workflowcore.WorkerLauncher,
	ownership workflowcore.SessionOwnership,
) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	facts := newFakeSessionFacts()
	if fake, ok := launcher.(*fakeWorkerLauncher); ok {
		fake.facts = facts
	}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		WorkerLauncher:   launcher,
		SessionOwnership: ownership,
		SessionFacts:     facts,
		WorkspaceFacts:   &fakeWorkspaceFacts{},
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

func workStepIDOf(t *testing.T, store *fakeStore, runID string) string {
	t.Helper()
	steps, err := store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			return s.ID
		}
	}
	t.Fatal("no work step in run")
	return ""
}

func stepByID(t *testing.T, store *fakeStore, runID, stepID string) domain.WorkflowStep {
	t.Helper()
	steps, err := store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.ID == stepID {
			return s
		}
	}
	t.Fatalf("step %s not found", stepID)
	return domain.WorkflowStep{}
}

func dispatchRecordsFor(t *testing.T, store *fakeStore, stepID string) []domain.WorkflowDispatchCheckpoint {
	t.Helper()
	got, err := store.ListWorkflowDispatchCheckpointsByStep(context.Background(), stepID)
	if err != nil {
		t.Fatalf("ListWorkflowDispatchCheckpointsByStep: %v", err)
	}
	return got
}

func ledgerPhases(t *testing.T, store *fakeStore, runID string) []string {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	out := make([]string, 0, len(cps))
	for _, cp := range cps {
		out = append(out, cp.DurablePhase)
	}
	return out
}

// ---- 1. intent only is never RUNNING ----------------------------------------

// TestAttemptWithOnlyDispatchIntentIsNeverRunning is the acceptance test for
// the whole file. It observes the durable state at the ONE instant it can only
// be the intent-only state — inside the launcher, after the intent write and
// before any launch has returned — and then again after a launch that failed.
//
// At both points the attempt row exists. At neither is anything running.
func TestAttemptWithOnlyDispatchIntentIsNeverRunning(t *testing.T) {
	ctx := context.Background()
	var observedAtLaunch struct {
		stepState domain.WorkflowStepState
		attempts  []domain.WorkflowAttempt
		records   []domain.WorkflowDispatchCheckpoint
		running   bool
	}
	launcher := &fakeWorkerLauncher{err: errors.New("runtime refused to start")}
	c, store, _ := newDispatchMachineCoordinator(launcher, &fakeSessionOwnership{})

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stepID := workStepIDOf(t, store, created.Run.ID)

	launcher.before = func(workflowcore.WorkerLaunchRequest) {
		step := stepByID(t, store, created.Run.ID, stepID)
		attempts, _ := store.ListWorkflowAttempts(ctx, step.ID)
		observedAtLaunch.stepState = step.State
		observedAtLaunch.attempts = attempts
		observedAtLaunch.records = dispatchRecordsFor(t, store, stepID)
		if len(attempts) > 0 {
			run, _, _ := store.GetWorkflowRun(ctx, created.Run.ID)
			observedAtLaunch.running = c.WorkerAttemptRunning(ctx, run.ID, step, attempts[len(attempts)-1])
		}
	}

	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", launcher.calls)
	}

	// The intent was durable BEFORE the launcher ran: that is the whole
	// ordering guarantee, and it is why a process that dies inside a launch
	// leaves a readable record of what it was doing.
	if len(observedAtLaunch.records) != 1 {
		t.Fatalf("dispatch records at launch time = %d, want exactly the intent", len(observedAtLaunch.records))
	}
	intent := observedAtLaunch.records[0]
	if intent.Phase != domain.DispatchPhaseWorkerLaunchIntent ||
		intent.LaunchOutcome != domain.LaunchOutcomeIntended ||
		intent.LaunchStage != domain.LaunchStageIntent {
		t.Fatalf("intent record = %+v, want the worker_launch_intent/intent/intended boundary", intent)
	}
	if intent.LaunchOutcome.Proven() || intent.LaunchOutcome.LicensesRunning() {
		t.Fatal("an intent must neither be a proven outcome nor license RUNNING")
	}
	if intent.LaunchedAt != nil {
		t.Fatalf("intent.LaunchedAt = %v, want nil: nothing had been launched yet", intent.LaunchedAt)
	}
	if intent.AttemptID == nil || *intent.AttemptID == "" {
		t.Fatal("the intent must name the attempt it belongs to")
	}
	if intent.IdempotencyKey == "" {
		t.Fatal("the intent must name the outbox command it was made under")
	}

	// An attempt row existed at that instant — and meant nothing.
	if len(observedAtLaunch.attempts) != 1 {
		t.Fatalf("attempts at launch time = %d, want exactly 1 open attempt", len(observedAtLaunch.attempts))
	}
	if observedAtLaunch.attempts[0].Outcome != "" {
		t.Fatal("the attempt opened at intent time must still be open when the launcher is entered")
	}
	if observedAtLaunch.stepState == domain.WorkflowStepRunning {
		t.Fatal("the step was RUNNING with only a dispatch intent persisted")
	}
	if observedAtLaunch.running {
		t.Fatal("WorkerAttemptRunning was true for an attempt whose dispatch had only been intended")
	}

	// And after the failed launch it is still not running, and there is still
	// no confirmation anywhere.
	step := stepByID(t, store, created.Run.ID, stepID)
	if step.State == domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want anything but running after a failed launch", step.State)
	}
	attempts, _ := store.ListWorkflowAttempts(ctx, stepID)
	if len(attempts) != 1 || attempts[len(attempts)-1].Outcome != domain.WorkflowAttemptFailed {
		t.Fatalf("attempts = %+v, want the one intent attempt, concluded failed", attempts)
	}
	if c.WorkerAttemptRunning(ctx, created.Run.ID, step, attempts[0]) {
		t.Fatal("WorkerAttemptRunning was true for a failed launch")
	}
	if status := c.WorkerDispatchStatusForStep(ctx, created.Run.ID, stepID); status.LicensesRunning() {
		t.Fatalf("dispatch status = %+v, want a phase that does not license RUNNING", status)
	}
}

// ---- 2. the launcher failed after intent ------------------------------------

// TestLauncherFailureAfterIntentRetriesPerPolicyCarryingEvidence covers the
// first failure shape: intent is durable, the launcher failed, and the answer
// is the pre-existing bounded retry — with the launch evidence recorded on a
// `failed` boundary rather than lost.
func TestLauncherFailureAfterIntentRetriesPerPolicyCarryingEvidence(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeWorkerLauncher{err: errors.New("tmux runtime: no such session")}
	c, store, clk := newDispatchMachineCoordinator(launcher, &fakeSessionOwnership{})

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stepID := workStepIDOf(t, store, created.Run.ID)
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Retry per policy: the run is NOT stopped, the step is NOT terminal, and
	// the step is emphatically not left running over a worker that does not
	// exist.
	if detail.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("a retryable launch failure must not stop the run")
	}
	step := stepByID(t, store, created.Run.ID, stepID)
	if step.State == domain.WorkflowStepRunning || step.State.Terminal() {
		t.Fatalf("step state = %q, want a non-terminal, non-running step while a retry is owed", step.State)
	}
	if !hasRetryableWorkerLaunchRecord(t, store, created.Run.ID) {
		t.Fatal("expected a durable, retryable worker_launch_error record")
	}

	records := dispatchRecordsFor(t, store, stepID)
	if len(records) != 2 {
		t.Fatalf("dispatch records = %d (%+v), want the intent plus its failure", len(records), records)
	}
	failure := records[1]
	if failure.Phase != domain.DispatchPhaseWorkerLaunchError ||
		failure.LaunchOutcome != domain.LaunchOutcomeFailed ||
		failure.LaunchStage != domain.LaunchStageSpawn {
		t.Fatalf("failure record = %+v, want worker_launch_error/spawn/failed", failure)
	}
	if failure.LaunchOutcome.LicensesRunning() {
		t.Fatal("a failed launch must never license RUNNING")
	}
	// The evidence captured so far travels with it: the attempt it belongs to,
	// the outbox command it was made under, the harness, and the runtime's own
	// words.
	if failure.AttemptID == nil || *failure.AttemptID != *records[0].AttemptID {
		t.Fatalf("failure record attempt = %v, want the attempt the intent opened", failure.AttemptID)
	}
	if failure.IdempotencyKey != records[0].IdempotencyKey {
		t.Fatal("the failure must be recorded under the same outbox command as its intent")
	}
	if failure.Harness == "" {
		t.Fatal("the failure must name the harness that failed")
	}
	if !strings.Contains(failure.Detail, "no such session") {
		t.Fatalf("failure detail = %q, want the runtime's own words", failure.Detail)
	}
	if failure.ErrorClass == "" {
		t.Fatal("the failure must carry its classification")
	}

	// The exact same failure, over and over, spends the budget and ends in
	// needs_attention — never in a permanently running step, and never in an
	// unbounded loop.
	// The retry floor is a durable time comparison, never a sleep: moving the
	// injected clock is all "time passed" ever means in this package.
	for i := 0; i < 5; i++ {
		clk.Advance(5 * time.Minute)
		if err := c.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	run, _, _ := store.GetWorkflowRun(ctx, created.Run.ID)
	step = stepByID(t, store, created.Run.ID, stepID)
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once the retry budget is spent", run.State)
	}
	if step.State != domain.WorkflowStepFailed {
		t.Fatalf("step state = %q, want failed: a launch that never happened is not a running step", step.State)
	}
	if step.SessionID != nil {
		t.Fatal("no session may be attributed to a step whose launch never completed")
	}
}

// ---- 3. launched, confirmation could not be persisted -----------------------

// TestLaunchSucceededButConfirmationPersistenceFailedIsItsOwnState covers the
// second failure shape, and the one the objective calls out by name: the
// launcher succeeded and the confirmation write did not.
//
// It must be collapsed into neither neighbour. Read as success, AO would run a
// worker it never recorded; read as failure, AO would relaunch over an agent
// that is already there.
func TestLaunchSucceededButConfirmationPersistenceFailedIsItsOwnState(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeWorkerLauncher{session: domain.SessionRecord{
		ID: "sess-launched", Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
	}}
	ownership := &fakeSessionOwnership{evidence: workflowcore.SessionOwnershipEvidence{
		Observed: true, RuntimeHandleID: "tmux-1", RuntimeLaunchID: "gen-7", AgentSessionID: "agent-9",
	}}
	c, store, _ := newDispatchMachineCoordinator(launcher, ownership)
	store.dispatchWriteErr = func(cp domain.WorkflowDispatchCheckpoint) error {
		if cp.Phase == domain.DispatchPhaseWorkerDispatched {
			return errors.New("disk full")
		}
		return nil
	}

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stepID := workStepIDOf(t, store, created.Run.ID)
	if _, err := c.StartRun(ctx, created.Run.ID); err == nil {
		t.Fatal("StartRun must surface the confirmation-persistence failure rather than report success")
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", launcher.calls)
	}

	// Not full success: no confirmation, no session on the step, no RUNNING,
	// and the outbox is still `dispatched` rather than acknowledged.
	step := stepByID(t, store, created.Run.ID, stepID)
	if step.State == domain.WorkflowStepRunning {
		t.Fatal("a launch whose confirmation could not be persisted must not be RUNNING")
	}
	if step.SessionID != nil {
		t.Fatalf("step session = %v, want none: the confirmation that would license it was never written", *step.SessionID)
	}
	entry := store.outbox["workflow-step-spawn:"+stepID]
	if entry.Status != domain.WorkflowOutboxDispatched {
		t.Fatalf("outbox status = %q, want it left dispatched so the next pass adopts rather than relaunches", entry.Status)
	}
	for _, r := range dispatchRecordsFor(t, store, stepID) {
		if r.LaunchOutcome == domain.LaunchOutcomeDispatched {
			t.Fatal("a confirmation record exists for a confirmation write that failed")
		}
	}

	// Not full failure either: the state is durably recorded, and it names the
	// session that WAS launched plus the ownership proof read back from it.
	cps, err := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var unconfirmed domain.WorkflowCheckpoint
	for _, cp := range cps {
		if cp.DurablePhase == "worker_launch_unconfirmed" {
			unconfirmed = cp
		}
	}
	if unconfirmed.ID == "" {
		t.Fatalf("no durable worker_launch_unconfirmed record; phases were %v", ledgerPhases(t, store, created.Run.ID))
	}
	if unconfirmed.SessionID == nil || *unconfirmed.SessionID != "sess-launched" {
		t.Fatalf("unconfirmed record session = %v, want the session the launcher returned", unconfirmed.SessionID)
	}
	for _, want := range []string{"sess-launched", "tmux-1", "gen-7", "agent-9", "disk full"} {
		if !strings.Contains(unconfirmed.RetryState, want) {
			t.Fatalf("unconfirmed record %q is missing %q", unconfirmed.RetryState, want)
		}
	}

	// And it reads back as its own phase, distinguishable from both neighbours.
	status := c.WorkerDispatchStatusForStep(ctx, created.Run.ID, stepID)
	if status.Phase != workflowcore.WorkerDispatchUnconfirmed {
		t.Fatalf("dispatch phase = %q, want %q", status.Phase, workflowcore.WorkerDispatchUnconfirmed)
	}
	if status.LicensesRunning() {
		t.Fatal("an unconfirmed launch must never license RUNNING")
	}
	if status.SessionID != "sess-launched" {
		t.Fatalf("dispatch status session = %q, want the launched session so a reconciler can adopt it", status.SessionID)
	}
}

// ---- 4. RUNNING, and only after the confirmation is durable -----------------

func TestStepReachesRunningOnlyAfterTheConfirmationIsDurable(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeWorkerLauncher{session: domain.SessionRecord{
		ID: "sess-ok", Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
	}}
	ownership := &fakeSessionOwnership{evidence: workflowcore.SessionOwnershipEvidence{
		Observed: true, RuntimeHandleID: "tmux-1", RuntimeLaunchID: "gen-7", AgentSessionID: "agent-9",
	}}
	c, store, _ := newDispatchMachineCoordinator(launcher, ownership)

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stepID := workStepIDOf(t, store, created.Run.ID)
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	records := dispatchRecordsFor(t, store, stepID)
	if len(records) != 2 {
		t.Fatalf("dispatch records = %+v, want the intent followed by its confirmation", records)
	}
	confirmation := records[1]
	if confirmation.Phase != domain.DispatchPhaseWorkerDispatched ||
		confirmation.LaunchOutcome != domain.LaunchOutcomeDispatched ||
		confirmation.LaunchStage != domain.LaunchStageConfirm {
		t.Fatalf("confirmation = %+v, want worker_dispatched/confirm/dispatched", confirmation)
	}
	if !confirmation.LaunchOutcome.LicensesRunning() {
		t.Fatal("a confirmed dispatch is the one outcome that licenses RUNNING")
	}
	// The launch evidence the schema exists to hold, actually held.
	if confirmation.SessionID == nil || *confirmation.SessionID != "sess-ok" {
		t.Fatalf("confirmation session = %v, want sess-ok", confirmation.SessionID)
	}
	if confirmation.RuntimeHandleID != "tmux-1" || confirmation.RuntimeLaunchID != "gen-7" || confirmation.AgentSessionID != "agent-9" {
		t.Fatalf("confirmation ownership evidence = %+v, want the proof read back from the launched session", confirmation)
	}
	if confirmation.Branch != "ao/wf" || confirmation.WorktreePath != "/ws/wf" {
		t.Fatalf("confirmation workspace = %q/%q, want the tree the launch landed in", confirmation.Branch, confirmation.WorktreePath)
	}
	if confirmation.LaunchedAt == nil {
		t.Fatal("a confirmed launch must record when the launch happened")
	}
	if ownership.calls == 0 {
		t.Fatal("the ownership proof must actually be read back before a launch is confirmed")
	}

	step := stepByID(t, store, created.Run.ID, stepID)
	if step.State != domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want running once the confirmation is durable", step.State)
	}
	if step.SessionID == nil || *step.SessionID != "sess-ok" {
		t.Fatalf("step session = %v, want sess-ok", step.SessionID)
	}
	attempts, _ := store.ListWorkflowAttempts(ctx, stepID)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v, want exactly one: the row opened at intent, reused by the confirmation", attempts)
	}
	if !c.WorkerAttemptRunning(ctx, created.Run.ID, step, attempts[0]) {
		t.Fatal("a confirmed, open attempt on a running step is running")
	}
	if entry := store.outbox["workflow-step-spawn:"+stepID]; entry.Status != domain.WorkflowOutboxAcknowledged {
		t.Fatalf("outbox status = %q, want acknowledged", entry.Status)
	}
}

// ---- 5. an intent that cannot be written launches nothing -------------------

func TestIntentThatCannotBePersistedNeverInvokesTheLauncher(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeWorkerLauncher{}
	c, store, _ := newDispatchMachineCoordinator(launcher, &fakeSessionOwnership{})
	store.dispatchWriteErr = func(cp domain.WorkflowDispatchCheckpoint) error {
		if cp.Phase == domain.DispatchPhaseWorkerLaunchIntent {
			return errors.New("disk full")
		}
		return nil
	}

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stepID := workStepIDOf(t, store, created.Run.ID)
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if launcher.calls != 0 {
		t.Fatalf("launcher calls = %d, want 0: a launch AO cannot record is a launch AO does not make", launcher.calls)
	}
	step := stepByID(t, store, created.Run.ID, stepID)
	if step.State == domain.WorkflowStepRunning {
		t.Fatal("nothing was launched, so nothing may be running")
	}
	if len(dispatchRecordsFor(t, store, stepID)) != 0 {
		t.Fatal("no dispatch boundary should exist when the intent write itself failed")
	}
	// The attempt row opened a moment before the failed write describes a launch
	// that never happened, and does not stay open over it.
	attempts, _ := store.ListWorkflowAttempts(ctx, stepID)
	for _, a := range attempts {
		if a.Outcome == "" {
			t.Fatalf("attempt %s left open over a dispatch that never launched", a.ID)
		}
	}
}

// ---- 6. a launcher that reports success and names nothing -------------------

func TestLauncherSuccessWithoutSessionEvidenceIsNeverConfirmed(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeWorkerLauncher{noEvidence: true}
	c, store, _ := newDispatchMachineCoordinator(launcher, &fakeSessionOwnership{})

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stepID := workStepIDOf(t, store, created.Run.ID)
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	step := stepByID(t, store, created.Run.ID, stepID)
	if step.State == domain.WorkflowStepRunning || step.SessionID != nil {
		t.Fatalf("step = %+v, want neither running nor holding a session AO cannot name", step)
	}
	records := dispatchRecordsFor(t, store, stepID)
	if len(records) != 2 || records[1].LaunchOutcome != domain.LaunchOutcomeAmbiguous {
		t.Fatalf("dispatch records = %+v, want the intent plus an ambiguous boundary", records)
	}
	if records[1].LaunchOutcome.Proven() {
		t.Fatal("an ambiguous launch is not a proven outcome")
	}
	// The outbox is left `dispatched`: the next pass goes through
	// adoptOrMarkAmbiguous, which adopts on evidence and never launches again.
	if entry := store.outbox["workflow-step-spawn:"+stepID]; entry.Status != domain.WorkflowOutboxDispatched {
		t.Fatalf("outbox status = %q, want dispatched", entry.Status)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1: an ambiguous launch is never retried", launcher.calls)
	}
}

// ---- 7. the unconfirmed state is recoverable, and never by relaunching ------

// TestUnconfirmedLaunchIsAdoptedRatherThanRelaunched is the reason phase 3's
// failure is a state and not a log line: a later pass has to be able to act on
// it. The launched session is adopted from its own natural key, the confirmation
// finally lands, and the step reaches RUNNING — with the launcher called exactly
// once from beginning to end.
func TestUnconfirmedLaunchIsAdoptedRatherThanRelaunched(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeWorkerLauncher{session: domain.SessionRecord{
		ID: "sess-launched", Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
	}}
	c, store, clk := newDispatchMachineCoordinator(launcher, &fakeSessionOwnership{
		evidence: workflowcore.SessionOwnershipEvidence{Observed: true, RuntimeLaunchID: "gen-7"},
	})
	store.dispatchWriteErr = func(cp domain.WorkflowDispatchCheckpoint) error {
		if cp.Phase == domain.DispatchPhaseWorkerDispatched {
			return errors.New("disk full")
		}
		return nil
	}

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stepID := workStepIDOf(t, store, created.Run.ID)
	if _, err := c.StartRun(ctx, created.Run.ID); err == nil {
		t.Fatal("StartRun must surface the confirmation-persistence failure")
	}
	if got := c.WorkerDispatchStatusForStep(ctx, created.Run.ID, stepID).Phase; got != workflowcore.WorkerDispatchUnconfirmed {
		t.Fatalf("dispatch phase = %q, want unconfirmed", got)
	}

	// Storage recovers, and the adoption window opens.
	store.dispatchWriteErr = nil
	clk.Advance(2 * time.Minute)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1: an unconfirmed launch is adopted, never relaunched", launcher.calls)
	}
	step := stepByID(t, store, created.Run.ID, stepID)
	if step.SessionID == nil || *step.SessionID != "sess-launched" {
		t.Fatalf("step session = %v, want the session that was launched all along", step.SessionID)
	}
	if step.State != domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want running once the confirmation finally landed", step.State)
	}
	status := c.WorkerDispatchStatusForStep(ctx, created.Run.ID, stepID)
	if !status.LicensesRunning() {
		t.Fatalf("dispatch status = %+v, want a confirmed phase", status)
	}
	attempts, _ := store.ListWorkflowAttempts(ctx, stepID)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v, want the one attempt opened at intent time", attempts)
	}
	if !c.WorkerAttemptRunning(ctx, created.Run.ID, step, attempts[0]) {
		t.Fatal("the adopted attempt is running once its dispatch is confirmed")
	}
}

// ---- 8. an unprovable ownership is not a confirmation ----------------------

// TestUnprovenOwnershipNeverConfirmsOrRuns is the other half of phase 3's
// evidence requirement. A launcher-returned session id is the launcher's WORD
// that it started something; only the ownership read-back says that session
// exists, belongs to this launch, and is fenced by a launch generation AO can
// tell apart from a row that outlived the process behind it.
//
// So an ownership proof AO could not read produces no confirmation and no
// RUNNING — for every way it can fail to read one.
func TestUnprovenOwnershipNeverConfirmsOrRuns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ownership workflowcore.SessionOwnershipEvidence
	}{
		{
			name:      "probe reports the session unreadable",
			ownership: workflowcore.SessionOwnershipEvidence{Unavailable: "the session row could not be found"},
		},
		{
			name:      "probe failed outright",
			ownership: workflowcore.SessionOwnershipEvidence{Unavailable: "reading the session failed: disk i/o error"},
		},
		{
			// The launcher's word, verbatim, with nothing behind it: handles and
			// ids present, and no read-back that says any of them are real.
			name: "handles present but never observed",
			ownership: workflowcore.SessionOwnershipEvidence{
				RuntimeHandleID: "tmux-1", RuntimeLaunchID: "gen-7", AgentSessionID: "agent-9",
				Unavailable: "no session read path is wired into this coordinator",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			launcher := &fakeWorkerLauncher{session: domain.SessionRecord{
				ID: "sess-claimed", Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
			}}
			c, store, _ := newDispatchMachineCoordinator(launcher, &fakeSessionOwnership{evidence: tc.ownership})

			created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			stepID := workStepIDOf(t, store, created.Run.ID)
			if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
				t.Fatalf("StartRun: %v", err)
			}

			// No confirmation, at all.
			for _, r := range dispatchRecordsFor(t, store, stepID) {
				if r.Phase == domain.DispatchPhaseWorkerDispatched || r.LaunchOutcome == domain.LaunchOutcomeDispatched {
					t.Fatalf("a dispatched confirmation was written on unproven ownership: %+v", r)
				}
			}
			// And therefore no RUNNING, no session on the step, no acknowledged
			// outbox — the three things a confirmation licenses.
			step := stepByID(t, store, created.Run.ID, stepID)
			if step.State == domain.WorkflowStepRunning {
				t.Fatal("the step reached RUNNING on a session whose ownership was never proven")
			}
			if step.SessionID != nil {
				t.Fatalf("step session = %q, want none until ownership is proven", *step.SessionID)
			}
			if entry := store.outbox["workflow-step-spawn:"+stepID]; entry.Status != domain.WorkflowOutboxDispatched {
				t.Fatalf("outbox status = %q, want it left dispatched for a later adoption", entry.Status)
			}
			attempts, _ := store.ListWorkflowAttempts(ctx, stepID)
			if len(attempts) != 1 {
				t.Fatalf("attempts = %+v, want the one attempt opened at intent time", attempts)
			}
			if c.WorkerAttemptRunning(ctx, created.Run.ID, step, attempts[0]) {
				t.Fatal("WorkerAttemptRunning was true for a launch whose ownership was never proven")
			}

			// It IS durably recorded, as its own state, naming the session so a
			// reconciler can adopt it rather than launch a second worker over it.
			status := c.WorkerDispatchStatusForStep(ctx, created.Run.ID, stepID)
			if status.Phase != workflowcore.WorkerDispatchUnconfirmed {
				t.Fatalf("dispatch phase = %q, want unconfirmed", status.Phase)
			}
			if status.LicensesRunning() {
				t.Fatal("an unproven launch must never license RUNNING")
			}
			if status.SessionID != "sess-claimed" {
				t.Fatalf("dispatch status session = %q, want the session the launcher claimed", status.SessionID)
			}
			var unconfirmed domain.WorkflowCheckpoint
			cps, _ := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
			for _, cp := range cps {
				if cp.DurablePhase == "worker_launch_unconfirmed" {
					unconfirmed = cp
				}
			}
			if unconfirmed.ID == "" {
				t.Fatalf("no durable unconfirmed record; phases were %v", ledgerPhases(t, store, created.Run.ID))
			}
			// The reason is recorded, because "AO could not prove who owns this"
			// and "AO could not write down what it proved" need different
			// answers from whoever reconciles them.
			if !strings.Contains(unconfirmed.RetryState, "ownership_unproven") {
				t.Fatalf("unconfirmed record %q does not say WHY it is unconfirmed", unconfirmed.RetryState)
			}
			if !strings.Contains(unconfirmed.RetryState, tc.ownership.Unavailable) {
				t.Fatalf("unconfirmed record %q does not carry the probe's own words", unconfirmed.RetryState)
			}
		})
	}
}

// TestConfirmationReasonDistinguishesWriteFailureFromUnprovenOwnership pins the
// distinction the reason exists for: both leave the same phase, and a
// reconciler that could not tell them apart could not choose between retrying
// the write and finding out who owns the session.
func TestConfirmationReasonDistinguishesWriteFailureFromUnprovenOwnership(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeWorkerLauncher{session: domain.SessionRecord{ID: "sess-observed"}}
	c, store, _ := newDispatchMachineCoordinator(launcher, &fakeSessionOwnership{
		evidence: workflowcore.SessionOwnershipEvidence{Observed: true, RuntimeLaunchID: "gen-7"},
	})
	store.dispatchWriteErr = func(cp domain.WorkflowDispatchCheckpoint) error {
		if cp.Phase == domain.DispatchPhaseWorkerDispatched {
			return errors.New("disk full")
		}
		return nil
	}

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err == nil {
		t.Fatal("StartRun must surface the confirmation-persistence failure")
	}
	cps, _ := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	var unconfirmed domain.WorkflowCheckpoint
	for _, cp := range cps {
		if cp.DurablePhase == "worker_launch_unconfirmed" {
			unconfirmed = cp
		}
	}
	if unconfirmed.ID == "" {
		t.Fatal("no durable unconfirmed record")
	}
	if !strings.Contains(unconfirmed.RetryState, "confirmation_write_failed") {
		t.Fatalf("unconfirmed record %q, want the write-failure reason, not the ownership one", unconfirmed.RetryState)
	}
	if strings.Contains(unconfirmed.RetryState, "ownership_unproven") {
		t.Fatalf("unconfirmed record %q blames ownership for a write failure", unconfirmed.RetryState)
	}
}
