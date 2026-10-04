package workflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// verifiablePlan is a verification plan with one command, so
// EvaluateReviewPolicy sees real deterministic coverage and an ordinary code
// change lands on ReasonDefaultConservative (the standard tier) rather than on
// ReasonInsufficientVerify.
func verifiablePlan() workflowcore.VerificationPlan {
	return workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"test", "./pkg/..."}, RetrySafe: true},
		},
	}
}

// depthDecisionsFor folds a run's checkpoint stream into the depth decisions it
// recorded, oldest first, so a test can assert both what was decided and how
// many times it was decided.
func depthDecisionsFor(t *testing.T, store *fakeStore, runID string) []domain.ReviewDepthDecision {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var out []domain.ReviewDepthDecision
	for _, cp := range cps {
		if cp.DurablePhase != "review_depth_decision" && cp.DurablePhase != "review_depth_escalated" {
			continue
		}
		decision, ok := workflowcore.DecodeReviewDepthDecisionForTest(cp.RetryState)
		if !ok {
			t.Fatalf("%s checkpoint is not decodable: %q", cp.DurablePhase, cp.RetryState)
		}
		out = append(out, decision)
	}
	return out
}

func countDepthCheckpoints(t *testing.T, store *fakeStore, runID, phase string) int {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

// A fast Task asks for no reviewer. An ordinary code change is standard risk,
// so AO raises it to a BOUNDED review rather than honouring the request or
// falling back to a full one — and the reviewer that launches gets the light
// prompt, with AO's own observations in it.
func TestFastTaskGetsABoundedReviewNotAFullOne(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A task run's frozen request is "none" — the strategy default.
	frozen, err := c.RunReviewDepthPolicy(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("RunReviewDepthPolicy: %v", err)
	}
	if frozen.Requested != domain.ReviewDepthNone {
		t.Fatalf("a task run froze requested depth %q, want none", frozen.Requested)
	}

	changes := []ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}}
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID, changes)
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	// A reviewer DID launch: "none" was not honoured for a change AO cannot
	// prove safe on its own.
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatal("no reviewer ran for a standard-risk change requested at depth none")
	}
	if launcher.launchCalls == 0 {
		t.Fatal("the reviewer was never launched")
	}

	decisions := depthDecisionsFor(t, store, created.Run.ID)
	if len(decisions) != 1 {
		t.Fatalf("recorded %d depth decisions, want exactly 1: %+v", len(decisions), decisions)
	}
	d := decisions[0]
	if d.Requested != domain.ReviewDepthNone || d.Effective != domain.ReviewDepthLight {
		t.Fatalf("requested/effective = %q/%q, want none/light", d.Requested, d.Effective)
	}
	if d.Source != domain.ReviewDepthClamped || d.Reason != domain.ReviewDepthReasonClampedByRiskTier {
		t.Errorf("source/reason = %q/%q, want clamped/clamped_by_risk_tier", d.Source, d.Reason)
	}
	if d.RiskTier != domain.ReviewRiskStandard {
		t.Errorf("risk tier = %q, want standard", d.RiskTier)
	}
	if d.PolicyVersion != domain.ReviewDepthPolicyVersion {
		t.Errorf("policy version = %q", d.PolicyVersion)
	}

	// And the reviewer actually received the bounded prompt, carrying AO's own
	// observation of the change rather than the worker's account of it.
	for _, want := range []string{
		"This is a BOUNDED review",
		"pkg/helper.go",
		"go test ./pkg/...",
		"is NOT evidence",
		domain.ReviewEscalationMarker,
	} {
		if !strings.Contains(launcher.lastPrompt, want) {
			t.Errorf("the dispatched prompt is missing %q", want)
		}
	}
}

