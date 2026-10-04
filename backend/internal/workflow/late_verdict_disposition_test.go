package workflow_test

// late_verdict_disposition_test.go — the last retry loop, closed.
//
// THE DURABLE STATE (wf-c4c84f52, review run 7e528219):
//
//	review_run   status=failed   verdict=''   late_verdict=approved
//	review step  failed
//	outbox       trigger_review  workflow-step-review:<step>:cycle1:codex  pending
//	run          needs_attention  reviewer_launch_failed
//
// The reviewer's launch failed and the step failed with it; then the reviewer
// answered. Adoption needs a legal transition and `failed` has none, so every
// pass re-tried and the cycle's dispatch stayed pending — which clause (7) of
// the quiescence proof reads, correctly, as "this run can still launch work".
// The branch never came home.
//
// What is pinned: one disposition, recorded once; the dispatch that can never
// launch closed; and the whole A -> B -> C chain unwinding with nobody pressing
// anything. The negatives are the point — a live step keeps its dispatch, and a
// verdict that CAN be adopted still is.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

const (
	lateVerdictRefusedPhase = "review_late_verdict_refused"
	lateVerdictAdoptedPhase = "review_late_verdict_adopted"
	lateVerdictReviewRunID  = "rr-late-7e528219"
	lateVerdictReviewID     = "rev-late-7e528219"
	lateVerdictTargetSHA    = "1d9063aa9b4886ff3ca6ee7f2495730101264b15b4842503e6bdb429ef35fba5"
)

// seedLateVerdictOnC gives C the exact shape: a review run AO closed out as
// failed whose reviewer answered afterwards, bound to C's own review step, plus
// the cycle's trigger_review still sitting pending.
//
// Every row is written through the store method production uses —
// RecordLateReviewVerdict refuses anything that is not terminal-without-verdict,
// so the fixture cannot accidentally build a shape the real path could not
// produce.
func (c *chainCase) seedLateVerdictOnC(t *testing.T, stepState domain.WorkflowStepState) domain.WorkflowStep {
	return c.seedLateVerdictOnCWith(t, stepState, domain.VerdictApproved)
}

func (c *chainCase) seedLateVerdictOnCWith(
	t *testing.T, stepState domain.WorkflowStepState, verdict domain.ReviewVerdict,
) domain.WorkflowStep {
	t.Helper()
	harness := domain.ReviewerHarness("codex")
	session := c.attachSessionTo(t, refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepPlan))
	if err := c.store.UpsertReview(c.ctx, domain.Review{
		ID: lateVerdictReviewID, SessionID: session, ProjectID: "agent-orchestrator",
		Harness: harness, CreatedAt: c.clk.Now(), UpdatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("UpsertReview: %v", err)
	}
	if err := c.store.InsertReviewRun(c.ctx, domain.ReviewRun{
		ID: lateVerdictReviewRunID, ReviewID: lateVerdictReviewID, SessionID: session,
		Harness: harness, TargetSHA: lateVerdictTargetSHA,
		Status: domain.ReviewRunRunning, CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("InsertReviewRun: %v", err)
	}
	// AO gives up on it: closed out as failed, with no verdict.
	if ok, err := c.store.UpdateReviewRunResult(c.ctx, lateVerdictReviewRunID,
		domain.ReviewRunFailed, "", "the reviewer could not be launched", "", false); err != nil || !ok {
		t.Fatalf("close the review run out: ok=%v err=%v", ok, err)
	}
	// And then the reviewer answers.
	c.clk.Advance(time.Second)
	if ok, err := c.store.RecordLateReviewVerdict(c.ctx, lateVerdictReviewRunID,
		verdict, "the reviewer's findings", c.clk.Now()); err != nil || !ok {
		t.Fatalf("record the late verdict: ok=%v err=%v", ok, err)
	}

	step := refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepReview)
	if _, err := c.store.SetWorkflowStepReviewRun(c.ctx, step.ID, lateVerdictReviewRunID, c.clk.Now()); err != nil {
		t.Fatalf("bind C's review run: %v", err)
	}
	if stepState != step.State {
		moveStepRow(t, c.quiescenceCase, step, stepState)
	}
	step = refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepReview)

	// The cycle's dispatch, still pending: enqueued, launch attempted, never
	// dispatched.
	c.seedPendingReviewDispatch(t, step, 1)
	return step
}

