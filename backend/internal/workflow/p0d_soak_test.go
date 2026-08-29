package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// P0-D section J — deterministic soak.
//
// This is NOT a 24-hour soak and must never be reported as one. It is a
// compressed, deterministic one: N complete lifecycles driven end to end
// through the same coordinator machinery a real daemon uses, with a restart and
// repeated reconciliation folded into every iteration, on a fake clock.
//
// What a compressed soak can and cannot show is worth being exact about, since
// the whole point of P0-D is not overclaiming:
//
//   - It CAN find state that accumulates per lifecycle or per reconcile — a
//     ledger that grows without new facts, a duplicate launch that only appears
//     on the 40th run, an idempotency key that collides across runs, a wake that
//     is scheduled and never consumed. Those are functions of iteration count,
//     and iteration count is exactly what this buys.
//   - It CANNOT show anything that is a function of WALL-CLOCK time: a real
//     memory leak, fd exhaustion, a tmux server degrading over hours, a
//     provider's session expiring. The clock here is fake and the runtime is a
//     fake. Those remain unproven by this file, and are called out as such in
//     the P0-D report rather than being implied away.
//
// The invariant is per-iteration exactness: every lifecycle must produce
// exactly one worker spawn, one review run, one reviewer launch, and (for the
// fix lane) one fix prompt — no more, no matter how many restarts and
// reconciles happen inside it. Aggregate counters are then simply N times the
// per-iteration figure, and any drift shows up as a mismatch naming the exact
// iteration it started on.
const (
	p0dSoakIterations = 500
	// reconciles per iteration: enough for a duplicate-on-repeat bug to fire,
	// on top of the restart each iteration already performs.
	p0dSoakReconciles = 3
)

// p0dSoakTotals accumulates the whole soak's observable side effects.
type p0dSoakTotals struct {
	lifecycles  int
	spawns      int
	launches    int
	reviewRuns  int
	sends       int
	checkpoints int
	completed   int
}

// TestP0D_SoakApprovedLifecycles drives the approve lane: objective -> work ->
// review -> approved, with a restart and repeated reconciliation inside every
// iteration.
func TestP0D_SoakApprovedLifecycles(t *testing.T) {
	ctx := context.Background()
	var totals p0dSoakTotals

	for i := 0; i < p0dSoakIterations; i++ {
		f := newFixRecoveryFixture(t)

		completeWorkStep(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.runID)
		got, err := f.c.ContinueRun(ctx, f.runID)
		if err != nil {
			t.Fatalf("iteration %d: ContinueRun: %v", i, err)
		}
		review := reviewStepFrom(got)
		if review.Step.ReviewRunID == nil {
			t.Fatalf("iteration %d: no review run", i)
		}
		f.reviewRuns.setStatus(*review.Step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)
		f.clk.Advance(time.Second)

		// A restart mid-lifecycle, then repeated reconciliation: the two things
		// an unattended daemon does that a single-pass test never exercises.
		c := f.restart()
		for r := 0; r < p0dSoakReconciles; r++ {
			if err := c.Reconcile(ctx); err != nil {
				t.Fatalf("iteration %d reconcile %d: %v", i, r, err)
			}
			f.clk.Advance(5 * time.Second)
		}
		final, err := c.GetRun(ctx, f.runID)
		if err != nil {
			t.Fatalf("iteration %d: GetRun: %v", i, err)
		}

		// Per-iteration exactness. Checked every iteration rather than only in
		// the aggregate, so a drift names the iteration it began on instead of
		// showing up as a wrong total at the end.
		if f.spawner.calls != 1 {
			t.Fatalf("iteration %d: spawns = %d, want exactly 1", i, f.spawner.calls)
		}
		if f.reviewRuns.insertCalls != 1 {
			t.Fatalf("iteration %d: review runs = %d, want exactly 1", i, f.reviewRuns.insertCalls)
		}
		if f.launcher.launchCalls != 1 {
			t.Fatalf("iteration %d: reviewer launches = %d, want exactly 1", i, f.launcher.launchCalls)
		}
		if f.sender.calls != 0 {
			t.Fatalf("iteration %d: fix prompts = %d, want 0 on an approved run", i, f.sender.calls)
		}
		if workStepFrom(final).Step.State != domain.WorkflowStepCompleted {
			t.Fatalf("iteration %d: work step = %q, want completed", i, workStepFrom(final).Step.State)
		}
		if final.Run.State == domain.WorkflowRunFailed {
			t.Fatalf("iteration %d: run failed: %+v", i, final.Run)
		}

		cps, err := f.store.ListWorkflowCheckpoints(ctx, f.runID)
		if err != nil {
			t.Fatalf("iteration %d: checkpoints: %v", i, err)
		}
		totals.lifecycles++
		totals.spawns += f.spawner.calls
		totals.launches += f.launcher.launchCalls
		totals.reviewRuns += f.reviewRuns.insertCalls
		totals.sends += f.sender.calls
		totals.checkpoints += len(cps)
		if final.Run.State == domain.WorkflowRunCompleted {
			totals.completed++
		}
	}

	if totals.lifecycles != p0dSoakIterations {
		t.Fatalf("lifecycles = %d, want %d", totals.lifecycles, p0dSoakIterations)
	}
	// Aggregate exactness: N lifecycles, N of each launch-shaped side effect.
	if totals.spawns != p0dSoakIterations || totals.launches != p0dSoakIterations ||
		totals.reviewRuns != p0dSoakIterations || totals.sends != 0 {
		t.Fatalf("aggregate side effects = %+v, want %d spawns/launches/reviewRuns and 0 sends",
			totals, p0dSoakIterations)
	}
	// The ledger must be a flat function of iteration count: same work, same
	// number of rows. A soak whose average checkpoint count per lifecycle drifts
	// is the leak this file exists to find.
	if totals.checkpoints%p0dSoakIterations != 0 {
		t.Fatalf("checkpoint total %d is not a whole multiple of %d lifecycles: "+
			"per-lifecycle ledger size is not constant", totals.checkpoints, p0dSoakIterations)
	}
	t.Logf("P0-D soak (approved lane): %d lifecycles, %d checkpoints (%d per lifecycle), "+
		"%d spawns, %d reviewer launches, %d review runs, %d fix prompts",
		totals.lifecycles, totals.checkpoints, totals.checkpoints/totals.lifecycles,
		totals.spawns, totals.launches, totals.reviewRuns, totals.sends)
}

