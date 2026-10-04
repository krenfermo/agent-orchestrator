# Project-memory baseline evidence (Phase 0)

Phase 0 of AO's project-memory work measures the pipeline that exists today. It
adds no behavior to planning, working, reviewing, fixing, or verifying: it wraps
those dispatch surfaces and writes down what each one had available and what it
consumed, so later phases have a real "before" to argue against instead of a
remembered one.

The single rule this whole subsystem exists to enforce is that **a number AO
could not measure is never presented as if it had been measured.** Every metric
carries the basis it was obtained on, and a metric that could not be obtained is
`null` with a stated reason. Nothing is ever silently zero.

- Code: `backend/internal/observe/projectmemory` (schema, recorder, sink) and
  `backend/internal/observe/projectmemory/wfdispatch` (the dispatch decorators).
- Harness: `backend/cmd/aobaseline`.

## Where evidence lives

Evidence is written under AO's data dir, honoring `AO_DATA_DIR`:

```
<AO_DATA_DIR or ~/.ao/data>/project-memory/baseline/<run key>/<record id>.json
```

One file per agent dispatch, filed under the run it belongs to. It is never
written beside the repository being measured and never into an OS-default
application-data location (`~/Library/Application Support`, `AppData\Roaming`,
`AppData\Local`) — `ValidateEvidenceDir` refuses both a relative directory and
any path inside those trees, matching the hard rule in `AGENTS.md` that all app
state lives under `~/.ao`.

The `<run key>` is the first of these the dispatch actually carried: workflow run
id, task id, session id, record id. It is never invented — a reviewer launch,
for example, does not know its workflow run, so its record is filed under the
reviewer session it created and links its review run instead.

## How to produce a baseline

```bash
cd backend
go run ./cmd/aobaseline                      # measure the repo you are standing in
go run ./cmd/aobaseline -repo /path/to/repo  # measure another checkout
```

or, from the repository root, `npm run baseline:project-memory`.

Flags: `-repo` (default: the git repository containing the working directory),
`-evidence-dir` (default: the path above), `-run-id` (default:
`baseline-<UTC timestamp>`).

The harness exercises four representative agent tasks — planner, worker,
reviewer, fix — and writes one evidence record for each. Every prompt and
context document it measures is produced by the **unmodified** builders the
daemon's own dispatchers call:

| Task              | Role         | Real pipeline code exercised                                        |
| ----------------- | ------------ | ------------------------------------------------------------------- |
| `planner-context` | `planner`    | `adapters/planner/command.ContextBuilder.Build`, recorded by the same `wfdispatch.RecordPlannerContext` the live planner decorator uses |
| `worker-prompt`   | `worker`     | `workflow.BuildPlanArtifact` + `workflow.BuildWorkStepPromptWithSpec` |
| `reviewer-prompt` | `reviewer`   | `workflow.BuildReviewPrompt`                                          |
| `fix-prompt`      | `fix_worker` | `workflow.BuildFixPrompt`                                             |

**What the harness deliberately does not do is call a provider.** It measures the
supply side (files inspected, source reachable, context assembled and sent). The
consumption side — prompt/output/cached tokens, the agent's own tool calls and
file reads — is recorded as `unavailable`, with that stated in each metric's
`method` field. Those numbers come from a live run, below.

Verify has no harness task: it assembles no context, so a record of it would be
a row of "not applicable" rather than a baseline. It is still instrumented for
live runs, where its command, duration, and exit status are real.

## Live-run recording

Set `AO_PROJECT_MEMORY_BASELINE=1` on the daemon. `wfdispatch.Instrument` then
wraps every agent dispatch surface (`Spawner`, `ReviewerLauncher`,
`MessageSender`, `VerifyRunner`, `Planner`) and each real dispatch writes an
evidence record with the same schema, including provider token telemetry where
the surface delivers it.

Off by default. A wrapper never changes what it wraps: arguments, return values,
and errors pass straight through, and the optional capabilities workflow
type-asserts for (`SubmissionReportingSender`, `PlannerDescriptor`) are preserved
through the wrapper so instrumentation cannot silently downgrade what the
pipeline can prove about itself. A recording failure is logged and never turned
into a dispatch failure.

## Evidence schema

`schemaVersion` is `project-memory-baseline/v1`.

### The `Metric` shape

Every number in a record is a metric:

```json
{ "value": 21383, "basis": "estimated", "method": "utf8 bytes / 4 heuristic (no provider tokenizer)" }
```

