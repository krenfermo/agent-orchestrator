# P0-D — final reliability validation and GO/NO-GO

P0-A and P0-B built the invariants. P0-C proved they hold against a real tmux
binary, a real raw-mode terminal and genuinely generated SQL. P0-D asks the
only question left: **can this be left running unattended for 24 hours on
macOS**, and is there a defensible way to merge the branch that says so.

This document is the evidence. Everything in it was run on this machine on the
`feat/engineering-control-center` branch; anything that was NOT run is called
out as not run rather than implied away.

## Verdict

**GO for unattended 24+ hour operation on macOS**, with the wall-clock caveat
in section J and the explicitly-not-run item in section F.

Windows/conpty remains NOT production-ready and is reported separately; it does
not gate the macOS answer.

---

## A. The actual CI merge gate

`.github/workflows/go.yml` is the gate. Four jobs: `build-test`
(`gofmt -l`, `go build`, `go vet`, `go test -race ./...`), `lint`
(golangci-lint v2.12.2 on the full ruleset), `api-drift` (regenerate the spec,
fail on `schema.ts` drift) and `sqlc-drift` (regenerate, fail on any diff under
`internal/storage/sqlite/gen`).

The lint job carries an explicit policy in its own comment: *"Blocking on the
full ruleset: the tree is clean at zero findings, so any new issue fails CI
rather than being grandfathered."*

### The premise this section started from was wrong

P0-D was briefed with "~298 findings of historical lint debt". That is not what
they were. Running the exact CI command against the merge-base:

| tree | findings |
| --- | --- |
| `main` (8732c4559) | **0** |
| `feat/engineering-control-center` at P0-D start | **296** |

`main` lints clean. Every one of the 296 was introduced by this branch. The
classification P0-D asked for therefore collapses to a single bucket:

| class | count |
| --- | --- |
| A — truly blocking current CI | 296 |
| B — grandfathered / baselined | 0 (no such mechanism exists) |
| C — introduced by this branch's work | 296 |
| D — unrelated historical debt | **0** |

### The decision

**Option 1 (fix them).** Option 2 was rejected on the facts: a "new issues
only" baseline established here would grandfather precisely the findings the
policy exists to catch, and would have hidden — among others — the four real
defects listed below. There is no baseline file, no new exclusion in
`.golangci.yml`, and no blanket disable.

The branch now lints at **0 issues**, matching `main`.

### Real defects found while clearing it

Lint was the trigger, not the point. Four were genuine and are fixed:

1. `store.UpdateWorkflowRunState`'s doc comment had been orphaned above
   `UpdateWorkflowRunPolicySnapshot` by a method inserted between comment and
   function — one method carried the other's contract, and the CAS method had
   none.
2. `TestAdd_AllowedRoots_RejectsSymlinkEscape` intended to skip on CI and its
   guard body was empty, so the symlink case ran on exactly the runners its own
   comment says cannot create symlinks.
3. `auth_ownership_test.go` guarded an assertion behind
   `if strings.Contains(...) == false {}` — an empty block that asserted
   nothing.
4. Dropping `selectIncidentDiagnosticProvider`'s always-nil error left its call
   site's `if err != nil` in place, where it would have read an unrelated outer
   `err`. Caught because the build still compiled.

Three vestigial bound constants (`attemptReapSettleWindow`,
`maxFreshReviewsPerRecovery`, `workAdoptionStabilityWindow`) were each a
second, silent statement of a bound enforced elsewhere. Each was verified to be
genuinely enforced at its real site before removal, and the rationale moved
there. That shape — two copies of one rule — is how a real drift starts.

Two genuine duplications were unified rather than suppressed:
`claimIncidentLaunch`/`claimIncidentRepair` now share one single-flight outbox
claim, and `pendingBranchAdvancedFreshReview`/`pendingProvenanceFreshReview`
now share one self-closing ledger read — the read that stops a restart, a poll
or a repeated Continue producing a second fresh review.

### Suppressions

Every suppression is per-site and carries the reason it is correct. The
substantive ones:

- **nine `nilerr` sites** in `internal/workflow`, all returning "not proven"
  rather than propagating an observation failure. That is the fail-closed
  direction recovery depends on: an unreadable workspace is not an unchanged
  one, and an unanswered probe is not proof of absence.
