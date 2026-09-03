-- name: InsertWorkflowQuestion :one
-- Checkpoint 8K-A: durable question row, inserted with classification
-- already computed (never "persisted but unclassified"). INSERT OR IGNORE
-- on the unique fingerprint index makes a duplicate detection a free no-op.
INSERT OR IGNORE INTO workflow_questions (
    id, workflow_run_id, workflow_step_id, workflow_attempt_id, session_id,
    asking_harness, asking_role, fingerprint, question_text, structured_choices,
    capture_provider, capture_parser_version, capture_range_lines,
    certainty, classification, classification_reason, state, created_at, autonomy_mode
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, workflow_run_id, workflow_step_id, workflow_attempt_id, session_id,
          asking_harness, asking_role, fingerprint, question_text, structured_choices,
          capture_provider, capture_parser_version, capture_range_lines,
          certainty, classification, classification_reason, state, created_at,
          answered_at, answer_source, answer_text, answer_reference, delivered, delivered_at,
          resolving_run_id, autonomy_mode;

-- name: GetWorkflowQuestionByFingerprint :one
SELECT id, workflow_run_id, workflow_step_id, workflow_attempt_id, session_id,
       asking_harness, asking_role, fingerprint, question_text, structured_choices,
       capture_provider, capture_parser_version, capture_range_lines,
       certainty, classification, classification_reason, state, created_at,
       answered_at, answer_source, answer_text, answer_reference, delivered, delivered_at,
       resolving_run_id, autonomy_mode
FROM workflow_questions
WHERE fingerprint = ?;

-- name: GetWorkflowQuestion :one
SELECT id, workflow_run_id, workflow_step_id, workflow_attempt_id, session_id,
       asking_harness, asking_role, fingerprint, question_text, structured_choices,
       capture_provider, capture_parser_version, capture_range_lines,
       certainty, classification, classification_reason, state, created_at,
       answered_at, answer_source, answer_text, answer_reference, delivered, delivered_at,
       resolving_run_id, autonomy_mode
FROM workflow_questions
WHERE id = ?;

-- name: ListOpenWorkflowQuestionsByRun :many
-- Open = state IN (pending, human_required): used both for the dispatch-guard
-- dedup check (don't re-scrape a pane that already produced an open question)
-- and for the API's per-run questions view.
SELECT id, workflow_run_id, workflow_step_id, workflow_attempt_id, session_id,
       asking_harness, asking_role, fingerprint, question_text, structured_choices,
       capture_provider, capture_parser_version, capture_range_lines,
       certainty, classification, classification_reason, state, created_at,
       answered_at, answer_source, answer_text, answer_reference, delivered, delivered_at,
       resolving_run_id, autonomy_mode
FROM workflow_questions
WHERE workflow_run_id = ? AND state IN ('pending', 'human_required')
ORDER BY created_at;

-- name: ListWorkflowQuestionsByRun :many
SELECT id, workflow_run_id, workflow_step_id, workflow_attempt_id, session_id,
       asking_harness, asking_role, fingerprint, question_text, structured_choices,
       capture_provider, capture_parser_version, capture_range_lines,
       certainty, classification, classification_reason, state, created_at,
       answered_at, answer_source, answer_text, answer_reference, delivered, delivered_at,
       resolving_run_id, autonomy_mode
FROM workflow_questions
WHERE workflow_run_id = ?
ORDER BY created_at;

-- name: CountPolicyAnsweredWorkflowQuestionsByStep :one
-- Backs the MaxAutoAnsweredQuestionsPerStep budget guard.
SELECT COUNT(*) FROM workflow_questions
WHERE workflow_step_id = ? AND answer_source = 'policy';

-- name: AnswerWorkflowQuestion :execrows
UPDATE workflow_questions
SET state = ?, answered_at = ?, answer_source = ?, answer_text = ?, answer_reference = ?
WHERE id = ? AND state = sqlc.arg(expected_state);

-- name: MarkWorkflowQuestionDelivered :execrows
UPDATE workflow_questions
SET delivered = 1, delivered_at = ?
WHERE id = ? AND delivered = 0;

-- name: ListUndeliveredAnsweredWorkflowQuestions :many
-- Restart-recovery sweep target: answered but not yet delivered.
SELECT id, workflow_run_id, workflow_step_id, workflow_attempt_id, session_id,
       asking_harness, asking_role, fingerprint, question_text, structured_choices,
       capture_provider, capture_parser_version, capture_range_lines,
       certainty, classification, classification_reason, state, created_at,
       answered_at, answer_source, answer_text, answer_reference, delivered, delivered_at,
       resolving_run_id, autonomy_mode
FROM workflow_questions
WHERE workflow_run_id = ? AND state = 'answered' AND delivered = 0;

-- name: CancelOpenWorkflowQuestionsByRun :execrows
UPDATE workflow_questions
SET state = 'cancelled'
WHERE workflow_run_id = ? AND state IN ('pending', 'human_required');

-- name: SetWorkflowQuestionResolvingRunID :execrows
-- Checkpoint 8K-B (pass 1): points a question at its currently in-flight
-- resolution attempt (workflow_question_resolutions.id), or clears the
-- pointer (NULL) once the attempt is no longer current. No CAS guard here:
-- the resolution row's own status/partial-unique-index is what prevents two
-- concurrent running attempts; this pointer is just "which one is current".
UPDATE workflow_questions
SET resolving_run_id = ?
WHERE id = ?;
