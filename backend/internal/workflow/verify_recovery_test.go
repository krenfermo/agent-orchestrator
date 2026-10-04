package workflow_test

// Checkpoint 8P-E.14C regression suite: recovering a run stopped on a STALE
// terminal verification failure, after the verifier or the environment that
// produced it has been corrected.
//
// The incident (wf-6528a538, 2026-08-22), exactly as it sat in ~/.ao/data:
//
//	workflow_run   state = needs_attention
//	steps          plan/work/review completed, fix waiting, verify FAILED
//	checkpoints    verify_result   passed=false errorClass=verify_environment_error
//	               verify_unrepairable "verify failed (verify_environment_error)
//	                                    after 0 fix cycles: stat …: no such file
//	                                    or directory"
//
// Both configured commands had passed. The only failing check was a file check,
// and it failed because the OLD verifier evaluated it in a different namespace
// from the one its own commands ran in. That defect was fixed and AO restarted —
// and the run did absolutely nothing, because a terminal verify step, a finished
// attempt row and needs_attention are each, separately, "this question has
// already been answered".
//
// The fixture below reproduces that durable triple through the real code path
// (no hand-written attempt rows, no seeded verify_result): the artifact the file
// check names is present but unreadable as a file, which is what
// verify_environment_error means, and correcting it is what "the environment was
// corrected" means. Nothing here is special-cased to that run id, that package,
// or that repository layout — the layout is only the fixture.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- fixture ---------------------------------------------------------------

type staleVerifyFixture struct {
	coord   *workflowcore.Coordinator
	store   *fakeStore
	clk     *fakeClock
	sender  *fakeMessageSender
	reviews *fakeReviewRuns
	ws      *mutableWorkspaceFacts
	runner  *scriptedVerifyRunner
	runID   string
	root    string
	// artifact is the absolute path of the file check's target: a directory
	// while the environment is broken, a real file once it is corrected.
	artifact string
	// reviewID lets a test change what the approved review targeted, so the
	// same-reviewed-target guard can be exercised directly.
	reviewID string
}

const staleVerifyArtifact = "internal/postrunqa/classify.go"

// newStaleVerifyFixture builds a run in the incident's exact shape and drives it
// through the real verify path until it is durably stopped on a recoverable
// verification environment failure.
func newStaleVerifyFixture(t *testing.T) *staleVerifyFixture {
	t.Helper()
	root := writeWorktree(t, map[string]string{
		"backend/go.mod":                   "module x\n",
		"backend/internal/postrunqa/qa.go": "package postrunqa\n",
	})
	// The artifact the plan names exists at the resolved path — but not as
	// something AO can read as a file. os.ReadFile answers EISDIR, which is
	// neither "missing" nor "wrong content": it is the environment failing to
	// deliver an answer, which is the whole definition of
	// verify_environment_error.
	artifact := filepath.Join(root, "backend", filepath.FromSlash(staleVerifyArtifact))
	if err := os.MkdirAll(artifact, 0o755); err != nil {
		t.Fatal(err)
	}

	fx := &staleVerifyFixture{root: root, artifact: artifact, reviewID: "review-incident"}
	fx.runner = goModuleRunner(root)
	plan := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"vet", "./..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
			{Command: "go", Args: []string{"test", "./internal/postrunqa/..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
		},
		Files: []workflowcore.VerificationFileCheck{{Path: staleVerifyArtifact, Exists: true}},
	}
	fx.coord, fx.store, fx.clk, fx.sender, fx.reviews, fx.ws, fx.runID = staleVerifyCoordinator(t, root, plan, fx.runner)

	if _, err := fx.coord.GetRun(context.Background(), fx.runID); err != nil {
		t.Fatalf("GetRun (reaching the incident state): %v", err)
	}
	fx.assertStoppedOnStaleVerify(t)
	return fx
}

