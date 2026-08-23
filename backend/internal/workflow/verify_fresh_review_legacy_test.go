package workflow_test

// The historical half of the wf-6528a538 incident: the durable state a daemon
// that predates Checkpoint 8P-E.14D left behind, and which POST /continue then
// answered with a 200 and no work at all.
//
// The rows, exactly as reported from ~/.ao/data:
//
//	run            needs_attention   attentionReason = verify_unrepairable
//	review         completed         approved F1 = 5f8f9dc4f1b7…
//	verify         failed
//	attempt 1      verify_environment_error
//	attempt 2      verify_workspace_changed      (recovery generation 1)
//	workspace      F2 != F1
//
// The fixture below reaches A/B/C through the real code path and then seeds D/E
// as the OLD binary wrote them — no SupersededByFreshReview flag, because that
// flag did not exist yet, which is the whole point of the migration.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// legacyRecoveryGeneration seeds the durable rows a pre-8P-E.14D daemon wrote
// when its authorized recovery generation discovered a changed workspace, and
// returns the drifted fingerprint the reviewer will eventually be asked about.
//
// `generation` is written into every row so a test can also seed a generation
// that does NOT match the run's newest one, which is the negative case proof 4
// exists for.
func (fx *freshReviewFixture) legacyRecoveryGeneration(t *testing.T, generation int, originClass domain.WorkflowErrorClass) string {
	t.Helper()
	ctx := context.Background()

	// The environment failure the recovery was authorized for is already on disk,
	// written by the real verify path. Its TargetKey is the one the generation
	// must carry, so the test never has to guess at (or re-implement) the hash.
	historical := latestVerifyResult(t, fx.store, fx.runID)
	if historical.ErrorClass != domain.WorkflowErrorVerifyEnvironment {
		t.Fatalf("fixture's historical failure = %q, want verify_environment_error", historical.ErrorClass)
	}
	verifyStepID := "verify"

	recovery := workflowcore.VerifyRecoveryRecord{
		Generation:          generation,
		TargetKey:           historical.TargetKey,
		ReviewedFingerprint: historical.ReviewedFingerprint,
		StopReason:          workflowcore.ReasonVerifyUnrepairable,
		ErrorClass:          originClass,
	}
	for _, phase := range []string{"verify_recovery_requested", "verify_reopened"} {
		fx.clk.Advance(time.Second)
		fx.seedCheckpoint(t, phase, verifyStepID, mustJSON(t, recovery), "")
	}

	// The worker's uncommitted task changes survive AO's repair, so the worktree
	// is no longer what review approved.
	drifted := fx.driftWorkspace(t)

	// Generation 1's own attempt and its verdict, as the old binary recorded
	// them: a real attempt row, closed out failed, and a VerifyResult with no
	// superseded flag on it.
	fx.clk.Advance(time.Second)
	attempt, err := fx.store.CreateWorkflowAttempt(ctx, "wfa-legacy-recovery", verifyStepID,
		"local-verify", historical.TargetKey, fx.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, fx.clk.Now(),
		domain.WorkflowAttemptFailed, domain.WorkflowErrorVerifyWorkspaceChanged); err != nil {
		t.Fatal(err)
	}
	legacy := workflowcore.VerifyResult{
		Version:             historical.Version,
		TargetKey:           historical.TargetKey,
		ReviewedFingerprint: historical.ReviewedFingerprint,
		PreFingerprint:      drifted,
		Passed:              false,
		ErrorClass:          domain.WorkflowErrorVerifyWorkspaceChanged,
		RecoveryGeneration:  generation,
	}
	fx.clk.Advance(time.Second)
	fx.seedCheckpoint(t, "verify_result", verifyStepID, mustJSON(t, legacy), "verify_failed")

	// And the stop it parked on: the flat reason the old finishVerifyFailure
	// recorded, which is what the run still reads as today.
	fx.clk.Advance(time.Second)
	fx.seedCheckpoint(t, workflowcore.ReasonVerifyUnrepairable, verifyStepID, "{}",
		"verify failed (verify_workspace_changed) after 0 fix cycles: workspace fingerprint no longer matches the approved review target")
	return drifted
}

