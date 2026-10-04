package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// repair_branch_cession.go — P1-D §L: letting a Repair Agent work on
// direct_branch, safely.
//
// P1-B refused outright. In direct-branch mode the stopped run holds the
// repository's branch lock for the whole of its life, so a repair run created
// against the same project would queue behind the very run it exists to
// unblock — a deadlock. Refusing was correct and it left the most common
// single-developer setup unable to use the feature at all.
//
// The fix is not to weaken the lock. It is to make the authority MOVE, on
// terms both sides can prove:
//
//	original run holds the lock
//	  -> a bounded repair intent exists for a repairable stop
//	  -> the lock is CEDED to the repair run, conditioned on the original
//	     still holding it
//	  -> the original cannot mutate: it no longer holds the lock
//	  -> the repair does its bounded work
//	  -> the lock is RETURNED, conditioned on the repair still holding it AND
//	     on the repair generation still being current
//	  -> the original obligation resumes
//
// Every step is a compare-and-set on who holds it right now, and the row never
// leaves the `held` state, so there is no instant at which the branch is
// unowned and a third run could take it. There is no stealing anywhere: a pass
// working from a stale view of ownership matches zero rows and is refused.

// branchLockCeder is the store capability cession needs. Asserted at the call
// site (mirroring ambiguousPlanReopener and planRevisionRegenerator) so a store
// or test double without it refuses with a readable error rather than failing
// to compile.
type branchLockCeder interface {
	Cede(ctx stdctx.Context, lockID, fromRunID, toRunID, toStepID string) (bool, error)
}

// Durable phases for the two halves of a cession. Both are written BEFORE the
// transfer they describe, so a crash in between leaves an explanation for a
// move that may not have happened -- which is recoverable -- rather than a move
// nobody can account for, which is not.
const (
	branchLockCededPhase    = "branch_lock_ceded_to_repair"
	branchLockReturnedPhase = "branch_lock_returned_from_repair"
)

// branchCessionRecord is the durable evidence of one transfer.
type branchCessionRecord struct {
	LockID string `json:"lockId"`
	// FromRunID and ToRunID name both ends, so the ledger reads as a transfer
	// rather than as two unrelated ownership changes.
	FromRunID string `json:"fromRunId"`
	ToRunID   string `json:"toRunId"`
	// RepairIntentID and RepairGeneration are the fence. A return that names a
	// generation the run has moved past is refused: it describes a repair the
	// lifecycle has superseded, and handing a branch back on its say-so would
	// let a stale agent's authority outlive it.
	RepairIntentID   string    `json:"repairIntentId"`
	RepairGeneration int       `json:"repairGeneration"`
	Branch           string    `json:"branch"`
	RepoPath         string    `json:"repoPath"`
	At               time.Time `json:"at"`
	// Kind distinguishes a transfer the previous owner made (`ceded`) from one
	// a repair made on its origin's behalf by taking the branch itself
	// (`custody`) — see branch_cession_chain.go. Empty on every row written
	// before the distinction existed, which reads as `ceded` and is what those
	// rows are.
	Kind string `json:"kind,omitempty"`
}

// cedeBranchLockToRepair moves the originating run's direct-branch locks to the
// repair run.
//
// It returns the cessions it made. A partial cession is possible in principle
// (a workspace project registers several repositories, each with its own lock)
// and is handled the only honest way: whatever was ceded is on the ledger, and
// returnBranchLockFromRepair walks the same ledger rather than re-deriving what
// it thinks ought to have moved.
func (c *Coordinator) cedeBranchLockToRepair(ctx stdctx.Context, origin domain.WorkflowRun, intent domain.RepairIntent) ([]branchCessionRecord, error) {
	if c.branchLocks == nil {
		return nil, nil
	}
	ceder, ok := c.branchLocks.(branchLockCeder)
	if !ok {
		return nil, fmt.Errorf("%w: this branch-lock manager cannot cede a lock, so a direct-branch repair cannot be authorized", ErrInvalid)
	}
	held, err := c.branchLocks.HeldByRun(ctx, origin.ID)
	if err != nil {
		return nil, err
	}
	var ceded []branchCessionRecord
	for _, lock := range held {
		rec := branchCessionRecord{
			LockID: lock.ID, FromRunID: origin.ID, ToRunID: intent.RepairRunID,
			RepairIntentID: intent.ID, RepairGeneration: intent.Generation,
			Branch: lock.Branch, RepoPath: lock.RepoPath, At: c.clock(),
			Kind: branchCessionKindCeded,
		}
		// Reason first: the ledger row is durable BEFORE the transfer, so a
		// crash between them leaves a recorded intent to move a lock that may
		// not have moved. Reconciliation can check who holds it and finish;
		// the reverse order would leave a moved lock nobody can explain.
		c.recordBranchCession(ctx, origin, branchLockCededPhase, rec,
			fmt.Sprintf("branch %s ceded to repair run %s (generation %d)", lock.Branch, intent.RepairRunID, intent.Generation))
		moved, cerr := ceder.Cede(ctx, lock.ID, origin.ID, intent.RepairRunID, "")
		if cerr != nil {
			return ceded, cerr
		}
		if !moved {
			// Somebody else holds it now, or it is no longer held. Refused
			// rather than forced: this pass's view of ownership is stale.
			return ceded, fmt.Errorf("%w: branch %s is no longer held by run %s, so it cannot be ceded to a repair",
				ErrInvalid, lock.Branch, origin.ID)
		}
		ceded = append(ceded, rec)
	}
	return ceded, nil
}