| `basis`       | Meaning                                                                                              | `value`  |
| ------------- | ---------------------------------------------------------------------------------------------------- | -------- |
| `measured`    | AO observed it directly: bytes it read, a duration it timed, a count of calls, or a figure provider telemetry reported. | non-null |
| `estimated`   | AO derived it from something it measured, via the heuristic named in `method`.                        | non-null |
| `unavailable` | AO could not obtain it. `method` says why.                                                            | **null** |

The labeling rule, enforced by `Metric.Validate` before any write:

- an `unavailable` metric carries no value and states a reason;
- a `measured` or `estimated` metric carries a value and states how it was obtained;
- no count is negative;
- no metric is unlabeled.

A record that breaks the rule is refused, so **an evidence file that exists is
one whose numbers are honestly labeled**. A metric that was simply never
populated is normalized to `unavailable` with the reason "not recorded by the
baseline harness" — an absence is filled in, a number never is.

`measured` / `estimated` / `unavailable` map onto the codebase's existing
`domain.MetricCertainty` vocabulary (`actual` / `inferred` / `unknown`) via
`Basis.Certainty()`.

Note the distinction that matters most when reading a file: **a measured `0` and
an `unavailable` `null` are different findings.** "This dispatch made no tool
calls" and "this surface cannot see tool calls" must never be conflated, which is
why `value` is nullable rather than defaulted.

### Record fields

| Field | Notes |
| ----- | ----- |
| `schemaVersion`, `recordId`, `generatedAt` | Record identity. |
| `role` | `planner` \| `worker` \| `reviewer` \| `fix_worker` \| `verify` (`domain.WorkflowRole`). |
| `workflowRunId`, `workflowStepId`, `taskId`, `projectId`, `sessionId` | Keys back to the run. Empty when the dispatch surface genuinely does not carry the id — left empty, never guessed. |
| `harness`, `provider`, `model` | `provider` is the static harness→vendor mapping (`domain.ProviderForHarness`), not an inference from a binary name. |
| `dispatch.startedAt` / `.completedAt` / `.durationMs` | Wall clock around the wrapped call. `durationMs` is measured. |
| `dispatch.succeeded`, `dispatch.error` | A failed dispatch is still recorded: a failed launch is a real fact about the pipeline. |
| `context.filesInspected` | Distinct paths this dispatch read. Measured only where the surface reports reads (the planner assembles its own context; a worker spawn hands a prompt to a provider process that reads on its own). |
| `context.filesInspectedBytes` | Bytes across every read, **re-reads counted again** — a re-read costs context twice. |
| `context.repeatedReads` | Reads beyond the first per path: the signal project memory is expected to reduce. |
| `context.sourceBytesAvailable` | Measured size on disk of the source in the dispatch's declared scope. |
| `context.sourceTokensAvailable` | Always `estimated` — AO does not run the provider's tokenizer. |
| `context.contextSentBytes` | Measured byte length of the payload AO handed the provider. |
| `context.contextSentTokens` | `estimated` from the bytes above, **upgraded to `measured`** when provider telemetry reported a real prompt-token count. |
| `context.files[]` | Per-path `path`, `reads`, `bytes` (measured), `estimatedTokens`. |
| `providerTokens.{prompt,output,cacheRead,cacheWrite,reasoning,total}` | Provider telemetry only. Never derived from AO's byte counts — a provider's cache accounting is not something a byte count can approximate. `total` sums only the counters actually reported; if none were, it is `unavailable`, because a sum of nothing is an unknown number of tokens, not zero. |
| `tools.total`, `tools.byName` | Tool calls, where the surface reports them. |
| `outcomes.reviewRunIds`, `outcomes.reviewVerdict` | Links to this run's review outcomes. An unknown verdict stays empty, never "approved by default". |
| `outcomes.verifyExitCode`, `.verifyPassed`, `.verifyDurationMs` | Verify outcome, `null` when this dispatch ran none. |
| `notes[]` | How the record was produced (e.g. that the harness dispatched no provider call). Notes never carry metrics. |

### Which metrics each surface can honestly report

`Capabilities` on a dispatch declares what that surface can see; everything else
is recorded `unavailable` with the reason stated. This is what keeps a worker
spawn from reporting "0 files inspected" when the truth is that AO cannot see the
provider process's reads.

