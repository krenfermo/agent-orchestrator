package workflow_test

// Regression coverage for incident wf-57f90ff2 (task 4 of master wf-872e7f57):
// a Codex worker that was visibly executing commands, running tests and
// investigating a compile failure, which AO durably recorded as
//
//	worker_observed_worker_active
//	worker_blocked / "worker awaiting input/blocked — needs human attention"
//	run = needs_attention, work step = waiting
//
// The worker went on to finish its turn unattended two and a half minutes
// later, so nothing was ever waiting for a person.
//
// The invariant these tests hold: AO may not park a run on "the worker needs
// you" because the worker is still alive, or because no completion signal has
// arrived. It needs positive evidence that a person is actually being asked
// something.
//
// Everything here runs against a real *sqlite.Store and the real GetRun /
// ContinueRun / observation paths. The fakes are the pane, the session facts
// and the clock.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// codexWorkingPaneText is what capture-pane returned while the incident's
// worker was busy: Codex's in-flight marker, some tool output, and no question
// of any kind.
func codexWorkingPaneText() string {
	return strings.Join([]string{
		"$ go build ./...",
		"internal/storage/sqlite/gen/models.go:41:2: undefined: BranchLockOwnershipKind",
		"$ npm run sqlc",
		"• Working (2m 10s • esc to interrupt)",
		"› investigate the sqlc failure",
		"",
		"gpt-5.6-sol low · ~/project",
	}, "\n")
}

// codexApprovalPaneText is a REAL Codex approval prompt: a question line
// followed by Codex's numbered option block, which its QuestionParser
// reconstructs.
func codexApprovalPaneText() string {
	return strings.Join([]string{
		"Should the retry cooldown be 2s or 8s?",
		"› 1. 2 seconds",
		"  2. 8 seconds",
	}, "\n")
}

// neverSpawner fails the test if anything tries to launch a worker. Every test
// in this file is an OBSERVATION test: no dispatch may happen on any of these
// paths, however many times they are polled.
type neverSpawner struct{ t *testing.T }

func (s neverSpawner) Spawn(context.Context, ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.t.Helper()
	s.t.Fatal("Spawn was called during a pure observation pass — a duplicate worker launch")
	return domain.SessionRecord{}, 0, 0, nil
}

// observedFixture is one run whose work step is already dispatched and running,
// wired the way the daemon wires it: real store, real questions store, a pane
// the test controls, and a spawner that must never be called.
type observedFixture struct {
	t      *testing.T
	ctx    context.Context
	store  *sqlite.Store
	facts  *fakeSessionFacts
	pane   *fakePaneReader
	clk    *fakeClock
	coord  *workflowcore.Coordinator
	runID  string
	stepID string
	sessID domain.SessionID
}

func newObservedFixture(t *testing.T, harness domain.AgentHarness, activity domain.ActivityState, paneText string) *observedFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	f := &observedFixture{
		t: t, ctx: ctx, store: store,
		facts: newFakeSessionFacts(),
		pane:  &fakePaneReader{text: paneText},
		clk:   &fakeClock{t: time.Date(2026, 8, 23, 16, 9, 0, 0, time.UTC)},
	}
	f.build()
	f.seedRun(harness, activity)
	return f
}

func (f *observedFixture) build() {
	f.coord = workflowcore.New(workflowcore.Deps{
		Store:          f.store,
		Projects:       f.store,
		Spawner:        neverSpawner{f.t},
		SessionFacts:   f.facts,
		QuestionsStore: f.store,
		PaneReader:     f.pane,
		Clock:          f.clk.Now,
	})
}

// restart rebuilds the Coordinator over the same store — what a daemon restart
// actually is.
func (f *observedFixture) restart() { f.build() }

func (f *observedFixture) seedRun(harness domain.AgentHarness, activity domain.ActivityState) {
	f.t.Helper()
	rec, err := f.store.CreateSession(f.ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: harness,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		f.t.Fatalf("seed session: %v", err)
	}
	f.sessID = rec.ID

	created, err := f.coord.CreateRun(f.ctx, "p", "Ownership-aware branch locking and parallel dispatch")
	if err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	for _, s := range created.Steps {
		if s.Step.Kind == domain.WorkflowStepWork {
			f.stepID = s.Step.ID
		}
	}
	now := f.clk.Now()
	mustStep := func(from, to domain.WorkflowStepState) {
		if _, err := f.store.UpdateWorkflowStepState(f.ctx, f.stepID, from, to, now); err != nil {
			f.t.Fatalf("step %s -> %s: %v", from, to, err)
		}
	}
	mustStep(domain.WorkflowStepPending, domain.WorkflowStepReady)
	mustStep(domain.WorkflowStepReady, domain.WorkflowStepRunning)
	if _, err := f.store.UpdateWorkflowStepSession(f.ctx, f.stepID, string(f.sessID), now); err != nil {
		f.t.Fatalf("attach session: %v", err)
	}
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil {
		f.t.Fatalf("run -> running: %v", err)
	}
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-dispatched", WorkflowRunID: f.runID, WorkflowStepID: &f.stepID, ProjectID: "p",
		SessionID: stringPtr(string(f.sessID)), Branch: "feat/engineering-control-center",
		DurablePhase: "worker_dispatched", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		f.t.Fatalf("seed checkpoint: %v", err)
	}
	f.setActivity(harness, activity, now)
}

