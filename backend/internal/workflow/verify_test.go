package workflow_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type fakeVerifyRunner struct {
	result workflowcore.VerifyCommandExecution
	err    error
	calls  int
	last   workflowcore.VerifyCommandRequest
	after  func()
}

func (f *fakeVerifyRunner) Run(_ context.Context, req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
	f.calls++
	f.last = req
	if f.after != nil {
		f.after()
	}
	return f.result, f.err
}

type sequenceWorkspaceFacts struct {
	observations []ports.WorkspaceObservation
	calls        int
}

func (f *sequenceWorkspaceFacts) MaterializeIntegrationCommit(_ context.Context, _ ports.WorkspaceInfo, _, _, _ string, _ []string) (string, string, bool, error) {
	return "", "", false, nil
}

func (f *sequenceWorkspaceFacts) ObserveWorkspace(_ context.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	i := f.calls
	f.calls++
	if i >= len(f.observations) {
		i = len(f.observations) - 1
	}
	return f.observations[i], nil
}

func verifyFixture(t *testing.T, plan workflowcore.VerificationPlan, runner workflowcore.VerifyRunner, observations ...ports.WorkspaceObservation) (*workflowcore.Coordinator, *fakeStore, string, string) {
	t.Helper()
	store := newFakeStore()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	runID := "wf-verify"
	sid := "sess-verify"
	reviewID := "review-verify"
	artifact := workflowcore.BuildPlanArtifact("project-1", "verify objective", "v1", plan)
	raw, err := workflowcore.MarshalPlanArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	steps := []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: raw},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &sid},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted, ReviewRunID: &reviewID},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepWaiting},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepPending},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending},
	}
	store.runs[runID] = domain.WorkflowRun{ID: runID, ProjectID: "project-1", Objective: "verify objective", State: domain.WorkflowRunWaiting, PolicyVersion: "v1", PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now, UpdatedAt: now}
	store.steps[runID] = steps
	path := observations[0].Path
	stepID := "work"
	store.checkpoints[runID] = []domain.WorkflowCheckpoint{{ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &stepID, ProjectID: "project-1", SessionID: &sid, Branch: "feature", WorktreePath: path, FingerprintAfter: workflowcore.WorkspaceFingerprint(observations[0]), CreatedAt: now}}
	reviews := newFakeReviewRuns()
	reviews.runs[reviewID] = domain.ReviewRun{ID: reviewID, SessionID: domain.SessionID(sid), Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, TargetSHA: workflowcore.WorkspaceFingerprint(observations[0])}
	clock := &fakeClock{t: now}
	ids := 0
	workspaceObservations := observations
	if len(observations) > 1 {
		workspaceObservations = observations[1:]
	}
	c := workflowcore.New(workflowcore.Deps{Store: store, ReviewRuns: reviews, WorkspaceFacts: &sequenceWorkspaceFacts{observations: workspaceObservations}, Verifier: runner, Clock: clock.Now, NewID: func() string { ids++; return fmt.Sprintf("v%d", ids) }})
	return c, store, runID, path
}

func cleanObservation(path string) ports.WorkspaceObservation {
	return ports.WorkspaceObservation{Path: path, Branch: "feature", HeadSHA: "abc123"}
}

func TestVerifyCommandOutcomesAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    workflowcore.VerifyCommandExecution
		wantRun   domain.WorkflowRunState
		wantStep  domain.WorkflowStepState
		wantClass domain.WorkflowErrorClass
	}{
		{"exit zero completes", workflowcore.VerifyCommandExecution{ExitCode: 0, DurationMS: 12}, domain.WorkflowRunCompleted, domain.WorkflowStepCompleted, ""},
		{"nonzero needs attention", workflowcore.VerifyCommandExecution{ExitCode: 2}, domain.WorkflowRunNeedsAttention, domain.WorkflowStepFailed, domain.WorkflowErrorVerifyCommandFailed},
		{"timeout needs attention", workflowcore.VerifyCommandExecution{ExitCode: -1, TimedOut: true}, domain.WorkflowRunNeedsAttention, domain.WorkflowStepFailed, domain.WorkflowErrorVerifyTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			obs := cleanObservation(dir)
			runner := &fakeVerifyRunner{result: tc.result}
			plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0, RetrySafe: true}}}
			c, store, id, _ := verifyFixture(t, plan, runner, obs, obs)
			detail, err := c.GetRun(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Run.State != tc.wantRun {
				t.Fatalf("run=%s", detail.Run.State)
			}
			var verify workflowcore.StepDetail
			for _, s := range detail.Steps {
				if s.Step.Kind == domain.WorkflowStepVerify {
					verify = s
				}
			}
			if verify.Step.State != tc.wantStep {
				t.Fatalf("step=%s", verify.Step.State)
			}
			if len(verify.Attempts) != 1 || verify.Attempts[0].ErrorClass != tc.wantClass {
				t.Fatalf("attempt=%+v", verify.Attempts)
			}
			if runner.calls != 1 {
				t.Fatalf("calls=%d", runner.calls)
			}
			_, err = c.GetRun(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if runner.calls != 1 {
				t.Fatalf("verification reran: %d", runner.calls)
			}
			_ = store
		})
	}
}

