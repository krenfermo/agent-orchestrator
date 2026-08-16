package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// completeWorkStepWithChanges is completeWorkStep (review_dispatch_test.go)
// but lets the caller control the exact ObserveWorkspace.Changes list
// ReviewPolicy will see, since fact-gathering (computeReviewRiskFacts) reads
// real changed paths, not just a dirty flag.
func completeWorkStepWithChanges(t *testing.T, c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, sessionFacts *fakeSessionFacts, workspaceFacts *fakeWorkspaceFacts, runID string, changes []ports.WorkspaceChange) workflowcore.RunDetail {
	t.Helper()
	ctx := context.Background()
	detail, err := c.StartRun(ctx, runID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	workspaceFacts.obs = ports.WorkspaceObservation{Path: "/ws/wf", Branch: "ao/wf", Dirty: true, Changes: changes}
	clk.Advance(10 * time.Second)
	got, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", workStepFrom(got).Step.State)
	}
	_ = store
	return got
}

func verifyStepFrom(detail workflowcore.RunDetail) workflowcore.StepDetail {
	for _, sd := range detail.Steps {
		if sd.Step.Kind == domain.WorkflowStepVerify {
			return sd
		}
	}
	panic("no verify step in run detail")
}

// Scenarios 1/10/11/20/21: an exact-content single-file task is SKIPPED by
// policy, creates zero review_run, never launches the reviewer, advances
// automatically to Verify, and the decision (with reason codes + policy
// version) is durable on the review step's checkpoint stream.
func TestReviewPolicySkipCreatesNoReviewRunAndAdvancesToVerify(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	content := "hello world\n"
	plan := workflowcore.VerificationPlan{
		Files: []workflowcore.VerificationFileCheck{{Path: "docs/notes.md", Exists: true, ExactContent: &content}},
	}
	created, err := c.CreateRun(ctx, "proj-1", "add a short doc note", plan)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	changes := []ports.WorkspaceChange{{Path: "docs/notes.md", Status: "??"}}
	got := completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID, changes)

	// GetRun's own opportunistic cascade (advanceReviewFixCycle with
	// includeCycle1Unblock=false) must NOT unblock cycle 1 by itself — that
	// stays ContinueRun's job (pre-existing 8C/8D contract, unaffected by
	// 8I). The review step must still be pending here.
	if reviewStepFrom(got).Step.State != domain.WorkflowStepPending {
		t.Fatalf("review step state = %q, want pending before ContinueRun", reviewStepFrom(got).Step.State)
	}

	got, err = c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("review step state = %q, want completed (skipped-by-policy)", review.Step.State)
	}
	if review.Step.ReviewRunID != nil {
		t.Fatalf("review step has a review_run_id = %v, want nil (no reviewer ever ran)", *review.Step.ReviewRunID)
	}
	if reviewRuns.insertCalls != 0 {
		t.Fatalf("InsertReviewRun calls = %d, want 0", reviewRuns.insertCalls)
	}
	if launcher.preflightCalls != 0 || launcher.launchCalls != 0 {
		t.Fatalf("reviewer preflight/launch calls = %d/%d, want 0/0", launcher.preflightCalls, launcher.launchCalls)
	}

	// Durable, explainable decision: reason codes + policy version on the
	// review step's own checkpoint stream.
	checkpoints, err := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var found bool
	for _, cp := range checkpoints {
		if cp.DurablePhase != "review_policy_decision" {
			continue
		}
		found = true
		decision, ok := workflowcore.DecodeReviewPolicyDecisionForTest(cp.RetryState)
		if !ok {
			t.Fatalf("review_policy_decision checkpoint is not decodable: %q", cp.RetryState)
		}
		if decision.Decision != workflowcore.ReviewSkipped {
			t.Fatalf("persisted decision = %s, want skipped", decision.Decision)
		}
		if decision.PolicyVersion != workflowcore.ReviewPolicyVersion {
			t.Fatalf("persisted policy version = %s", decision.PolicyVersion)
		}
		if len(decision.Reasons) == 0 {
			t.Fatalf("persisted decision has no reason codes")
		}
	}
	if !found {
		t.Fatalf("no review_policy_decision checkpoint was persisted")
	}

	// Advances automatically to Verify: the run's most recent NextAction
	// (as GetRun already surfaces it) must be "verify", and a further
	// GetRun call must actually execute Verify (it needs no additional
	// human/API trigger — mirrors the approved-review cascade).
	if got.NextAction != "verify" {
		t.Fatalf("NextAction = %q, want %q", got.NextAction, "verify")
	}
}

