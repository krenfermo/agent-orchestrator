# P1 autonomy readiness

*Status: P1-E complete. This is the INTEGRATION document for phase 1 — the one
that says how the P1 pieces compose, what was proven about them together, and
what is deliberately still open. It does not restate the phase docs: each
section links to the one that owns the mechanism, and adds only what is true of
the whole.*

| Phase | What it added | Doc |
| --- | --- | --- |
| P1-A | Task / Autonomous / Master strategies, AUTO policy, approval separated from strategy | [`execution-strategy.md`](execution-strategy.md) |
| P1-B | Deterministic Resume, plan reuse/regeneration, the bounded Repair Agent, recovery assessment | [`p1b-recovery-and-repair.md`](p1b-recovery-and-repair.md) |
| P1-C | Runtime capacity scheduler, durable claims, fairness, Runtime GC | [`p1c-capacity-and-runtime-gc.md`](p1c-capacity-and-runtime-gc.md) |
| P1-D | Frozen placement + its generation, branch authority and cession, unified admission, provider-attempt ledger and failover | [`p1d-placement-locks-and-failover.md`](p1d-placement-locks-and-failover.md) |
| P1-E | Per-task placement override and the generation-safe placement transition; terminal placement retirement; the `Runtime.Destroy` boundary; integrated validation | this document |

## The architecture, in one pass

One obligation moves through P1 like this, and every arrow is a durable row
rather than a call stack:

```
strategy selection      P1-A   frozen on the run, never re-derived
  -> planning           P1-A   Master only; Task and Autonomous skip it
  -> task creation      P1-A   strategy, criteria and write intent in ONE transaction
  -> placement freeze   P1-D   execution_placements, written before any mutation
  -> provider attempt   P1-D   provider_attempts, ordinal 1
  -> branch authority   P1-D   direct-branch only; isolation is physical otherwise
  -> admission          P1-D   ONE decision over nine conditions, capacity last
  -> runtime launch     P1-C   capacity claim + ao-session ownership token
  -> worker completion  P0-B   generation-fenced dispatch state machine
  -> review / fix       P0-B   review authority bound to the reviewed generation
  -> verify             P0-B   bound to the approved artifact
  -> integration        P1-D   integration lane, CAS on the target ref
  -> capacity release   P1-C   at the instant the run is terminal
  -> placement retire   P1-E   at the same instant (see "What P1-E changed")
  -> runtime/worktree GC P1-C/D by ownership and incarnation proofs
  -> parent convergence P1-B   the parent completes after authoritative children
```

**Three generations, and they never collapse into one.** A *lifecycle*
generation advances when the obligation is retried. A *placement* generation
advances only when the physical placement is replaced. A *provider attempt*
advances neither — a failover leaves the obligation and the checkout exactly
where they were. Collapsing any pair would mean every retry minted a worktree,
or every failover looked like new work.

**Waiting is free.** `AdmissionWaitReason.SpendsRetryBudget()` and
`ConsumesCapacity()` answer false for every value in the vocabulary, and the
tests assert that over the whole vocabulary rather than per reason — so a new
reason cannot quietly be added that charges a run for queuing.

## What P1-E changed

Three production changes. Everything else in this phase was validation.

### 1. Per-task placement override, and the placement transition (§B/§C)

P1-D shipped every placement route read-only and recorded why: a write that
re-points a placement is a write that can aim a running agent at a different
checkout. P1-E is the model that write needed, and it is deliberately **two**
operations rather than one.

```
REQUEST     execution_placement_overrides
            what somebody wants. Consumed once, by the freeze.
            auto | direct_branch | isolated_worktree

TRANSITION  execution_placement_transitions
            one placement generation replaced by another, after
            proving no authority still owns the old one.
```

The asymmetry is the whole point:

- **Before the freeze** a request decides the placement. It is read once, in
  `selectExecutionPlacement`, and the frozen record then wins forever.
- **After the freeze** a request decides *nothing*. It is still recorded — an
  operator's intent is not discarded — and the caller is told by name that a
  transition is required. A standing override can never silently re-point live
  work on the next reconcile.

`auto` is a real value, not the absence of one: withdrawing an override has to
be sayable, and expressing it as "delete the row" would make a withdrawal
indistinguishable from a request nobody made.

