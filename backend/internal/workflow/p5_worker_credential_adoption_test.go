package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p5_worker_credential_adoption_test.go -- the Spawn/bind window, from the
// coordinator's side.
//
// A worker credential is minted before the spawn, because the environment is
// fixed at spawn, and bound after it, because the session does not exist
// before it. A daemon that dies in between leaves a real worker in a real pane
// holding a token bound to nothing -- and every session route refuses an
// unbound credential, so that worker can never report on its own work.
//
// Recovery already adopted the SESSION, correctly and on natural-key evidence.
// What it did not do was adopt the IDENTITY, so the worker came back and stayed
// mute for the rest of the run. These tests pin the three answers an adoption
// can now give and what the coordinator does with each.

// fakeCredentialAdopter is workflowcore.WorkerCredentialAdopter.
type fakeCredentialAdopter struct {
	calls    []workflowcore.WorkerCredentialAdoptionRequest
	blocked  bool
	detail   string
	err      error
	adopted  string
	sawFence []string
}

func (a *fakeCredentialAdopter) AdoptWorkerCredential(
	_ context.Context, req workflowcore.WorkerCredentialAdoptionRequest,
) (workflowcore.WorkerCredentialAdoption, error) {
	a.calls = append(a.calls, req)
	a.sawFence = append(a.sawFence, req.AttemptID)
	if a.err != nil {
		return workflowcore.WorkerCredentialAdoption{}, a.err
	}
	if a.blocked {
		return workflowcore.WorkerCredentialAdoption{Blocked: true, Detail: a.detail}, nil
	}
	return workflowcore.WorkerCredentialAdoption{CredentialID: a.adopted}, nil
}

// adoptionCoordinator is the dispatch state machine's fixture plus an adopter,
// driven into the state a crash between Spawn and bind leaves behind: the
// launch happened, its confirmation did not, and the next pass adopts.
func adoptionCoordinator(
	t *testing.T, adopter workflowcore.WorkerCredentialAdopter,
) (*workflowcore.Coordinator, *fakeStore, *fakeClock, *fakeWorkerLauncher) {
	t.Helper()
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	facts := newFakeSessionFacts()
	launcher := &fakeWorkerLauncher{
		session: domain.SessionRecord{
			ID:       "sess-launched",
			Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
		},
		facts: facts,
	}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:                   store,
		WorkerLauncher:          launcher,
		SessionOwnership:        &fakeSessionOwnership{evidence: workflowcore.SessionOwnershipEvidence{Observed: true, RuntimeLaunchID: "gen-7"}},
		SessionFacts:            facts,
		WorkspaceFacts:          &fakeWorkspaceFacts{},
		WorkerCredentialAdopter: adopter,
		Clock:                   clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk, launcher
}

// crashBetweenSpawnAndConfirmation drives the fixture into the recovery state
// and returns the run and the work step being recovered.
func crashBetweenSpawnAndConfirmation(t *testing.T, c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock) (string, string) {
	t.Helper()
	ctx := context.Background()
	store.dispatchWriteErr = func(cp domain.WorkflowDispatchCheckpoint) error {
		if cp.Phase == domain.DispatchPhaseWorkerDispatched {
			return errors.New("the daemon died here")
		}
		return nil
	}
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stepID := workStepIDOf(t, store, created.Run.ID)
	if _, err := c.StartRun(ctx, created.Run.ID); err == nil {
		t.Fatal("StartRun must surface the confirmation failure that models the crash")
	}
	// The daemon comes back.
	store.dispatchWriteErr = nil
	clk.Advance(2 * time.Minute)
	return created.Run.ID, stepID
}

// The headline: adopting the session also adopts the identity, fenced to the
// attempt doing the adopting.
func TestAdoptingAWorkerAlsoReattachesItsCredential(t *testing.T) {
	adopter := &fakeCredentialAdopter{adopted: "cred-1"}
	c, store, clk, launcher := adoptionCoordinator(t, adopter)
	ctx := context.Background()
	runID, stepID := crashBetweenSpawnAndConfirmation(t, c, store, clk)

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(adopter.calls) == 0 {
		t.Fatal("the adopted worker's credential was never re-attached; that worker cannot report on its own work")
	}
	got := adopter.calls[0]
	if got.StepID != stepID || got.RunID != runID {
		t.Fatalf("adoption named step %s of run %s, want %s of %s", got.StepID, got.RunID, stepID, runID)
	}
	if got.SessionID != "sess-launched" {
		t.Fatalf("adoption named session %q, want the session the launch produced", got.SessionID)
	}
	// The fence matters as much as the binding: an adopted credential still
	// fenced to the crashed launch's attempt is refused by the next request.
	if got.AttemptID == "" {
		t.Fatal("adoption named no attempt to fence the credential to")
	}
	attempts, _ := store.ListWorkflowAttempts(ctx, stepID)
	if len(attempts) == 0 || attempts[len(attempts)-1].ID != got.AttemptID {
		t.Fatalf("adoption fenced to %q, want the step's newest attempt %+v", got.AttemptID, attempts)
	}

	// And the recovery still does what it always did.
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1: an adoption never launches a second worker", launcher.calls)
	}
	step := stepByID(t, store, runID, stepID)
	if step.State != domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want running", step.State)
	}
}

