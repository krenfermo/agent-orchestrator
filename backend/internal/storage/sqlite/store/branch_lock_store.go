package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// AcquireBranchLock takes the durable execution lock over one
// repository+branch pair (Checkpoint 8P-E.11).
//
// Mutual exclusion is the database's job, not this function's: the INSERT is
// attempted unconditionally and the partial UNIQUE index on
// (lock_key) WHERE state='held' (migration 0117) rejects a second holder. Only
// after a rejection do we read back who actually holds it, and return that
// holder inside a domain.BranchLockConflictError.
//
// Doing it in that order — write first, read only to explain a failure — is
// what makes this safe under concurrency. A read-then-write ("is it free? then
// take it") would leave a window in which two callers both read "free"; here
// the losing caller cannot have inserted anything.
//
// Re-acquiring a lock the same run already holds is idempotent: it returns the
// existing lock rather than conflicting with itself, so a redispatch, a
// reconcile pass, and the original acquisition can all call this freely.
func (s *Store) AcquireBranchLock(ctx context.Context, lock domain.BranchLock) (domain.BranchLock, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.InsertBranchLock(ctx, gen.InsertBranchLockParams{
		ID:             lock.ID,
		LockKey:        lock.LockKey,
		ProjectID:      string(lock.ProjectID),
		RepoPath:       lock.RepoPath,
		RepoName:       lock.RepoName,
		Branch:         lock.Branch,
		WorkflowRunID:  lock.WorkflowRunID,
		WorkflowStepID: stringToNullString(lock.WorkflowStepID),
		SessionID:      stringToNullString(lock.SessionID),
		OwnerToken:     lock.OwnerToken,
		State:          string(domain.BranchLockHeld),
		BaseSha:        lock.BaseSHA,
		AcquiredAt:     lock.AcquiredAt,
		RenewedAt:      lock.AcquiredAt,
		ReleasedAt:     sql.NullTime{},
		ReleaseReason:  "",
		CreatedAt:      lock.AcquiredAt,
		UpdatedAt:      lock.AcquiredAt,
	})
	if err == nil {
		return branchLockFromRow(row), nil
	}
	held, getErr := s.qw.GetHeldBranchLock(ctx, lock.LockKey)
	if getErr != nil {
		if errors.Is(getErr, sql.ErrNoRows) {
			// The insert failed but nobody holds the lock: this is a genuine
			// storage failure, not contention, and must surface as one rather
			// than as a wait that would never end.
			return domain.BranchLock{}, fmt.Errorf("acquire branch lock %s: %w", lock.LockKey, err)
		}
		return domain.BranchLock{}, fmt.Errorf("acquire branch lock %s: read holder: %w", lock.LockKey, getErr)
	}
	holder := branchLockFromRow(held)
	if holder.WorkflowRunID == lock.WorkflowRunID {
		return holder, nil
	}
	return domain.BranchLock{}, domain.BranchLockConflictError{Holder: holder}
}

// GetHeldBranchLock returns the current holder of a repository+branch pair.
// found=false means the pair is free.
func (s *Store) GetHeldBranchLock(ctx context.Context, lockKey string) (domain.BranchLock, bool, error) {
	row, err := s.qr.GetHeldBranchLock(ctx, lockKey)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.BranchLock{}, false, nil
	}
	if err != nil {
		return domain.BranchLock{}, false, fmt.Errorf("get held branch lock %s: %w", lockKey, err)
	}
	return branchLockFromRow(row), true, nil
}

// ListHeldBranchLocks returns every currently held lock. Boot reconciliation
// reads the full set at once and decides each row's fate against live workflow
// state.
func (s *Store) ListHeldBranchLocks(ctx context.Context) ([]domain.BranchLock, error) {
	rows, err := s.qr.ListHeldBranchLocks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list held branch locks: %w", err)
	}
	return sortedBranchLocks(rows), nil
}

// ListHeldBranchLocksByProject returns the locks currently occupying a
// project's repositories, for Project Settings and board occupancy reads.
func (s *Store) ListHeldBranchLocksByProject(ctx context.Context, projectID domain.ProjectID) ([]domain.BranchLock, error) {
	rows, err := s.qr.ListHeldBranchLocksByProject(ctx, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("list held branch locks for project %s: %w", projectID, err)
	}
	return sortedBranchLocks(rows), nil
}

