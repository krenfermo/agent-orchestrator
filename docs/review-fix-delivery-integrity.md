# Review → fix delivery integrity

How a reviewer's findings reach the fix worker, what AO can prove about that
delivery, and the incident that showed the proof was missing.

Companion to `docs/workflow-lifecycle-mapping.md`. Scope is deliberately narrow:
review→fix dispatch, persistence, prompt construction and observability. The
reviewer lifecycle itself is closed and is not described here.

## The durable path

Every hop is a durable read or write. No step depends on in-memory state, so a
daemon restart at any point resumes from the same facts.

| # | Where | What happens |
|---|-------|--------------|
| 1 | `review_run.verdict` / `.late_verdict` | The reviewer's verdict and body are written by the out-of-band `ao review submit` call, never by workflow. |
| 2 | `domain.ReviewRun.EffectiveVerdict()` / `EffectiveBody()` | The single accessor pair every consumer uses. It resolves the two-column storage split (normal vs. late verdict) and returns nothing at all for a run that has been superseded. |
| 3 | `workflow/cascade.go` `maybeDispatchFix` | Reads the review run named by `workflow_steps.review_run_id`, requires `EffectiveVerdict() == changes_requested`, re-derives the cycle number from the session's review-run count, and checks the fix budget. |
| 4 | `workflow/fix_prompt.go` `BuildFixPrompt` | Pure and deterministic. Interpolates `EffectiveBody()` **verbatim** between `---` fences. Nothing truncates, summarizes or reformats it. |
| 5 | `workflow/fix_dispatch.go` `dispatchFixStep` | Enqueues a `workflow_outbox` row under a cycle- and transport-attempt-specific idempotency key. Only the `pending` branch may deliver. |
| 6 | `fix_dispatch_intent` checkpoint | Written **strictly before** `Send`, fatal on failure. Carries the prompt receipt digest and (new) the findings evidence. Its absence is positive proof that `Send` was never reached. |
| 7 | `MessageSender.Send` → runtime | The bytes are handed to the transport. Above 4 KB they travel through a private file and a tmux paste buffer (`ports.PromptTransportBufferFile`). |
| 8 | `fix_dispatched` checkpoint + `workflow_attempts` row | The delivery record and the attempt it produced. |
| 9 | `workflow/fix_progress.go` `observeFixStep` | Judges the cycle from session facts and a live workspace fingerprint comparison — never from the agent's prose. |

The fix attempt is durably bound to workflow run, fix step, review run, review
verdict, review target SHA, worker session, fix cycle number, fix attempt ID and
the findings digest — all on the `fix_dispatch_intent` and `fix_dispatched`
checkpoints, and all surfaced by `GET /workflow-runs/{id}` as `steps[].fixDelivery`.

## Incident wf-a816d7fe (2026-08-27)

A reviewer returned `changes_requested` with two findings, one naming
`docs/worker-lifecycle-audit.md:142-167`. AO dispatched the fix. The worker went
idle without touching the worktree and AO stopped on `ambiguous_worker_state`.

### What the durable state proved

- `review_run.verdict = changes_requested`, `body` 1669 bytes, `late_verdict`
  empty, `superseded_by` empty. So `EffectiveBody()` returned the full findings.
- The `fix_dispatch_intent` record: `promptBytes: 4600`, `transport:
  buffer_file`, `contextPack: false`, `promptReceipt: dc4c9991…`.
- Rebuilding the prompt from the run's objective, the plan artifact's acceptance
  criteria, an empty effective spec, the review run ID and that exact 1669-byte
  body reproduces **4600 bytes whose SHA-256 is `dc4c9991…`** — byte-identical to
  the prompt AO recorded delivering, with the findings embedded verbatim.

So AO built the correct prompt. **Prompt construction was never the defect.**

### The actual defect

`sessions.latest_user_prompt` for the worker held 510 bytes — the closing
guardrails paragraph — with `\r` line separators. The agent's own transcript
shows the same 510 bytes as the message it received, and the agent replied:

