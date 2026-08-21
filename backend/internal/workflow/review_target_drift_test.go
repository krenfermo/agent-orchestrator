package workflow_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// Checkpoint 8P-E.13A.3 — the real reproduction, from ~/.ao/data.
//
// Child run wf-507d9a93 (direct_branch, feat/engineering-control-center)
// observed its work step at 20:51:35Z and recorded fingerprint
// a7451feb…, computed over HEAD 8df30fa1b with the worker's edits still
// uncommitted. The run then sat in needs_attention for 110 minutes. At
// 21:36Z a concurrent actor committed on the same branch (6fb66f7a9), which
// moved HEAD and cleaned the worktree. The reviewer was finally dispatched at
// 22:41:42Z, read THAT state, and approved — but target_sha was still the
// 110-minute-old work fingerprint. Verify at 22:44:22Z observed 053afb36…
// (clean at 6fb66f7a9), compared it against a7451feb…, and failed with
// verify_workspace_changed after 0 fix cycles, even though nothing at all
// changed after the reviewer approved.
//
// The fix records target_sha as the workspace the reviewer is actually about
// to read. These tests pin both halves of the required invariant: no false
// verify_workspace_changed when nothing moved after approval, and a real
// post-approval change still fails.

// driftFixture wires a full work->review->verify coordinator over a real
// temp worktree, with a workspace fake whose observation the test mutates to
// simulate the repository moving underneath the run.
func driftFixture(t *testing.T, runner workflowcore.VerifyRunner) (*workflowcore.Coordinator, *fakeStore, *fakeClock, *fakeSessionFacts, *fakeWorkspaceFacts, *fakeReviewRuns, *fakeReviewerLauncher, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "frontend", "src"), 0o755); err != nil {
		t.Fatalf("mkdir frontend/src: %v", err)
	}
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "feat/engineering-control-center", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}

	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 21, 20, 32, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: launcher,
		Verifier:         runner,
		Clock:            clk.Now,
		NewID:            func() string { idSeq++; return fmt.Sprintf("id%d", idSeq) },
	})
	return c, store, clk, sessionFacts, workspaceFacts, reviewRuns, launcher, dir
}

// driftWorkerEdit is the worker's delivered change: a real frontend file on
// disk plus the untracked git-status entry that names it, so the fingerprint
// hashes real bytes exactly as it does in production.
func driftWorkerEdit(t *testing.T, dir string) ports.WorkspaceObservation {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "frontend", "src", "Board.tsx"), []byte("export const Board = () => null;\n"), 0o644); err != nil {
		t.Fatalf("write Board.tsx: %v", err)
	}
	return ports.WorkspaceObservation{
		Path:      dir,
		Branch:    "feat/engineering-control-center",
		HeadSHA:   "8df30fa1b496d7023afc17d243591a2d37637a2d",
		Dirty:     true,
		Untracked: true,
		Changes:   []ports.WorkspaceChange{{Path: "frontend/src/Board.tsx", Status: "??"}},
	}
}

// driftCommitted is the same content after a concurrent actor committed it on
// the same branch: HEAD moved, the worktree is clean, no status entries.
func driftCommitted(dir string) ports.WorkspaceObservation {
	return ports.WorkspaceObservation{
		Path:    dir,
		Branch:  "feat/engineering-control-center",
		HeadSHA: "6fb66f7a97c61d96fad82be33936662d43c8493d",
	}
}

// driftStartRun creates a run with a real verification plan and drives it to a
// completed work step, leaving the review step pending.
func driftStartRun(t *testing.T, c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, sessionFacts *fakeSessionFacts, workspaceFacts *fakeWorkspaceFacts, dir string) string {
	t.Helper()
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the board card")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := created.Run.ID

	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"build", "./..."}, RequiredExitCode: 0, RetrySafe: true}}}
	artifact := workflowcore.BuildPlanArtifact("proj-1", "ship the board card", "v1", plan)
	raw, err := workflowcore.MarshalPlanArtifact(artifact)
	if err != nil {
		t.Fatalf("MarshalPlanArtifact: %v", err)
	}
	for i, s := range store.steps[runID] {
		if s.Kind == domain.WorkflowStepPlan {
			store.steps[runID][i].ArtifactJSON = raw
		}
	}

	detail, err := c.StartRun(ctx, runID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: dir, Branch: "feat/engineering-control-center"},
	})
	workspaceFacts.obs = driftWorkerEdit(t, dir)
	clk.Advance(19 * time.Minute)
	got, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", workStepFrom(got).Step.State)
	}
	return runID
}

func driftVerifyStep(t *testing.T, detail workflowcore.RunDetail) workflowcore.StepDetail {
	t.Helper()
	for _, s := range detail.Steps {
		if s.Step.Kind == domain.WorkflowStepVerify {
			return s
		}
	}
	t.Fatal("no verify step")
	return workflowcore.StepDetail{}
}

