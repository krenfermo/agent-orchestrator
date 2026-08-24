package workflow_test

// Exactly one verification of a target may decide (verify_authority.go), and
// repairing the runs where that was not yet true (verify_race_reconcile.go).
//
// The bug these pin is not theoretical. Two executions of the SAME verify
// attempt against the SAME fingerprint ran 0.3s apart on wf-04e8309d: one
// failed on a flaky command and opened a fix cycle, the other passed and
// completed the run. Both acted, and the run ended terminal with a fix step
// running — a state no single execution can produce.
//
// The decisive guard is a durable compare-and-swap on the attempt's conclusion,
// so most of these tests are about what a LOSER must not do.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type raceFixture struct {
	t     *testing.T
	store *fakeStore
	clk   *fakeClock
	runID string
	now   time.Time
}

func newRaceFixture(t *testing.T) *raceFixture {
	t.Helper()
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	runID := "wf-race"
	fx := &raceFixture{t: t, store: newFakeStore(), clk: &fakeClock{t: now}, runID: runID, now: now}
	sid := "sess-race"
	fx.store.steps[runID] = []domain.WorkflowStep{
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, SessionID: &sid},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 2, State: domain.WorkflowStepCompleted},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 3, State: domain.WorkflowStepWaiting},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 4, State: domain.WorkflowStepRunning},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 5, State: domain.WorkflowStepPending},
	}
	fx.store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "project-1", Objective: "race objective",
		State: domain.WorkflowRunRunning, PolicyVersion: "v1",
		PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now, UpdatedAt: now,
	}
	// One in-flight verify attempt: the shape both executions race over.
	fx.store.attempts["verify"] = []domain.WorkflowAttempt{{
		ID: "wfa-verify-race", WorkflowStepID: "verify", AttemptNumber: 1,
		Harness: "local-verify", Model: "target-key-bfbbb150", StartedAt: now,
	}}
	return fx
}

func (fx *raceFixture) coord() *workflowcore.Coordinator {
	ids := 0
	return workflowcore.New(workflowcore.Deps{
		Store: fx.store, ReviewRuns: newFakeReviewRuns(), SessionFacts: newFakeSessionFacts(),
		MessageSender: &fakeMessageSender{}, Clock: fx.clk.Now,
		NewID: func() string { ids++; return "race-id" },
	})
}

func (fx *raceFixture) attempt() domain.WorkflowAttempt {
	fx.t.Helper()
	for _, a := range fx.store.attempts["verify"] {
		if a.ID == "wfa-verify-race" {
			return a
		}
	}
	fx.t.Fatal("the verify attempt vanished")
	return domain.WorkflowAttempt{}
}

// ---- 1. concurrent decisions: exactly one wins --------------------------

// The core property, exercised the way it actually fails: many goroutines all
// concluding the same attempt at once.
func TestConcurrentVerifyDecisionsElectExactlyOneWinner(t *testing.T) {
	fx := newRaceFixture(t)
	const racers = 100
	var wg sync.WaitGroup
	wins := make(chan bool, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outcome := domain.WorkflowAttemptSucceeded
			if i%2 == 0 {
				outcome = domain.WorkflowAttemptFailed
			}
			won, err := fx.store.ClaimWorkflowAttemptOutcome(
				context.Background(), "wfa-verify-race", fx.now, outcome, "")
			if err != nil {
				t.Error(err)
			}
			wins <- won
		}(i)
	}
	close(start)
	wg.Wait()
	close(wins)

	n := 0
	for w := range wins {
		if w {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d of %d concurrent executions won the decision, want exactly 1", n, racers)
	}
	if fx.attempt().FinishedAt == nil {
		t.Fatal("the attempt was never concluded")
	}
}