func (fx *freshReviewFixture) seedCheckpoint(t *testing.T, phase, stepID, retryState, nextAction string) {
	t.Helper()
	id := stepID
	if _, err := fx.store.CreateWorkflowCheckpoint(context.Background(), domain.WorkflowCheckpoint{
		ID:             "legacy-" + phase + "-" + fx.clk.Now().Format("150405.000000000"),
		WorkflowRunID:  fx.runID,
		WorkflowStepID: &id,
		ProjectID:      "project-1",
		RetryState:     retryState,
		NextAction:     nextAction,
		DurablePhase:   phase,
		PayloadVersion: "v1",
		CreatedAt:      fx.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// errPreflight is a reviewer that cannot be started at all — the shape a daemon
// dying between the durable decision and the dispatch leaves behind.
var errPreflight = errors.New("claude: not installed")

// ---- the historical incident ------------------------------------------------

// TestContinueRecoversAlreadyPersistedWorkspaceChange is the reported bug: the
// exact durable state of wf-6528a538 after its generation-1 recovery had already
// concluded verify_workspace_changed under the old binary.
func TestContinueRecoversAlreadyPersistedWorkspaceChange(t *testing.T) {
	fx := newFreshReviewFixture(t)
	ctx := context.Background()
	fx.correctAO(t)
	drifted := fx.legacyRecoveryGeneration(t, 1, domain.WorkflowErrorVerifyEnvironment)

	// The state the bug report describes, before anything is attempted.
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	if got := fx.stepState(t, domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
		t.Fatalf("verify step = %q, want failed", got)
	}
	if got := fx.stepState(t, domain.WorkflowStepReview); got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed", got)
	}
	historicalAttempts := append([]domain.WorkflowAttempt{}, fx.store.attempts["verify"]...)
	if len(historicalAttempts) != 2 {
		t.Fatalf("historical verify attempts = %d, want 2 (the environment failure and the recovery's workspace change)", len(historicalAttempts))
	}

	// G. The person presses Continue on the upgraded daemon.
	fx.continueRun(t)

	if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want exactly 1: Continue was still a no-op", got)
	}
	if fx.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", fx.launcher.launchCalls)
	}
	fresh := fx.freshReviewRun(t)
	if fresh.TargetSHA != drifted {
		t.Fatalf("the fresh review targets %q, want the CURRENT workspace %q", fresh.TargetSHA, drifted)
	}

	// The reviewer approves F2, and verification runs against exactly that.
	fx.submitFreshVerdict(t, domain.VerdictApproved, "approved: current workspace")
	fx.poll(t, 2)

	run, _, err := fx.store.GetWorkflowRun(ctx, fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", run.State)
	}
	if got := fx.stepState(t, domain.WorkflowStepVerify); got != domain.WorkflowStepCompleted {
		t.Fatalf("verify step = %q, want completed", got)
	}
	results := fx.verifyResults(t)
	final := results[len(results)-1]
	if !final.Passed || final.ReviewedFingerprint != drifted || final.RecoveryGeneration != 1 {
		t.Fatalf("final verify = passed:%v reviewed:%q generation:%d, want a pass against %q under generation 1",
			final.Passed, final.ReviewedFingerprint, final.RecoveryGeneration, drifted)
	}

	// F1 and every historical row are preserved, unedited.
	prior := fx.reviews.runs[fx.priorReviewID]
	if prior.TargetSHA != fx.approvedFingerprint || prior.Verdict != domain.VerdictApproved {
		t.Fatalf("the historical review run was mutated: %+v", prior)
	}
	if results[0].Passed || results[0].RecoveryGeneration != 0 ||
		results[0].ErrorClass != domain.WorkflowErrorVerifyEnvironment {
		t.Fatalf("the historical environment failure was rewritten: %+v", results[0])
	}
	if results[1].ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged ||
		results[1].RecoveryGeneration != 1 || results[1].SupersededByFreshReview {
		t.Fatalf("the legacy workspace-change result was rewritten: %+v", results[1])
	}
	attempts := fx.store.attempts["verify"]
	for i, want := range historicalAttempts {
		if attempts[i].ID != want.ID || attempts[i].Outcome != want.Outcome || attempts[i].ErrorClass != want.ErrorClass {
			t.Fatalf("historical attempt %d was mutated: %+v, want %+v", i, attempts[i], want)
		}
	}

	// Exactly one of everything the transition creates.
	if got := len(fx.reviews.runs); got != 2 {
		t.Fatalf("review runs = %d, want exactly 2 (F1's and the fresh one)", got)
	}
	if fx.reviews.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls = %d, want exactly 1", fx.reviews.insertCalls)
	}
	if got := len(fx.store.outbox); got != 1 {
		t.Fatalf("outbox entries = %d, want exactly 1", got)
	}
	if got := fx.phaseCount("verify_fresh_review_approved"); got != 1 {
		t.Fatalf("verify_fresh_review_approved checkpoints = %d, want exactly 1", got)
	}
	// No second recovery generation was consumed to get here.
	if got := fx.phaseCount("verify_recovery_requested"); got != 1 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want exactly 1", got)
	}

	// Nothing was re-run: no worker redispatch, no fix budget spent.
	if got := fx.stepState(t, domain.WorkflowStepWork); got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want still completed", got)
	}
	if len(fx.store.attempts["work"]) != 0 || len(fx.store.attempts["fix"]) != 0 {
		t.Fatalf("work/fix attempts were created: work=%d fix=%d",
			len(fx.store.attempts["work"]), len(fx.store.attempts["fix"]))
	}
	if fx.sender.calls != 0 {
		t.Fatalf("fix worker prompts sent = %d, want 0", fx.sender.calls)
	}
}

