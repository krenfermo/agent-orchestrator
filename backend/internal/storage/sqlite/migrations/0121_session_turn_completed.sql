-- Persist the receipt that a task finished the work it was given, so a
-- successful task reads Completed instead of Idle once it goes quiet.
--
-- AO has no task entity: a task IS a session, and a session that finishes its
-- work deliberately stays alive so the user can keep talking to it. Until now
-- the only durable trace a finished turn left was activity_state='idle' — the
-- same value a session that never did anything carries, and the same value a
-- session whose agent simply went quiet carries. That is why a finished task
-- came back as "Inactive": the fact that it succeeded was never written down,
-- only observed and discarded by the reducer.
--
-- turn_completed_at is that missing fact. Lifecycle stamps it from the agent's
-- own report that its turn ended (a Stop-class hook, or the Chat driver's
-- turn-completed event) and clears it the moment work is in flight again, so
-- it always describes the CURRENT quiet period. It is never written from an
-- idle reading, a runtime probe, a dead pane, or elapsed time.
--
-- NULL means "no completion reported for the current turn". Every existing row
-- reads that way, so no historical session is promoted to Completed by the
-- mere act of upgrading; the backfill below moves only the ones whose success
-- is already proven by another durable record.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN turn_completed_at TIMESTAMP;
-- +goose StatementEnd

-- Historical evidence, and the only kind AO already has: a session-owned
-- execution lock that was released because the task's turn ended (Checkpoint
-- 8P-E.14A writes that reason and no other path does). It proves the same
-- thing the new column records — this session reported the end of its work —
-- so a direct-branch task that finished before this migration is classified
-- from its own history rather than from a guess. Restricted to a live, idle,
-- non-terminated session holding no lock, and to the turn-ended-by-stop
-- reason: teardown releases ('session-end', 'process-exited', termination)
-- prove only that the agent went away.
-- +goose StatementBegin
UPDATE sessions
SET turn_completed_at = (
    SELECT MAX(l.released_at)
    FROM branch_locks AS l
    WHERE l.session_id = sessions.id
      AND l.workflow_run_id = ''
      AND l.state = 'released'
      AND l.released_at IS NOT NULL
      AND l.release_reason LIKE 'task turn ended (stop)%'
)
WHERE is_terminated = 0
  AND activity_state = 'idle'
  AND NOT EXISTS (
      SELECT 1 FROM branch_locks AS h
      WHERE h.session_id = sessions.id AND h.state = 'held'
  )
  AND EXISTS (
      SELECT 1 FROM branch_locks AS l
      WHERE l.session_id = sessions.id
        AND l.workflow_run_id = ''
        AND l.state = 'released'
        AND l.released_at IS NOT NULL
        AND l.release_reason LIKE 'task turn ended (stop)%'
  );
-- +goose StatementEnd