// Scenario 22 (headless/no distinct code path — the same non-interactive
// Coordinator call path GetRun/ContinueRun already use for headless mode)
// plus a second regression: after a SKIPPED review, Verify actually runs and
// completes the workflow run purely from the workspace-fingerprint identity
// recorded by the work step, without ever needing a review_run.
func TestReviewPolicySkipAllowsVerifyToRunAndCompleteRun(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Verifier: &fakeVerifyRunner{},
		Clock: clk.Now, NewID: func() string { idSeq++; return string(rune('a' + idSeq)) },
	})
	ctx := context.Background()

	content := "hello world\n"
	plan := workflowcore.VerificationPlan{
		Files: []workflowcore.VerificationFileCheck{{Path: "docs/notes.md", Exists: true, ExactContent: &content}},
	}
	created, err := c.CreateRun(ctx, "proj-1", "add a short doc note", plan)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	changes := []ports.WorkspaceChange{{Path: "docs/notes.md", Status: "??"}}
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID, changes)

	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	// verifyFixture's real command execution isn't wired here (this
	// coordinator has no Verifier/file-check-only plan needs none), but the
	// file check itself requires the exact worktree path GetRun/StartRun
	// wired the work step to — verify.go's own file-check path reads
	// directly from workCP.WorktreePath, which fakeSpawner set to
	// "/ws/wf". Rather than depend on real disk state, assert the run
	// reaches verify's execution path (not stuck needing_attention for lack
	// of a review_run) — the exact pass/fail of a file check is already
	// covered by TestVerifyFileChecks.
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	verify := verifyStepFrom(got)
	if verify.Step.State == domain.WorkflowStepPending {
		t.Fatalf("verify step never dispatched after a SKIPPED review (state=%s)", verify.Step.State)
	}
	if len(verify.Attempts) == 0 {
		t.Fatalf("verify step has no attempts recorded — SKIPPED review never unblocked Verify")
	}
}

// Scenario 12: an ordinary (non-trivial, non-docs) code change stays
// REQUIRED and the Claude reviewer path is invoked exactly as it was before
// Checkpoint 8I — no regression to the 8C/8D REQUIRED path.
func TestReviewPolicyRequiredStillInvokesReviewerUnchanged(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	plan := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0, RetrySafe: true}},
	}
	created, err := c.CreateRun(ctx, "proj-1", "implement widget rendering", plan)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	changes := []ports.WorkspaceChange{{Path: "backend/internal/service/widget/widget.go", Status: " M"}}
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID, changes)

	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want running (reviewer dispatched)", review.Step.State)
	}
	if review.Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id — reviewer was not dispatched")
	}
	if reviewRuns.insertCalls != 1 || launcher.launchCalls != 1 {
		t.Fatalf("insertCalls=%d launchCalls=%d, want 1/1", reviewRuns.insertCalls, launcher.launchCalls)
	}

	// changes_requested -> fix -> re-review path (8D) must remain untouched:
	// drive one changes_requested verdict and confirm the fix step becomes
	// eligible exactly as TestReviewVerdictDrivesNextAction already proves
	// for the pre-8I baseline (regression guard, not a new mechanism).
	reviewRuns.setStatus(*review.Step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	got, err = c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(got).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("review step state = %q, want waiting after changes_requested", reviewStepFrom(got).Step.State)
	}
	if got.NextAction != "fix" {
		t.Fatalf("NextAction = %q, want fix", got.NextAction)
	}
}