- **tmux's unreadable `pane_pid`** stays UNKNOWN liveness rather than an error —
  the semantics P0-C proved against real tmux.
- **`errorlint` on `failUnownableCreate`** is refused: `destroyErr` is
  legitimately nil on that branch, and `%w` would put `%!w(<nil>)` into the
  message an operator reads while chasing an orphaned session.
- **`gosec` G124 on the session cookies**: HttpOnly and SameSite are always
  set, and `Secure` tracks the real scheme. The loopback desktop daemon serves
  plain http, where an unconditionally-Secure cookie would never be stored at
  all.
- **`gosec` G404 in the wake scheduler**: retry jitter, not a secret.
- **`gosec` G202 in the questions store**: the concatenation is generated `?`
  placeholders only; every value is bound.

---

## B. End-to-end lifecycle

Both lanes exercised, objective → planning → plan persistence → work → review →
fix → fresh review → verify → completed:

- **read-only**: `TestReadOnlyTaskWithUnchangedWorktreeCompletesInsteadOfAmbiguous`,
  `TestReadOnlyTaskPreservingAKnownDirtyBaselineCompletes`,
  `TestReadOnlyCompletionConvergesAcrossARestart`, and the negative
  `TestReadOnlyTaskThatMutatesTheWorktreeNeedsAttention` — a read-only task that
  DOES mutate parks rather than passing.
- **mutating**: `TestMutatingTaskWithRealChangeStillCompletes`, plus the 1000
  lifecycles in section J.
- **writeIntent is conservative by default**:
  `TestUndeclaredTaskWithNoChangeIsStillAmbiguous` — an undeclared task with no
  change stays ambiguous, so the read-only path can only be reached by an
  explicit durable declaration.

Review target, approved HEAD, fix generation and verify authority are covered
by the `review_authority_*`, `approved_head_provenance_*`, `fix_generation` and
`verify_authority_*` suites; parent/child convergence in section H.

---

## C. Crash / restart matrix

All 20 boundaries have named, passing coverage. Most of it predates P0-D (it is
what P0-A and P0-B built); P0-D's contribution was to verify the mapping is
complete rather than assumed.

| # | boundary | evidence |
| --- | --- | --- |
| 1 | after objective creation, before plan row | `TestBootRecoveryHealsAnObjectiveRunThatHasNoPlanRow` |
| 2 | planner in flight | `TestPlannerRecoveryRunningIsAmbiguousButRespondedFinalizes` |
| 3 | after plan response, before persistence | same |
| 4 | approval transitions | `TestApprovePlanConvergesAWriteSetACrashLeftHalfDone`, `TestCP11AndCP12ValidatedAutonomousPlanIsApprovedAfterRestart` |
| 5 | after work dispatch intent | `TestRecoveryBoundaryA_PendingOutboxNeverSpawnedDispatchesOnce` |
| 6 | after launch, before confirmation | `TestRecoveryBoundaryB_DispatchedOutboxAdoptsFoundSessionWithoutRespawning` |
| 7 | after confirmation, before attempt open | `TestCrashAfterAckBeforeAttemptOpenConverges` |
| 8 | worker running | `TestRecoveryBoundaryE_ActiveSessionStaysRunningAcrossRestart`, `TestRestartDuringAnActiveWorkerAdoptsWithoutDuplicating` |
| 9 | immediately after worker commit | `TestRecoveryBoundaryF_TerminatedWithCommitEvidenceCompletesAcrossRestart` |
| 10 | after completion, before attempt closure | `TestRecoveryBoundaryCD_StaleDispatchAfterFullBookkeepingIsNoOp`, `TestRestartBetweenTheReapRecordAndTheRowResumes` |
| 11 | before review dispatch confirmation | `TestReviewRecoveryBoundaryA/B`, `TestReviewCase11_LaunchedThenCrashedBeforeConfirmationIsSweptExactlyOnce` |
| 12 | reviewer running | `TestReviewRecovery_LiveReviewerIsNeverReplacedWhileItWorks` |
| 13 | after changes_requested, before fix dispatch | `TestDaemonRestartBetweenVerdictAndFixDispatchDeliversTheSameDurableFindings` |
| 14 | after fix claim, before Send | `TestCrashAfterClaimBeforeSendDeliversOnceUnderTheSameClaim` |
| 15 | after Send, before ack | `TestCrashAfterSendBeforeAckAdoptsTheSameGeneration` |
| 16 | after fix attempt open | `TestCrashAfterAttemptOpenBeforeDispatchRecordConverges` |
| 17 | after fix completion, before re-review | `TestFixLifecycleSurvivesRestartNoDuplicateDispatch` |
| 18 | after approval, before verify | `TestVerifyRecoverySurvivesRestartBeforeExecution` |
| 19 | during verify | `TestVerifyRecoverySurvivesRestartAfterExecution`, `TestVerifyRecoversPersistedSuccessWithoutRerun` |
| 20 | immediately before final completion | `TestReadOnlyCompletionConvergesAcrossARestart`, `TestDirectBranch_RestartBetweenVerificationAndPromotionStillConverges` |

