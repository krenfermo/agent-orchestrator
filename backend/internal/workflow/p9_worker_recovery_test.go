package workflow_test

// P9 BLOCK D — crash/restart recovery of a worker launch, against a REAL
// *sqlite.Store and the real Coordinator dispatch / reconciliation paths.
//
// A daemon crash is reproduced exactly, with no sleeps and no timing: the
// launch runs on its own goroutine, and the Spawner (or the branch-lock renewal
// that follows the confirmation) calls runtime.Goexit at the precise boundary
// under test. Everything written before that point is durable; nothing after it
// happens; no in-memory state survives, because the next "boot" is a brand-new
// Coordinator over the same store.
//
// The runtime is the P9 port with a scripted proof per session, so every
// ownership answer — owned, a mismatch, a legacy row, an unreadable probe — is
// driven explicitly. The real-tmux counterpart lives in
// internal/workerownership/tmuxe2e.

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowports "github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p9Spawner writes a session row carrying COMPLETE runtime provenance, the way
// session_manager does, and can simulate the daemon dying inside a launch.
type p9Spawner struct {
	mu      sync.Mutex
	store   *sqlite.Store
	clk     *fakeClock
	calls   int
	created int
	// ids are the session ids the store actually assigned, by call number.
	ids map[int]domain.SessionID
	// crashBeforeRuntime / crashAfterRuntime name the call numbers inside which
	// the daemon dies: before anything was created, or after the session row and
	// its runtime exist but before Spawn returned.
	crashBeforeRuntime map[int]bool
	crashAfterRuntime  map[int]bool
}

func (s *p9Spawner) Spawn(ctx context.Context, cfg workflowports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	before, after := s.crashBeforeRuntime[n], s.crashAfterRuntime[n]
	s.mu.Unlock()
	if before {
		runtime.Goexit()
	}
	id := domain.SessionID(fmt.Sprintf("p9-sess-%d", n)) // the store assigns the real id
	launch := fmt.Sprintf("p9-launch-%d", n)
	now := s.clk.Now()
	rec := domain.SessionRecord{
		ID: id, ProjectID: cfg.ProjectID, Kind: cfg.Kind, Harness: cfg.Harness, IssueID: cfg.IssueID,
		FirstSignalAt: now,
		Activity:      domain.Activity{State: domain.ActivityActive, LastActivityAt: now, LastSignalAt: now},
		Metadata: domain.SessionMetadata{
			Branch: "ao/p9", WorkspacePath: "/wt/p9",
			RuntimeHandleID: string(id), RuntimeInstanceID: fmt.Sprintf("$%d", n),
			RuntimeLaunchID: launch, RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(id, launch),
		},
		CreatedAt: now, UpdatedAt: now,
	}
	created, err := s.store.CreateSession(ctx, rec)
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	// The ownership token binds the id the STORE assigned, exactly as
	// session_manager mints it after the row exists.
	created.Metadata = rec.Metadata
	created.Metadata.RuntimeHandleID = string(created.ID)
	created.Metadata.RuntimeOwnerToken = domain.SessionRuntimeOwnerToken(created.ID, launch)
	created.FirstSignalAt, created.Activity = rec.FirstSignalAt, rec.Activity
	if err := s.store.UpdateSession(ctx, created); err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	s.mu.Lock()
	s.created++
	s.ids[n] = created.ID
	s.mu.Unlock()
	if after {
		runtime.Goexit()
	}
	return created, 0, 0, nil
}

func (s *p9Spawner) counts() (calls, created int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.created
}

// crashOnRenewLocks dies inside the branch-lock renewal confirmWorkerDispatch
// performs AFTER the confirmation, the acknowledge and the session bind, and
// BEFORE the step goes RUNNING.
type crashOnRenewLocks struct{ *fakeBranchLocks }

func (crashOnRenewLocks) Renew(context.Context, string, string, string) { runtime.Goexit() }

// unconfirmedOwnership reads the session back but refuses to call it observed,
// so the launch is durably recorded as launched-and-unconfirmed WITH its launch
// id and worktree — the shape a confirmation write failure leaves.
type unconfirmedOwnership struct{ store *sqlite.Store }

