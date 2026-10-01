-- AR-1a / D-SEC-3: an amendment's approver is the authenticated principal, not
-- a name the caller typed.
--
-- Migration 0132 made acceptance criteria amendable only "with a named human
-- approver", and stored that name in approved_by. But the name came from the
-- request body: any caller holding workflow.run could attribute an amendment to
-- anybody, and nothing durable said how the approver had been identified.
--
-- From AR-1a the daemon derives the approver from the request's principal and
-- records two facts beside the display name:
--
--   * approved_by_user_id: the users.id of the principal that approved it.
--   * approved_auth_method: HOW that principal was identified. 'trusted_local'
--     is recorded as such and never upgraded: on a desktop install a
--     header-less loopback call resolves the bootstrap owner, so an approval
--     made that way is weaker evidence than a password or OIDC login, and an
--     auditor must be able to tell them apart. An agent principal can never
--     approve, so 'agent' is not an allowed value at all.
--
-- Rows written before AR-1a keep '' in both columns: their approver is the
-- unverified name the caller supplied, and the data says so rather than
-- pretending otherwise. ADD COLUMN only -- no table rebuild, so no incoming
-- foreign key is disturbed.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE workflow_task_criterion_amendments
    ADD COLUMN approved_by_user_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_task_criterion_amendments
    ADD COLUMN approved_auth_method TEXT NOT NULL DEFAULT ''
    CHECK (approved_auth_method IN ('', 'password', 'oidc', 'trusted_local'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflow_task_criterion_amendments DROP COLUMN approved_auth_method;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_task_criterion_amendments DROP COLUMN approved_by_user_id;
-- +goose StatementEnd