func TestVerifyFileChecks(t *testing.T) {
	dir := t.TempDir()
	content := "verified\n"
	if err := os.WriteFile(filepath.Join(dir, "artifact.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	for _, tc := range []struct {
		name  string
		check workflowcore.VerificationFileCheck
		want  domain.WorkflowRunState
		class domain.WorkflowErrorClass
	}{
		{"exists", workflowcore.VerificationFileCheck{Path: "artifact.txt", Exists: true}, domain.WorkflowRunCompleted, ""},
		{"exact", workflowcore.VerificationFileCheck{Path: "artifact.txt", Exists: true, ExactContent: &content}, domain.WorkflowRunCompleted, ""},
		{"hash", workflowcore.VerificationFileCheck{Path: "artifact.txt", Exists: true, SHA256: hex.EncodeToString(sum[:])}, domain.WorkflowRunCompleted, ""},
		{"missing", workflowcore.VerificationFileCheck{Path: "missing.txt", Exists: true}, domain.WorkflowRunNeedsAttention, domain.WorkflowErrorVerifyArtifactMissing},
		{"mismatch", workflowcore.VerificationFileCheck{Path: "artifact.txt", Exists: true, SHA256: strings.Repeat("0", 64)}, domain.WorkflowRunNeedsAttention, domain.WorkflowErrorVerifyArtifactMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := cleanObservation(dir)
			c, _, id, _ := verifyFixture(t, workflowcore.VerificationPlan{Files: []workflowcore.VerificationFileCheck{tc.check}}, &fakeVerifyRunner{}, obs, obs)
			detail, err := c.GetRun(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Run.State != tc.want {
				t.Fatalf("state=%s", detail.Run.State)
			}
			for _, s := range detail.Steps {
				if s.Step.Kind == domain.WorkflowStepVerify && len(s.Attempts) > 0 && s.Attempts[0].ErrorClass != tc.class {
					t.Fatalf("class=%s", s.Attempts[0].ErrorClass)
				}
			}
		})
	}
}

func TestVerifyWorkspaceGuards(t *testing.T) {
	t.Run("changed before verify does not execute", func(t *testing.T) {
		dir := t.TempDir()
		reviewed := cleanObservation(dir)
		changed := reviewed
		changed.HeadSHA = "different"
		runner := &fakeVerifyRunner{}
		plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", RetrySafe: true}}}
		c, _, id, _ := verifyFixture(t, plan, runner, reviewed, changed)
		detail, err := c.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if runner.calls != 0 || detail.Run.State != domain.WorkflowRunNeedsAttention {
			t.Fatalf("calls=%d state=%s", runner.calls, detail.Run.State)
		}
	})
	t.Run("changed during verify fails conservative", func(t *testing.T) {
		dir := t.TempDir()
		before := cleanObservation(dir)
		after := before
		after.HeadSHA = "different"
		runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
		plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", RetrySafe: true}}}
		c, _, id, _ := verifyFixture(t, plan, runner, before, before, after)
		detail, err := c.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Run.State != domain.WorkflowRunNeedsAttention {
			t.Fatalf("state=%s", detail.Run.State)
		}
	})
}

func TestVerifyInterruptedAttemptPolicy(t *testing.T) {
	dir := t.TempDir()
	obs := cleanObservation(dir)
	for _, tc := range []struct {
		name      string
		retrySafe bool
		wantCalls int
		want      domain.WorkflowRunState
	}{
		{"retry safe resumes same attempt", true, 1, domain.WorkflowRunCompleted},
		{"unsafe becomes ambiguous", false, 0, domain.WorkflowRunNeedsAttention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
			plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", RetrySafe: tc.retrySafe}}}
			c, store, id, _ := verifyFixture(t, plan, runner, obs, obs)
			artifact := workflowcore.BuildPlanArtifact("project-1", "verify objective", "v1", plan)
			target := workflowcore.WorkspaceFingerprint(obs)
			raw, _ := workflowcore.MarshalPlanArtifact(artifact)
			store.steps[id][0].ArtifactJSON = raw
			key := verifyTargetForTest(target, plan)
			store.attempts["verify"] = []domain.WorkflowAttempt{{ID: "existing", WorkflowStepID: "verify", AttemptNumber: 1, Harness: "local-verify", Model: key, StartedAt: time.Now()}}
			store.steps[id][4].State = domain.WorkflowStepWaiting
			detail, err := c.GetRun(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if runner.calls != tc.wantCalls || detail.Run.State != tc.want {
				t.Fatalf("calls=%d state=%s", runner.calls, detail.Run.State)
			}
			if len(store.attempts["verify"]) != 1 {
				t.Fatalf("duplicate attempts: %d", len(store.attempts["verify"]))
			}
		})
	}
}

func TestVerifyRecoversPersistedSuccessWithoutRerun(t *testing.T) {
	dir := t.TempDir()
	obs := cleanObservation(dir)
	runner := &fakeVerifyRunner{}
	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", RetrySafe: true}}}
	c, store, id, _ := verifyFixture(t, plan, runner, obs, obs)
	target := workflowcore.WorkspaceFingerprint(obs)
	key := verifyTargetForTest(target, plan)
	attemptID := "existing"
	stepID := "verify"
	store.attempts[stepID] = []domain.WorkflowAttempt{{ID: attemptID, WorkflowStepID: stepID, AttemptNumber: 1, Harness: "local-verify", Model: key, StartedAt: time.Now()}}
	store.steps[id][4].State = domain.WorkflowStepRunning
	result, _ := json.Marshal(workflowcore.VerifyResult{Version: "v1", TargetKey: key, ReviewedFingerprint: target, PreFingerprint: target, PostFingerprint: target, Passed: true, Checks: []workflowcore.VerifyCheckResult{{Kind: "command", Label: "go", Passed: true}}})
	store.checkpoints[id] = append(store.checkpoints[id], domain.WorkflowCheckpoint{ID: "verify-result", WorkflowRunID: id, WorkflowStepID: &stepID, AttemptID: &attemptID, RetryState: string(result), DurablePhase: "verify_result", CreatedAt: time.Now().Add(time.Minute)})
	detail, err := c.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 || detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("calls=%d state=%s", runner.calls, detail.Run.State)
	}
}

func TestCancelledRunNeverVerifies(t *testing.T) {
	dir := t.TempDir()
	obs := cleanObservation(dir)
	runner := &fakeVerifyRunner{}
	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", RetrySafe: true}}}
	c, store, id, _ := verifyFixture(t, plan, runner, obs, obs)
	store.runs[id] = domain.WorkflowRun{ID: id, ProjectID: "project-1", State: domain.WorkflowRunCancelled}
	if _, err := c.GetRun(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatalf("verify calls=%d", runner.calls)
	}
}

func verifyTargetForTest(fp string, plan workflowcore.VerificationPlan) string {
	// Create a temporary coordinator target indirectly is intentionally avoided;
	// this mirrors verificationTargetKey's canonical JSON algorithm.
	b, _ := jsonMarshal(plan)
	sum := sha256.Sum256(append([]byte(fp+"\n"), b...))
	return hex.EncodeToString(sum[:])
}
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
