# Product reliability UX — flow matrix and verified defects

Branch `feat/product-reliability-ux`, worktree `ao-product-reliability-ux`, based on
`feat/engineering-control-center` @ `970f2252e` (319 commits ahead of `main`, which it
strictly contains).

The goal of this phase is narrow and stated as a user outcome: **one simple Task,
start to finish, without the user diagnosing SQLite, hunting for IDs, opening a
recovery terminal, or learning the lifecycle vocabulary.**

## The finding that organises everything below

AO's daemon already computes almost every answer the user needs, in a closed,
versioned, durable vocabulary. The renderer then **drops it on the floor.**

Three concrete drops, all verified by reading the generated API schema against the
renderer's own call sites:

| Computed by the daemon, delivered on the wire | Rendered? |
| --- | --- |
| `WorkflowRunDetailView.advice` — the whole P3-C "what do I do now" contract: category, `requiresHuman`, `automaticAction` + why it is blocked, `expectedNextStage`, `reasonCode`, repair budget/spent/eligibility, `waitUntil`/`waitReason`, `availableActions`, **`blockedActions` with a reason for each** | **No. Not one field.** |
| `WorkflowStepView.reviewPolicy.facts` — `changedFilesUnprovable`, `unprovableChangeSetReason`, `changedFilesSource`, `changedFileCount`, `committedChangedFileCount` | **No.** Only `decision` and `reasons` are shown. |
| Everything needed for a diagnostic export (`run` + `presentation.technical` + `advice` + steps/attempts) | Shown on screen, **but nowhere exportable** — so the user retypes it, or goes to SQLite. |

This reframes the phase. The lifecycle is not the thing to fix here — it already has
regression coverage for **42 distinct real incidents** (`wf-00283521` … `wf-fe15c59d`).
What is broken is that the product refuses to *say* what it knows.

## Flow matrix — the ten user tasks

Legend: **OK** = verified working, do not re-fix · **GAP** = verified defect, still open ·
**FIXED-ELSEWHERE** = already integrated on this base, not to be repeated.

| # | User task | Success state | Error state | Recovery state | Verdict |
| --- | --- | --- | --- | --- | --- |
| 1 | Start a bounded Task on a project | A `workflow_run` exists with a frozen verify plan | Refused before creation | Re-open the form, values kept | **GAP — entry point.** The project-scoped offer is only "New session" (`POST /orchestrators/delegate` → a worker session, no `workflow_run`). Creating a Task means finding the global **Workflows** nav. Inside the session dialog the prompt field is literally labelled **"Task"**, which is also the name of a workflow strategy. |
| 2 | Task refused when it has no checks | Never reaches a worker | `VERIFICATION_REQUIRED` | Fix the plan in place | **FIXED-ELSEWHERE.** Form blocks on `verificationIsExecutable`, daemon refuses independently (`workflow_task_verification_test.go`). Regression of `wf-aee38f69`. |
| 3 | Worker commits; changed files recognised | Change set proven, review scoped | — | — | **FIXED-ELSEWHERE** (`review_committed_changes_test.go`). |
| 4 | Verify reuses valid evidence | Evidence reused, no re-run | — | — | **FIXED-ELSEWHERE** (`verify_recovery_test.go`, `evidence_snapshot.go`). |
| 5 | A file the worker wrote is git-ignored | AO says *why* it cannot prove the change set | Named cause | Act on the cause | **GAP — the explanation is dropped.** The daemon computes `changedFilesUnprovable` **and** `unprovableChangeSetReason` ("explains that, in terms a person can act on"). The renderer shows neither, so the run reads as "nothing happened". |
| 6 | Planner returns an inconsistent result | Rejected, retried durably | Reason recorded | Regenerate / reuse plan | **FIXED-ELSEWHERE** (`task_planner_view_test.go`, `p1b_recovery_internal_test.go`). |
| 7 | Restart / adoption keeps identity and ownership | Correct owner | — | — | **FIXED-ELSEWHERE** (`runtime_ownership_persistence_test.go`, `restart_recovery_test.go`). |
| 8 | Cancel / recover without losing commits or worktrees | Commits and worktree survive | — | Recovery panel offers only authorised ops | **OK.** `WorkflowRecoveryPanel` already renders only backend-authorised operations and hides refused ones by design. Reuse, do not rewrite. |
| 9 | Spanish selected → Spanish UI | All chrome in Spanish | — | — | **GAP.** `es.json` is complete (0 missing keys), but the **plan-approval controls are hardcoded English**: `"Generate Plan"`, `"Approve Plan"`, `"Cancel"` and their pending states. These are the single most decision-critical buttons in a Master/Autonomous run. `newTask.taskRequired` is also untranslated in `es`. |
| 10 | Costs and tokens shown without invented amounts | Real figures with provenance | "unknown", never `$0.00` | — | **OK — exemplary.** `costText` refuses to print an unknown cost as zero; `source` and `cost.basis` carry provenance. Do not touch. |

