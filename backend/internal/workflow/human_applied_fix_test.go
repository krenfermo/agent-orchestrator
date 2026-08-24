package workflow_test

// Checkpoint 8P-E.22 — the transition that was missing from wf-c23a4b0c.
//
// Durable state of the real incident, reproduced by every fixture here:
//
//	run=needs_attention  reason=fix_budget_exhausted
//	work=completed  review=waiting  fix=waiting  verify=pending
//	four review cycles, all changes_requested, max_fix_cycles=3
//
// A person then applied the blocking finding themselves — which is what AO's
// own advice for that stop tells them to do — and POST /continue answered 200
// and did nothing, because a cycle-N+1 review is gated on the FIX STEP
// recording a new fingerprint and only a fix worker writes one.
//
// What these tests pin is not "the run resumes". It is that the resume costs
// nothing it should not: no extra budget, no fabricated fix attempt, no
// skipped reviewer, and exactly one fresh review per fingerprint however many
// times anyone presses the button.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// budgetExhaustedFixture reproduces the real stop, durably.
type budgetExhaustedFixture struct {
	*fixRecoveryFixture
	reviewRunID string
	oldFP       string
}

func newBudgetExhaustedFixture(t *testing.T) *budgetExhaustedFixture {
	t.Helper()
	f := newFixRecoveryFixture(t)
	got := f.driveToFixDispatch()
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID

	// The content the last review judged, and the stop it produced.
	oldFP := f.workspaceFingerprint()
	f.reviewRuns.runs[reviewRunID] = withTargetSHA(f.reviewRuns.runs[reviewRunID], oldFP)
	f.parkAsFixStop(workflowcore.ReasonFixBudgetExhausted,
		"the reviewer still requests changes after 4 review cycles (max_fix_cycles=3)")
	// The worker has been silent since: nothing of AO's is still moving.
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now().Add(-time.Hour)}
		rec.TurnCompletedAt = f.clk.Now().Add(-time.Hour)
	})
	f.clk.Advance(time.Minute)
	return &budgetExhaustedFixture{fixRecoveryFixture: f, reviewRunID: reviewRunID, oldFP: oldFP}
}

// applyExternalFix changes the workspace the way a person editing the worktree
// does: new content, and nothing of AO's involved.
func (f *budgetExhaustedFixture) applyExternalFix(t *testing.T, marker string) string {
	t.Helper()
	f.workspaceFacts.obs.HeadSHA = marker
	f.clk.Advance(time.Minute)
	return f.workspaceFingerprint()
}

// The transition itself.
func TestHumanAppliedFixReopensAnIndependentReview(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	newFP := f.applyExternalFix(t, "human-edited-tree")
	f.launcher.launchCalls = 0

	got := f.continueRun()

	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q: Continue was still a no-op", f.runState())
	}
	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed rows = %d, want exactly 1", n)
	}
	// A fresh reviewer was launched against the NEW content.
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 fresh independent review", f.launcher.launchCalls)
	}
	if st := reviewStepFrom(got).Step.State; st != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want running", st)
	}
	// The record says what AO observed, and no more.
	note := f.latestCheckpointPhaseNextAction("human_applied_fix_observed")
	if !strings.Contains(note, "no fix cycle was consumed") {
		t.Fatalf("record does not state that no fix cycle was consumed: %q", note)
	}
	rec := f.humanFixRecord(t)
	if rec["oldFingerprint"] != f.oldFP || rec["newFingerprint"] != newFP {
		t.Fatalf("record fingerprints = %v, want old %s new %s", rec, f.oldFP, newFP)
	}
	if rec["previousReviewRunId"] != f.reviewRunID {
		t.Fatalf("record previousReviewRunId = %v, want %s", rec["previousReviewRunId"], f.reviewRunID)
	}
	// AO must not claim it knows who did it.
	attribution, _ := rec["attribution"].(string)
	if !strings.Contains(attribution, "cannot attribute") {
		t.Fatalf("attribution = %q, want it to decline to name an author", attribution)
	}
}

