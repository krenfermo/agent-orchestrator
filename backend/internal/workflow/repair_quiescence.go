package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// repair_quiescence.go — a repair that cannot write is not a repair in flight.
//
// THE REMAINING EXCEPTION (wf-f5025a7c, repair generation 2 of wf-724a1e97).
//
// head_convergence.go refuses to converge an origin run automatically while a
// repair generation is "in flight", and it defined in flight as "the repair run
// is not terminal". That was too coarse by exactly one state. needs_attention
// is not terminal, so a repair parked on a HUMAN decision — which can launch
// nothing, write nothing and move nothing until a person acts — held its origin
// shut forever. The origin then needed a person to press Continue, which is the
// precise dependency this whole piece of work exists to remove: wf-724a1e97
// still cannot adopt 247d3bc5f by itself, because wf-f5025a7c is parked.
//
// THE FIX IS NOT "IGNORE PARKED REPAIRS". A repair parked on a human decision
// may still own a live agent, a held runtime slot, a ceded branch and an
// unfinished dispatch — "the run row says needs_attention" is a statement about
// a row, not about the machine. Reading it as permission would let AO point a
// reviewer at a worktree a repair agent is actively writing, which is worse than
// the deadlock it replaces.
//
// So quiescence is PROVEN, never assumed, and every clause below is a durable
// fact re-derived at the moment it is asked. The default is refusal: a proof
// that cannot be read is not a proof, and the run stays repair_active.
//
//	 1. the repair run is parked in needs_attention;
//	 2. its stop is human-owned, and AO has no automatic transition pending for
//	    it — not self-remediable, and no durable wake scheduled;
//	 3. no step of the repair is ready or running, so nothing of its own is
//	    executing or authorized to execute;
//	 4. it holds no capacity claim that represents a live mutating execution,
//	    and none queued that could become one — which is the structural half of
//	    "no runtime is writing", because this codebase's standing invariant is
//	    NO RUNTIME LAUNCH WITHOUT AN AUTHORITATIVE CAPACITY CLAIM
//	    (capacity_scheduler.go);
//	 5. every session it owns is provably not a live writer: terminated, or
//	    idle and silent past the same settle window human_applied_fix.go uses —
//	    which is the session half of the same question, and it is required as
//	    well as (4) rather than instead of it;
//	 6. every branch it was ceded is still held BY IT, so the fold can hand it
//	    back under compare-and-set rather than taking it from whoever holds it
//	    now;
//	 7. no outbox entry of its own is pending or dispatched, so no launch is
//	    queued or in flight;
//	 8. it is the run's CURRENT repair generation, so no superseded callback is
//	    what this pass is acting on.
//
// WHAT THE FOLD DOES, AND WHAT IT REFUSES TO DO. It returns the branch,
// generation-conditioned, and records the generation as QUIESCENT. It does not
// record it resolved — nothing was resolved, the repair did not repair anything
// and a person still owns it. It spends no repair generation. It moves no
// lifecycle. All it does is stop a parked repair from being mistaken for a
// running one, so the ORIGIN may go and evaluate its own workspace under its own
// unchanged rules — which, if a new authoritative head is there, means exactly
// one fresh review of it.
//
// AND IT IS NOT A LATCH. Quiescence is re-derived every time it is asked, never
// read back from the checkpoint it writes. A person who continues the parked
// repair makes it live again in the same instant, and the next pass reports it
// active — because the proof, not the record, is what answers the question.

// repairQuiescentPhase records one generation observed quiescent. It is
// deliberately NOT repairResolvedPhase: resolvedRepairGenerations must keep
// reconsidering this generation, because its real outcome has not happened yet.
const repairQuiescentPhase = "workflow_repair_quiescent"

// repairQuiescence is what the proof concluded, with its working shown.
type repairQuiescence struct {
	// Quiescent is true only when every clause below held at this instant.
	Quiescent bool
	// Reason names the first clause that failed, or summarises the proof when
	// none did. It is what the ledger and the log record, so a refusal is
	// explainable without re-running anything.
	Reason string
	// Proved lists the clauses that passed, in order, so the durable record of
	// a fold says what was actually established rather than merely asserting
	// the conclusion.
	Proved []string
}

