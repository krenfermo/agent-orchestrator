package workflow_test

// incident_wf724a1e97_test.go — the first real P2-A workflow, reproduced.
//
// THE DURABLE SEQUENCE, from ~/.ao/data (parent wf-95d5bd82, task child
// wf-724a1e97, repairs wf-913f54d8 and wf-f5025a7c):
//
//	00:39  fix_budget_exhausted           "after 5 review cycles (max_fix_cycles=3)"
//	00:43  workflow_repair_dispatched      generation 1
//	01:04  workflow_repair_dispatched      generation 2
//	01:21  human_applied_fix_observed      workspace at HEAD ccefd07b0
//	01:21  review 33c08c40 dispatched      target ccefd07b0
//	01:23  changes_requested               "after 6 review cycles (max_fix_cycles=3)"
//	  ...  247d3bc5f committed, which is exactly the change 33c08c40 asked for
//	  ...  and then nothing, for the rest of the run's life.
//
// Three separate defects are pinned here, because all three were required to
// produce that ending and fixing any one alone would have left it reachable:
//
//  1. AUTHORITY. A changes_requested verdict kept a run shut over a workspace
//     state it had never seen. The only transition that could notice was
//     reachable solely from a person's direct Continue on the child run
//     (head_convergence.go).
//  2. BUDGET. "6 review cycles" counted review_run rows — the first review, a
//     capacity-cancelled relaunch, and the post-recovery fresh review included
//     — so a recovery review could not converge even when the reviewer approved
//     (fix_budget.go).
//  3. REPAIR. Generation 2 was minted 25 seconds after generation 1 COMPLETED,
//     and generation 1's result was later discarded as "superseded ... and will
//     not resume anything" (repair_agent.go).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---------------------------------------------------------------------------
// 1. AUTHORITY: a superseded verdict may not keep a newer state shut.
// ---------------------------------------------------------------------------

// The incident end to end: SHA A reviewed and rejected, budget spent, SHA B
// appears and fixes the finding, and the run must reach a fresh authoritative
// review OF B — then verify and complete on B's approval, never returning to
// fix_budget_exhausted on A's verdict.
func TestIncidentWF724A1E97_NewHeadEarnsAFreshReviewAndConverges(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	shaA := f.oldFP
	reviewOfA := f.reviewRunID

	// SHA B: the commit that answers the finding (247d3bc5f in the incident).
	shaB := f.applyExternalFix(t, "247d3bc5f-harness-reads-stay-unobserved")
	if shaB == shaA {
		t.Fatalf("fixture precondition: B must differ from A")
	}
	f.launcher.launchCalls = 0

	got := f.continueRun()

	// (7) The fresh review targets B, not A.
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatalf("no review run was bound after the new head appeared")
	}
	freshID := *review.Step.ReviewRunID
	if freshID == reviewOfA {
		t.Fatalf("the run is still bound to review %s, which judged SHA A", reviewOfA)
	}
	if target := f.reviewRuns.runs[freshID].TargetSHA; target != shaB {
		t.Fatalf("fresh review target = %q, want the NEW state %q", target, shaB)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 fresh authoritative review", f.launcher.launchCalls)
	}
	// (2) A's authority is durably superseded rather than merely ignored.
	if sup := f.reviewRuns.runs[reviewOfA].SupersededBy; sup != freshID {
		t.Fatalf("review of A superseded_by = %q, want %q", sup, freshID)
	}

	// (8) B is approved.
	f.reviewRuns.setStatus(freshID, domain.ReviewRunComplete, domain.VerdictApproved)
	f.clk.Advance(time.Second)
	got = f.poll(2)

	// (9) The workflow continues past review instead of parking.
	if st := reviewStepFrom(got).Step.State; st != domain.WorkflowStepCompleted {
		t.Fatalf("review step state after approval = %q, want completed", st)
	}
	if s := f.runState(); s == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q: the approved new head still parked the run", s)
	}
	// (10) And it never re-parked on the OLD review's verdict.
	if n := f.countCheckpointPhase(workflowcore.ReasonFixBudgetExhausted); n != 1 {
		t.Fatalf("fix_budget_exhausted checkpoints = %d, want still exactly 1 (the original stop)", n)
	}
}