// The budget is not raised and no fix attempt is invented: the ledger must never
// claim an agent did work a person did.
func TestHumanAppliedFixSpendsNoBudgetAndFabricatesNoAttempt(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	attemptsBefore := f.fixAttempts()
	policyBefore := f.policySnapshot(t)
	f.applyExternalFix(t, "human-edited-tree")
	// The fixture's own cycle-1 dispatch already sent one prompt; this test is
	// about what the RECOVERY sends, which must be nothing.
	f.sender.calls = 0

	f.continueRun()

	if got := f.fixAttempts(); got != attemptsBefore {
		t.Fatalf("fix attempts = %d, want still %d: no fix cycle may be fabricated", got, attemptsBefore)
	}
	if got := f.policySnapshot(t); got != policyBefore {
		t.Fatalf("the run's policy snapshot changed; the fix budget must not be raised")
	}
	if f.sender.calls != 0 {
		t.Fatalf("fix prompts sent = %d, want 0: this is not a fix cycle", f.sender.calls)
	}
}

// One fingerprint, at most one fresh review — across repeated Continues, polls
// and a restart.
func TestHumanAppliedFixIsIdempotentPerFingerprint(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.applyExternalFix(t, "human-edited-tree")
	f.launcher.launchCalls = 0

	for i := 0; i < 5; i++ {
		if i == 2 {
			f.c = f.newCoordinator() // a daemon restart mid-recovery
		}
		f.continueRun()
		f.poll(2)
	}
	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed rows = %d, want exactly 1 across five Continues and a restart", n)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 for one fingerprint", f.launcher.launchCalls)
	}
}

// ---- the refusals -----------------------------------------------------------

// An unchanged workspace is not a fix. This is the guard that stops Continue
// from becoming "re-review on demand".
func TestUnchangedWorkspaceIsNotAHumanAppliedFix(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.launcher.launchCalls = 0

	f.continueRun()

	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 0 {
		t.Fatalf("observed rows = %d, want 0 for an unchanged workspace", n)
	}
	if f.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", f.launcher.launchCalls)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want it still stopped", f.runState())
	}
}

// A worker that has been active since the stop makes the change ambiguous: it
// may be that agent's delivery still landing, and adopting it would credit a
// person for an agent's work.
func TestActiveAgentSinceTheStopBlocksTheRecovery(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.applyExternalFix(t, "human-edited-tree")
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: f.clk.Now()}
	})
	f.launcher.launchCalls = 0

	f.continueRun()

	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 0 {
		t.Fatalf("observed rows = %d, want 0 while this run's own agent is active", n)
	}
	if f.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", f.launcher.launchCalls)
	}
}

// A run stopped for anything else is untouched.
func TestHumanAppliedFixOnlyAppliesToABudgetExhaustedStop(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.parkAsFixStop(workflowcore.ReasonFixWorkerBlocked, "the fix worker is waiting on input")
	f.workspaceFacts.obs.HeadSHA = "changed-anyway"
	f.launcher.launchCalls = 0

	f.continueRun()

	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 0 {
		t.Fatalf("observed rows = %d, want 0 for an unrelated stop", n)
	}
}

// A workspace AO cannot read is missing evidence, never evidence of a fix.
func TestUnreadableWorkspaceBlocksTheRecovery(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.workspaceFacts.err = errUnreadableWorkspace
	f.launcher.launchCalls = 0

	f.continueRun()

	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 0 {
		t.Fatalf("observed rows = %d, want 0 when the workspace cannot be read", n)
	}
}

