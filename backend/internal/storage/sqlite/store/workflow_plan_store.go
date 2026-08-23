package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func workflowPlanFromRow(r gen.WorkflowPlan) domain.WorkflowPlanRecord {
	return domain.WorkflowPlanRecord{
		WorkflowRunID: r.WorkflowRunID, Status: domain.WorkflowPlanStatus(r.Status),
		ApprovalMode: domain.WorkflowPlanApprovalMode(r.ApprovalMode), Provider: r.Provider,
		Model: r.Model, PromptContextVersion: r.PromptContextVersion,
		ContextManifestJSON: r.ContextManifestJson, GeneratedPlanJSON: r.GeneratedPlanJson,
		ValidationJSON: r.ValidationJson, PlanHash: r.PlanHash,
		CommandStatus: domain.WorkflowPlanCommandStatus(r.CommandStatus), ErrorClass: r.ErrorClass,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, GeneratedAt: nullTimeToTimePtr(r.GeneratedAt),
		ApprovedAt: nullTimeToTimePtr(r.ApprovedAt), RejectedAt: nullTimeToTimePtr(r.RejectedAt),
	}
}

func (s *Store) CreateWorkflowPlan(ctx context.Context, runID string, mode domain.WorkflowPlanApprovalMode, contextVersion string, now time.Time) (domain.WorkflowPlanRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	r, err := s.qw.InsertWorkflowPlan(ctx, gen.InsertWorkflowPlanParams{WorkflowRunID: runID, ApprovalMode: string(mode), PromptContextVersion: contextVersion, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return domain.WorkflowPlanRecord{}, fmt.Errorf("insert workflow plan: %w", err)
	}
	return workflowPlanFromRow(r), nil
}

func (s *Store) GetWorkflowPlan(ctx context.Context, runID string) (domain.WorkflowPlanRecord, bool, error) {
	r, err := s.qr.GetWorkflowPlan(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowPlanRecord{}, false, nil
	}
	if err != nil {
		return domain.WorkflowPlanRecord{}, false, fmt.Errorf("get workflow plan: %w", err)
	}
	return workflowPlanFromRow(r), true, nil
}

func (s *Store) StartWorkflowPlanCommand(ctx context.Context, runID, provider, model, manifest string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.StartWorkflowPlanCommand(ctx, gen.StartWorkflowPlanCommandParams{Provider: provider, Model: model, ContextManifestJson: manifest, UpdatedAt: now, WorkflowRunID: runID})
	return n > 0, err
}

func (s *Store) PersistWorkflowPlanResponse(ctx context.Context, runID, planJSON string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.PersistWorkflowPlanResponse(ctx, gen.PersistWorkflowPlanResponseParams{GeneratedPlanJson: planJSON, GeneratedAt: sql.NullTime{Time: now, Valid: true}, UpdatedAt: now, WorkflowRunID: runID})
	return n > 0, err
}

func (s *Store) FinishWorkflowPlan(ctx context.Context, runID string, status domain.WorkflowPlanStatus, command domain.WorkflowPlanCommandStatus, validationJSON, hash, errorClass string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.FinishWorkflowPlan(ctx, gen.FinishWorkflowPlanParams{Status: string(status), CommandStatus: string(command), ValidationJson: validationJSON, PlanHash: hash, ErrorClass: errorClass, UpdatedAt: now, WorkflowRunID: runID})
	return n > 0, err
}

func (s *Store) InsertWorkflowTasks(ctx context.Context, tasks []domain.WorkflowTask) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "insert workflow plan tasks", func(q *gen.Queries) error {
		for _, task := range tasks {
			scope := task.ScopeJSON
			if scope == "" {
				scope = "{}"
			}
			if err := q.InsertWorkflowTask(ctx, gen.InsertWorkflowTaskParams{ID: task.ID, WorkflowRunID: task.WorkflowRunID, PlanStepID: task.PlanStepID, Ordinal: task.Ordinal, Title: task.Title, Description: task.Description, AcceptanceCriteriaJson: task.AcceptanceCriteriaJSON, VerifyJson: task.VerifyJSON, ScopeJson: scope, State: string(task.State), CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt}); err != nil {
				return err
			}
		}
		for _, task := range tasks {
			for _, dep := range task.Dependencies {
				if err := q.InsertWorkflowTaskDependency(ctx, gen.InsertWorkflowTaskDependencyParams{WorkflowTaskID: task.ID, DependsOnTaskID: dep}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Store) ListWorkflowTasks(ctx context.Context, runID string) ([]domain.WorkflowTask, error) {
	rows, err := s.qr.ListWorkflowTasks(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.WorkflowTask, 0, len(rows))
	for _, r := range rows {
		var deps []string
		var raw []byte
		switch v := r.DependenciesJson.(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &deps)
		}
		out = append(out, domain.WorkflowTask{ID: r.ID, WorkflowRunID: r.WorkflowRunID, PlanStepID: r.PlanStepID, Ordinal: r.Ordinal, Title: r.Title, Description: r.Description, AcceptanceCriteriaJSON: r.AcceptanceCriteriaJson, VerifyJSON: r.VerifyJson, ScopeJSON: r.ScopeJson, State: domain.WorkflowTaskState(r.State), ExecutionRunID: nullStringToPtr(r.ExecutionRunID), Dependencies: deps, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, CompletedAt: nullTimeToTimePtr(r.CompletedAt)})
	}
	return out, nil
}

func (s *Store) UpdateWorkflowTaskState(ctx context.Context, id string, expected, next domain.WorkflowTaskState, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	completed := sql.NullTime{}
	if next == domain.WorkflowTaskCompleted {
		completed = sql.NullTime{Time: now, Valid: true}
	}
	n, err := s.qw.UpdateWorkflowTaskState(ctx, gen.UpdateWorkflowTaskStateParams{State: string(next), UpdatedAt: now, CompletedAt: completed, ID: id, ExpectedState: string(expected)})
	return n > 0, err
}

// UpdateWorkflowTaskScope replaces a task's estimated read/write scope. It is
// deliberately unconditional (no expected-value CAS): the scope is a derived
// estimate, not lifecycle state, and the newest estimate always wins -- the
// one refreshed with what a completed task actually wrote is strictly better
// than the one guessed from its acceptance criteria.
func (s *Store) UpdateWorkflowTaskScope(ctx context.Context, taskID, scopeJSON string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if scopeJSON == "" {
		scopeJSON = "{}"
	}
	n, err := s.qw.UpdateWorkflowTaskScope(ctx, gen.UpdateWorkflowTaskScopeParams{ScopeJson: scopeJSON, UpdatedAt: now, ID: taskID})
	return n > 0, err
}

// ReplaceWorkflowTaskRelationships writes a plan's whole task-pair
// classification in one transaction. Upsert-by-pair rather than
// delete-then-insert: re-classifying a plan whose tasks have not changed must
// never leave a window in which a concurrent reader sees a task graph with no
// relationships at all.
func (s *Store) ReplaceWorkflowTaskRelationships(ctx context.Context, rels []domain.WorkflowTaskRelationship) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "replace workflow task relationships", func(q *gen.Queries) error {
		for _, rel := range rels {
			overlap := rel.Overlap
			if overlap == nil {
				overlap = []string{}
			}
			raw, err := json.Marshal(overlap)
			if err != nil {
				return fmt.Errorf("marshal relationship overlap: %w", err)
			}
			if err := q.UpsertWorkflowTaskRelationship(ctx, gen.UpsertWorkflowTaskRelationshipParams{
				WorkflowRunID: rel.WorkflowRunID, TaskID: rel.TaskID, RelatedTaskID: rel.RelatedTaskID,
				Relation: string(rel.Relation), Reason: rel.Reason, Detail: rel.Detail,
				OverlapJson: string(raw), CreatedAt: rel.CreatedAt,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListWorkflowTaskRelationships returns every stored pair classification for a
// run, ordered by the canonical (task_id, related_task_id) pair.
func (s *Store) ListWorkflowTaskRelationships(ctx context.Context, runID string) ([]domain.WorkflowTaskRelationship, error) {
	rows, err := s.qr.ListWorkflowTaskRelationships(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.WorkflowTaskRelationship, 0, len(rows))
	for _, r := range rows {
		var overlap []string
		if r.OverlapJson != "" {
			_ = json.Unmarshal([]byte(r.OverlapJson), &overlap)
		}
		if overlap == nil {
			overlap = []string{}
		}
		out = append(out, domain.WorkflowTaskRelationship{
			WorkflowRunID: r.WorkflowRunID, TaskID: r.TaskID, RelatedTaskID: r.RelatedTaskID,
			Relation: domain.WorkflowTaskRelation(r.Relation), Reason: r.Reason, Detail: r.Detail,
			Overlap: overlap, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

func (s *Store) SetWorkflowTaskExecutionRun(ctx context.Context, taskID, executionRunID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SetWorkflowTaskExecutionRun(ctx, gen.SetWorkflowTaskExecutionRunParams{ExecutionRunID: sql.NullString{String: executionRunID, Valid: true}, UpdatedAt: now, ID: taskID})
	return n > 0, err
}

func (s *Store) FindWorkflowRunByPlannedTask(ctx context.Context, taskID string) (string, bool, error) {
	id, err := s.qr.FindWorkflowRunByPlannedTask(ctx, sql.NullString{String: taskID, Valid: true})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func (s *Store) ApproveWorkflowPlan(ctx context.Context, runID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ApproveWorkflowPlan(ctx, gen.ApproveWorkflowPlanParams{ApprovedAt: sql.NullTime{Time: now, Valid: true}, UpdatedAt: now, WorkflowRunID: runID})
	return n > 0, err
}

// SetWorkflowPlanApprovalMode flips a plan's approval mode after creation --
// used by Checkpoint 8P-D so an autonomous-policy-triggered auto-approval
// leaves an honest, inspectable record (approval_mode="auto") even when the
// plan was originally created with the client's requested "manual". Guarded
// to never touch an already-approved plan, matching ApproveWorkflowPlan's own
// CAS discipline.
func (s *Store) SetWorkflowPlanApprovalMode(ctx context.Context, runID string, mode domain.WorkflowPlanApprovalMode, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SetWorkflowPlanApprovalMode(ctx, gen.SetWorkflowPlanApprovalModeParams{ApprovalMode: string(mode), UpdatedAt: now, WorkflowRunID: runID})
	return n > 0, err
}

func (s *Store) RejectWorkflowPlan(ctx context.Context, runID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.RejectWorkflowPlan(ctx, gen.RejectWorkflowPlanParams{RejectedAt: sql.NullTime{Time: now, Valid: true}, UpdatedAt: now, WorkflowRunID: runID})
	return n > 0, err
}