// proveRepairQuiescent answers "can this repair generation still write?".
//
// It reads only. Its default is "no", and every early return names the fact it
// could not establish.
func (c *Coordinator) proveRepairQuiescent(
	ctx stdctx.Context, origin domain.WorkflowRun, intent domain.RepairIntent, repairRun domain.WorkflowRun,
) repairQuiescence {
	out := repairQuiescence{}
	no := func(format string, args ...any) repairQuiescence {
		out.Quiescent = false
		out.Reason = fmt.Sprintf(format, args...)
		return out
	}
	proved := func(clause string) { out.Proved = append(out.Proved, clause) }

	// (8) first, because everything after it is about the right generation.
	// A superseded intent describes a repair the lifecycle has already moved
	// past, and acting on its say-so is exactly the stale-writer class this
	// codebase fences everywhere else.
	if current := len(c.repairIntents(ctx, origin.ID)); intent.Generation != current {
		return no("repair generation %d is not this run's current generation (%d), so it is not what a fold may act on",
			intent.Generation, current)
	}
	proved("generation is current")

	// (1) parked, not merely non-terminal.
	if repairRun.State != domain.WorkflowRunNeedsAttention {
		return no("repair run %s is %s, not parked for a person", intent.RepairRunID, repairRun.State)
	}
	proved("repair run is parked in needs_attention")

	// (2) the stop is a person's, and AO is not about to do something about it.
	reason, disp, known := c.stopReason(ctx, repairRun)
	if !known {
		return no("AO has no durable record of why repair run %s stopped", intent.RepairRunID)
	}
	if disp.HumanAction == "" {
		return no("repair run %s is stopped on %q, which is not a human-owned decision", intent.RepairRunID, reason)
	}
	if disp.SelfRemediable {
		return no("repair run %s is stopped on %q, which AO is still remediating itself", intent.RepairRunID, reason)
	}
	if c.wakeScheduler != nil {
		next, werr := c.wakeScheduler.NextForRun(ctx, domain.WorkflowRunID(intent.RepairRunID))
		if werr != nil {
			return no("AO could not read repair run %s's wake schedule, so it cannot prove no automatic transition is pending", intent.RepairRunID)
		}
		if next != nil {
			return no("repair run %s has a %s wake scheduled, so an automatic transition is still pending", intent.RepairRunID, next.Reason)
		}
	}
	proved("stop is human-owned with no automatic transition pending")

	// (3) nothing of its own is executing or authorized to execute. Every step
	// kind counts, not only the obviously-mutating ones: a verify step runs the
	// project's own commands, and an advance step writes to a branch.
	steps, err := c.store.ListWorkflowSteps(ctx, intent.RepairRunID)
	if err != nil {
		return no("AO could not read repair run %s's steps, so it cannot prove nothing of its own is executing", intent.RepairRunID)
	}
	for _, step := range steps {
		if step.State == domain.WorkflowStepReady || step.State == domain.WorkflowStepRunning {
			return no("repair run %s's %s step is %s, so it is executing or authorized to execute",
				intent.RepairRunID, step.Kind, step.State)
		}
	}
	proved("no step is ready or running")

	// (4) no capacity claim that is, or could become, a live mutating execution.
	if c.capacity == nil {
		return no("this configuration has no capacity ledger, so AO cannot prove repair run %s holds no live execution slot", intent.RepairRunID)
	}
	claims, err := c.capacity.ListCapacityClaimsForRun(ctx, intent.RepairRunID)
	if err != nil {
		return no("AO could not read repair run %s's capacity claims", intent.RepairRunID)
	}
	for _, claim := range claims {
		switch claim.State {
		case domain.CapacityClaimHeld:
			if executionKindMutates(claim.Kind) {
				return no("repair run %s holds a %s capacity claim (%s), which represents a live mutating execution",
					intent.RepairRunID, claim.Kind, claim.DispatchKey)
			}
		case domain.CapacityClaimQueued:
			// A queued claim is a launch waiting for room. It is not writing
			// yet and it is exactly what would start writing the moment a slot
			// frees, with nobody in this path watching.
			return no("repair run %s has a queued %s capacity claim (%s), which can still become a launch",
				intent.RepairRunID, claim.Kind, claim.DispatchKey)
		}
	}
	proved("holds no live or queued mutating capacity claim")

	// (5) the session half of the same question, required as well as (4). A
	// capacity ledger proves what AO authorized; a session proves what is
	// actually there. Neither substitutes for the other.
	if c.sessionFacts == nil {
		return no("this configuration has no session facts, so AO cannot prove repair run %s owns no live writer", intent.RepairRunID)
	}
	now := c.clock()
	for _, sessionID := range c.runtimeOwningSessionsForRun(ctx, repairRun, steps) {
		rec, found, serr := c.sessionFacts.GetSession(ctx, sessionID)
		if serr != nil {
			return no("AO could not read session %s to prove repair run %s owns no live writer", sessionID, intent.RepairRunID)
		}
		if !found {
			// A step names a session whose row is not there. That is an
			// inconsistency, not an absence of writers, and there is nothing
			// left to prove anything with — so it is a refusal, like every
			// other missing fact here.
			return no("repair run %s's step names session %s, which AO cannot read, so it cannot prove there is no live writer",
				intent.RepairRunID, sessionID)
		}
		if rec.IsTerminated {
			// Recorded terminated: not a writer, and durably so.
			continue
		}
		if agentMayStillBeDelivering(rec, now) {
			return no("repair run %s's session %s is active or has spoken within the settle window, so it may still be writing",
				intent.RepairRunID, sessionID)
		}
	}
	proved("every session it owns is terminated or provably silent")

	// (6) the branches it was ceded are still ITS OWN, so the fold's return is a
	// compare-and-set on ownership rather than a seizure from whoever holds
	// them now.
	if c.branchLocks != nil {
		outstanding := c.cededBranchLocks(ctx, origin.ID, intent)
		if len(outstanding) > 0 {
			held, herr := c.branchLocks.HeldByRun(ctx, intent.RepairRunID)
			if herr != nil {
				return no("AO could not read which branches repair run %s holds", intent.RepairRunID)
			}
			heldIDs := map[string]struct{}{}
			for _, lock := range held {
				heldIDs[lock.ID] = struct{}{}
			}
			for _, rec := range outstanding {
				if _, ok := heldIDs[rec.LockID]; !ok {
					return no("branch %s was ceded to repair run %s but is no longer held by it, so who owns it now is not provable",
						rec.Branch, intent.RepairRunID)
				}
			}
		}
		proved("every ceded branch is still held by this repair generation")
	}

	// (7) nothing queued or in flight in its own outbox.
	entries, err := c.store.ListWorkflowOutboxByRun(ctx, intent.RepairRunID)
	if err != nil {
		return no("AO could not read repair run %s's dispatch outbox", intent.RepairRunID)
	}
	for _, entry := range entries {
		switch entry.Status {
		case domain.WorkflowOutboxPending:
			return no("repair run %s has a pending %s dispatch (%s), which can still launch work",
				intent.RepairRunID, entry.CommandType, entry.IdempotencyKey)
		case domain.WorkflowOutboxDispatched:
			return no("repair run %s has a dispatched-but-unacknowledged %s command (%s), so a launch may be in flight",
				intent.RepairRunID, entry.CommandType, entry.IdempotencyKey)
		}
	}
	proved("no dispatch is pending or in flight")

	out.Quiescent = true
	out.Reason = fmt.Sprintf(
		"repair generation %d (run %s) is parked on %q and cannot write: %s",
		intent.Generation, intent.RepairRunID, reason, strings.Join(out.Proved, "; "))
	return out
}

