package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// BranchLocks is the narrow direct-branch execution-lock surface the
// coordinator depends on (Checkpoint 8P-E.11). Satisfied by
// *branchlock.Manager. Optional: a nil BranchLocks means no run ever acquires
// a lock, which is exactly correct for an installation where every project is
// still in isolated-worktree mode, and is also what every pre-8P-E.11 test
// exercises unchanged.
type BranchLocks interface {
	Acquire(ctx stdctx.Context, req BranchLockRequest) ([]domain.BranchLock, error)
	ReleaseRun(ctx stdctx.Context, runID, reason string) (int64, error)
	HeldByRun(ctx stdctx.Context, runID string) ([]domain.BranchLock, error)
	Renew(ctx stdctx.Context, runID, stepID, sessionID string)
	// RecoverStale releases the locks one run holds that it can never use
	// again (Checkpoint 8P-E.13A). It returns how many it freed, so a caller
	// blocked by that run knows whether retrying is worth anything.
	RecoverStale(ctx stdctx.Context, runID string) (int64, error)
}

// BranchLockRequest mirrors branchlock.AcquireRequest at the workflow
// boundary, following this package's convention of depending on its own narrow
// types rather than importing an implementation package.
type BranchLockRequest struct {
	ProjectID domain.ProjectID
	RunID     string
	StepID    string
	SessionID string
	// RepoPath and Branch name the direct-branch target this run's frozen
	// placement selected, for the case the PROJECT's mode does not select one.
	// Both empty means "derive the targets from the project", which is what
	// every caller did before P3-C and what a project actually configured for
	// direct-branch execution still does.
	RepoPath string
	Branch   string
}

// WorkspaceCommitter performs the autonomous local commit for a direct-branch
// run. Satisfied by the workspace router. Optional: nil disables autonomous
// commits entirely, which is the correct pre-8P-E.11 behavior.
type WorkspaceCommitter interface {
	CommitAll(ctx stdctx.Context, info ports.WorkspaceInfo, message string) (string, bool, error)
}

// branchLockOutcome classifies an acquisition failure from the sentinel errors
// alone, so the coordinator never has to import the branchlock implementation.
type branchLockOutcome int

const (
	branchLockWaiting branchLockOutcome = iota
	branchLockBlockedDirty
	branchLockFailed
)

func classifyBranchLockError(err error) branchLockOutcome {
	switch {
	case errors.Is(err, domain.ErrBranchLockHeld):
		return branchLockWaiting
	case errors.Is(err, ports.ErrWorkspaceRepositoryDirty):
		return branchLockBlockedDirty
	default:
		return branchLockFailed
	}
}

func asBranchLockConflict(err error, target *domain.BranchLockConflictError) bool {
	return errors.As(err, target)
}

