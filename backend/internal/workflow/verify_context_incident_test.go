package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// Checkpoint 8P-E.14 regression suite for the wf-2077d15a incident.
//
// The run under test is the exact shape of the real one: work completed, review
// approved (after AO's own reviewer-launch retry, which this file deliberately
// does not touch), and verification configured as `go build ./...` with
// workingDirectory "." in a repository whose Go module lives in backend/.

// scriptedVerifyRunner answers per working directory, so a test can express
// "this command succeeds in backend/ and fails at the repo root" — the precise
// asymmetry the incident turned on.
type scriptedVerifyRunner struct {
	respond func(req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error)
	calls   []workflowcore.VerifyCommandRequest
}

func (r *scriptedVerifyRunner) Run(_ context.Context, req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
	r.calls = append(r.calls, req)
	return r.respond(req)
}

func (r *scriptedVerifyRunner) dirs(root string) []string {
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		rel, err := filepath.Rel(realPath(root), realPath(c.Directory))
		if err != nil {
			rel = c.Directory
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

const wrongModuleRootStderr = "pattern ./...: directory prefix . does not contain main module or its selected dependencies"

// goModuleRunner is the honest simulation of the Go toolchain for these tests:
// it succeeds only when started from a directory that actually holds a go.mod
// (or go.work), and otherwise says exactly what Go said in the incident.
func goModuleRunner(root string) *scriptedVerifyRunner {
	return &scriptedVerifyRunner{respond: func(req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		if hasModuleFile(req.Directory) {
			return workflowcore.VerifyCommandExecution{ExitCode: 0, DurationMS: 40}, nil
		}
		return workflowcore.VerifyCommandExecution{ExitCode: 1, StderrTail: wrongModuleRootStderr}, nil
	}}
}

func hasModuleFile(dir string) bool {
	for _, name := range []string{"go.mod", "go.work"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func writeWorktree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// incidentFixture is verifyReentryFixture's shape with the worktree layout and
// verification plan under the test's control, and a real MessageSender so a
// wrongly-classified failure would visibly reach a fix worker.
func incidentFixture(t *testing.T, root string, plan workflowcore.VerificationPlan, runner workflowcore.VerifyRunner) (
	*workflowcore.Coordinator, *fakeStore, *fakeClock, *fakeMessageSender, string,
) {
	t.Helper()
	store := newFakeStore()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	runID, sid, reviewID := "wf-2077d15a", "sess-incident", "review-incident"

	artifact := workflowcore.BuildPlanArtifact("project-1", "verify objective", "v1", plan)
	raw, err := workflowcore.MarshalPlanArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: raw},
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

	sender := &fakeMessageSender{}
	clk := &fakeClock{t: now}
	ids := 0
	c := workflowcore.New(workflowcore.Deps{
		Store: store, ReviewRuns: reviews, WorkspaceFacts: &mutableWorkspaceFacts{obs: approved},
		SessionFacts: facts, Verifier: runner, MessageSender: sender,
		Clock: clk.Now, NewID: func() string { ids++; return fmt.Sprintf("ic%d", ids) },
	})
	return c, store, clk, sender, runID
}

func goBuildPlan(workingDir string) workflowcore.VerificationPlan {
	return workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{
		{Command: "go", Args: []string{"build", "./..."}, WorkingDirectory: workingDir, RequiredExitCode: 0, RetrySafe: true},
	}}
}

// TestVerifyResolvesModuleRootInMultiPartRepo is the incident itself.
//
// Before this checkpoint: `go build ./...` ran at the repo root, exited 1 with
// "directory prefix . does not contain main module", AO called that a code
// defect, dispatched a fix worker, the worker changed nothing (correctly), and
// the run died in fix_no_verifiable_change → needs_attention.
func TestVerifyResolvesModuleRootInMultiPartRepo(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"backend/go.mod":        "module x\n",
		"backend/main.go":       "package main\n",
		"frontend/package.json": "{}",
		"README.md":             "# repo\n",
	})
	runner := goModuleRunner(root)
	c, store, _, sender, runID := incidentFixture(t, root, goBuildPlan("."), runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times for an AO working-directory mistake", sender.calls)
	}
	if dirs := runner.dirs(root); len(dirs) != 1 || dirs[0] != "backend" {
		t.Fatalf("verify ran in %v, want exactly one run in backend", dirs)
	}
	// The decision is durable and explains itself.
	res := latestVerifyResult(t, store, runID)
	if len(res.ContextResolutions) != 1 {
		t.Fatalf("context resolutions = %+v, want exactly one", res.ContextResolutions)
	}
	if got := res.ContextResolutions[0]; got.Requested != "." || got.Resolved != "backend" || got.Reason == "" {
		t.Fatalf("resolution = %+v, want . -> backend with a reason", got)
	}
}

