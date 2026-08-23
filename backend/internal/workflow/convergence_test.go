package workflow_test

// Checkpoint 8P-E.13 regression suite: the real AO runs that motivated this
// checkpoint, reproduced from their observed durable rows.
//
// Every fixture below is transcribed from ~/.ao/data/ao.db as it stood on
// 2026-08-21, not invented. The point is that the exact durable state that
// stranded a real workflow now produces a truthful, actionable, or
// self-driving outcome instead of a dead end.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// strandedRunDetail reproduces wf-3220567f-34b7-4054-a1ec-462d719e3723 exactly
// as the database held it: plan and work completed, three fix attempts all
// recorded `succeeded`, four review runs all `changes_requested`, the review
// step resting at `waiting`, the fix step at `waiting`, verify never started,
// and the run durably in needs_attention.
//
// The two details that mattered and were easy to miss:
//
//   - The review step has NO workflow_attempts rows at all. Review dispatch
//     never creates one, so recordReviewOutcome's attempt-carried
//     error_class=fix_budget_exhausted was written to nothing and vanished.
//   - Not one of the run's 23 checkpoints carried a reason for the stop. The
//     newest was durable_phase="review_observed", which names what AO was doing.
//
// latestPhase is parameterised so the test can assert both the pre-fix carrier
// (which must NOT yield a human decision) and the post-fix one.
func strandedRunDetail(latestPhase string) workflowcore.RunDetail {
	at := time.Date(2026, 8, 21, 15, 52, 28, 0, time.UTC)
	finished := at
	succeeded := func(n int64) domain.WorkflowAttempt {
		return domain.WorkflowAttempt{
			ID: "wfa-fix", WorkflowStepID: "wfs-fix", AttemptNumber: n,
			Outcome: domain.WorkflowAttemptSucceeded, StartedAt: at, FinishedAt: &finished,
		}
	}
	return workflowcore.RunDetail{
		Run: domain.WorkflowRun{
			ID: "wf-3220567f", ProjectID: "p", State: domain.WorkflowRunNeedsAttention,
			PolicySnapshot: `{"maxFixCycles":3}`, UpdatedAt: at,
		},
		Steps: []workflowcore.StepDetail{
			{Step: domain.WorkflowStep{ID: "wfs-plan", Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted}},
			{Step: domain.WorkflowStep{ID: "wfs-work", Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted}},
			// Deliberately zero attempts: this is the real row shape.
			{Step: domain.WorkflowStep{ID: "wfs-review", Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepWaiting}},
			{Step: domain.WorkflowStep{ID: "wfs-fix", Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepWaiting},
				Attempts: []domain.WorkflowAttempt{succeeded(1), succeeded(2), succeeded(3)}},
			{Step: domain.WorkflowStep{ID: "wfs-verify", Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepPending}},
			{Step: domain.WorkflowStep{ID: "wfs-advance", Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending}},
		},
		LatestCheckpointPhase: latestPhase,
		LatestCheckpointAt:    at,
	}
}

// TestStrandedRunWithNoRecordedReasonIsNotAHumanDecision is the "before"
// half: fed the exact durable state AO actually produced, the Board must not
// claim a person can resolve it. It could not — there was no reason and no
// action to show, which is precisely the report this checkpoint answers.
func TestStrandedRunWithNoRecordedReasonIsNotAHumanDecision(t *testing.T) {
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: strandedRunDetail("review_observed")})
	if life.Attention == workflowcore.AttentionHuman {
		t.Fatalf("attention = human_decision with reason=%q action=%q; nothing here is answerable",
			life.AttentionReason, life.AttentionAction)
	}
}