func (c *chainCase) seedPendingReviewDispatch(t *testing.T, step domain.WorkflowStep, cycle int) domain.WorkflowOutboxEntry {
	t.Helper()
	stepID := step.ID
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(c.ctx, domain.WorkflowOutboxEntry{
		ID:             fmt.Sprintf("wfo-late-%s-c%d", step.ID, cycle),
		WorkflowRunID:  c.grandRunID,
		WorkflowStepID: &stepID,
		IdempotencyKey: fmt.Sprintf("workflow-step-review:%s:cycle%d:codex", step.ID, cycle),
		CommandType:    domain.WorkflowOutboxTriggerReview,
		Payload:        "{}",
		CreatedAt:      c.clk.Now(),
	})
	if err != nil {
		t.Fatalf("enqueue the review dispatch: %v", err)
	}
	if entry.Status != domain.WorkflowOutboxPending {
		t.Fatalf("fixture precondition: the dispatch is %s, want pending", entry.Status)
	}
	return entry
}

func (c *chainCase) outboxStatus(t *testing.T, id string) domain.WorkflowOutboxStatus {
	t.Helper()
	entries, err := c.store.ListWorkflowOutboxByRun(c.ctx, c.grandRunID)
	if err != nil {
		t.Fatalf("ListWorkflowOutboxByRun: %v", err)
	}
	for _, e := range entries {
		if e.ID == id {
			return e.Status
		}
	}
	t.Fatalf("outbox entry %s is gone; a retirement must never delete a row", id)
	return ""
}

func (c *chainCase) dispositionRows(t *testing.T, phase string) []map[string]any {
	t.Helper()
	cps, err := c.store.ListWorkflowCheckpoints(c.ctx, c.grandRunID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var out []map[string]any
	for _, cp := range cps {
		if cp.DurablePhase != phase {
			continue
		}
		body := map[string]any{}
		_ = json.Unmarshal([]byte(cp.RetryState), &body)
		out = append(out, body)
	}
	return out
}

// ---------------------------------------------------------------------------
// The real incident, end to end.
// ---------------------------------------------------------------------------

func TestALateVerdictOnAFailedStepIsRefusedOnceAndFreesTheChain(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})
	step := c.seedLateVerdictOnC(t, domain.WorkflowStepFailed)
	entryID := fmt.Sprintf("wfo-late-%s-c1", step.ID)

	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("fixture precondition: the branch is held by %s, want C", got)
	}
	if got := c.outboxStatus(t, entryID); got != domain.WorkflowOutboxPending {
		t.Fatalf("fixture precondition: the dispatch is %s, want pending", got)
	}

	// Reconcile #1: the verdict is disposed of, the dead dispatch is closed, and
	// the deepest link of the chain folds.
	c.reconcileOnly(t)

	refusals := c.dispositionRows(t, lateVerdictRefusedPhase)
	if len(refusals) != 1 {
		t.Fatalf("late-verdict refusals = %d, want exactly 1", len(refusals))
	}
	if got := refusals[0]["reason"]; got != "step_terminal" {
		t.Fatalf("refusal reason = %v, want step_terminal (%v)", got, refusals[0])
	}
	if got := refusals[0]["verdict"]; got != string(domain.VerdictApproved) {
		t.Fatalf("refusal verdict = %v, want approved: the reviewer's real answer must be recorded, not discarded", got)
	}
	if got := refusals[0]["reviewRunId"]; got != lateVerdictReviewRunID {
		t.Fatalf("refusal names review run %v, want %s", got, lateVerdictReviewRunID)
	}
	if got := refusals[0]["targetSha"]; got != lateVerdictTargetSHA {
		t.Fatalf("refusal names target %v, want the SHA the review judged", got)
	}
	if len(c.dispositionRows(t, lateVerdictAdoptedPhase)) != 0 {
		t.Fatal("the verdict was recorded as adopted; a terminal step cannot adopt one")
	}
	if got := c.outboxStatus(t, entryID); got != domain.WorkflowOutboxFailed {
		t.Fatalf("the dispatch is %s, want failed: a dispatch whose step has ended can never launch", got)
	}
	if got := c.holder(t); got != c.repairRunID {
		t.Fatalf("after reconcile 1 the branch is held by %s, want B (%s)", got, c.repairRunID)
	}

	// Reconcile #2: B -> A.
	c.reconcileOnly(t)
	if got := c.holder(t); got != c.runID {
		t.Fatalf("after reconcile 2 the branch is held by %s, want A (%s)", got, c.runID)
	}

	// Nothing a person did, and nothing bought.
	for _, id := range []string{c.runID, c.repairRunID, c.grandRunID} {
		run, _, err := c.store.GetWorkflowRun(c.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.State == domain.WorkflowRunCancelled {
			t.Fatalf("run %s was cancelled; this must resolve without one", id)
		}
	}
	if n := c.countPhase("workflow_repair_dispatched"); n != 2 {
		t.Fatalf("repair dispatches = %d, want still 2", n)
	}
}

