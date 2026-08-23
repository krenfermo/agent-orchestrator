package workflow_test

// Checkpoint 8P-E.14D regression suite: an authorized verification recovery
// whose approval no longer describes the workspace.
//
// The incident (wf-6528a538-4b67-4c1b-8cc2-941a3bc42ad9), continuing exactly
// where verify_recovery_test.go's fixture leaves off:
//
//	verify_recovery_requested   generation 1
//	verify_reopened             generation 1
//	verify_result               passed=false  verify_workspace_changed
//	                            reviewedFingerprint 5f8f9dc4f1b7…
//	                            preFingerprint      1b9f3f812e9b…
//
// AO refused to reuse the approval, correctly, and then had nowhere to go. The
// worker's uncommitted task changes were still sitting in the worktree, HEAD had
// not moved, and the only thing that had actually changed between the approval
// and the recovery was AO itself. Everything below asserts the transition that
// closes that dead end WITHOUT ever letting AO certify code no reviewer read.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- fixture ---------------------------------------------------------------

type freshReviewFixture struct {
	coord    *workflowcore.Coordinator
	store    *fakeStore
	clk      *fakeClock
	sender   *fakeMessageSender
	reviews  *fakeReviewRuns
	launcher *fakeReviewerLauncher
	ws       *mutableWorkspaceFacts
	runner   *scriptedVerifyRunner
	runID    string
	root     string
	artifact string
	// approvedFingerprint is F1: what review approved and the recovery was
	// authorized for.
	approvedFingerprint string
	// priorReviewID is the review_run that produced F1's approval.
	priorReviewID string
}

const (
	freshReviewHeadSHA  = "abc123"
	freshReviewWorkFile = "backend/internal/postrunqa/qa.go"
)

// newFreshReviewFixture reproduces the incident's durable state through the real
// code path: work completed, a real reviewer approved F1, and verification then
// failed on AO's own verifier with verify_environment_error, parking the run.
//
// It differs from newStaleVerifyFixture in exactly the two ways the drift
// recovery needs: a reviewer launcher is wired (so a fresh cycle has somebody to
// ask), and the work checkpoint records the HEAD the approval was given at (so
// AO can prove the commit history did not move). Both are what production
// always has; the stale-verify fixture simply never needed them.
func newFreshReviewFixture(t *testing.T) *freshReviewFixture {
	t.Helper()
	root := writeWorktree(t, map[string]string{
		"backend/go.mod":                   "module x\n",
		"backend/internal/postrunqa/qa.go": "package postrunqa\n",
	})
	artifact := filepath.Join(root, "backend", filepath.FromSlash(staleVerifyArtifact))
	if err := os.MkdirAll(artifact, 0o755); err != nil {
		t.Fatal(err)
	}

	fx := &freshReviewFixture{root: root, artifact: artifact, priorReviewID: "review-incident"}
	fx.runner = goModuleRunner(root)
	plan := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"vet", "./..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
		},
		Files: []workflowcore.VerificationFileCheck{{Path: staleVerifyArtifact, Exists: true}},
	}

	store := newFakeStore()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	runID, sid := "wf-fresh-review", "sess-fresh"

	artifactJSON, err := workflowcore.MarshalPlanArtifact(
		workflowcore.BuildPlanArtifact("project-1", "verify objective", "v1", plan))
	if err != nil {
		t.Fatal(err)
	}
	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: artifactJSON},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &sid},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted, ReviewRunID: &fx.priorReviewID},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepWaiting},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepPending},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending},
	}
	store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "project-1", Objective: "verify objective", State: domain.WorkflowRunWaiting,
		PolicyVersion: "v1", PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now, UpdatedAt: now,
	}

	approved := cleanObservation(root)
	fx.approvedFingerprint = workflowcore.WorkspaceFingerprint(approved)
	workStepID := "work"
	store.checkpoints[runID] = []domain.WorkflowCheckpoint{{
		ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "project-1",
		SessionID: &sid, Branch: "feature", WorktreePath: root, HeadSHA: freshReviewHeadSHA,
		FingerprintAfter: fx.approvedFingerprint, CreatedAt: now,
	}}

	fx.reviews = newFakeReviewRuns()
	fx.reviews.runs[fx.priorReviewID] = domain.ReviewRun{
		ID: fx.priorReviewID, SessionID: domain.SessionID(sid), Harness: domain.ReviewerClaudeCode,
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		TargetSHA: fx.approvedFingerprint, CreatedAt: now,
	}
	facts := newFakeSessionFacts()
	facts.put(domain.SessionRecord{
		ID: domain.SessionID(sid), ProjectID: "project-1",
		Activity: domain.Activity{State: domain.ActivityActive},
		Metadata: domain.SessionMetadata{Branch: "feature", WorkspacePath: root},
	})

	fx.store, fx.clk = store, &fakeClock{t: now}
	fx.ws = &mutableWorkspaceFacts{obs: approved}
	fx.sender = &fakeMessageSender{}
	fx.launcher = &fakeReviewerLauncher{}
	fx.runID = runID
	ids := 0
	fx.coord = workflowcore.New(workflowcore.Deps{
		Store: store, ReviewRuns: fx.reviews, WorkspaceFacts: fx.ws,
		SessionFacts: facts, Verifier: fx.runner, MessageSender: fx.sender,
		ReviewerLauncher: fx.launcher,
		Clock:            fx.clk.Now, NewID: func() string { ids++; return fmt.Sprintf("fr%d", ids) },
	})

	if _, err := fx.coord.GetRun(context.Background(), runID); err != nil {
		t.Fatalf("GetRun (reaching the incident state): %v", err)
	}
	if got := fx.store.runs[runID].State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: the fixture never reached the incident state", got)
	}
	if got := fx.stepState(t, domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
		t.Fatalf("verify step = %q, want failed", got)
	}
	return fx
}

