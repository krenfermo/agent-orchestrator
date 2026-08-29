package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// PersistNormalizedWorkflowPlan re-persists the normalized form of an
// already-responded plan, conditioned on the exact bytes the caller read
// (expected). Returns false when the row moved on -- a different plan status,
// a different command status, or a concurrent writer that already replaced
// the JSON -- so the caller can refuse rather than assume its write landed.
// See P9 in docs/worker-lifecycle-audit.md.
func (s *Store) PersistNormalizedWorkflowPlan(ctx context.Context, runID, expected, normalized string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.PersistNormalizedWorkflowPlan(ctx, gen.PersistNormalizedWorkflowPlanParams{GeneratedPlanJson: normalized, UpdatedAt: now, WorkflowRunID: runID, ExpectedPlanJson: expected})
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
		var attention domain.WorkflowTaskAttention
		if r.AttentionJson != "" {
			// A body that will not unmarshal must not lose the task. The reason
			// column is the load-bearing part (it is what the CHECK guarantees
			// a parked task has); the detail is a convenience.
			_ = json.Unmarshal([]byte(r.AttentionJson), &attention)
		}
		out = append(out, domain.WorkflowTask{ID: r.ID, WorkflowRunID: r.WorkflowRunID, PlanStepID: r.PlanStepID, Ordinal: r.Ordinal, Title: r.Title, Description: r.Description, AcceptanceCriteriaJSON: r.AcceptanceCriteriaJson, VerifyJSON: r.VerifyJson, ScopeJSON: r.ScopeJson, State: domain.WorkflowTaskState(r.State), ExecutionRunID: nullStringToPtr(r.ExecutionRunID), Dependencies: deps, AttentionReason: r.AttentionReason, Attention: attention, AttentionAt: nullTimeToTimePtr(r.AttentionAt), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, CompletedAt: nullTimeToTimePtr(r.CompletedAt)})
	}
	return out, nil
}

