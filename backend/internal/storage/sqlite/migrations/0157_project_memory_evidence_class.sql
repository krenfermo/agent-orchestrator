-- P4-H: how strong the claim behind a durable fact is.
--
-- P2-D gave every memory item a provenance KIND (which proof applies) and an
-- AUTHORITY (whether that proof still holds). Both are about the licence. What
-- neither says is what the P4-H brief asks for in one line: never present an
-- inference as a confirmed fact.
--
-- Two rows can be repo derivations with intact authority and still be very
-- different claims. "AGENTS.md says worktrees live under ~/.ao" is a sentence
-- AO copied out of a file a reader can open. "Authorisation is decided in
-- these three packages" is AO's own reading of a directory census. Rendering
-- the second the way the first is rendered is how a plausible guess becomes a
-- premise nobody rechecks.
--
-- domain.MemoryEvidenceClass carries the vocabulary:
--
--   workflow_verified  a workflow produced it AND verification passed
--   user_provided      a person stated it
--   observed           AO read it and is repeating it
--   derived            AO concluded it from evidence it observed
--
-- STRICTLY ADDITIVE, and deliberately NOT backfilled. Every row written before
-- this migration reads back as '' — "this row does not say" — which is the
-- honest description of it. Assigning a class to rows whose producer never
-- made the distinction would fabricate exactly the confidence signal the
-- column exists to make trustworthy.
--
-- No CHECK allowlist, following the convention 0133 and 0146 set: Go owns
-- validity (domain.MemoryEvidenceClass.Valid), the vocabulary is still
-- growing, and MemoryEvidenceClass.Rank() already ranks an unrecognised value
-- BELOW every known one — so a class from a newer build loses arguments rather
-- than winning them by being unfamiliar.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE project_memory_items ADD COLUMN evidence_class TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE project_memory_items DROP COLUMN evidence_class;
-- +goose StatementEnd