func (o unconfirmedOwnership) ObserveSessionOwnership(ctx context.Context, id domain.SessionID) workflowcore.SessionOwnershipEvidence {
	rec, _, _ := o.store.GetSession(ctx, id)
	return workflowcore.SessionOwnershipEvidence{
		SessionID: id, RuntimeHandleID: rec.Metadata.RuntimeHandleID, RuntimeLaunchID: rec.Metadata.RuntimeLaunchID,
		Branch: rec.Metadata.Branch, WorktreePath: rec.Metadata.WorkspacePath,
		Unavailable: "simulated: the confirmation could not be made durable",
	}
}

type p9Fixture struct {
	t       *testing.T
	ctx     context.Context
	store   *sqlite.Store
	clk     *fakeClock
	spawner *p9Spawner
	runtime *scriptedRuntimeOwnership
	ws      *fakeWorkspaceFacts
	coord   *workflowcore.Coordinator
	boots   int
	runID   string
	workID  string
}

func newP9Fixture(t *testing.T) *p9Fixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	f := &p9Fixture{t: t, ctx: ctx, store: store, clk: &fakeClock{t: time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)}}
	f.spawner = &p9Spawner{store: store, clk: f.clk, ids: map[int]domain.SessionID{}, crashBeforeRuntime: map[int]bool{}, crashAfterRuntime: map[int]bool{}}
	f.runtime = newScriptedRuntimeOwnership()
	f.ws = &fakeWorkspaceFacts{obs: workflowports.WorkspaceObservation{Path: "/wt/p9", Branch: "ao/p9", HeadSHA: "base"}}
	f.coord = f.newCoordinator(nil, nil)
	created, err := f.coord.CreateRun(ctx, "proj-1", "P9 worker recovery objective")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	f.workID = f.workStep().ID
	return f
}

// newCoordinator is one daemon boot over the shared store.
func (f *p9Fixture) newCoordinator(ownership workflowcore.SessionOwnership, locks workflowcore.BranchLocks) *workflowcore.Coordinator {
	f.boots++
	return workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store, Spawner: f.spawner, SessionFacts: f.store, WorkspaceFacts: f.ws,
		SessionOwnership: ownership, BranchLocks: locks, WorkerRuntimeOwnership: f.runtime,
		Clock: f.clk.Now, NewID: launchIDSeq(fmt.Sprintf("boot%d-", f.boots)),
	})
}

// sid is the session id the store assigned to spawn call n.
func (f *p9Fixture) sid(n int) domain.SessionID {
	f.t.Helper()
	f.spawner.mu.Lock()
	defer f.spawner.mu.Unlock()
	id, ok := f.spawner.ids[n]
	if !ok {
		f.t.Fatalf("spawn call %d created no session", n)
	}
	return id
}

// boot simulates a daemon restart: nothing in memory survives.
func (f *p9Fixture) boot() { f.coord = f.newCoordinator(nil, nil) }

// startCrashing runs StartRun on its own goroutine, so a Goexit inside the
// launch ends exactly that "daemon" and nothing else.
func (f *p9Fixture) startCrashing(c *workflowcore.Coordinator) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = c.StartRun(f.ctx, f.runID)
	}()
	wg.Wait()
}

func (f *p9Fixture) start() {
	f.t.Helper()
	if _, err := f.coord.StartRun(f.ctx, f.runID); err != nil {
		f.t.Fatalf("StartRun: %v", err)
	}
}

func (f *p9Fixture) reconcile(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		if err := f.coord.Reconcile(f.ctx); err != nil {
			f.t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
}

func (f *p9Fixture) run() domain.WorkflowRun {
	f.t.Helper()
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v ok=%v", err, ok)
	}
	return run
}

