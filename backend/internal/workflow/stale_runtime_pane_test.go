package workflow_test

// A reviewer session AO can no longer classify, because its tmux pane will not
// name a process.
//
// Production evidence (wf-9405592d-b13a-4f02-b239-4826e4631cd2): tmux answered
// `#{pane_pid}` with an empty string for instance $34. The adapter raised it as
// an error, and because every consumer of that observation treated an error as
// fatal, ONE stale pane:
//
//   - aborted boot reconciliation for every other run on the machine,
//   - made GET /api/v1/workflows/{id} return 500 for that run, forever,
//   - and made the wake poller re-drive autonomous_progress into the identical
//     failure all night.
//
// The rule these tests pin: an unclassifiable runtime observation is scoped to
// the run it describes. It never adopts, never destroys, never fabricates -- and
// it never becomes anyone else's failure.

import (
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// staleReviewerPane puts the fixture in the exact production shape: a durable
// cancel intent that was never confirmed, over a reviewer whose runtime can no
// longer be classified.
func staleReviewerPane(t *testing.T, f *reviewAuthorityFixture) string {
	t.Helper()
	subject := f.authoritativeRunID()
	identity := "workflow-review-" + subject
	stepID := f.reviewStep().ID
	rid := subject
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-stale-cancel-intent", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", ReviewRunID: &rid, DurablePhase: "review_cancel_intent",
		PayloadVersion: "v1",
		RetryState:     `{"reviewRunId":"` + subject + `","handleId":"` + identity + `"}`,
		CreatedAt:      f.clk.Now(),
	}); err != nil {
		t.Fatalf("seed cancel intent: %v", err)
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("fixture has no live reviewer to lose track of")
	}
	// The pane stops naming its process. Everything AO can still ask returns
	// "cannot tell" -- and production refuses to kill on that.
	f.launcher.probeUnknown = true
	f.launcher.strictOwnership = true
	return identity
}

// (e) A WORKFLOW READ STAYS A READ. GET must answer with the run's state, not
// with the runtime's inability to classify a pane.
func TestStalePane_GetRunStaysReadableAndConvergesToNeedsAttention(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	identity := staleReviewerPane(t, f)

	// The frontend polls. Every one of these used to be a 500.
	for i := 0; i < 8; i++ {
		if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d returned an error for a run with an unclassifiable reviewer pane: %v", i, err)
		}
	}

	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: an obligation AO cannot classify must reach a person", got)
	}
	// FAIL CLOSED. Nothing was killed and nothing was adopted on an unanswered
	// probe -- the reviewer is exactly where it was.
	if f.launcher.cancelCalls != 0 {
		t.Fatalf("cancel attempts = %d, want 0 against a session AO cannot prove it owns", f.launcher.cancelCalls)
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("a reviewer AO could not classify was destroyed anyway")
	}
	// And the reason is on the ledger, not just in a log line.
	if !f.hasPhase("review_reviewer_unproven") {
		t.Fatalf("no durable evidence of the unprovable reviewer; phases = %v", f.checkpointPhases())
	}
}

// (i) REPEATED RESTARTS ARE IDEMPOTENT. A daemon that boots five times over the
// same stale pane must not launch anything, kill anything, or grow the ledger
// without end.
func TestStalePane_RepeatedDaemonRestartsAreIdempotent(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	identity := staleReviewerPane(t, f)
	launches := f.launcher.launchCalls

	for i := 0; i < 6; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	settled := len(f.checkpointPhases())
	state := f.run().State
	for i := 0; i < 4; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile (replay) %d: %v", i, err)
		}
	}

	if got := len(f.checkpointPhases()); got != settled {
		t.Fatalf("ledger grew from %d to %d rows across further restarts; the probe budget is not bounding it", settled, got)
	}
	if f.run().State != state {
		t.Fatalf("run state moved from %q to %q on a replay with no new evidence", state, f.run().State)
	}
	if f.launcher.launchCalls != launches {
		t.Fatalf("a replacement reviewer was launched over an unclassifiable one (%d launches)", f.launcher.launchCalls-launches)
	}
	if f.launcher.cancelCalls != 0 {
		t.Fatalf("cancel attempts = %d, want 0", f.launcher.cancelCalls)
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("the unclassifiable reviewer was destroyed by a restart")
	}
}

// (g) DURABLE EVIDENCE WINS. A review that actually concluded converges from its
// own durable record; the live pane is not consulted and is not needed.
func TestStalePane_DurableVerdictConvergesWithoutTheLivePane(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := f.authoritativeRunID()

	// The reviewer submitted, and only afterwards did its pane become
	// unreadable -- the overnight-stall shape, where the work was done and the
	// runtime fact was the only thing missing.
	f.submitVerdict(subject, domain.VerdictApproved)
	f.launcher.probeUnknown = true
	f.launcher.strictOwnership = true

	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed from the durable verdict", got)
	}
	if got := f.run().State; got == domain.WorkflowRunNeedsAttention {
		t.Fatal("a run with a durable approval was parked for a person because its pane went unreadable")
	}
}