// Repeated Continue/Reconcile against the historical state: exactly one causes
// the transition, and the other forty-nine change nothing.
func TestRepeatedContinueOnHistoricalWorkspaceChangeIsIdempotent(t *testing.T) {
	fx := newFreshReviewFixture(t)
	fx.correctAO(t)
	fx.legacyRecoveryGeneration(t, 1, domain.WorkflowErrorVerifyEnvironment)

	// Fifty Continues while the fresh reviewer is still out.
	for i := 0; i < 50; i++ {
		fx.clk.Advance(time.Second)
		if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
			t.Fatalf("ContinueRun #%d: %v", i, err)
		}
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile #%d: %v", i, err)
		}
	}
	if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want exactly 1 across 50 Continues", got)
	}
	if fx.launcher.launchCalls != 1 || fx.reviews.insertCalls != 1 {
		t.Fatalf("reviewer launches = %d, review runs inserted = %d, want 1 and 1",
			fx.launcher.launchCalls, fx.reviews.insertCalls)
	}
	if got := len(fx.store.outbox); got != 1 {
		t.Fatalf("outbox entries = %d, want exactly 1", got)
	}
	if got := fx.phaseCount("verify_recovery_requested"); got != 1 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want exactly 1", got)
	}
	if len(fx.store.attempts["work"]) != 0 || len(fx.store.attempts["fix"]) != 0 {
		t.Fatal("a repeated Continue redispatched the worker or spent fix budget")
	}

	// And it still finishes, once, after all that.
	fx.submitFreshVerdict(t, domain.VerdictApproved, "approved")
	fx.poll(t, 2)
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", got)
	}
	verifying := 0
	for _, res := range fx.verifyResults(t) {
		if res.Passed {
			verifying++
		}
	}
	if verifying != 1 {
		t.Fatalf("passing verify results = %d, want exactly 1", verifying)
	}
}

