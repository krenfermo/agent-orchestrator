package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// InsertWorkflowQuestionResolution persists a new Decision Resolver attempt
// row (Checkpoint 8K-B, pass 1). If the attempt is inserted with
// status=running, the partial unique index on
// workflow_question_resolutions(workflow_question_id) WHERE status='running'
// (0105) rejects a second concurrent running attempt for the same question;
// the caller sees that as a plain error, not a special return value —
// mirroring how InsertWorkflowQuestion's fingerprint uniqueness is handled
// differently (INSERT OR IGNORE) is deliberate: unlike a duplicate captured
// question, which is expected and should be treated as a no-op, a second
// concurrent resolver dispatch for the same question is a bug/race that
// should surface loudly.
func (s *Store) InsertWorkflowQuestionResolution(ctx context.Context, r domain.WorkflowQuestionResolution) (domain.WorkflowQuestionResolution, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	evidenceJSON, err := marshalEvidenceReferences(r.EvidenceReferences)
	if err != nil {
		return domain.WorkflowQuestionResolution{}, fmt.Errorf("marshal evidence references: %w", err)
	}

	row, err := s.qw.InsertWorkflowQuestionResolution(ctx, gen.InsertWorkflowQuestionResolutionParams{
		ID:                 string(r.ID),
		WorkflowQuestionID: string(r.WorkflowQuestionID),
		WorkflowRunID:      string(r.WorkflowRunID),
		AskingSessionID:    sessionIDToNullString(r.AskingSessionID),
		ResolverHarness:    string(r.ResolverHarness),
		ResolverSessionID:  sessionIDToNullString(r.ResolverSessionID),
		Status:             string(r.Status),
		Answer:             sql.NullString{String: r.Answer, Valid: r.Answer != ""},
		ReasonSummary:      sql.NullString{String: r.ReasonSummary, Valid: r.ReasonSummary != ""},
		EvidenceReferences: evidenceJSON,
		Certainty:          certaintyToNullString(r.Certainty),
		RequiresHuman:      boolToInt64(r.RequiresHuman),
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
	})
	if err != nil {
		return domain.WorkflowQuestionResolution{}, fmt.Errorf("insert workflow question resolution: %w", err)
	}
	return workflowQuestionResolutionFromRow(row)
}

// GetWorkflowQuestionResolution fetches one resolution attempt by id.
func (s *Store) GetWorkflowQuestionResolution(ctx context.Context, id string) (domain.WorkflowQuestionResolution, bool, error) {
	row, err := s.qr.GetWorkflowQuestionResolution(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowQuestionResolution{}, false, nil
	}
	if err != nil {
		return domain.WorkflowQuestionResolution{}, false, fmt.Errorf("get workflow question resolution %s: %w", id, err)
	}
	out, derr := workflowQuestionResolutionFromRow(row)
	return out, true, derr
}

// GetCurrentResolutionForQuestion follows the question's resolving_run_id
// pointer to fetch whichever resolution attempt is (or was most recently)
// current. Returns ok=false, no error, when the question has never had a
// resolution attempt (resolving_run_id is NULL) or its pointer target is
// missing.
func (s *Store) GetCurrentResolutionForQuestion(ctx context.Context, questionID string) (domain.WorkflowQuestionResolution, bool, error) {
	row, err := s.qr.GetCurrentResolutionForQuestion(ctx, questionID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowQuestionResolution{}, false, nil
	}
	if err != nil {
		return domain.WorkflowQuestionResolution{}, false, fmt.Errorf("get current resolution for workflow question %s: %w", questionID, err)
	}
	out, derr := workflowQuestionResolutionFromRow(row)
	return out, true, derr
}

// TransitionResolutionStatus applies a CAS-style transition on a resolution
// attempt, mirroring AnswerWorkflowQuestion's compare-and-swap pattern: the
// update only applies if the row is currently in expectedStatus. Returns
// ok=false with no error if it was not (a concurrent transition, e.g. a
// staleness sweep racing a real completion) — the caller must treat that as
// "someone else already handled it", not a hard error.
func (s *Store) TransitionResolutionStatus(ctx context.Context, id string, expectedStatus, newStatus domain.ResolutionStatus, answer, reasonSummary string, evidenceReferences []string, certainty *domain.QuestionCertainty, requiresHuman bool, updatedAt time.Time, completedAt *time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	evidenceJSON, err := marshalEvidenceReferences(evidenceReferences)
	if err != nil {
		return false, fmt.Errorf("marshal evidence references: %w", err)
	}

	var completedAtVal sql.NullTime
	if completedAt != nil {
		completedAtVal = sql.NullTime{Time: *completedAt, Valid: true}
	}

	n, err := s.qw.TransitionResolutionStatus(ctx, gen.TransitionResolutionStatusParams{
		Status:             string(newStatus),
		Answer:             sql.NullString{String: answer, Valid: answer != ""},
		ReasonSummary:      sql.NullString{String: reasonSummary, Valid: reasonSummary != ""},
		EvidenceReferences: evidenceJSON,
		Certainty:          certaintyToNullString(certainty),
		RequiresHuman:      boolToInt64(requiresHuman),
		UpdatedAt:          updatedAt,
		CompletedAt:        completedAtVal,
		ID:                 id,
		ExpectedStatus:     string(expectedStatus),
	})
	if err != nil {
		return false, fmt.Errorf("transition workflow question resolution %s: %w", id, err)
	}
	return n > 0, nil
}