**A transition is refused unless every authority has let go.**
`PlacementQuiescence` is an AND over durable facts, each asked of the component
that owns the answer — the run's state, the placement's own state, the
provider-attempt ledger, the capacity scheduler, the branch-lock manager. It is
never an inspection of the filesystem: a checkout that looks idle proves nothing
about whether a provider is mid-write, a merge is half-applied, or a repair
holds a ceded lock.

The refusal vocabulary is closed, and every value names an **authority** rather
than a symptom, because the operator's next action differs per authority:

| Refusal | What it means | What clears it |
| --- | --- | --- |
| `no_operator_authority` | the request named nobody | name a requester |
| `unknown_placement_request` | a placement value this build cannot read | a valid value; never coerced to a default |
| `no_frozen_placement` | nothing is frozen yet | record an override instead |
| `placement_not_current` | the named generation is not the newest | re-read and retry |
| `lifecycle_state_drifted` | the placement is not in the asserted state | re-read; the request describes a world that moved |
| `active_provider_attempt` | a provider may be writing right now | let it finish, or fail it over |
| `held_capacity_claim` | a runtime slot is still charged | clears on its own |
| `live_runtime` | a worker/reviewer/fix/repair runtime is bound | stop it |
| `held_branch_authority` | the run still holds a branch lock | clears when the run releases it |
| `outstanding_integration` | a merge or review still owns the checkout | let it conclude |
| `run_is_terminal` | nothing will launch into a replacement | nothing; a replacement would only orphan a checkout |
| `authority_unreadable` | an authority could not be read | fail-closed by design; retry |

Ordering: prove, then write the intent, then move — the same order the branch-lock
cession uses, so a crash leaves an explanation for a move that may not have
happened rather than a move nobody can account for.

**Idempotency is carried by the schema.** A partial unique index admits one
outstanding override per obligation, so a double-click supersedes rather than
stacks. A second partial index admits one *surviving* transition per superseded
generation, so a repeated transition request returns what already happened
instead of minting a third generation. Refusals are excluded from that index on
purpose: a transition refused because a claim was still held has to be retryable
once it clears, and a refusal occupying the idempotency key would make the first
"not yet" a permanent no.

**The old generation stays.** Retired to `preserved` when it is an isolated
placement whose work never landed — those commits may be the only copy — and
`terminal` otherwise. Both are auditable, and `ListPlacements` still returns them.

**An isolated replacement gets its own branch.** Generation 1 keeps the
historical `ao/<run>/<task>` name exactly; a replacement gets `-g<N>` appended.
The suffix rather than a nested path is forced by git: `refs/heads/a/b` cannot
exist while `refs/heads/a` does, so a nested name would make the replacement
uncreatable precisely when its predecessor is being preserved.

**What is deliberately not built:** there is no operation that changes a
worktree path while something is running in it. A transition retires a
generation and freezes a successor; the successor is materialised by the
ordinary launch path under the ordinary admission gate. "Re-point this running
agent" is not expressible.

Surface: `POST /api/v1/workflows/{id}/placement/override` and
`.../placement/transition`; `ao workflow placement override|transition`. A
refused transition is **409 with the refusing authority named**, never a 500 and
never a silent 200.

### 2. Terminal placement retirement (§O)

The integration audit found a seam between two individually-correct modules. A
terminal run returned its capacity slots at the instant it finished (P1-C, in
`completeRun`/`CancelRun`) but its **placement** was only retired by
`reconcilePlacementsForRun`, which is reached from `Coordinator.Reconcile` —
and the daemon runs that at **boot**.

On a long-lived daemon the retirement therefore never happened during ordinary
operation, so a finished run's placement sat `active` forever. That is not
merely untidy: the placement sweep will only remove an isolated checkout when
the record says `integrated` and names the commit its work landed at, so a
placement stuck in `active` is one the sweep is *right* to refuse — and every
run that completed without integrating (a direct-branch run, a read-only task, a
cancelled run) left an AO worktree that nothing would collect until the next
restart.

`retirePlacementsForTerminalRun` closes it with the argument P1-C already made
for capacity: a run that is over releases what it holds at the moment it is
over. It reuses `reconcilePlacementsForRun` rather than repeating its rules, so
"preserved when the work never landed, terminal otherwise" has one definition.

A run in `needs_attention` is **not** terminal and keeps everything — that is the
state a person is looking at when they go to recover a run, and a placement
retired underneath them is a checkout the recovery path can no longer name.

