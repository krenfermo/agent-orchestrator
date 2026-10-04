-- P1-D §C: durable runtime ownership evidence for worker sessions.
--
-- P1-C's Runtime GC can prove a REVIEWER pane is AO's own — reviewers attach an
-- ownership token at creation (ports.RuntimeConfig.Owner) and the tmux adapter
-- reads it back per incarnation. Worker sessions carried no such token, and the
-- sessions table recorded no incarnation either, so a finished worker's tmux
-- session was only ever provably AO's when a P1-C capacity claim happened to
-- name it. Everything older was reported "unprovable" and left on the machine
-- forever. That was P1-C's own documented deferral; this closes it.
--
-- Two columns, both pure additions:
--
--   * runtime_instance_id — the immutable incarnation (tmux's `$N`) the
--     session's runtime was created as. It is the AUTHORITY key for every
--     destructive action; runtime_handle_id is a reusable name and is only a
--     discovery key. Recording it is what lets GC address the exact session AO
--     created rather than whatever now answers to its name.
--
--   * runtime_owner_token — the ownership marker attached ATOMICALLY at
--     creation. It binds the runtime to this session AND to the launch
--     generation (runtime_launch_id), so a token read back off a live session
--     proves not merely "AO made this" but "AO made this for this session's
--     launch N". A newer launch of the same session carries a different token,
--     which is what stops a stale handle adopting a replacement.
--
-- Both default to '' — the honest value for every session that already exists.
-- An empty token is NOT ownership, and GC must keep leaving those alone: this
-- migration adds the ability to prove ownership going forward, and
-- deliberately does not fabricate it backwards.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN runtime_instance_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN runtime_owner_token TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN runtime_owner_token;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN runtime_instance_id;
-- +goose StatementEnd