// A second decision on an already-concluded attempt always loses, whatever it
// found — this is the stale execution arriving late.
func TestFailingAndPassingDecisionsOnOneAttemptCannotBothWin(t *testing.T) {
	for _, tc := range []struct {
		name           string
		first, second  domain.WorkflowAttemptOutcome
		wantFinalState domain.WorkflowAttemptOutcome
	}{
		{"passing first, failing second", domain.WorkflowAttemptSucceeded, domain.WorkflowAttemptFailed, domain.WorkflowAttemptSucceeded},
		{"failing first, passing second", domain.WorkflowAttemptFailed, domain.WorkflowAttemptSucceeded, domain.WorkflowAttemptFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRaceFixture(t)
			ctx := context.Background()
			first, err := fx.store.ClaimWorkflowAttemptOutcome(ctx, "wfa-verify-race", fx.now, tc.first, "")
			if err != nil || !first {
				t.Fatalf("the first decision did not win: %v %v", first, err)
			}
			second, err := fx.store.ClaimWorkflowAttemptOutcome(ctx, "wfa-verify-race", fx.now.Add(time.Second), tc.second, "")
			if err != nil {
				t.Fatal(err)
			}
			if second {
				t.Fatal("a second, contradictory decision was allowed to win — this is the wf-04e8309d race")
			}
			// The winner's verdict is what stands, not the last writer's.
			if got := fx.attempt().Outcome; got != tc.wantFinalState {
				t.Fatalf("attempt outcome = %q, want %q: the loser overwrote the winner", got, tc.wantFinalState)
			}
		})
	}
}

// ---- 2. the loser produces no side effects ---------------------------------

// A losing FAILED execution must not open a fix cycle. This is the half that
// dispatched fix cycle 5 into an already-finished run.
func TestLosingFailedVerificationDispatchesNoFix(t *testing.T) {
	fx := newRaceFixture(t)
	ctx := context.Background()
	// The winner concluded first, passing.
	if won, err := fx.store.ClaimWorkflowAttemptOutcome(ctx, "wfa-verify-race", fx.now, domain.WorkflowAttemptSucceeded, ""); err != nil || !won {
		t.Fatalf("precondition: the winner should have won: %v %v", won, err)
	}
	sender := &fakeMessageSender{}
	coord := workflowcore.New(workflowcore.Deps{
		Store: fx.store, ReviewRuns: newFakeReviewRuns(), SessionFacts: newFakeSessionFacts(),
		MessageSender: sender, Clock: fx.clk.Now, NewID: func() string { return "race-id" },
	})

	// The loser now tries to record its failure and act on it.
	run := fx.store.runs[fx.runID]
	var verifyStep domain.WorkflowStep
	for _, s := range fx.store.steps[fx.runID] {
		if s.Kind == domain.WorkflowStepVerify {
			verifyStep = s
		}
	}
	_, _, err := coord.FinishVerifyFailureForTest(ctx, run, verifyStep, fx.attempt(),
		workflowcore.VerifyResult{Version: "v1", ErrorClass: domain.WorkflowErrorVerifyCommandFailed}, "go test failed")
	if err != nil {
		t.Fatalf("the loser should stand down quietly, not error: %v", err)
	}

	if sender.calls != 0 {
		t.Fatalf("the losing verification dispatched %d message(s) — a fix cycle against a target another execution already passed", sender.calls)
	}
	for _, s := range fx.store.steps[fx.runID] {
		if s.Kind == domain.WorkflowStepFix && s.State != domain.WorkflowStepWaiting {
			t.Fatalf("the loser moved the fix step to %s", s.State)
		}
	}
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunRunning {
		t.Fatalf("the loser changed the run state to %s", got)
	}
	if got := fx.attempt().Outcome; got != domain.WorkflowAttemptSucceeded {
		t.Fatalf("the loser overwrote the winning outcome with %q", got)
	}
	// It must still be on the record: two contradictory answers, one of which
	// counted, is exactly what a person needs to see.
	if n := len(checkpointsByPhase(fx.store, fx.runID, "verify_result_superseded")); n != 1 {
		t.Fatalf("superseded records = %d, want 1: the losing result must not be discarded", n)
	}
}

// ---- 3. a run never goes terminal over work in flight ----------------------

func TestRunDoesNotCompleteWhileAFixIsRunning(t *testing.T) {
	fx := newRaceFixture(t)
	steps := fx.store.steps[fx.runID]
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepFix {
			steps[i].State = domain.WorkflowStepRunning
		}
	}
	fx.store.steps[fx.runID] = steps

	run := fx.store.runs[fx.runID]
	var verifyStep domain.WorkflowStep
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepVerify {
			verifyStep = s
		}
	}
	if _, _, err := fx.coord().CompleteVerifiedRunForTest(context.Background(), run, verifyStep); err != nil {
		t.Fatalf("completing should decline quietly, not error: %v", err)
	}
	if got := fx.store.runs[fx.runID].State; got == domain.WorkflowRunCompleted {
		t.Fatal("the run completed while a fix worker was still running — it would claim finished work an agent is still editing, and strand the advance step")
	}
}

// ---- 4. the legacy incoherent state is repaired durably --------------------