None of these synchronise on sleeps: they drive a fake clock and assert on
durable rows.

---

## D. Repeated restart / ABA — `p0d_repeated_restart_test.go` (new)

The matrix above proves ONE restart at ONE boundary converges. That is not the
property an unattended daemon needs. What it does is restart repeatedly against
records that are not changing, and the failure mode hiding there is ABA: a pass
that is individually idempotent but leaves a trace each time, so the fifth
restart is not the first.

Five consecutive restarts, for each of the five states P0-D names, asserting
the whole sequence rather than the endpoint:

| state | observed across 5 restarts |
| --- | --- |
| worker running | spawns 1, ledger 3 rows, attempts 1 — all flat; session id unchanged |
| fix running | sends 1, spawns 1, ledger 17 rows, attempts 1 — all flat |
| review running | launches 1, review runs 1, ledger 13 rows — all flat; review run id unchanged |
| completed, not finalized | spawns 1, launches 1, sends 0, ledger 14 rows — all flat |
| stale runtime | spawns **0**, ledger 4 rows — flat; never converges on completed |

**The ledger does not grow.** A restart that learns nothing new writes nothing
at all.

**The assertion was verified capable of failing.** Injecting a simulated
one-row-per-restart leak fails all five tests and names the exact restart the
drift began on:

```
worker-running: restart 3 differs from restart 1
  restart 1: {spawns:1 launches:0 reviewRuns:0 sends:0 checkpoints:3 attempts:1}
  restart 3: {spawns:1 launches:0 reviewRuns:0 sends:0 checkpoints:4 attempts:1}
```

A test that cannot fail proves nothing — the same argument P0-C's unbracketed-
paste negative control rests on.

---

## E. Real tmux chaos

`tmux 3.7b`, the real binary. The whole adapter suite (60 tests, including the
`TestRealTmux_*` lifecycle and chaos set) run **10 consecutive times**: all
pass.

Covered by the existing P0-C suite and re-verified here: killing a worker pane,
destroying a session, recreating the same session NAME, a stale handle against a
new incarnation, missing/malformed/empty/negative `pane_pid`, an unrelated live
session, runtime recreation, duplicate reconciliation, and an old generation
attempting destroy or adoption.

Leak check after ten rounds:

- **live tmux servers: 1** — and that one is a pre-existing AO dev session 32
  hours old, not created by the tests.
- **the operator's default tmux server: untouched** (`no server running`).

Dead socket *files* do accumulate in `/private/tmp/tmux-501/` (one per test,
~277 after these runs). Those are inodes with no server behind them, and they
are the deliberate price of per-test server isolation. Production uses one
socket per data dir (`config.TmuxSocket`), not one per session, so this does not
translate into a runtime leak. Noted as test hygiene, not a defect.

---

## F. Real Claude TUI large-prompt test — **NOT RUN**

A real `claude` binary (2.1.251) IS installed on this machine, so this was a
genuine decision rather than an impossibility. It was not run, for one reason
that is not a technical limitation:

**it would spend the user's Anthropic quota and send a >4 KB prompt to an
external service on their account.** That is an outward-facing, billable action,
and it is theirs to authorise, not mine to assume.

The secondary reasons stand on their own: the TUI needs host credentials, its
output is not deterministic, and driving it to a submission is exactly the
flaky terminal timing P0-D says not to force.

What remains true, and is the automated guarantee:
`raw_paste_transport_integration_test.go` proves the >4 KB transport against
real tmux and a real raw-mode consumer (`term.MakeRaw` + DECSET 2004), with a
negative control that reproduces the original defect — 190 bare Enter presses
and zero markers — through the old unbracketed paste. That control passes,
which is what makes the positive result mean anything.