// staleVerifyCoordinator is incidentFixture's wiring, widened to hand the test
// the review-run and workspace fakes so the same-target guards can be driven.
func staleVerifyCoordinator(t *testing.T, root string, plan workflowcore.VerificationPlan, runner workflowcore.VerifyRunner) (
	*workflowcore.Coordinator, *fakeStore, *fakeClock, *fakeMessageSender, *fakeReviewRuns, *mutableWorkspaceFacts, string,
) {
	t.Helper()
	store := newFakeStore()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	runID, sid, reviewID := "wf-stale-verify", "sess-stale", "review-incident"

	artifactJSON, err := workflowcore.MarshalPlanArtifact(
		workflowcore.BuildPlanArtifact("project-1", "verify objective", "v1", plan))
	if err != nil {
		t.Fatal(err)
	}
	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: artifactJSON},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &sid},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted, ReviewRunID: &reviewID},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepWaiting},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepPending},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending},
	}
	store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "project-1", Objective: "verify objective", State: domain.WorkflowRunWaiting,
		PolicyVersion: "v1", PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now, UpdatedAt: now,
	}
	approved := cleanObservation(root)
	workStepID := "work"
	store.checkpoints[runID] = []domain.WorkflowCheckpoint{{
		ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "project-1",
		SessionID: &sid, Branch: "feature", WorktreePath: root,
		FingerprintAfter: workflowcore.WorkspaceFingerprint(approved), CreatedAt: now,
	}}

	reviews := newFakeReviewRuns()
	reviews.runs[reviewID] = domain.ReviewRun{
		ID: reviewID, SessionID: domain.SessionID(sid), Harness: domain.ReviewerClaudeCode,
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		TargetSHA: workflowcore.WorkspaceFingerprint(approved),
	}
	facts := newFakeSessionFacts()
	facts.put(domain.SessionRecord{
		ID: domain.SessionID(sid), ProjectID: "project-1",
		Activity: domain.Activity{State: domain.ActivityActive},
		Metadata: domain.SessionMetadata{Branch: "feature", WorkspacePath: root},
	})
	ws := &mutableWorkspaceFacts{obs: approved}
	sender := &fakeMessageSender{}
	clk := &fakeClock{t: now}
	ids := 0
	c := workflowcore.New(workflowcore.Deps{
		Store: store, ReviewRuns: reviews, WorkspaceFacts: ws,
		SessionFacts: facts, Verifier: runner, MessageSender: sender,
		Clock: clk.Now, NewID: func() string { ids++; return fmt.Sprintf("sv%d", ids) },
	})
	return c, store, clk, sender, reviews, ws, runID
}