// setActivity publishes the session facts AO reads. lastActivityAt is what the
// corroboration window is measured against.
func (f *observedFixture) setActivity(harness domain.AgentHarness, state domain.ActivityState, lastActivityAt time.Time) {
	f.facts.put(domain.SessionRecord{
		ID: f.sessID, ProjectID: "p", Kind: domain.KindWorker, Harness: harness,
		Activity:      domain.Activity{State: state, LastActivityAt: lastActivityAt},
		FirstSignalAt: f.clk.Now().Add(-3 * time.Minute),
		Metadata:      domain.SessionMetadata{RuntimeHandleID: "handle-" + string(f.sessID)},
	})
}

func (f *observedFixture) poll(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		if _, err := f.coord.GetRun(f.ctx, f.runID); err != nil {
			f.t.Fatalf("GetRun %d: %v", i, err)
		}
		f.clk.Advance(2 * time.Second)
	}
}

func (f *observedFixture) run() domain.WorkflowRun {
	f.t.Helper()
	r, ok, err := f.store.GetWorkflowRun(f.ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v ok=%v", err, ok)
	}
	return r
}

func (f *observedFixture) step() domain.WorkflowStep {
	f.t.Helper()
	steps, err := f.store.ListWorkflowSteps(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			return s
		}
	}
	f.t.Fatal("no work step")
	return domain.WorkflowStep{}
}

func (f *observedFixture) countPhase(phase string) int {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

func (f *observedFixture) phases() []string {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	out := make([]string, 0, len(cps))
	for _, cp := range cps {
		out = append(out, cp.DurablePhase)
	}
	return out
}

func (f *observedFixture) questions() []domain.WorkflowQuestion {
	f.t.Helper()
	qs, err := f.store.ListWorkflowQuestionsByRun(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowQuestionsByRun: %v", err)
	}
	return qs
}

// assertNotStopped is the invariant, spelled out once.
func (f *observedFixture) assertNotStopped(what string) {
	f.t.Helper()
	if got := f.countPhase("worker_blocked"); got != 0 {
		f.t.Fatalf("%s: %d worker_blocked checkpoint(s) written; phases = %v", what, got, f.phases())
	}
	if got := f.run().State; got == domain.WorkflowRunNeedsAttention {
		f.t.Fatalf("%s: run state = %q, want it left running", what, got)
	}
	if got := f.step().State; got != domain.WorkflowStepRunning {
		f.t.Fatalf("%s: work step = %q, want running", what, got)
	}
}

// ---------------------------------------------------------------------------
// The incident itself.
// ---------------------------------------------------------------------------

func TestCodexWorkerRunningCommandsIsNeverClassifiedAsAwaitingHuman(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexWorkingPaneText())

	// Codex's PermissionRequest hook latched the session at waiting_input and
	// nothing in its hook set can clear it before the turn ends — while the
	// agent goes on building, running tests and reading output.
	for i := 0; i < 12; i++ {
		f.setActivity(domain.HarnessCodex, domain.ActivityWaitingInput, f.clk.Now())
		f.poll(1)
	}

	f.assertNotStopped("a Codex worker executing commands")
	if got := f.questions(); len(got) != 0 {
		t.Fatalf("questions = %+v, want none: no question was on the pane, and an unparseable pane is not evidence of one", got)
	}
	// The step must still be pointed at the same single session.
	if sid := f.step().SessionID; sid == nil || *sid != string(f.sessID) {
		t.Fatalf("work step session = %v, want the original %s", sid, f.sessID)
	}
}