// TestVerifyRootModuleStillRunsAtTheRoot: the ordinary single-module repo is
// completely untouched — no redirect, no repair, no new checkpoints.
func TestVerifyRootModuleStillRunsAtTheRoot(t *testing.T) {
	root := writeWorktree(t, map[string]string{"go.mod": "module x\n", "main.go": "package main\n"})
	runner := goModuleRunner(root)
	c, store, _, sender, runID := incidentFixture(t, root, goBuildPlan("."), runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed", detail.Run.State)
	}
	if dirs := runner.dirs(root); len(dirs) != 1 || dirs[0] != "." {
		t.Fatalf("verify ran in %v, want the worktree root", dirs)
	}
	if len(latestVerifyResult(t, store, runID).ContextResolutions) != 0 {
		t.Fatal("a root-module repo must not record a working-directory redirect")
	}
	_ = sender
}

// TestVerifyPreservesExplicitWorkingDirectory: a plan that already names a
// valid directory inside the module is authoritative. AO does not second-guess
// a working directory that works.
func TestVerifyPreservesExplicitWorkingDirectory(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"backend/go.mod":               "module x\n",
		"backend/internal/a/a.go":      "package a\n",
		"services/api/go.mod":          "module api\n",
		"services/api/internal/b/b.go": "package b\n",
	})
	runner := goModuleRunner(root)
	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{
		{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: "services/api", RequiredExitCode: 0, RetrySafe: true},
	}}
	c, _, _, _, runID := incidentFixture(t, root, plan, runner)

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if dirs := runner.dirs(root); len(dirs) != 1 || dirs[0] != "services/api" {
		t.Fatalf("verify ran in %v, want the explicitly configured services/api", dirs)
	}
}

// TestVerifySelfHealsWrongModuleRootAndContinues covers the self-heal loop
// proper: the configured directory looks like a module root (it has a go.mod),
// so pre-flight leaves it alone, and only the toolchain's own answer proves it
// wrong. AO corrects the context, persists the decision, re-runs the same
// command, and the run continues with no human and no fix worker.
func TestVerifySelfHealsWrongModuleRootAndContinues(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"go.mod":          "module placeholder\n",
		"backend/go.mod":  "module x\n",
		"backend/main.go": "package main\n",
	})
	// The root go.mod exists but the toolchain still refuses there.
	runner := &scriptedVerifyRunner{respond: func(req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		if strings.HasSuffix(realPath(req.Directory), string(filepath.Separator)+"backend") {
			return workflowcore.VerifyCommandExecution{ExitCode: 0}, nil
		}
		return workflowcore.VerifyCommandExecution{ExitCode: 1, StderrTail: wrongModuleRootStderr}, nil
	}}
	c, store, _, sender, runID := incidentFixture(t, root, goBuildPlan("."), runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed after a self-healed verify (steps: %s)", detail.Run.State, stepStates(detail))
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times during a self-heal", sender.calls)
	}
	if dirs := runner.dirs(root); len(dirs) != 2 || dirs[0] != "." || dirs[1] != "backend" {
		t.Fatalf("verify directories = %v, want [. backend]", dirs)
	}
	repairs := checkpointsByPhase(store, runID, "verify_context_repair")
	if len(repairs) != 1 {
		t.Fatalf("durable repair checkpoints = %d, want exactly 1", len(repairs))
	}
	var resolution workflowcore.VerifyContextResolution
	if err := json.Unmarshal([]byte(repairs[0].RetryState), &resolution); err != nil {
		t.Fatal(err)
	}
	if !resolution.Repaired || resolution.Resolved != "backend" {
		t.Fatalf("persisted repair = %+v, want a repaired resolution to backend", resolution)
	}
}

// TestVerifyAmbiguousModuleRootsStopWithAPreciseReason: several candidate
// module roots is exactly the case AO must not guess about. It stops, it says
// why, and it still never blames the code.
func TestVerifyAmbiguousModuleRootsStopWithAPreciseReason(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"backend/go.mod": "module x\n",
		"tools/go.mod":   "module y\n",
	})
	runner := goModuleRunner(root)
	c, _, _, sender, runID := incidentFixture(t, root, goBuildPlan("."), runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times for an unresolvable verification context", sender.calls)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention", detail.Run.State)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.AttentionReason != workflowcore.ReasonVerifyConfigInvalid {
		t.Fatalf("attention reason = %q, want %q", life.AttentionReason, workflowcore.ReasonVerifyConfigInvalid)
	}
	if life.Attention != workflowcore.AttentionHuman || life.AttentionAction == "" {
		t.Fatalf("attention=%q action=%q, want an actionable human decision", life.Attention, life.AttentionAction)
	}
	if !strings.Contains(detail.NextAction, "backend") || !strings.Contains(detail.NextAction, "tools") {
		t.Fatalf("next action %q does not name the ambiguous candidates", detail.NextAction)
	}
}

