package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// fix_attempt_terminalization.go — closing the fix attempts whose cycle is over.
//
// P3-D §10 states the invariant: every attempt ends in an outcome, and none may
// sit at NULL once its cycle has ended. §28 names the shape that broke it. A
// review cycle mints its fix attempt row; the review is then cancelled, fails
// without a verdict, or is replaced by a newer one; and the row stays open
// forever. Nothing closes it, because the only writer that would have was the
// cycle that no longer exists.
//
// An open attempt row is not cosmetic. It is the sentence "a writer may be
// working in this tree", and every guard that asks whether it is safe to act
// reads it and stands down. So a fossil open row is a run wedged behind a claim
// nobody can retract — and, before fix_attempt_identity.go, one nobody could
// even attribute to a cycle.
//
// This is deliberately NOT the attempt reaper (attempt_reaper.go). The reaper
// closes attempts ABANDONED by a crash, and it is careful and slow about it
// precisely because it cannot tell live work from a fossil except by waiting
// and gathering. This closes attempts whose cycle is PROVABLY OVER, which is a
// different and much stronger fact: it needs no age threshold, no liveness
// probe and no quiet window, because the question is not "is anything still
// running" but "does anything still authorize this row". Nothing does.
//
// Three proofs of supersession, each positive, each read off durable rows:
//
//  1. the review run that authorized the cycle is terminal without a verdict —
//     cancelled or failed. Its findings are not an authority any more, and the
//     fix cycle they opened cannot be finished by anyone;
//  2. the review run recorded its verdict and a LATER review run exists for the
//     same session. The cycle was answered; the next one is somebody else's;
//  3. the step that owns the attempt, or the run that owns the step, is
//     terminal. Whatever the cycle was, it is over.
//
// And one non-proof, which is the whole of its safety: a review run AO cannot
// READ supersedes nothing. Unreadable is not cancelled. An unconcluded row
// whose authority could not be resolved is left exactly where it is, because
// closing it would tell every guard the tree is quiet while a fix worker may
// still be writing to it — the one failure mode that actually costs something.

// fixAttemptSupersededPhase is the durable record of one terminalized fix
// attempt: one per attempt id, ever. Written BEFORE the row is closed, so a
// crash between the two leaves the record and an open row, and the next pass
// finishes the job without writing a second record.
const fixAttemptSupersededPhase = "fix_attempt_superseded"

// fixAttemptSupersededRecord is the evidence a person can re-check rather than
// take on trust: which cycle the row belonged to, and which of the three proofs
// closed it.
type fixAttemptSupersededRecord struct {
	AttemptID   string `json:"attemptId"`
	ReviewRunID string `json:"reviewRunId"`
	CycleNumber int    `json:"cycleNumber"`
	Proof       string `json:"proof"`
	Detail      string `json:"detail"`
}