// The other half of the same fix, one layer down: the detector must not write a
// human_required row when the pane carries no question. That fabricated row was
// the only "evidence" the incident's stop ever had.
func TestUnparseablePaneNeverManufacturesAHumanRequiredQuestion(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexWorkingPaneText())
	f.poll(5)

	for _, q := range f.questions() {
		t.Fatalf("a question was persisted from a pane with no question on it: %+v", q)
	}
	if f.pane.calls == 0 {
		t.Fatal("the pane was never read; this test did not exercise detection at all")
	}
}

// ---------------------------------------------------------------------------
// A. A genuine Codex input request still stops the run — exactly once.
// ---------------------------------------------------------------------------

func TestGenuineCodexQuestionStopsTheRunExactlyOnce(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexApprovalPaneText())
	f.poll(10)

	if got := f.countPhase("worker_blocked"); got != 1 {
		t.Fatalf("worker_blocked checkpoints = %d, want exactly 1; phases = %v", got, f.phases())
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	if got := f.step().State; got != domain.WorkflowStepWaiting {
		t.Fatalf("work step = %q, want waiting", got)
	}
	qs := f.questions()
	if len(qs) != 1 {
		t.Fatalf("questions = %d, want exactly 1", len(qs))
	}
	if strings.TrimSpace(qs[0].QuestionText) == "" {
		t.Fatal("the corroborating question must carry the text AO actually read from the pane")
	}
	// And the ledger must name what it decided, not mislabel it "active" the
	// way the incident's own worker_observed_worker_active checkpoint did.
	if got := f.countPhase("worker_observed_worker_awaiting_human"); got != 1 {
		t.Fatalf("worker_observed_worker_awaiting_human = %d, want 1; phases = %v", got, f.phases())
	}
}

// ---------------------------------------------------------------------------
// B. A permission request stops the run. `blocked` is proof on its own: it is
//    only entered from a correlated permission dialog and clears itself when
//    that tool completes or the turn ends, so it cannot go stale mid-turn.
// ---------------------------------------------------------------------------

func TestPermissionRequestStopsTheRunWithoutNeedingAParsedQuestion(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessClaudeCode, domain.HarnessCodex} {
		t.Run(string(harness), func(t *testing.T) {
			// Deliberately an unparseable pane: `blocked` must stand on its own.
			f := newObservedFixture(t, harness, domain.ActivityBlocked, codexWorkingPaneText())
			f.poll(4)

			if got := f.countPhase("worker_blocked"); got != 1 {
				t.Fatalf("worker_blocked checkpoints = %d, want exactly 1; phases = %v", got, f.phases())
			}
			if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run state = %q, want needs_attention", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// C. A quiet but healthy worker is not a stop.
// ---------------------------------------------------------------------------

func TestQuietHealthyWorkerIsNeverAFalseStop(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityIdle, codexWorkingPaneText())
	f.clk.Advance(20 * time.Minute)
	f.setActivity(domain.HarnessCodex, domain.ActivityIdle, f.clk.Now().Add(-20*time.Minute))
	f.poll(10)

	f.assertNotStopped("a quiet but healthy worker")
}

// ---------------------------------------------------------------------------
// D. A disappeared/crashed session keeps its existing recovery behaviour.
// ---------------------------------------------------------------------------

func TestTerminatedSessionStillFailsTheStep(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexWorkingPaneText())
	// The session disappears entirely — the reaper's shape.
	f.facts.byID = map[domain.SessionID]domain.SessionRecord{}
	f.facts.byIssue = map[string]domain.SessionRecord{}
	f.poll(2)

	if got := f.step().State; got != domain.WorkflowStepFailed {
		t.Fatalf("work step = %q, want failed for a vanished session", got)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	if f.countPhase("worker_observed_worker_failed") != 1 {
		t.Fatalf("expected a worker_failed observation; phases = %v", f.phases())
	}
}

// ---------------------------------------------------------------------------
// E + F. A restart mid-turn adopts the running worker, and repeated polling is
//        idempotent — no duplicate launch, no accumulating checkpoints.
// ---------------------------------------------------------------------------

func TestRestartDuringAnActiveWorkerAdoptsWithoutDuplicating(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexWorkingPaneText())
	f.poll(3)

	f.restart()
	if err := f.coord.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	before := len(f.phases())

	for i := 0; i < 25; i++ {
		f.setActivity(domain.HarnessCodex, domain.ActivityWaitingInput, f.clk.Now())
		f.poll(1)
		if err := f.coord.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}

	f.assertNotStopped("a worker observed across a restart and 25 further passes")
	if after := len(f.phases()); after != before {
		t.Fatalf("checkpoints grew from %d to %d over repeated idempotent polling: %v", before, after, f.phases())
	}
	sessions, err := f.store.ListSessions(f.ctx, "p")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want exactly 1 (no duplicate worker)", len(sessions))
	}
	// neverSpawner would already have failed the test if a launch had been
	// attempted; this states the same guarantee where a reader will look for it.
	if sid := f.step().SessionID; sid == nil || *sid != string(f.sessID) {
		t.Fatalf("work step session = %v, want the original %s", sid, f.sessID)
	}
}