func (f *p9Fixture) workStep() domain.WorkflowStep {
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

func (f *p9Fixture) outbox() domain.WorkflowOutboxEntry {
	f.t.Helper()
	entries, err := f.store.ListWorkflowOutboxByRun(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowOutboxByRun: %v", err)
	}
	for _, e := range entries {
		if e.CommandType == domain.WorkflowOutboxSpawnWorkerSession {
			return e
		}
	}
	f.t.Fatal("no spawn_worker_session outbox entry")
	return domain.WorkflowOutboxEntry{}
}

func (f *p9Fixture) ledger() []domain.WorkflowCheckpoint {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	return cps
}

func (f *p9Fixture) count(phase string) int {
	n := 0
	for _, cp := range f.ledger() {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

func (f *p9Fixture) dispatchRecords(phase domain.WorkflowDispatchPhase) int {
	f.t.Helper()
	records, err := f.store.ListWorkflowDispatchCheckpointsByStep(f.ctx, f.workID)
	if err != nil {
		f.t.Fatalf("ListWorkflowDispatchCheckpointsByStep: %v", err)
	}
	n := 0
	for _, r := range records {
		if r.Phase == phase {
			n++
		}
	}
	return n
}

func (f *p9Fixture) sessionOnStep() string {
	if s := f.workStep().SessionID; s != nil {
		return *s
	}
	return ""
}

func (f *p9Fixture) assertSpawns(wantCalls, wantCreated int) {
	f.t.Helper()
	calls, created := f.spawner.counts()
	if calls != wantCalls || created != wantCreated {
		f.t.Fatalf("spawn calls/created = %d/%d, want %d/%d", calls, created, wantCalls, wantCreated)
	}
}

// assertSettled holds I18: once recovery has answered, more boots and passes
// write nothing and launch nothing.
func (f *p9Fixture) assertSettled(what string) {
	f.t.Helper()
	calls, created := f.spawner.counts()
	beforeLedger := f.ledger()
	before := len(beforeLedger)
	for i := 0; i < 3; i++ {
		f.boot()
		f.reconcile(2)
		_, _ = f.coord.GetRun(f.ctx, f.runID)
	}
	if afterLedger := f.ledger(); len(afterLedger) != before {
		f.t.Fatalf("%s: recovery is not idempotent — the ledger grew %d -> %d across further boots; added %v",
			what, before, len(afterLedger), phasesOf(afterLedger)[before:])
	}
	if c2, cr2 := f.spawner.counts(); c2 != calls || cr2 != created {
		f.t.Fatalf("%s: further boots launched again (%d/%d -> %d/%d)", what, calls, created, c2, cr2)
	}
}

func (f *p9Fixture) assertParkedOwnershipUnproven() {
	f.t.Helper()
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		f.t.Fatalf("run state = %q, want needs_attention; phases = %v", got, phasesOf(f.ledger()))
	}
	if f.count(workflowcore.ReasonWorkerOwnershipUnproven) == 0 {
		f.t.Fatalf("the stop does not name worker_ownership_unproven; phases = %v", phasesOf(f.ledger()))
	}
	if got := f.workStep().State; got == domain.WorkflowStepRunning {
		f.t.Fatal("the step is RUNNING over a worker whose ownership is unproven")
	}
	if got := f.outbox().Status; got != domain.WorkflowOutboxDispatched {
		f.t.Fatalf("outbox = %q, want still dispatched — nothing may launch over a runtime that may be running", got)
	}
}

func phasesOf(cps []domain.WorkflowCheckpoint) []string {
	out := make([]string, 0, len(cps))
	for _, cp := range cps {
		out = append(out, cp.DurablePhase)
	}
	return out
}

// ---- C1 ------------------------------------------------------------------------

// C1 — crash before any launch record. Nothing durable names a launch, so there
// is nothing to recover: the next start launches exactly one worker, and every
// later boot leaves it alone.
func TestP9Crash_C1_BeforeLaunchIntent(t *testing.T) {
	f := newP9Fixture(t)
	f.boot() // the daemon that created the run died before starting it
	f.start()
	f.assertSpawns(1, 1)
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
	f.assertSettled("C1")
	if f.sessionOnStep() != string(f.sid(1)) || f.workStep().State != domain.WorkflowStepRunning {
		t.Fatalf("step = %s/%q, want running over p9-sess-1", f.sessionOnStep(), f.workStep().State)
	}
}

// ---- C2 ------------------------------------------------------------------------

// C2 — crash after the intent, before any runtime existed. The claim and the
// intent are durable; no session exists under the dispatch key. Recovery proves
// that (natural key read, nothing found) and relaunches exactly once.
func TestP9Crash_C2_AfterIntentBeforeRuntime(t *testing.T) {
	f := newP9Fixture(t)
	f.spawner.crashBeforeRuntime[1] = true
	f.startCrashing(f.coord)
	f.assertSpawns(1, 0)
	if got := f.outbox().Status; got != domain.WorkflowOutboxDispatched {
		t.Fatalf("durable before: outbox = %q, want dispatched", got)
	}

	f.boot()
	f.reconcile(1) // inside the settle window: a launch may still be in flight
	f.assertSpawns(1, 0)

	f.clk.Advance(time.Minute)
	f.reconcile(1)
	f.clk.Advance(time.Minute)
	f.reconcile(1)
	f.assertSpawns(2, 1)
	f.runtime.set(f.sid(2), domain.WorkerRuntimeOwned)
	if f.sessionOnStep() != string(f.sid(2)) {
		t.Fatalf("durable after: step session = %q, want the single relaunch p9-sess-2", f.sessionOnStep())
	}
	f.assertSettled("C2")
}

// ---- C3 ------------------------------------------------------------------------

// C3 / I13 — crash after the runtime was created, before the launch was
// confirmed. The runtime proves it is this launch, so it is ADOPTED: no second
// worker, and RUNNING through the same confirmation a fresh launch takes.
func TestP9Crash_C3_AfterRuntimeBeforeConfirmationAdoptsAProvenRuntime(t *testing.T) {
	f := newP9Fixture(t)
	f.spawner.crashAfterRuntime[1] = true
	f.startCrashing(f.coord)
	f.assertSpawns(1, 1)
	if f.sessionOnStep() != "" {
		t.Fatal("durable before: the step already holds a session")
	}

	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
	f.boot()
	f.clk.Advance(time.Minute)
	f.reconcile(1)

	f.assertSpawns(1, 1)
	if f.sessionOnStep() != string(f.sid(1)) || f.workStep().State != domain.WorkflowStepRunning {
		t.Fatalf("durable after: step = %s/%q, want running over the adopted p9-sess-1; phases=%v",
			f.sessionOnStep(), f.workStep().State, phasesOf(f.ledger()))
	}
	if got := f.dispatchRecords(domain.DispatchPhaseWorkerDispatched); got != 1 {
		t.Fatalf("confirmations = %d, want exactly 1", got)
	}
	f.assertSettled("C3 adopt")
}

// C3 / I3 I5 I6 I15 I16 I17 — the same crash, but the runtime does NOT prove it
// is this launch. Nothing is adopted and nothing is launched over it: the step
// parks with ownership unproven, distinct from any failure.
func TestP9Crash_C3_UnprovenRuntimeFailsClosed(t *testing.T) {
	for _, proof := range []domain.WorkerRuntimeProof{
		domain.WorkerRuntimeInstanceMismatch, domain.WorkerRuntimeOwnerMismatch,
		domain.WorkerRuntimeInstallationMismatch, domain.WorkerRuntimeProvenanceMissing,
		domain.WorkerRuntimeUnsupported,
	} {
		t.Run(string(proof), func(t *testing.T) {
			f := newP9Fixture(t)
			f.spawner.crashAfterRuntime[1] = true
			f.startCrashing(f.coord)
			f.runtime.set(f.sid(1), proof)
			f.boot()
			f.clk.Advance(time.Minute)
			f.reconcile(1)

			f.assertSpawns(1, 1)
			if f.sessionOnStep() != "" {
				t.Fatalf("a runtime proving %s was bound to the step", proof)
			}
			f.assertParkedOwnershipUnproven()
			f.assertSettled("C3 " + string(proof))
		})
	}
}

// An unreadable runtime is waited out — one failed read must not park a live
// worker — and then, if it stays unreadable, fails closed. It is never adopted
// and never relaunched over.
func TestP9Crash_C3_UnreadableRuntimeIsWaitedOutThenFailsClosed(t *testing.T) {
	f := newP9Fixture(t)
	f.spawner.crashAfterRuntime[1] = true
	f.startCrashing(f.coord)
	f.boot()
	f.clk.Advance(time.Minute)
	before := len(f.ledger())
	f.reconcile(2)
	if f.run().State == domain.WorkflowRunNeedsAttention || f.sessionOnStep() != "" {
		t.Fatalf("a single unreadable probe parked or adopted: state=%q session=%q", f.run().State, f.sessionOnStep())
	}
	if after := len(f.ledger()); after != before {
		t.Fatalf("waiting out an unreadable runtime wrote %d rows", after-before)
	}
	f.clk.Advance(20 * time.Minute)
	f.reconcile(1)
	f.assertSpawns(1, 1)
	f.assertParkedOwnershipUnproven()
}

// The unreadable probe recovers inside the grace: the worker is adopted as if
// nothing had happened.
func TestP9Crash_C3_RuntimeReadableAgainInsideTheGraceIsAdopted(t *testing.T) {
	f := newP9Fixture(t)
	f.spawner.crashAfterRuntime[1] = true
	f.startCrashing(f.coord)
	f.boot()
	f.clk.Advance(time.Minute)
	f.reconcile(1)
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
	f.clk.Advance(time.Minute)
	f.reconcile(1)
	f.assertSpawns(1, 1)
	if f.sessionOnStep() != string(f.sid(1)) {
		t.Fatalf("step session = %q, want p9-sess-1 adopted once readable", f.sessionOnStep())
	}
}

// I14 — the launch AO recorded disagrees with the one the session now runs.
func TestP9Crash_C3_RecordedLaunchMismatchIsNotAdopted(t *testing.T) {
	f := newP9Fixture(t)
	f.startCrashing(f.newCoordinator(unconfirmedOwnership{f.store}, nil))
	f.assertSpawns(1, 1)
	rec, _, _ := f.store.GetSession(f.ctx, f.sid(1))
	rec.Metadata.RuntimeLaunchID = "p9-launch-relaunched-elsewhere"
	rec.Metadata.RuntimeOwnerToken = domain.SessionRuntimeOwnerToken(rec.ID, rec.Metadata.RuntimeLaunchID)
	if err := f.store.UpdateSession(f.ctx, rec); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned) // owned by ITS launch, not the recorded one
	f.boot()
	f.clk.Advance(time.Minute)
	f.reconcile(1)
	f.assertSpawns(1, 1)
	if f.sessionOnStep() != "" {
		t.Fatal("a session running a launch AO never recorded was adopted")
	}
	if f.run().State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention", f.run().State)
	}
}