// correctAO is the out-of-band repair: AO's verifier defect is fixed and the
// daemon restarts. The worker's uncommitted task changes are deliberately
// preserved across it — which is the whole reason the workspace fingerprint has
// moved while HEAD has not.
func (fx *freshReviewFixture) correctAO(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(fx.artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.artifact, []byte("package postrunqa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// driftWorkspace moves the worktree the way the incident's did: one more
// uncommitted edit by the same task, on the same branch, at the same commit.
func (fx *freshReviewFixture) driftWorkspace(t *testing.T) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fx.root, filepath.FromSlash(freshReviewWorkFile)),
		[]byte("package postrunqa\n\n// more of the same task's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	obs := fx.ws.obs
	obs.Dirty = true
	obs.Changes = []ports.WorkspaceChange{{Path: freshReviewWorkFile, Status: " M"}}
	fx.ws.obs = obs
	drifted := workflowcore.WorkspaceFingerprint(obs)
	if drifted == fx.approvedFingerprint {
		t.Fatal("the fixture's drift did not move the workspace fingerprint")
	}
	return drifted
}

func (fx *freshReviewFixture) stepState(t *testing.T, kind domain.WorkflowStepKind) domain.WorkflowStepState {
	t.Helper()
	for _, s := range fx.store.steps[fx.runID] {
		if s.Kind == kind {
			return s.State
		}
	}
	t.Fatalf("no %s step", kind)
	return ""
}

func (fx *freshReviewFixture) reviewStep(t *testing.T) domain.WorkflowStep {
	t.Helper()
	for _, s := range fx.store.steps[fx.runID] {
		if s.Kind == domain.WorkflowStepReview {
			return s
		}
	}
	t.Fatal("no review step")
	return domain.WorkflowStep{}
}

func (fx *freshReviewFixture) phaseCount(phase string) int {
	return len(checkpointsByPhase(fx.store, fx.runID, phase))
}

func (fx *freshReviewFixture) verifyResults(t *testing.T) []workflowcore.VerifyResult {
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

// freshReviewRun returns the review_run created for the fresh cycle — the one
// that is not the stale approval. Runs closed out as "failed" are skipped: those
// are review_launch_recovery.go's diagnostics for a reviewer that never started,
// which migration 0014 deliberately excludes from the dedupe index for exactly
// this reason. Fails when there is not exactly one live fresh run.
func (fx *freshReviewFixture) freshReviewRun(t *testing.T) domain.ReviewRun {
	t.Helper()
	var found []domain.ReviewRun
	for id, r := range fx.reviews.runs {
		if id != fx.priorReviewID && r.Status != domain.ReviewRunFailed {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("fresh review runs = %d, want exactly 1: %+v", len(found), found)
	}
	return found[0]
}

// approveFreshReview is the reviewer's own out-of-band `ao review submit`, the
// same way every other review test lands a verdict.
func (fx *freshReviewFixture) submitFreshVerdict(t *testing.T, verdict domain.ReviewVerdict, body string) {
	t.Helper()
	rr := fx.freshReviewRun(t)
	rr.Status, rr.Verdict, rr.Body = domain.ReviewRunComplete, verdict, body
	fx.reviews.runs[rr.ID] = rr
}

func (fx *freshReviewFixture) continueRun(t *testing.T) {
	t.Helper()
	fx.clk.Advance(time.Minute)
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
}

func (fx *freshReviewFixture) poll(t *testing.T, times int) {
	t.Helper()
	for i := 0; i < times; i++ {
		fx.clk.Advance(10 * time.Second)
		if _, err := fx.coord.GetRun(context.Background(), fx.runID); err != nil {
			t.Fatalf("GetRun: %v", err)
		}
	}
}

// ---- the incident ----------------------------------------------------------

// TestVerifyRecoveryObtainsFreshReviewOfTheCurrentWorkspace is the real
// incident, end to end: A..K of the reported sequence.
func TestVerifyRecoveryObtainsFreshReviewOfTheCurrentWorkspace(t *testing.T) {
	fx := newFreshReviewFixture(t)
	ctx := context.Background()

	// C/D: the verifier defect is corrected, the worker's uncommitted changes are
	// preserved across the restart, and a person presses Continue.
	fx.correctAO(t)
	drifted := fx.driftWorkspace(t)

	fx.continueRun(t)

	// G: AO scheduled a fresh review rather than parking permanently.
	if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want exactly 1", got)
	}
	if got := fx.store.runs[fx.runID].State; got == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run parked in needs_attention instead of re-reviewing the current workspace")
	}
	if fx.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", fx.launcher.launchCalls)
	}
	fresh := fx.freshReviewRun(t)
	if fresh.TargetSHA != drifted {
		t.Fatalf("the fresh review targets %q, want the CURRENT workspace %q", fresh.TargetSHA, drifted)
	}

	// H/I/J/K: the reviewer approves the current workspace, and verification runs
	// against that exact fingerprint and passes.
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
	if !final.Passed || final.ReviewedFingerprint != drifted {
		t.Fatalf("final verify = passed:%v reviewed:%q, want a pass against the freshly reviewed %q",
			final.Passed, final.ReviewedFingerprint, drifted)
	}
	if final.RecoveryGeneration != 1 {
		t.Fatalf("final verify recovery generation = %d, want 1", final.RecoveryGeneration)
	}

	// Exactly one recovery verification consumed the fresh approval.
	usingFresh := 0
	for _, r := range results {
		if r.ReviewedFingerprint == drifted && !r.SupersededByFreshReview {
			usingFresh++
		}
	}
	if usingFresh != 1 {
		t.Fatalf("verify results against the fresh target = %d, want exactly 1", usingFresh)
	}

	// The historical approval of F1 is preserved, unedited, and the fresh one is
	// a separate row.
	prior := fx.reviews.runs[fx.priorReviewID]
	if prior.TargetSHA != fx.approvedFingerprint || prior.Verdict != domain.VerdictApproved {
		t.Fatalf("the historical review run was mutated: %+v", prior)
	}
	if fresh.ID == fx.priorReviewID {
		t.Fatal("the fresh review reused the historical review run instead of creating its own")
	}
	if results[0].Passed || results[0].RecoveryGeneration != 0 {
		t.Fatalf("the historical verify result was rewritten: %+v", results[0])
	}

	// The worker was never redispatched, and no fix cycle was consumed.
	if got := fx.stepState(t, domain.WorkflowStepWork); got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want still completed: work was re-entered", got)
	}
	if len(fx.store.attempts["work"]) != 0 || len(fx.store.attempts["fix"]) != 0 {
		t.Fatalf("work/fix attempts were created: work=%d fix=%d",
			len(fx.store.attempts["work"]), len(fx.store.attempts["fix"]))
	}
	if fx.sender.calls != 0 {
		t.Fatalf("fix worker prompts sent = %d, want 0", fx.sender.calls)
	}
	if got := fx.phaseCount(workflowcore.ReasonVerifyFixReentry); got != 0 {
		t.Fatalf("verify_fix_reentry checkpoints = %d, want 0", got)
	}

	// One durable lifecycle, one of each row.
	if got := fx.phaseCount("verify_recovery_requested"); got != 1 {
		t.Fatalf("verify_recovery_requested checkpoints = %d, want 1", got)
	}
	if got := fx.phaseCount("verify_fresh_review_approved"); got != 1 {
		t.Fatalf("verify_fresh_review_approved checkpoints = %d, want 1", got)
	}
	if fx.reviews.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls = %d, want exactly 1", fx.reviews.insertCalls)
	}
	if got := len(fx.store.outbox); got != 1 {
		t.Fatalf("outbox entries = %d, want exactly 1 (the fresh review's own)", got)
	}
}

