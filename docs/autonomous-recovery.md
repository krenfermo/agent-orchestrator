# Autonomous recovery: evidence, provenance and adoption

Two real workflows sat blocked overnight with finished work in their worktrees.
Neither was blocked by a bug in the work; both were blocked by an inference AO
made from an ABSENCE.

| run | worker | AO said | reality |
| --- | --- | --- | --- |
| `wf-00283521-c214-4288-9078-a2ed3548fcee` | `medusa-4` | `agent_start_failed` — "no first signal within 10m0s" | the worker ran for ~16 minutes and produced a complete implementation, committed by hand as `74f053a6` |
| `wf-cd5bad10-e5f2-4427-879e-24836091cae3` | `agent-orchestrator-35` | `verify_workspace_changed` → `verify_unrepairable` | two legitimate reviewer-requested fix cycles, every check passing, committed by hand as `1de0aa7e` |
| `wf-f0efac7e-b8d1-4b39-a8ae-7172795ea520` (Medusa parent) | — | `child_needs_attention` → `attention_cleared` → `child_needs_attention`, 8s apart | the child never moved |

This document is the map of what changed. Nothing here relaxes review,
verification, branch ownership, write-set ownership or the destructive-operation
safeguards; every mechanism below ends in the ordinary review and verification
path, and several of them exist specifically so that path can be reached at all.

## 1. A missing first signal is not a dead worker

`backend/internal/workflow/worker_signal_reconcile.go`

`FirstSignalAt` being unset says the HOOK PIPELINE has produced nothing. AO read
it as "the process never started". Those are different claims and only the first
one is supported: a provider whose hooks were never installed, whose hook
transport is broken, or which has not yet reached a hook-firing boundary looks
exactly like one stuck at a trust prompt.

Past the ten-minute startup grace AO now opens a **reconciliation** instead of
closing the step, and weighs every independent fact it can obtain:

- the session row (found, not terminated, not exited);
- activity or a completed turn at or after the dispatch — which the terminal
  activity observer supplies even when no hook ever fires;
