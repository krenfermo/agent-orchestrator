# 11. The full security audit: a parent run of child runs

Date: 2026-09-23
Status: Accepted (Frente 2 / 2E). Builds on ADR 0004 (container runner), ADR
0010 (host agent for builtin/trusted packages) and the 2B durable run.

## Context

After 2D, `security-audit` could run four modes on demand, each on its own
boundary: `secret-scan`, `dependencies` and `static-code` as closed tools in the
container runner, and `authz-review` as Claude Code on the host for a builtin or
trusted package. A person asking "audit this project" had to start four runs and
read four reports. 2E adds one audit that coordinates them and produces one
verifiable report -- without a second execution system, without mixing
attestations, and without turning a partial scan into a completed audit.

## Decision

### 1. A composite mode in the manifest

`ao.skill/v1` gains `executor: composite` with a `composes` list. The builtin
`security-audit` 0.4.0 declares `full-audit`, composing `secret-scan`,
`dependencies`, `static-code`, `authz-review` in that order. The validator
refuses a composite that could widen anything: its capabilities must be EXACTLY
the union of the composed modes' (`repo.read`, `deps.read`, `report.write`),
its risk and approval at least the strictest among them, no unknown, duplicate or
nested composite, and `composes` on nothing but a composite.

### 2. A parent run with child runs -- not a composition inside one run

Considered: one SkillRun that runs the four modes internally. Rejected, because
it would put four executors, two kinds of attestation, three image approvals and
four reports behind one row, and every 2B/2C/2D guarantee would have to be
re-proven for a new code path.

Chosen: the audit is a **parent** run (tool `ao.security-audit/v1`, runner
`composite`, no controls -- it executes nothing) and each composed mode is a
**child** run created with `parent_run_id` and executed by the unchanged 2B
path: `prepareRun` (activation, inputs, trust, authorization against its own
executor's attestation), then the container runner or the host agent, then its
own report and digest. Children run sequentially, in manifest order.

- **Authorization.** The parent is authorized as a composite parent: grants,
  permissions and approval for the union, no controls (`CompositeParent`, which
  only a composite mode can take). Every child is authorized again, fully, when
  it launches. Nothing is granted on the audit's behalf.
- **Acceptance.** The parent is refused before acceptance when the grant is
  missing, or when no composed mode could run now (`SKILL_AUDIT_NOTHING_RUNNABLE`).
- **Isolation between children.** A child's request is built from the parent's
  request alone. No child's output and no repository content can change which
  mode runs next, with what inputs or permissions.
- **Single flight.** A mode that already has a run in flight on the project is
  not adopted (that run is not the audit's authorization); it is recorded as
  `busy` and the audit is partial.
- **Cancellation.** Cancelling the audit cancels the running child as a cancel
  and starts no further child. Cancelling a child alone leaves the audit
  running; that mode is then not verified.
- **Recovery.** A dead daemon's audit and its running child both end
  `SKILL_RUN_INTERRUPTED`; the child is reaped by its own tool's reaper. Nothing
  resumes on its own.
- **Idempotency.** As 2B, on the parent.

### 3. `partial` is a terminal state of its own (migration 0173)

A report was only allowed on `succeeded` (0172's CHECK). A partial audit would
therefore have had to be `succeeded` -- announcing a partial scan as a completed
audit -- or `failed` without its report. 0173 rebuilds `skill_runs` (declared,
pragma-off, `skill_run_findings` preserved) to add:

- `partial`: terminal, carries a report and its digest, always an error code
  (`SKILL_AUDIT_PARTIAL`), never the same as `succeeded`;
- `parent_run_id`, indexed.

Outcomes: `succeeded` only when every composed mode produced a verified report;
`partial` when at least one did and one did not; `failed`
(`SKILL_AUDIT_NO_VERIFIED_RESULT`) when none did; `cancelled`; `failed` on
shutdown or interruption. Down turns `partial` into `failed` (0172 cannot hold
its report) and drops the column.

### 4. The consolidated report (`ao.security-audit/v1`)

Built from children read back from the store; a child's findings are used only
when its stored report still hashes to its recorded digest. It carries:

- `completeness` (`complete` | `partial`) and a factual summary -- sentences
  built from counts and findings, what was not covered FIRST;
- per mode: status, whether verified, its run id and report digest, its
  coverage (from its own report), its limitations and its usage (provider-reported
  tokens and cost for the agent, wall clock for tools -- nothing estimated);
- findings with their sources (mode, run, rule, severity). Two findings are
  merged only when they are the same category at the same file and line; the
  stricter severity and confidence win and both sources are kept. File-level
  findings are never merged;
- limitations: every mode that did not produce a verified report is named as
  NOT performed, and none claims the absence of a vulnerability.

It is validated against its embedded schema with the strict validator, redacted,
validated again, and stored with its SHA-256 like every other report. The CLI
exports it byte for byte after checking that digest; the children's own reports
stay available at their own run ids.

## Consequences

- No second execution system: the audit adds orchestration and consolidation
  only. Every boundary, approval and attestation is the one its mode already had.
- A partial audit is visible as partial in the API (state and report), the CLI
  (banner and non-zero exit) and the UI.
- Usage is recorded where a mode reports it; Frente 4 can read it from the
  report's `modes[].usage` without a schema change.
- Existing activations are untouched: projects pinned to 0.3.0 keep 0.3.0 and
  do not see `full-audit` until a person re-enables them at 0.4.0.