### 3. The `Runtime.Destroy` ABA boundary (§Q)

P1-D documented that `Runtime.Destroy(session name)` is ABA-unsafe while
production safety rests on `DestroyInstance`, and left the footgun open. **The
primitive cannot be made safe** — a runtime session name is reusable, so the
check and the destroy are always two moments — and removing it would be the
broad runtime API redesign this phase rules out.

What P1-E closes is the operational hazard rather than the primitive:

- `ports.Runtime.Destroy` is now marked in the contract as name-addressed,
  ABA-unsafe and **non-authoritative**, with the rule stated: it may not be used
  to make an ownership decision.
- `TestOwnershipSensitivePathsNeverUseNameOnlyDestroy` walks the source of the
  packages that *do* make ownership decisions — `internal/runtimegc`,
  `internal/workflow`, `internal/lifecycle` — and fails on any `.Destroy(` call
  in them. It is a static check because the hazard is somebody **adding** such a
  call, and a behavioural test only covers the paths it happens to drive.

The boundary is stated rather than widened. `internal/session_manager` and
`internal/review` still call it, and legitimately: theirs is
teardown-then-recreate within AO's own handle namespace, where destroying a
stranger that had taken the name is the same outcome as the `Create` that
immediately follows. **This does not make the primitive safe, and nothing here
claims it does.**

## Operator observability

Three read surfaces, each named for a question an operator actually asks. They
are separate on purpose and the split is not fragmentation — a fourth combined
command was considered under §V and not built, because each of these answers one
question completely.

| Question | Route | CLI |
| --- | --- | --- |
| what is this run, what strategy and approval policy, where are its steps | `GET /api/v1/workflows/{id}` | `ao workflow …` |
| what is it waiting for, what can I do about it | `GET /api/v1/workflows/{id}/recovery` | `ao workflow recover status <id>` |
| where does the work happen, which provider is trying, which authority is withholding the launch | `GET /api/v1/workflows/{id}/placement` | `ao workflow placement <id>` · `ao provider attempts <id>` |
| what did somebody ask for, and what did AO refuse | same placement route | `ao workflow placement <id>` |
| runtime capacity and the sweep | `GET /api/v1/runtime/capacity`, `POST /api/v1/runtime/gc` | `ao capacity status` · `ao runtime gc` · `ao worktree list|gc` |

No tokens are exposed on any of them. A placement's owner token names a daemon
incarnation and is an ownership credential in shape, which is nothing an
operator needs to diagnose a stuck run.

## Validation evidence

Everything below was run on macOS (Darwin 25.6.0) on the P1-E branch. Where a
claim is bounded, the bound is stated rather than rounded away.

### Integrated end-to-end

| Property | Evidence |
| --- | --- |
| bounded TASK, mutating, end to end with no planner and no hierarchy | `TestP1E_MutatingTaskRunsEndToEndAndLeavesNoAuthorityBehind` |
| read-only TASK takes no branch mutation authority | `TestP1E_ReadOnlyTaskTakesNoMutationAuthority`; completion semantics in `read_only_completion_test.go` |
| TASK converges across restarts at four different points, one spawn each | `TestP1E_TaskConvergesAcrossRestartsAtSeveralPoints` |
| AUTONOMOUS end to end, including changes-requested → fix → approve → verify | `TestAutonomous_HeadlessProgression_NoGET`, `TestAutonomous_ChangesRequestedFixLoopThenVerifyThenComplete`, `TestAutonomous_RestartNoDuplicateWork` |
| MASTER converges; siblings have distinct placement authority; parent binds no runtime of its own | `TestP1E_MasterObjectiveConvergesAndTheWholeTreeIsClean` |
| safe Master parallelism, and unsafe branch parallelism refused | `TestIsolatedSiblingsAdmitConcurrentlyWithinSchedulerCapacity`, `TestDirectBranchSiblingsSerializeOnTheBranch`, `TestOneWorkflowCannotMonopolizeCapacity` |
| Resume is idempotent at worker/review/fix/verify | `p1b_resume_idempotency_test.go`, `TestRepeatedAdmissionCreatesNoDuplicateAuthority` |
| Repair resumes the original obligation, on a worktree and on a direct branch | `p1b_recovery_test.go`, `p1d_branch_cession_test.go` |
| provider failover: all seven flows, budget bounded, ambiguity never fails over | `p1d_provider_attempt_test.go` (14 tests), `p1d_failover_safety_test.go` |
| the capacity/placement/provider matrix, including restart reconstructing the waiting reason | `p1d_admission_test.go` (12 tests) |
| crash matrix across module seams | `p1d_crash_matrix_test.go`, plus P0-A/P0-B suites and the restart points above |
| terminal cleanup leaves no authority | `TestP1E_MutatingTask…`, `TestP1E_MasterObjective…` — one read over capacity, runtimes, locks, placements, attempts and wakes together |
| failure cleanup preserves what recovery needs | `TestP1E_CancelledRunPreservesTheEvidenceRecoveryNeeds`, `TestP1E_NeedsAttentionKeepsItsPlacementLive` |