// I15 — the worktree the session reports is not the one the launch recorded.
func TestP9Crash_C3_WorktreeMismatchIsNotAdopted(t *testing.T) {
	f := newP9Fixture(t)
	f.startCrashing(f.newCoordinator(unconfirmedOwnership{f.store}, nil))
	rec, _, _ := f.store.GetSession(f.ctx, f.sid(1))
	rec.Metadata.WorkspacePath = "/wt/somebody-elses-checkout"
	if err := f.store.UpdateSession(f.ctx, rec); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
	f.boot()
	f.clk.Advance(time.Minute)
	f.reconcile(1)
	f.assertSpawns(1, 1)
	if f.sessionOnStep() != "" {
		t.Fatal("a session in another worktree was adopted")
	}
	f.assertParkedOwnershipUnproven()
}

// I9 / I2 — a live, AO-owned session from an EARLIER generation sits under the
// dispatch key when the current generation's launch died before creating
// anything. It is AO's runtime, but not this claim's: never adopted.
func TestP9Crash_EarlierGenerationSessionIsNotAdoptedByTheCurrentClaim(t *testing.T) {
	f := newP9Fixture(t)
	// The earlier generation's orphan, created ten minutes before the claim now
	// in force, with complete provenance and a live runtime.
	orphanAt := f.clk.Now()
	orphan, err := f.store.CreateSession(f.ctx, domain.SessionRecord{
		ProjectID: "proj-1", IssueID: domain.IssueID("workflow-step:" + f.workID), Kind: domain.KindWorker,
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: orphanAt, LastSignalAt: orphanAt},
		CreatedAt: orphanAt, UpdatedAt: orphanAt,
	})
	if err != nil {
		t.Fatalf("seed earlier-generation session: %v", err)
	}
	id := orphan.ID
	orphan.Metadata = domain.SessionMetadata{Branch: "ao/p9", WorkspacePath: "/wt/p9", RuntimeHandleID: string(id),
		RuntimeInstanceID: "$0", RuntimeLaunchID: "gen1", RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(id, "gen1")}
	if err := f.store.UpdateSession(f.ctx, orphan); err != nil {
		t.Fatalf("stamp earlier-generation session: %v", err)
	}
	f.runtime.set(id, domain.WorkerRuntimeOwned)
	f.clk.Advance(10 * time.Minute)
	f.spawner.crashBeforeRuntime[1] = true
	f.startCrashing(f.coord)

	f.boot()
	f.clk.Advance(time.Minute)
	f.reconcile(1)
	if f.sessionOnStep() == string(id) {
		t.Fatal("I9: the current generation adopted a session an earlier generation created")
	}
	calls, _ := f.spawner.counts()
	if calls != 1 {
		t.Fatalf("spawn calls = %d, want 1 — nothing may launch beside a live AO runtime under this key", calls)
	}
	f.assertParkedOwnershipUnproven()
}

