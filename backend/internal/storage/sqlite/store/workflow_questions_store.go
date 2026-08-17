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

// InsertWorkflowQuestion persists a captured, already-classified question
// (Checkpoint 8K-A). INSERT OR IGNORE on the unique fingerprint index makes
// a duplicate detection a free no-op: if a row with this fingerprint
// already exists, the insert affects zero rows and this method fetches and
// returns the existing row instead (inserted=false) rather than erroring or
// reclassifying.
func (s *Store) InsertWorkflowQuestion(ctx context.Context, q domain.WorkflowQuestion) (domain.WorkflowQuestion, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	choicesJSON, err := marshalChoices(q.StructuredChoices)
	if err != nil {
		return domain.WorkflowQuestion{}, false, fmt.Errorf("marshal structured choices: %w", err)
	}

	row, err := s.qw.InsertWorkflowQuestion(ctx, gen.InsertWorkflowQuestionParams{
		ID:                   string(q.ID),
		WorkflowRunID:        string(q.WorkflowRunID),
		WorkflowStepID:       stepIDToNullString(q.WorkflowStepID),
		WorkflowAttemptID:    stringPtrToNullString(q.WorkflowAttemptID),
		SessionID:            sessionIDToNullString(q.SessionID),
		AskingHarness:        sql.NullString{String: string(q.AskingHarness), Valid: q.AskingHarness != ""},
		AskingRole:           sql.NullString{String: q.AskingRole, Valid: q.AskingRole != ""},
		Fingerprint:          q.Fingerprint,
		QuestionText:         q.QuestionText,
		StructuredChoices:    choicesJSON,
		CaptureProvider:      sql.NullString{String: q.CaptureProvider, Valid: q.CaptureProvider != ""},
		CaptureParserVersion: sql.NullString{String: q.CaptureParserVersion, Valid: q.CaptureParserVersion != ""},
		CaptureRangeLines:    sql.NullInt64{Int64: int64(q.CaptureRangeLines), Valid: q.CaptureRangeLines != 0},
		Certainty:            sql.NullString{String: string(q.Certainty), Valid: q.Certainty != ""},
		Classification:       sql.NullString{String: string(q.Classification), Valid: q.Classification != ""},
		ClassificationReason: sql.NullString{String: q.ClassificationReason, Valid: q.ClassificationReason != ""},
		State:                sql.NullString{String: string(q.State), Valid: q.State != ""},
		CreatedAt:            q.CreatedAt,
	})
	if errors.Is(err, sql.ErrNoRows) {
		// INSERT OR IGNORE skipped the row: a duplicate fingerprint already
		// exists. Fetch the existing row rather than treating this as an
		// error — this is the fingerprint-dedup no-op path.
		existing, ferr := s.qw.GetWorkflowQuestionByFingerprint(ctx, q.Fingerprint)
		if ferr != nil {
			return domain.WorkflowQuestion{}, false, fmt.Errorf("fetch existing question by fingerprint after ignored insert: %w", ferr)
		}
		out, derr := workflowQuestionFromRow(existing)
		return out, false, derr
	}
	if err != nil {
		return domain.WorkflowQuestion{}, false, fmt.Errorf("insert workflow question: %w", err)
	}
	out, derr := workflowQuestionFromRow(row)
	return out, true, derr
}

// GetWorkflowQuestion fetches one question by id.
func (s *Store) GetWorkflowQuestion(ctx context.Context, id string) (domain.WorkflowQuestion, bool, error) {
	row, err := s.qr.GetWorkflowQuestion(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowQuestion{}, false, nil
	}
	if err != nil {
		return domain.WorkflowQuestion{}, false, fmt.Errorf("get workflow question %s: %w", id, err)
	}
	out, derr := workflowQuestionFromRow(row)
	return out, true, derr
}

// ListOpenWorkflowQuestionsByRun returns pending/human_required questions
// for a run, used both by the dispatch-guard dedup check and the API view.
func (s *Store) ListOpenWorkflowQuestionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error) {
	rows, err := s.qr.ListOpenWorkflowQuestionsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list open workflow questions for run %s: %w", runID, err)
	}
	return workflowQuestionsFromRows(rows)
}

