# P8 — Control Center UX

Status: implemented, independently reviewed and visually validated on real data; integrated into `feat/engineering-control-center`. Section 8 supersedes any earlier detail it contradicts.
Base: `feat/engineering-control-center@eeeeb7253`.

P8 makes AO operable and legible before P9. It is a presentation layer over the
durable workflow model. It changes no lifecycle, no policy, and no P7 behaviour.

## 1. The UI before P8

| Concern | Where it lived | Problem |
|---|---|---|
| Start work | 6 entry points. Board "New session" (accent), board "New workflow run", sidebar menu ×2, ⌘N, palette "New session" | "New session" / `shell.newTask` delegates a **session**. "Task" is also a workflow **strategy**. The accent button was the session one. The palette had no workflow entry. |
| Mode | Strategy radios were third on the form. The detail page showed a text line. | "Autonomous" meant three things: the strategy, the approval axis (`executionMode`) and a global setting. |
| Run status | The detail page rendered it five times: completed banner, `WorkflowStatusPanel` (stage), raw `state` line, strategy/mode/derived-label line, `WorkflowActivityPanel` (phase). | Four vocabularies (`state`, `phase`, `stage`, derived label) side by side. |
| Liveness | Its own panel | Separate from the status, with no "who is working". |
| needs_attention | Status summary + advice panel + recovery panel + technical disclosure | No "what AO does not know" section. It read like a generic error. |
| Workflow vs session | Only the work step linked a session | No structure view. Verify looked like an agent step. |
| Review | Step card | The raw verdict string was shown. A missing reviewer rendered as **"claude-code"**, which was an invented default. No fix-cycle budget was shown. |
| Verify | Step card, `WorkflowVerifyDetails` | No summary. Nothing said whether it caused a fix cycle. |
| Timeline | Full list at the bottom | No compact progression. |
| Cost / context | `WorkflowUsageSection` + token ledger, at the bottom | Complete but deep. No one-glance measured/modelled/unknown view. |
| Diagnostics | `WorkflowDiagnosticsButton` (allowlist) | No usage, liveness, fix budget or build version. |

## 2. What P8 changed

### Visual / renderer only
- **Nuevo trabajo**. The workflow form is the one place work starts.
  - The mode (Task / Autonomous / Master) comes first, as three cards with a short, accurate explainer.
  - The summary before creation names the mode, approval, repair, placement, review depth, memory, verification, project, repo and branch. It also says the agent/model is "assigned by routing when each step launches".
  - "New work" is now the accent button on the board. The palette gains "New work", a navigation with the project preselected.
  - "New agent session" stays. Its dialog still says it creates no workflow.
- **Control center header** (`WorkflowControlCenter`). It replaces the status panel, liveness panel, raw-state line, strategy line and activity panel. It shows:
  - the headline status, mode chip, approval chip, review depth chip, and a compaction chip only when the run explicitly enabled compaction;
  - the daemon's summary and guidance sentence;
  - current step, who is working (role · harness / model · session link), last agent signal, when the agent's state last changed, last recorded event, duration and fix cycle N / max;
  - the compact durable timeline;
  - a technical line with the raw `state · phase · stage`.
- **needs_attention block**, with four sections:
  - **What happened.** The localized summary, then the daemon's sentence, then the generic copy.
  - **What AO knows.** Reason, error class, affected step, session, attempt, last signal, last checkpoint, branch, worktree, authority and repair eligibility, each only when present.
  - **What AO could not determine.** "AO cannot determine the cause" when the reason is missing or is one of the daemon's own ambiguous / unprovable / unreadable / unclassified codes. Also a missing session, no observed agent, unproven authority ("No se pudo confirmar que la sesión activa siga perteneciendo a este workflow.") and an unclassifiable repair condition.
  - **Recommended action.** The daemon's `recommendedAction`, plus AO's own automatic action or the reason it refuses one.