// ensureBranchLock is the gate every direct-branch work dispatch passes
// through before a session is spawned.
//
// It answers three different questions with three different, truthful states,
// which is the whole point of doing this before the outbox CAS rather than
// after:
//
//   - Acquired: the run owns every repository+branch it will write. Dispatch
//     proceeds normally.
//   - Waiting: another run owns one of them. The run moves to Waiting with a
//     waiting_for_branch checkpoint naming the branch and the owning run, and
//     a durable wake is scheduled so it resumes on its own once the lock frees.
//     It is never reported as inactive, and never as a failure.
//   - Needs attention: a target repository holds a human's uncommitted work.
//     No amount of waiting fixes that, so the run parks in NeedsAttention with
//     a dirty_worktree checkpoint that names the repositories and says what to
//     do, instead of silently mixing AO's edits into someone's work in
//     progress.
//
// Returning ok=false always means "a durable state was already recorded; the
// caller must stop without touching the outbox."
func (c *Coordinator) ensureBranchLock(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (bool, error) {
	if c.branchLocks == nil {
		return true, nil
	}
	// P3-C §28: name the repository and branch this run's FROZEN placement
	// says it will write to.
	//
	// Without it the lock manager derives its targets from the project's
	// execution MODE, and a run whose direct-branch placement was chosen
	// explicitly inside an isolated-default project produced no targets at all
	// -- which the manager reads as "nothing to lock" and reports as success.
	// The dirty-worktree gate never ran and the worker launched onto the user's
	// real branch owning nothing. See branchlock.Manager.Acquire.
	//
	// Empty for a run with no readable placement, which leaves the manager's own
	// project-derived behaviour exactly as it was.
	repoPath, branch := c.directBranchTarget(ctx, run, step)
	acquire := func() error {
		_, err := c.branchLocks.Acquire(ctx, BranchLockRequest{
			ProjectID: domain.ProjectID(run.ProjectID),
			RunID:     run.ID,
			StepID:    step.ID,
			RepoPath:  repoPath,
			Branch:    branch,
		})
		return err
	}
	err := acquire()
	// Checkpoint 8P-E.13A: a conflict is not automatically a wait. The holder
	// may be a run that stopped permanently and is protecting nothing, in which
	// case queueing behind it would be queueing forever. Ask once whether the
	// holder's lock is stale, and if it was reclaimed, take the branch now
	// rather than parking on an obstacle that no longer exists.
	if err != nil && classifyBranchLockError(err) == branchLockWaiting && c.recoverStaleBranchLockHolder(ctx, run, err) {
		err = acquire()
	}
	if err == nil {
		return true, nil
	}
	switch classifyBranchLockError(err) {
	case branchLockWaiting:
		return false, c.markRunWaitingForBranch(ctx, run, step, err)
	case branchLockBlockedDirty:
		return false, c.markRunDirtyWorktree(ctx, run, step, err)
	default:
		return false, err
	}
}

// markRunWaitingForBranch records the truthful waiting_for_branch state. It
// mirrors markRunWaitingForCapacity exactly — the step and the outbox entry
// are left untouched so no duplicate dispatch is possible, and the run moves to
// Waiting rather than to needs_attention or a synthetic failure — because the
// situations are the same shape: a temporary, self-resolving external blocker.
func (c *Coordinator) markRunWaitingForBranch(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, cause error) error {
	now := c.clock()
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunPending {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunWaiting, now); err != nil {
			return err
		}
		run.State = domain.WorkflowRunWaiting
	}
	stepID := step.ID
	wait := branchWaitFromLockError(cause)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		Branch:         wait.Branch,
		WorktreePath:   wait.RepoPath,
		NextAction:     "waiting_for_branch: " + cause.Error() + " — will resume automatically once that workflow releases the branch",
		DurablePhase:   branchWaitPhase,
		PayloadVersion: "v1",
		// The structured holder travels in RetryState so the board can render
		// "Waiting for branch X — currently used by WF-Y" from real fields
		// instead of parsing the human-readable NextAction sentence.
		RetryState: marshalBranchWait(wait),
		CreatedAt:  now,
	}); err != nil {
		return err
	}
	c.scheduleBranchLockWake(ctx, run, &step)
	return nil
}

// markRunDirtyWorktree records the needs_attention state a human has to clear.
// Deliberately NOT a wait: no wake is scheduled, because nothing about waiting
// makes someone's uncommitted changes go away, and a run that silently retried
// forever would look identical to a hung one.
func (c *Coordinator) markRunDirtyWorktree(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, cause error) error {
	now := c.clock()
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunPending || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return err
		}
	}
	stepID := step.ID
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction: "dirty_worktree: " + cause.Error() +
			" — AO will not start work over uncommitted changes it did not make. Commit, stash, or discard them, then continue this run.",
		DurablePhase:   "dirty_worktree",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	})
	return err
}