// ListWorkflowQuestionsByRun returns all questions (any state) for a run,
// oldest first.
func (s *Store) ListWorkflowQuestionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error) {
	rows, err := s.qr.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list workflow questions for run %s: %w", runID, err)
	}
	return workflowQuestionsFromRows(rows)
}

// ListPendingWorkflowQuestions returns every open/in-flight question across
// ALL runs (no run-id filter), optionally restricted to the given states.
// Checkpoint 8K-B pass 3's global "Pending Decisions" inbox source. An empty
// states slice defaults to the inbox's three states
// (human_required/resolving/waiting_for_capacity is a NextAction, not a
// persisted state, so "resolving" covers it here — see
// pendingDecisionsDefaultStates's doc comment at the call site).
//
// Hand-written raw SQL against the read connection, mirroring
// TransitionWorkflowQuestionState's documented exception: this file
// (workflow_questions.sql) has a reproduced sqlc v1.31.1 codegen bug, so
// pass 2 established the precedent of hand-writing new queries here rather
// than risking silent corruption by adding to the generated file. A plain
// multi-state SELECT is exactly the shape that bug has been seen to corrupt
// (see workflow_question_resolutions.sql's own from-scratch repro), so this
// follows the same precedent rather than trying sqlc again.
func (s *Store) ListPendingWorkflowQuestions(ctx context.Context, states []string) ([]domain.WorkflowQuestion, error) {
	if len(states) == 0 {
		states = []string{string(domain.QuestionStateHumanRequired), string(domain.QuestionStateResolving)}
	}
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = st
	}
	query := `SELECT id, workflow_run_id, workflow_step_id, workflow_attempt_id, session_id,
       asking_harness, asking_role, fingerprint, question_text, structured_choices,
       capture_provider, capture_parser_version, capture_range_lines,
       certainty, classification, classification_reason, state, created_at,
       answered_at, answer_source, answer_text, answer_reference, delivered, delivered_at,
       resolving_run_id
FROM workflow_questions
WHERE state IN (` + strings.Join(placeholders, ",") + `)
ORDER BY created_at`
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending workflow questions: %w", err)
	}
	defer rows.Close()

	out := make([]domain.WorkflowQuestion, 0)
	for rows.Next() {
		var r gen.WorkflowQuestion
		if err := rows.Scan(
			&r.ID, &r.WorkflowRunID, &r.WorkflowStepID, &r.WorkflowAttemptID, &r.SessionID,
			&r.AskingHarness, &r.AskingRole, &r.Fingerprint, &r.QuestionText, &r.StructuredChoices,
			&r.CaptureProvider, &r.CaptureParserVersion, &r.CaptureRangeLines,
			&r.Certainty, &r.Classification, &r.ClassificationReason, &r.State, &r.CreatedAt,
			&r.AnsweredAt, &r.AnswerSource, &r.AnswerText, &r.AnswerReference, &r.Delivered, &r.DeliveredAt,
			&r.ResolvingRunID,
		); err != nil {
			return nil, fmt.Errorf("scan pending workflow question: %w", err)
		}
		q, derr := workflowQuestionFromRow(r)
		if derr != nil {
			return nil, derr
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending workflow questions: %w", err)
	}
	return out, nil
}

// CountPolicyAnsweredWorkflowQuestionsByStep backs the
// MaxAutoAnsweredQuestionsPerStep budget guard.
func (s *Store) CountPolicyAnsweredWorkflowQuestionsByStep(ctx context.Context, stepID string) (int64, error) {
	n, err := s.qr.CountPolicyAnsweredWorkflowQuestionsByStep(ctx, sql.NullString{String: stepID, Valid: stepID != ""})
	if err != nil {
		return 0, fmt.Errorf("count policy-answered workflow questions for step %s: %w", stepID, err)
	}
	return n, nil
}