// ---------------------------------------------------------------------------
// G. Claude Code is not regressed: a real question still stops the run, and a
//    bare waiting_input with nothing on the pane still does not.
// ---------------------------------------------------------------------------

func TestClaudeCodeObservationIsNotRegressed(t *testing.T) {
	t.Run("real question still stops", func(t *testing.T) {
		f := newObservedFixture(t, domain.HarnessClaudeCode, domain.ActivityWaitingInput, ambiguousCooldownPaneText())
		f.poll(6)
		if got := f.countPhase("worker_blocked"); got != 1 {
			t.Fatalf("worker_blocked = %d, want exactly 1; phases = %v", got, f.phases())
		}
		if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
			t.Fatalf("run state = %q, want needs_attention", got)
		}
	})

	t.Run("bare waiting_input does not stop", func(t *testing.T) {
		f := newObservedFixture(t, domain.HarnessClaudeCode, domain.ActivityWaitingInput, "just some output\nno question here\n")
		for i := 0; i < 6; i++ {
			f.setActivity(domain.HarnessClaudeCode, domain.ActivityWaitingInput, f.clk.Now())
			f.poll(1)
		}
		f.assertNotStopped("claude-code waiting_input with no observed question")
	})
}

// ---------------------------------------------------------------------------
// H. A fallback Claude -> Codex worker is observed under Codex's semantics —
//    the session's own harness decides, not whatever was routed first.
// ---------------------------------------------------------------------------

func TestFallbackToCodexKeepsCorrectObservationSemantics(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexWorkingPaneText())

	// The run's history says claude-code was tried first and failed, exactly as
	// the incident's did before it failed over.
	attempt, err := f.store.CreateWorkflowAttempt(f.ctx, "wfa-claude", f.stepID, "claude-code", "", f.clk.Now())
	if err != nil {
		t.Fatalf("CreateWorkflowAttempt: %v", err)
	}
	if err := f.store.UpdateWorkflowAttemptOutcome(f.ctx, attempt.ID, f.clk.Now(),
		domain.WorkflowAttemptFailed, domain.WorkflowErrorAgentStartFailed); err != nil {
		t.Fatalf("UpdateWorkflowAttemptOutcome: %v", err)
	}
	if _, err := f.store.CreateWorkflowAttempt(f.ctx, "wfa-codex", f.stepID, "codex", "", f.clk.Now()); err != nil {
		t.Fatalf("CreateWorkflowAttempt(codex): %v", err)
	}

	for i := 0; i < 8; i++ {
		f.setActivity(domain.HarnessCodex, domain.ActivityWaitingInput, f.clk.Now())
		f.poll(1)
	}
	f.assertNotStopped("a Codex worker reached by failover from claude-code")
}

// ---------------------------------------------------------------------------
// The uncorroborated tail stays bounded: a session that goes completely silent
// in a state AO cannot explain reaches a truthful ambiguity, never a claim that
// the worker is waiting on the user.
// ---------------------------------------------------------------------------

func TestSilentUncorroboratedNeedsInputReachesAnHonestAmbiguity(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexWorkingPaneText())
	silentSince := f.clk.Now()
	f.poll(2)
	f.assertNotStopped("before the corroboration window elapses")

	f.clk.Advance(30 * time.Minute)
	f.setActivity(domain.HarnessCodex, domain.ActivityWaitingInput, silentSince)
	f.poll(2)

	if got := f.countPhase("worker_blocked"); got != 0 {
		t.Fatalf("worker_blocked = %d: an uncorroborated reading must never become a worker-blocked stop", got)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once the state is provably unexplainable", got)
	}
	if f.countPhase("worker_observed_worker_observation_ambiguous") != 1 {
		t.Fatalf("expected an honest ambiguity observation; phases = %v", f.phases())
	}
	if got := f.countPhase("worker_dispatch_ambiguous"); got != 1 {
		t.Fatalf("worker_dispatch_ambiguous stops = %d, want 1; phases = %v", got, f.phases())
	}
}

// ---------------------------------------------------------------------------
// The already-persisted incident recovers through one ordinary Continue.
// ---------------------------------------------------------------------------