## Why the localization gap survived a guardrail

`src/renderer/i18n/renderer-coverage.test.ts` exists precisely to fail on new hardcoded
English. It walks `ts.isJsxText` (plain text children) and JSX **attributes** — but never
a `JsxExpression` used as a **child**. So `{busy ? "Approving…" : "Approve Plan"}` is
invisible to it.

Measured: extending the visitor to JSX expression children surfaces 81 literals, of which
**all but 9 are inside `components/chat/*`, already on the file's own deferred list**. The
9 remaining are the 6 real strings above plus 3 technical data fallbacks (`"planner"`,
`"default"`, `"claude-code"`). The gap is cheap to close and it is the reason this class
of defect recurs.

## Selected for implementation on this branch (Phase E)

Three problems, plus one small one, **all renderer-only**. None touches the lifecycle,
the daemon, SQLite, or any branch another agent is stabilising.

- **P0-1 — Render the advice AO already sends.** Especially `blockedActions` with their
  reasons ("why is this greyed out" must be answerable) and what AO will do by itself.
- **P0-2 — An exportable, secret-free diagnostic bundle.** Run ID, project, strategy,
  stage, step, attempt, reason code, error class, generations, decisions, evidence
  references, available and blocked actions. Composed from data already on the run
  detail. This is the item that removes "go read SQLite".
- **P1-3 — Show the change-set facts.** Never let a run read as "no work happened" when
  the daemon has recorded why it cannot prove otherwise.
- **P1-4 — Localize the plan-approval controls and close the guardrail gap** that let
  them through.

Deliberately **not** attempted here: rebuilding the creation flow into a single "Nuevo
trabajo" wizard (task 1). It is real and it is the user's headline complaint, but it is a
navigation and information-architecture change across the sidebar, the board, the shell
and two routes, and doing it in the same branch as the reliability fixes would make both
unreviewable. It is specified as a follow-up contract at the end of this document.

---

# Delivered

Four commits on `feat/product-reliability-ux`, **all renderer-only — zero files under
`backend/` changed**, so nothing here can collide with the lifecycle stabilisation or the
Skills work happening in other worktrees.

| Commit | What it closes |
| --- | --- |
| `5d8a8dc57` | The plan-approval controls are localized, and the guardrail that should have caught them is fixed. |
| `5f427f521` | The `advice` block is rendered — including every refused action **with its reason**. |
| `45c3fbe72` | The change-set facts are rendered, so an unprovable change set stops reading as "nothing happened". |
| `d57e69b57` | One-click, secret-free diagnostic export. |

## Verified results

- `npm run typecheck` — clean.
- `npm test` (full renderer suite) — **244 files, 2851 passed, 1 skipped, 0 failed.**
- `npm run build` — succeeds.
- Backend untouched, so backend suites were not re-run and are not claimed.

A fresh worktree needs three installs before the suite is honest — root, `frontend/`, and
`packages/product-ui/` — plus `frontend/node_modules/electron/path.txt`, which npm's
blocked postinstall never writes. Without them 9 files fail for missing dependencies and
none of it is about the code. That is the environment trap recorded in the working notes,
now with the exact remedy.

## What the surfaces say, in both locales

Rendered from the real components with fixture data:

```
EN  This needs a decision from you            ES  Necesita una decisión tuya
    Next            Verifying                     Sigue            Verificando
    Automatic repair  Available (2 of 3 used)     Reparación automática  Disponible (2 de 3 usadas)
    Not available right now                       No disponible ahora mismo
      Repair automatically — AO is about           Reparar automáticamente — AO va a
        to repair this itself                        repararlo por su cuenta
      Continue — this stop cannot be resumed       Continuar — esta parada no se puede reanudar

EN  AO could not establish what this work     ES  AO no pudo establecer qué cambió
      changed                                        este trabajo
EN  3 files changed · 2 of 3 came from        ES  3 archivos cambiados · 2 de 3 vienen
      the branch history                             del historial de la rama
EN  Copy diagnostics                          ES  Copiar diagnóstico
```

And the export itself, with the redactions visible — the fixture deliberately contained
`/Users/joaquinmora/.ao/worktrees/...` and a fake `sk-live-SECRET` inside the objective
body, and neither reaches the output:

```
# AO run diagnostics
run: wf-1234abcd            project: medusa         strategy: task
title: Fix the flaky checkout test          ← first line only; the body did not travel
## Stop
attentionReason: verify_ambiguous           errorClass: verify_ambiguous
## Advice
category: human_action                      expectedNextStage: verifying
availableActions: cancel
blocked:repair: automatic_repair_pending    ← the answer to "why is this greyed out"
repairBudget: 2 of 3 used                   adviceVersion: v1
## Execution
attempt: att-9f2c (#2)   provider: claude-code   session: ses-77   authority: active
placement: isolated_worktree   executionBranch: ao/wf-1234abcd   worktree: wf-1234abcd
                                                      ↑ basename only; no home directory
## Step 2 — review (failed)
reviewReasons: change_set_unprovable
changeSet: unprovable
unprovableReason: the work step recorded no base commit, so AO cannot tell committed
                  work from work that was never done
attempt 1: failed · verify_ambiguous · claude-code/opus · 2026-09-09T10:20:00.000Z
```

## Residual risks — stated, not hidden

1. **Two sentences stay English in every locale.** `advice.explanation` and
   `unprovableChangeSetReason` are composed as prose *by the daemon*. Localizing them
   means giving each a code in the closed vocabulary and a key per locale — a backend
   change, deliberately not made here. Today they render verbatim, which is the same
   contract `presentation.technical.attentionDetail` has always had.
2. **No screenshot.** The Chrome extension was not connected in this session, so the UI
   evidence above is rendered-DOM text rather than an image. The components were rendered
   by the real code with the real catalogs; only the picture is missing.
3. **The advice panel is only on the run detail route.** The board card and the Decisions
   view still show the older projection. Not a regression — they never showed advice — but
   it means "why can't I press this" is answerable in one place, not everywhere.
4. **Verified against fixtures, not a live run.** No workflow was launched against a real
   project, per the phase rules. The field mapping is checked against the generated
   OpenAPI schema, which is CI-enforced against the Go source, but no end-to-end run
   exercised these panels.
5. **The creation flow is untouched.** The headline complaint — that "Task" can mean two
   different things — is not fixed by this branch. See below.

## Round 2 — the creation flow and the last English sentences

Three more commits close the criteria the first round left open.

| Commit | What it closes |
| --- | --- |
| `f63a9d875` | The two creation surfaces are named for the object each makes, the project preselect actually works, and a contract test pins each surface to its endpoint. |
| `bd710947d` | The advice panel uses the daemon's stable code instead of its English fallback; the guardrail now tests itself. |

### The preselect was broken in the build almost everyone uses

The Board's "New workflow run" button already navigated to `/workflows?projectId=…`, and
the form already read `?projectId=`. It read it off `window.location.search` — and in
desktop mode the router runs on **hash history** (`router.tsx`), where the whole location
lives after the `#` and `window.location.search` is empty. So the preselect silently did
nothing in the Electron app, and creating a run from a project still started by re-picking
that project.

Fixed by reading it through the router's own `validateSearch`, which is history-agnostic.
This introduces the first `validateSearch` in the codebase; it is the TanStack-native way
and it is what makes the same code correct under both histories.

### The naming collision, concretely

The prompt field inside "Delegate a worker" was labelled **"Task"** — the same word that
names a workflow strategy. It is now "Instructions", and the dialog's description, which
was `sr-only` and therefore invisible to anyone not using a screen reader, is now visible
and says which object this makes and which surface makes the other:

```
[en] TITLE: Delegate a worker            [es] TITLE: Delegar un worker
[en] FIELD: Instructions                 [es] FIELD: Instrucciones
[en] DESC : This starts an interactive   [es] DESC : Esto inicia un worker interactivo
     worker you drive yourself. It does        que tú diriges. No crea una ejecución de
     not create a workflow run — use            workflow: usa «Nueva ejecución de
     "New workflow run" for a Task,             workflow» para un run Task, Autónoma o
     Autonomous or Master run with checks.      Master con checks.
```

### The contract test asserts endpoints, not navigation

The original failure was not a broken button — both surfaces worked. It was that a surface
labelled **Task** created a *session*. A navigation test would not have caught that, so
`-creation-surface-contract.test.tsx` mocks `apiClient`, lets the real hooks and real
validation run, and asserts the endpoint actually reached:

- "New workflow run" → `POST /api/v1/projects/{projectId}/workflows`, and **never**
  `/orchestrators/delegate`
- "New session" → `POST /api/v1/orchestrators/delegate`, and **never** the workflows path,
  with no `strategy` and no `verification` in the body

11 tests, also covering: a Task with no executable check POSTs *nothing at all*; 401 and
403 refusals are shown and the typed objective survives them; a refused delegate never
reports a session that does not exist; and strategy, placement, approval, repair and
review depth all travel in the create body, where the daemon freezes them.

### One real bug found while writing those tests

The form did `void createRun(...).then(...)` with no `.catch`. The refusal *was* shown
(`createError` renders), but the rejection `mutateAsync` re-throws surfaced as an
unhandled promise rejection. Now caught — and a daemon refusal no longer clears the form
the user has to correct.

### Localization