// TestVerifyGenuineFailureStillReachesTheFixWorker is the other half of the
// classification: nothing above may make AO stop taking real failures
// seriously.
func TestVerifyGenuineFailureStillReachesTheFixWorker(t *testing.T) {
	root := writeWorktree(t, map[string]string{"backend/go.mod": "module x\n", "backend/main.go": "package main\n"})
	runner := &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		return workflowcore.VerifyCommandExecution{ExitCode: 1, StdoutTail: "--- FAIL: TestThing (0.01s)\nFAIL\tgithub.com/x/y\t0.4s"}, nil
	}}
	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{
		{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0, RetrySafe: true},
	}}
	c, _, clk, sender, runID := incidentFixture(t, root, plan, runner)
	ctx := context.Background()

	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.LatestCheckpointPhase != workflowcore.ReasonVerifyFixReentry {
		t.Fatalf("latest checkpoint = %q, want a verify fix re-entry", detail.LatestCheckpointPhase)
	}
	clk.Advance(time.Minute)
	if _, err := c.GetRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatalf("fix dispatches = %d, want exactly 1 for a genuinely failing test", sender.calls)
	}
	if !strings.Contains(sender.lastMsg, "TestThing") {
		t.Fatalf("fix prompt does not carry the real failure output: %q", sender.lastMsg)
	}
	// It also ran in the right place: resolution applies to failing runs too.
	if dirs := runner.dirs(root); dirs[0] != "backend" {
		t.Fatalf("verify ran in %v, want backend", dirs)
	}
}

// TestVerifyMissingBinaryIsNotACodeFailure: a runtime/tool failure names itself
// and never masquerades as a defect in the worker's code.
func TestVerifyMissingBinaryIsNotACodeFailure(t *testing.T) {
	root := writeWorktree(t, map[string]string{"go.mod": "module x\n"})
	runner := &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		return workflowcore.VerifyCommandExecution{}, fmt.Errorf(`exec: "go": executable file not found in $PATH`)
	}}
	c, _, _, sender, runID := incidentFixture(t, root, goBuildPlan("."), runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times for a missing verifier binary", sender.calls)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.AttentionReason != workflowcore.ReasonVerifyToolUnavailable {
		t.Fatalf("attention reason = %q, want %q", life.AttentionReason, workflowcore.ReasonVerifyToolUnavailable)
	}
}

// TestVerifyContextRepairSurvivesReconcileWithoutDuplicating: re-entering the
// coordinator (the restart/reconcile path) must not re-run verification, create
// a second attempt, or repeat the repair — the durable records are the memory.
func TestVerifyContextRepairSurvivesReconcileWithoutDuplicating(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"go.mod":          "module placeholder\n",
		"backend/go.mod":  "module x\n",
		"backend/main.go": "package main\n",
	})
	runner := &scriptedVerifyRunner{respond: func(req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		if strings.HasSuffix(realPath(req.Directory), string(filepath.Separator)+"backend") {
			return workflowcore.VerifyCommandExecution{ExitCode: 0}, nil
		}
		return workflowcore.VerifyCommandExecution{ExitCode: 1, StderrTail: wrongModuleRootStderr}, nil
	}}
	c, store, clk, _, runID := incidentFixture(t, root, goBuildPlan("."), runner)
	ctx := context.Background()

	if _, err := c.GetRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := len(runner.calls)
	repairsAfterFirst := len(checkpointsByPhase(store, runID, "verify_context_repair"))

	for i := 0; i < 3; i++ {
		clk.Advance(time.Minute)
		if _, err := c.GetRun(ctx, runID); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.calls) != callsAfterFirst {
		t.Fatalf("verify re-executed on reconcile: %d calls, want %d", len(runner.calls), callsAfterFirst)
	}
	if got := len(checkpointsByPhase(store, runID, "verify_context_repair")); got != repairsAfterFirst {
		t.Fatalf("repair checkpoints = %d, want %d", got, repairsAfterFirst)
	}
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	verify := stepFromDetail(t, detail, domain.WorkflowStepVerify)
	if len(verify.Attempts) != 1 {
		t.Fatalf("verify attempts = %d, want exactly 1", len(verify.Attempts))
	}
}

func checkpointsByPhase(store *fakeStore, runID, phase string) []domain.WorkflowCheckpoint {
	var out []domain.WorkflowCheckpoint
	for _, cp := range store.checkpoints[runID] {
		if cp.DurablePhase == phase {
			out = append(out, cp)
		}
	}
	return out
}

func latestVerifyResult(t *testing.T, store *fakeStore, runID string) workflowcore.VerifyResult {
	t.Helper()
	var latest *domain.WorkflowCheckpoint
	for i := range store.checkpoints[runID] {
		cp := &store.checkpoints[runID][i]
		if cp.DurablePhase != "verify_result" {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		t.Fatal("no verify_result checkpoint recorded")
	}
	var res workflowcore.VerifyResult
	if err := json.Unmarshal([]byte(latest.RetryState), &res); err != nil {
		t.Fatal(err)
	}
	return res
}