// ---- C4 ------------------------------------------------------------------------

// C4 — crash after the confirmation, the acknowledge and the session bind, before
// RUNNING. The durable launch is complete; recovery finishes it without a second
// worker.
func TestP9Crash_C4_AfterConfirmationBeforeRunning(t *testing.T) {
	f := newP9Fixture(t)
	f.startCrashing(f.newCoordinator(nil, crashOnRenewLocks{newFakeBranchLocks()}))
	f.assertSpawns(1, 1)
	if got := f.dispatchRecords(domain.DispatchPhaseWorkerDispatched); got != 1 {
		t.Fatalf("durable before: confirmations = %d, want 1", got)
	}
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
	f.boot()
	f.clk.Advance(time.Minute)
	f.reconcile(2)
	_, _ = f.coord.ContinueRun(f.ctx, f.runID)
	f.assertSpawns(1, 1)
	if f.sessionOnStep() != string(f.sid(1)) {
		t.Fatalf("durable after: step session = %q, want p9-sess-1", f.sessionOnStep())
	}
	if got := f.workStep().State; got != domain.WorkflowStepRunning {
		t.Fatalf("durable after: step = %q, want running over its confirmed, bound, live worker; phases=%v", got, phasesOf(f.ledger()))
	}
	f.assertSettled("C4")
}