// EXACTLY ONCE, over any number of passes and restarts: no second refusal, no
// second retirement, no observation spam, no reviewer.
func TestALateVerdictDispositionIsExactlyOnceAcrossPassesAndRestarts(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})
	step := c.seedLateVerdictOnC(t, domain.WorkflowStepFailed)
	entryID := fmt.Sprintf("wfo-late-%s-c1", step.ID)
	observedBefore := countPhaseOn(t, c.quiescenceCase, c.grandRunID, "review_observed")
	launchesBefore := c.rl.launchCalls

	for i := 0; i < 6; i++ {
		c.restart()
		c.reconcileOnly(t)
		if _, err := c.c.GetRun(c.ctx, c.grandRunID); err != nil {
			t.Fatalf("GetRun: %v", err)
		}
	}

	if n := len(c.dispositionRows(t, lateVerdictRefusedPhase)); n != 1 {
		t.Fatalf("late-verdict refusals = %d across 6 passes and restarts, want exactly 1", n)
	}
	if got := c.outboxStatus(t, entryID); got != domain.WorkflowOutboxFailed {
		t.Fatalf("the dispatch is %s, want failed", got)
	}
	if got := countPhaseOn(t, c.quiescenceCase, c.grandRunID, "review_observed") - observedBefore; got > 1 {
		t.Fatalf("re-reading one disposed verdict wrote %d observations; a decided verdict must stop being re-applied", got)
	}
	if got := c.rl.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0: a refused verdict must never start a second reviewer", got)
	}
}

// A crash between the disposition and the dispatch retirement converges: the
// next pass finishes the half that is missing, and neither half is done twice.
func TestACrashBetweenDispositionAndRetirementConverges(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})
	step := c.seedLateVerdictOnC(t, domain.WorkflowStepFailed)
	entryID := fmt.Sprintf("wfo-late-%s-c1", step.ID)

	// The disposition landed and the process died before the retirement: exactly
	// what the ledger looks like mid-pass.
	run, _, err := c.store.GetWorkflowRun(c.ctx, c.grandRunID)
	if err != nil {
		t.Fatal(err)
	}
	stepID := step.ID
	reviewRunID := lateVerdictReviewRunID
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-lvrefused-" + lateVerdictReviewRunID + "-" + step.ID,
		WorkflowRunID: c.grandRunID, WorkflowStepID: &stepID, ProjectID: run.ProjectID,
		ReviewRunID: &reviewRunID, ReviewVerdict: string(domain.VerdictApproved),
		DurablePhase: lateVerdictRefusedPhase, NextAction: "refused before the crash",
		PayloadVersion: "v1",
		RetryState: fmt.Sprintf(`{"disposition":"refused","reason":"step_terminal","reviewRunId":%q,"stepId":%q,"verdict":"approved"}`,
			lateVerdictReviewRunID, step.ID),
		CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("simulate the disposition: %v", err)
	}

	c.restart()
	c.reconcileOnly(t)

	if n := len(c.dispositionRows(t, lateVerdictRefusedPhase)); n != 1 {
		t.Fatalf("refusals = %d after converging, want exactly 1", n)
	}
	if got := c.outboxStatus(t, entryID); got != domain.WorkflowOutboxFailed {
		t.Fatalf("the dispatch is %s, want failed: the next pass must finish the retirement", got)
	}
}

// ---------------------------------------------------------------------------
// Negatives.
// ---------------------------------------------------------------------------

// A LIVE review step keeps its dispatch. Retiring one would silently cancel a
// review somebody is waiting for.
//
// The step is live because its reviewer is still RUNNING — deliberately not
// "waiting with an adoptable late verdict", which is a different case entirely:
// there the verdict is adopted, the step reaches completed, and retiring the
// answered cycle's dispatch is then correct. See the adoption test below.
func TestALiveReviewStepKeepsItsPendingDispatch(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{runningReviewOnC: true})
	step := refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepReview)
	if step.State.Terminal() {
		t.Fatalf("fixture precondition: the review step is %s, want a live one", step.State)
	}
	entry := c.seedPendingReviewDispatch(t, step, 1)

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.outboxStatus(t, entry.ID); got == domain.WorkflowOutboxFailed {
		t.Fatal("a live review step's pending dispatch was retired; only a terminal step's can be")
	}
}

// A verdict that CAN be adopted still is: the refusal path must not have become
// the answer to every late verdict.
func TestALateVerdictOnAWaitingStepIsStillAdopted(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})
	c.seedLateVerdictOnC(t, domain.WorkflowStepWaiting)

	c.reconcileOnly(t)

	if n := len(c.dispositionRows(t, lateVerdictRefusedPhase)); n != 0 {
		t.Fatalf("an adoptable late verdict was refused (%d rows)", n)
	}
	if n := len(c.dispositionRows(t, lateVerdictAdoptedPhase)); n != 1 {
		t.Fatalf("late-verdict adoptions = %d, want exactly 1: a waiting step is liftable and the verdict is real", n)
	}
	step := refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepReview)
	if step.State != domain.WorkflowStepCompleted {
		t.Fatalf("the review step is %s, want completed: an adopted approval advances toward verify", step.State)
	}
}

