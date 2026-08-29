package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// UpsertTaskWorktree writes the durable record of one task's AO-owned
// worktree, keyed by task: a retry for the same task updates the row it
// already has rather than adding a second one nobody can tell apart.
//
// created_at is only ever taken from the INSERT, never from the update (see
// the query), so the row keeps saying when this task's worktree first
// appeared even after several state transitions.
func (s *Store) UpsertTaskWorktree(ctx context.Context, rec domain.TaskWorktreeRecord) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !rec.State.IsKnown() {
		// The column CHECK would reject this too, but as a constraint
		// violation naming the table rather than the value -- and a caller
		// that invented a lifecycle state deserves to be told which one.
		return fmt.Errorf("task worktree %s: unknown state %q", rec.TaskID, rec.State)
	}
	deps := rec.Dependencies
	if deps == nil {
		deps = []domain.TaskWorktreeDependency{}
	}
	// Sorted on the way in so two runs that resolved the same dependencies
	// serialize to byte-identical JSON.
	sort.Slice(deps, func(i, j int) bool { return deps[i].TaskID < deps[j].TaskID })
	raw, err := json.Marshal(deps)
	if err != nil {
		return fmt.Errorf("marshal task worktree dependencies: %w", err)
	}
	return s.qw.UpsertTaskWorktree(ctx, gen.UpsertTaskWorktreeParams{
		TaskID:           rec.TaskID,
		WorkflowRunID:    rec.WorkflowRunID,
		ProjectID:        string(rec.ProjectID),
		RepoPath:         rec.RepoPath,
		WorktreePath:     rec.Path,
		Branch:           rec.Branch,
		TargetBranch:     rec.TargetBranch,
		BaseSha:          rec.BaseSHA,
		DependenciesJson: string(raw),
		ExecutionMode:    string(rec.ExecutionMode),
		State:            string(rec.State),
		IntegratedSha:    rec.IntegratedSHA,
		BranchDeleted:    boolToInt64(rec.BranchDeleted),
		Detail:           rec.Detail,
		CreatedAt:        rec.CreatedAt,
		UpdatedAt:        rec.UpdatedAt,
		ReleasedAt:       timePtrToNullTime(rec.ReleasedAt),
	})
}

// GetTaskWorktree returns the worktree record for one task, if it has one. A
// direct-branch task never does, and neither does a task whose worktree was
// never requested, so absence is a normal answer rather than an error.
func (s *Store) GetTaskWorktree(ctx context.Context, taskID string) (domain.TaskWorktreeRecord, bool, error) {
	row, err := s.qr.GetTaskWorktree(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskWorktreeRecord{}, false, nil
	}
	if err != nil {
		return domain.TaskWorktreeRecord{}, false, fmt.Errorf("get task worktree %s: %w", taskID, err)
	}
	return taskWorktreeFromGen(row), true, nil
}

// ListTaskWorktrees returns every AO-managed task worktree record (P1-D §X).
//
// Unfiltered on purpose: the placement sweep has to see the records it must
// REFUSE in order to report them, and a query that returned only removable
// rows would hide the state an operator most wants to see.
func (s *Store) ListTaskWorktrees(ctx context.Context) ([]domain.TaskWorktreeRecord, error) {
	rows, err := s.qr.ListTaskWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("list task worktrees: %w", err)
	}
	out := make([]domain.TaskWorktreeRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskWorktreeFromGen(row))
	}
	return out, nil
}

// ListTaskWorktreesByRun returns every worktree record for a run, ordered by
// task id.
func (s *Store) ListTaskWorktreesByRun(ctx context.Context, runID string) ([]domain.TaskWorktreeRecord, error) {
	rows, err := s.qr.ListTaskWorktreesByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list task worktrees for run %s: %w", runID, err)
	}
	out := make([]domain.TaskWorktreeRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskWorktreeFromGen(row))
	}
	return out, nil
}

// ListUnfinishedTaskWorktrees returns every record the worktree lifecycle
// manager is not yet done with, across every run.
//
// Startup reconciliation reads this rather than walking runs. A worktree whose
// run has since gone terminal is exactly the orphan a reconcile pass exists to
// find, and a run-scoped read is the one read that could never see it.
func (s *Store) ListUnfinishedTaskWorktrees(ctx context.Context) ([]domain.TaskWorktreeRecord, error) {
	rows, err := s.qr.ListUnfinishedTaskWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("list unfinished task worktrees: %w", err)
	}
	out := make([]domain.TaskWorktreeRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskWorktreeFromGen(row))
	}
	return out, nil
}

func taskWorktreeFromGen(row gen.WorkflowTaskWorktree) domain.TaskWorktreeRecord {
	deps := []domain.TaskWorktreeDependency{}
	if row.DependenciesJson != "" {
		_ = json.Unmarshal([]byte(row.DependenciesJson), &deps)
	}
	if deps == nil {
		deps = []domain.TaskWorktreeDependency{}
	}
	return domain.TaskWorktreeRecord{
		WorkflowRunID: row.WorkflowRunID,
		TaskID:        row.TaskID,
		ProjectID:     domain.ProjectID(row.ProjectID),
		RepoPath:      row.RepoPath,
		Path:          row.WorktreePath,
		Branch:        row.Branch,
		TargetBranch:  row.TargetBranch,
		BaseSHA:       row.BaseSha,
		Dependencies:  deps,
		ExecutionMode: domain.ExecutionMode(row.ExecutionMode),
		State:         domain.TaskWorktreeState(row.State),
		IntegratedSHA: row.IntegratedSha,
		BranchDeleted: row.BranchDeleted != 0,
		Detail:        row.Detail,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
		ReleasedAt:    nullTimeToTimePtr(row.ReleasedAt),
	}
}