// Test: the repository moving between work completion and review dispatch
// must not strand the run. The reviewer reads the moved state, so the target
// recorded for it is the moved state — and verify, finding nothing changed
// after approval, proceeds to completion instead of verify_workspace_changed.
func TestReviewTargetFollowsTheWorkspaceTheReviewerReads(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0, DurationMS: 8}}
	c, store, clk, sessionFacts, workspaceFacts, reviewRuns, _, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, store, clk, sessionFacts, workspaceFacts, dir)
	workFingerprint := workflowcore.WorkspaceFingerprint(workspaceFacts.obs)

	// 110 minutes pass; a concurrent actor commits on the same branch.
	clk.Advance(110 * time.Minute)
	workspaceFacts.obs = driftCommitted(dir)
	movedFingerprint := workflowcore.WorkspaceFingerprint(workspaceFacts.obs)
	if movedFingerprint == workFingerprint {
		t.Fatal("fixture is not exercising drift: fingerprints are equal")
	}

	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatal("review step has no review_run_id after dispatch")
	}
	reviewRun := reviewRuns.runs[*review.Step.ReviewRunID]
	if reviewRun.TargetSHA != movedFingerprint {
		t.Fatalf("review target = %s, want the workspace the reviewer reads (%s); stale work fingerprint was %s",
			reviewRun.TargetSHA, movedFingerprint, workFingerprint)
	}

	// The drift itself is recorded durably rather than silently swallowed.
	var recorded *domain.WorkflowCheckpoint
	for i, cp := range store.checkpoints[runID] {
		if cp.DurablePhase == "review_target_observed" {
			recorded = &store.checkpoints[runID][i]
		}
	}
	if recorded == nil {
		t.Fatal("no review_target_observed checkpoint recorded")
	}
	if recorded.FingerprintBefore != workFingerprint || recorded.FingerprintAfter != movedFingerprint {
		t.Fatalf("checkpoint before/after = %s/%s, want %s/%s", recorded.FingerprintBefore, recorded.FingerprintAfter, workFingerprint, movedFingerprint)
	}

	// Reviewer approves. Nothing changes afterwards.
	reviewRuns.setStatus(reviewRun.ID, domain.ReviewRunComplete, domain.VerdictApproved)
	clk.Advance(3 * time.Minute)
	got, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after approval: %v", err)
	}
	verify := driftVerifyStep(t, got)
	for _, a := range verify.Attempts {
		if a.ErrorClass == domain.WorkflowErrorVerifyWorkspaceChanged {
			t.Fatalf("verify failed with verify_workspace_changed although nothing changed after approval")
		}
	}
	if runner.calls != 1 {
		t.Fatalf("verify command ran %d times, want 1", runner.calls)
	}
	if verify.Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("verify step = %q, want completed", verify.Step.State)
	}
	if got.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", got.Run.State)
	}
}

// Test: the safety check is not weakened. A change that lands AFTER the
// reviewer approved still fails verification, with the same error class.
func TestVerifyStillFailsOnAPostApprovalChange(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, store, clk, sessionFacts, workspaceFacts, reviewRuns, _, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, store, clk, sessionFacts, workspaceFacts, dir)

	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatal("review step has no review_run_id after dispatch")
	}
	reviewRuns.setStatus(*review.Step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)

	// A genuine, unreviewed edit to the same frontend file lands after approval.
	if err := os.WriteFile(filepath.Join(dir, "frontend", "src", "Board.tsx"), []byte("export const Board = () => 'unreviewed';\n"), 0o644); err != nil {
		t.Fatalf("rewrite Board.tsx: %v", err)
	}
	clk.Advance(2 * time.Minute)
	got, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after approval: %v", err)
	}
	verify := driftVerifyStep(t, got)
	if runner.calls != 0 {
		t.Fatalf("verify commands ran %d times, want 0 — the guard must fire before execution", runner.calls)
	}
	found := false
	for _, a := range verify.Attempts {
		if a.ErrorClass == domain.WorkflowErrorVerifyWorkspaceChanged {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected verify_workspace_changed for a post-approval edit, attempts = %+v", verify.Attempts)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got.Run.State)
	}
}

// Test: the target a cycle was dispatched against is durable and stable.
// adoptReviewOrMarkAmbiguous looks the in-flight review_run up BY target_sha,
// so a re-observation on a later call — with the workspace moved again —
// must not resolve a different target and orphan the running reviewer.
func TestReviewTargetIsStableAcrossRedispatchOfTheSameCycle(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, store, clk, sessionFacts, workspaceFacts, reviewRuns, launcher, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, store, clk, sessionFacts, workspaceFacts, dir)

	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	first := reviewStepFrom(got)
	if first.Step.ReviewRunID == nil {
		t.Fatal("review step has no review_run_id after dispatch")
	}
	target := reviewRuns.runs[*first.Step.ReviewRunID].TargetSHA

	// The workspace moves again while the reviewer is still running.
	workspaceFacts.obs = driftCommitted(dir)
	clk.Advance(time.Minute)
	if _, err := c.ContinueRun(ctx, runID); err != nil {
		t.Fatalf("second ContinueRun: %v", err)
	}
	if reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls = %d, want 1", reviewRuns.insertCalls)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1", launcher.launchCalls)
	}
	if reviewRuns.runs[*first.Step.ReviewRunID].TargetSHA != target {
		t.Fatalf("review target changed under a running reviewer: %s -> %s", target, reviewRuns.runs[*first.Step.ReviewRunID].TargetSHA)
	}
	_ = store
}

// Test: an unavailable workspace observation must not break dispatch. The
// target falls back to the work-completion fingerprint (the pre-8P-E.13A.3
// behavior) and no review_target_observed checkpoint is written, so a later
// call can still resolve a real one.
func TestReviewTargetFallsBackWhenTheWorkspaceCannotBeObserved(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, store, clk, sessionFacts, workspaceFacts, reviewRuns, _, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, store, clk, sessionFacts, workspaceFacts, dir)
	workFingerprint := workflowcore.WorkspaceFingerprint(workspaceFacts.obs)

	workspaceFacts.err = fmt.Errorf("git: repository is locked")
	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatal("dispatch did not happen when the workspace could not be observed")
	}
	if got := reviewRuns.runs[*review.Step.ReviewRunID].TargetSHA; got != workFingerprint {
		t.Fatalf("review target = %s, want the work-completion fallback %s", got, workFingerprint)
	}
	for _, cp := range store.checkpoints[runID] {
		if cp.DurablePhase == "review_target_observed" {
			t.Fatal("a review_target_observed checkpoint was written from a failed observation")
		}
	}
}