- a **forced, never throttled** live git observation of the worktree;
- an optional runtime liveness probe (`WorkerLivenessProbe`, wired in the daemon
  to the Session Manager's own tmux/process probe).

The verdicts, in order:

| evidence | verdict |
| --- | --- |
| git-verified work in the tree | `completed` — adopt it, never re-dispatch |
| provably not running, nothing to show | `failed`, `agent_start_failed` |
| any positive liveness | `signal_delayed`, no state change |
| past `workStepSignalReconcileTimeout` (45m) with nothing at all | `failed`, `agent_start_failed` |
| otherwise | `reconciling`, no state change |

Durable worker lifecycle states: `starting`, `running`, `signal_delayed`,
`reconciling`, `completed`, `failed`. The two waiting verdicts write a
`worker_signal_delayed` checkpoint carrying the evidence, once per distinct
verdict rather than once per poll.

The forced observation also covers the neighbouring inference: a TERMINATED
session with a throttled observation used to read as "ended with no verifiable
work".

## 2. Workspace provenance

`backend/internal/workflow/workspace_provenance.go`

When verification finds the worktree differing from the approved review target,
AO used to hold exactly one fact — "the fingerprint moved" — which is equally
true of its own authorized fix worker and of a stranger editing the directory.

It now answers the second question. Every attribution is recorded as a
`workspace_provenance` checkpoint carrying the approved fingerprint, the
observed fingerprint, HEAD, the approved HEAD, branch, worktree, changed files,
the owning run/task/session, the mutation phase, the expected write set (when AO
holds one) and the paths falling outside it, plus a plain-language rationale.

| class | meaning |
| --- | --- |
| `AUTHORIZED_WORK` | this run's own work step produced it |
| `AUTHORIZED_FIX` | this run's own fix step produced it, under a cycle AO dispatched |
| `PREEXISTING` | already there when the task was dispatched |
| `OTHER_AO_TASK` | another AO task owns this branch/worktree |
| `EXTERNAL` | not the worktree or branch this run was authorized for |
| `CONFLICTING` | the approved commit is no longer reachable — history was rewritten |
| `UNKNOWN` | the default, and the answer whenever any required fact could not be read |

`AUTHORIZED_WORK` and `AUTHORIZED_FIX` get **one fresh, independent review of
what is actually in the worktree**, bounded at `maxProvenanceFreshReviews` (3),
served through the same `pendingFreshReview` dispatch branch as every other
fresh-review mechanism. Verification then runs against exactly what that review
approved. Every other class stops exactly as before — now with the class and the
rationale on the record, so the stop is readable.

`ContinueRun` carries the same transition for a run already parked
(`resumeProvenanceWorkspaceChange`), re-deriving every fact against the worktree
as it stands now and refusing everything it cannot prove.

## 3. Completed-work adoption

`backend/internal/workflow/work_adoption.go`

Both incidents ended with a person committing the work by hand, and AO having no
transition that could say "that is the work; carry on". The only routes out of a
durably failed work step were "start another worker" — which writes a second
implementation over the first — and "mark it done", which skips review.

`ContinueRun` may now ADOPT an existing commit as a work step's result, on seven
proofs, all required, all re-derived against the repository at decision time:

1. the commit descends from the base this task was dispatched at;
2. the range `base..head` is a real, non-empty change (git patch identity);
3. branch and worktree are the ones AO recorded for this task, and HEAD is the
   tip of that branch;
4. nothing of AO's is moving: every step at rest, no unfinished attempt, no open
   question, this run's own agent silent past the settle window;
5. the session AO spawned for the step exists and names this same worktree;
6. the worktree is stable across two observations;
7. the adoption is recorded durably (`work_commit_adopted`), bounded at
   `maxWorkAdoptions` (3), before anything moves.

Adoption runs BEFORE the dispatch branch in `ContinueRun`, so a second writer can
never be started over an adoptable changeset. The step's completion checkpoint is
written in exactly the shape `observeWorkStep` writes, so review dispatch treats
an adopted result identically to a produced one. **Adoption buys a review, never
a pass**: the run goes through review → fix → fresh review if needed → verify →
advance, and the abandoned attempt stays recorded as failed, because a person
landing a commit is not the agent having succeeded.

## 4. Parent attention derives from durable child state

`backend/internal/workflow/attention.go`, `master_coordinator.go`

The parent's `child_needs_attention` mirror was cleared unless the child's stop
was provably human-owned — and "human-owned" is derived from the child's NEWEST
checkpoint. Any row landing on the child after its stop made that lookup come
back empty for one pass, which unparked the parent; the next pass re-derived the
stop and parked it again.

The test is now the other way round: the mirror is held while the child sits in
`needs_attention`, and cleared only on positive proof that its stop is
self-remediable (`stopIsSelfRemediable`). Not being able to name a child's stop
is not evidence the child recovered.

Repeated identical propagation is deduplicated by
`recordChildAttentionStopOnce`, which looks back past unrelated intervening rows
to the last point at which the mirror could meaningfully have changed.

## 5. Provider preflight

`backend/internal/workflow/provider_preflight.go`,
`backend/internal/providerpreflight/`

Before an unattended dispatch AO asks whether the provider can start without an
operator: CLI resolvable, credentials usable, workspace trust recorded,
permission posture usable non-interactively. Refusals carry their own error
classes and canonical attention reasons:

- `provider_auth_required`
- `provider_workspace_trust_required`
- `provider_preflight_failed`

Two rules govern it. **It never answers a prompt** — trust and permission
acceptance are read from each provider's own supported configuration (the same
`~/.claude.json`, resolved through the same isolated `CLAUDE_CONFIG_DIR`, that
the spawned process will read), never written on a person's behalf and never
piped a "yes". And **what it cannot check is never a refusal**: every
undeterminable answer is Unknown, which the dispatch path treats as ready.
Grounding a dispatch on an inconclusive probe would be strictly worse than the
incident it exists for.

## 6. The diagnosis as an observable job

`backend/internal/workflow/incident_diagnosis_job.go`

An investigation was already durable and already independent of the UI. What it
lacked was STATUS: everything AO reported came from one row saying a launch had
been requested, and that row cannot tell "running" from "waiting for a person"
from "dead". A launch now also records the session it produced
(`incident_diagnosis_started`), and the job is derived from that session on every
read:

`queued` · `starting` · `running` · `waiting_for_provider` · `waiting_for_user` ·
`completed` · `failed`

with agent, provider, start time, elapsed seconds, last activity, last signal and
the blocking interaction when AO can tell. A diagnostic agent parked on a trust
prompt now reports `diagnosis_blocked` / `waiting_for_user` instead of
"Investigando". Exposed on the incident API as `diagnosisJob`.

## 7. Child-aware incident packs

`backend/internal/workflow/incident_child_pack.go`

When a parent's stop is `child_needs_attention` or `child_failed`, its incident
pack carries that child's own bounded evidence — reason and detail, steps, newest
attempt, session/provider state, recent checkpoints, reviewer verdict and
findings, verify result, workspace status and the newest provenance record — as a
priority-1 section that is never dropped. A parent stopped on a child has no
diagnosis to make without it, which is why the only answer used to be "diagnose
the child first", about a run whose id AO was already holding.

## 8. One notification per incident

`backend/internal/workflow/attention_notify.go`, `backend/internal/notify/`

The parent's mirror of a child's stop is the same real-world incident the child
already reported, and it is the half most likely to keep arriving because the
parent re-derives it on every reconcile pass. It no longer notifies. Stop
notifications now carry project, run and time, because a message read the next
morning has to be actionable on its own.