// ListHeldBranchLocksByRun returns the locks one workflow run currently holds.
func (s *Store) ListHeldBranchLocksByRun(ctx context.Context, runID string) ([]domain.BranchLock, error) {
	rows, err := s.qr.ListHeldBranchLocksByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list held branch locks for run %s: %w", runID, err)
	}
	return sortedBranchLocks(rows), nil
}

// ReleaseBranchLock releases one held lock. released=false means the row was
// already released by some other path (a run-terminal cascade, a reconcile) —
// a normal race outcome, never an error.
func (s *Store) ReleaseBranchLock(ctx context.Context, id, reason string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ReleaseBranchLock(ctx, gen.ReleaseBranchLockParams{
		ReleasedAt:    sql.NullTime{Time: at, Valid: true},
		ReleaseReason: reason,
		UpdatedAt:     at,
		ID:            id,
	})
	if err != nil {
		return false, fmt.Errorf("release branch lock %s: %w", id, err)
	}
	return n > 0, nil
}

// ReleaseBranchLocksByRun releases every lock a run still holds and returns how
// many were released. It is the cascade every terminal transition (completed,
// failed, cancelled) and every crash-recovery path calls: a run that is over
// must never keep a branch occupied.
func (s *Store) ReleaseBranchLocksByRun(ctx context.Context, runID, reason string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ReleaseBranchLocksByRun(ctx, gen.ReleaseBranchLocksByRunParams{
		ReleasedAt:    sql.NullTime{Time: at, Valid: true},
		ReleaseReason: reason,
		UpdatedAt:     at,
		WorkflowRunID: runID,
	})
	if err != nil {
		return 0, fmt.Errorf("release branch locks for run %s: %w", runID, err)
	}
	return n, nil
}

// RenewBranchLock refreshes a held lock's liveness heartbeat and its current
// step/session scope. It never transfers ownership: the run id is part of the
// WHERE clause, so a renewal by anyone else affects 0 rows.
func (s *Store) RenewBranchLock(ctx context.Context, id, runID, stepID, sessionID string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.RenewBranchLock(ctx, gen.RenewBranchLockParams{
		RenewedAt:      at,
		WorkflowStepID: stringToNullString(stepID),
		SessionID:      stringToNullString(sessionID),
		UpdatedAt:      at,
		ID:             id,
		WorkflowRunID:  runID,
	})
	if err != nil {
		return false, fmt.Errorf("renew branch lock %s: %w", id, err)
	}
	return n > 0, nil
}

// AdoptBranchLock transfers a still-held lock to the current daemon instance
// after a restart. adopted=false means the row stopped being held between the
// reconciliation read and this write — another path already resolved it.
func (s *Store) AdoptBranchLock(ctx context.Context, id, ownerToken string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.AdoptBranchLock(ctx, gen.AdoptBranchLockParams{
		OwnerToken: ownerToken,
		RenewedAt:  at,
		UpdatedAt:  at,
		ID:         id,
	})
	if err != nil {
		return false, fmt.Errorf("adopt branch lock %s: %w", id, err)
	}
	return n > 0, nil
}

func sortedBranchLocks(rows []gen.BranchLock) []domain.BranchLock {
	out := make([]domain.BranchLock, 0, len(rows))
	for _, r := range rows {
		out = append(out, branchLockFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AcquiredAt.Before(out[j].AcquiredAt) })
	return out
}

func branchLockFromRow(r gen.BranchLock) domain.BranchLock {
	return domain.BranchLock{
		ID:             r.ID,
		LockKey:        r.LockKey,
		ProjectID:      domain.ProjectID(r.ProjectID),
		RepoPath:       r.RepoPath,
		RepoName:       r.RepoName,
		Branch:         r.Branch,
		WorkflowRunID:  r.WorkflowRunID,
		WorkflowStepID: r.WorkflowStepID.String,
		SessionID:      r.SessionID.String,
		OwnerToken:     r.OwnerToken,
		State:          domain.BranchLockState(r.State),
		BaseSHA:        r.BaseSha,
		AcquiredAt:     r.AcquiredAt,
		RenewedAt:      r.RenewedAt,
		ReleasedAt:     nullTimeToTimePtr(r.ReleasedAt),
		ReleaseReason:  r.ReleaseReason,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
}
