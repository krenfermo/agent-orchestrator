# P0-C — real-runtime evidence

What P0-C added is not another layer of invariants. P0-A and P0-B built those,
and their tests drive scripted runtimes and fake senders. P0-C is the evidence
that the invariants hold against the things production actually runs on: a real
`tmux` binary, a real pane, a real raw-mode terminal consumer, and generated
SQLite code that is genuinely a function of the checked-in SQL.

## 1. Real tmux lifecycle

`backend/internal/adapters/runtime/tmux/real_tmux_lifecycle_integration_test.go`.

Every test runs on its own isolated tmux server (`Options.Socket`), so it can
never see, adopt or kill anything on the operator's default server, and two of
them running at once cannot collide. They skip only when the host has no `tmux`
binary, and say so; the version under test is logged.

| Fact proven | Test |
| --- | --- |
| an AO-owned session is observable through `SessionFacts` — instance, ownership token, liveness — from the real server | `TestRealTmux_OwnedSessionIsObservableThroughSessionFacts` |
| a runtime rebuilt from nothing but the socket recovers the same incarnation; repeated reconciliation is idempotent and repeated recreation launches or adopts nothing extra | `TestRealTmux_RecreatedRuntimeRecoversTheSameOwnedSessionIdempotently` |
| a destroyed incarnation is proven absence, a session that later takes the same NAME is never adopted by the old handle, and a stale generation's destroy cannot reach the newer session | `TestRealTmux_StaleInstanceIsNeverAdoptedEvenUnderTheSameName` |
| a session AO did not create carries no ownership token and is observably unowned; an AO session owned by A is not adoptable by a caller expecting B | `TestRealTmux_WrongAndMissingOwnerAreRejected` |
| an empty, whitespace, non-numeric, negative, zero or float `pane_pid` is UNKNOWN liveness on a session that provably still exists — never an error, never death | `TestRealTmux_EmptyOrMalformedPanePIDIsUnknownLivenessNotAnError` |
| one broken real session does not affect an unrelated one | `TestRealTmux_OneBadSessionDoesNotAffectAnUnrelatedOne` |

`stale_pane_integration_test.go` and `tmux_integration_test.go` (server
isolation, restart recovery, supervised-exit keep-alive) predate this and still
carry their own real-tmux evidence.

## 2. >4 KB raw-mode prompt transport

`backend/internal/adapters/runtime/tmux/raw_paste_transport_integration_test.go`.

The production defect: `paste-buffer` without `-p`. tmux replaces every LF in a
paste buffer with CR, and a TUI reading input in raw mode sees CR as Enter — so
a 90-line fix prompt was submitted as ~90 separate messages and only the last
fragment survived. The reviewer's findings live in the middle of a fix prompt,
so it presented as "the fix worker ignored the review".

The consumer is a real process in a real pane whose tty is put into raw mode
with `term.MakeRaw` and which requests bracketed paste with DECSET 2004 — the
two things an agent TUI does and a cooked-mode shell does not. Assertions are
made on the raw byte stream it recorded.

**Limitation, stated plainly: this is AO's own minimal raw-mode harness, not the
Claude TUI.** Claude Code is not a deterministic, offline, scriptable process,
so launching it here would make the suite non-hermetic. What is proven is the
terminal semantics every such TUI depends on, not the behaviour of any
particular agent binary. Nothing in this file may be reported as "real Claude
E2E".

What makes that proof meaningful is the negative control:
`TestRealTmux_PasteWithoutBracketsShredsThePromptIntoSubmissions` runs the same
real tmux, the same real raw-mode consumer and the same >4 KB prompt through the
OLD unbracketed paste, and requires the defect to reproduce — 190 bare Enter
presses and zero markers. A test that cannot fail on the bug proves nothing
about the fix. If that control ever stops reproducing, the harness has stopped
modelling the terminal and the positive tests below are worthless.

Proven for both a >4 KB worker prompt and a >4 KB fix prompt: one logical paste
(exactly one open and one close marker), the payload byte-for-byte identical to
what was sent (sha256 compared, CR-in-brackets normalised as the documented
terminal reading), beginning/middle/end markers surviving in order, the reviewer
findings block in the middle surviving verbatim, and exactly ONE Enter outside
the brackets — the submit — against 188 line breaks inside them.

## 3. Fix generation on the real wire

`TestRealTmux_OneFixGenerationDeliversExactlyOnePasteAcrossARestart`.