// ParkWorkflowTaskForAttention moves a task into the durable needs_attention
// state with the detail a person needs to act on it.
//
// Conditional on the expected state for the same reason UpdateWorkflowTaskState
// is: two reconcile passes racing on one task must not both park it, and a task
// that has since been cancelled must not be revived by a late conflict report.
// A false return means the task was not in the expected state, which the caller
// must treat as "somebody else already decided", not as an error.
//
// expectedAttempt is the second half of the predicate, and it closes the one
// genuine ABA in the master-task path. `needs_attention` is deliberately
// non-terminal and its only exit is a person, so a task travels
// running -> needs_attention -> running without bound, and `state = 'running'`
// is satisfied by EVERY generation of running. A pass that observed the
// conflict of integration attempt N and then paused -- a slow git probe, a
// rebase, a re-scheduled poll -- could land its park after a human had already
// resumed the task into attempt N+1: parking the new attempt for the old
// attempt's conflict, and overwriting the new attempt's attention_json with
// stale SHAs.
//
// The discriminator was already durable and merely unread.
// WorkflowTaskAttention.Attempt is incremented by the parking caller itself and
// SURVIVES a resume (ResumeWorkflowTaskFromAttention clears attention_reason and
// attention_at and leaves attention_json alone), so it counts generations of
// running exactly. Reading it from the predicate is what turns the doc comment's
// claim -- "the resume produced exactly one new attempt" -- into something the
// storage layer checks.
//
// A task whose attention_json has no attempt (an older row, or one never parked)
// reads as 0, and a caller that observed no attempt passes 0: the historical
// rows keep exactly the fence they always had.
func (s *Store) ParkWorkflowTaskForAttention(ctx context.Context, id string, expected domain.WorkflowTaskState, expectedAttempt int, reason string, attention domain.WorkflowTaskAttention, now time.Time) (bool, error) {
	if reason == "" {
		// The schema refuses this too; failing here names the caller instead of
		// surfacing a CHECK violation from three layers down.
		return false, errors.New("park workflow task: a parked task must carry a reason")
	}
	body, err := json.Marshal(attention)
	if err != nil {
		return false, fmt.Errorf("marshal task attention: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE workflow_tasks
		    SET state = 'needs_attention',
		        attention_reason = ?,
		        attention_json = ?,
		        attention_at = ?,
		        updated_at = ?
		  WHERE id = ?
		    AND state = ?
		    AND COALESCE(json_extract(attention_json, '$.attempt'), 0) = ?`,
		reason, string(body), sql.NullTime{Time: now, Valid: true}, now, id, string(expected), expectedAttempt)
	if err != nil {
		return false, fmt.Errorf("park workflow task %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("park workflow task %s: %w", id, err)
	}
	return n > 0, nil
}

// ResumeWorkflowTaskFromAttention is the one exit from the parked state, and
// the reason resuming is idempotent: the statement only matches a task that is
// actually parked, so a second resume of the same task affects zero rows and
// cannot produce a second integration attempt.
//
// The attention body is deliberately kept and only the reason and timestamp
// are released. What the task was parked on is what tells the next attempt it
// is a retry rather than a first try.
//
// expectedAttempt is the symmetric half of the park's own discriminator: resume
// the stop the person actually read, not whichever stop is current. Without it,
// `state = 'needs_attention'` matches any stop of this task, so a resume issued
// against attempt N's conflict could release a task that had since been resumed,
// re-integrated, and parked again on a different conflict as attempt N+1 --
// clearing a stop nobody looked at.
func (s *Store) ResumeWorkflowTaskFromAttention(ctx context.Context, id string, next domain.WorkflowTaskState, expectedAttempt int, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE workflow_tasks
		    SET state = ?,
		        attention_reason = '',
		        attention_at = NULL,
		        updated_at = ?
		  WHERE id = ?
		    AND state = 'needs_attention'
		    AND COALESCE(json_extract(attention_json, '$.attempt'), 0) = ?`,
		string(next), now, id, expectedAttempt)
	if err != nil {
		return false, fmt.Errorf("resume workflow task %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("resume workflow task %s: %w", id, err)
	}
	return n > 0, nil
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

// AmendWorkflowTaskCriterion records one human-approved amendment to a task's
// acceptance criteria and applies the resulting criteria to the task, in ONE
// transaction.
//
// Atomic on purpose. The ledger row is the justification for the change and the
// task row is the change; a crash that separated them would leave either a
// criterion nobody can account for, or an explanation for something that never
// happened. Both are worse than the operation simply not having occurred.
func (s *Store) AmendWorkflowTaskCriterion(ctx context.Context, a domain.WorkflowTaskCriterionAmendment, criteria []string, now time.Time) error {
	if len(a.Evidence) == 0 || a.ApprovedBy == "" || a.Reason == "" {
		// The schema refuses these too; failing here names the caller rather
		// than surfacing a CHECK violation from three layers down.
		return errors.New("amend workflow task criterion: reason, evidence and an approving human are all required")
	}
	evidence, err := json.Marshal(a.Evidence)
	if err != nil {
		return fmt.Errorf("marshal amendment evidence: %w", err)
	}
	applied, err := json.Marshal(criteria)
	if err != nil {
		return fmt.Errorf("marshal amended criteria: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "amend workflow task criterion", func(q *gen.Queries) error {
		if err := q.InsertWorkflowTaskCriterionAmendment(ctx, gen.InsertWorkflowTaskCriterionAmendmentParams{
			ID: a.ID, WorkflowRunID: a.WorkflowRunID, TaskID: a.TaskID,
			CriterionIndex: a.CriterionIndex, OriginalCriterion: a.OriginalCriterion,
			AmendedCriterion: a.AmendedCriterion, Disposition: string(a.Disposition),
			Reason: a.Reason, EvidenceJson: string(evidence), ApprovedBy: a.ApprovedBy,
			SupersededReviewRunID: a.SupersededReviewRunID, CreatedAt: a.CreatedAt,
		}); err != nil {
			return err
		}
		_, err := q.UpdateWorkflowTaskAcceptanceCriteria(ctx, gen.UpdateWorkflowTaskAcceptanceCriteriaParams{
			AcceptanceCriteriaJson: string(applied), UpdatedAt: now, ID: a.TaskID,
		})
		return err
	})
}

// ListWorkflowTaskCriterionAmendments returns every amendment recorded for a
// master run, oldest first.
func (s *Store) ListWorkflowTaskCriterionAmendments(ctx context.Context, runID string) ([]domain.WorkflowTaskCriterionAmendment, error) {
	rows, err := s.qr.ListWorkflowTaskCriterionAmendments(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.WorkflowTaskCriterionAmendment, 0, len(rows))
	for _, r := range rows {
		var evidence []string
		_ = json.Unmarshal([]byte(r.EvidenceJson), &evidence)
		out = append(out, domain.WorkflowTaskCriterionAmendment{
			ID: r.ID, WorkflowRunID: r.WorkflowRunID, TaskID: r.TaskID,
			CriterionIndex: r.CriterionIndex, OriginalCriterion: r.OriginalCriterion,
			AmendedCriterion: r.AmendedCriterion,
			Disposition:      domain.WorkflowTaskCriterionDisposition(r.Disposition),
			Reason:           r.Reason, Evidence: evidence, ApprovedBy: r.ApprovedBy,
			SupersededReviewRunID: r.SupersededReviewRunID, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
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

// CreateObjectiveRunWithPlan inserts a master objective's run row, its plan
// step and its workflow_plans row in ONE transaction.
//
// CP1: those writes used to be two independent transactions in
// Coordinator.CreateObjectiveRun, and a crash between them left a run with a
// `plan` step and no plan row. Nothing could then recognise it as a master
// objective — GetWorkflowPlan reports master == false, so getMasterRun never
// runs, ContinueRun falls through its master branch into the work/review lookup
// and errors, and boot recovery falls through to a step loop that finds no work
// step. The run was not resumable, not completable and not explicable.
//
// The window is closed rather than healed because the approval mode is a
// parameter of the create request and lives nowhere on the run: a healer can
// only default it (to `manual`, never `auto`, since inferring `auto` would
// start an unattended planner nobody asked for) and say that it did. Not
// splitting the writes means no run ever needs that guess.
// healOrphanedObjectiveRun still exists for the rows a pre-fix daemon left on
// disk.
func (s *Store) CreateObjectiveRunWithPlan(
	ctx context.Context,
	run domain.WorkflowRun,
	steps []domain.WorkflowStep,
	mode domain.WorkflowPlanApprovalMode,
	contextVersion string,
	now time.Time,
) (domain.WorkflowRun, []domain.WorkflowStep, domain.WorkflowPlanRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var (
		insertedRun   domain.WorkflowRun
		insertedSteps = make([]domain.WorkflowStep, 0, len(steps))
		insertedPlan  domain.WorkflowPlanRecord
	)
	err := s.inTx(ctx, "create objective run with plan", func(q *gen.Queries) error {
		row, err := q.InsertWorkflowRun(ctx, gen.InsertWorkflowRunParams{
			ID:               run.ID,
			ProjectID:        domain.ProjectID(run.ProjectID),
			Objective:        run.Objective,
			State:            run.State,
			PolicyVersion:    run.PolicyVersion,
			PolicySnapshot:   run.PolicySnapshot,
			CreatedAt:        run.CreatedAt,
			UpdatedAt:        run.UpdatedAt,
			ParentWorkflowID: stringPtrToNullString(run.ParentWorkflowID),
			PlannedTaskID:    stringPtrToNullString(run.PlannedTaskID),
		})
		if err != nil {
			return fmt.Errorf("insert workflow run: %w", err)
		}
		insertedRun = workflowRunFromRow(row)
		for _, step := range steps {
			artifactJSON := step.ArtifactJSON
			if artifactJSON == "" {
				artifactJSON = "{}"
			}
			stepRow, serr := q.InsertWorkflowStep(ctx, gen.InsertWorkflowStepParams{
				ID:              step.ID,
				WorkflowRunID:   step.WorkflowRunID,
				Kind:            step.Kind,
				Ordinal:         step.Ordinal,
				DependsOnStepID: stringPtrToNullString(step.DependsOnStepID),
				State:           step.State,
				CreatedAt:       step.CreatedAt,
				UpdatedAt:       step.UpdatedAt,
				ArtifactJson:    artifactJSON,
			})
			if serr != nil {
				return fmt.Errorf("insert workflow step %d: %w", step.Ordinal, serr)
			}
			insertedSteps = append(insertedSteps, workflowStepFromRow(stepRow))
		}
		planRow, perr := q.InsertWorkflowPlan(ctx, gen.InsertWorkflowPlanParams{
			WorkflowRunID:        run.ID,
			ApprovalMode:         string(mode),
			PromptContextVersion: contextVersion,
			CreatedAt:            now,
			UpdatedAt:            now,
		})
		if perr != nil {
			return fmt.Errorf("insert workflow plan: %w", perr)
		}
		insertedPlan = workflowPlanFromRow(planRow)
		return nil
	})
	if err != nil {
		return domain.WorkflowRun{}, nil, domain.WorkflowPlanRecord{}, err
	}
	return insertedRun, insertedSteps, insertedPlan, nil
}

// ReopenAmbiguousWorkflowPlan is CP7's fail-closed way back out of
// `planner_ambiguous`.
//
// A planner that was in flight when the daemon restarted leaves a plan AO
// cannot judge: it may have produced a complete plan and it may have produced
// nothing, and no durable row distinguishes the two. recovery.go's verdict —
// invalid/failed/planner_ambiguous — is therefore correct and stays. What was
// wrong is that it was PERMANENT: GeneratePlan's own status switch treats
// `invalid` as a no-op forever after, so a whole objective died of one crossed
// restart.
//
// This statement makes that state REOPENABLE, and nothing more. It does not
// adopt the discarded planner's work, does not decide whether a plan was
// produced, and does not run anything. It returns the row to `pending`/`idle` —
// the exact state StartWorkflowPlanCommand's own CAS arms from, and the state
// GeneratePlan falls through to real generation on — so the reopen re-enters
// the ordinary path rather than a parallel one.
//
// THE PREDICATE IS AN OBSERVED-VERSION COMPARE-AND-SWAP, NOT A GENERATION.
// The distinction is not pedantic and this comment must not be softened. A
// generation has to be minted by the thing it fences; the outbox's token is
// minted by the dispatch that claims the row, and a planner generation would
// have to be minted by the planner launch — but nothing durably records a
// planner subprocess at all: no intent row, no launch id, no handle, no natural
// key. There is no field to name and no writer to name it. So what the human's
// action carries is the plan ROW's version, `updated_at`, which every statement
// touching the row rewrites — including the FinishWorkflowPlan that wrote the
// ambiguity. That makes a second ambiguity a different version, so a reopen
// computed against the first matches zero rows and refuses.
//
// The three state columns alone would NOT be sufficient.
// SetWorkflowPlanApprovalMode is fenced only on status != 'approved', so it
// fires happily against an ambiguous row, changing approval_mode and updated_at
// and leaving status, command_status and error_class exactly as they were — and
// a second ambiguity writes the identical triple. The three columns are the
// TYPE CHECK ("this row is in the ambiguous-terminal state"); updated_at is what
// makes it THIS ambiguous-terminal state. Both halves are required.
//
// What it does not guarantee, stated plainly: two writes that land on an
// identical stored updated_at are indistinguishable, because the value comes
// from an injected clock and a test clock that does not advance (or a coarser
// storage precision) collapses two versions into one. In production the two
// ambiguities are separated by a daemon restart, so the exposure is negligible
// — but this is a row version, never a unique token, and never a generation.
//
// Hand-written rather than generated for the same reason
// ClaimWorkflowAttemptOutcome is: sqlc is not part of the build here, and the
// statement is deliberately trivial so it stays readable next to the generated
// ones.
func (s *Store) ReopenAmbiguousWorkflowPlan(
	ctx context.Context,
	runID string,
	observedUpdatedAt time.Time,
	validationJSON string,
	now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if strings.TrimSpace(validationJSON) == "" {
		validationJSON = "{}"
	}
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE workflow_plans
		    SET status = 'pending',
		        command_status = 'idle',
		        error_class = '',
		        validation_json = ?,
		        updated_at = ?
		  WHERE workflow_run_id = ?
		    AND status = 'invalid'
		    AND command_status = 'failed'
		    AND error_class = 'planner_ambiguous'
		    AND updated_at = ?`,
		validationJSON, now, runID, observedUpdatedAt)
	if err != nil {
		return false, fmt.Errorf("reopen ambiguous workflow plan %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reopen ambiguous workflow plan %s: %w", runID, err)
	}
	return n == 1, nil
}