// A restart at each intermediate phase converges on the same fresh review.
func TestHistoricalWorkspaceChangeRecoverySurvivesRestart(t *testing.T) {
	t.Run("between the durable request and the reopen", func(t *testing.T) {
		fx := newFreshReviewFixture(t)
		fx.correctAO(t)
		drifted := fx.legacyRecoveryGeneration(t, 1, domain.WorkflowErrorVerifyEnvironment)

		// The reviewer cannot start, so Continue gets as far as the durable
		// decision and the reopen, and no further.
		fx.launcher.preflightErr = errPreflight
		fx.continueRun(t)
		if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
			t.Fatalf("verify_fresh_review_required checkpoints = %d, want 1", got)
		}

		// Restart. The decision is re-read, never re-decided — proven by moving
		// the worktree AGAIN in a way that would fail the predicate outright: the
		// resume path must not consult it.
		obs := fx.ws.obs
		obs.HeadSHA = "moved-after-the-decision"
		fx.ws.obs = obs
		fx.launcher.preflightErr = nil
		fx.clk.Advance(time.Minute)
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		fx.continueRun(t)

		if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
			t.Fatalf("verify_fresh_review_required checkpoints = %d, want still exactly 1", got)
		}
		if fx.launcher.launchCalls != 1 {
			t.Fatalf("reviewer launches = %d, want exactly 1", fx.launcher.launchCalls)
		}
		// The reviewer is asked about the workspace the DECISION named, not the
		// one that appeared after it — the decision is durable, not re-derived.
		if fresh := fx.freshReviewRun(t); fresh.TargetSHA != drifted {
			t.Fatalf("the resumed fresh review targets %q, want the recorded %q", fresh.TargetSHA, drifted)
		}
	})

	t.Run("after approval, before verification", func(t *testing.T) {
		fx := newFreshReviewFixture(t)
		fx.correctAO(t)
		drifted := fx.legacyRecoveryGeneration(t, 1, domain.WorkflowErrorVerifyEnvironment)
		fx.continueRun(t)
		fx.submitFreshVerdict(t, domain.VerdictApproved, "approved")

		fx.runner.respond = func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
			return workflowcore.VerifyCommandExecution{}, context.Canceled
		}
		fx.clk.Advance(time.Minute)
		if _, err := fx.coord.GetRun(context.Background(), fx.runID); err == nil {
			t.Fatal("GetRun during the simulated shutdown returned no error")
		}

		fx.runner.respond = goModuleRunner(fx.root).respond
		fx.clk.Advance(time.Minute)
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile after restart: %v", err)
		}
		fx.poll(t, 2)

		if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunCompleted {
			t.Fatalf("run state = %q, want completed", got)
		}
		if got := fx.phaseCount("verify_fresh_review_approved"); got != 1 {
			t.Fatalf("verify_fresh_review_approved checkpoints = %d, want exactly 1", got)
		}
		final := fx.verifyResults(t)
		if last := final[len(final)-1]; !last.Passed || last.ReviewedFingerprint != drifted {
			t.Fatalf("final verify = passed:%v reviewed:%q, want a pass against %q", last.Passed, last.ReviewedFingerprint, drifted)
		}
	})
}

// ---- the protections the migration must not weaken --------------------------