// 1. The fresh review requests changes. The EXISTING fix/review/verify loop
// takes it from there — no second mechanism, and the fresh-review branch does
// not hijack the fix-driven cycle that follows.
func TestFreshReviewChangesRequestedFallsIntoTheExistingFixLoop(t *testing.T) {
	fx := newFreshReviewFixture(t)
	fx.correctAO(t)
	fx.driftWorkspace(t)
	fx.continueRun(t)

	fx.submitFreshVerdict(t, domain.VerdictChangesRequested, "please rename the helper")
	fx.poll(t, 2)

	if fx.sender.calls != 1 {
		t.Fatalf("fix worker prompts sent = %d, want exactly 1: the ordinary fix cycle did not take over", fx.sender.calls)
	}
	if got := len(fx.store.attempts["fix"]); got != 1 {
		t.Fatalf("fix attempts = %d, want exactly 1", got)
	}
	if got := len(checkpointsByPhase(fx.store, fx.runID, "fix_dispatched")); got != 1 {
		t.Fatalf("fix_dispatched checkpoints = %d, want exactly 1: the ordinary fix cycle never dispatched", got)
	}
	// Still exactly one fresh review: the changes_requested verdict is answered
	// by the fix loop, never by a second re-review authorization.
	if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want 1", got)
	}
}