> Your message arrived truncated (it starts mid-word), and I read it as a
> restatement of the guardrails rather than a new task.

Root cause: `tmux paste-buffer` **replaces every LF with CR by default**, and AO
was calling it without `-p`, so no bracketed-paste markers were emitted. A shell
pane never notices — the tty line discipline maps CR back to NL — but an agent
TUI reads its input in raw mode, where a bare CR is **Enter**. The 4600-byte,
~90-line prompt was therefore delivered as ~90 one-line submissions. Only the
final fragment survived in the composer. The reviewer's findings sit in the
middle of the prompt and never arrived.

Verified by reading the bytes a pane actually receives (tmux 3.7b):

```
paste-buffer -d        ->  AAA\rBBB\rCCC\r
paste-buffer -d -p     ->  \x1b[200~AAA\rBBB\rCCC\r\x1b[201~
```

Every layer above reported success, which is why this was invisible:

- tmux exited 0.
- The composer-empty probe returned `submitted` — truthfully. The composer *was*
  empty, because all ~90 fragments had been submitted.
- `observeFixStep` then correctly observed an unchanged workspace and correctly
  raised `ambiguous_worker_state`. That stop was right; it just named the wrong
  suspect.

### The fix

`pasteBufferArgs` now emits `paste-buffer -d -p`. Inside bracketed-paste markers
the CRs are literal pasted text, so the whole prompt lands in the composer as one
message and `SendMessage`'s trailing Enter submits it exactly once. `-r` is
deliberately not added: a real terminal paste carries CR for its line breaks too.

This affected **every** prompt over `ports.MaxInlinePromptBytes` (4 KB) sent to a
raw-mode agent TUI, not only fix prompts.

## What AO can now prove about a delivery

`steps[].fixDelivery` on the run detail response, derived read-time from the
dispatch checkpoints (`workflow/fix_delivery_report.go`). Non-secret by
construction: identifiers, counts, sizes, digests, and a bounded snippet no
longer than the review step's existing `findingsSummary`. **No prompt text is
persisted or returned.**

| Question | Field |
|---|---|
| Which review verdict triggered it | `reviewVerdict` |
| Review generation / run ID | `reviewRunId`, `reviewTargetSha` |
| Count of findings | `findingsCount` |
| Digest of the finding payload | `findingsDigest` (SHA-256), `findingsBytes` |
| Were the findings actually in the prompt | `findingsEmbedded` |
| Which findings (recognizably) | `findingsSnippet`, `findingsSource` |
| Fix attempt ID / generation | `fixAttemptId`, `cycleNumber`, `transportAttempt` |
| Was prompt delivery acknowledged | `submission`, `acknowledged` |
| **Did the agent receive the bytes AO sent** | `receiptMatch` (`match` / `other` / `none`) |
| Worker session ID | `sessionId` |
| Terminal reason | `terminalErrorClass`, `terminalOutcome`, `nextAction`, `reason` |
| Delivery phase | `state`, `dispatchedAt` |

`findingsEmbedded` and `receiptMatch` are the pair that separates the two failure
modes this incident conflated:

- `findingsEmbedded: false` — AO built a prompt without the findings. A
  construction bug.
- `findingsEmbedded: true` **and** `receiptMatch: "other"` — AO built the right
  prompt and the agent received different bytes. A transport bug. This is what
  wf-a816d7fe would have reported.
- `findingsEmbedded: true` and `receiptMatch: "match"` — the worker had the
  complete findings and chose not to act. A worker or prompt-semantics question.

`receiptMatch` is observability only; nothing branches on it, and a receipt that
cannot be read reports empty rather than guessing.

## Fail-closed behaviour is unchanged

A mutating fix worker that produces no verifiable change still stops the run with
`ambiguous_worker_state`. None of this suppresses that protection — it only makes
the stop explain itself. The read-only exception (`workflow/read_only_completion.go`)
is untouched and still applies solely to work steps whose plan declared
`writeIntent: read_only`; the run above declared `mutating`.