// seedHistoricalFalseWorkerBlocked reproduces exactly what wf-57f90ff2 has on
// disk: an evidence-free human_required question, a worker_blocked stop, a work
// step resting at waiting, and a run in needs_attention.
func (f *observedFixture) seedHistoricalFalseWorkerBlocked() {
	f.t.Helper()
	now := f.clk.Now()
	stepID := domain.WorkflowStepID(f.stepID)
	if _, _, err := f.store.InsertWorkflowQuestion(f.ctx, domain.WorkflowQuestion{
		ID: "wfq-historical", WorkflowRunID: domain.WorkflowRunID(f.runID), WorkflowStepID: &stepID,
		SessionID: &f.sessID, AskingHarness: domain.HarnessCodex, AskingRole: "work",
		Fingerprint: "25e71ca9d392cb334f2409412747001f003b06f241619a3cb9503a4ece9cbaab",
		// The shape the old detector wrote: no text, no choices, unknown.
		QuestionText: "", CaptureProvider: "tmux", CaptureParserVersion: "8k-a.v1",
		CaptureRangeLines: 120, Certainty: domain.QuestionCertaintyUnknown,
		Classification:       domain.QuestionClassificationHumanRequired,
		ClassificationReason: "question text could not be reconstructed reliably",
		State:                domain.QuestionStateHumanRequired, CreatedAt: now,
	}); err != nil {
		f.t.Fatalf("seed question: %v", err)
	}
	if _, err := f.store.UpdateWorkflowStepState(f.ctx, f.stepID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
		f.t.Fatalf("seed step -> waiting: %v", err)
	}
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID, domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, now); err != nil {
		f.t.Fatalf("seed run -> needs_attention: %v", err)
	}
	for _, phase := range []string{"worker_observed_worker_active", "worker_blocked"} {
		if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
			ID: "wfc-historical-" + phase, WorkflowRunID: f.runID, WorkflowStepID: &f.stepID, ProjectID: "p",
			SessionID:    stringPtr(string(f.sessID)),
			NextAction:   "worker awaiting input/blocked — needs human attention",
			DurablePhase: phase, PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
		}); err != nil {
			f.t.Fatalf("seed checkpoint %s: %v", phase, err)
		}
	}
}

func TestContinueRecoversTheHistoricalFalseWorkerBlocked(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexWorkingPaneText())
	f.seedHistoricalFalseWorkerBlocked()
	f.restart()

	// Polling alone must change nothing: only an explicit Continue may reopen a
	// stop a person owns.
	f.poll(3)
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state after polling = %q, want it left parked", got)
	}
	if got := f.step().State; got != domain.WorkflowStepWaiting {
		t.Fatalf("work step after polling = %q, want waiting", got)
	}

	// The worker has since finished its turn, exactly as the real one did.
	f.setActivity(domain.HarnessCodex, domain.ActivityIdle, f.clk.Now())
	if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	if got := f.run().State; got == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want it un-parked by the recheck", got)
	}
	if f.countPhase("worker_blocked_recheck") != 1 {
		t.Fatalf("expected an audited recheck; phases = %v", f.phases())
	}
	// The evidence-free question must be retired, so it stops blocking dispatch.
	for _, q := range f.questions() {
		if q.State.Open() {
			t.Fatalf("question %s is still open after the recheck: %+v", q.ID, q)
		}
	}
	// Repeat Continue must be safe and bounded.
	for i := 0; i < 20; i++ {
		if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}
	if got := f.countPhase("worker_blocked_recheck"); got > 3 {
		t.Fatalf("rechecks = %d, want a bounded number", got)
	}
	sessions, err := f.store.ListSessions(f.ctx, "p")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 (no duplicate worker on recovery)", len(sessions))
	}
}

// A worker-blocked stop that a REAL question still supports is never reopened,
// however many times Continue is pressed.
func TestContinueDoesNotReopenAGenuinelyBlockedWorker(t *testing.T) {
	f := newObservedFixture(t, domain.HarnessCodex, domain.ActivityWaitingInput, codexApprovalPaneText())
	f.poll(6)
	if f.countPhase("worker_blocked") != 1 {
		t.Fatalf("fixture never reached the genuine stop; phases = %v", f.phases())
	}

	for i := 0; i < 5; i++ {
		if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want it still parked on a real question", got)
	}
	if got := f.countPhase("worker_blocked_recheck"); got != 0 {
		t.Fatalf("rechecks = %d, want 0: a stop with surviving evidence must never be reopened", got)
	}
	open := 0
	for _, q := range f.questions() {
		if q.State.Open() {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("open questions = %d, want the real one left alone", open)
	}
}
