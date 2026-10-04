-- Persist WHEN A SESSION WAS LAST HEARD FROM, separately from when its
-- activity state last CHANGED.
--
-- activity_last_at has always been a transition clock. lifecycle's reducer
-- folds a signal that carries the state the row already holds and returns
-- without writing (the `sameState` early return), which is correct for every
-- consumer scoped to a pause: one notification per pause, one blocked-entry
-- fact per dialog, one waiting-input measurement per episode. Refreshing it on
-- every repeat would collapse all of those onto "now".
--
-- The cost was that AO had no other clock, so "is this worker alive?" was
-- answered from the transition one. An agent that stays `active` for twenty
-- minutes emits scores of PostToolUse signals and changes state in none of
-- them, so it is indistinguishable from an agent that said `active` once and
-- died. Run wf-1c2cb9bd (2026-09-09, project medusa) is the worked example:
-- activity_last_at pinned at 17:10:20Z while the worker made 111 model calls
-- over the following twenty minutes, edited seven files across two
-- repositories and ran two test suites. The Board reported it as silent, and
-- workerNeedsInputCorroborationWindow -- whose own comment asserts that "for a
-- worker that is genuinely still working, [the reading] keeps moving ... so an
-- actively working agent can never age into it" -- was one uncorroborated
-- waiting_input hint away from stopping a healthy run as ambiguous.
--
-- last_signal_at is that missing clock. Lifecycle stamps it from every signal
-- that provably belongs to the session's current launch, coalesced to bound
-- write amplification (see lifecycle.livenessCoalesceWindow). It is never
-- consulted to decide a state, a status, a completion or a pause: those all
-- continue to read the facts they already read.
--
-- It is deliberately NOT added to sessions_cdc_update. change_log is an
-- append-only ledger with no retention policy, and a heartbeat does not belong
-- in one; the clients that need liveness (the Board, the run view) already
-- poll while a run is non-terminal, so the column reaches them there.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN last_signal_at TIMESTAMP;
-- +goose StatementEnd

-- Every existing row is seeded from the transition clock. That is not an
-- invention: activity_last_at is a real moment at which this session was heard
-- from -- it is simply the last such moment that also changed the state. It is
-- therefore a lower bound on liveness, which is the safe direction: a session
-- backfilled this way can read as quieter than it was, never as more alive.
-- +goose StatementBegin
UPDATE sessions SET last_signal_at = activity_last_at WHERE last_signal_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN last_signal_at;
-- +goose StatementEnd