// Bounded: a run corrected by hand over and over is not helped by a fourth
// silent re-review.
func TestHumanAppliedFixRecoveriesAreBounded(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.launcher.launchCalls = 0

	for i := 0; i < 6; i++ {
		f.applyExternalFix(t, "human-edit-"+string(rune('a'+i)))
		// Put the run back in the stopped shape each round, as a fresh
		// changes_requested + budget-exhausted cycle would.
		f.parkAsFixStop(workflowcore.ReasonFixBudgetExhausted, "the reviewer still requests changes")
		f.mutateSession(func(rec *domain.SessionRecord) {
			rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now().Add(-time.Hour)}
			rec.TurnCompletedAt = f.clk.Now().Add(-time.Hour)
		})
		f.continueRun()
	}
	if n := f.countCheckpointPhase("human_applied_fix_observed"); n > 3 {
		t.Fatalf("observed rows = %d, want at most 3 (maxHumanAppliedFixRecoveries)", n)
	}
}

// ---- fixture helpers --------------------------------------------------------

var errUnreadableWorkspace = &workspaceReadError{}

type workspaceReadError struct{}

func (e *workspaceReadError) Error() string { return "workspace cannot be read" }

func (f *fixRecoveryFixture) workspaceFingerprint() string {
	obs, err := f.workspaceFacts.ObserveWorkspace(context.Background(), ports.WorkspaceInfo{Path: "/ws/wf"})
	if err != nil {
		return ""
	}
	return workflowcore.WorkspaceFingerprint(obs)
}

func (f *fixRecoveryFixture) policySnapshot(t *testing.T) string {
	t.Helper()
	run, ok, err := f.store.GetWorkflowRun(context.Background(), f.runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	return run.PolicySnapshot
}

// humanFixRecord decodes the durable record so the test asserts what was
// actually persisted rather than what the code intended.
func (f *fixRecoveryFixture) humanFixRecord(t *testing.T) map[string]any {
	t.Helper()
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase != "human_applied_fix_observed" {
			continue
		}
		var out map[string]any
		if err := jsonUnmarshalString(cp.RetryState, &out); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		return out
	}
	t.Fatal("no human_applied_fix_observed record was written")
	return nil
}

func withTargetSHA(r domain.ReviewRun, sha string) domain.ReviewRun {
	r.TargetSHA = sha
	return r
}

// ---- helpers this branch's fixture does not already provide -----------------

// parkAsFixStop writes the durable shape a fix stop leaves behind: the fix and
// review steps resting at waiting, the run in needs_attention, and the reason
// recorded as the run's newest checkpoint — where stopReason reads it from.
func (f *fixRecoveryFixture) parkAsFixStop(reason, detail string) {
	f.t.Helper()
	ctx := context.Background()
	steps, err := f.store.ListWorkflowSteps(ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	f.clk.Advance(time.Second)
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepFix && s.Kind != domain.WorkflowStepReview {
			continue
		}
		if s.State == domain.WorkflowStepRunning || s.State == domain.WorkflowStepReady {
			if _, err := f.store.UpdateWorkflowStepState(ctx, s.ID, s.State, domain.WorkflowStepWaiting, f.clk.Now()); err != nil {
				f.t.Fatalf("park %s step: %v", s.Kind, err)
			}
		}
	}
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		if _, err := f.store.UpdateWorkflowRunState(ctx, f.runID, run.State, domain.WorkflowRunNeedsAttention, f.clk.Now()); err != nil {
			f.t.Fatalf("park run: %v", err)
		}
	}
	f.clk.Advance(time.Second)
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-stop-" + reason + f.clk.Now().Format("150405.000000000"), WorkflowRunID: f.runID,
		ProjectID: run.ProjectID, NextAction: detail, DurablePhase: reason,
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("CreateWorkflowCheckpoint: %v", err)
	}
}

func (f *fixRecoveryFixture) continueRun() workflowcore.RunDetail {
	f.t.Helper()
	f.clk.Advance(2 * time.Second)
	got, err := f.c.ContinueRun(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("ContinueRun: %v", err)
	}
	return got
}

func (f *fixRecoveryFixture) fixAttempts() int {
	f.t.Helper()
	attempts, err := f.store.ListWorkflowAttempts(context.Background(), f.fixStepID)
	if err != nil {
		f.t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	return len(attempts)
}

func jsonUnmarshalString(raw string, out any) error {
	return json.Unmarshal([]byte(raw), out)
}