// AnswerWorkflowQuestion persists an answer (policy or human) for a
// question currently in expectedState, transitioning it to newState
// (normally answered). Returns ok=false with no error if the question was
// not in expectedState (a concurrent answer, e.g. a duplicate human
// submission) — the caller must treat that as "already handled", not a
// hard error.
func (s *Store) AnswerWorkflowQuestion(ctx context.Context, id string, expectedState, newState domain.QuestionState, source domain.AnswerSource, answerText, answerReference string, answeredAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.AnswerWorkflowQuestion(ctx, gen.AnswerWorkflowQuestionParams{
		State:           sql.NullString{String: string(newState), Valid: true},
		AnsweredAt:      sql.NullTime{Time: answeredAt, Valid: !answeredAt.IsZero()},
		AnswerSource:    sql.NullString{String: string(source), Valid: true},
		AnswerText:      sql.NullString{String: answerText, Valid: true},
		AnswerReference: sql.NullString{String: answerReference, Valid: answerReference != ""},
		ID:              id,
		ExpectedState:   sql.NullString{String: string(expectedState), Valid: true},
	})
	if err != nil {
		return false, fmt.Errorf("answer workflow question %s: %w", id, err)
	}
	return n > 0, nil
}

// MarkWorkflowQuestionDelivered flips delivered=0->1 idempotently: a second
// call after delivery is already true affects zero rows and returns
// ok=false with no error.
func (s *Store) MarkWorkflowQuestionDelivered(ctx context.Context, id string, deliveredAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkWorkflowQuestionDelivered(ctx, gen.MarkWorkflowQuestionDeliveredParams{
		DeliveredAt: sql.NullTime{Time: deliveredAt, Valid: !deliveredAt.IsZero()},
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("mark workflow question %s delivered: %w", id, err)
	}
	return n > 0, nil
}

// ListUndeliveredAnsweredWorkflowQuestions is the restart-recovery sweep
// target: state=answered AND delivered=0 for a run.
func (s *Store) ListUndeliveredAnsweredWorkflowQuestions(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error) {
	rows, err := s.qr.ListUndeliveredAnsweredWorkflowQuestions(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list undelivered answered workflow questions for run %s: %w", runID, err)
	}
	return workflowQuestionsFromRows(rows)
}

// CancelOpenWorkflowQuestionsByRun marks all pending/human_required
// questions for a run cancelled (run-cancel path). No resolver call, no
// delivery attempt follows for a cancelled question.
func (s *Store) CancelOpenWorkflowQuestionsByRun(ctx context.Context, runID string) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CancelOpenWorkflowQuestionsByRun(ctx, runID)
	if err != nil {
		return 0, fmt.Errorf("cancel open workflow questions for run %s: %w", runID, err)
	}
	return n, nil
}

// SetWorkflowQuestionResolvingRunID points a question at its currently
// in-flight Decision Resolver attempt (Checkpoint 8K-B, pass 1), or clears
// the pointer when runID is nil. No CAS guard: the resolution row's own
// status plus the partial unique index on workflow_question_resolutions is
// what prevents two concurrent running attempts; this pointer only records
// which attempt is current.
func (s *Store) SetWorkflowQuestionResolvingRunID(ctx context.Context, questionID string, runID *string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SetWorkflowQuestionResolvingRunID(ctx, gen.SetWorkflowQuestionResolvingRunIDParams{
		ResolvingRunID: stringPtrToNullString(runID),
		ID:             questionID,
	})
	if err != nil {
		return false, fmt.Errorf("set resolving_run_id for workflow question %s: %w", questionID, err)
	}
	return n > 0, nil
}