// fixEnvironment is the out-of-band correction: AO (or its host) is repaired, so
// the very same check that could not be answered now can be. The reviewed work
// itself is untouched — the workspace fingerprint the fixture reports is
// deliberately unchanged, because that is the whole premise of the recovery.
func (fx *staleVerifyFixture) fixEnvironment(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(fx.artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.artifact, []byte("package postrunqa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (fx *staleVerifyFixture) assertStoppedOnStaleVerify(t *testing.T) {
	t.Helper()
	run, _, err := fx.store.GetWorkflowRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: the fixture never reached the incident state", run.State)
	}
	if got := fx.stepState(t, domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
		t.Fatalf("verify step = %q, want failed: the fixture never reached the incident state", got)
	}
	result := latestVerifyResult(t, fx.store, fx.runID)
	if result.Passed || result.ErrorClass != domain.WorkflowErrorVerifyEnvironment {
		t.Fatalf("verify result = passed:%v class:%q, want a verify_environment_error failure", result.Passed, result.ErrorClass)
	}
	if got := fx.attentionReason(t); got != workflowcore.ReasonVerifyUnrepairable {
		t.Fatalf("attention reason = %q, want %q", got, workflowcore.ReasonVerifyUnrepairable)
	}
}

func (fx *staleVerifyFixture) stepState(t *testing.T, kind domain.WorkflowStepKind) domain.WorkflowStepState {
	t.Helper()
	for _, s := range fx.store.steps[fx.runID] {
		if s.Kind == kind {
			return s.State
		}
	}
	t.Fatalf("no %s step", kind)
	return ""
}

func (fx *staleVerifyFixture) attentionReason(t *testing.T) string {
	t.Helper()
	detail, err := fx.coord.GetRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	return life.AttentionReason
}

func (fx *staleVerifyFixture) phaseCount(phase string) int {
	return len(checkpointsByPhase(fx.store, fx.runID, phase))
}

func (fx *staleVerifyFixture) verifyAttempts() []domain.WorkflowAttempt {
	return fx.store.attempts["verify"]
}

// verifyResults returns every durable VerifyResult of the run, oldest first.
func (fx *staleVerifyFixture) verifyResults(t *testing.T) []workflowcore.VerifyResult {
	t.Helper()
	var out []workflowcore.VerifyResult
	for _, cp := range checkpointsByPhase(fx.store, fx.runID, "verify_result") {
		var res workflowcore.VerifyResult
		if err := json.Unmarshal([]byte(cp.RetryState), &res); err != nil {
			t.Fatalf("verify_result checkpoint is unreadable: %v", err)
		}
		out = append(out, res)
	}
	return out
}

func (fx *staleVerifyFixture) continueRun(t *testing.T) {
	t.Helper()
	fx.clk.Advance(time.Minute)
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
}

// ---- the incident ----------------------------------------------------------

// TestHumanContinueRecoversStaleVerifyEnvironmentFailure is the real incident,
// start to finish. Everything asserted here is something that did NOT happen
// before this checkpoint.
func TestHumanContinueRecoversStaleVerifyEnvironmentFailure(t *testing.T) {
	fx := newStaleVerifyFixture(t)
	ctx := context.Background()

	callsWhileBroken := len(fx.runner.calls)
	historicalResult := latestVerifyResult(t, fx.store, fx.runID)
	historicalAttempts := append([]domain.WorkflowAttempt{}, fx.verifyAttempts()...)
	if len(historicalAttempts) != 1 || historicalAttempts[0].Outcome != domain.WorkflowAttemptFailed {
		t.Fatalf("historical verify attempts = %+v, want exactly one failed attempt", historicalAttempts)
	}

	// The verifier/environment defect is corrected, and AO is asked to continue.
	fx.fixEnvironment(t)
	fx.continueRun(t)

	run, _, err := fx.store.GetWorkflowRun(ctx, fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed: Continue did not recover the stale verify failure", run.State)
	}
	if got := fx.stepState(t, domain.WorkflowStepVerify); got != domain.WorkflowStepCompleted {
		t.Fatalf("verify step = %q, want completed", got)
	}

	// The corrected verifier really ran, against the same reviewed target.
	if len(fx.runner.calls) <= callsWhileBroken {
		t.Fatalf("verifier calls = %d, want more than the %d from the failed attempt: verification never re-executed",
			len(fx.runner.calls), callsWhileBroken)
	}
	results := fx.verifyResults(t)
	if len(results) != 2 {
		t.Fatalf("verify results = %d, want exactly 2 (the historical failure and the recovery attempt)", len(results))
	}
	recovered := results[len(results)-1]
	if !recovered.Passed || recovered.RecoveryGeneration != 1 {
		t.Fatalf("recovery result = passed:%v generation:%d, want passed generation 1", recovered.Passed, recovered.RecoveryGeneration)
	}
	if recovered.ReviewedFingerprint != historicalResult.ReviewedFingerprint {
		t.Fatalf("recovery verified %q, want the SAME reviewed target %q",
			recovered.ReviewedFingerprint, historicalResult.ReviewedFingerprint)
	}

	// The old evidence is still on disk, unedited.
	if results[0].Passed || results[0].ErrorClass != domain.WorkflowErrorVerifyEnvironment || results[0].RecoveryGeneration != 0 {
		t.Fatalf("the historical verify result was rewritten: %+v", results[0])
	}
	attempts := fx.verifyAttempts()
	if len(attempts) != 2 {
		t.Fatalf("verify attempts = %d, want 2 (history plus the recovery attempt)", len(attempts))
	}
	if attempts[0].ID != historicalAttempts[0].ID || attempts[0].Outcome != domain.WorkflowAttemptFailed ||
		attempts[0].ErrorClass != domain.WorkflowErrorVerifyEnvironment {
		t.Fatalf("the historical attempt row was mutated: %+v", attempts[0])
	}
	if attempts[1].ID == attempts[0].ID {
		t.Fatal("the recovery reused the historical attempt id instead of creating a distinguishable one")
	}
	if attempts[1].Outcome != domain.WorkflowAttemptSucceeded {
		t.Fatalf("recovery attempt outcome = %q, want succeeded", attempts[1].Outcome)
	}

	// Durable evidence of the recovery itself, exactly once.
	if got := fx.phaseCount("verify_recovery_requested"); got != 1 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want exactly 1", got)
	}
	if got := fx.phaseCount("verify_reopened"); got != 1 {
		t.Fatalf("verify_reopened checkpoints = %d, want exactly 1", got)
	}

	// Nothing else was re-run to get here.
	if fx.sender.calls != 0 {
		t.Fatalf("fix worker prompts sent = %d, want 0: infrastructure recovery must never dispatch a fix", fx.sender.calls)
	}
	if got := fx.stepState(t, domain.WorkflowStepWork); got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want still completed: work was re-entered", got)
	}
	if len(fx.store.attempts["work"]) != 0 || len(fx.store.attempts["fix"]) != 0 {
		t.Fatalf("work/review/fix attempts were created: work=%d fix=%d",
			len(fx.store.attempts["work"]), len(fx.store.attempts["fix"]))
	}
	if got := fx.stepState(t, domain.WorkflowStepReview); got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want still completed: review was redispatched", got)
	}
	if got := fx.phaseCount(workflowcore.ReasonVerifyFixReentry); got != 0 {
		t.Fatalf("verify_fix_reentry checkpoints = %d, want 0", got)
	}
}