// scheduleBranchLockWake persists the wake that resumes a branch-blocked run.
// Best-effort in exactly the same way scheduleCapacityWake is: a nil scheduler
// or a scheduling error means no automatic resume, never a failed run.
// known_reset_at is always nil — a lock has no announced release time.
func (c *Coordinator) scheduleBranchLockWake(ctx stdctx.Context, run domain.WorkflowRun, step *domain.WorkflowStep) {
	if c.wakeScheduler == nil {
		return
	}
	var stepID *domain.WorkflowStepID
	if step != nil {
		id := domain.WorkflowStepID(step.ID)
		stepID = &id
	}
	if _, err := c.wakeScheduler.Schedule(ctx, domain.WorkflowRunID(run.ID), stepID, wake.ReasonBranchLock, nil); err != nil && c.log != nil {
		c.log.Warn("workflow: branch-lock wake schedule failed", "run", run.ID, "err", err)
	}
}

// releaseBranchLocks frees every lock a run holds. It is called from every
// terminal path — completion, cancellation, and the failure paths that end a
// run — because a branch left locked by a run that is over blocks every future
// run on that branch, and no later pass would know to free it.
//
// Best-effort by design: a release failure is logged, never propagated. The
// alternative would be turning a completed run into a failed one over
// bookkeeping, and boot reconciliation already releases any lock whose run is
// terminal, so a missed release is self-healing rather than permanent.
func (c *Coordinator) releaseBranchLocks(ctx stdctx.Context, runID, reason string) {
	if c.branchLocks == nil {
		return
	}
	// Read what this run holds BEFORE releasing it: once released, the rows no
	// longer answer "which branches just became free", and that is exactly the
	// question the queued runs are waiting on (Checkpoint 8P-E.13A).
	held, herr := c.branchLocks.HeldByRun(ctx, runID)
	if herr != nil && c.log != nil {
		c.log.Warn("workflow: branch lock queue lookup failed", "run", runID, "err", herr)
	}
	n, err := c.branchLocks.ReleaseRun(ctx, runID, reason)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: branch lock release failed", "run", runID, "err", err)
		}
		return
	}
	if n > 0 && c.log != nil {
		c.log.Info("workflow: branch locks released", "run", runID, "count", n, "reason", reason)
	}
	if n > 0 {
		c.wakeBranchQueue(ctx, runID, held)
	}
}

// autonomousLocalCommit performs the local commit that concludes a
// direct-branch run when the project's policy allows it (Checkpoint 8P-E.11).
//
// This is the behavioral heart of the checkpoint's autonomy rule: a routine
// local commit is part of normal autonomous completion, not a decision worth
// interrupting a human for. When the policy says automatic, AO commits and
// keeps going. When it says require_approval, AO stops and says so — a real
// decision, surfaced as one. When it says never, AO simply leaves the work
// uncommitted and records that it did, so the state is never a mystery.
//
// It returns an error only for a genuine failure to perform an allowed commit;
// "policy forbade it" and "nothing to commit" are both normal outcomes.
func (c *Coordinator) autonomousLocalCommit(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) error {
	if c.projects == nil {
		return nil
	}
	// The isolated half needs no branch lock: an AO worktree is this task's
	// alone by construction, which is why the placement -- not a lock -- is
	// what proves ownership there. Deciding placement BEFORE the branch-lock
	// guard is what keeps a deployment without a lock manager from silently
	// skipping the isolated commit, which would be F5 again by another route.
	if !c.runPlacementIsDirectBranch(ctx, run) {
		// F5: this used to return nil for every isolated task, so an AO
		// worktree whose worker had not run `git commit` reached integration
		// with its entire deliverable as dirty, untracked state -- and
		// integration, finding the branch head unchanged, recorded a successful
		// no-op. The isolated half of this boundary is in isolated_commit.go;
		// it commits inside the task's OWN worktree, under the same "verified
		// but not yet completed" authority window this function uses for a
		// branch lock.
		return c.commitIsolatedWorktree(ctx, run, step)
	}
	if c.branchLocks == nil || c.workspaceCommitter == nil {
		return nil
	}
	project, ok, err := c.projects.GetProject(ctx, run.ProjectID)
	if err != nil || !ok {
		return err
	}
	policy := project.Config.EffectiveGitPolicy()
	locks, err := c.branchLocks.HeldByRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if len(locks) == 0 {
		return nil
	}
	switch policy.LocalCommit {
	case domain.GitActionAutomatic:
		return c.commitHeldRepositories(ctx, run, step, locks)
	case domain.GitActionRequireApproval:
		return c.recordCommitDeferred(ctx, run, step,
			"awaiting_commit_approval: work is complete and uncommitted; the project's local-commit policy requires explicit approval before AO commits it")
	default:
		return c.recordCommitDeferred(ctx, run, step,
			"commit_skipped: work is complete and left uncommitted because the project's local-commit policy is 'never'")
	}
}