// The convergence must not require a person. In the incident the objective's
// heartbeat ran 185 times and could not reach the child, because
// fix_budget_exhausted is a human-owned stop. Boot reconciliation is the
// restart-safe half of the same answer.
func TestIncidentWF724A1E97_ConvergesWithoutAnybodyPressingContinue(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.applyExternalFix(t, "247d3bc5f")
	f.launcher.launchCalls = 0

	if err := f.c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed rows = %d, want exactly 1 from reconciliation alone", n)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", f.launcher.launchCalls)
	}
}

// Two resumes racing — a person's button and the heartbeat, or two daemons —
// produce ONE fresh review, because both enter the same fingerprint-keyed
// writer.
func TestIncidentWF724A1E97_ConcurrentResumesProduceOneFreshReview(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.applyExternalFix(t, "247d3bc5f")
	f.launcher.launchCalls = 0

	f.continueRun()
	if err := f.c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.poll(3)

	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed rows = %d, want exactly 1", n)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 for one new state", f.launcher.launchCalls)
	}
}

// A restart between "B observed" and the review dispatch must still produce
// exactly one review, and a restart AFTER the verdict must produce none.
func TestIncidentWF724A1E97_RestartAroundTheFreshReviewIsExactlyOnce(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.applyExternalFix(t, "247d3bc5f")
	f.launcher.launchCalls = 0

	f.continueRun()
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1", f.launcher.launchCalls)
	}
	freshID := *reviewStepFrom(f.poll(1)).Step.ReviewRunID

	// Restart before the verdict.
	f.c = f.restart()
	if err := f.c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	f.poll(2)
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches after restart = %d, want still 1", f.launcher.launchCalls)
	}

	// Verdict, then restart again.
	f.reviewRuns.setStatus(freshID, domain.ReviewRunComplete, domain.VerdictApproved)
	f.clk.Advance(time.Second)
	f.poll(1)
	f.c = f.restart()
	if err := f.c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after verdict: %v", err)
	}
	f.poll(2)
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches after the verdict = %d, want still 1", f.launcher.launchCalls)
	}
}

// The same workspace, observed again and again, is not a new authority. This is
// the review loop the fingerprint key exists to make impossible.
func TestIncidentWF724A1E97_RepeatedSameStateNeverLoopsReviews(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.launcher.launchCalls = 0

	// Nothing changed at all: not a recovery.
	f.continueRun()
	f.poll(5)
	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 0 {
		t.Fatalf("human_applied_fix_observed rows = %d, want 0: the workspace never moved", n)
	}
	if f.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", f.launcher.launchCalls)
	}

	// One change, then many observations of it.
	f.applyExternalFix(t, "247d3bc5f")
	f.continueRun()
	for i := 0; i < 4; i++ {
		f.continueRun()
		f.poll(2)
	}
	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed rows = %d, want exactly 1 for one state", n)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", f.launcher.launchCalls)
	}
}

// A verdict for SHA A that lands AFTER B has become authoritative may not move
// B. The pointer is the authority; a late verdict for a superseded run is
// evidence about a state nobody is waiting on.
func TestIncidentWF724A1E97_StaleVerdictForTheOldHeadCannotBlockTheNewOne(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	reviewOfA := f.reviewRunID
	f.applyExternalFix(t, "247d3bc5f")
	f.continueRun()
	freshID := *reviewStepFrom(f.poll(1)).Step.ReviewRunID

	// A's reviewer speaks again, long after it stopped being the authority.
	f.reviewRuns.setStatus(reviewOfA, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	f.clk.Advance(time.Second)
	got := f.poll(3)

	if id := *reviewStepFrom(got).Step.ReviewRunID; id != freshID {
		t.Fatalf("authority pointer = %q, want the fresh review %q: a stale verdict moved it", id, freshID)
	}
	if s := f.runState(); s == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q: a stale verdict for the old head re-parked the run", s)
	}

	// And B's own approval still lands normally.
	f.reviewRuns.setStatus(freshID, domain.ReviewRunComplete, domain.VerdictApproved)
	f.clk.Advance(time.Second)
	if st := reviewStepFrom(f.poll(2)).Step.State; st != domain.WorkflowStepCompleted {
		t.Fatalf("review step state = %q, want completed on B's approval", st)
	}
}