// The non-degradability guarantee, end to end: a run that explicitly asked for
// no reviewer at all still gets a FULL independent review when the delivered
// change touches an authentication path, and the ledger says the clamp is why.
func TestHighRiskChangeCannotBeDegradedByAnExplicitRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"auth", "internal/auth/token.go"},
		{"payments", "internal/billing/stripe_webhook.go"},
		{"migration", "backend/internal/storage/sqlite/migrations/0042_add.sql"},
		{"infrastructure", ".github/workflows/ci.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionFacts := newFakeSessionFacts()
			spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
			workspaceFacts := &fakeWorkspaceFacts{}
			reviewRuns := newFakeReviewRuns()
			launcher := &fakeReviewerLauncher{}
			c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
			ctx := context.Background()

			created, err := c.CreateRun(ctx, "proj-1", "adjust the thing", verifiablePlan())
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			// The most degrading request a caller can make, made explicitly.
			if err := c.ApplyReviewDepthPolicy(ctx, created.Run.ID, domain.ReviewDepthNone); err != nil {
				t.Fatalf("ApplyReviewDepthPolicy: %v", err)
			}

			changes := []ports.WorkspaceChange{{Path: tc.path, Status: " M"}}
			completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID, changes)
			if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}

			decisions := depthDecisionsFor(t, store, created.Run.ID)
			if len(decisions) != 1 {
				t.Fatalf("recorded %d depth decisions, want 1", len(decisions))
			}
			d := decisions[0]
			if d.Effective != domain.ReviewDepthDeep {
				t.Fatalf("effective depth = %q for %s, want deep", d.Effective, tc.path)
			}
			if d.RiskTier != domain.ReviewRiskHigh {
				t.Errorf("risk tier = %q, want high", d.RiskTier)
			}
			if d.Source != domain.ReviewDepthClamped {
				t.Errorf("source = %q, want clamped", d.Source)
			}
			if strings.Contains(launcher.lastPrompt, "This is a BOUNDED review") {
				t.Error("a high-risk change was dispatched with the bounded prompt")
			}
		})
	}
}

// The depth is frozen: once a run is moving, the request cannot be lowered
// under a review that is already in flight.
func TestReviewDepthRequestIsFrozenOnceTheRunStarts(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID,
		[]ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}})

	if err := c.ApplyReviewDepthPolicy(ctx, created.Run.ID, domain.ReviewDepthNone); err == nil {
		t.Fatal("lowering the depth of a running run was accepted; it must be refused")
	}
	// And an unrecognised depth is refused outright rather than normalised into
	// something nobody asked for.
	fresh, err := c.CreateRun(ctx, "proj-1", "another", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := c.ApplyReviewDepthPolicy(ctx, fresh.Run.ID, domain.ReviewDepth("skim")); err == nil {
		t.Fatal("an unrecognised depth was accepted")
	}
}

// A bounded reviewer that says it is out of its depth raises the step so the
// NEXT cycle is a full independent review. The escalation is durable and the
// verdict still takes the ordinary changes_requested path.
func TestReviewerEscalationMarkerDeepensTheNextCycle(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID,
		[]ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}})
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID

	// The reviewer submits its verdict out of band, as the real CLI does, and
	// uses the escalation channel.
	run := reviewRuns.runs[reviewRunID]
	run.Status = domain.ReviewRunComplete
	run.Verdict = domain.VerdictChangesRequested
	run.Body = domain.ReviewEscalationMarker + " this turns out to touch the session token path\n\nDetails follow."
	reviewRuns.runs[reviewRunID] = run
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	// The verdict itself takes the ordinary path: findings go to a fix.
	if final.NextAction != "fix" {
		t.Fatalf("next action = %q, want fix — an escalation must not become a second way for a review to end", final.NextAction)
	}
	if countDepthCheckpoints(t, store, created.Run.ID, "review_depth_escalated") != 1 {
		t.Fatal("no durable escalation was recorded")
	}
	depth := c.EffectiveReviewDepth(ctx, final.Run, reviewStepFrom(final).Step)
	if depth.Effective != domain.ReviewDepthDeep {
		t.Fatalf("effective depth after escalation = %q, want deep", depth.Effective)
	}
	if depth.Reason != domain.ReviewDepthReasonEscalatedByReviewer {
		t.Errorf("reason = %q, want escalated_by_reviewer", depth.Reason)
	}

	// Idempotent: observing the same verdict again writes no second row.
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if n := countDepthCheckpoints(t, store, created.Run.ID, "review_depth_escalated"); n != 1 {
		t.Fatalf("re-observing the verdict wrote %d escalation rows, want 1", n)
	}
}

// An ordinary changes_requested does NOT escalate: the marker is the channel,
// not any request for changes.
func TestOrdinaryChangesRequestedDoesNotEscalate(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID,
		[]ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}})
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	run := reviewRuns.runs[reviewRunID]
	run.Status, run.Verdict, run.Body = domain.ReviewRunComplete, domain.VerdictChangesRequested, "The helper is renamed but its callers are not."
	reviewRuns.runs[reviewRunID] = run
	clk.Advance(time.Second)

	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if n := countDepthCheckpoints(t, store, created.Run.ID, "review_depth_escalated"); n != 0 {
		t.Fatalf("an ordinary changes_requested escalated (%d rows)", n)
	}
}