- **Incident advisor (minimal, deterministic).** Each rule reads one or two durable fields, and a terminal run raises nothing:
  - quiet worker (≥ 600 s, the threshold now shared with the liveness panel);
  - unproven authority;
  - fix budget exhausted;
  - Verify failed;
  - a provider-shaped attempt failure;
  - usage budget warning or exhausted;
  - a human question waiting.
- **Run anatomy** (`WorkflowRunAnatomy`): the workflow → sessions tree, a Review card and a Verify card kept apart, and a usage digest with measured / modelled / unknown chips.
- Step cards show a localized role ("Worker", "Reviewer"…) with the raw kind beside it, and a localized verdict. A missing reviewer reads "Not recorded", not "claude-code".
- Spanish copy aligned with the requested vocabulary (Ejecutando, Esperando al agente, Esperando tu respuesta, Necesita atención…).

### Backend / API contract
- `WorkflowRunView.maxFixCycles` (`*int`, omitempty). This is the frozen `WorkflowPolicy.MaxFixCycles` read from the run's policy snapshot, using the same pattern as `reviewDepth` and `contextEconomy`. It is nil when the snapshot is unreadable or carries no positive budget, so it is never the package default.
- The OpenAPI spec and `schema.ts` were regenerated. There is no new route, no query change and no migration.

### Diagnostics export
The existing allowlisted "Copy diagnostics" now also carries:
- fix cycles N of max, and the review and Verify outcomes;
- the failed-check **count** (never labels, since a check label is a command whose args may carry a secret);
- the liveness clocks;
- token totals and their source;
- cost known / amount / basis / unpriced models, and the budget state;
- the desktop app version.

It still carries no prompts, conversation, tool arguments, env or objective body.

## 3. State mapping (presentation over the durable model)

Evaluated in order. The durable run state is never replaced.

| Durable fact | Headline (es / en) |
|---|---|
| `run.state` = completed / cancelled / failed | Completado / Cancelado / Falló |
| `run.state` = needs_attention | Necesita atención / Needs you |
| any question `state = human_required` | Esperando tu respuesta / Waiting for your answer |
| `run.state` = pending | Preparando / Preparing |
| `stage` = preparing, planning, reviewing, correcting, verifying, integrating, waiting | same stage |
| `stage` = working, and the current work/fix step is `ready` or `waiting` with no liveness observed on that step | Esperando al agente / Waiting for the agent |
| `stage` = working, any other case | Ejecutando / Working |
| terminal `stage` on a non-terminal row | Ejecutando (the row wins) |

Other mappings:

- **Review:** `reviewPolicy.decision = skipped` → skipped. `verdict` approved / changes_requested → itself. A step that is running or waiting → in progress. A step that is pending or ready → pending. Anything else → failed / unknown.
- **Fix cycle N:** the highest `fixDelivery.cycleNumber` on the fix step(s), which comes from the same `fix_dispatched` ledger the budget is enforced against. It is 0 with no fix step, and unknown when a fix step ran without a delivery record.
- **Fix cycle max:** `run.maxFixCycles`.
- **Verify:** a recorded `verification.passed` → passed / failed. No result yet: pending / ready → pending, running / waiting → running, failed → failed, anything else → unknown.
- **"Caused a fix cycle":** only when a fix delivery has `findingsSource = verification`, or `attentionReason = verify_fix_reentry`.
- **Timeline:** the daemon's durable `presentation.timeline`, folded. Consecutive launches of one phase merge with a count, a verdict attaches to its review, and provider failures are counted on the phase they interrupted. Nothing is added.
- **Tokens:** `provider_reported` → measured. `estimated` → modelled. Anything else, or not recorded → unknown.
- **Cost:** `provider_reported`, or `calculated` from measured tokens → measured. `calculated` from estimated tokens → modelled. Not known → unknown, with no amount printed.
- **Context:** current / peak / growth only from an observable trajectory (measured). AO's assembled-context estimate is always modelled.

