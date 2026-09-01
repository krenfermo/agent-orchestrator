package wfmemory

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// taskmemory.go — the adapter between workflow's task boundaries and the
// memory subsystem (P2-B §11).
//
// It exists so neither side has to know the other's vocabulary: workflow
// speaks in runs, tasks and placements, project memory speaks in origins,
// generations and provenance. The translation is small and lives in exactly
// one place, which is what stops "is this work integrated" from being decided
// twice with two different answers.
//
// The adapter itself makes no policy. Whether a task's memory may become
// canonical is decided by the workflow boundary that knows the placement (see
// workflow/task_memory.go); this type only carries that decision across.

// TaskMemoryAdapter satisfies workflow.TaskMemory over the memory service.
type TaskMemoryAdapter struct {
	svc *projectmemory.Service
	cfg projectmemory.Config
}

// NewTaskMemory builds the adapter. A nil service, or a disabled mode, yields
// nil — and workflow treats a nil TaskMemory as the ordinary "memory is off"
// state rather than as a failure.
func NewTaskMemory(svc *projectmemory.Service, cfg projectmemory.Config) *TaskMemoryAdapter {
	if svc == nil || !cfg.Mode.Enabled() {
		return nil
	}
	return &TaskMemoryAdapter{svc: svc, cfg: cfg}
}

// Compile-time proof that the adapter satisfies the port workflow declares.
var _ workflowcore.TaskMemory = (*TaskMemoryAdapter)(nil)

// InvalidatePaths retires memory derived from paths a task changed.
func (a *TaskMemoryAdapter) InvalidatePaths(
	ctx stdctx.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string,
) (int64, error) {
	if a == nil {
		return 0, nil
	}
	return a.svc.Invalidate(ctx, projectID, repoPath, paths, reason)
}

// RecordOutcome persists the bounded facts of a finished task.
//
// Origin follows Integrated, and nothing else: work the placement proved is in
// the repository's own history produces canonical memory, and everything else
// produces task-local memory visible only to the task that made it. The memory
// package enforces the rest of that rule at read time.
func (a *TaskMemoryAdapter) RecordOutcome(ctx stdctx.Context, facts workflowcore.TaskOutcomeFacts) error {
	if a == nil {
		return nil
	}
	return a.svc.RecordTaskOutcome(ctx, projectmemory.TaskOutcome{
		ProjectID:      facts.ProjectID,
		RepoPath:       facts.RepoPath,
		TaskRef:        facts.TaskRef,
		Title:          facts.Title,
		WhatChanged:    facts.WhatChanged,
		Why:            facts.Why,
		FilesChanged:   facts.FilesChanged,
		Modules:        facts.Modules,
		Verification:   facts.Verification,
		Commit:         facts.Commit,
		Integrated:     facts.Integrated,
		WorkflowRunID:  facts.WorkflowRunID,
		Share:          facts.Share,
		DependsOnTasks: facts.DependsOnTasks,
	})
}

// PromoteTask turns one task's facts into canonical project knowledge. It is
// called only by the boundary that can prove the work is integrated.
func (a *TaskMemoryAdapter) PromoteTask(
	ctx stdctx.Context, projectID domain.ProjectID, taskRef, commit string,
) (int, error) {
	if a == nil {
		return 0, nil
	}
	return a.svc.PromoteTaskMemory(ctx, projectID, taskRef, commit)
}

// DiscardTask drops one task's unintegrated facts.
func (a *TaskMemoryAdapter) DiscardTask(
	ctx stdctx.Context, projectID domain.ProjectID, taskRef string,
) (int64, error) {
	if a == nil {
		return 0, nil
	}
	return a.svc.DiscardTaskMemory(ctx, projectID, taskRef)
}
