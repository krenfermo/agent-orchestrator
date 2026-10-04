package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// fix_budget.go — what "the fix budget" counts, stated once.
//
// THE INCIDENT (wf-724a1e97). max_fix_cycles was 3 and the run stopped saying
// "the reviewer still requests changes after 6 review cycles". Both numbers were
// durable and neither was wrong about what it measured; they simply measured
// different things. The budget was being enforced against a count of REVIEW RUN
// ROWS for the worker session:
//
//	for _, r := range reviewRuns(session) { if r.Harness == harness { n++ } }
//
// which counts every review that ever existed for that session, whatever caused
// it. On this run that was six rows for three actual fix cycles:
//
//	623ffbfa  cycle 0's review — the FIRST review, which no fix produced
//	0dc7e12c  after fix cycle 1
//	21aef5c4  after fix cycle 2
//	04095c8b  CANCELLED and re-dispatched for reviewer capacity — one review,
//	          two rows, and the second row is a transport retry, not a cycle
//	b626d1a1  after fix cycle 3   <- the budget genuinely ran out here
//	33c08c40  the fresh review of an externally applied fix, which by
//	          human_applied_fix.go's own contract consumes no cycle at all
//
// Two consequences, and the second is the one that mattered. The stop's
// arithmetic was unreadable — nobody can reconcile "6" against "3" — and, far
// worse, the post-recovery fresh review was itself counted, so
// resumeHumanAppliedFix could never converge: whatever the reviewer said about
// the corrected tree, the verdict landed on a counter that was already over
// budget and the run parked again on fix_budget_exhausted.
//
// THE RULE. The fix budget counts FIX CYCLES: deliveries AO actually made to a
// worker asking it to change the tree. Nothing else may consume it —
//
//	not the first review, which no fix caused;
//	not a reviewer relaunch after a capacity cancellation, which is one
//	  question asked twice by AO's own transport;
//	not a fresh review of a change AO did not make (human_applied_fix.go,
//	  repair recovery), which is a NEW authority over a NEW state.
//
// and it is a fold over the append-only ledger — the fix_dispatched checkpoints
// on the fix step, counted by distinct cycle number — so it is identical before
// and after a restart, and a crash between two cycles can neither lose one nor
// invent one.

// fixCyclesSpent is how many fix cycles this run has actually dispatched.
//
// Distinct cycle numbers, not row count: a cycle re-delivered by
// fix_cycle_resume.go, or completed twice across a crash boundary, is one cycle
// that happened, not two the run must pay for.
//
// Its failure mode is deliberate. A ledger it cannot read returns (0, false),
// and every caller treats "not known" as "do not spend budget on a guess" —
// refusing to dispatch rather than dispatching a cycle the run may not have.
func (c *Coordinator) fixCyclesSpent(ctx stdctx.Context, runID, fixStepID string) (int, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return 0, false
	}
	seen := map[int]struct{}{}
	for _, cp := range cps {
		if cp.DurablePhase != fixDispatchedPhase {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != fixStepID {
			continue
		}
		n := fixCycleNumberOf(cp)
		if n <= 0 {
			// A dispatch record whose cycle number cannot be read is still a
			// dispatch that happened. Counting it under a synthetic key keeps
			// "spent" honest without pretending to know which cycle it was.
			n = -len(seen) - 1
		}
		seen[n] = struct{}{}
	}
	return len(seen), true
}

// fixStepOf returns the run's fix step. Fix budget questions are meaningless
// without one, so the boolean is a real answer rather than a formality.
func (c *Coordinator) fixStepOf(ctx stdctx.Context, runID string) (domain.WorkflowStep, bool) {
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return domain.WorkflowStep{}, false
	}
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepFix {
			return steps[i], true
		}
	}
	return domain.WorkflowStep{}, false
}

// fixBudgetState is the whole of the budget answer for one run, derived at the
// moment it is asked.
type fixBudgetState struct {
	// Spent is the number of fix cycles already dispatched.
	Spent int
	// Budget is policy.MaxFixCycles.
	Budget int
	// Known is false when AO could not read the ledger or the fix step. Callers
	// must fail closed on it: an unknown budget authorizes nothing.
	Known bool
}

// Exhausted reports whether another fix cycle would exceed the budget.
func (s fixBudgetState) Exhausted() bool { return s.Known && s.Spent >= s.Budget }

// NextCycle is the cycle number the next dispatch would carry.
func (s fixBudgetState) NextCycle() int { return s.Spent + 1 }

// fixBudget derives the run's fix-cycle budget state.
func (c *Coordinator) fixBudget(ctx stdctx.Context, run domain.WorkflowRun) fixBudgetState {
	out := fixBudgetState{Budget: policyForRun(run).MaxFixCycles}
	fixStep, ok := c.fixStepOf(ctx, run.ID)
	if !ok {
		return out
	}
	spent, known := c.fixCyclesSpent(ctx, run.ID, fixStep.ID)
	out.Spent, out.Known = spent, known
	return out
}