// returnBranchLockFromRepair hands the branch back when a repair ends.
//
// Two conditions, and both are refusals rather than best-effort:
//
//   - the REPAIR must still hold the lock. A repair that already lost it (a
//     restart adopted it elsewhere, an operator intervened) has nothing to
//     give back, and forcing a transfer would be taking a branch from whoever
//     legitimately holds it now.
//   - the repair generation must still be the run's CURRENT one. A superseded
//     repair returning a branch would hand authority back on the say-so of an
//     agent the lifecycle has already replaced.
//
// Idempotent: a second return finds the lock already with the origin, matches
// zero rows, and reports that nothing moved -- which is the truth.
func (c *Coordinator) returnBranchLockFromRepair(ctx stdctx.Context, origin domain.WorkflowRun, intent domain.RepairIntent) error {
	if c.branchLocks == nil {
		return nil
	}
	ceder, ok := c.branchLocks.(branchLockCeder)
	if !ok {
		return nil
	}
	current := len(c.repairIntents(ctx, origin.ID))
	if intent.Generation < current {
		return fmt.Errorf("%w: repair generation %d has been superseded by generation %d; it may not return a branch lock",
			ErrInvalid, intent.Generation, current)
	}
	for _, rec := range c.cededBranchLocks(ctx, origin.ID, intent) {
		// The transfer FIRST, the record second, which is the opposite of the
		// cession above and for the opposite reason. A cession's dangerous
		// crash is a branch that moved with nothing to explain it; a return's
		// is a ledger that says the branch came back when it did not — because
		// cededBranchLocks would then stop listing the cession and nothing
		// would ever hand the branch back. That was the shape of the
		// wf-c4c84f52 leak, one restart earlier. Recording second cannot lose
		// a branch: completeBranchCessionBookkeeping re-derives the missing row
		// from the lock table, which is the authority on who holds what.
		moved, err := ceder.Cede(ctx, rec.LockID, intent.RepairRunID, origin.ID, "")
		if err != nil {
			return err
		}
		if !moved {
			// The repair does not hold it: already returned, released, or
			// somebody else's now. Nothing is forced, and the bookkeeping pass
			// closes the row once the lock table settles the question.
			continue
		}
		c.recordBranchCession(ctx, origin, branchLockReturnedPhase, rec,
			fmt.Sprintf("branch %s returned to run %s from repair run %s", rec.Branch, origin.ID, intent.RepairRunID))
	}
	return nil
}

// cededBranchLocks folds the ledger back into the transfers that are still
// outstanding for one repair generation: ceded, and not yet returned.
//
// It reads the ledger rather than re-deriving from the lock table on purpose.
// What has to be undone is what was actually DONE, and only the ledger knows
// that -- a lock table read would also pick up locks this repair never received.
func (c *Coordinator) cededBranchLocks(ctx stdctx.Context, runID string, intent domain.RepairIntent) []branchCessionRecord {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return nil
	}
	// Paired by IDENTITY rather than by the order the rows come back in. Two
	// checkpoints written inside one clock tick are ordered by id, and a return
	// that happens to sort before its own cession would resurrect a transfer
	// that was already given back -- which is a leak, and an invisible one.
	ceded := map[string]branchCessionRecord{}
	returned := map[string]bool{}
	for _, cp := range checkpoints {
		if cp.DurablePhase != branchLockCededPhase && cp.DurablePhase != branchLockReturnedPhase {
			continue
		}
		var rec branchCessionRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil || rec.LockID == "" {
			continue
		}
		if rec.RepairGeneration != intent.Generation {
			continue
		}
		if cp.DurablePhase == branchLockCededPhase {
			ceded[branchCessionKey(rec)] = rec
		} else {
			returned[branchCessionKey(rec)] = true
		}
	}
	out := make([]branchCessionRecord, 0, len(ceded))
	for key, rec := range ceded {
		if returned[key] {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func (c *Coordinator) recordBranchCession(ctx stdctx.Context, run domain.WorkflowRun, phase string, rec branchCessionRecord, detail string) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return
	}
	// A return's row is identified by what it is about — the lock, the repair
	// that had it, the generation — rather than by a minted id, so a racing
	// daemon or a restart completing the same bookkeeping collides on the
	// primary key instead of writing a second account of one transfer. A
	// cession keeps a minted id: each one is a distinct event, and two of them
	// for the same lock and generation cannot happen (the second Cede would be
	// refused).
	id := "wfc-" + c.newID()
	if phase == branchLockReturnedPhase {
		id = branchCessionFoldID(rec.LockID, rec.ToRunID, rec.RepairGeneration)
	}
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             id,
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		DurablePhase:   phase,
		NextAction:     detail,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	})
}