// A. Repeated Continue is idempotent: the second one finds a terminal run and
// changes nothing, and the first one never opened two generations however many
// times the cascade re-entered maybeVerify inside it.
func TestRepeatedContinueAfterVerifyRecoveryIsIdempotent(t *testing.T) {
	fx := newStaleVerifyFixture(t)
	fx.fixEnvironment(t)
	fx.continueRun(t)

	callsAfterRecovery := len(fx.runner.calls)
	for i := 0; i < 3; i++ {
		fx.clk.Advance(time.Minute)
		if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); !errors.Is(err, workflowcore.ErrAlreadyTerminal) {
			t.Fatalf("ContinueRun on a completed run = %v, want ErrAlreadyTerminal", err)
		}
	}
	if len(fx.runner.calls) != callsAfterRecovery {
		t.Fatalf("verifier calls = %d, want unchanged %d", len(fx.runner.calls), callsAfterRecovery)
	}
	if got := fx.phaseCount("verify_recovery_requested"); got != 1 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want exactly 1", got)
	}
	if got := len(fx.verifyAttempts()); got != 2 {
		t.Fatalf("verify attempts = %d, want exactly 2", got)
	}
}

// B. The daemon dies after the recovery was authorized but before it produced a
// result. The reopen must survive: boot reconcile finishes the SAME generation
// rather than authorizing a second one.
func TestVerifyRecoverySurvivesRestartBeforeExecution(t *testing.T) {
	fx := newStaleVerifyFixture(t)
	fx.fixEnvironment(t)

	// The verifier is torn down mid-execution — the shape a daemon shutdown
	// takes from inside maybeVerify.
	fx.runner.respond = func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		return workflowcore.VerifyCommandExecution{}, context.Canceled
	}
	fx.clk.Advance(time.Minute)
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); !errors.Is(err, context.Canceled) {
		t.Fatalf("ContinueRun during the simulated shutdown = %v, want context.Canceled", err)
	}
	if got := fx.phaseCount("verify_recovery_requested"); got != 1 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want 1 before the restart", got)
	}
	if got := fx.stepState(t, domain.WorkflowStepVerify); got.Terminal() {
		t.Fatalf("verify step = %q, want non-terminal: the reopen did not survive", got)
	}

	// Restart: the host is healthy again and boot reconcile runs.
	fx.runner.respond = goModuleRunner(fx.root).respond
	fx.clk.Advance(time.Minute)
	if err := fx.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}

	run, _, err := fx.store.GetWorkflowRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed after the restart finished the reopened verification", run.State)
	}
	if got := fx.phaseCount("verify_recovery_requested"); got != 1 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want still exactly 1: the restart opened a second generation", got)
	}
	if got := len(fx.verifyAttempts()); got != 2 {
		t.Fatalf("verify attempts = %d, want exactly 2: the restart duplicated the recovery attempt", got)
	}
	results := fx.verifyResults(t)
	if got := results[len(results)-1].RecoveryGeneration; got != 1 {
		t.Fatalf("recovery generation of the final result = %d, want 1", got)
	}
}