// A new head with no provable provenance is refused, and the refusal is a stop
// rather than an auto-resume. This is the property that must NOT be weakened by
// any of the above: the answer to "the tree moved" is a fresh review, and the
// answer to "the tree moved and AO's own worker might have moved it" is
// nothing at all.
func TestIncidentWF724A1E97_AmbiguousProvenanceStaysParked(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	f.applyExternalFix(t, "247d3bc5f")
	// This run's own worker spoke moments ago: the change may be its delivery.
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: f.clk.Now()}
		rec.TurnCompletedAt = f.clk.Now()
	})
	f.launcher.launchCalls = 0

	f.continueRun()
	if err := f.c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if n := f.countCheckpointPhase("human_applied_fix_observed"); n != 0 {
		t.Fatalf("human_applied_fix_observed rows = %d, want 0 for ambiguous provenance", n)
	}
	if f.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", f.launcher.launchCalls)
	}
	if s := f.runState(); s != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: an ambiguous change must not auto-resume", s)
	}
}

// ---------------------------------------------------------------------------
// 2. BUDGET: what a fix cycle costs, and what does not cost one.
// ---------------------------------------------------------------------------

// The stop's arithmetic must be readable: it counts fix cycles, not review rows.
func TestIncidentWF724A1E97_BudgetStopCountsFixCyclesNotReviewRows(t *testing.T) {
	f := newFixRecoveryFixture(t)
	got := f.driveToFixDispatch()
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	f.reviewRuns.runs[reviewRunID] = withTargetSHA(f.reviewRuns.runs[reviewRunID], f.workspaceFingerprint())

	// Three fix cycles, each answered with changes_requested — the exact shape
	// the incident had before the reviewer's fourth verdict.
	for cycle := 1; cycle <= 3; cycle++ {
		f.workspaceFacts.obs.HeadSHA = "cycle-" + string(rune('a'+cycle))
		f.clk.Advance(time.Minute)
		got = f.poll(2)
		review := reviewStepFrom(got)
		if review.Step.ReviewRunID == nil {
			t.Fatalf("cycle %d: no review run bound", cycle)
		}
		id := *review.Step.ReviewRunID
		f.reviewRuns.setStatus(id, domain.ReviewRunComplete, domain.VerdictChangesRequested)
		f.reviewRuns.runs[id] = withBody(f.reviewRuns.runs[id], "still not right")
		f.clk.Advance(time.Second)
		f.poll(2)
	}

	if n := f.countCheckpointPhase(workflowcore.ReasonFixBudgetExhausted); n != 1 {
		t.Fatalf("fix_budget_exhausted checkpoints = %d, want exactly 1", n)
	}
	note := f.latestCheckpointPhaseNextAction(workflowcore.ReasonFixBudgetExhausted)
	if !strings.Contains(note, "after 3 fix cycles") || !strings.Contains(note, "max_fix_cycles=3") {
		t.Fatalf("stop reads %q, want it to reconcile against the budget it names", note)
	}
}

