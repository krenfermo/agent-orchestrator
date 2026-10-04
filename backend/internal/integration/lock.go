package integration

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrLockBusy reports that another integration already owns this target
// branch. It is emphatically not a failure: it is the single-lane property
// working, and the caller's correct response is to leave the task where it is
// and try again after the holder finishes. It wraps domain.ErrBranchLockHeld
// so callers that already match on that keep matching.
var ErrLockBusy = errors.New("integration: target integration lock is held")

// LockRequest names the one repository+branch pair an integration needs.
type LockRequest struct {
	ProjectID     domain.ProjectID
	WorkflowRunID string
	SessionID     string
	TaskID        string
	RepoName      string
	RepoPath      string
	TargetBranch  string
}

// LockHandle identifies an acquisition so it can be given back.
type LockHandle struct {
	ID      string
	LockKey string
}

// Locker is the mutual exclusion the coordinator runs inside. It is a port so
// the coordinator can be tested without a database, but the only production
// implementation is BranchLocker below, over the same branch_locks table (and
// therefore the same lock keys) that direct-branch execution uses. Sharing the
// key is the point: an integration and a direct writer of one branch exclude
// each other, while two isolated task worktrees on different branches do not
// exclude anything at all.
type Locker interface {
	// Acquire returns ErrLockBusy when someone else holds the target.
	Acquire(ctx context.Context, req LockRequest) (LockHandle, error)
	Release(ctx context.Context, handle LockHandle, reason string) error
}

// BranchLocker adapts *branchlock.Manager to Locker.
type BranchLocker struct{ mgr *branchlock.Manager }

// NewBranchLocker wires the real branch-lock manager as the integration lane.
func NewBranchLocker(mgr *branchlock.Manager) *BranchLocker { return &BranchLocker{mgr: mgr} }

// Acquire takes the target's integration lock, or reports ErrLockBusy.
func (l *BranchLocker) Acquire(ctx context.Context, req LockRequest) (LockHandle, error) {
	locks, err := l.mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: req.ProjectID,
		RunID:     req.WorkflowRunID,
		SessionID: req.SessionID,
		// The kind is what makes the acquisition auditable as an integration
		// rather than as a run writing the branch by hand; exclusion itself
		// comes from the key, which is the repository+branch pair.
		Kind:     domain.BranchLockOwnershipTargetIntegration,
		RepoName: req.RepoName,
		RepoPath: req.RepoPath,
		Branch:   req.TargetBranch,
	})
	if err != nil {
		if errors.Is(err, domain.ErrBranchLockHeld) {
			// Both errors are wrapped: ErrLockBusy is what this package's
			// callers match, and domain.ErrBranchLockHeld is what everything
			// that already understands a busy branch matches.
			return LockHandle{}, fmt.Errorf("%w: %w", ErrLockBusy, err)
		}
		return LockHandle{}, err
	}
	if len(locks) != 1 {
		// A target_integration acquisition is always exactly one pair. Anything
		// else means the request was built wrong, and releasing by handle would
		// then leave the others held forever.
		for _, lock := range locks {
			_, _ = l.mgr.ReleaseLock(ctx, lock.ID, "integration: unexpected multi-target acquisition")
		}
		return LockHandle{}, fmt.Errorf("integration: expected one target lock for %s, got %d", req.TargetBranch, len(locks))
	}
	return LockHandle{ID: locks[0].ID, LockKey: locks[0].LockKey}, nil
}

// Release gives back exactly the pair Acquire took, and nothing else its
// owner may hold.
func (l *BranchLocker) Release(ctx context.Context, handle LockHandle, reason string) error {
	if handle.ID == "" {
		return nil
	}
	_, err := l.mgr.ReleaseLock(ctx, handle.ID, reason)
	return err
}