// A verify_workspace_changed that did NOT come from an authorized recovery is
// still human-owned: Continue must not touch it, however many times it is
// pressed. This is the whole reason resumeStaleVerifyFailure's error-class guard
// was not simply widened.
func TestGenericWorkspaceChangeIsStillNotRecoverableByContinue(t *testing.T) {
	fx := newFreshReviewFixture(t)
	fx.correctAO(t)
	drifted := fx.driftWorkspace(t)

	// The same failing verdict, with no recovery lineage behind it at all: no
	// verify_recovery_requested row, and recoveryGeneration 0.
	historical := latestVerifyResult(t, fx.store, fx.runID)
	fx.clk.Advance(time.Second)
	fx.seedCheckpoint(t, "verify_result", "verify", mustJSON(t, workflowcore.VerifyResult{
		Version:             historical.Version,
		TargetKey:           historical.TargetKey,
		ReviewedFingerprint: historical.ReviewedFingerprint,
		PreFingerprint:      drifted,
		ErrorClass:          domain.WorkflowErrorVerifyWorkspaceChanged,
	}), "verify_failed")
	fx.clk.Advance(time.Second)
	fx.seedCheckpoint(t, workflowcore.ReasonVerifyUnrepairable, "verify", "{}",
		"verify failed (verify_workspace_changed) after 0 fix cycles")

	for i := 0; i < 10; i++ {
		fx.clk.Advance(time.Second)
		if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
			t.Fatalf("ContinueRun: %v", err)
		}
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if got := fx.phaseCount("verify_fresh_review_required"); got != 0 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want 0: a generic workspace change was auto-absorbed", got)
	}
	if got := fx.phaseCount("verify_recovery_requested"); got != 0 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want 0", got)
	}
	if fx.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
	}
	if got := fx.stepState(t, domain.WorkflowStepReview); got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want still completed", got)
	}
	if got := fx.stepState(t, domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
		t.Fatalf("verify step = %q, want still failed", got)
	}
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want still needs_attention", got)
	}
}

// The other three ways the durable proof can be missing. Each must leave the run
// exactly where it is — never a guess, and never a fresh review.
func TestHistoricalWorkspaceChangeRefusesIncompleteProof(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, fx *freshReviewFixture)
	}{
		{
			// Proof 4: the generation exists but did not originate from an
			// infrastructure failure, so it was never eligible for recovery.
			name: "the recovery generation came from an ineligible failure",
			setup: func(t *testing.T, fx *freshReviewFixture) {
				fx.legacyRecoveryGeneration(t, 1, domain.WorkflowErrorVerifyCommandFailed)
			},
		},
		{
			// Proof 4: the workspace change belongs to a generation that is not
			// the run's newest authorized one.
			name: "the workspace change belongs to a different generation",
			setup: func(t *testing.T, fx *freshReviewFixture) {
				fx.legacyRecoveryGeneration(t, 2, domain.WorkflowErrorVerifyEnvironment)
				// Re-point the run's newest generation at 2 while the result the
				// fixture wrote claims 2 as well, then add a NEWER generation 3
				// with no result of its own: the newest generation is now 3, and
				// the workspace change no longer belongs to it.
				fx.clk.Advance(time.Second)
				fx.seedCheckpoint(t, "verify_recovery_requested", "verify", mustJSON(t,
					workflowcore.VerifyRecoveryRecord{Generation: 3, ErrorClass: domain.WorkflowErrorVerifyEnvironment}), "")
				fx.clk.Advance(time.Second)
				fx.seedCheckpoint(t, "verify_reopened", "verify", mustJSON(t,
					workflowcore.VerifyRecoveryRecord{Generation: 3, ErrorClass: domain.WorkflowErrorVerifyEnvironment}), "")
			},
		},
		{
			// Proof 5/6: the commit history moved, so the drift is not this
			// task's own uncommitted work.
			name: "HEAD moved since the approval",
			setup: func(t *testing.T, fx *freshReviewFixture) {
				fx.legacyRecoveryGeneration(t, 1, domain.WorkflowErrorVerifyEnvironment)
				obs := fx.ws.obs
				obs.HeadSHA = "someone-elses-commit"
				fx.ws.obs = obs
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFreshReviewFixture(t)
			fx.correctAO(t)
			tc.setup(t, fx)

			for i := 0; i < 5; i++ {
				fx.clk.Advance(time.Second)
				if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
					t.Fatalf("ContinueRun: %v", err)
				}
			}
			if got := fx.phaseCount("verify_fresh_review_required"); got != 0 {
				t.Fatalf("verify_fresh_review_required checkpoints = %d, want 0", got)
			}
			if fx.launcher.launchCalls != 0 {
				t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
			}
			if got := fx.stepState(t, domain.WorkflowStepReview); got != domain.WorkflowStepCompleted {
				t.Fatalf("review step = %q, want still completed", got)
			}
			if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run state = %q, want still needs_attention", got)
			}
		})
	}
}
