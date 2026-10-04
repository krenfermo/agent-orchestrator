package workflow_test

// incident_wfc4c84f52_test.go — the buried stop, reproduced end to end.
//
// THE DURABLE STATE, from ~/.ao/data:
//
//	wf-c4c84f52   needs_attention
//	  02:30:32   review_dispatched
//	  02:30:32   review_launch_confirmed
//	  02:30:32   review_launch_abandoned
//	  02:30:32   reviewer_launch_failed        <- the stop, human-owned
//	  02:30:32   review_observed (ended failed)
//	  02:31:46   review_observed  verify/approved   \
//	  ...                                            > 301 more, to 05:57
//	  05:57:03   review_observed  verify/approved   /
//
// The reviewer's approved verdict arrived AFTER its launch had been recorded as
// failed, so every later pass re-applied it, found both transitions invalid
// (failed -> completed, needs_attention -> waiting) and wrote an observation
// anyway. Three hours later the newest checkpoint was an observation, and every
// reader that took "newest" for "why" reported unclassified_stop — including
// the quiescence proof, which then refused to let the repair chain give the
// branch back.
//
// Two things are pinned here: the stop survives its own observations, on every
// surface; and an observation that changes nothing stops writing a row.

import (
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// buriedStopCase is a parked run whose stop is buried under observations.
//
// It is a run of its own rather than the quiescence fixture's repair, because
// that fixture's repair legitimately records stops of its own as it is read —
// and a newer stop SHOULD win. What is under test here is the opposite: that
// rows which are not stops never win, however many of them there are.
type buriedStopCase struct {
	*quiescenceCase
	runUnderTest string
	stopReason   string
}

func newBuriedStopCase(t *testing.T, observations int) *buriedStopCase {
	t.Helper()
	q := newQuiescenceCase(t)
	c := &buriedStopCase{quiescenceCase: q, stopReason: workflowcore.ReasonReviewerLaunchFailed}

	created, err := q.c.CreateRun(q.ctx, "agent-orchestrator", "a run whose reviewer could not be launched")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	c.runUnderTest = created.Run.ID
	moveRunRow(t, q, c.runUnderTest, domain.WorkflowRunNeedsAttention)

	burst := q.clk.Now()
	// The launch trail and the failure, all inside one instant, exactly as the
	// real ledger has them — and with ids ordered so the stop does NOT sort
	// last, so a reader that fell back to id order would get this wrong.
	for i, phase := range []string{
		"review_dispatched", workflowcore.ReasonReviewerLaunchFailed,
		"review_launch_confirmed", "review_launch_abandoned",
	} {
		if _, err := q.store.CreateWorkflowCheckpoint(q.ctx, domain.WorkflowCheckpoint{
			ID: fmt.Sprintf("wfc-burst-%d", i), WorkflowRunID: c.runUnderTest, ProjectID: created.Run.ProjectID,
			DurablePhase:   phase,
			NextAction:     "the reviewer could not be launched",
			PayloadVersion: "v1", RetryState: "{}", CreatedAt: burst,
		}); err != nil {
			t.Fatalf("seed burst row %s: %v", phase, err)
		}
	}
	// And the observations that buried it.
	for i := 0; i < observations; i++ {
		if _, err := q.store.CreateWorkflowCheckpoint(q.ctx, domain.WorkflowCheckpoint{
			ID: fmt.Sprintf("wfc-observed-%03d", i), WorkflowRunID: c.runUnderTest, ProjectID: created.Run.ProjectID,
			DurablePhase:   "review_observed",
			NextAction:     "verify",
			ReviewVerdict:  string(domain.VerdictApproved),
			PayloadVersion: "v1", RetryState: "{}",
			CreatedAt: burst.Add(time.Duration(i+1) * time.Second),
		}); err != nil {
			t.Fatalf("seed observation %d: %v", i, err)
		}
	}
	q.clk.Advance(time.Duration(observations+2) * time.Second)
	return c
}

// reasonOn each surface, so "the screen says one thing and the reconciler
// another" is a test failure rather than an incident.
func (c *buriedStopCase) apiReason(t *testing.T) string {
	t.Helper()
	detail, err := c.c.GetRun(c.ctx, c.runUnderTest)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{
		Detail: detail, Questions: detail.Questions,
	}).AttentionReason
}

func (c *buriedStopCase) boardReason(t *testing.T) string {
	t.Helper()
	entries, err := c.c.ProjectBoard(c.ctx, "agent-orchestrator", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ProjectBoard: %v", err)
	}
	for _, e := range entries {
		if e.Run.ID == c.runUnderTest {
			return e.Lifecycle.AttentionReason
		}
	}
	t.Fatalf("the run under test is not on the Board")
	return ""
}