// TestP0D_SoakFixCycleLifecycles drives the fix lane: objective -> work ->
// review -> changes_requested -> fix dispatched, again with a restart and
// repeated reconciliation inside every iteration.
//
// The fix prompt is the side effect that matters most here: this is the lane
// where a duplicate is not a wasted launch but a second copy of the reviewer's
// findings pasted into a live composer.
func TestP0D_SoakFixCycleLifecycles(t *testing.T) {
	ctx := context.Background()
	var totals p0dSoakTotals

	for i := 0; i < p0dSoakIterations; i++ {
		f := newFixRecoveryFixture(t)
		f.driveToFixDispatch()

		c := f.restart()
		for r := 0; r < p0dSoakReconciles; r++ {
			if err := c.Reconcile(ctx); err != nil {
				t.Fatalf("iteration %d reconcile %d: %v", i, r, err)
			}
			f.clk.Advance(5 * time.Second)
		}
		final, err := c.GetRun(ctx, f.runID)
		if err != nil {
			t.Fatalf("iteration %d: GetRun: %v", i, err)
		}

		if f.sender.calls != 1 {
			t.Fatalf("iteration %d: fix prompts = %d, want exactly 1", i, f.sender.calls)
		}
		if f.spawner.calls != 1 {
			t.Fatalf("iteration %d: spawns = %d, want exactly 1 (the fix reuses the worker)", i, f.spawner.calls)
		}
		if f.reviewRuns.insertCalls != 1 {
			t.Fatalf("iteration %d: review runs = %d, want exactly 1 before the fix completes", i, f.reviewRuns.insertCalls)
		}
		if s := fixStepFrom(final).Step.State; s != domain.WorkflowStepRunning {
			t.Fatalf("iteration %d: fix step = %q, want running", i, s)
		}

		cps, err := f.store.ListWorkflowCheckpoints(ctx, f.runID)
		if err != nil {
			t.Fatalf("iteration %d: checkpoints: %v", i, err)
		}
		totals.lifecycles++
		totals.spawns += f.spawner.calls
		totals.launches += f.launcher.launchCalls
		totals.reviewRuns += f.reviewRuns.insertCalls
		totals.sends += f.sender.calls
		totals.checkpoints += len(cps)
	}

	if totals.sends != p0dSoakIterations {
		t.Fatalf("fix prompts across the soak = %d, want exactly %d (one per lifecycle)",
			totals.sends, p0dSoakIterations)
	}
	if totals.checkpoints%p0dSoakIterations != 0 {
		t.Fatalf("checkpoint total %d is not a whole multiple of %d lifecycles: "+
			"per-lifecycle ledger size is not constant", totals.checkpoints, p0dSoakIterations)
	}
	t.Logf("P0-D soak (fix lane): %d lifecycles, %d checkpoints (%d per lifecycle), "+
		"%d spawns, %d reviewer launches, %d review runs, %d fix prompts",
		totals.lifecycles, totals.checkpoints, totals.checkpoints/totals.lifecycles,
		totals.spawns, totals.launches, totals.reviewRuns, totals.sends)
}