// commitHeldRepositories commits each locked repository independently.
//
// Per-repository is not an implementation detail: in a workspace project the
// root and each child are separate Git repositories, and a change inside
// medusa/backend_node must be committed by backend_node, never staged by the
// parent medusa repository. Committing per lock — each of which names exactly
// one repository — is what makes that boundary structural instead of a rule
// someone has to remember.
func (c *Coordinator) commitHeldRepositories(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, locks []domain.BranchLock) error {
	message := autonomousCommitMessage(run)
	for _, lock := range locks {
		sha, committed, err := c.workspaceCommitter.CommitAll(ctx, ports.WorkspaceInfo{
			Path:      lock.RepoPath,
			Branch:    lock.Branch,
			ProjectID: lock.ProjectID,
			RepoPath:  lock.RepoPath,
		}, message)
		if err != nil {
			return fmt.Errorf("autonomous commit in %s: %w", lock.RepoPath, err)
		}
		if !committed {
			continue
		}
		stepID := step.ID
		if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             "wfc-" + c.newID(),
			WorkflowRunID:  run.ID,
			WorkflowStepID: &stepID,
			ProjectID:      run.ProjectID,
			Branch:         lock.Branch,
			WorktreePath:   lock.RepoPath,
			BaseSHA:        lock.BaseSHA,
			HeadSHA:        sha,
			NextAction:     "local_commit_created: " + sha + " on " + lock.Branch,
			DurablePhase:   "autonomous_local_commit",
			PayloadVersion: "v1",
			RetryState:     "{}",
			CreatedAt:      c.clock(),
		}); err != nil {
			return err
		}
	}
	return nil
}

// failRunOnCommitError parks a verified-but-uncommitted run in NeedsAttention.
// The lock is deliberately NOT released: the repository still holds work only
// this run knows about, and freeing the branch would let another run start
// writing on top of it. A human resolves the commit, and the lock is released
// by the run's eventual terminal transition or by boot reconciliation.
func (c *Coordinator) failRunOnCommitError(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, cause error) (domain.WorkflowRun, domain.WorkflowStep, error) {
	now := c.clock()
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return run, step, err
		}
		run.State = domain.WorkflowRunNeedsAttention
	}
	stepID := step.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction: "local_commit_failed: " + cause.Error() +
			" — the work is verified but still uncommitted, and the branch stays locked to this run until a human resolves it",
		DurablePhase:   "autonomous_local_commit_failed",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return run, step, err
	}
	return run, step, nil
}

func (c *Coordinator) recordCommitDeferred(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, nextAction string) error {
	stepID := step.ID
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     nextAction,
		DurablePhase:   "autonomous_local_commit_deferred",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	})
	return err
}

const autonomousCommitSubjectMaxLen = 68

func autonomousCommitMessage(run domain.WorkflowRun) string {
	subject := strings.TrimSpace(strings.SplitN(run.Objective, "\n", 2)[0])
	if subject == "" {
		subject = "autonomous workflow changes"
	}
	if len(subject) > autonomousCommitSubjectMaxLen {
		subject = strings.TrimSpace(subject[:autonomousCommitSubjectMaxLen])
	}
	return fmt.Sprintf("feat: %s\n\nProduced by Agent Orchestrator workflow %s.", subject, run.ID)
}