// ---- C5 / C7 ------------------------------------------------------------------

// C5/C7 — restart with a confirmed worker running: protected, never relaunched,
// nothing written.
func TestP9Crash_C5_RestartOverALiveWorker(t *testing.T) {
	f := newP9Fixture(t)
	f.start()
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
	f.assertSettled("C5")
	f.assertSpawns(1, 1)
	if f.workStep().State != domain.WorkflowStepRunning {
		t.Fatalf("step = %q, want still running", f.workStep().State)
	}
}

// §17 — three hours of wall clock pass (a slept laptop) over a live, owned,
// confirmed worker. No stop, no relaunch, no ownership question.
func TestP9Crash_ClockJumpOverALiveWorkerConcludesNothing(t *testing.T) {
	f := newP9Fixture(t)
	f.start()
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
	f.clk.Advance(3 * time.Hour)
	f.boot()
	f.reconcile(2)
	_, _ = f.coord.ContinueRun(f.ctx, f.runID)
	_, _ = f.coord.GetRun(f.ctx, f.runID)
	f.assertSpawns(1, 1)
	if got := f.run().State; got != domain.WorkflowRunRunning {
		t.Fatalf("run = %q after a clock jump over a live worker; phases=%v", got, phasesOf(f.ledger()))
	}
	if f.count(workflowcore.ReasonWorkerOwnershipUnproven)+f.count(workflowcore.ReasonWorkerDispatchAmbiguous) != 0 {
		t.Fatal("a clock jump raised an ownership or dispatch ambiguity over a live worker")
	}
}

// ---- C6 ------------------------------------------------------------------------