// 2. A transient reviewer-launch failure during the fresh review is handled by
// the existing reviewer-launch recovery, not by a new retry path — and it never
// authorizes a second fresh review.
func TestFreshReviewLaunchFailureUsesExistingLaunchRecovery(t *testing.T) {
	fx := newFreshReviewFixture(t)
	fx.correctAO(t)
	fx.driftWorkspace(t)
	fx.launcher.launchErr = fmt.Errorf("spawn: temporary failure")

	fx.continueRun(t)

	if fx.launcher.launchCalls == 0 {
		t.Fatal("the fresh reviewer was never launched")
	}
	if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want exactly 1", got)
	}
	launchesAfterFailure := fx.launcher.launchCalls

	// The launch recovery owns the retry from here. Polling must not stack up
	// launches, and must not open a second re-review authorization.
	fx.poll(t, 5)
	if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want still 1", got)
	}
	if fx.launcher.launchCalls > launchesAfterFailure+1 {
		t.Fatalf("reviewer launches = %d, want at most one more than the %d already attempted",
			fx.launcher.launchCalls, launchesAfterFailure)
	}

	// And once the cause is corrected, the human-driven Continue resumes the same
	// fresh review rather than starting another one.
	fx.launcher.launchErr = nil
	fx.continueRun(t)
	if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want still exactly 1", got)
	}
}