// branchWaitPhase is the durable phase of a waiting_for_branch checkpoint. It
// is also the key GetRun reads back to surface the structured wait state.
const branchWaitPhase = "waiting_for_branch"

// BranchWait is the structured waiting_for_branch state a run surfaces while
// another run owns its repository+branch pair. Every field is a fact copied
// from the conflicting lock -- there is no derived or estimated value here, so
// the board never has to show a placeholder or a fake "inactive".
type BranchWait struct {
	Branch              string `json:"branch"`
	RepoPath            string `json:"repoPath,omitempty"`
	HeldByWorkflowRunID string `json:"heldByWorkflowRunId,omitempty"`
	HeldBySessionID     string `json:"heldBySessionId,omitempty"`

	// The three fields below are Checkpoint 8P-E.13A's read-time enrichment.
	// They are deliberately NOT part of the persisted checkpoint payload: the
	// holder's state changes after the wait was recorded, and a stored copy
	// would be a snapshot of a fact that has since moved. They are resolved
	// live by enrichBranchWait on every read, or left empty when the holder
	// cannot be resolved.

	// HeldByState is the owning run's current durable state.
	HeldByState string `json:"heldByState,omitempty"`
	// HeldByReason explains why the branch is still held — the difference
	// between "the holder is working" and "the holder stopped and a person has
	// to decide", which is the difference between waiting and being stuck.
	HeldByReason string `json:"heldByReason,omitempty"`
	// AutoResume reports whether this wait is expected to clear without anyone
	// doing anything. False means the queue is behind a decision, and the user
	// is being told so rather than left watching a spinner.
	AutoResume bool `json:"autoResume,omitempty"`
}

func branchWaitFromLockError(err error) BranchWait {
	var conflict domain.BranchLockConflictError
	if asBranchLockConflict(err, &conflict) {
		return BranchWait{
			Branch:              conflict.Holder.Branch,
			RepoPath:            conflict.Holder.RepoPath,
			HeldByWorkflowRunID: conflict.Holder.WorkflowRunID,
			HeldBySessionID:     conflict.Holder.SessionID,
		}
	}
	return BranchWait{}
}

func marshalBranchWait(w BranchWait) string {
	b, err := json.Marshal(w)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// branchWaitFromCheckpoints returns the structured wait state recorded by the
// most recent waiting_for_branch checkpoint, or nil when the run has never
// parked on a branch. The caller only consults it while the run is actually
// waiting, so a stale checkpoint from an earlier, already-resolved wait can
// never be surfaced as a current one.
func branchWaitFromCheckpoints(cps []domain.WorkflowCheckpoint) *BranchWait {
	wait, _, ok := latestBranchWait(cps)
	if !ok {
		return nil
	}
	return &wait
}

// latestBranchWait returns the newest waiting_for_branch checkpoint's decoded
// wait state together with the step that parked, so a caller re-scheduling that
// run's branch wake can address the same scope the original park used.
func latestBranchWait(cps []domain.WorkflowCheckpoint) (BranchWait, *domain.WorkflowStepID, bool) {
	for i := len(cps) - 1; i >= 0; i-- {
		if cps[i].DurablePhase != branchWaitPhase {
			continue
		}
		var wait BranchWait
		if err := json.Unmarshal([]byte(cps[i].RetryState), &wait); err != nil || wait.Branch == "" {
			return BranchWait{}, nil, false
		}
		var stepID *domain.WorkflowStepID
		if cps[i].WorkflowStepID != nil && *cps[i].WorkflowStepID != "" {
			id := domain.WorkflowStepID(*cps[i].WorkflowStepID)
			stepID = &id
		}
		return wait, stepID, true
	}
	return BranchWait{}, nil, false
}