// C6 — the worker finishes exactly as the daemon dies: its turn receipt is on the
// row and its workload has exited. That is an ending for work observation, never
// a phantom and never a relaunch.
func TestP9Crash_C6_WorkerFinishesAtTheCrash(t *testing.T) {
	f := newP9Fixture(t)
	f.start()
	f.clk.Advance(time.Minute)
	rec, _, _ := f.store.GetSession(f.ctx, f.sid(1))
	rec.TurnCompletedAt = f.clk.Now()
	rec.Activity.State = domain.ActivityIdle
	if err := f.store.UpdateSession(f.ctx, rec); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	f.ws.obs.Dirty = true
	f.ws.obs.Changes = []workflowports.WorkspaceChange{{Path: "main.go", Status: " M"}}
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwnedExited)
	f.boot()
	f.reconcile(2)
	f.assertSpawns(1, 1)
	if f.count(workflowcore.ReasonWorkerDispatchAmbiguous)+f.count(workflowcore.ReasonWorkerOwnershipUnproven) != 0 {
		t.Fatalf("a worker that finished at the crash was raised as ambiguous; phases=%v", phasesOf(f.ledger()))
	}
}

// ---- C8 ------------------------------------------------------------------------

// C8 — restart with a CONFIRMED, RUNNING worker whose runtime is provably gone
// behind a row that still reads live (a phantom). That worker may already have
// changed the worktree, so it is never silently relaunched over: the state is
// classified explicitly, with its evidence, and a person decides. Nothing is
// launched, and the classification is written once.
func TestP9Crash_C8_ConfirmedWorkerProvenGoneIsClassifiedNotRelaunched(t *testing.T) {
	f := newP9Fixture(t)
	f.start()
	f.runtime.set(f.sid(1), domain.WorkerRuntimeAbsent)
	f.boot()
	f.clk.Advance(time.Minute)
	f.reconcile(1)
	f.assertSpawns(1, 1)
	if f.run().State != domain.WorkflowRunNeedsAttention || f.count(workflowcore.ReasonWorkerDispatchAmbiguous) != 1 {
		t.Fatalf("a phantom running worker was not classified explicitly: run=%q phases=%v", f.run().State, phasesOf(f.ledger()))
	}
	if f.count(workflowcore.ReasonWorkerOwnershipUnproven) != 0 {
		t.Fatal("a proven-gone runtime was reported as unproven ownership; it is a proven fact")
	}
	f.assertSettled("C8")
}

// I7 + I9 — the launch died after creating its session and BEFORE confirming it,
// and its runtime is provably gone: that is a pre-work launch that never ran, so
// exactly one replacement is launched. Then the DEAD generation's session
// reports a finished turn late; it must not complete, advance or re-own the
// replacement's step.
func TestP9Crash_DeadUnconfirmedLaunchIsReplacedOnceAndItsLateCompletionIsIgnored(t *testing.T) {
	f := newP9Fixture(t)
	f.spawner.crashAfterRuntime[1] = true
	f.startCrashing(f.coord)
	f.runtime.set(f.sid(1), domain.WorkerRuntimeAbsent)
	f.boot()
	for i := 0; i < 4; i++ {
		f.clk.Advance(time.Minute)
		f.reconcile(1)
	}
	calls, created := f.spawner.counts()
	if calls != 2 || created != 2 {
		t.Fatalf("spawns = %d/%d, want exactly one replacement (2/2); phases=%v", calls, created, phasesOf(f.ledger()))
	}
	f.runtime.set(f.sid(2), domain.WorkerRuntimeOwned)
	if f.sessionOnStep() != string(f.sid(2)) || f.workStep().State != domain.WorkflowStepRunning {
		t.Fatalf("step = %s/%q, want running over the replacement %s", f.sessionOnStep(), f.workStep().State, f.sid(2))
	}

	// I9: generation 1 finishes late.
	old, _, _ := f.store.GetSession(f.ctx, f.sid(1))
	old.TurnCompletedAt = f.clk.Now()
	old.Activity.State = domain.ActivityExited
	old.IsTerminated = true
	if err := f.store.UpdateSession(f.ctx, old); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	f.ws.obs.Dirty = true
	f.reconcile(2)
	_, _ = f.coord.GetRun(f.ctx, f.runID)
	step := f.workStep()
	if step.SessionID == nil || *step.SessionID != string(f.sid(2)) {
		t.Fatalf("I9: the step's owner moved off the replacement: %v", step.SessionID)
	}
	if step.State != domain.WorkflowStepRunning {
		t.Fatalf("I9: generation 1's late completion moved generation 2's step to %q", step.State)
	}
}