// 3/4/5. A daemon restart at each of the three durable phases resumes the same
// fresh review rather than re-deciding, re-dispatching or re-approving it.
func TestFreshReviewSurvivesRestartAtEveryPhase(t *testing.T) {
	t.Run("before the fresh review is dispatched", func(t *testing.T) {
		fx := newFreshReviewFixture(t)
		fx.correctAO(t)
		drifted := fx.driftWorkspace(t)

		// The reviewer cannot be launched at all, so Continue gets as far as the
		// durable request and no further — the shape a crash between the two takes.
		fx.launcher.preflightErr = fmt.Errorf("claude: not installed")
		fx.continueRun(t)
		if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
			t.Fatalf("verify_fresh_review_required checkpoints = %d, want 1", got)
		}

		// Restart: boot reconcile plus the ordinary poll finish the same request.
		fx.launcher.preflightErr = nil
		fx.clk.Advance(time.Minute)
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		fx.poll(t, 2)
		if got := fx.phaseCount("verify_fresh_review_required"); got != 1 {
			t.Fatalf("verify_fresh_review_required checkpoints = %d, want still 1 after the restart", got)
		}
		fresh := fx.freshReviewRun(t)
		if fresh.TargetSHA != drifted {
			t.Fatalf("the resumed fresh review targets %q, want %q", fresh.TargetSHA, drifted)
		}
	})

	t.Run("while the fresh reviewer is running", func(t *testing.T) {
		fx := newFreshReviewFixture(t)
		fx.correctAO(t)
		fx.driftWorkspace(t)
		fx.continueRun(t)

		before := fx.freshReviewRun(t)
		for i := 0; i < 3; i++ {
			fx.clk.Advance(time.Minute)
			if err := fx.coord.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			fx.poll(t, 1)
		}
		if got := fx.freshReviewRun(t); got.ID != before.ID {
			t.Fatalf("a restart created a second reviewer session: %q -> %q", before.ID, got.ID)
		}
		if fx.launcher.launchCalls != 1 {
			t.Fatalf("reviewer launches = %d, want exactly 1 across every restart", fx.launcher.launchCalls)
		}
		if fx.reviews.insertCalls != 1 {
			t.Fatalf("InsertReviewRun calls = %d, want exactly 1", fx.reviews.insertCalls)
		}
	})

	t.Run("after approval, before verification", func(t *testing.T) {
		fx := newFreshReviewFixture(t)
		fx.correctAO(t)
		drifted := fx.driftWorkspace(t)
		fx.continueRun(t)
		fx.submitFreshVerdict(t, domain.VerdictApproved, "approved")

		// The verifier is unavailable for the first pass — a restart landing
		// between the approval and the verification.
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
			t.Fatalf("run state = %q, want completed after the restart", got)
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

// 6. Fifty reconcile/poll passes over the completed lifecycle create nothing.
func TestFreshReviewRepeatedReconcileIsInert(t *testing.T) {
	fx := newFreshReviewFixture(t)
	fx.correctAO(t)
	fx.driftWorkspace(t)
	fx.continueRun(t)
	fx.submitFreshVerdict(t, domain.VerdictApproved, "approved")
	fx.poll(t, 2)
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunCompleted {
		t.Fatalf("fixture did not complete: %q", got)
	}

	checkpoints := len(fx.store.checkpoints[fx.runID])
	attempts := len(fx.store.attempts["verify"])
	calls := len(fx.runner.calls)

	for i := 0; i < 50; i++ {
		fx.clk.Advance(time.Second)
		if _, err := fx.coord.GetRun(context.Background(), fx.runID); err != nil {
			t.Fatal(err)
		}
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(fx.store.checkpoints[fx.runID]); got != checkpoints {
		t.Fatalf("checkpoints = %d, want unchanged %d after 50 reconciles", got, checkpoints)
	}
	if got := len(fx.store.attempts["verify"]); got != attempts {
		t.Fatalf("verify attempts = %d, want unchanged %d", got, attempts)
	}
	if got := len(fx.runner.calls); got != calls {
		t.Fatalf("verifier calls = %d, want unchanged %d", got, calls)
	}
	if fx.launcher.launchCalls != 1 || fx.reviews.insertCalls != 1 {
		t.Fatalf("reviewer launches = %d, review runs inserted = %d, want 1 and 1",
			fx.launcher.launchCalls, fx.reviews.insertCalls)
	}
	if got := len(fx.store.outbox); got != 1 {
		t.Fatalf("outbox entries = %d, want unchanged 1", got)
	}
}

// 7. An unrelated external change — the commit history moved — is never
// absorbed. It stops the run with the reason that names precisely what AO could
// not attribute.
func TestUnrelatedExternalWorkspaceChangeIsNeverAutoReviewed(t *testing.T) {
	fx := newFreshReviewFixture(t)
	fx.correctAO(t)
	fx.driftWorkspace(t)
	// Somebody committed, pulled or rebased in this worktree. That is not this
	// task's uncommitted work.
	obs := fx.ws.obs
	obs.HeadSHA = "someone-elses-commit"
	fx.ws.obs = obs

	fx.continueRun(t)

	if got := fx.phaseCount("verify_fresh_review_required"); got != 0 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want 0: an external edit was absorbed", got)
	}
	if fx.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
	}
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	if got := fx.stepState(t, domain.WorkflowStepReview); got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want still completed: the review was reopened for an unattributable change", got)
	}
	results := fx.verifyResults(t)
	last := results[len(results)-1]
	if last.ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged {
		t.Fatalf("verify error class = %q, want verify_workspace_changed", last.ErrorClass)
	}
	if last.SupersededByFreshReview {
		t.Fatal("a refused drift was recorded as superseded by a fresh review")
	}
	detail, err := fx.coord.GetRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.AttentionReason != workflowcore.ReasonVerifyWorkspaceUnattributable {
		t.Fatalf("attention reason = %q, want %q", life.AttentionReason, workflowcore.ReasonVerifyWorkspaceUnattributable)
	}
	if life.AttentionAction == "" {
		t.Fatal("the stop names no action a person can take")
	}
}