// executionKindMutates reports whether a held slot of this kind represents an
// execution that can change the workspace.
//
// A reviewer is the one kind that cannot: it reads a tree and submits a verdict,
// and refusing quiescence for a reviewer slot would mean a leaked reviewer claim
// could hold an origin shut forever for a reason that is not about writing.
// Everything else — and anything AO adds later that this switch does not know —
// is treated as mutating.
func executionKindMutates(kind domain.ExecutionKind) bool {
	return kind != domain.ExecutionKindReviewer
}

// foldQuiescentRepair records a proven-quiescent generation and gives the branch
// back, so the origin can go and look at its own workspace again.
//
// It is idempotent over its own ledger row, which is what makes two concurrent
// reconciles one transition: the second finds the generation already recorded
// quiescent and returns without writing or moving anything. The branch return is
// separately idempotent (cededBranchLocks stops listing a lock once its return
// is on the ledger) and separately generation-conditioned, so neither half can
// be applied on a superseded generation's say-so.
//
// It deliberately performs no lifecycle transition of its own. The origin is
// still parked on its own stop; what changes is only that a parked repair stops
// being counted as a live one.
func (c *Coordinator) foldQuiescentRepair(
	ctx stdctx.Context, origin domain.WorkflowRun, intent domain.RepairIntent, proof repairQuiescence,
) {
	if c.quiescentRepairGenerations(ctx, origin.ID)[intent.Generation] {
		return
	}
	// The branch first, and only then the record: a crash between them leaves a
	// branch already back with its origin and no row claiming it moved, which
	// the next pass re-derives and completes. The reverse order would leave a
	// record asserting a return that never happened.
	if err := c.returnBranchLockFromRepair(ctx, origin, intent); err != nil {
		if c.log != nil {
			c.log.Warn("workflow: a quiescent repair could not return its branch lock; not folding it",
				"run", origin.ID, "repair", intent.RepairRunID, "generation", intent.Generation, "err", err)
		}
		return
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: origin.ID,
		ProjectID:     origin.ProjectID,
		DurablePhase:  repairQuiescentPhase,
		NextAction: fmt.Sprintf(
			"repair generation %d (run %s) is parked for a person and can no longer write; its branch is back with this run and it no longer blocks an independent review of the current workspace — it is NOT resolved, and no repair budget was spent",
			intent.Generation, intent.RepairRunID),
		PayloadVersion: "v1",
		RetryState: fmt.Sprintf(`{"generation":%d,"repairRunId":%q,"outcome":"quiescent","proof":%q}`,
			intent.Generation, intent.RepairRunID, proof.Reason),
		CreatedAt: c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording a quiescent repair failed", "run", origin.ID, "err", err)
		return
	}
	if c.log != nil {
		c.log.Info("workflow: repair generation observed quiescent",
			"run", origin.ID, "repair", intent.RepairRunID, "generation", intent.Generation,
			"proof", proof.Reason)
	}
}