// ---- C10 / I8 ------------------------------------------------------------------

// C10 / I8 — two recovery loops race over the same crashed launch. Exactly one
// owner results: one bound session, one RUNNING, one acknowledge, no new launch.
// Repeated to shake out interleavings; run under -race.
func TestP9Crash_C10_ConcurrentRecoveryProducesOneOwner(t *testing.T) {
	const rounds = 20
	for round := 0; round < rounds; round++ {
		f := newP9Fixture(t)
		f.spawner.crashAfterRuntime[1] = true
		f.startCrashing(f.coord)
		f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
		f.clk.Advance(time.Minute)

		a := f.newCoordinator(nil, nil)
		b := f.newCoordinator(nil, nil)
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, c := range []*workflowcore.Coordinator{a, b} {
			wg.Add(1)
			go func(c *workflowcore.Coordinator) {
				defer wg.Done()
				<-barrier
				errs <- c.Reconcile(f.ctx)
			}(c)
		}
		close(barrier)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: Reconcile: %v", round, err)
			}
		}

		f.assertSpawns(1, 1)
		if f.sessionOnStep() != string(f.sid(1)) {
			t.Fatalf("round %d: step session = %q, want the single adopted owner", round, f.sessionOnStep())
		}
		if got := f.workStep().State; got != domain.WorkflowStepRunning {
			t.Fatalf("round %d: step = %q, want running", round, got)
		}
		if got := f.outbox().Status; got != domain.WorkflowOutboxAcknowledged {
			t.Fatalf("round %d: outbox = %q, want acknowledged exactly once", round, got)
		}
		if got := f.dispatchRecords(domain.DispatchPhaseWorkerDispatched); got != 1 {
			t.Fatalf("round %d: confirmations = %d, want exactly 1 — two recovery loops both confirmed", round, got)
		}
		if got := f.count("worker_dispatched"); got != 1 {
			t.Fatalf("round %d: worker_dispatched markers = %d, want exactly 1", round, got)
		}
	}
}

// ---- I10 -----------------------------------------------------------------------

// I10 — a crashed launch under a run that was then cancelled is never recovered
// into it: no adoption, no RUNNING, no ledger write.
func TestP9Crash_TerminalRunIsNeverRecoveredIntoRunning(t *testing.T) {
	f := newP9Fixture(t)
	f.spawner.crashAfterRuntime[1] = true
	f.startCrashing(f.coord)
	f.runtime.set(f.sid(1), domain.WorkerRuntimeOwned)
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	before := len(f.ledger())
	f.boot()
	f.clk.Advance(time.Minute)
	f.reconcile(3)
	if f.sessionOnStep() != "" || f.workStep().State == domain.WorkflowStepRunning {
		t.Fatal("a cancelled run's crashed launch was adopted into RUNNING")
	}
	if got := f.run().State; got != domain.WorkflowRunCancelled {
		t.Fatalf("run = %q, want still cancelled", got)
	}
	if after := len(f.ledger()); after != before {
		t.Fatalf("reconciliation wrote %d rows to a cancelled run: %v", after-before, phasesOf(f.ledger())[before:])
	}
	f.assertSpawns(1, 1)
}

// Finding 4 of the independent review: after a long gap (a reboot, a slept
// laptop) the dispatch record is hours old. The unreadable-runtime grace runs
// from the FIRST failed read in this process, so one failed read at boot is
// never an immediate stop.
func TestP9Crash_HoursOldLaunchIsNotParkedOnItsFirstUnreadableRead(t *testing.T) {
	f := newP9Fixture(t)
	f.spawner.crashAfterRuntime[1] = true
	f.startCrashing(f.coord)
	f.clk.Advance(6 * time.Hour)
	f.boot()
	f.reconcile(2)
	if f.run().State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("an hours-old launch was parked on its first unreadable read; phases=%v", phasesOf(f.ledger()))
	}
	f.clk.Advance(20 * time.Minute)
	f.reconcile(1)
	f.assertParkedOwnershipUnproven()
	f.assertSpawns(1, 1)
}