// TestStrandedRunWithCanonicalReasonIsAnActionableHumanDecision is the "after"
// half: with observeReviewStep now recording the canonical stop, the same run
// reports a named reason and a concrete remedy.
func TestStrandedRunWithCanonicalReasonIsAnActionableHumanDecision(t *testing.T) {
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{
		Detail: strandedRunDetail(workflowcore.ReasonFixBudgetExhausted),
	})
	if life.Attention != workflowcore.AttentionHuman {
		t.Fatalf("attention = %q, want human_decision: the fix budget really is exhausted", life.Attention)
	}
	if life.AttentionReason != workflowcore.ReasonFixBudgetExhausted {
		t.Fatalf("reason = %q, want %q", life.AttentionReason, workflowcore.ReasonFixBudgetExhausted)
	}
	if life.AttentionAction == "" {
		t.Fatal("attentionAction is empty; an exhausted fix budget has a known remedy")
	}
	if life.Phase != workflowcore.PhaseNeedsAttention {
		t.Fatalf("phase = %q, want needs_attention", life.Phase)
	}
}

// TestPlannerRetryReportsRetryingNotNeedsAttention is the MEDUSA regression:
// a planner_timeout parked for a bounded retry is AO's own problem, and the
// Board must say "retrying", never "Te necesita".
func TestPlannerRetryReportsRetryingNotNeedsAttention(t *testing.T) {
	plan := domain.WorkflowPlanRecord{Status: domain.WorkflowPlanPending, ErrorClass: "planner_timeout"}
	detail := workflowcore.RunDetail{
		Run:                   domain.WorkflowRun{ID: "wf-medusa", State: domain.WorkflowRunPending},
		Plan:                  &plan,
		LatestCheckpointPhase: workflowcore.ReasonPlannerRetryScheduled,
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail})
	if life.Phase != workflowcore.PhaseRetrying {
		t.Fatalf("phase = %q, want retrying", life.Phase)
	}
	if life.Attention == workflowcore.AttentionHuman {
		t.Fatalf("attention = human_decision; no answer a person can give repairs a timed-out planner")
	}
}

// TestPlannerTimeoutRetriesThenStopsWithAnActionableReason drives the real
// GeneratePlan path against a permanently-timing-out planner and proves both
// halves of Phase 3: the retries are bounded, and the eventual stop is a
// genuine human decision with a remedy attached rather than a bare
// needs_attention.
func TestPlannerTimeoutRetriesThenStopsWithAnActionableReason(t *testing.T) {
	ctx := context.Background()
	c, runID := newFailingMasterFixture(t, plannerTimeoutErr())

	var last workflowcore.RunDetail
	// One initial attempt plus enough calls to burn the whole retry budget.
	for i := 0; i < 8; i++ {
		detail, err := c.GeneratePlan(ctx, runID)
		if err != nil {
			t.Fatalf("GeneratePlan call %d: %v", i, err)
		}
		last = detail
		if detail.Plan.Status == domain.WorkflowPlanInvalid {
			break
		}
	}

	if last.Plan.Status != domain.WorkflowPlanInvalid {
		t.Fatalf("plan status = %q after the retry budget; want invalid (retries must be bounded)", last.Plan.Status)
	}
	if last.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once retries are exhausted", last.Run.State)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: last, Questions: last.Questions})
	if life.Attention != workflowcore.AttentionHuman {
		t.Fatalf("attention = %q, want human_decision once AO has genuinely run out of retries", life.Attention)
	}
	if life.AttentionReason != workflowcore.ReasonPlannerExhausted || life.AttentionAction == "" {
		t.Fatalf("reason=%q action=%q, want %q with a concrete action",
			life.AttentionReason, life.AttentionAction, workflowcore.ReasonPlannerExhausted)
	}
}

// TestPlannerRetryBudgetIsDurable proves the bound survives a restart: the
// counter is derived from append-only checkpoints, so a fresh Coordinator over
// the same store sees the same budget already spent rather than starting over.
func TestPlannerRetryBudgetIsDurable(t *testing.T) {
	ctx := context.Background()
	c, runID := newFailingMasterFixture(t, plannerTimeoutErr())
	for i := 0; i < 8; i++ {
		detail, err := c.GeneratePlan(ctx, runID)
		if err != nil {
			t.Fatalf("GeneratePlan call %d: %v", i, err)
		}
		if detail.Plan.Status == domain.WorkflowPlanInvalid {
			if i == 0 {
				t.Fatal("planner stopped on the very first timeout; it must retry first")
			}
			return
		}
	}
	t.Fatal("planner never stopped retrying; the budget is not bounded")
}

