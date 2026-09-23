# Skill runs — durable, asynchronous, with history (Frente 2 / 2B)

*Status: **merged** (ECC `b0ecb59a2`). Migration **0172**. 2C (agent modes)
extends it without a migration — §7.*

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

- ~~Report validation against the manifest's declared output schema and
  storage-layer redaction~~ — **done in 2C for agent reports** (§7). The
  `static-code` report is still AO's own tool output with a fixed type that
  carries no matched text, and is not run through them.
- A run is not re-dispatched after a restart; `queued` at boot ends
  `SKILL_RUN_INTERRUPTED` like `running`. Rerunning is explicit.
- `approvalRevokedDuringRun` stays advisory: AO still does not kill a running
  container on revocation (the existing revocation policy).
- History diffing and export.
- The workflow engine does not consume runs (D3: runs live outside it).

## 7. Agent modes (Frente 2 / 2C)

*Status: **implemented** on `feat/skills-2c-agent-mode` (base `b0ecb59a2`),
pending review and merge. No migration. Design and residual risks: ADR 0010.*

A mode declares `executor: tool | agent` (absent = `tool`). The builtin
`security-audit` is **0.2.0**, with `authz-review` as its one agent mode;
`static-code` stays a tool mode in the Docker runner, unchanged.

**Flow.** Project → security-audit → `authz-review` → run (the same durable
run as 2B: states, CAS, ownership, idempotency, single-flight, reconcile,
report SHA-256) → AO stages a read-only copy → Claude Code reads `SKILL.md`,
the mode guide and the copy with Read/Grep/Glob only → structured findings →
AO verifies the copy and the source are unchanged → validates → completes →
redacts → validates → stores.

**Before acceptance** (each is a 4xx with a `run_refused` audit row and no run):

| Code | When |
| --- | --- |
| `SKILL_AGENT_UNAVAILABLE` (409) | No host agent executor, or it attests nothing (no CLI, a CLI missing a confinement flag, credentials that need a person). |
| `SKILL_AGENT_UNTRUSTED` (403) | The package is neither byte-for-byte the embedded builtin nor a signature-trusted, unrevoked install. |
| `SKILL_AGENT_SCHEMA_NOT_CANONICAL` (409) | The package's output schema is not AO's findings.v1. |
| `SKILL_RUN_REFUSED` (403) | Authorization denied (e.g. `repo.read` not granted, or a capability no host agent can carry). |
| `SKILL_AGENT_REQUIRES_DURABLE_RUN` (409) | The legacy synchronous path; agent modes are durable only. |

**After acceptance:**

| State / code | When |
| --- | --- |
| `refused` `SKILL_AGENT_STAGING_TAMPERED` | The staged copy changed during the run. |
| `refused` `SKILL_AGENT_SOURCE_CHANGED` | A source file changed between staging and the end of the run. |
| `refused` `SKILL_AGENT_UNAVAILABLE` / `SKILL_STAGING_UNUSABLE` | The executor or staging declined at launch. |
| `failed` `SKILL_AGENT_PROVIDER_FAILED` | The CLI exited non-zero, reported an error, or timed out. |
| `failed` `SKILL_RUN_OUTPUT_INVALID` | No structured output, a schema violation, or a cited path AO did not stage. Nothing is stored. |
| `failed` `SKILL_RUN_INTERRUPTED` / `SKILL_RUN_DAEMON_SHUTDOWN` | As in 2B. A dead daemon's agent is killed by its pid file (only if its cwd is that run's copy) and its copy removed. |

A run records `tool = ao.skill-agent/v1`, `runner_id = host-agent/claude-code`
and the four host-agent controls. The run detail serves the stored findings.v1
report as `report`; the CLI and the settings panel render it as findings, not as
container evidence.

**Configuration** (daemon env, all optional): `AO_SKILL_AGENT_BIN` (default
`claude`, resolved like the planner's), `AO_SKILL_AGENT_MODEL` (default
`sonnet`), `AO_SKILL_AGENT_TIMEOUT` (default `15m`), and
`AO_SKILL_AGENT_MAX_BUDGET_USD` (passed as `--max-budget-usd`; the cost hook —
AO sets no budget itself). Copies are staged under
`<dataDir>/skill-agent/.ao-skill-staging/<runId>` and removed when the run ends.

**Tests.**

- `internal/skillcatalog/agent_test.go` — executor contract, host-agent
  controls only carry `repo.read`/`report.write`, untrusted package refused,
  sets never mix, `MatchesBuiltin` rejects an edited manifest or extra/missing
  files.
- `internal/skillreport` — strict validator (duplicate keys, trailing data,
  unknown properties, enum/pattern/length/type/date-time) and unsupported
  keywords refused; redaction of shapes, harvested literals, prefixes, digests
  and encodings, over the whole document.
- `internal/skillagent` — a real subprocess stands in for the CLI: scrubbed
  env, read-only copy, deny list and agent-config exclusions, tamper (modified
  and created files), changed source, provider failures, timeout kills the
  process group, reap by pid file and never an unrelated process. Live, against
  the real CLI: `AO_SKILL_AGENT_LIVE=1 go test ./internal/skillagent/ -run Live`.
- `internal/service/skills/agentrun_test.go` — against real SQLite: success
  with AO-overwritten run block and coverage, redaction reaching neither the
  report nor the DB/WAL, invalid output (free text, schema violation, extra
  capability, unstaged path), claimed mode/target dropped, error
  classification, untrusted package and insufficient capability refused before
  acceptance, executors not interchangeable, `static-code` unchanged, cancel,
  shutdown, reconcile, dry run.
- **E2E** (`backend/e2e/skillruns/skillagent_e2e_test.go`), real daemon + real
  SQLite + real Claude Code, with planted prompt injection, a staged secret, a
  denied secret and an outside canary; restart and history; untrusted package;
  insufficient capability; a real provider failure; and, with a fake provider
  CLI through the real daemon, invalid output, tampering, a secret in the
  findings and a daemon SIGKILLed mid-run:

  ```
  AO_SKILL_AGENT_E2E=1 go test ./e2e/skillruns/ -run SkillAgent -v -count=1 -timeout 30m
  ```

  The daemon keeps the real HOME (Claude Code's credential is in the login
  keychain); every AO path is scratch.

**Debt (2C).** The agent runs on the host, not in the runner (ADR 0010 lists
what that does not claim); Codex is not a provider; only `authz-review` is an
agent mode; revocation of a trust root after install is not re-evaluated beyond
the origin row; the provider's token spend is recorded in the report's AO notes, not
in the usage ledger.