// C. A restart AFTER the recovery executed re-derives the same answer and
// re-runs nothing — in both directions: a recovery that succeeded, and one that
// failed on the infrastructure again.
func TestVerifyRecoverySurvivesRestartAfterExecution(t *testing.T) {
	t.Run("after a successful recovery", func(t *testing.T) {
		fx := newStaleVerifyFixture(t)
		fx.fixEnvironment(t)
		fx.continueRun(t)
		calls, attempts := len(fx.runner.calls), len(fx.verifyAttempts())

		for i := 0; i < 3; i++ {
			fx.clk.Advance(time.Minute)
			if err := fx.coord.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
		}
		if len(fx.runner.calls) != calls || len(fx.verifyAttempts()) != attempts {
			t.Fatalf("restart re-ran verification: calls %d->%d attempts %d->%d",
				calls, len(fx.runner.calls), attempts, len(fx.verifyAttempts()))
		}
	})

	t.Run("after a recovery that failed again", func(t *testing.T) {
		fx := newStaleVerifyFixture(t)
		// The person's correction did not work: the environment is still broken.
		fx.continueRun(t)
		fx.assertStoppedOnStaleVerify(t)
		calls, attempts := len(fx.runner.calls), len(fx.verifyAttempts())
		requests := fx.phaseCount("verify_recovery_requested")

		for i := 0; i < 3; i++ {
			fx.clk.Advance(time.Minute)
			if err := fx.coord.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if _, err := fx.coord.GetRun(context.Background(), fx.runID); err != nil {
				t.Fatal(err)
			}
		}
		if len(fx.runner.calls) != calls || len(fx.verifyAttempts()) != attempts {
			t.Fatalf("restart/poll re-ran a verification nobody authorized: calls %d->%d attempts %d->%d",
				calls, len(fx.runner.calls), attempts, len(fx.verifyAttempts()))
		}
		if got := fx.phaseCount("verify_recovery_requested"); got != requests {
			t.Fatalf("verify_recovery_requested checkpoints = %d, want unchanged %d: restart authorized a recovery", got, requests)
		}
	})
}

// D. The infrastructure keeps failing. Each Continue gets one bounded new
// generation, and after the bound the run stops with a reason that no longer
// says "continue it again".
func TestVerifyRecoveryIsBoundedWhenInfrastructureKeepsFailing(t *testing.T) {
	fx := newStaleVerifyFixture(t)

	for i := 1; i <= 4; i++ {
		fx.continueRun(t)
		run, _, err := fx.store.GetWorkflowRun(context.Background(), fx.runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != domain.WorkflowRunNeedsAttention {
			t.Fatalf("continue %d: run state = %q, want needs_attention", i, run.State)
		}
		if got := fx.stepState(t, domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
			t.Fatalf("continue %d: verify step = %q, want failed", i, got)
		}
	}
	if got := fx.phaseCount("verify_recovery_requested"); got != 3 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want exactly 3 (the bound), not one per Continue", got)
	}
	if got := fx.attentionReason(t); got != workflowcore.ReasonVerifyRecoveryExhausted {
		t.Fatalf("attention reason = %q, want %q once the bound is reached", got, workflowcore.ReasonVerifyRecoveryExhausted)
	}
	// The bound is recorded once, not once per further Continue.
	fx.continueRun(t)
	if got := fx.phaseCount(workflowcore.ReasonVerifyRecoveryExhausted); got != 1 {
		t.Fatalf("verify_recovery_exhausted checkpoints = %d, want exactly 1", got)
	}
	if fx.sender.calls != 0 {
		t.Fatalf("fix worker prompts sent = %d, want 0 across every bounded retry", fx.sender.calls)
	}
}

// E. A genuine verification failure caused by the code keeps its existing
// behaviour: the fix-cycle budget decides it, and Continue never reopens it
// through this path.
func TestGenuineVerifyFailureIsNeverReopenedByRecovery(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"backend/go.mod":  "module x\n",
		"backend/main.go": "package main\n",
	})
	// A command that runs perfectly well and says the code is wrong.
	runner := &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		return workflowcore.VerifyCommandExecution{ExitCode: 1, StdoutTail: "--- FAIL: TestThing\n"}, nil
	}}
	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{
		{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0, RetrySafe: true},
	}}
	c, store, clk, sender, _, _, runID := staleVerifyCoordinator(t, root, plan, runner)
	ctx := context.Background()

	// One fix cycle of budget, already spent: the next failure is terminal.
	run := store.runs[runID]
	run.PolicySnapshot = `{"maxFixCycles":1}`
	store.runs[runID] = run
	store.checkpoints[runID] = append(store.checkpoints[runID], domain.WorkflowCheckpoint{
		ID: "spent-budget", WorkflowRunID: runID, ProjectID: "project-1",
		DurablePhase: workflowcore.ReasonVerifyFixReentry, PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: clk.Now(),
	})

	if _, err := c.GetRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.AttentionReason != workflowcore.ReasonVerifyBudgetExhausted {
		t.Fatalf("attention reason = %q, want %q: the fixture is not testing a genuine code failure", life.AttentionReason, workflowcore.ReasonVerifyBudgetExhausted)
	}
	callsBefore := len(runner.calls)

	for i := 0; i < 3; i++ {
		clk.Advance(time.Minute)
		if _, err := c.ContinueRun(ctx, runID); err != nil {
			t.Fatalf("ContinueRun: %v", err)
		}
	}
	if got := len(checkpointsByPhase(store, runID, "verify_recovery_requested")); got != 0 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want 0: a code failure was reopened as infrastructure", got)
	}
	if len(runner.calls) != callsBefore {
		t.Fatalf("verifier calls = %d, want unchanged %d", len(runner.calls), callsBefore)
	}
	if store.runs[runID].State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", store.runs[runID].State)
	}
	_ = sender
}