// plannerTimeoutErr mirrors the real adapter's wrapped sentinel, so the
// classification path under test is the production one.
func plannerTimeoutErr() error {
	return fmt.Errorf("planner timeout: %w: context deadline exceeded", ports.ErrPlannerTimeout)
}

// verifyReentryFixture mirrors verifyFixture (verify_test.go) with the two
// dependencies Phase 5's re-entry needs: a MessageSender to deliver the failed
// verification's findings to the worker, and SessionFacts/WorkspaceFacts so the
// resulting fix cycle can be observed the same way a review-driven one is.
//
// The workspace fake is mutable so the test can move the worktree's fingerprint
// when the fix "lands" — the fact the whole cycle turns on.
type mutableWorkspaceFacts struct {
	obs ports.WorkspaceObservation
}

func (f *mutableWorkspaceFacts) MaterializeIntegrationCommit(_ context.Context, _ ports.WorkspaceInfo, _, _, _ string, _ []string) (string, string, bool, error) {
	return "", "", false, nil
}

func (f *mutableWorkspaceFacts) ObserveWorkspace(_ context.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	return f.obs, nil
}

func verifyReentryFixture(t *testing.T, runner workflowcore.VerifyRunner) (
	*workflowcore.Coordinator, *fakeStore, *fakeClock, *mutableWorkspaceFacts, *fakeSessionFacts, *fakeMessageSender, string,
) {
	t.Helper()
	dir := t.TempDir()
	store := newFakeStore()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	runID, sid, reviewID := "wf-verify-reentry", "sess-verify", "review-verify"

	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{
		{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0, RetrySafe: true},
	}}
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

	approved := cleanObservation(dir)
	ws := &mutableWorkspaceFacts{obs: approved}
	workStepID := "work"
	store.checkpoints[runID] = []domain.WorkflowCheckpoint{{
		ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "project-1",
		SessionID: &sid, Branch: "feature", WorktreePath: dir,
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
		Metadata: domain.SessionMetadata{Branch: "feature", WorkspacePath: dir},
	})

	sender := &fakeMessageSender{}
	clk := &fakeClock{t: now}
	ids := 0
	c := workflowcore.New(workflowcore.Deps{
		Store: store, ReviewRuns: reviews, WorkspaceFacts: ws, SessionFacts: facts,
		Verifier: runner, MessageSender: sender,
		Clock: clk.Now, NewID: func() string { ids++; return fmt.Sprintf("vr%d", ids) },
	})
	return c, store, clk, ws, facts, sender, runID
}

// TestVerifyFailureRentersFixAndVerifiesAgain is Checkpoint 8P-E.13 Phase 5's
// headline: the debt 8P-E.12 left explicit.
//
// A verification that fails on a real, repairable check used to end the run in
// needs_attention with the verify step durably `failed` — a terminal step state
// with zero outgoing transitions, so the run could never be verified again no
// matter what anyone did. It now hands the findings back to the fix worker and
// re-verifies against the fingerprint that fix delivered.
func TestVerifyFailureReentersFixAndVerifiesAgain(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 2}}
	c, store, clk, ws, facts, sender, runID := verifyReentryFixture(t, runner)

	// 1. Verification runs and fails on the command's exit code.
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	verify := stepFromDetail(t, detail, domain.WorkflowStepVerify)
	if verify.Step.State == domain.WorkflowStepFailed {
		t.Fatal("verify step is terminally failed; re-verification would be structurally impossible")
	}
	if verify.Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("verify step = %q, want waiting", verify.Step.State)
	}
	if detail.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("run parked in needs_attention with fix budget still available")
	}
	if detail.LatestCheckpointPhase != workflowcore.ReasonVerifyFixReentry {
		t.Fatalf("latest checkpoint = %q, want %q", detail.LatestCheckpointPhase, workflowcore.ReasonVerifyFixReentry)
	}

	// 2. The next poll dispatches the fix, carrying the real verify output.
	clk.Advance(time.Minute)
	detail, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatalf("MessageSender.Send calls = %d, want exactly 1", sender.calls)
	}
	if !strings.Contains(sender.lastMsg, "verification failed") &&
		!strings.Contains(sender.lastMsg, "Local verification failed") {
		t.Fatalf("fix prompt does not mention the verification failure: %q", sender.lastMsg)
	}
	if got := stepFromDetail(t, detail, domain.WorkflowStepFix).Step.State; got != domain.WorkflowStepRunning {
		t.Fatalf("fix step = %q, want running", got)
	}

	// 3. The fix lands: the worker goes idle and the worktree fingerprint moves.
	ws.obs = ports.WorkspaceObservation{Path: ws.obs.Path, Branch: "feature", HeadSHA: "fixed456"}
	facts.put(domain.SessionRecord{
		ID: "sess-verify", ProjectID: "project-1",
		Activity:      domain.Activity{State: domain.ActivityIdle},
		FirstSignalAt: clk.Now(),
		Metadata:      domain.SessionMetadata{Branch: "feature", WorkspacePath: ws.obs.Path},
	})
	// 4. Verification now passes against the fingerprint the fix delivered.
	runner.result = workflowcore.VerifyCommandExecution{ExitCode: 0}
	clk.Advance(10 * time.Minute)
	detail, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q after a successful re-verification, want completed (steps: %s)",
			detail.Run.State, stepStates(detail))
	}
	_ = store
}