## 4. Lifecycle and safety
- **No UI action added by P8 changes lifecycle.**
  - New controls are navigations only: "New work" on the board and in the palette, and the session links.
  - All lifecycle buttons remain the daemon-authorised `WorkflowActions`, the existing plan approval pair, cancel-and-archive, and the recovery panel. None were added or re-gated.
- **No recovery is promised.** The attention block points at the authorised buttons and names AO's own automatic action. It never offers one of its own.
- **P7:** no compaction control, no enforcement, no savings display. The compaction chip appears only when a person already turned it on for that run.

## 5. Performance
- No new request, poll or timer.
- The header is computed from the run detail the page already polls every 2 s. The master-plan status label hook is unchanged.
- Removing `WorkflowActivityPanel` and `WorkflowLivenessPanel` from the page removed no fetches, because neither fetched.
- No transcript or log reads. Verify shows up to 5 check lines with first-line reasons; the full tails stay in the step card.

## 6. Debts moved to P9 (discovered, not fixed)

**Frontend presentation and localization**
1. **`usage.metrics.fixCycles` counts fix-step attempts, not budget cycles.** This is the wf-724a1e97 class: a re-delivered cycle counts twice. P8 no longer uses it, but `WorkflowUsageSection` still labels it "Fix cycles".
2. **Sessions do not link back to their workflow.** There is no API from session to workflow run, so the session page cannot say "this session belongs to workflow X".
3. **Six locales** (fr, de, ja, ko, zh-CN, pt-BR) carry English copy for the new `cc.*` keys.

**Ownership and recovery**
4. **Ownership is not provable over HTTP beyond `presentation.technical.authority`.** There is no lease or InstanceID, so "is the process alive and still ours" stays unprovable. The advisor says so instead of guessing.

**API coverage gaps**
5. **Workflow rows emit no CDC events.** Pages keep polling at 2 s. This is pre-existing.
6. **P7 shadow economics has no HTTP readback** (CLI only), so it is not shown.
7. **The daemon has no version or build endpoint.** Diagnostics can only state the desktop app version.
8. **The DTO `errorClass` enum is out of sync with the domain.** It is missing `invalid_placement`, `integration_failed` and `superseded`, and carries `provider_*` values the domain list does not.
9. **The Verify budget maximum is not projected.** Only the `verify_budget_exhausted` reason is.
10. **Hard usage budget:** the create API has no field for it.

## 7. Tests

**Backend**
- `TestMaxFixCyclesForRun*`: frozen value, and nil for an empty, `{}`, unparseable or zero-budget snapshot.
- `TestWorkflowRunViewCarriesTheFrozenFixCycleBudget`: on the wire.

**Frontend: mapping** (`lib/workflow-control-center.test.ts`)
- headline status
- session tree
- review, and fix cycles (attempts ≠ cycles)
- Verify, including triggered fix
- attention report, known and unknown
- incident rules, including the no-false-positive cases
- compact timeline
- measured / modelled / unknown
- duration

**Frontend: stories** (`components/workflow-control-center.test.tsx`)
- **B:** running
- **C:** needs_attention, in Spanish, with cause undetermined and ownership unproven
- **D:** completed flow
- **E:** workflow vs session
- usage labels

**Frontend: updated tests**
- detail page, now on the control center facts
- board and contract tests, for the "New work" name
- diagnostics bundle: new fields, and no check commands or stderr

## 8. Independent integration review (supersedes earlier details above)

An independent adversarial review (fresh context) and a read-only visual
validation of the real desktop app against the real `~/.ao/data` found defects
the fixtures did not. All were fixed in the feature (`fix(ui,p8)` commit),
presentation/copy only.

**Semantics as integrated**