// retentionReason is what branch-lock retention asks about a stopped owner —
// the reconciler's own view, through the exported classifier the branchlock
// package consumes.
func (c *buriedStopCase) retentionReason(t *testing.T) string {
	t.Helper()
	run, ok, err := c.store.GetWorkflowRun(c.ctx, c.runUnderTest)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	disp, err := c.c.ClassifyLockOwner(c.ctx, run)
	if err != nil {
		t.Fatalf("ClassifyLockOwner: %v", err)
	}
	return disp.Reason
}

// cliReason is what `ao workflow recover status` prints: the recovery
// assessment's reasonCode, which the HTTP layer copies verbatim.
func (c *buriedStopCase) cliReason(t *testing.T) string {
	t.Helper()
	assessment, err := c.c.AssessRecovery(c.ctx, c.runUnderTest)
	if err != nil {
		t.Fatalf("AssessRecovery: %v", err)
	}
	return assessment.ReasonCode
}

// ---------------------------------------------------------------------------
// (1) and (3): the stop survives its observations.
// ---------------------------------------------------------------------------

func TestAStopSurvives302ObservationsOnEverySurface(t *testing.T) {
	c := newBuriedStopCase(t, 302)

	for name, got := range map[string]string{
		"API/run detail":       c.apiReason(t),
		"Board":                c.boardReason(t),
		"retention/reconciler": c.retentionReason(t),
		"CLI recover status":   c.cliReason(t),
	} {
		if got != c.stopReason {
			t.Errorf("%s reports %q, want %q", name, got, c.stopReason)
		}
	}
}

// (5) restart: a new Coordinator over the same rows classifies identically.
// Nothing about this may live in memory.
func TestTheStopClassificationSurvivesARestart(t *testing.T) {
	c := newBuriedStopCase(t, 302)
	before := c.apiReason(t)
	c.restart()
	if after := c.apiReason(t); after != before {
		t.Fatalf("after a restart the stop reads %q, want %q", after, before)
	}
	if got := c.retentionReason(t); got != c.stopReason {
		t.Fatalf("after a restart retention reads %q, want %q", got, c.stopReason)
	}
}

// (7) N observations do not change the answer, and neither does none.
func TestObservationCountDoesNotChangeTheStop(t *testing.T) {
	for _, n := range []int{0, 1, 2, 50, 302} {
		c := newBuriedStopCase(t, n)
		if got := c.apiReason(t); got != c.stopReason {
			t.Fatalf("with %d observations the stop reads %q, want %q", n, got, c.stopReason)
		}
	}
}

// (2) a genuine clearing transition DOES move the authority: a stop AO proved
// resolved stops explaining the run.
func TestAClearedStopIsNotResurrectedByLaterObservations(t *testing.T) {
	c := newBuriedStopCase(t, 10)
	run, _, err := c.store.GetWorkflowRun(c.ctx, c.runUnderTest)
	if err != nil {
		t.Fatal(err)
	}
	c.clk.Advance(time.Minute)
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-cleared", WorkflowRunID: c.runUnderTest, ProjectID: run.ProjectID,
		DurablePhase: "attention_cleared", NextAction: "resumed automatically",
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// And more observations after the clear, which must not bring the stop back.
	for i := 0; i < 5; i++ {
		c.clk.Advance(time.Second)
		if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
			ID: fmt.Sprintf("wfc-post-clear-%d", i), WorkflowRunID: c.runUnderTest, ProjectID: run.ProjectID,
			DurablePhase: "review_observed", NextAction: "verify",
			PayloadVersion: "v1", RetryState: "{}", CreatedAt: c.clk.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if got := c.apiReason(t); got == c.stopReason {
		t.Fatalf("a cleared stop is still being reported as %q", got)
	}
}

// ---------------------------------------------------------------------------
// (7) the write side: an observation that changes nothing writes nothing.
// ---------------------------------------------------------------------------

// The livelock, reproduced through the real observation path: a review run
// whose late verdict cannot be applied because the step already failed. The
// first pass records what it saw; every pass after it adds nothing, so it
// writes nothing.
func TestARedundantReviewObservationIsNotRecordedTwice(t *testing.T) {
	f := newFossilCase(t)
	before := countPhaseOn(t, f.quiescenceCase, f.repairRunID, "review_observed")

	// Re-read and re-reconcile the run whose concluded review can no longer be
	// applied. The first pass may legitimately record what it saw; the ones
	// after it add nothing, and must therefore write nothing.
	for i := 0; i < 5; i++ {
		if _, err := f.c.GetRun(f.ctx, f.repairRunID); err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	after := countPhaseOn(t, f.quiescenceCase, f.repairRunID, "review_observed")
	if after-before > 1 {
		t.Fatalf("re-observing one concluded review wrote %d rows across 5 passes; a redundant observation must write none",
			after-before)
	}
}

func countPhaseOn(t *testing.T, q *quiescenceCase, runID, phase string) int {
	t.Helper()
	cps, err := q.store.ListWorkflowCheckpoints(q.ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints(%s): %v", runID, err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}