func TestIncoherentTerminalRunIsReconciled(t *testing.T) {
	fx := newRaceFixture(t)
	// Exactly wf-04e8309d: terminal, fix running, advance never run, and a
	// verify attempt that concluded successfully — the decision the run's
	// completion actually reflects.
	steps := fx.store.steps[fx.runID]
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepFix:
			steps[i].State = domain.WorkflowStepRunning
		case domain.WorkflowStepVerify:
			steps[i].State = domain.WorkflowStepWaiting
		}
	}
	fx.store.steps[fx.runID] = steps
	run := fx.store.runs[fx.runID]
	run.State = domain.WorkflowRunCompleted
	fx.store.runs[fx.runID] = run
	if won, err := fx.store.ClaimWorkflowAttemptOutcome(context.Background(), "wfa-verify-race", fx.now, domain.WorkflowAttemptSucceeded, ""); err != nil || !won {
		t.Fatal("precondition")
	}
	// The fix the loser dispatched, still open.
	fx.store.attempts["fix"] = []domain.WorkflowAttempt{{
		ID: "wfa-fix-loser", WorkflowStepID: "fix", AttemptNumber: 5, StartedAt: fx.now,
	}}

	coord := fx.coord()
	if _, err := coord.ContinueRun(context.Background(), fx.runID); err != nil {
		t.Fatalf("ContinueRun on the incoherent run: %v", err)
	}

	for _, s := range fx.store.steps[fx.runID] {
		if s.Kind == domain.WorkflowStepFix && s.State == domain.WorkflowStepRunning {
			t.Fatal("the fix step the loser started is still running")
		}
	}
	var fixAttempt domain.WorkflowAttempt
	for _, a := range fx.store.attempts["fix"] {
		if a.ID == "wfa-fix-loser" {
			fixAttempt = a
		}
	}
	if fixAttempt.FinishedAt == nil {
		t.Fatal("the loser's fix attempt was left open")
	}
	// The run is NOT re-completed, NOT cancelled: its state stands.
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %s, want completed and untouched", got)
	}
	rows := checkpointsByPhase(fx.store, fx.runID, "verify_race_reconciled")
	if len(rows) != 1 {
		t.Fatalf("repair records = %d, want exactly 1", len(rows))
	}
}

// Repeated Continues repair once and then find nothing to do.
func TestReconcilingAnIncoherentRunIsIdempotent(t *testing.T) {
	fx := newRaceFixture(t)
	steps := fx.store.steps[fx.runID]
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepFix {
			steps[i].State = domain.WorkflowStepRunning
		}
		if steps[i].Kind == domain.WorkflowStepVerify {
			steps[i].State = domain.WorkflowStepWaiting
		}
	}
	fx.store.steps[fx.runID] = steps
	run := fx.store.runs[fx.runID]
	run.State = domain.WorkflowRunCompleted
	fx.store.runs[fx.runID] = run
	if won, _ := fx.store.ClaimWorkflowAttemptOutcome(context.Background(), "wfa-verify-race", fx.now, domain.WorkflowAttemptSucceeded, ""); !won {
		t.Fatal("precondition")
	}

	coord := fx.coord()
	for i := 0; i < 10; i++ {
		fx.clk.Advance(time.Second)
		_, _ = coord.ContinueRun(context.Background(), fx.runID)
	}
	if n := len(checkpointsByPhase(fx.store, fx.runID, "verify_race_reconciled")); n != 1 {
		t.Fatalf("repair records = %d after 10 Continues, want exactly 1", n)
	}
}

// A coherent terminal run is left completely alone, and still refuses Continue.
func TestCoherentTerminalRunIsUntouched(t *testing.T) {
	fx := newRaceFixture(t)
	steps := fx.store.steps[fx.runID]
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepVerify {
			steps[i].State = domain.WorkflowStepCompleted
		}
	}
	fx.store.steps[fx.runID] = steps
	run := fx.store.runs[fx.runID]
	run.State = domain.WorkflowRunCompleted
	fx.store.runs[fx.runID] = run

	if _, err := fx.coord().ContinueRun(context.Background(), fx.runID); err == nil {
		t.Fatal("Continue on a coherent terminal run should still refuse")
	}
	if n := len(checkpointsByPhase(fx.store, fx.runID, "verify_race_reconciled")); n != 0 {
		t.Fatalf("repair records = %d, want 0: nothing was incoherent", n)
	}
}

// ---- 5. different targets/generations still verify sequentially ------------

