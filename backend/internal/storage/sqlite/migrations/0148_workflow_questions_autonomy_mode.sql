-- +goose Up
-- +goose StatementBegin
-- P3-C: record WHICH autonomy policy routed a question to the Decision
-- Resolver, so an answer AO decided for itself is durably distinguishable
-- from one a human gave and from one the discovery-shape heuristic routed.
--
-- Why a column rather than a re-read of classification_reason: the reason is
-- prose, and matching on prose to decide whether a decision was autonomous
-- would make the distinction depend on wording. It is also the field the
-- durable decision record has to carry (which policy was in force when AO
-- took this decision), and a decision that cannot name the policy that
-- authorized it is not explainable after the policy changes.
--
-- Empty string means "not routed by an autonomy policy", which is every row
-- written before P3-C and every question the classifier resolved on its own
-- fact-backed or discovery-shape grounds. The default therefore preserves the
-- exact pre-P3-C reading of every existing row.
ALTER TABLE workflow_questions ADD COLUMN autonomy_mode TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflow_questions DROP COLUMN autonomy_mode;
-- +goose StatementEnd
