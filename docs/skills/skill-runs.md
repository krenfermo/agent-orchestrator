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

*Status: **merged** (ECC `b9ec8ca92`). No migration. Design and residual
risks: ADR 0010.*

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

## 8. Deterministic scanners (Frente 2 / 2D)

*Status: **merged** (ECC `45554edc7`). No migration. `security-audit` 0.3.0.*

Two more **tool** modes, on the SAME engine as `static-code` — not a second
framework. A tool is a row in `skillrunner/scantools.go`: which files it
stages, which files it reads, its closed rule set, an optional AO-authored
section, and its limitations. Every row runs the same AO-authored POSIX-shell
script in the same digest-approved alpine image with `--network none`,
non-root, read-only rootfs, `--cap-drop ALL`, cgroup limits, the boundary
evidence first and the coverage accounting last. Each mode needs its own
administrative image approval for its exact scope **and tool**; a static-code
approval authorizes nothing else.

| Mode | Tool | Capabilities | What it reports |
| --- | --- | --- | --- |
| `static-code` | `ao.static-scan/v1` | repo.read, report.write | unchanged |
| `secret-scan` | `ao.secret-scan/v1` | repo.read, report.write | credential SHAPES (SEC-001..013: cloud, forge, chat, payment and AI-provider keys, private key blocks, JWTs, URL credentials, credential-named literals, rc-file auth tokens, webhooks) by rule and location; SEC-100 for a manifest-denied file that is present |
| `dependencies` | `ao.dependency-scan/v1` | repo.read, deps.read, report.write | an inventory of declared dependencies (npm, Go, pip, Cargo) and DEP-001 git/URL source, DEP-002 unpinned version, DEP-003 no lockfile, DEP-004 plain-HTTP registry |

**Secrets never become report content.** The engine prints a rule id and
`path:line`, never the match. Files the manifest denies (`.env`, `.env.*`,
`*.pem`, `*.key`, `id_rsa*`) are never staged, so no tool can read them; the
secret scan reports their PRESENCE (SEC-100, by path, computed on the host).
The dependency inventory strips URL userinfo inside the container and AO
redacts known credential shapes again before storing.

**No invented vulnerabilities.** `dependencies` is offline: 0.3.0 drops
`net.egress` from the mode (and the unused `api.osv.dev` allowlist entry), so it
runs under confinement alone. Its findings are facts read off a manifest
(`confirmed`), none is a vulnerability claim, and its limitations say that no
advisory data was consulted. Recognised manifests it does not parse (Gemfile,
pyproject.toml, pom.xml, ...) are listed as unparsed. It stages only manifests
and lockfiles — never source.

**Behaviour change for `static-code`:** the manifest deny list now applies to its
staging too (it was declared but not enforced on the container path); a denied
file appears in coverage as `denied-by-manifest`.