// TransitionWorkflowQuestionState applies a CAS-style state-only transition
// (Checkpoint 8K-B, pass 2): the update only applies if the row is currently
// in expectedState, mirroring AnswerWorkflowQuestion's compare-and-swap
// pattern exactly — but WITHOUT touching answered_at/answer_source/
// answer_text/answer_reference, since forcing resolving -> human_required
// (resolver failed/stale/requires_human) or any open/resolving state ->
// cancelled is never an "answer" and must never look like one on the row.
//
// This is a hand-written parameterized statement executed directly against
// the write connection rather than a generated sqlc query. sqlc v1.31.1 has
// a reproducible SQLite-codegen bug in this exact file: a CAS-style
// UPDATE ... SET <col> = ?, ... WHERE id = ? AND <col> = ? query (any
// combination of plain "?", numbered "?N", or sqlc.arg(...) tried) either
// silently truncates its final placeholder out of the generated raw SQL
// string constant, or hard-fails to parse — verified via an isolated
// single-query repro that generates the identical query text correctly on
// its own, so the corruption is specific to this query coexisting with the
// rest of this file's queries, not the query shape itself. Given a choice
// between shipping a state-transition query nobody can safely verify wasn't
// silently corrupted, and a few hand-written lines using the store's
// existing writeDB/writeMu, this is the safer, explicit exception.
func (s *Store) TransitionWorkflowQuestionState(ctx context.Context, id string, expected, next domain.QuestionState, reason string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE workflow_questions SET state = ?, classification_reason = ? WHERE id = ? AND state = ?`,
		string(next), reason, id, string(expected),
	)
	if err != nil {
		return false, fmt.Errorf("transition workflow question %s state: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("transition workflow question %s state: %w", id, err)
	}
	return n > 0, nil
}

func marshalChoices(choices []domain.QuestionChoice) (sql.NullString, error) {
	if len(choices) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(choices)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

func unmarshalChoices(s sql.NullString) ([]domain.QuestionChoice, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	var choices []domain.QuestionChoice
	if err := json.Unmarshal([]byte(s.String), &choices); err != nil {
		return nil, err
	}
	return choices, nil
}

func stepIDToNullString(id *domain.WorkflowStepID) sql.NullString {
	if id == nil || *id == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: string(*id), Valid: true}
}

func sessionIDToNullString(id *domain.SessionID) sql.NullString {
	if id == nil || *id == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: string(*id), Valid: true}
}

func workflowQuestionsFromRows(rows []gen.WorkflowQuestion) ([]domain.WorkflowQuestion, error) {
	out := make([]domain.WorkflowQuestion, 0, len(rows))
	for _, r := range rows {
		q, err := workflowQuestionFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

func workflowQuestionFromRow(r gen.WorkflowQuestion) (domain.WorkflowQuestion, error) {
	choices, err := unmarshalChoices(r.StructuredChoices)
	if err != nil {
		return domain.WorkflowQuestion{}, fmt.Errorf("unmarshal structured choices for question %s: %w", r.ID, err)
	}

	var stepID *domain.WorkflowStepID
	if r.WorkflowStepID.Valid {
		v := domain.WorkflowStepID(r.WorkflowStepID.String)
		stepID = &v
	}
	var attemptID *string
	if r.WorkflowAttemptID.Valid {
		v := r.WorkflowAttemptID.String
		attemptID = &v
	}
	var sessionID *domain.SessionID
	if r.SessionID.Valid {
		v := domain.SessionID(r.SessionID.String)
		sessionID = &v
	}
	var answerSource *domain.AnswerSource
	if r.AnswerSource.Valid {
		v := domain.AnswerSource(r.AnswerSource.String)
		answerSource = &v
	}
	var resolvingRunID *domain.WorkflowQuestionResolutionID
	if r.ResolvingRunID.Valid {
		v := domain.WorkflowQuestionResolutionID(r.ResolvingRunID.String)
		resolvingRunID = &v
	}

	return domain.WorkflowQuestion{
		ID:                   domain.WorkflowQuestionID(r.ID),
		WorkflowRunID:        domain.WorkflowRunID(r.WorkflowRunID),
		WorkflowStepID:       stepID,
		WorkflowAttemptID:    attemptID,
		SessionID:            sessionID,
		AskingHarness:        domain.AgentHarness(r.AskingHarness.String),
		AskingRole:           r.AskingRole.String,
		Fingerprint:          r.Fingerprint,
		QuestionText:         r.QuestionText,
		StructuredChoices:    choices,
		CaptureProvider:      r.CaptureProvider.String,
		CaptureParserVersion: r.CaptureParserVersion.String,
		CaptureRangeLines:    int(r.CaptureRangeLines.Int64),
		Certainty:            domain.QuestionCertainty(r.Certainty.String),
		Classification:       domain.QuestionClassification(r.Classification.String),
		ClassificationReason: r.ClassificationReason.String,
		State:                domain.QuestionState(r.State.String),
		CreatedAt:            r.CreatedAt,
		AnsweredAt:           nullTimeToTimePtr(r.AnsweredAt),
		AnswerSource:         answerSource,
		AnswerText:           r.AnswerText.String,
		AnswerReference:      r.AnswerReference.String,
		Delivered:            r.Delivered != 0,
		DeliveredAt:          nullTimeToTimePtr(r.DeliveredAt),
		ResolvingRunID:       resolvingRunID,
	}, nil
}