**This is classified as non-blocking operational evidence, not a hidden
failure.** Nothing in this report may be read as "real Claude E2E was run".

---

## G. Wake / retry soak

`internal/workflow/wake` and `internal/workflow/wakepoller` at `-count=20`, and
the workflow package's wake/retry/backoff/capacity tests at `-count=5`: all
pass.

Covered: transient failure → bounded backoff
(`TestScheduler_ScheduleBackoffGrowsOnRepeatedUnknownReset`), duplicate due wake
and repeated reparks → no churn (`TestScheduler_RepeatedReparksDoNotChurn`,
`TestScheduler_IdempotentUpsert`), restart with pending/claimed wake, completed
wake, and terminal states scheduling nothing
(`TestHumanRequiredQuestion_NeverSchedulesWake`,
`TestCancelRunCancelsOpenWakeSchedule`). Each capacity lane asserts *exactly
once* under a headless poller.

Deterministic unrecoverable states do not retry forever: they land on a named
fail-closed stop (section I).

---

## H. Parent / child / board projection

`Master|Child|Parent|Board` at `-count=5`: all pass. No backend projection
divergence was found, so nothing was changed here.

Specifically: child running → parent running; child completed → parent
progresses; child needs_attention → parent mirrors
(`TestRepeatedIdenticalChildAttentionIsDeduplicated`); child recovers → parent
clears the stale mirror (`TestDaemonRestartHealsAStaleParentMirror`,
`TestHumanContinueOnTheChildClearsTheParentMirror`,
`TestParentReconcilesStaleChildAttentionWhenTheChildResumes`); a terminal child
cannot appear working (`TestTerminalChildFailureIsNeverClearedAsARecovery`,
`TestChildCancellationDoesNotLeaveTheTaskRunning`); an unrelated child cannot
influence a parent (`TestLifecycle_ChildStopNamesTheExactChild`); and a
completed read-only child never parks its parent.

---

## I. Legacy data recovery

All five unprovable-record classes fail closed with a named reason, recover
only on proof, and are bounded. `Legacy|Unprov|FailClosed|Stranded` at
`-count=5`: all pass.

- missing attempt generation → `TestLegacyGenerationlessFixDeliveryRecoversSafely`
- missing fix generation → `TestUnprovableLegacyFixStateFailsClosed`,
  `TestFixGenerationUnprovableIsANamedActionableStop`
- missing approved-HEAD provenance → `TestAnUnprovableApprovedHeadStaysFailClosedAndSaysSo`,
  `TestNothingAutomaticRecoversAnUnprovableApprovedHead`, with bounded operator
  recovery (`TestOperatorRecoveryIsBounded`,
  `TestOperatorRecoveryRefusesWhenTheApprovedHeadIsProvable`)
- missing runtime ownership proof → `TestLegacy_UnprovableReviewerIsNeitherAdoptedNorDestroyed`,
  `TestStalePane_UnprovenProvenanceNeverAdoptsOrDestroys`
- missing planner launch identity → `TestLegacy_ConfirmationWithoutInstanceIsNotBindable`,
  `TestRecovery_ReviewRunWithNoLaunchRecordIsNeverBound`

No provenance is fabricated, no autonomous loop is unbounded
(`TestReviewSweep_UnprovenEvidenceIsBoundedByItsProbeBudget`), and the
deterministic legacy condition surfaces as a human-actionable stop rather than a
raw 500 (`TestRecoveryUnreconcilableIsAHumanActionableStop`,
`TestAlreadyStrandedRunExplainsItselfFromLegacyRows`).

---

## J. Deterministic soak — `p0d_soak_test.go` (new)

**1000 complete lifecycles** — 500 per lane — each with a daemon restart and
three reconciles folded in:

| lane | lifecycles | spawns | reviewer launches | review runs | fix prompts | ledger |
| --- | --- | --- | --- | --- | --- | --- |
| approved | 500 | 500 | 500 | 500 | 0 | 7000 (14/lifecycle) |
| fix cycle | 500 | 500 | 500 | 500 | 500 | 8500 (17/lifecycle) |

Exactly one of each side effect per lifecycle, asserted per iteration so drift
names the iteration it began on, and a **constant** per-lifecycle ledger. No
duplicate launches, no duplicate prompts, no stuck runs, no leaked wakes, no
ledger growth anomaly.

