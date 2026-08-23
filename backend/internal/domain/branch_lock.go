package domain

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// BranchLockState is the durable state of one execution lock.
type BranchLockState string

// BranchLockOwnershipKind explains which mutable git surface a lock protects.
// The kind is audit metadata; exclusion remains keyed by the concrete
// repository+branch so direct writers and integrators cannot overlap, while
// isolated task branches remain independent.
type BranchLockOwnershipKind string

const (
	BranchLockOwnershipDirectBranch      BranchLockOwnershipKind = "direct_branch"
	BranchLockOwnershipTaskWorkspace     BranchLockOwnershipKind = "isolated_task_workspace"
	BranchLockOwnershipTargetIntegration BranchLockOwnershipKind = "target_integration"
)

func (k BranchLockOwnershipKind) WithDefault() BranchLockOwnershipKind {
	if k == "" {
		return BranchLockOwnershipDirectBranch
	}
	return k
}

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
		" is held by " + e.Holder.OwnerDescription()
}

func (e BranchLockConflictError) Unwrap() error { return ErrBranchLockHeld }

// BranchLock is one durable execution lock over a repository+branch pair.
//
// Ownership is by workflow run OR by a single session, never by both
// (Checkpoint 8P-E.14). An autonomous run owns the branch for the whole run,
// across every session it spawns, so WorkflowRunID is the owner and SessionID
// is only the current scope. An ordinary task has no run to belong to, so the
// session itself is the owner and WorkflowRunID is empty. Both kinds compete
// for the same lock_key, which is what makes a task and a workflow contend for
// one repository+branch instead of quietly writing over each other.
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
	OwnershipKind  BranchLockOwnershipKind
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

// SessionOwned reports whether an ordinary task session owns this lock rather
// than an autonomous workflow run (Checkpoint 8P-E.14). It is decided by the
// absence of a run, not by the presence of a session: a workflow-owned lock
// also carries a SessionID (the step's current worker), so testing SessionID
// alone would misclassify every workflow lock.
func (l BranchLock) SessionOwned() bool {
	return strings.TrimSpace(l.WorkflowRunID) == "" && strings.TrimSpace(l.SessionID) != ""
}

// OwnerKey is the identity a release, a renewal, or an idempotent re-acquire
// must match. Two acquisitions belong to the same owner exactly when their
// OwnerKeys are equal, which is why this is the one place the run-vs-session
// distinction is resolved. An empty result means the lock names no owner at
// all, and must never compare equal to anything.
func (l BranchLock) OwnerKey() string {
	if run := strings.TrimSpace(l.WorkflowRunID); run != "" {
		return "run:" + run
	}
	if session := strings.TrimSpace(l.SessionID); session != "" {
		return "session:" + session
	}
	return ""
}

// OwnerDescription names the holder for a human. It is what a blocked task
// shows the operator, so it says which workflow or which session to go look at
// rather than reporting an anonymous "busy".
func (l BranchLock) OwnerDescription() string {
	if run := strings.TrimSpace(l.WorkflowRunID); run != "" {
		return "workflow " + run
	}
	if session := strings.TrimSpace(l.SessionID); session != "" {
		return "task session " + session
	}
	return "an unidentified owner"
}

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