// (h) INSUFFICIENT PROVENANCE FAILS CLOSED FOR THE AFFECTED RUN ONLY, and it
// fails closed in the strong sense: no adoption, no termination, no reuse of the
// identity.
func TestStalePane_UnprovenProvenanceNeverAdoptsOrDestroys(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	identity := staleReviewerPane(t, f)
	launches := f.launcher.launchCalls

	f.converge()

	if f.launcher.launchCalls != launches {
		t.Fatalf("a second reviewer was launched onto the same work (%d new launches)", f.launcher.launchCalls-launches)
	}
	if f.launcher.cancelCalls != 0 {
		t.Fatalf("cancel attempts = %d, want 0", f.launcher.cancelCalls)
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("the reviewer was destroyed without proof of ownership")
	}
	if f.hasPhase("review_cancel_confirmed") {
		t.Fatal("a cancellation AO could not prove was recorded as confirmed")
	}
}

// (d) ONE BAD RUN IS ONE BAD RUN. Boot reconciliation must repair every other
// run on the machine even while one of them cannot be reconciled at all.
func TestReconcile_OneUnreconcilableRunDoesNotAbortTheOthers(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	ctx := f.ctx

	// A second, entirely unrelated run, left in the state a restart interrupts:
	// a step mid-execution with no independent fact source.
	other, err := f.c.CreateRun(ctx, "proj-1", "an unrelated objective")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// The verify step, not the plan step: since CP24-CP27 an interrupted plan
	// step is re-derivable and boot recovery finishes it instead of parking the
	// run. Verify is still a step kind with no independent fact source, which
	// is the condition this test needs.
	interruptedStep := other.Steps[4].Step
	if ok, uerr := f.store.UpdateWorkflowStepState(ctx, interruptedStep.ID,
		domain.WorkflowStepPending, domain.WorkflowStepReady, f.clk.Now()); uerr != nil || !ok {
		t.Fatalf("seed ready verify step: ok=%v err=%v", ok, uerr)
	}
	if ok, uerr := f.store.UpdateWorkflowStepState(ctx, interruptedStep.ID,
		domain.WorkflowStepReady, domain.WorkflowStepRunning, f.clk.Now()); uerr != nil || !ok {
		t.Fatalf("seed running verify step: ok=%v err=%v", ok, uerr)
	}
	if ok, uerr := f.store.UpdateWorkflowRunState(ctx, other.Run.ID,
		domain.WorkflowRunPending, domain.WorkflowRunRunning, f.clk.Now()); uerr != nil || !ok {
		t.Fatalf("seed running run: ok=%v err=%v", ok, uerr)
	}

	// The FIRST run cannot be reconciled at all -- the durable read it needs
	// fails outright, which is the strongest form of the production condition.
	boom := errors.New("runtime observation unavailable for this run")
	f.store.listStepsErrFor = map[string]error{f.runID: boom}

	rerr := f.c.Reconcile(ctx)

	// The failure is REPORTED (boot must not pretend it repaired everything)...
	if rerr == nil {
		t.Fatal("Reconcile hid a run it could not reconcile")
	}
	if !strings.Contains(rerr.Error(), f.runID) {
		t.Fatalf("error = %v, want it to name the run that failed", rerr)
	}
	// ...and the unrelated run was reconciled anyway.
	otherRun, ok, gerr := f.store.GetWorkflowRun(ctx, other.Run.ID)
	if gerr != nil || !ok {
		t.Fatalf("GetWorkflowRun(other): %v (ok=%v)", gerr, ok)
	}
	if otherRun.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("unrelated run state = %q, want needs_attention: it was never reconciled because another run failed first",
			otherRun.State)
	}
	otherSteps, serr := f.store.ListWorkflowSteps(ctx, other.Run.ID)
	if serr != nil {
		t.Fatalf("ListWorkflowSteps(other): %v", serr)
	}
	if otherSteps[4].State != domain.WorkflowStepWaiting {
		t.Fatalf("unrelated verify step = %q, want waiting", otherSteps[4].State)
	}

	// The failing run is parked with a reason a person can act on, rather than
	// being re-driven blindly on every boot.
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("failing run state = %q, want needs_attention", got)
	}
	phases := ledgerPhases(t, f.store, f.runID)
	found := false
	for _, p := range phases {
		if p == workflowcore.ReasonRecoveryUnreconcilable {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %q stop recorded; phases = %v", workflowcore.ReasonRecoveryUnreconcilable, phases)
	}
}