// A review that ends without a verdict — the reviewer died, ran out of time, or
// was cancelled — must never be read as an approval, at any depth. Exhaustion
// is not consent.
func TestALightReviewThatEndsWithoutAVerdictNeverApproves(t *testing.T) {
	for _, status := range []domain.ReviewRunStatus{
		domain.ReviewRunFailed, domain.ReviewRunCancelled, domain.ReviewRunComplete,
	} {
		t.Run(string(status), func(t *testing.T) {
			sessionFacts := newFakeSessionFacts()
			spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
			workspaceFacts := &fakeWorkspaceFacts{}
			reviewRuns := newFakeReviewRuns()
			launcher := &fakeReviewerLauncher{}
			c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
			ctx := context.Background()

			created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID,
				[]ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}})
			got, err := c.ContinueRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}
			// The review ran light, and then ended with no verdict at all.
			reviewRuns.setStatus(*reviewStepFrom(got).Step.ReviewRunID, status, domain.VerdictNone)
			clk.Advance(time.Second)

			final, err := c.GetRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if final.NextAction == "verify" {
				t.Fatalf("a review that ended as %q advanced the run to verify", status)
			}
			if reviewStepFrom(final).Step.State == domain.WorkflowStepCompleted {
				t.Fatalf("a review that ended as %q completed the review step", status)
			}
		})
	}
}

// The depth decision is decided ONCE and read back afterwards, never
// recomputed: repeated observation, and a "restart" that rebuilds the
// coordinator over the same store, must both reach the same recorded answer
// without writing a second decision.
func TestReviewDepthIsDecidedOnceAndSurvivesRestart(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID,
		[]ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}})
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	for i := 0; i < 3; i++ {
		clk.Advance(time.Second)
		if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}
	if n := countDepthCheckpoints(t, store, created.Run.ID, "review_depth_decision"); n != 1 {
		t.Fatalf("recorded %d depth decisions across repeated observation, want 1", n)
	}

	// A daemon restart: a brand-new coordinator over the same durable store.
	restarted := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Clock: clk.Now,
		NewID: func() string { return "restart-id" },
	})
	after, err := restarted.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	depth := restarted.EffectiveReviewDepth(ctx, after.Run, reviewStepFrom(after).Step)
	if depth.Effective != domain.ReviewDepthLight {
		t.Fatalf("effective depth after restart = %q, want light", depth.Effective)
	}
	if n := countDepthCheckpoints(t, store, created.Run.ID, "review_depth_decision"); n != 1 {
		t.Fatalf("a restart wrote a second depth decision (%d rows)", n)
	}
}

// A cancelled run must not keep deciding anything, and the depth already
// recorded stays readable afterwards — cancellation ends work, it does not
// erase the ledger.
func TestCancellingARunLeavesItsDepthDecisionIntact(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID,
		[]ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}})
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	before := countDepthCheckpoints(t, store, created.Run.ID, "review_depth_decision")

	if _, err := c.CancelRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	clk.Advance(time.Second)
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun after cancel: %v", err)
	}
	if after := countDepthCheckpoints(t, store, created.Run.ID, "review_depth_decision"); after != before {
		t.Fatalf("depth decisions %d -> %d across cancellation, want unchanged", before, after)
	}
	decisions := depthDecisionsFor(t, store, created.Run.ID)
	if len(decisions) == 0 || decisions[0].Effective != domain.ReviewDepthLight {
		t.Fatalf("the recorded decision did not survive cancellation: %+v", decisions)
	}
}

// A skipped review still records the depth it would have run at, so the ledger
// has no gap where a review used to be.
func TestASkippedReviewStillRecordsItsDepth(t *testing.T) {
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
	completeWorkStepWithChanges(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID,
		[]ports.WorkspaceChange{{Path: "docs/notes.md", Status: "??"}})
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if launcher.launchCalls != 0 {
		t.Fatalf("a reviewer launched for a policy-skipped review (%d launches)", launcher.launchCalls)
	}
	decisions := depthDecisionsFor(t, store, created.Run.ID)
	if len(decisions) != 1 {
		t.Fatalf("recorded %d depth decisions for a skipped review, want 1", len(decisions))
	}
	if decisions[0].Effective != domain.ReviewDepthNone || decisions[0].RiskTier != domain.ReviewRiskLow {
		t.Fatalf("effective/tier = %q/%q, want none/low", decisions[0].Effective, decisions[0].RiskTier)
	}
}