// F. A verification that PASSED is never reopened, by any number of Continues.
func TestSuccessfulVerifyIsNeverReopened(t *testing.T) {
	fx := newStaleVerifyFixture(t)
	fx.fixEnvironment(t)
	fx.continueRun(t)
	if fx.store.runs[fx.runID].State != domain.WorkflowRunCompleted {
		t.Fatalf("fixture did not complete: %q", fx.store.runs[fx.runID].State)
	}
	calls := len(fx.runner.calls)

	for i := 0; i < 3; i++ {
		fx.clk.Advance(time.Minute)
		if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); !errors.Is(err, workflowcore.ErrAlreadyTerminal) {
			t.Fatalf("ContinueRun = %v, want ErrAlreadyTerminal", err)
		}
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if len(fx.runner.calls) != calls {
		t.Fatalf("a passed verification was re-executed: %d -> %d calls", calls, len(fx.runner.calls))
	}
	if got := fx.stepState(t, domain.WorkflowStepVerify); got != domain.WorkflowStepCompleted {
		t.Fatalf("verify step = %q, want still completed", got)
	}
}

// G. The reviewed target changed. The recovery must not silently re-use an
// approval that was given for different work — in either of the two ways the
// target can move.
func TestVerifyRecoveryRefusesAChangedTarget(t *testing.T) {
	t.Run("workspace moved under the approval", func(t *testing.T) {
		fx := newStaleVerifyFixture(t)
		fx.fixEnvironment(t)
		// The worktree is no longer what review approved.
		moved := fx.ws.obs
		moved.HeadSHA = "moved-since-review"
		fx.ws.obs = moved

		fx.continueRun(t)

		run, _, err := fx.store.GetWorkflowRun(context.Background(), fx.runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != domain.WorkflowRunNeedsAttention {
			t.Fatalf("run state = %q, want needs_attention: a moved target was verified against a stale approval", run.State)
		}
		results := fx.verifyResults(t)
		last := results[len(results)-1]
		if last.Passed || last.ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged {
			t.Fatalf("recovery result = passed:%v class:%q, want a verify_workspace_changed failure", last.Passed, last.ErrorClass)
		}
		if fx.sender.calls != 0 {
			t.Fatalf("fix worker prompts sent = %d, want 0", fx.sender.calls)
		}
	})

	t.Run("the approved review's target changed after authorization", func(t *testing.T) {
		fx := newStaleVerifyFixture(t)
		fx.fixEnvironment(t)

		// Authorize a recovery, then interrupt it before it produces a result.
		fx.runner.respond = func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
			return workflowcore.VerifyCommandExecution{}, context.Canceled
		}
		fx.clk.Advance(time.Minute)
		if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); !errors.Is(err, context.Canceled) {
			t.Fatalf("ContinueRun = %v, want context.Canceled", err)
		}

		// A different fingerprint is now the approved target.
		rr := fx.reviews.runs[fx.reviewID]
		rr.TargetSHA = "some-other-approved-target"
		fx.reviews.runs[fx.reviewID] = rr
		fx.runner.respond = goModuleRunner(fx.root).respond
		callsBefore := len(fx.runner.calls)

		fx.clk.Advance(time.Minute)
		if _, err := fx.coord.GetRun(context.Background(), fx.runID); err != nil {
			t.Fatal(err)
		}
		if len(fx.runner.calls) != callsBefore {
			t.Fatalf("verification executed against a target the recovery was not authorized for: %d -> %d calls",
				callsBefore, len(fx.runner.calls))
		}
		results := fx.verifyResults(t)
		last := results[len(results)-1]
		if last.Passed || last.ErrorClass != domain.WorkflowErrorVerifyAmbiguous {
			t.Fatalf("result = passed:%v class:%q, want a verify_ambiguous refusal", last.Passed, last.ErrorClass)
		}
		if fx.store.runs[fx.runID].State != domain.WorkflowRunNeedsAttention {
			t.Fatalf("run state = %q, want needs_attention", fx.store.runs[fx.runID].State)
		}
	})
}

