package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// This file is the workflow half of Checkpoint 8P-E.13A's branch-lock
// lifecycle: who is allowed to keep holding a branch, and what happens the
// moment one is freed. The policy that consumes it lives in
// internal/branchlock/retention.go; what lives here is the one judgement only
// this package can make — "is this stopped run one AO will resume, and does it
// have uncommitted work the branch has to protect?".

// LockOwnerDisposition is what the workflow layer knows about a branch-lock
// owner that is parked in needs_attention. It is deliberately a workflow type,
// mirrored (not imported) at the branchlock boundary by the daemon adapter,
// following this package's convention of owning its own port types.
type LockOwnerDisposition struct {
	// SelfRemediable means AO still has a durable plan to resume this run.
	SelfRemediable bool
	// ProtectsWork means the run has already put changes into the repository
	// that only it knows about, so releasing its branch would let a second
	// workflow write on top of them.
	ProtectsWork bool
	// Reason is the canonical attention reason for the stop, or
	// unclassifiedStop when nothing durable named it.
	Reason string
}

// ClassifyLockOwner answers, for one stopped run, the two questions branch-lock
// retention turns on.
//
// SelfRemediable reuses the exact attention vocabulary the Board renders, so a
// branch queued behind a retrying planner and a Board card saying "AO is
// handling this" can never disagree: they are the same lookup.
//
// ProtectsWork is evidence-based, never a guess. A run that has attached a
// worker or fix session to the repository may have left edits in the working
// tree that exist nowhere else, and AO has no way to tell those apart from a
// clean tree without asking git — so the branch stays locked and the situation
// is reported. A run that never got a session onto the branch (the exact shape
// of a run that stopped while still queued for the lock) has produced nothing
// to protect, and its lock is safe to reclaim.
//
// Note what is NOT evidence here: how long the lock has been held, whether the
// session is idle, or whether the run "looks" abandoned. A lock is released
// because of what the run durably is, never because of how long it has been
// that way.
func (c *Coordinator) ClassifyLockOwner(ctx stdctx.Context, run domain.WorkflowRun) (LockOwnerDisposition, error) {
	reason, disp, ok := c.stopReason(ctx, run)
	if !ok {
		reason = unclassifiedStop
	}
	out := LockOwnerDisposition{
		SelfRemediable: ok && disp.SelfRemediable,
		Reason:         reason,
	}
	protects, err := c.runHasUncommittedWork(ctx, run)
	if err != nil {
		return LockOwnerDisposition{}, err
	}
	out.ProtectsWork = protects
	return out, nil
}

// runHasUncommittedWork reports whether a run ever got a worker session onto
// its repository, which is the durable evidence that it may be holding changes
// only it knows about.
//
// The plan step is excluded on purpose: planning writes an artifact row, never
// the working tree. Review is excluded for the same reason — a reviewer session
// is launched read-only (see the reviewer adapter's allowlist), so a run that
// only ever reviewed has changed nothing.
func (c *Coordinator) runHasUncommittedWork(ctx stdctx.Context, run domain.WorkflowRun) (bool, error) {
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return false, err
	}
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepWork, domain.WorkflowStepFix:
		default:
			continue
		}
		if s.SessionID != nil && *s.SessionID != "" {
			return true, nil
		}
		if cp, ok, cerr := c.store.GetLatestWorkflowCheckpointByStep(ctx, s.ID); cerr == nil && ok {
			if cp.SessionID != nil && *cp.SessionID != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

// syncCancelledTask mirrors a cancelled child run onto its parent's task row
// and lets the parent look at itself again.
//
// Deliberately best-effort and deliberately narrow: it only ever moves a
// `running` task to `cancelled`, the exact transition reconcileMasterTasksOnce
// would make on its own next pass, so running both cannot produce a state
// neither would have produced alone.
func (c *Coordinator) syncCancelledTask(ctx stdctx.Context, run domain.WorkflowRun) {
	if c.planStore == nil || run.PlannedTaskID == nil || *run.PlannedTaskID == "" {
		return
	}
	if _, err := c.planStore.UpdateWorkflowTaskState(ctx, *run.PlannedTaskID,
		domain.WorkflowTaskRunning, domain.WorkflowTaskCancelled, c.clock()); err != nil && c.log != nil {
		c.log.Warn("workflow: cancelled task sync failed", "run", run.ID, "task", *run.PlannedTaskID, "err", err)
	}
	if run.ParentWorkflowID != nil && *run.ParentWorkflowID != "" {
		c.maybeScheduleAutonomousHeartbeat(ctx, *run.ParentWorkflowID)
	}
}

// enrichBranchWait resolves, at read time, the part of a branch wait that the
// checkpoint written when the run parked could not know: what the holder is
// doing now, and therefore whether this wait clears by itself.
//
// This is what stops the Board from having to choose between two wrong
// answers. Before it, a queued run could only say "waiting for branch X, held
// by workflow Y" — true, but silent about whether Y was a run about to finish
// or a run that stopped nineteen hours ago. Both looked identical, and only one
// of them was something a person needed to know about.
func (c *Coordinator) enrichBranchWait(ctx stdctx.Context, wait *BranchWait) {
	if wait == nil || wait.HeldByWorkflowRunID == "" {
		return
	}
	holder, ok, err := c.store.GetWorkflowRun(ctx, wait.HeldByWorkflowRunID)
	if err != nil {
		return
	}
	if !ok {
		wait.HeldByReason = "the owning workflow no longer exists; the branch is reclaimed automatically"
		wait.AutoResume = true
		return
	}
	wait.HeldByState = string(holder.State)
	switch {
	case holder.State.Terminal():
		wait.HeldByReason = "the owning workflow is " + string(holder.State) + "; the branch is reclaimed automatically"
		wait.AutoResume = true
	case holder.State != domain.WorkflowRunNeedsAttention:
		wait.HeldByReason = "the owning workflow is still working on this branch"
		wait.AutoResume = true
	default:
		disp, derr := c.ClassifyLockOwner(ctx, holder)
		switch {
		case derr != nil:
			wait.HeldByReason = "the owning workflow has stopped and AO could not classify why"
		case disp.SelfRemediable:
			wait.HeldByReason = "the owning workflow stopped on something AO resumes by itself (" + disp.Reason + ")"
			wait.AutoResume = true
		case disp.ProtectsWork:
			wait.HeldByReason = "the owning workflow needs a human decision (" + disp.Reason +
				") and left uncommitted work in this repository, so the branch stays locked to it — continue or cancel that workflow to free it"
		default:
			wait.HeldByReason = "the owning workflow needs a human decision (" + disp.Reason +
				") but holds no uncommitted work; the branch is reclaimed automatically"
			wait.AutoResume = true
		}
	}
}

// recoverStaleBranchLockHolder gives the run blocked by a lock conflict the
// chance to reclaim a branch whose owner can never use it again.
//
// This runs on the waiting run's own dispatch path rather than on a timer,
// which is the whole design: the only workflow that needs a branch reclaimed is
// the one trying to take it, and it already knows exactly which run is in its
// way. Returns true when at least one lock was actually released, i.e. when
// retrying the acquisition is worth it.
func (c *Coordinator) recoverStaleBranchLockHolder(ctx stdctx.Context, run domain.WorkflowRun, cause error) bool {
	if c.branchLocks == nil {
		return false
	}
	holder := branchWaitFromLockError(cause).HeldByWorkflowRunID
	if holder == "" || holder == run.ID {
		return false
	}
	released, err := c.branchLocks.RecoverStale(ctx, holder)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: stale branch-lock recovery failed", "run", run.ID, "holder", holder, "err", err)
		}
		return false
	}
	if released > 0 && c.log != nil {
		c.log.Info("workflow: reclaimed stale branch lock", "run", run.ID, "holder", holder, "released", released)
	}
	return released > 0
}