// A refusal is a stop, not a degraded success. The worker is running and
// provably cannot report, so the run parks under a named reason rather than
// being confirmed over an identity nobody decided.
func TestAnUnaccountableCredentialParksTheRunInsteadOfConfirming(t *testing.T) {
	adopter := &fakeCredentialAdopter{
		blocked: true,
		detail:  "work step has 2 worker credentials that were never bound to a session",
	}
	c, store, clk, launcher := adoptionCoordinator(t, adopter)
	ctx := context.Background()
	runID, stepID := crashBetweenSpawnAndConfirmation(t, c, store, clk)

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	step := stepByID(t, store, runID, stepID)
	if step.State == domain.WorkflowStepRunning {
		t.Fatal("the step was confirmed running over a credential AO cannot account for")
	}
	run, _, err := store.GetWorkflowRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", run.State)
	}
	// The stop has to be NAMED, or a person is told a run needs attention and
	// not what for.
	phases := ledgerPhases(t, store, runID)
	if !containsString(phases, workflowcore.ReasonWorkerCredentialUnadoptable) {
		t.Fatalf("ledger phases = %v, want one naming %s", phases, workflowcore.ReasonWorkerCredentialUnadoptable)
	}
	// And the launch is never repeated: the worker that exists is still the
	// only worker for this step.
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", launcher.calls)
	}
	if entry := store.outbox["workflow-step-spawn:"+stepID]; entry.Status != domain.WorkflowOutboxDispatched {
		t.Fatalf("outbox status = %q, want dispatched: the launch really happened", entry.Status)
	}
}

// A store failure while re-attaching is NOT a stop. The worker session is real
// and adopting it is right whatever happened to the credential bookkeeping; the
// sweep and the next recovery pass both re-derive that obligation from durable
// rows. Failing the adoption here would turn a bookkeeping hiccup into a parked
// run with a live worker in it.
func TestACredentialAdoptionFailureDoesNotStopTheRecovery(t *testing.T) {
	adopter := &fakeCredentialAdopter{err: errors.New("database is locked")}
	c, store, clk, _ := adoptionCoordinator(t, adopter)
	ctx := context.Background()
	runID, stepID := crashBetweenSpawnAndConfirmation(t, c, store, clk)

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	step := stepByID(t, store, runID, stepID)
	if step.State != domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want running: a credential read failure is not a reason to abandon a real worker", step.State)
	}
}

// A deployment with no adopter wired behaves exactly as it did before the
// interface existed -- which is also correct where a worker needs no identity
// of its own.
func TestRecoveryIsUnchangedWithNoAdopterWired(t *testing.T) {
	c, store, clk, launcher := adoptionCoordinator(t, nil)
	ctx := context.Background()
	runID, stepID := crashBetweenSpawnAndConfirmation(t, c, store, clk)

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	step := stepByID(t, store, runID, stepID)
	if step.State != domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want running", step.State)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", launcher.calls)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ---- the OTHER adoption entry point ----------------------------------------
//
// AO adopts a launch it cannot confirm through two doors, and both leave the
// same worker holding the same unbound credential:
//
//   - adoptLiveLaunch, on a runtime ownership/liveness proof. That is the door
//     the tests above drive, and it is the one a daemon that died with a live
//     worker actually reaches: dispatch reconciliation runs before dispatch is
//     re-entered, so it sees this shape first;
//   - adoptOrMarkAmbiguous -> recordDispatchSuccess, on a natural-key match,
//     reached when dispatch is re-entered for a step whose outbox is already
//     dispatched -- a human resume of a durably failed launch, most of all.
//
// Both call the same helper (Coordinator.adoptWorkerCredential) and both refuse
// the same way, so the rule is stated once. What is asserted here is the
// second door's REFUSAL shape, which has its own stop path
// (raiseUnadoptableWorkerCredential) rather than the reconciler's.

// The refusal concludes the attempt it opened. An attempt left with no outcome
// says work is in flight, and the one thing this stop is certain of is that no
// work is being accepted from this adoption.
func TestTheRefusalIsANamedStopWithNoAttemptLeftOpen(t *testing.T) {
	adopter := &fakeCredentialAdopter{blocked: true, detail: "two orphaned credentials for one step"}
	c, store, clk, _ := adoptionCoordinator(t, adopter)
	ctx := context.Background()
	runID, stepID := crashBetweenSpawnAndConfirmation(t, c, store, clk)

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	attempts, err := store.ListWorkflowAttempts(ctx, stepID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) == 0 {
		t.Fatal("no attempt was recorded for the adoption")
	}
	for _, a := range attempts {
		if a.Outcome == "" {
			t.Fatalf("attempt %s was left open by a stop; the step still reads as having work in flight", a.ID)
		}
	}
	run, _, err := store.GetWorkflowRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", run.State)
	}
}
