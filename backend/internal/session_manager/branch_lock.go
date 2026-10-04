package sessionmanager

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Checkpoint 8P-E.14: ordinary tasks contend for direct-branch execution locks.
//
// Before this checkpoint the durable repository+branch lock existed only on the
// autonomous workflow path (workflow/branch_execution.go). Ordinary tasks
// resolved the same repository and the same branch through the same workspace
// router — that part was always project-authoritative — but took no lock at
// all. Two tasks, or a task and a workflow, could therefore both be checked out
// on one branch in one working tree with neither aware of the other.
//
// The fix is deliberately not a second lock implementation. It is the same
// branchlock.Manager, the same lock_key (repo path + branch), and the same
// dirty-repository refusal; the only new thing is that a lock may now be owned
// by a session instead of by a run (see domain.BranchLock.OwnerKey).
//
// What this checkpoint does NOT do, by explicit decision: a task that cannot
// get the lock fails fast with the owner named, rather than parking in a
// durable waiting state and resuming on release. Tasks have no run, no outbox,
// and no wake scheduler, so a truthful wait would mean building a task queue.
// That is recorded as follow-up work, not faked here — and crucially, failing
// is the safe direction: the one thing a blocked task must never do is invent a
// derived branch to work around the contention.

// BranchLocks is the narrow direct-branch execution-lock surface the session
// manager depends on. Satisfied by *branchlock.Manager through the daemon's
// wiring adapter; declared here, in the consumer, so this package does not
// import the branchlock implementation for two method signatures.
//
// Optional throughout: a nil BranchLocks means no session ever takes a lock,
// which is exactly correct for an installation where every project is still in
// isolated-worktree mode, and is what every pre-8P-E.14 test exercises.
type BranchLocks interface {
	// AcquireForSession takes every direct-branch lock the project requires,
	// owned by this session. It returns no locks and no error for a project
	// that is not in direct-branch mode.
	AcquireForSession(ctx context.Context, projectID domain.ProjectID, sessionID domain.SessionID) ([]domain.BranchLock, error)
	// ReleaseSession frees every lock the session owns.
	ReleaseSession(ctx context.Context, sessionID, reason string) (int64, error)
}

// SetBranchLocks wires the direct-branch execution lock manager.
//
// Late binding matches SetTerminalInputGate and the branchlock manager's own
// SetClassifier: the daemon builds the session manager before it builds the
// lock manager, and splitting either one in half to fix the order would cost
// more than handing the dependency over afterwards.
func (m *Manager) SetBranchLocks(locks BranchLocks) {
	m.branchLocksMu.Lock()
	defer m.branchLocksMu.Unlock()
	m.branchLocks = locks
}

func (m *Manager) currentBranchLocks() BranchLocks {
	m.branchLocksMu.Lock()
	defer m.branchLocksMu.Unlock()
	return m.branchLocks
}

// acquireSessionBranchLocks is the gate an ordinary task passes through before
// its workspace is prepared.
//
// Ordering matters and is the reason this sits between the seed row and
// createSessionWorkspace: the direct-branch adapter's Create is what actually
// checks the branch out in the user's repository, so the lock has to be held
// before that runs, and the session id the lock is recorded under only exists
// once the seed row does.
//
// A workflow-owned spawn skips acquisition entirely — its run already holds the
// same locks, and asking again would make the run contend with itself.
//
// Nothing here needs to undo a successful acquisition on a later spawn failure:
// every failure after this point funnels through rollbackSpawnSeedRow or
// MarkTerminated, and both release the session's locks.
//
// This is the acquisition that decides whether the task may run at all, and it
// is the only one that refuses. Ownership after this point is turn-scoped
// (Checkpoint 8P-E.14A): lifecycle gives the branch back when the agent reports
// its turn finished and takes it again when the next turn starts, because an
// ordinary task that completes successfully is never terminated — it goes idle
// and stays alive — so tying the lock to termination held the branch forever.
func (m *Manager) acquireSessionBranchLocks(ctx context.Context, cfg ports.SpawnConfig, id domain.SessionID) error {
	locks := m.currentBranchLocks()
	if locks == nil || cfg.WorkflowRunID != "" {
		return nil
	}
	if _, err := locks.AcquireForSession(ctx, cfg.ProjectID, id); err != nil {
		return branchLockSpawnError(err)
	}
	return nil
}

// releaseSessionBranchLocks frees a session's own locks. Best-effort by design,
// exactly as the workflow's releaseBranchLocks is: turning a completed task
// into a failed one over lock bookkeeping would be worse than the miss, and
// boot reconciliation already releases any lock whose owning session is
// terminated or has no turn in flight (branchlock/retention.go), so a missed
// release is self-healing rather than permanent.
func (m *Manager) releaseSessionBranchLocks(ctx context.Context, id domain.SessionID, reason string) {
	locks := m.currentBranchLocks()
	if locks == nil {
		return
	}
	n, err := locks.ReleaseSession(ctx, string(id), reason)
	if err != nil {
		m.logger.Warn("session branch lock release failed", "sessionID", id, "error", err)
		return
	}
	if n > 0 {
		m.logger.Info("session branch locks released", "sessionID", id, "count", n, "reason", reason)
	}
}

// branchLockSpawnError turns a lock failure into the message the operator acts
// on. Both outcomes are refusals rather than fallbacks, because the only
// available fallback — quietly working on a different branch — is the defect
// this checkpoint exists to remove.
func branchLockSpawnError(err error) error {
	var conflict domain.BranchLockConflictError
	if errors.As(err, &conflict) {
		return fmt.Errorf("%w: %s in %s is already being worked on by %s; wait for it to finish or cancel it, then start this task again",
			ErrBranchBusy, conflict.Holder.Branch, conflict.Holder.RepoPath, conflict.Holder.OwnerDescription())
	}
	if errors.Is(err, ports.ErrWorkspaceRepositoryDirty) {
		return fmt.Errorf("%w: %s — AO will not start work over uncommitted changes it did not make; commit, stash, or discard them, then start this task again",
			ErrBranchDirty, err.Error())
	}
	return fmt.Errorf("branch lock: %w", err)
}

// ErrBranchBusy reports that another task or workflow owns the project's
// repository+branch. Exported so the HTTP layer can map it to 409 Conflict
// rather than a generic 500: it is a legitimate, self-clearing state, not a
// fault.
var ErrBranchBusy = errors.New("branch is in use")

// ErrBranchDirty reports that the target repository holds uncommitted work AO
// did not create. Distinct from ErrBranchBusy because waiting does not fix it —
// a human has to decide what happens to those changes.
var ErrBranchDirty = errors.New("repository has uncommitted changes")