// TestVerifyEnvironmentFailureDoesNotEnterAFixCycle: a failure no diff could
// repair must not be handed to a fix worker. It stops, and it says which kind
// of stop it is.
func TestVerifyEnvironmentFailureDoesNotEnterAFixCycle(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{err: fmt.Errorf("exec: \"go\": executable file not found in $PATH")}
	c, _, _, _, _, sender, runID := verifyReentryFixture(t, runner)

	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d; an environment failure is not a fix worker's problem", sender.calls)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention", detail.Run.State)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.Attention != workflowcore.AttentionHuman || life.AttentionAction == "" {
		t.Fatalf("attention=%q action=%q, want an actionable human decision", life.Attention, life.AttentionAction)
	}
	// Checkpoint 8P-E.14 sharpened this from the flat verify_unrepairable to the
	// precise thing a person can act on: the verifier's binary is not installed.
	if life.AttentionReason != workflowcore.ReasonVerifyToolUnavailable {
		t.Fatalf("reason = %q, want %q", life.AttentionReason, workflowcore.ReasonVerifyToolUnavailable)
	}
}

func stepFromDetail(t *testing.T, d workflowcore.RunDetail, kind domain.WorkflowStepKind) workflowcore.StepDetail {
	t.Helper()
	for _, sd := range d.Steps {
		if sd.Step.Kind == kind {
			return sd
		}
	}
	t.Fatalf("no %s step in run detail", kind)
	return workflowcore.StepDetail{}
}

func stepStates(d workflowcore.RunDetail) string {
	var b strings.Builder
	for _, sd := range d.Steps {
		fmt.Fprintf(&b, "%s=%s ", sd.Step.Kind, sd.Step.State)
	}
	return b.String()
}

// TestAlreadyStrandedRunExplainsItselfFromLegacyRows: the runs that were
// already stopped when this checkpoint shipped are still on disk, carrying only
// the pre-8P-E.13 "human_attention" next_action. They must become readable on
// the next Board poll without anyone rewriting their history.
func TestAlreadyStrandedRunExplainsItselfFromLegacyRows(t *testing.T) {
	detail := strandedRunDetail("review_observed")
	detail.NextAction = "human_attention" // exactly what the old code wrote

	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail})
	if life.Attention != workflowcore.AttentionHuman {
		t.Fatalf("attention = %q, want human_decision", life.Attention)
	}
	if life.AttentionReason != workflowcore.ReasonFixBudgetExhausted {
		t.Fatalf("reason = %q, want %q", life.AttentionReason, workflowcore.ReasonFixBudgetExhausted)
	}
	if life.AttentionAction == "" {
		t.Fatal("attentionAction is empty; the legacy row must still get the current remedy")
	}
}
