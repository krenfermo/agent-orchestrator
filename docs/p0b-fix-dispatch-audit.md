# P0-B — fix-cycle dispatch audit and generation contract

**Status: IMPLEMENTED.** This is the fix-cycle counterpart to
[`p0b-worker-launch-audit.md`](p0b-worker-launch-audit.md). That document closed
the worker launch path: `attempt_generation` on the dispatch record, and every
launch/claim/confirm/ack/fail/release/reopen transition conditioned on it. This
one closes the last structurally generation-less path in P0-B — the fix cycle in
`workflow/fix_dispatch.go` — using the same identity model and the same
table-generic store primitives. All paths are relative to `backend/internal/`.

**Explicitly out of scope, and unchanged:** the reviewer's own lifecycle
(`review_dispatch.go`, `review_launch_recovery.go`, `review_authority.go`, and
migrations `0135`–`0138`). No reviewer generation or CAS was altered. No
canonical SQL changed either: every predicate below is an existing store method
that the reviewer and worker paths already use.

---

## 1. The path, and where it was open

    review changes_requested
      -> maybeDispatchFix / maybeDispatchVerifyFix   (cascade.go, verify.go)
      -> dispatchFixStep                             (fix_dispatch.go)
      -> outbox claim
      -> applyFixLifecycleDecision + prompt build
      -> recordFixDispatchIntent                     (fix_delivery_recovery.go)
      -> MessageSender.Send / SendReportingSubmission
      -> delivery acknowledgement                    (outbox -> acknowledged)
      -> fix attempt row opened
      -> observeFixStep                              (fix_progress.go)
      -> recordFixOutcome                            (attempt closed, fingerprint)
      -> dispatchReviewStep re-review / verify

Everything from the claim onward was arbitrated by `id + expected_status`, a
predicate **any** concurrent or post-restart pass satisfies:

| transition | predicate before | predicate now |
|---|---|---|
| claim (`pending -> dispatched`) | `UpdateWorkflowOutboxStatus`, which stamps no token and **clears** any that was there | `ClaimWorkflowOutboxDispatch(id, now, gen.ID)` |
| acknowledge (`-> acknowledged`) | `UpdateWorkflowOutboxStatus` | `AcknowledgeWorkflowOutboxDispatch(id, expected, now, gen.ID)` |
| transport retry / dispatch failure (`-> failed`) | `UpdateWorkflowOutboxStatus` | `FailWorkflowOutboxWithGeneration(id, expected, now, class, gen.ID, gen.ID)` |
| refusal (authority or generation stale) | left the row `dispatched`, owned by nobody | `ReleaseDispatchedWorkflowOutboxGeneration(id, "", gen.ID)` |

Two concrete failures followed from that, and both are the reason this block
exists:

- **Duplicate prompt.** Pass A claims, calls `Send`, and stalls before recording
  success. A reconciler or a restart re-derives the cycle; pass B enqueues under
  the same idempotency key, finds the row `dispatched` and — with nothing on it
  saying *whose* dispatch it is — completes it as its own. The receipt-based
  recovery in `fix_delivery_recovery.go` narrowed this, but its evidence is about
  the **session** ("did some prompt with this digest arrive"), never about which
  dispatch owns the claim; two dispatches of one cycle are byte-identical prompts
  and therefore indistinguishable to it.
- **Stale lifecycle mutation.** A cycle derived from review generation N could
  open an attempt and write the `fix_dispatched` fingerprint checkpoint that
  `dispatchReviewStep` later reads as its licence to open review N+1.
  `fixAuthorityRefusal` re-read the *verdict*, which catches an approved or
  rebound review, but nothing bound the in-flight dispatch to the exact review
  generation it came from — so a re-derivation across a restart could not tell
  "still the same authority" from "the same review run, re-reviewed since".

---

## 2. The identity

`fixDispatchGeneration` (`workflow/fix_generation.go`) is minted **before** the
claim, stamped onto the outbox row **by** the claim, and written into the durable
pre-delivery record **strictly before** `Send`. Either side reconstructs the
other.

| field | answers |
|---|---|
| `ID` | the claim token; empty means legacy |
| `WorkflowRunID`, `TaskID` | which workflow? which task? |
| `FixStepID` | which step? |
| `ReviewRunID`, `ReviewGeneration` | which review run, and which review generation authorized it (`sha256(id ‖ effective verdict ‖ target sha)`) |
| `CycleNumber`, `TransportAttempt`, `Redelivery` | which fix cycle, transport attempt and re-delivery |
| `SessionID` | which worker session |
| `FindingsDigest` | which findings payload |
| `FixAttemptID` | which attempt it opened |

Everything except `ID` and `FixAttemptID` is the **binding** — the answer to
"which logical dispatch is this?". Two generations with the same binding describe
the same intended delivery and may be adopted for one another across a restart;
two with different bindings never may. `FixAttemptID` is excluded because it does
not exist when the generation is minted; `ID` is excluded because a recovery
adopts the token it finds and must then prove that what the token names is what
it is about to complete.