// wakeBranchQueue resumes every run parked on a repository+branch that has just
// been freed.
//
// Without it a queued run still recovers — its own branch_lock wake keeps
// firing — but only after the rest of an exponential backoff of up to
// MaxBackoffSeconds, which is a wait for information AO already has. A release
// is the event the queue was waiting for, so the queue is told.
//
// Best-effort throughout, like every other wake write in this package: a run
// that fails to be woken here is not lost, it is merely slower.
// waitingOnABranch reports whether a run is in a state a branch release can
// actually unblock.
//
// Waiting is the state the current code parks a branch-queued run in. Checkpoint
// 8P-E13A.1 adds needs_attention deliberately: rows written before that
// checkpoint — and any run whose branch wait got misfiled as a stop — are
// durably parked there, and refusing to wake them means the branch frees while
// the run that was queued for it stays stopped forever. Waking a run is safe in
// either state: ContinueRun is idempotent and simply re-evaluates what is
// dispatchable, and a run whose stop has nothing to do with this branch will
// find nothing to do and park again.
func waitingOnABranch(run domain.WorkflowRun) bool {
	return run.State == domain.WorkflowRunWaiting || run.State == domain.WorkflowRunNeedsAttention
}

func (c *Coordinator) wakeBranchQueue(ctx stdctx.Context, releasedBy string, freed []domain.BranchLock) {
	if c.wakeScheduler == nil || len(freed) == 0 {
		return
	}
	keys := make(map[string]struct{}, len(freed))
	for _, lock := range freed {
		keys[domain.BranchLockKey(lock.RepoPath, lock.Branch)] = struct{}{}
	}
	runs, err := c.store.ListNonTerminalWorkflowRuns(ctx)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: branch queue wake lookup failed", "err", err)
		}
		return
	}
	for _, queued := range runs {
		if queued.ID == releasedBy || !waitingOnABranch(queued) {
			continue
		}
		cps, cerr := c.store.ListWorkflowCheckpoints(ctx, queued.ID)
		if cerr != nil {
			continue
		}
		waitingOn, stepID, ok := latestBranchWait(cps)
		if !ok {
			continue
		}
		if _, blocked := keys[domain.BranchLockKey(waitingOn.RepoPath, waitingOn.Branch)]; !blocked {
			continue
		}
		// The wake is re-scoped to the exact step that parked, so this
		// reschedules the run's existing branch_lock wake in place instead of
		// adding a second, differently-scoped row alongside it.
		if _, werr := c.wakeScheduler.WakeNow(ctx, domain.WorkflowRunID(queued.ID), stepID, wake.ReasonBranchLock); werr != nil {
			if c.log != nil {
				c.log.Warn("workflow: branch queue wake failed", "run", queued.ID, "err", werr)
			}
			continue
		}
		if c.log != nil {
			c.log.Info("workflow: woke run queued on freed branch", "run", queued.ID, "branch", waitingOn.Branch)
		}
	}
}