func (r fixAttemptSupersededRecord) json() string {
	b, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// fixCycleSupersession is the answer for one cycle: is it over, why, and could
// AO tell at all.
type fixCycleSupersession struct {
	Superseded bool
	// Known is false when the authority could not be read. Never acted on.
	Known  bool
	Proof  string
	Detail string
}

// assessFixCycleSupersession applies proofs 1 and 2 to one cycle identity.
//
// The cycle number is compared against the review runs that exist for the
// authorizing run's session — the same cardinality cascade.go counts cycles
// from, so the two cannot drift into disagreeing about which cycle is current.
func (c *Coordinator) assessFixCycleSupersession(
	ctx stdctx.Context, reviewRunID string, cycleNumber int,
) fixCycleSupersession {
	if c.reviewRuns == nil || reviewRunID == "" {
		return fixCycleSupersession{}
	}
	reviewRun, found, err := c.reviewRuns.GetReviewRun(ctx, reviewRunID)
	if err != nil {
		return fixCycleSupersession{}
	}
	if !found {
		// A row naming a review run that does not exist. This is not silence —
		// the read answered — but it is also not something to act on: AO cannot
		// show the cycle ended, only that it cannot find what opened it. Left
		// to the reaper, which is built for exactly the unattributable.
		return fixCycleSupersession{Known: true, Proof: "", Detail: fmt.Sprintf(
			"review run %s no longer exists; this cycle cannot be attributed", reviewRunID)}
	}
	// Proof 1: the authority is terminal and never produced a verdict.
	if reviewRun.TerminalWithoutVerdict() {
		return fixCycleSupersession{Superseded: true, Known: true, Proof: "review_cycle_cancelled",
			Detail: fmt.Sprintf("review run %s is %s and recorded no verdict, so the fix cycle it opened cannot be finished",
				reviewRunID, reviewRun.Status)}
	}
	// Proof 2: a later cycle exists on the same session.
	runs, err := c.reviewRuns.ListReviewRunsBySession(ctx, reviewRun.SessionID)
	if err != nil {
		return fixCycleSupersession{}
	}
	cycles := 0
	for _, r := range runs {
		if r.Harness == reviewRun.Harness {
			cycles++
		}
	}
	if cycles > cycleNumber {
		return fixCycleSupersession{Superseded: true, Known: true, Proof: "later_review_cycle",
			Detail: fmt.Sprintf("cycle %d was answered and the session is now on cycle %d", cycleNumber, cycles)}
	}
	return fixCycleSupersession{Known: true}
}

// TerminalizeSupersededFixAttempts closes every fix attempt on this run whose
// cycle is provably over, and returns how many it closed.
//
// Best-effort and per-run, like every other reconciliation in recovery.go's
// sweep: a run whose review authority cannot be read must not stop the others
// from being reconciled. Idempotent through two independent mechanisms — the
// one-per-attempt checkpoint, and the conclude CAS, which matches only a row
// that is still open — so any number of passes produce one closure and one
// record.
func (c *Coordinator) TerminalizeSupersededFixAttempts(ctx stdctx.Context, run domain.WorkflowRun) (int, error) {
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, step := range steps {
		if step.Kind != domain.WorkflowStepFix {
			continue
		}
		attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID)
		if err != nil {
			return closed, err
		}
		for _, a := range attempts {
			if a.Outcome != "" {
				continue
			}
			reviewRunID, cycleNumber, ok := FixAttemptCycle(a)
			if !ok {
				// Legacy/unproven: no derivable cycle, so no proof is
				// available and none is invented (P3-D §8).
				continue
			}
			verdict := c.assessFixCycleSupersession(ctx, reviewRunID, cycleNumber)
			// Proof 3 stands on its own and needs no review read at all.
			if !verdict.Superseded && (run.State.Terminal() || step.State.Terminal()) {
				verdict = fixCycleSupersession{Superseded: true, Known: true, Proof: "lifecycle_terminal",
					Detail: fmt.Sprintf("the fix step is %s and the run is %s; this cycle cannot continue",
						step.State, run.State)}
			}
			if !verdict.Superseded || !verdict.Known {
				continue
			}
			did, terr := c.terminalizeFixAttempt(ctx, run, step, a, reviewRunID, cycleNumber, verdict)
			if terr != nil {
				return closed, terr
			}
			if did {
				closed++
			}
		}
	}
	return closed, nil
}

// terminalizeFixAttempt writes the record, then closes the row. In that order,
// and never the other way round: a closure nobody can explain is exactly the
// unattributable row this file exists to abolish, one generation later.
func (c *Coordinator) terminalizeFixAttempt(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	reviewRunID string,
	cycleNumber int,
	verdict fixCycleSupersession,
) (bool, error) {
	if !c.fixAttemptSupersessionRecorded(ctx, run.ID, attempt.ID) {
		stepID := step.ID
		record := fixAttemptSupersededRecord{
			AttemptID: attempt.ID, ReviewRunID: reviewRunID, CycleNumber: cycleNumber,
			Proof: verdict.Proof, Detail: verdict.Detail,
		}
		if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             "wfc-" + c.newID(),
			WorkflowRunID:  run.ID,
			WorkflowStepID: &stepID,
			ProjectID:      run.ProjectID,
			NextAction:     verdict.Detail,
			DurablePhase:   fixAttemptSupersededPhase,
			PayloadVersion: "v1",
			RetryState:     record.json(),
			CreatedAt:      c.clock(),
		}); err != nil {
			return false, err
		}
	}
	// `cancelled`, not `failed`: the attempt did not fail, it stopped being
	// authorized. Claiming a failure AO never observed would put an outcome on
	// the ledger that nothing happened to justify.
	//
	// The claim form matches only `finished_at IS NULL`, so two passes produce
	// one conclusion and the loser is a no-op rather than an overwriter.
	ok, err := c.store.ClaimWorkflowAttemptOutcome(ctx, attempt.ID, c.clock(),
		domain.WorkflowAttemptCancelled, domain.WorkflowErrorSuperseded)
	if err != nil {
		return false, err
	}
	if ok && c.log != nil {
		c.log.Info("workflow: closed a superseded fix attempt",
			"run", run.ID, "step", step.ID, "attempt", attempt.ID,
			"cycle", cycleNumber, "proof", verdict.Proof)
	}
	return ok, nil
}

// fixAttemptSupersessionRecorded reports whether this attempt already has its
// one record, so a re-entered pass writes no second one.
func (c *Coordinator) fixAttemptSupersessionRecorded(ctx stdctx.Context, runID, attemptID string) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Unreadable: claim it is recorded, so nothing is written twice. The
		// closure below is idempotent on its own.
		return true
	}
	for _, cp := range cps {
		if cp.DurablePhase != fixAttemptSupersededPhase {
			continue
		}
		var rec fixAttemptSupersededRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.AttemptID == attemptID {
			return true
		}
	}
	return false
}