// H. A run stopped for an unrelated reason is untouched, even with a failed
// verify step underneath it.
func TestUnrelatedAttentionReasonIsNeverReopenedByVerifyRecovery(t *testing.T) {
	fx := newStaleVerifyFixture(t)
	fx.fixEnvironment(t)

	// The run's current stop is now something only a person can resolve, and it
	// is not about verification at all.
	fx.clk.Advance(time.Minute)
	if _, err := fx.store.CreateWorkflowCheckpoint(context.Background(), domain.WorkflowCheckpoint{
		ID: "unrelated-stop", WorkflowRunID: fx.runID, ProjectID: "project-1",
		NextAction:   "the reviewer still requests changes after every allowed fix cycle",
		DurablePhase: workflowcore.ReasonFixBudgetExhausted, PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	callsBefore := len(fx.runner.calls)

	for i := 0; i < 3; i++ {
		fx.continueRun(t)
	}
	if got := fx.phaseCount("verify_recovery_requested"); got != 0 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want 0", got)
	}
	if len(fx.runner.calls) != callsBefore {
		t.Fatalf("verifier calls = %d, want unchanged %d", len(fx.runner.calls), callsBefore)
	}
	if fx.store.runs[fx.runID].State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", fx.store.runs[fx.runID].State)
	}
	if got := fx.attentionReason(t); got != workflowcore.ReasonFixBudgetExhausted {
		t.Fatalf("attention reason = %q, want the unrelated reason %q to still stand", got, workflowcore.ReasonFixBudgetExhausted)
	}
}

// I. One generation produces exactly one attempt and one result, however hard
// the run is polled while that generation is open.
func TestVerifyRecoveryGenerationHasExactlyOneAttempt(t *testing.T) {
	fx := newStaleVerifyFixture(t)
	fx.fixEnvironment(t)
	fx.continueRun(t)

	calls := len(fx.runner.calls)
	for i := 0; i < 5; i++ {
		fx.clk.Advance(time.Minute)
		if _, err := fx.coord.GetRun(context.Background(), fx.runID); err != nil {
			t.Fatal(err)
		}
	}
	if len(fx.runner.calls) != calls {
		t.Fatalf("polling re-executed the recovery attempt: %d -> %d calls", calls, len(fx.runner.calls))
	}
	byGeneration := map[int]int{}
	for _, res := range fx.verifyResults(t) {
		byGeneration[res.RecoveryGeneration]++
	}
	if byGeneration[1] != 1 {
		t.Fatalf("verify results for recovery generation 1 = %d, want exactly 1", byGeneration[1])
	}
	if got := len(fx.verifyAttempts()); got != 2 {
		t.Fatalf("verify attempts = %d, want exactly 2", got)
	}
}

// J. Reading a run can never recover it. This is the invariant that keeps the
// Board's 2s poll — which drives real dispatch elsewhere in this package — from
// silently retrying every terminal verification in the system.
func TestPollingAloneNeverReopensATerminalVerifyFailure(t *testing.T) {
	fx := newStaleVerifyFixture(t)
	fx.fixEnvironment(t)
	callsBefore := len(fx.runner.calls)

	for i := 0; i < 8; i++ {
		fx.clk.Advance(30 * time.Second)
		if _, err := fx.coord.GetRun(context.Background(), fx.runID); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.coord.ProjectBoard(context.Background(), "project-1", 24*time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := fx.phaseCount("verify_recovery_requested"); got != 0 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want 0: a read path recovered a terminal failure", got)
	}
	if len(fx.runner.calls) != callsBefore {
		t.Fatalf("verifier calls = %d, want unchanged %d", len(fx.runner.calls), callsBefore)
	}
	fx.assertStoppedOnStaleVerify(t)

	// And the same run, continued once, does recover — proving the fixture was
	// genuinely recoverable the whole time and only the trigger was missing.
	fx.continueRun(t)
	if fx.store.runs[fx.runID].State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed after the explicit Continue", fx.store.runs[fx.runID].State)
	}
}