**Other 2D candidates reviewed.** `api-infra-review` is a review mode (an
agent's judgement), not a deterministic scan, and stays unimplemented;
`active-pentest` is out of scope. Nothing else in the package is a 2D scanner.

**Tests.** `skillrunner/scantools_live_test.go` runs both tools in the real
container (busybox grep/awk): every planted shape found, no value in the report,
denied files reported and not read, exactly the planted dependency facts, a
token in a git URL absent, a clean locked project with zero findings.
`service/skills/scanmodes_test.go`: each mode reaches the runner with its tool,
the deny list and its own defaults; a static-code approval does not authorize
secret-scan. **E2E** `e2e/skillruns/scanners_e2e_test.go` (real daemon, SQLite,
Docker): both scanners on a positive and a clean project, static-code
regression, refused without an image approval, refused without `deps.read`,
restart with history and verified reports, no container or staging left, and
no planted value in the database, its WAL or the daemon log:

```
AO_SKILL_RUN_E2E=1 AO_SKILL_RUN_E2E_SHARED_ROOT=<a path the runtime shares> \
  go test ./e2e/skillruns/ -run 'Scanners|SkillRun' -v -count=1 -timeout 20m
```

**Debt (2D).** Advisory lookup needs data (a local advisory DB AO would have to
ship and update, or `net.egress`, still refused); only the working tree is
scanned, not git history; the secret scan is shape-based; package.json is read
line by line (minified manifests are not inventoried); the UI shows findings
but not the dependency inventory (the CLI summarises it).

## 9. The full security audit (Frente 2 / 2E)

*Status: **implemented** on `feat/skills-2e-security-audit` (base
`45554edc7`), pending review and merge. Migration **0173**. `security-audit`
0.4.0. Design: ADR 0011.*

`POST .../skills/security-audit/run` with `modeId: full-audit` accepts an
**audit**: a parent run (`tool = ao.security-audit/v1`, `runnerId = composite`)
that runs `secret-scan`, `dependencies`, `static-code` and `authz-review` as
**child runs** (`parentRunId` set), in that order, each on its own boundary --
Docker for the tools (each with its own image approval), the host agent for
`authz-review` (builtin/trusted only). Each child keeps its own report and
digest; the parent stores the consolidated `ao.security-audit/v1` report.

| Parent outcome | When |
| --- | --- |
| `succeeded` | every composed mode produced a verified report |
| `partial` (`SKILL_AUDIT_PARTIAL`) | some did and some did not; the report and the run say which, and why |
| `failed` `SKILL_AUDIT_NO_VERIFIED_RESULT` | none did (no report) |
| `cancelled` | a person cancelled the audit; the running child ends cancelled, no later child starts |
| `failed` `SKILL_RUN_INTERRUPTED` / `SKILL_RUN_DAEMON_SHUTDOWN` | as 2B, for the audit and its running child |

Refused before acceptance (4xx, no run): the grant lacks a capability of the
union (`SKILL_RUN_REFUSED`), or no composed mode could run now
(`SKILL_AUDIT_NOTHING_RUNNABLE`). A mode that cannot run at its launch
(`refused_before_start`: e.g. no provider, an untrusted package) or whose mode
already had a run in flight (`busy`) has no child run and makes the audit partial.

**Surfaces.** Run detail adds `children`; summaries add `parentRunId`; `state`
adds `partial`. `ao skills run security-audit --mode full-audit` prints the
consolidated report (completeness and gaps first) and exits non-zero for a
partial audit; `ao skills runs --project P <audit-id> --export audit.json` writes
the stored bytes after checking their SHA-256. The project settings panel shows
completeness first, each mode with its coverage and a link to its own report,
the consolidated findings with their sources, limitations, and the export
command; while an audit runs it lists its children.

**Tests.** `skillcatalog/composite_test.go` (composite contract, no widening,
parent authorization without controls); `migrate_skill_run_audit_test.go` (0173
invariants, findings preserved, Down); the rebuild inventory exercises 0173
against real child rows; `service/skills/audit_test.go` (complete, partial on a
missing provider / unapproved image / invalid agent output, no verified result,
refused before acceptance, cancel, shutdown, busy mode not adopted, idempotency,
reconcile, secrets kept out of the consolidated report, composite dry run);
CLI and UI tests. **E2E** `e2e/skillruns/audit_e2e_test.go` with a real daemon,
SQLite, Docker and Claude Code:

```
AO_SKILL_AUDIT_E2E=1 AO_SKILL_RUN_E2E_SHARED_ROOT=<a path the runtime shares> \
  go test ./e2e/skillruns/ -run SecurityAudit -v -count=1 -timeout 45m
```

**Debt (2E).** Children run sequentially (one boundary at a time; a parallel
audit would need a capacity decision); a cancelled audit keeps no consolidated
report (its children keep theirs); deduplication is deliberately conservative
(same category, file and line); usage is what each mode reports (the agent's
tokens and cost, the tools' wall clock) and is not yet in the usage ledger
(Frente 4); the audit covers the working tree only.
