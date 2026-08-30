package workflow

import (
	stdctx "context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// head_convergence.go — a review's authority ends at the state it reviewed.
//
// THE INCIDENT (wf-724a1e97, the first real P2-A workflow).
//
//	00:39  fix_budget_exhausted            after 3 fix cycles
//	00:43  workflow_repair_dispatched      generation 1
//	01:04  workflow_repair_dispatched      generation 2
//	01:21  human_applied_fix_observed      HEAD ccefd07b0
//	01:21  review 33c08c40 dispatched      target ccefd07b0
//	01:23  changes_requested               -> fix_budget_exhausted again
//	  ...  247d3bc5f committed on the branch, fixing that exact finding
//	  ...  nothing. Forever.
//
// The last authoritative review looked at ccefd07b0 and said "changes
// requested". The branch then moved to 247d3bc5f, which is the change that
// review asked for. AO held the run shut on a verdict about a commit that was
// no longer the head of anything.
//
// THE DEFECT is not that AO refused to assume 247d3bc5f was correct — refusing
// that is right, and this file does not weaken it. The defect is that AO had
// exactly one transition able to notice a post-stop state change
// (resumeHumanAppliedFix) and it was reachable ONLY from a person's direct
// ContinueRun on the child run:
//
//   - the objective's own reconcile calls ContinueRun for a child only when its
//     review step is pending/ready, and a budget-exhausted child rests at
//     waiting (master_coordinator.go);
//   - and it refuses a child whose stop is human-owned at all, which
//     fix_budget_exhausted is by disposition (attention.go).
//
// So the parent's heartbeat ran 185 times over the incident and could not have
// converged the child on any of them. The one recovery that did happen, at
// 01:21, happened because a person pressed the button.
//
// THE RULE, stated as an invariant:
//
//	A changes_requested verdict has authority over exactly the workspace state
//	it reviewed. When AO can prove the run's own workspace has moved to a state
//	no review has judged, that verdict may no longer be the reason the run is
//	stopped — and the answer is ONE fresh authoritative review of the new state,
//	never an implicit approval of it.
//
// WHAT THIS FILE ADDS is a read-only probe and two callers, and deliberately
// nothing else. It does not mutate; it routes. When it says "converge", the
// caller enters ContinueRun — the same single writer a person's button enters —
// so the fingerprint-keyed idempotence in human_applied_fix.go remains the only
// thing standing between one new state and one new review. Two concurrent
// resumes, a poll racing a restart, and a repair completing while the heartbeat
// fires all reduce to the same one write.

// freshReviewConvergence is the probe's answer. Every field is derived now, and
// the negative answers carry their reason so a stop can be explained rather
// than merely observed.
type freshReviewConvergence struct {
	// Converge is true when the run is being held on a superseded review of a
	// state its workspace has moved past, and nothing else owns the workspace.
	Converge bool
	// Reason is AO's own sentence about the answer, positive or negative.
	Reason string
	// SupersededReview is the review whose authority no longer describes the
	// workspace, when there is one.
	SupersededReview string
	// TargetFingerprint is the unreviewed state a fresh review would judge.
	TargetFingerprint string
	// RepairActive reports that a repair generation for this run is still in
	// flight. It is the one refusal that is not a defect: the repair owns the
	// branch, and converging underneath it would review a tree a repair agent
	// is still writing into.
	RepairActive bool
	// RepairRunID/RepairGeneration/RepairBudget describe that repair, for the
	// API surface a person reads.
	RepairRunID      string
	RepairGeneration int
	RepairBudget     int
}

// probeFreshReviewConvergence answers "is this run being held shut by a review
// of a state it has moved past?" without changing anything.
//
// Entry points differ in ONE respect only, and it is the repair gate: a person
// asking for this explicitly may converge a run whose repair is still running
// (they can see both, and their instruction is newer than AO's), while an
// automatic caller may not, because a repair agent writing into the same
// worktree makes every fingerprint it reads a moving target. Everything else —
// every provenance, ownership, idempotence and settle-window proof — is
// identical, because it is literally the same evaluation.
func (c *Coordinator) probeFreshReviewConvergence(
	ctx stdctx.Context, run domain.WorkflowRun, humanInitiated bool,
) (freshReviewConvergence, error) {
	out := freshReviewConvergence{RepairBudget: policyForRun(run).EffectiveRepairPolicy().MaxRepairCycles}

	intent, active, proof := c.repairFlightState(ctx, run.ID)
	if active {
		out.RepairActive = true
		out.RepairRunID = intent.RepairRunID
		out.RepairGeneration = intent.Generation
		if !humanInitiated {
			why := proof.Reason
			if why == "" {
				why = "it has not reached a terminal state"
			}
			out.Reason = fmt.Sprintf(
				"repair generation %d (run %s) can still act, so AO will not open a review of a tree that may still be written: %s",
				intent.Generation, intent.RepairRunID, why)
			return out, nil
		}
	}

	decision, err := c.evaluateExternalAppliedFix(ctx, run)
	if err != nil {
		return out, err
	}
	out.Reason = decision.Reason
	if !decision.Adoptable {
		return out, nil
	}
	out.Converge = true
	out.SupersededReview = decision.PreviousReview.ID
	out.TargetFingerprint = decision.NewFingerprint
	out.Reason = fmt.Sprintf(
		"review %s speaks for workspace state %s, but this run's workspace is now %s — one fresh authoritative review of the current state is due",
		decision.PreviousReview.ID, shortFingerprint(decision.OldFingerprint), shortFingerprint(decision.NewFingerprint))
	return out, nil
}

// repairFlightState reports the run's newest repair generation, whether it can
// still act, and the quiescence proof behind that answer.
//
// Only the NEWEST generation can be in flight: reconcileRepairOutcome resolves
// every earlier one, and a run may hold at most one live repair by construction
// (repair_agent.go's evidence-digest single-flight). Reading only the newest is
// therefore not a shortcut, it is the invariant restated.
//
// "Can still act" is deliberately NOT "the run row is non-terminal". That was
// the coarse test wf-f5025a7c fell through: a repair parked on a human decision
// is not terminal, cannot write anything, and under the old rule held its origin
// shut until somebody pressed Continue. A generation PROVEN quiescent
// (repair_quiescence.go) is not in flight — and the proof is re-derived here on
// every call rather than read back from the fold's checkpoint, so a person who
// continues that repair makes it live again in the same instant.
//
// Its default is "no repair in flight" on a read failure, which is safe HERE
// and only here: the caller's next move is the ordinary evidence-gated
// ContinueRun, every guard of which re-derives its own preconditions and
// refuses on ambiguity. A false "not in flight" costs a refused probe, never a
// write. The quiescence proof itself fails in the opposite direction, refusing
// on anything it cannot establish.
func (c *Coordinator) repairFlightState(
	ctx stdctx.Context, runID string,
) (domain.RepairIntent, bool, repairQuiescence) {
	intents := c.repairIntents(ctx, runID)
	for i := len(intents) - 1; i >= 0; i-- {
		intent := intents[i]
		if intent.RepairRunID == "" {
			continue
		}
		run, found, err := c.store.GetWorkflowRun(ctx, intent.RepairRunID)
		if err != nil || !found {
			return domain.RepairIntent{}, false, repairQuiescence{}
		}
		if run.State.Terminal() {
			return intent, false, repairQuiescence{}
		}
		origin, ok, oerr := c.store.GetWorkflowRun(ctx, runID)
		if oerr != nil || !ok {
			// Cannot read the origin, so cannot prove anything about its
			// repair. The repair stays in flight, which is the refusal.
			return intent, true, repairQuiescence{Reason: "AO could not read the origin run to prove its repair is quiescent"}
		}
		proof := c.proveRepairQuiescent(ctx, origin, intent, run)
		return intent, !proof.Quiescent, proof
	}
	return domain.RepairIntent{}, false, repairQuiescence{}
}

// converge routes a run whose authoritative review has been outlived by its own
// workspace back into the single mutating path.
//
// It returns the refreshed detail when it acted, so a caller that already holds
// one does not have to guess whether to re-read. When it does not act it
// returns (nil, nil) and the caller's own view is still correct.
func (c *Coordinator) converge(ctx stdctx.Context, run domain.WorkflowRun) (*RunDetail, error) {
	// A repair parked for a person, that has been PROVEN unable to write, is
	// folded first: its branch goes back to this run under compare-and-set and
	// the generation is recorded quiescent (never resolved — nothing was
	// resolved, and no repair budget is spent). Only then is the probe asked,
	// so the origin gets to evaluate its own workspace instead of waiting on a
	// repair that stopped hours ago. See repair_quiescence.go for the eight
	// facts this requires and for why every one of them fails closed.
	c.reconcileQuiescentRepair(ctx, run)

	probe, err := c.probeFreshReviewConvergence(ctx, run, false)
	if err != nil {
		return nil, err
	}
	if !probe.Converge {
		return nil, nil
	}
	if c.log != nil {
		c.log.Info("workflow: converging a run held on a superseded review",
			"run", run.ID, "supersededReview", probe.SupersededReview,
			"target", shortFingerprint(probe.TargetFingerprint))
	}
	// ContinueRun, not a private write: resumeHumanAppliedFix lives inside it,
	// keyed by the new fingerprint, and every other resume rule it runs first
	// gets its ordinary chance to be the better explanation. Two callers
	// reaching this at once therefore produce one adoption and one review, not
	// two of either.
	detail, err := c.ContinueRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	return &detail, nil
}

// repairLifecycleFor folds this run's repair ledger into the projection the API
// and the UI read.
//
// Every value is durable and re-derived; none of it is stored. Its failure mode
// is the zero value, which reads as "no repair", because reporting a repair AO
// cannot prove exists would be worse than reporting none.
func (c *Coordinator) repairLifecycleFor(ctx stdctx.Context, run domain.WorkflowRun) RepairLifecycle {
	out := RepairLifecycle{Budget: policyForRun(run).EffectiveRepairPolicy().MaxRepairCycles}
	intents := c.repairIntents(ctx, run.ID)
	if len(intents) > 0 {
		newest := intents[len(intents)-1]
		out.Attempt = newest.Generation
		out.RunID = newest.RepairRunID
	}
	intent, active, proof := c.repairFlightState(ctx, run.ID)
	out.QuiescenceReason = proof.Reason
	if intent.RepairRunID != "" {
		out.Attempt = intent.Generation
		out.RunID = intent.RepairRunID
	}
	out.Active = active
	out.Quiescent = proof.Quiescent
	out.Exhausted = out.Budget > 0 && !out.Active && c.repairsSpentFor(ctx, run.ID) >= out.Budget
	out.WaitingForFreshReview = c.waitingForFreshAuthoritativeReview(ctx, run.ID)
	return out
}

// waitingForFreshAuthoritativeReview reports the state between "AO adopted a
// change nobody has reviewed" and "a reviewer answered about it".
//
// Read from the ledger's ORDER rather than from a flag: the newest
// human_applied_fix_observed is later than the newest fix_budget_exhausted
// exactly when the adoption is the run's current situation, and the review step
// being live is what makes it a wait rather than a memory. A flag would be one
// more thing that can be true while the run is somewhere else.
func (c *Coordinator) waitingForFreshAuthoritativeReview(ctx stdctx.Context, runID string) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	var adopted, stopped int64
	for _, cp := range cps {
		switch cp.DurablePhase {
		case humanAppliedFixPhase:
			if n := cp.CreatedAt.UnixNano(); n > adopted {
				adopted = n
			}
		case ReasonFixBudgetExhausted:
			if n := cp.CreatedAt.UnixNano(); n > stopped {
				stopped = n
			}
		}
	}
	if adopted == 0 || adopted <= stopped {
		return false
	}
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return false
	}
	for _, step := range steps {
		if step.Kind == domain.WorkflowStepReview {
			return step.State == domain.WorkflowStepRunning || step.State == domain.WorkflowStepReady
		}
	}
	return false
}
