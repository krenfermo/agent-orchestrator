# Skill runs — durable, asynchronous, with history (Frente 2 / 2B)

*Status: **implemented** on `feat/skills-2b-run-persistence` (base `6ee90c567`),
pending review and merge. Migration **0172**.*

Before 2B, `POST /projects/{id}/skills/{skillId}/run` held the HTTP request open
for the whole container run and returned the report in the response. Nothing
kept it: a closed tab, a daemon restart or a second click lost the result, and
the only trace was a one-line `run_executed` row in `skill_audit`.

2B makes a run a durable record:
**Project → Skill → Mode → Run → progress → result/findings → history.**

What 2B deliberately does **not** change: the isolation boundary (ADR 0004),
the executable modes (only `static-code`), LLM execution (none), DAST, secrets,
egress, `process.exec`. The runner gained exactly one thing — an optional run id
used as a container label and a staging directory name — and no isolation flag.

## 1. Contract

**A run exists if and only if AO accepted it.** Every check that can be made
before anything is staged — activation, mode, declared inputs, a runner,
authorization against AO's own attestation, an implemented mode, a stageable
scope — happens first. A request that fails one gets a 4xx and a `run_refused`
audit row, as before, and **no run**: there was never anything to execute.

| From | To | When |
| --- | --- | --- |
| `queued` | `running` | the executor claimed it (compare-and-set; loses to a cancel) |
| `queued` | `cancelled` | a person cancelled it before it started |
| `queued` | `failed` | its daemon stopped before it started |
| `running` | `succeeded` | the scan produced a report; report, findings and digest are written in **one** transaction |
| `running` | `refused` | the execution boundary declined to launch or to trust the output |
| `running` | `failed` | no result AO can stand behind, or its daemon stopped while it ran |
| `running` | `cancelled` | a person cancelled it; the container was stopped and removed |

Terminal states have no outgoing transition. Every transition is a
compare-and-set on the state the writer believes the row is in, so two writers
can never both move one run.

**`refused` vs `failed`.** `refused` is the boundary saying *no*: image not
approved, approval revoked or expired, image not present or its digest
mismatched, no trust root, tool not approved, runtime unavailable, staging
unusable or invisible to the runtime, staged inputs not delivered, isolation not
demonstrated by the run's own evidence. Nothing an operator should retry without
changing something. `failed` is an attempt that produced nothing trustworthy:
runtime error, timeout, invalid output — or the daemon that owned the run
stopped.

**Ownership and restarts.** A run is executed only by the daemon instance that
accepted it (`owner_instance`). A daemon that boots and finds a non-terminal run
owned by another instance knows that owner is gone and **cannot know** what
happened after its last write — the scan may have finished and never been
recorded. It does not guess: it removes that run's container (by the
`ao.skillrun.id` label) and its staged copy of the source (by directory name),
touching nothing else, and ends the run `failed` with `SKILL_RUN_INTERRUPTED`,
naming the state it was last seen in. Rerunning is a person's decision.

A clean shutdown cancels in-flight executors; each records `failed`
`SKILL_RUN_DAEMON_SHUTDOWN` while the store is still open.

**Idempotency.** Both rules are also enforced by unique indexes:

- the same `idempotencyKey` on the same project returns the run it created,
  whatever state it is in now;
- while a run for (project, skill, mode) is queued or running, a second request
  returns **that** run (`created: false`) instead of a second container over the
  same checkout. A double click is harmless.

**Integrity.** A succeeded run stores the report as exact bytes with their
SHA-256. Reading a run re-hashes them: `integrity` is `verified`, `mismatch`
(the report is **not** served) or `none` (no report).

**Secrets.** A finding carries the rule and the location, never matched text
(`skillrunner.ScanFinding`). Inputs are the manifest's declared, validated
inputs. The E2E plants a literal password and asserts its value appears nowhere
in `ao.db` or its WAL.

## 2. Schema (0172)

- `skill_runs` — id, project (cascade), skill id/version, mode, tool, state,
  optional idempotency key, requester, inputs, **granted** capabilities, package
  digest, runner id and attested controls, owner instance, image digest and
  approval, summary, finding count, truncated, `report_json` + `report_sha256`,
  `error_code`/`error_message`, `cancel_requested`, created/started/finished/
  updated timestamps. No foreign key to `skill_installs`: a run is history.
- `skill_run_findings` — one row per finding, `(run_id, ordinal)` key, cascade.
- Invariants in the schema, not only the service: terminal ⇔ `finished_at`;
  `succeeded` ⇔ report and digest; `failed`/`refused` always carry a code; at
  most one in-flight run per (project, skill, mode); one run per idempotency key.

## 3. Surfaces

| Surface | What |
| --- | --- |
| `POST /api/v1/projects/{id}/skills/{skillId}/run` | **202** `{run, created}`. `project.manage`. Body: `modeId`, `inputs`, optional `idempotencyKey`. |
| `GET /api/v1/projects/{id}/skills/runs[?limit=]` | History, newest first, no reports. `project.read`. |
| `GET /api/v1/projects/{id}/skills/runs/{runId}` | Run + findings + verified report. Another project's run is 404. `project.read`. |
| `POST /api/v1/projects/{id}/skills/runs/{runId}/cancel` | Cancel a queued/running run; 409 when already ended. `project.manage`. |
| `ao skills run` | Starts, waits, prints the verified report; `--no-wait`, `--idempotency-key`, `--wait-timeout`. Project and skill are URL-escaped. |
| `ao skills runs --project P [run-id]` | History, or one run with its verified report. |
| Project settings → Skills | Run follows the run to its end, Cancel while in flight, run history (including refused/failed/cancelled/interrupted runs). |

`runs` is a reserved skill id: `/projects/{id}/skills/runs` is the history.

## 4. Builtin `security-audit`

Embedded in the binary and made **available** at every boot, idempotently. It is
**never enabled**: no project gains a capability because AO was upgraded. The
same version already installed with the same bytes is left alone and writes no
audit row; the same version installed with **different** bytes is never
overwritten — it is reported.

## 5. Tests

- Store/schema: `migrate_skill_runs_test.go` (the invariants above), migration
  ledger, FK guards.
- Service: `runs_test.go` against real SQLite — async success, idempotency,
  double click, cancel, refused vs failed (including a revoked approval),
  refusal-before-acceptance creates no run, project scoping, tampered report,
  reconcile after a dead owner, shutdown. `builtin_test.go`.
- Live container tests across packages are serialized by a host-wide lock
  (`internal/testsupport/dockerlock`), because they share one runtime.
- **E2E** (`backend/e2e/skillruns`), real daemon + real SQLite + real Docker:

  ```
  AO_SKILL_RUN_E2E=1 AO_SKILL_RUN_E2E_SHARED_ROOT=<a path the runtime shares> \
    go test ./e2e/skillruns/ -v -count=1 -timeout 20m
  ```

## 6. Not done here (debt, recorded)

- **Report validation against the manifest's declared output schema** and
  **storage-layer redaction** (Subfase 5 of `roadmap.md`). The `static-code`
  report is AO's own tool output with a fixed type that carries no matched
  text; an agent-produced report (2C) will need both before it is stored.
- A run is not re-dispatched after a restart; `queued` at boot ends
  `SKILL_RUN_INTERRUPTED` like `running`. Rerunning is explicit.
- `approvalRevokedDuringRun` stays advisory: AO still does not kill a running
  container on revocation (the existing revocation policy).
- History diffing and export.
- The workflow engine does not consume runs (D3: runs live outside it).