The generation rides on the records that already existed
(`promptDeliveryRecord.Generation`), so `fixAuthority`, `fixDelivery`, the
findings digest, the prompt receipt, `receiptMatch` and the >4 KB tmux transport
are all **extended, not replaced**.

---

## 3. Delivery gates

Two, in this order, immediately before `Send`:

1. `fixAuthorityRefusal` — unchanged. *Does the current review authorize a fix
   cycle at all?* (superseded step binding, approval with no unanswered
   `verify_fix_reentry`, unreadable store).
2. `fixGenerationStaleRefusal` — new. *Is the cycle in hand still the cycle that
   review authorized?* Refuses on a target-session mismatch, a findings-digest
   mismatch, a review-run mismatch, a review generation that has been superseded,
   or a review run it cannot read. Default is refusal.

A superseded review passes the first and fails the second. Refusal is inert:
nothing is sent, nothing is recorded, and the claim is released so the dispatch
that *is* current can take the row.

---

## 4. Crash boundaries

Recovery adopts a generation; it never mints one for a delivery that already
happened. `resolveOwningFixGeneration` reads the row's claim token and every
pre-delivery record for this `(step, cycle, transport attempt)` and answers
`owned` / `legacyAdopted` / `unprovable`.

| # | crash point | durable state | resolution |
|---|---|---|---|
| A | intent persisted, before claim | no outbox row past `pending` | ordinary dispatch, one prompt |
| B | claim persisted, before `Send` | row claimed, **no** pre-delivery record | intent-absence proves `Send` never ran → deliver once **under the token already on the row** |
| C | `Send` ok, before ack | row claimed, record present | receipt/turn evidence proves delivery → adopt, complete bookkeeping, never re-send |
| D | ack persisted, before attempt open | row `acknowledged` (token cleared), record present | the record is the ownership proof → open the attempt, write the dispatch record |
| E | attempt open, before worker observation | attempt + dispatch record | ordinary observation |
| F | attempt open, before dispatch record | attempt, **no** `fix_dispatched` record | `fixCycleDispatchIncomplete` (intent yes, dispatched no) lets it fall through to recovery, which completes the bookkeeping with no second prompt and no second attempt |
| G | re-review transition starts, before completion | fingerprint checkpoint written | `dispatchReviewStep`'s own generation, unchanged |

Boundary F was previously invisible: the attempt-count guard called the cycle
dispatched, and `observeFixStep` had no checkpoint to observe against, so the run
sat still. The new guard is stated positively — *intent present, dispatch record
absent* — so a ledger that has lost its dispatch rows for some other reason is
never read as one AO may deliver into again.

---

## 5. Fail-closed, and legacy rows

`fix_generation_unprovable` (`attention.go`) is the named condition for durable
state that cannot be mapped onto exactly one dispatch:

- two different generations recorded against one cycle;
- a claim token that disagrees with the pre-delivery record it should match;
- a claim token over one or more generation-less deliveries of the same cycle;
- a generation-less delivery whose findings digest or review run is not the one
  this pass derived;
- a recorded generation whose binding is not the dispatch this pass derived
  (the superseded-review case, after a crash).

It is inert and terminal-for-automation: no send, no attempt, no state advance,
no wake, no retry. It is recorded **once** (`recordAttentionStopOnce`), because
nothing AO does by itself changes the ledger. It is deliberately distinct from
`fix_dispatch_ambiguous`, which is a question about the *session*; this one is a
question about the *ledger*, which no amount of looking at the worker answers.

**Legacy (generation-less) rows are never given a fabricated generation.** A
generation-less dispatch stays generation-less for the life of its outbox row and
is completed under the empty token the row actually holds — which is exactly what
`AcknowledgeWorkflowOutboxDispatch` requires for a row claimed before the column
existed, and what makes an invented token match nothing rather than match
anything. It recovers safely only when every generation-less record for the cycle
agrees with the other and with what this pass derived (findings digest, review
run); otherwise it fails closed as above.

---

## 6. What answers recovery's questions

All from durable rows, no timestamps and no sleeps:

| question | source |
|---|---|
| which workflow / task / fix step? | generation, on the pre-delivery and dispatch records |
| which review generation? | `generation.reviewGeneration`, compared against `reviewGenerationToken(current)` |
| which fix cycle / fix generation / attempt? | `generation.cycleNumber` / `generation.id` / `generation.fixAttemptId` |
| which worker session / findings payload? | `generation.sessionId` / `generation.findingsDigest` |
| what durable record authorizes `Send`? | the `fix_dispatch_intent` checkpoint, written fatally-before-`Send` |
| has this exact generation already been delivered? | the outbox row's status + claim token, plus `classifyFixDelivery`'s receipt/turn evidence |

The same facts are projected read-time by `FixDeliveryReport` and the API's
`fixDelivery` view (`generation`, `reviewGeneration`, `redelivery`).