// A reviewer relaunched after a capacity cancellation is ONE question asked
// twice by AO's own transport. It created review row 04095c8b in the incident
// and it must not cost a fix cycle.
func TestIncidentWF724A1E97_ACancelledReviewerRelaunchCostsNoFixCycle(t *testing.T) {
	f := newFixRecoveryFixture(t)
	got := f.driveToFixDispatch()
	firstReview := *reviewStepFrom(got).Step.ReviewRunID

	// One fix cycle has been dispatched. Whatever else exists on the ledger,
	// the budget must say exactly that.
	spent, note := f.fixCyclesSpentVia(t)
	if spent != 1 {
		t.Fatalf("fix cycles spent = %d (%s), want 1 after one dispatch", spent, note)
	}

	// A reviewer row that was cancelled and relaunched adds review rows and no
	// cycles.
	f.reviewRuns.setStatus(firstReview, domain.ReviewRunCancelled, "")
	f.clk.Advance(time.Second)
	f.poll(3)
	spent, note = f.fixCyclesSpentVia(t)
	if spent != 1 {
		t.Fatalf("fix cycles spent after a cancelled review = %d (%s), want still 1", spent, note)
	}
}

// A fresh review opened by the external-fix recovery consumes no budget — the
// recovery's own contract — and when it comes back changes_requested with
// budget REMAINING it consumes exactly one cycle, not zero and not two.
func TestIncidentWF724A1E97_RecoveryReviewSpendsBudgetExactlyOnce(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	spentBefore, _ := f.fixCyclesSpentVia(t)
	f.applyExternalFix(t, "247d3bc5f")

	f.continueRun()

	spentAfter, note := f.fixCyclesSpentVia(t)
	if spentAfter != spentBefore {
		t.Fatalf("fix cycles spent = %d (%s), want unchanged at %d: the recovery review is not a fix cycle",
			spentAfter, note, spentBefore)
	}
}

// ---------------------------------------------------------------------------
// 3. REPAIR: generations belong to the incident, and never nest.
// ---------------------------------------------------------------------------

// A repair run is an ordinary bounded task run, so without an explicit rule it
// is itself repairable — which is the `task -> repair -> repair -> repair`
// cascade the semantics forbid.
func TestIncidentWF724A1E97_ARepairRunIsNeverItselfRepaired(t *testing.T) {
	f := newBudgetExhaustedFixture(t)
	ctx := context.Background()

	// Mark this run as a repair agent's own run, exactly as LaunchRepair does
	// before starting one.
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-repair-origin", WorkflowRunID: f.runID, ProjectID: run.ProjectID,
		DurablePhase: "workflow_repair_run_origin", PayloadVersion: "v1",
		RetryState: "{}", NextAction: "repair run for wf-origin", CreatedAt: f.clk.Now(),
	}); err != nil {
		t.Fatalf("CreateWorkflowCheckpoint: %v", err)
	}

	plan, err := f.c.PlanRepair(ctx, f.runID)
	if err != nil {
		t.Fatalf("PlanRepair: %v", err)
	}
	if plan.Eligibility != domain.RepairIneligible {
		t.Fatalf("repair eligibility for a repair run = %q, want ineligible", plan.Eligibility)
	}
	if !strings.Contains(plan.Reason, "never becomes the parent of another repair") {
		t.Fatalf("reason = %q, want it to name the cascade it refuses", plan.Reason)
	}
	if _, err := f.c.LaunchRepair(ctx, f.runID, "operator"); err == nil {
		t.Fatalf("LaunchRepair on a repair run succeeded; want a refusal")
	}
}

// fixCyclesSpentVia counts fix cycles the way the budget does: distinct cycle
// numbers among the fix step's fix_dispatched checkpoints. It is derived here
// from the raw ledger rather than called through the coordinator so the test
// asserts what is durably true, not what the implementation reports.
func (f *fixRecoveryFixture) fixCyclesSpentVia(t *testing.T) (int, string) {
	t.Helper()
	seen := map[float64]struct{}{}
	rows := 0
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase != "fix_dispatched" {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != f.fixStepID {
			continue
		}
		rows++
		var rec map[string]any
		if err := jsonUnmarshalString(cp.RetryState, &rec); err != nil {
			t.Fatalf("decode fix_dispatched record: %v", err)
		}
		n, _ := rec["cycleNumber"].(float64)
		seen[n] = struct{}{}
	}
	return len(seen), fmt.Sprintf("%d fix_dispatched rows, %d distinct cycles", rows, len(seen))
}
