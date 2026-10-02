-- AR-1a / D-SEC-2 (Codex AR1A-02): close the window in which a review run is
-- already 'running' but its reviewer's own credential has not been minted yet.
--
-- A review verdict submitted with no agent credential is accepted for a
-- running run only when no reviewer identity speaks for that run. Deciding
-- that from the credential ledger alone left two races: the run row is
-- inserted before the launcher mints the reviewer's credential (so a
-- header-less call from the worker's own shell in that window saw "no
-- credential" and could record the verdict), and the ledger read and the
-- verdict write were two statements.
--
-- reviewer_identity_expected is written in the SAME insert that creates the
-- run: 1 when the launcher that will start this run's reviewer hands every
-- reviewer its own credential (and refuses to launch one it cannot). A
-- non-agent verdict write is conditioned on it being 0 and on no unrevoked
-- reviewer credential existing, in one UPDATE, so neither window remains.
-- Rows written before this migration keep 0 and their previous behaviour.
-- ADD COLUMN only; no rebuild.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE review_run
    ADD COLUMN reviewer_identity_expected INTEGER NOT NULL DEFAULT 0
    CHECK (reviewer_identity_expected IN (0, 1));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE review_run DROP COLUMN reviewer_identity_expected;
-- +goose StatementEnd