### What this soak is not

It is compressed and deterministic, on a fake clock, against fake runtimes. It
CAN find state that accumulates per iteration. It CANNOT find anything that is
a function of wall-clock time — a real memory leak, fd exhaustion, a tmux server
degrading over hours, a provider session expiring.

**It must not be called a 24-hour soak.**

### Wall-clock soak

A separate wall-clock soak ran the workflow lifecycle and real-tmux suites back
to back for **30 minutes** (22:19–22:49), tracking the live tmux server count
every round:

| rounds | failures | live tmux servers |
| --- | --- | --- |
| 70 | **0** | 1, constant (the pre-existing dev session; never grew) |

Thirty minutes is thirty minutes. It is reported as such and is not evidence
about hour 20 of an unattended day. What it does add over the deterministic
soak is real elapsed time against the real tmux binary: 70 consecutive rounds
of create/destroy/adopt against a live server with no accumulation and no
flake.

---

## K. sqlc final gate

`npm run sqlc` (sqlc v1.31.1) run twice.

- checked-in `gen/` already matched canonical SQL before the first run (no diff)
- **second run: zero diff**
- the `sqlc-drift` CI job's check (`git diff --exit-code -- backend/internal/storage/sqlite/gen`) passes

No unexplained drift.

---

## L. Full validation

| gate | result |
| --- | --- |
| `gofmt -l .` | clean |
| `go build ./...` | pass |
| `go vet ./...` | pass |
| golangci-lint v2.12.2, exact CI command | **0 issues** |
| `go test ./internal/workflow/... -count=1` | pass |
| `go test ./internal/workflow/... -race -count=1` | pass (295s) |
| `go test ./internal/session_manager/... -count=1` | pass |
| `go test ./internal/adapters/runtime/tmux/... -count=1` | pass |
| `go test ./internal/adapters/runtime/tmux/... -count=10` | pass |
| `go test ./internal/storage/... -count=1` | pass |
| `go test ./internal/daemon/... -count=1` | pass |
| `go test ./internal/review/... -count=1` | pass |
| `go test ./... -count=1` | pass |
| `go test ./... -race -count=1` (the CI job) | pass |
| `npm run sqlc` ×2 | zero diff |
| API drift (`schema.ts`) | no drift |
| `npm run frontend:typecheck` | pass |

---

## M. Windows / conpty — audit only

Re-audited, and the P0-C statement is confirmed still accurate:
`internal/adapters/runtime/conpty` implements the `Runtime` verbs but has **no
`SessionFacts`**, no `DestroyInstance`, no InstanceID/generation, no ownership
proof, and no restart-recovery evidence. The entire ABA closure the tmux adapter
is built on does not exist there.

**WINDOWS/CONPTY IS NOT PRODUCTION-READY** and is retained as written in
`docs/p0c-runtime-evidence.md` §6. Nothing was implemented for Windows in P0-D.

---

## N. GO / NO-GO against the stated rules

| # | rule | status |
| --- | --- | --- |
| 1 | no known macOS P0 lifecycle blocker | ✅ |
| 2 | worker generation/ownership recovery deterministic | ✅ |
| 3 | fix generation/ownership recovery deterministic | ✅ |
| 4 | reviewer generation/recovery correct | ✅ |
| 5 | review→verify authority exact | ✅ |
| 6 | stale writers/generations rejected | ✅ |
| 7 | crash/restart matrix converges | ✅ (§C, §D) |
| 8 | parent/child converges | ✅ (§H) |
| 9 | one bad run cannot abort unrelated runs | ✅ |
| 10 | deterministic failures do not retry forever | ✅ (§G, §I) |
| 11 | real tmux lifecycle tests pass | ✅ (§E, 10×) |
| 12 | large-prompt transport proof valid | ✅ (§F, with its control) |
| 13 | no unexplained sqlc drift | ✅ (§K) |
| 14 | branch has a defensible CI merge path | ✅ (§A, 0 findings) |
| 15 | P0 changes introduce no new lint failures | ✅ (§A) |
| 16 | legacy unprovable records fail closed | ✅ (§I) |
| 17 | no known infinite wake loop | ✅ (§G) |
| 18 | no known duplicate launch/prompt race | ✅ (§D, §J) |

All 18 hold. **P0-D: GO.**