The two cleanup properties are asserted in one file on purpose: a cleanup
aggressive enough to satisfy §O and a preservation careful enough to satisfy §P
are in tension, and only checking both catches a change that trades one for the
other.

### Real git, real tmux, real providers

- **Real multi-worktree git** — `internal/integration/p1e_multi_worktree_git_test.go`:
  three master children (two independent, one dependent) integrating into one
  target that provably ends holding all three; a real merge conflict named with
  the exact file, evidence preserved, the losing checkout left clean and
  un-collected, and three identical retries producing the same refusal and no
  target movement; external drift detected, replayed over, revalidated, with the
  external commit still reachable and present in the reflog; unreplayable drift
  stopping with the target exactly where the external actor left it.
- **Real tmux** — the four `real_tmux_*` suites (17 tests: worker and reviewer
  ownership, stale-incarnation rejection, provider-failover replacement safety,
  GC) run **5 consecutive times**, green every time. The operator's default tmux
  server was never started.
- **Real providers** — both smokes RUN, through AO's production adapter
  boundary, in temporary workspaces, against the operator's existing logins:
  - Claude: packaged Node → `claude-agent-acp` → the installed Claude Code, two
    tiny turns including a resume with standing-context replacement.
  - Codex: a real `codex` app-server (ChatGPT login), thread resume on a fresh
    process, plus the steer test.

  **Scope, stated plainly:** these prove the *provider adapter* boundary. The
  worker *launch* path — tmux ownership tokens, prompt delivery, completion
  evidence, capacity release, cleanup — is proven by the real tmux suites with a
  controlled agent binary, not by a real provider driving a real worker
  end to end. Nothing here claims otherwise.

### Soaks

**Deterministic (§X).** Compressed, on a fake clock, with a restart folded into
a share of every lane:

| Lane | Runs | Completed | Spawns | Reviewer launches | Fix prompts | Leaks |
| --- | --- | --- | --- | --- | --- | --- |
| TASK | 100 | 100 | 100 | 100 | 0 | none |
| AUTONOMOUS | 100 | 100 | 100 | 134 | 34 | none |
| MASTER | 50 | 50 | 100 | 100 | — | none |

"Leaks" is one read over capacity claims, runtime-bound claims, branch locks,
live placements, authoritative provider attempts and pending wakes, taken per
iteration so a leak names the iteration it started on. Zero across all 250.
Duplicate spawns: 0. Parent/child divergence: 0.

What this **cannot** show is anything that is a function of wall-clock time: a
real memory leak, fd exhaustion, a tmux server degrading over hours, a provider
session expiring. The clock is fake and the runtime is a fake.

The counts above are the full run, produced with `AO_P1E_SOAK_FULL=1`. The
**ordinary suite runs a fifth of them** (20/20/10) and the reason is worth
stating rather than hiding: each iteration drives a real sqlite store and a real
git repository, which under `-race` costs about five minutes and would push
`internal/workflow` past `go test`'s ten-minute per-package default — turning a
required gate red on time rather than on correctness. The reduced run still
asserts every invariant on every iteration (one spawn, one reviewer, one
placement, zero residue), so a leak that is a function of iteration count is
caught the moment it appears; it simply has less headroom above it.

**Wall-clock (§Y).** 45 minutes 3 seconds of real elapsed time (19:56:23Z →
20:41:26Z), **64 rounds**, each looping the deterministic soaks, the real tmux
suites and the real git and workspace suites, then sampling the machine.

- 63 rounds green, **1 failure**: `TestRealTmux_LargeFixPromptArrivesAsOneBracketedPaste`
  on round 42, under three concurrent test binaries plus a `-race` build and a
  lint run. It passed 10/10 in isolation afterwards. Recorded as a
  **timing-sensitive flake under CPU contention**, not as a defect — and
  recorded rather than dropped, because a 1-in-64 real-tmux paste flake is worth
  knowing about.