// 8. Outside an authorized recovery, verify_workspace_changed is exactly what it
// always was: no re-review, no new checkpoint phase, and the pre-existing stop.
func TestWorkspaceChangedOutsideRecoveryIsUnchanged(t *testing.T) {
	fx := newFreshReviewFixture(t)
	// No Continue, hence no authorized recovery. Re-park the run on a plain
	// workspace change by correcting the environment and letting the ordinary
	// (non-recovery) verify path re-run from a non-terminal verify step.
	fx.correctAO(t)
	fx.driftWorkspace(t)

	for i := 0; i < 5; i++ {
		fx.poll(t, 1)
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := fx.phaseCount("verify_fresh_review_required"); got != 0 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want 0 outside a recovery", got)
	}
	if fx.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
	}
	if got := fx.stepState(t, domain.WorkflowStepReview); got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want still completed", got)
	}
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
}

// 9. A verification that PASSES never reopens the review step, however hard the
// run is polled afterwards.
func TestSuccessfulVerifyNeverReopensReview(t *testing.T) {
	fx := newFreshReviewFixture(t)
	fx.correctAO(t)
	// No drift at all: the recovery verifies the same target and passes.
	fx.continueRun(t)
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", got)
	}

	for i := 0; i < 5; i++ {
		fx.poll(t, 1)
		if err := fx.coord.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
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
}

// 10. A verification target key that no longer matches the one the recovery was
// authorized for never gets a fresh review, and therefore never reuses the old
// approval. The target key is a hash of the reviewed fingerprint AND the
// verification plan, so editing the plan moves it even when the reviewed
// fingerprint question is untouched — which is exactly the "this is not the same
// task any more" case the predicate must catch.
func TestFreshReviewRefusesAMismatchedTargetKey(t *testing.T) {
	fx := newFreshReviewFixture(t)
	fx.correctAO(t)
	fx.driftWorkspace(t)

	// The project's verification plan is edited between the historical failure
	// and the recovery.
	edited, err := workflowcore.MarshalPlanArtifact(workflowcore.BuildPlanArtifact(
		"project-1", "verify objective", "v1", workflowcore.VerificationPlan{
			Commands: []workflowcore.VerificationCommandCheck{
				{Command: "go", Args: []string{"build", "./..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
			},
			Files: []workflowcore.VerificationFileCheck{{Path: staleVerifyArtifact, Exists: true}},
		}))
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range fx.store.steps[fx.runID] {
		if s.Kind == domain.WorkflowStepPlan {
			s.ArtifactJSON = edited
			fx.store.steps[fx.runID][i] = s
		}
	}

	fx.continueRun(t)

	if got := fx.phaseCount("verify_fresh_review_required"); got != 0 {
		t.Fatalf("verify_fresh_review_required checkpoints = %d, want 0: a moved target key was re-reviewed anyway", got)
	}
	if got := fx.phaseCount("verify_fresh_review_approved"); got != 0 {
		t.Fatalf("verify_fresh_review_approved checkpoints = %d, want 0", got)
	}
	if fx.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
	}
	if got := fx.stepState(t, domain.WorkflowStepReview); got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want still completed", got)
	}
	if got := fx.store.runs[fx.runID].State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	results := fx.verifyResults(t)
	if last := results[len(results)-1]; last.Passed || last.SupersededByFreshReview {
		t.Fatalf("verification consumed an approval for a target key it was never authorized for: %+v", last)
	}
	detail, err := fx.coord.GetRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.AttentionReason != workflowcore.ReasonVerifyWorkspaceUnattributable {
		t.Fatalf("attention reason = %q, want %q", life.AttentionReason, workflowcore.ReasonVerifyWorkspaceUnattributable)
	}
}