| Surface | file reads | tool calls | context payload | provider tokens |
| ------- | ---------- | ---------- | --------------- | --------------- |
| Planner (`Planner.Generate`) | yes — AO assembles the context document | no | yes | no |
| Worker (`Spawner.Spawn`) | no | no | yes (the prompt) | no |
| Reviewer (`ReviewerLauncher.Launch`) | no | no | yes (prompt + system prompt) | no |
| Fix (`MessageSender.Send`) | no | no | yes (the fix message only, not the session's accumulated history) | no |
| Verify (`VerifyRunner.Run`) | no | yes (the command) | n/a — no provider call | n/a |

## Reading a baseline

The first question this baseline was built to answer is the ratio between what a
role's scope holds and what the role is actually given. One run over this
repository (the figures move with the repository, so re-run rather than quoting
these):

| Task              | `contextSentTokens` (est.) | `sourceTokensAvailable` (est.) | `filesInspected` |
| ----------------- | -------------------------: | -----------------------------: | ---------------: |
| `planner-context` |                     21,401 |                      6,728,413 |    5 (measured)  |
| `worker-prompt`   |                        285 |                        734,553 |    `null`        |
| `reviewer-prompt` |                        840 |                        145,173 |    `null`        |
| `fix-prompt`      |                        348 |                        139,362 |    `null`        |

The worker, reviewer, and fix prompts are guardrails and an objective: the agent
is told what to do and left to find the code itself. Whether that is the right
trade is exactly what Phase 1 is for — and the `null` in the last column is the
honest reason this harness alone cannot answer it, since AO does not see the
reads those agents then make. Run the daemon with
`AO_PROJECT_MEMORY_BASELINE=1` and provider telemetry fills in the other half.

## Tests

- `backend/internal/observe/projectmemory` — the labeling rule, serialization
  (including that a measured `0` and an `unavailable` `null` survive a round
  trip), sink path rules, and the source scan.
- `backend/internal/observe/projectmemory/wfdispatch` — that each decorator
  records the right role and passes its dispatch through unchanged, and that the
  optional capabilities survive wrapping.
- `backend/cmd/aobaseline` — that the harness writes one valid evidence file per
  task, under `AO_DATA_DIR`, with the supply side measured and the consumption
  side explicitly unavailable.

## The routing block

An evidence record can also carry what the context router decided for that
dispatch (`EvidenceRecord.Routing`). It is an additive extension: the field is
omitted entirely when there is nothing to say, `EvidenceSchemaVersion` is
unchanged, and every consumer of this schema keeps reading every record. See
[context-router-metrics.md](context-router-metrics.md).

## The P2-A memory-pack baseline

P2-A adds a second, complementary measurement, and the two must not be
conflated. This harness measures **one dispatch's payload as AO sent it**;
`projectmemory.PackStats` measures **one role's memory pack as AO assembled
it**. Together they are the before/after over AO-assembled context that P2-B
needs.

Per pack, the memory subsystem records: the role, the candidate item count and
byte total selection could choose from, the selected item count and rendered
byte/token total, how many facts were dropped and how many were reduced to their
summary, how many were withheld because AO could no longer vouch for them, the
source paths behind the selected facts, and the fallback reason whenever the
pack is empty or degraded.

Measured on this repository at P2-A (default limits, default 24 KiB / 40-fact
budget, one changed path):

| Role | Candidates | Candidate bytes | Selected | Selected bytes | ~Tokens | Dropped |
| --- | --- | --- | --- | --- | --- | --- |
| Planner | 467 | 173,108 | 40 | 5,981 | 1,496 | 427 |
| Worker | 546 | 154,752 | 14 | 23,734 | 5,934 | 532 (+1 to summary) |
| Reviewer | 544 | 201,300 | 40 | 15,409 | 3,853 | 504 |
| Repair | 531 | 134,798 | 40 | 15,409 | 3,853 | 491 |

Indexing the same repository: 3,333 files walked, 3,136 admitted, 31.8 MB read,
560 facts and 589 relations written in ~780 ms. A second pass over the unchanged
tree writes **zero** facts, reconfirms all 560, and retires none.

### The same honesty rule applies

`SourcesReused` lists the paths whose *summarised* content AO supplied from
memory rather than re-deriving it this dispatch. **It is not a count of file
reads the agent avoided, and must never be reported as one.** The `null` in the
`filesInspected` column above is the same limitation restated: AO does not
observe the Worker's, Reviewer's or either Repair Agent's own reads, with or
without `AO_PROJECT_MEMORY_BASELINE`, so no P2-A number can speak for them.

Full design: [project-memory.md](project-memory.md).