// ListRunningResolutions returns every resolution attempt currently marked
// running, across all runs. Pass 2's staleness-sweep target; no sweep logic
// lives here yet.
func (s *Store) ListRunningResolutions(ctx context.Context) ([]domain.WorkflowQuestionResolution, error) {
	rows, err := s.qr.ListRunningResolutions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running workflow question resolutions: %w", err)
	}
	out := make([]domain.WorkflowQuestionResolution, 0, len(rows))
	for _, r := range rows {
		res, derr := workflowQuestionResolutionFromRow(r)
		if derr != nil {
			return nil, derr
		}
		out = append(out, res)
	}
	return out, nil
}

// CancelRunningResolutionsByQuestion marks any currently-running resolution
// attempt for a question cancelled (run/question-cancel path, mirroring
// CancelOpenWorkflowQuestionsByRun). A no-op (0 rows, no error) when no
// attempt is currently running for that question.
func (s *Store) CancelRunningResolutionsByQuestion(ctx context.Context, questionID string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CancelRunningResolutionsByQuestion(ctx, gen.CancelRunningResolutionsByQuestionParams{
		UpdatedAt:          at,
		CompletedAt:        sql.NullTime{Time: at, Valid: true},
		WorkflowQuestionID: questionID,
	})
	if err != nil {
		return 0, fmt.Errorf("cancel running resolutions for workflow question %s: %w", questionID, err)
	}
	return n, nil
}

// ListWorkflowQuestionResolutionsByRun returns every resolution attempt ever
// recorded for a run, sorted by CreatedAt ascending. Checkpoint 8K-B pass
// 3's telemetry read source. Sorted here in Go, not via SQL ORDER BY: see
// the query's own doc comment in workflow_question_resolutions.sql for the
// reproduced sqlc v1.31.1 codegen bug that rules out a trailing ORDER BY on
// this specific query.
func (s *Store) ListWorkflowQuestionResolutionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestionResolution, error) {
	rows, err := s.qr.ListWorkflowQuestionResolutionsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list workflow question resolutions for run %s: %w", runID, err)
	}
	out := make([]domain.WorkflowQuestionResolution, 0, len(rows))
	for _, r := range rows {
		res, derr := workflowQuestionResolutionFromRow(r)
		if derr != nil {
			return nil, derr
		}
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// SetResolutionResolverSessionID records the resolver's launched session
// identity (Checkpoint 8K-B, pass 2) once Launch succeeds. Separate from
// TransitionResolutionStatus's CAS transition (that query has no
// resolver_session_id column) so the pending->running transition and this
// identity write are two small, independently-idempotent statements.
func (s *Store) SetResolutionResolverSessionID(ctx context.Context, id string, resolverSessionID string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SetResolutionResolverSessionID(ctx, gen.SetResolutionResolverSessionIDParams{
		ResolverSessionID: sql.NullString{String: resolverSessionID, Valid: resolverSessionID != ""},
		ID:                id,
	})
	if err != nil {
		return false, fmt.Errorf("set resolver session id for resolution %s: %w", id, err)
	}
	return n > 0, nil
}

func marshalEvidenceReferences(refs []string) (sql.NullString, error) {
	if len(refs) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

func unmarshalEvidenceReferences(s sql.NullString) ([]string, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	var refs []string
	if err := json.Unmarshal([]byte(s.String), &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

func certaintyToNullString(c *domain.QuestionCertainty) sql.NullString {
	if c == nil || *c == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: string(*c), Valid: true}
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func workflowQuestionResolutionFromRow(r gen.WorkflowQuestionResolution) (domain.WorkflowQuestionResolution, error) {
	evidence, err := unmarshalEvidenceReferences(r.EvidenceReferences)
	if err != nil {
		return domain.WorkflowQuestionResolution{}, fmt.Errorf("unmarshal evidence references for resolution %s: %w", r.ID, err)
	}

	var askingSessionID *domain.SessionID
	if r.AskingSessionID.Valid {
		v := domain.SessionID(r.AskingSessionID.String)
		askingSessionID = &v
	}
	var resolverSessionID *domain.SessionID
	if r.ResolverSessionID.Valid {
		v := domain.SessionID(r.ResolverSessionID.String)
		resolverSessionID = &v
	}
	var certainty *domain.QuestionCertainty
	if r.Certainty.Valid {
		v := domain.QuestionCertainty(r.Certainty.String)
		certainty = &v
	}

	return domain.WorkflowQuestionResolution{
		ID:                 domain.WorkflowQuestionResolutionID(r.ID),
		WorkflowQuestionID: domain.WorkflowQuestionID(r.WorkflowQuestionID),
		WorkflowRunID:      domain.WorkflowRunID(r.WorkflowRunID),
		AskingSessionID:    askingSessionID,
		ResolverHarness:    domain.AgentHarness(r.ResolverHarness),
		ResolverSessionID:  resolverSessionID,
		Status:             domain.ResolutionStatus(r.Status),
		Answer:             r.Answer.String,
		ReasonSummary:      r.ReasonSummary.String,
		EvidenceReferences: evidence,
		Certainty:          certainty,
		RequiresHuman:      r.RequiresHuman != 0,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
		CompletedAt:        nullTimeToTimePtr(r.CompletedAt),
	}, nil
}