`workflow.Advice` documents `Summary`/`Explanation` as AO's own English and a **fallback**
for a client with no localized copy of `SummaryCode`. The advice panel was rendering the
fallback as the source, leaving an English sentence in every other locale for a code the
renderer already had 34 translations of. It now prefers `wf.summary.<summaryCode>` and
falls back only when there is no copy.

The second sentence, `unprovableChangeSetReason`, genuinely needs a new code in
`backend/internal/workflow/` — the package the lifecycle front is stabilising. Not
touched. Specified in `docs/contracts/unprovable-change-set-code.md`: the seven causes
already distinguishable in `review_changed_files.go`, the five-step change, the API
regeneration, and its acceptance criteria.

The guardrail now tests itself. Detection moved into `findHardcodedLiterals`, and six
cases assert which shapes must not pass — starting with the JSX-child ternary that escaped
it — and which legitimately do. A gate that silently stops detecting is worse than no gate.

### Verified, with exit codes

| Command | Exit | Result |
| --- | --- | --- |
| `npm run typecheck` | **0** | clean |
| `npm test` | **0** | 245 files, 2871 passed, 1 skipped, 0 failed |
| `npm run build` | **0** | built |

Backend still untouched across the whole branch.

### Criteria, closed

1. **Met.** A Task is created from the Board or the sidebar project menu, and the contract
   test proves a `workflow_run` POST is what happens.
2. **Met.** The two surfaces have different names, the field no longer says "Task", and
   the dialog states its consequence before the click.
3. **Met** (round 1) — on the run detail route.
4. **Met** (round 1).
5. **Met** (round 1).
6. **Met**, except the one daemon-composed sentence that has a written contract and no
   implementation.
7. **Not this branch's** — the lifecycle front's, already covered by its regression suite.

### Residual risks

- Still no screenshot: the Chrome extension was not connected. Evidence is rendered-DOM
  text from the real components.
- Still fixtures, not a live run. The contract test exercises the real form, real hooks and
  real validation against a mocked `apiClient`; it does not prove the daemon accepts the
  body. The field names are checked against the CI-enforced generated schema.
- The advice panel remains only on the run detail route.
- `validateSearch` is new to this codebase. It is scoped to `/workflows` and covered, but
  it is a pattern the next router change should be aware of.

## The follow-up contract: one "Nuevo trabajo" entry

Deliberately deferred, because it is a navigation and information-architecture change
across the sidebar, the board, the shell and two routes, and mixing it into the
reliability fixes would have made both unreviewable.

**The problem, precisely.** From a project, the only offer is "New session", which POSTs
`/orchestrators/delegate` and produces a worker session with **no `workflow_run`**. The
dialog is titled "Delegate a worker" but its prompt field is labelled **"Task"** — the
same word that names a workflow strategy. Creating an actual Task workflow requires
knowing to leave the project, open the global **Workflows** nav, and use a different form.
Two different things are called Task, and the more discoverable one is not the one that
creates a run.

**The contract for the fix.**

- One entry point, `Nuevo trabajo`, reachable from the project *and* globally, that asks
  in order: project → kind → objective → optional advanced → start.
- The kind choice states its consequence in the same breath: **Task / Autonomous /
  Master create a `workflow_run`**; **Interactive session opens a worker you drive
  yourself and creates no run.** Free sessions are not removed — they stop being the
  default a user lands in by accident.
- For Task, the existing verify editor moves into that flow unchanged. It already blocks
  on `verificationIsExecutable` and the daemon refuses independently with
  `VERIFICATION_REQUIRED`; neither should be re-implemented.
- Presets only where a project's real configuration backs them. No command inferred from
  the objective's prose — that rule already governs `lib/task-verification.ts` and must
  survive the move.
- The API needs nothing new. `POST /projects/{projectId}/workflows` already takes
  strategy, approval, repair, placement, review depth, criteria and the verification plan
  in one create call.

## Acceptance criteria — when AO is ready for a trial project again

Not "the unit tests pass". These are the observable behaviours:

1. A Task can be created, from the surface a user actually reaches first, and a
   `workflow_run` provably exists afterwards.
2. At no point does a control named Task create a session, or vice versa, without the
   consequence being stated before the click.
3. A run that stops shows: what AO is doing, what it will do by itself, what it needs
   from the user, and — for anything it refuses — why. **All four without a terminal.**
4. A run whose work is unprovable says so and says why, and is never presented as a run
   where nothing happened.
5. The full diagnostic state of any run can be exported in one action, with no secret,
   home directory, or pasted specification body in it.
6. With Spanish selected, every control in the create→run→decide path is in Spanish.
7. A completed Task is completed because Verify said so on real evidence — never because
   a worker's prose said it was done.

Criteria 3, 4, 5 and 6 are met by this branch on the run detail route. **1 and 2 are
not**, and they are the follow-up above. 7 is the lifecycle's, already covered by the
other front's regression suite.