// quiescentRepairGenerations folds the ledger's quiescence rows.
//
// A read failure returns every generation as ALREADY folded, which refuses the
// fold rather than repeating it — the same direction humanAppliedFixState fails
// in, and for the same reason: an unreadable ledger must never look like "not
// yet done".
func (c *Coordinator) quiescentRepairGenerations(ctx stdctx.Context, runID string) map[int]bool {
	out := map[int]bool{}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		for _, intent := range c.repairIntents(ctx, runID) {
			out[intent.Generation] = true
		}
		return out
	}
	for _, cp := range cps {
		if cp.DurablePhase != repairQuiescentPhase {
			continue
		}
		var body struct {
			Generation int `json:"generation"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &body) == nil && body.Generation > 0 {
			out[body.Generation] = true
		}
	}
	return out
}

// reconcileQuiescentRepair proves and folds, in that order, for the run's
// current repair generation.
//
// It is the only caller of foldQuiescentRepair, and it is reached from the same
// two automatic places converge() is: the objective's own reconcile pass and
// boot recovery. Nobody presses anything.
func (c *Coordinator) reconcileQuiescentRepair(ctx stdctx.Context, origin domain.WorkflowRun) repairQuiescence {
	intents := c.repairIntents(ctx, origin.ID)
	if len(intents) == 0 {
		return repairQuiescence{Reason: "this run has no repair generations"}
	}
	intent := intents[len(intents)-1]
	if intent.RepairRunID == "" {
		return repairQuiescence{Reason: "the newest repair intent names no repair run"}
	}
	repairRun, found, err := c.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if err != nil || !found {
		return repairQuiescence{Reason: fmt.Sprintf("AO could not read repair run %s", intent.RepairRunID)}
	}
	if repairRun.State.Terminal() {
		// A terminal repair is reconcileRepairOutcome's business, not this
		// file's: it has a real outcome to fold, and calling it quiescent would
		// mislabel a repair that finished as one that merely stopped.
		return repairQuiescence{Reason: fmt.Sprintf("repair run %s is %s, which is an outcome rather than a quiescence", intent.RepairRunID, repairRun.State)}
	}
	proof := c.proveRepairQuiescent(ctx, origin, intent, repairRun)
	if proof.Quiescent {
		c.foldQuiescentRepair(ctx, origin, intent, proof)
	}
	return proof
}