- Process count flat at 82–88 for the whole run. **One** tmux server (the
  tests' own), **zero** sessions on the operator's default server, **zero**
  stray worktrees, every round.
- Open fds sampled system-wide (`lsof -u`) rose from ~15.7k to ~24k in the first
  nine rounds and then stayed flat through round 64. The rise tracks the
  concurrent test binaries rather than AO; the **plateau** is the signal, and it
  is flat.

This is 45 minutes. It is **not** 24-hour evidence and nothing here should be
read as such.

### Gates

`gofmt -l`, `go vet`, `go build`, `go test -count=1`, `go test -race -count=1`,
golangci-lint v2.12.2 at **0 issues**, sqlc generation stable across repeated
runs, OpenAPI + `schema.ts` regenerated with no drift, frontend `typecheck`,
`typecheck:e2e` and the full vitest suite.

## Known limitations

Stated rather than implied, because a limitation an operator discovers is worse
than one they were told about.

1. **`Runtime.Destroy` remains ABA-unsafe.** The primitive is marked and fenced
   off from ownership-sensitive paths by a test; it is not fixed, and it cannot
   be without an incarnation-addressed API everywhere.
2. **Reviewer capacity is released one cycle late.** Re-audited at P1-E and
   unchanged: admission reads capacity, placement, branch and provider
   authority, none of which is the review run's terminal verdict, so the
   sufficient witness for an immediate release still does not exist where the
   release would have to happen. The slot is released on the next reconcile.
   Bounded and correct; deliberately not redesigned to optimise one slot.
3. **A transition into an isolated placement does not move the predecessor's
   checkout.** The replacement gets its own branch and its own checkout; the
   predecessor is preserved where it is. Reclaiming it is `ao worktree gc`'s job
   once it is genuinely finished with, which is the same rule every other
   preserved checkout follows.
4. **Placement retirement for a run made terminal outside the coordinator** —
   a state written directly into the store by something other than
   `completeRun`/`CancelRun` — still waits for the next `Coordinator.Reconcile`,
   i.e. the next daemon boot. Every coordinator-owned terminal transition
   retires immediately.
5. **Real-provider evidence covers the adapter boundary, not the worker launch
   path.** See above.
6. **Windows/conpty is out of scope for P1**, as it has been since P0.
7. **Four renderer-smoke Playwright specs fail on this branch**, and the cause
   is not P1-E. The branch introduced `stores/auth-store.ts` and
   `lib/platform-adapter.ts` — an identity resolution and a `/readyz` daemon
   probe that both render *in place of* the shell when they cannot be answered —
   without updating `frontend/e2e/`. Every renderer spec then asserted against a
   sign-in form. P1-E added the two stubs those probes need, shared once in
   `e2e/support/fake-bridge.ts`, which took the suite from 21 failures to 4. The
   remaining four are spec-vs-UI drift from the same branch's shell and settings
   work (`daemon-status`, `settings-section[data-section=updates]` and
   `session-terminal` do not mount for a spec running without a daemon), and
   fixing them means deciding what the renderer should render in that state —
   UI scope P1-E does not own. See the P1-E report for the exact list. Note the
   workflow's own description of this job: it is fast renderer-regression
   coverage, explicitly **not** the canonical T0/P0 end-to-end gate.

## P1 GO criteria

P1 is GO when all of the following hold, and each is a test rather than a
judgement:

1. Task, Autonomous and Master each complete end to end.
2. Safe Master parallelism works; unsafe branch parallelism is refused.
3. Resume is idempotent in the integrated system.
4. Repair resumes the original obligation, on both placement types.
5. Capacity is enforced and never leaked.
6. Placement is frozen, and a transition is generation-safe, quiesced and
   audited.
7. Review/fix/verify authority is exact; integration happens exactly once.
8. A conflict preserves its evidence; external drift is never overwritten.
9. Runtime GC and worktree GC are safe.
10. Safe failover works, ambiguous execution never fails over, the budget is
    bounded, and a stale provider cannot regain authority.
11. Parent/child convergence is correct; terminal runs do not relaunch; no
    infinite wake or retry loop exists.
12. Real git and real tmux suites are green, and every CI gate passes.
