package domain

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// BranchLockState is the durable state of one execution lock.
type BranchLockState string

const (
	// BranchLockHeld means a workflow run is the current writer of the
	// repository+branch pair.
	BranchLockHeld BranchLockState = "held"
	// BranchLockReleased means the row is history: the pair is free.
	BranchLockReleased BranchLockState = "released"
)

// ErrBranchLockHeld reports that a repository+branch pair is already owned by
// another workflow run (Checkpoint 8P-E.11). It is not a failure: the waiting
// run parks in a truthful waiting_for_branch state and resumes when the lock is
// released. Callers match it with errors.Is and read the owner off
// BranchLockConflictError.
var ErrBranchLockHeld = errors.New("branch lock: already held")

// BranchLockConflictError carries the current holder so a waiting run can say
// exactly which workflow occupies the branch instead of reporting an opaque
// "busy".
type BranchLockConflictError struct {
	Holder BranchLock
}

func (e BranchLockConflictError) Error() string {
	return "branch lock: " + e.Holder.Branch + " in " + e.Holder.RepoPath +
		" is held by workflow " + e.Holder.WorkflowRunID
}

func (e BranchLockConflictError) Unwrap() error { return ErrBranchLockHeld }

// BranchLock is one durable execution lock over a repository+branch pair.
type BranchLock struct {
	ID        string
	LockKey   string
	ProjectID ProjectID
	// RepoPath is the canonical absolute path of the locked repository. For a
	// workspace project this is one specific registered repository, never the
	// project root as a stand-in for all of them.
	RepoPath string
	// RepoName is RootWorkspaceRepoName for a single-repo project or a
	// workspace root, otherwise the registered child repo name.
	RepoName       string
	Branch         string
	WorkflowRunID  string
	WorkflowStepID string
	SessionID      string
	OwnerToken     string
	State          BranchLockState
	BaseSHA        string
	AcquiredAt     time.Time
	RenewedAt      time.Time
	ReleasedAt     *time.Time
	ReleaseReason  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Held reports whether the lock is currently owned.
func (l BranchLock) Held() bool { return l.State == BranchLockHeld }

// branchLockKeySeparator is a unit separator: it cannot appear in a filesystem
// path or a git branch name, so the composed key is unambiguous.
const branchLockKeySeparator = "\x1f"

// BranchLockKey composes the durable identity of one execution lock from a
// repository path and a branch. The path is canonicalized (cleaned and made
// absolute) so two spellings of the same repository — a relative path, a
// trailing slash — resolve to the same lock and cannot both be held at once.
// Symlink resolution deliberately happens at the adapter boundary rather than
// here, so this stays a pure function usable from tests and from read paths
// that must not touch the filesystem.
func BranchLockKey(repoPath, branch string) string {
	path := strings.TrimSpace(repoPath)
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		path = filepath.Clean(path)
	}
	return path + branchLockKeySeparator + strings.TrimSpace(branch)
}