// A dispatch belonging to a DIFFERENT step is never touched, however dead this
// one is.
func TestADispatchForAnotherStepIsNeverRetired(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})
	step := c.seedLateVerdictOnC(t, domain.WorkflowStepFailed)
	other := refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepVerify)
	otherID := other.ID
	if _, _, err := c.store.EnqueueWorkflowOutboxEntry(c.ctx, domain.WorkflowOutboxEntry{
		ID: "wfo-other-step", WorkflowRunID: c.grandRunID, WorkflowStepID: &otherID,
		IdempotencyKey: fmt.Sprintf("workflow-step-review:%s:cycle1:codex", other.ID),
		CommandType:    domain.WorkflowOutboxTriggerReview, Payload: "{}", CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("enqueue the unrelated dispatch: %v", err)
	}

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.outboxStatus(t, fmt.Sprintf("wfo-late-%s-c1", step.ID)); got != domain.WorkflowOutboxFailed {
		t.Fatalf("the dead dispatch is %s, want failed", got)
	}
	if got := c.outboxStatus(t, "wfo-other-step"); got == domain.WorkflowOutboxFailed {
		t.Fatal("a dispatch belonging to another step was retired")
	}
}

// A review run that is genuinely still RUNNING is not a late verdict at all:
// nothing is disposed, nothing is retired, and the chain stays put.
func TestARunningReviewIsNeverDisposed(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true, runningReviewOnC: true})

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if n := len(c.dispositionRows(t, lateVerdictRefusedPhase)); n != 0 {
		t.Fatalf("a running review was disposed (%d rows)", n)
	}
	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("branch holder = %s, want C: a running reviewer is a live obligation", got)
	}
}

// Two daemons disposing of the same verdict produce one row, because the
// refusal's identity is derived from what it is about rather than minted.
func TestTwoDaemonsDisposeOfOneVerdictOnce(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})
	step := c.seedLateVerdictOnC(t, domain.WorkflowStepFailed)
	second := c.newCoordinatorOverSameStore()

	c.reconcileOnly(t)
	if err := second.Reconcile(c.ctx); err != nil {
		t.Fatalf("Reconcile (second daemon): %v", err)
	}

	if n := len(c.dispositionRows(t, lateVerdictRefusedPhase)); n != 1 {
		t.Fatalf("refusals = %d with two daemons, want exactly 1", n)
	}
	if got := c.outboxStatus(t, fmt.Sprintf("wfo-late-%s-c1", step.ID)); got != domain.WorkflowOutboxFailed {
		t.Fatalf("the dispatch is %s, want failed", got)
	}
	_ = workflowcore.ReasonReviewerLaunchFailed
}

// An adopted late verdict must cost exactly what an on-time one would: a
// changes_requested answer buys ONE fix obligation, not a second review cycle
// and not two fixes.
func TestAnAdoptedLateChangesRequestedBuysExactlyOneFixCycle(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})
	c.seedLateVerdictOnCWith(t, domain.WorkflowStepWaiting, domain.VerdictChangesRequested)
	launchesBefore := c.rl.launchCalls

	c.reconcileOnly(t)
	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if n := len(c.dispositionRows(t, lateVerdictAdoptedPhase)); n != 1 {
		t.Fatalf("late-verdict adoptions = %d, want exactly 1", n)
	}
	if n := countPhaseOn(t, c.quiescenceCase, c.grandRunID, "fix_dispatch_intent"); n != 1 {
		t.Fatalf("fix dispatch intents = %d across three passes, want exactly 1: an adopted verdict buys one fix, once", n)
	}
	if got := c.rl.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0: adopting a real review must never re-run it", got)
	}
}

// A review run a REPLACEMENT already superseded has no authority left, so its
// late verdict is neither adopted nor refused — there is nothing to decide, and
// nothing to loop on either.
func TestASupersededRunsLateVerdictIsLeftAlone(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})
	c.seedLateVerdictOnC(t, domain.WorkflowStepFailed)
	if ok, err := c.store.MarkReviewRunSupersededBy(c.ctx, lateVerdictReviewRunID, "rr-somebody-else"); err != nil || !ok {
		t.Fatalf("supersede the review run: ok=%v err=%v", ok, err)
	}

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if n := len(c.dispositionRows(t, lateVerdictRefusedPhase)); n != 0 {
		t.Fatalf("a superseded run's late verdict was disposed (%d rows); it has no authority to dispose of", n)
	}
	if n := len(c.dispositionRows(t, lateVerdictAdoptedPhase)); n != 0 {
		t.Fatalf("a superseded run's late verdict was adopted (%d rows)", n)
	}
}