The generation half of this is proven in `internal/workflow` (a fix generation is
claimed durably; a stale one cannot send; a crash after Send before ack adopts
the same delivery). Those tests drive a fake sender, so what they cannot show is
what reached the pane. This is the other half: the bytes one generation put on
the wire, and the absence of the bytes a second delivery would have put there
after a daemon restart.

The negative claim is made provable rather than waited for: tmux delivers to one
pane in order, so a small sentinel sent after the restart has reconciled is a
happens-before edge. Anything a duplicate delivery would have written must
already be in the recording by the time the sentinel appears.

## 4. Generated SQLite

`sqlc.yaml` at `backend/`, driven by `npm run sqlc`
(`go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`). Two drifts were
found, and both were hand edits to generated files:

1. `gen/workflow.sql.go` — four query functions had been appended by hand at the
   end of the file instead of regenerated, so they sat outside sqlc's canonical
   ordering and with hand-reflowed call sites. Content-identical; regeneration
   restores the canonical order.
2. `gen/workflow_plan.sql.go` — `PersistNormalizedWorkflowPlanParams` had been
   hand-renamed from what sqlc emits. The query used two positional `?` bindings
   against `generated_plan_json`, so sqlc names the second `GeneratedPlanJson_2`;
   somebody renamed it to `ExpectedPlanJson` in the generated file and the query
   doc comment was dropped at the same time.

   **Fixed at the source, not preserved.** The CAS in
   `queries/workflow_plan.sql` now binds `sqlc.arg(...)` names, so sqlc emits
   `ExpectedPlanJson` itself and the caller in
   `store/workflow_plan_store.go` is unchanged.

Generation is deterministic (two consecutive runs are byte-identical) and the
tree is clean afterwards. Nothing enforced this before, which is how the drift
accumulated: `.github/workflows/go.yml` now has a `sqlc-drift` job that
regenerates and fails on any diff under `gen/`, mirroring the existing
`api-drift` job.

## 5. Reviewer launch/recovery: the residual, and why it is safe

P0-B deferred "reset/reopen ABA beyond the failed-generation key". The audit
found every mutation in `review_launch_recovery.go` bound to its generation in
SQL:

| mutation | binding |
| --- | --- |
| claim | `ClaimWorkflowOutboxDispatch` stamps `dispatch_generation` |
| fail | `FailWorkflowOutboxWithGeneration` CASes on `dispatch_generation` |
| release | `ReleaseDispatchedWorkflowOutboxGeneration` CASes on `dispatch_generation` |
| reopen | `ReopenFailedWorkflowOutboxGeneration` CASes on `failure_generation` |
| reset epoch | checkpoint keyed `head_sha` = the generation, UNIQUE (migration 0136) |
| partial review_run close-out | `UpdateReviewRunResult` CASes on `status='running'` |

`clearReviewLaunchStop` is the one exception, and necessarily so: it un-parks a
RUN, and a run has no reviewer generation to CAS against. Its invariant is
therefore a scoping one — a reviewer launch succeeding must never unblock a
newer lifecycle generation — and
`backend/internal/workflow/review_launch_stop_scope_internal_test.go` proves it:
a foreign stop is left alone, a reviewer-launch stop that a newer unrelated stop
has superseded is left alone, and the mirror case (the newer stop resolved
first) still clears, so the behaviour is about ORDER and not about the mere
presence of a foreign reason.

## 6. WINDOWS/CONPTY NOT PRODUCTION-READY

Audit only; nothing was implemented here.

`backend/internal/adapters/runtime/conpty` implements `Create`, `Destroy`,
`IsAlive`, `IsSupervisedProcessAlive`, `IsExactSupervisedProcessAlive`,
`SendMessage`, `Interrupt`, `SendInput`, `GetOutput` and `Attach`. It does
**not** implement `ports.SessionFactsReader`. Concretely it has:

- **no `SessionFacts`** — no single coherent per-incarnation observation, so
  nothing on Windows can distinguish "my session is still here" from "something
  else has my session's name";
- **no `DestroyInstance`** — destructive actions cannot be addressed to one
  exact incarnation;
- **no InstanceID / generation** — the handle carries no immutable per-
  incarnation identity, which is the ABA closure the tmux adapter is built on;
- **no ownership proof** — nothing equivalent to `AO_SESSION_OWNER`;
- **no restart recovery evidence** — there is no real-conpty equivalent of the
  suite in section 1.

Until those exist, **WINDOWS/CONPTY IS NOT PRODUCTION-READY** for the
generation-safe worker/reviewer lifecycle, and must not be described as such.