// The guard must not make a genuinely new verification impossible: a different
// target is a different attempt id and claims its own decision.
func TestDifferentTargetsEachGetTheirOwnDecision(t *testing.T) {
	fx := newRaceFixture(t)
	ctx := context.Background()
	fx.store.attempts["verify"] = append(fx.store.attempts["verify"], domain.WorkflowAttempt{
		ID: "wfa-verify-next", WorkflowStepID: "verify", AttemptNumber: 2,
		Harness: "local-verify", Model: "target-key-different", StartedAt: fx.now.Add(time.Minute),
	})
	if won, err := fx.store.ClaimWorkflowAttemptOutcome(ctx, "wfa-verify-race", fx.now, domain.WorkflowAttemptFailed, domain.WorkflowErrorVerifyCommandFailed); err != nil || !won {
		t.Fatal("the first target's decision should win")
	}
	won, err := fx.store.ClaimWorkflowAttemptOutcome(ctx, "wfa-verify-next", fx.now.Add(time.Minute), domain.WorkflowAttemptSucceeded, "")
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("a verification of a DIFFERENT target was refused its own decision — the guard must not freeze the run")
	}
}

// ---- 6. the in-process claim admits one executor --------------------------

func TestOnlyOneGoroutineClaimsAnAttemptForExecution(t *testing.T) {
	coord := newRaceFixture(t).coord()
	const racers = 50
	var wg sync.WaitGroup
	claims := make(chan bool, racers)
	start := make(chan struct{})
	releases := make(chan func(), racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, ok := coord.ClaimVerifyExecutionForTest("wfa-verify-race")
			claims <- ok
			releases <- release
		}()
	}
	close(start)
	wg.Wait()
	close(claims)
	close(releases)

	n := 0
	for c := range claims {
		if c {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d goroutines claimed the same attempt for execution, want 1", n)
	}
	// Once released, the attempt is claimable again — a crashed executor must
	// not lock the run out forever.
	for r := range releases {
		r()
	}
	if _, ok := coord.ClaimVerifyExecutionForTest("wfa-verify-race"); !ok {
		t.Fatal("the claim was not released")
	}
}

// ---- 7. restart between claim and result -----------------------------------

// A daemon that dies mid-verification leaves an attempt in flight. After the
// restart the attempt is still claimable and still decidable exactly once —
// the in-process claim must not survive as a phantom lock, and the durable CAS
// must still be open.
func TestRestartBetweenClaimAndResultLeavesTheAttemptDecidable(t *testing.T) {
	fx := newRaceFixture(t)
	before := fx.coord()
	if _, ok := before.ClaimVerifyExecutionForTest("wfa-verify-race"); !ok {
		t.Fatal("precondition: the first executor should claim it")
	}
	// The process dies here: no release, no outcome.
	after := fx.coord()
	if _, ok := after.ClaimVerifyExecutionForTest("wfa-verify-race"); !ok {
		t.Fatal("after a restart the attempt was not claimable — the run would never verify again")
	}
	won, err := fx.store.ClaimWorkflowAttemptOutcome(context.Background(), "wfa-verify-race", fx.now, domain.WorkflowAttemptSucceeded, "")
	if err != nil || !won {
		t.Fatalf("the resumed execution could not decide: %v %v", won, err)
	}
}

// ---- 8. the worktree path a request records is the authorized one ----------

// Not a repair of a defect — a guard on one. The integration fresh-review
// record must carry the run's authorized worktree verbatim, because every
// consumer of it (the reviewer's checkout, the verifier's working directory)
// treats it as the place the work lives.
func TestIntegrationRequestCarriesTheAuthorizedWorktreeVerbatim(t *testing.T) {
	const authorized = "/Users/someone/Projects/dev-orchestrator/agent-orchestrator"
	fx := newReconcileFixture(t)
	for i, cp := range fx.store.checkpoints[fx.runID] {
		if cp.ID == "work-cp" {
			fx.store.checkpoints[fx.runID][i].WorktreePath = authorized
		}
	}
	rec, ok := fx.coord.PendingIntegrationFreshReviewForTest(context.Background(), fx.runID, "review")
	if !ok {
		t.Fatal("precondition: the fixture's request should be outstanding")
	}
	if rec.WorktreePath != "" && rec.WorktreePath != authorized {
		t.Fatalf("the request carries worktree %q, which is not the authorized %q — a reviewer sent there would read the wrong tree",
			rec.WorktreePath, authorized)
	}
}