-- The completion receipt drives a user-visible status, and it can change on a
-- Stop that lands on an already-idle row — no activity_state change to piggy
-- back on — so subscribers have to be invalidated by the column itself.
-- +goose StatementBegin
DROP TRIGGER IF EXISTS sessions_cdc_update;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER sessions_cdc_update
AFTER UPDATE ON sessions
WHEN OLD.activity_state <> NEW.activity_state
    OR OLD.is_terminated <> NEW.is_terminated
    OR (OLD.first_signal_at IS NULL AND NEW.first_signal_at IS NOT NULL)
    OR (OLD.turn_completed_at IS NULL AND NEW.turn_completed_at IS NOT NULL)
    OR (OLD.turn_completed_at IS NOT NULL AND NEW.turn_completed_at IS NULL)
    OR OLD.preview_url <> NEW.preview_url
    OR OLD.preview_revision <> NEW.preview_revision
    OR OLD.display_name <> NEW.display_name
    OR OLD.terminate_on_pr_merge <> NEW.terminate_on_pr_merge
    OR OLD.is_pinned <> NEW.is_pinned
    OR OLD.pinned_at <> NEW.pinned_at
    OR (OLD.pinned_at IS NULL AND NEW.pinned_at IS NOT NULL)
    OR (OLD.pinned_at IS NOT NULL AND NEW.pinned_at IS NULL)
    OR OLD.session_mode <> NEW.session_mode
    OR OLD.auto_inject_review <> NEW.auto_inject_review
    OR OLD.auto_review_enabled <> NEW.auto_review_enabled
    OR OLD.harness <> NEW.harness
    OR OLD.runtime_launch_id <> NEW.runtime_launch_id
    OR OLD.agent_session_id <> NEW.agent_session_id
    OR OLD.native_transcript_path <> NEW.native_transcript_path
    OR OLD.auto_inject_ci <> NEW.auto_inject_ci
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_updated',
        json_object(
            'id', NEW.id,
            'activity', NEW.activity_state,
            'isTerminated', json(CASE WHEN NEW.is_terminated THEN 'true' ELSE 'false' END),
            'terminateOnPrMerge', json(CASE WHEN NEW.terminate_on_pr_merge THEN 'true' ELSE 'false' END),
            'previewUrl', NEW.preview_url,
            'previewRevision', NEW.preview_revision,
            'isPinned', json(CASE WHEN NEW.is_pinned THEN 'true' ELSE 'false' END),
            'mode', NEW.session_mode,
            'autoInjectReview', json(CASE WHEN NEW.auto_inject_review THEN 'true' ELSE 'false' END),
            'autoInjectCI', json(CASE WHEN NEW.auto_inject_ci THEN 'true' ELSE 'false' END),
            'autoReviewEnabled', json(CASE WHEN NEW.auto_review_enabled THEN 'true' ELSE 'false' END)
        ),
        NEW.updated_at);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS sessions_cdc_update;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN turn_completed_at;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER sessions_cdc_update
AFTER UPDATE ON sessions
WHEN OLD.activity_state <> NEW.activity_state
    OR OLD.is_terminated <> NEW.is_terminated
    OR (OLD.first_signal_at IS NULL AND NEW.first_signal_at IS NOT NULL)
    OR OLD.preview_url <> NEW.preview_url
    OR OLD.preview_revision <> NEW.preview_revision
    OR OLD.display_name <> NEW.display_name
    OR OLD.terminate_on_pr_merge <> NEW.terminate_on_pr_merge
    OR OLD.is_pinned <> NEW.is_pinned
    OR OLD.pinned_at <> NEW.pinned_at
    OR (OLD.pinned_at IS NULL AND NEW.pinned_at IS NOT NULL)
    OR (OLD.pinned_at IS NOT NULL AND NEW.pinned_at IS NULL)
    OR OLD.session_mode <> NEW.session_mode
    OR OLD.auto_inject_review <> NEW.auto_inject_review
    OR OLD.auto_review_enabled <> NEW.auto_review_enabled
    OR OLD.harness <> NEW.harness
    OR OLD.runtime_launch_id <> NEW.runtime_launch_id
    OR OLD.agent_session_id <> NEW.agent_session_id
    OR OLD.native_transcript_path <> NEW.native_transcript_path
    OR OLD.auto_inject_ci <> NEW.auto_inject_ci
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_updated',
        json_object(
            'id', NEW.id,
            'activity', NEW.activity_state,
            'isTerminated', json(CASE WHEN NEW.is_terminated THEN 'true' ELSE 'false' END),
            'terminateOnPrMerge', json(CASE WHEN NEW.terminate_on_pr_merge THEN 'true' ELSE 'false' END),
            'previewUrl', NEW.preview_url,
            'previewRevision', NEW.preview_revision,
            'isPinned', json(CASE WHEN NEW.is_pinned THEN 'true' ELSE 'false' END),
            'mode', NEW.session_mode,
            'autoInjectReview', json(CASE WHEN NEW.auto_inject_review THEN 'true' ELSE 'false' END),
            'autoInjectCI', json(CASE WHEN NEW.auto_inject_ci THEN 'true' ELSE 'false' END),
            'autoReviewEnabled', json(CASE WHEN NEW.auto_review_enabled THEN 'true' ELSE 'false' END)
        ),
        NEW.updated_at);
END;
-- +goose StatementEnd