- **Fix cycle N** is the *current* cycle: the newest fix delivery's
  `cycleNumber`. It is not the enforced "spent" count (the budget folds distinct
  `fix_dispatched` checkpoints; a delivery being recorded/retried already carries
  its cycle's number). Diagnostics field: `fixCycle: N of max`.
  `usage.metrics.fixCycles` counts fix-step attempts and is now labelled
  "Fix attempts / Intentos de fix" in the usage section.
- **Verify**: `waiting` behind a verification-sourced fix (or
  `verify_fix_reentry`) → **handed_back** ("Devuelto a fix"); only then "this
  failure was handed back to a fix cycle". A later passed/failed result never
  inherits an earlier cycle's cause. A plain `waiting` verify is pending;
  `completed` without a readable result is **completed_unrecorded**, never
  "unknown" next to a "verified" banner.
- **Current step / who is working**: a `needs_attention` run has no current
  step; "who is working" requires the step's durable state to be `running`.
  "Waiting for the agent" still requires a ready/waiting work|fix step AND no
  observed liveness on that same step.
- **Agent clocks**: liveness exists only for a running step whose row carries a
  session. A fix cycle delivered into the worker's existing session has none, so
  the copy says "agent clocks not available for this step" (never "no running
  agent"), and "not applicable" when no step is in progress.
- **Session tree**: no model is shown for fix (the attempt stores its cycle key)
  or verify (a fingerprint); verify shows no harness.
- **needs_attention**: fix-step stops take the session from `fixDelivery`; a
  Verify stop lists no missing session/clock as unknown; the header no longer
  repeats the stop sentence (the attention block states it once);
  `review_dispatch_ambiguous` is undetermined.
- **Advisor**: no "0% of budget" when the daemon sends no percent; no "0 failed
  checks" when no check result is recorded; timeline counts "failed attempts"
  (the daemon emits `provider_failed` for any failed attempt, including verify).
- **Usage**: `ao_counted` / `mixed` tokens are shown as modelled; a known cost
  with unpriced models is marked partial; context with unplaceable calls is a
  lower bound.
- **New work**: Autonomous/Master/agent copy aligned with the backend (same
  planner; Master's tasks run as Task workflows; the agent is chosen when each
  agent step starts). "Work" is the primary board button.
- **Detail heading**: the objective's first line; the full specification is
  folded below it.

**Visual validation (real data, read-only)**

Dev Electron app against `~/.ao/data`, no workflow created, no prompt, no agent.
Screens: New work form, project board, completed run `wf-66f0ee54`,
needs_attention `wf-32518ccf` (verify_unrepairable) and `wf-43e93bbf`
(dispatch_failed). 0 console errors, 0 non-GET API calls, 0 horizontal
overflow after fixes; idle traffic = the pre-existing run-detail (2 s),
recovery (5 s) and workspace polls.

Daemon startup (pre-existing behaviour, not P8 code) wrote to the real DB while
it ran; workflow runs, states, wakes, sessions and migrations were unchanged and
`integrity_check` is ok. Writes observed: 1 observational checkpoint
`review_reviewer_unproven` on an already-completed run (`wf-0aadfcde`, state and
`updated_at` unchanged), code-graph and project-memory re-index of
`agent-orchestrator`, one model-catalog refresh, one `ao.daemon.started`
telemetry event, and their `change_log` rows. A pre-launch snapshot is at
`~/.ao/backups/pre-p8-visual-20260913-191437/`.

**Additional debts for P9 (discovered in review)**

11. Worker liveness does not cover fix cycles delivered into an existing session
    (the fix step row has no session), so a silent fix agent cannot be flagged.
12. Boot reconciliation probes reviewers of completed runs and appends an
    observational checkpoint to them.
13. Pre-existing untranslated surfaces seen on real data: daemon attention
    sentences with no `wf.summary.*` copy (e.g. `verify_unrepairable`,
    `dispatch_failed`) render in English and as raw codes in the workflow list;
    the step routing summary shows English labels ("Current agent", "Reason");
    `shell.workflowUsage.routingReason.user_preferred_provider` is a raw key;
    the recovery panel repeats the stop sentence.
14. The six non-es/en locales still word `shell.workflowUsage.highUsageWarning`
    as "cycles".
