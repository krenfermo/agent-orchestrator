# Project memory: authority, provenance and drift (P2-D)

P2-A built the durable memory store. P2-B put it on the normal execution path.
P2-C made one task's knowledge reusable by the next. Each of those made memory
**more available**. None of them made AO able to prove, at read time, that a
fact it is about to hand an agent is still vouched for.

P2-D is that proof. Its one sentence is:

> **The repository and AO's own durable workflow facts are the source of truth.
> Project memory is derived. If AO cannot demonstrate that an item is still in
> force, it is not served as authoritative.**

Fail closed for authority. Fall back to the repository or to legacy context
wherever that is possible — which it almost always is, because memory is an
optimisation and never a dependency.

---

## 1. The pipeline

```
   repository + durable workflow facts        <- source of truth
                 |
                 v
            provenance                        <- what AO observed, recorded once
      (workflow_mutation_provenance,
       source paths / digests / commits,
       repository identity, generation)
                 |
                 v
          derived memory                      <- project_memory_items / _relations
                 |
                 v
       authority validation                   <- two axes, ANDed: state x authority
        drift.go  +  validate.go
                 |
                 v
         context selection                    <- pack.go filterServable, durable_memory.go
                 |
                 v
              role                            <- planner / worker / reviewer / repair
```

Nothing flows upward. A repository file that claims "this memory is
authoritative" is repository content, which is untrusted data; only AO's own
durable rows grant authority (§9).

---

## 2. The two axes

A fact carries two independent columns, and **serving requires both**:

| axis | column | question | code |
| --- | --- | --- | --- |
| evidence | `state` | do this fact's sources still look the way they did? | `drift.go` |
| licence | `authority` | is what made this the project's knowledge still on record? | `validate.go` |

```go
func (i ProjectMemoryItem) Servable() bool {
    return i.State.Authoritative() && i.Authority.Provable()
}
```

They are separate columns because they move independently and are repaired
differently. A decision whose files nobody has touched (`state = valid`) loses
its licence the moment the integration that promoted it turns out never to have
happened. A module summary whose files changed (`state = stale`) keeps a
perfectly good promotion authority an operator still needs to see. One column
with one vocabulary would give "why is this not being served" a single slot for
two answers.

**Every value other than the default withholds.** A future authority value, or
one written by a newer build and read by an older one, fails `Provable()` and is
withheld rather than served — fail-closed is the default case, not a branch
somebody has to remember to write.

### `state` (P2-A, unchanged)

`valid` · `stale` · `invalidated` · `rebuilding`

### `authority` (P2-D, migration 0146)

| value | meaning |
| --- | --- |
| `authoritative` | every proof this fact's provenance kind requires was checked and held. The only value that is served. |
| `unprovable` | a proof it needs is missing, broken, or contradicted. Kept, never served. |
| `legacy_unprovable` | written before P2-D and carries none of this model's provenance. Withheld, and offered a bounded rebuild. |

`unprovable` and `legacy_unprovable` are separate because they want different
operator responses. An unprovable fact is one whose proof **broke** and is worth
investigating; a legacy fact never had one, which is a migration rather than an
incident.

The P2-D brief's wider vocabulary maps onto existing fields rather than onto new
ones:

| brief | how AO expresses it |
| --- | --- |
| `authoritative` | `state = valid` and `authority = authoritative` |
| `provisionally_valid` | `origin = task_local` (plus its `knowledgeShare`) — see [project-memory.md §7](project-memory.md) |
| `stale` / `invalidated` / `rebuilding` | `state` |
| `conflicting` | `knowledgeStatus = conflicting` metadata (P2-C) |
| `unprovable` | `authority` |

### Reason classes

Every non-authoritative row carries a class-prefixed reason, so an operator
surface can group by cause without parsing prose:

`memory_provenance_missing` · `memory_source_drift` ·
`memory_generation_stale` · `memory_repo_identity_changed` ·
`memory_promotion_unprovable` · `memory_legacy_no_provenance` ·
`memory_superseded_source_changed`

---

## 3. Provenance

Each fact records **which proof applies to it** (`provenance_kind`) rather than
leaving a validator to infer it from the item type. Two decisions — one an
indexer lifted out of a document, one a task recorded — are the same type and
have entirely different things to prove.

| `provenance_kind` | what must hold |
| --- | --- |
| `repo_derivation` | repository identity compatible; a source commit or at least one source path to be checked against; digests still matching (drift's department) |
| `task_outcome` | repository identity compatible; if `origin = canonical`, a `promotion_authority` row and an `integrated_commit` |
| `workflow_knowledge` | as `task_outcome` — a decision or risk lifted out of a durable workflow row |
| `legacy` / empty | nothing can be proven; withheld |

Alongside it every fact carries `repo_identity`, `promotion_authority`,
`verified_commit` and `integrated_commit`. The last three commits are kept apart
on purpose (§5).

---

## 4. `workflow_mutation_provenance` — no longer deferred

Migration 0133 created the table. Until P2-D it had a schema, a store method and
**zero rows in production**: every "which workflow/task produced this change"
question was answered by re-reading `workflow_checkpoints.retry_state` JSON.

P2-D adds the production writer (`internal/workflow/mutation_provenance.go`) and
the columns a **memory promotion** needs, which are more than the verification
path ever did (migration 0146): `project_id`, `repo_identity`, `repo_path`,
`placement`, `boundary`, `generation`, the three `integration_target_*` columns,
`integration_method`, and `idempotency_key`.

### Mutation boundaries

AO records **boundaries, not writes**. There are five, because there are five
moments at which the answer to "may this become project knowledge" can change:

| boundary | when | what it licenses |
| --- | --- | --- |
| `dispatch` | the tree and head AO handed an agent | attribution of a later difference |
| `work_result` | what a worker produced | nothing on its own |
| `repair_result` | what a repair agent produced on top of it | nothing on its own; keeps the repair visible (§7) |
| `verified` | the head verification passed on | direct-branch promotion (§5) |
| `integrated` | the target ref moved | isolated-worktree promotion (§6) |

Written at exactly three call sites
(`internal/workflow/mutation_boundaries.go`):

- `completeVerifiedRun`, after the autonomous local commit and while the branch
  lock is still held — the first instant the work is both good and durable.
  Writes `verified`, plus either `work_result` or `repair_result`.
- the same place, when a fix step ran (§7).
- `handleIntegrationOutcome`, the single point where **both** execution modes
  agree the target ref moved. Writing it once, from the Integration
  Coordinator's own `Record`, is what stops the two modes ever disagreeing about
  whether a task integrated.

Evidence is written **before** the decision it supports, everywhere. A crash
between them leaves AO holding proof of an integration whose promotion was not
recorded — which a later pass can finish — rather than a promotion whose
integration it cannot prove.

### Exactly once, and generation-safe

- **Exactly once** is a property of the boundary, not of the call.
  `domain.MutationIdempotencyKey` is derived from `(run, task, boundary,
  generation, head SHA, integration target SHA)` — facts of the moment, never of
  the writing, so no clock and no row id participate. A duplicate completion
  callback and a daemon that died mid-write derive the same key, and the partial
  unique index in 0146 collapses them to one row. A writer that honestly cannot
  identify a boundary leaves the key empty, and empty keys never collide with
  each other.
- **Generation-safe**: before writing, the writer reads the newest durable row
  for the same `(task, boundary)` and refuses if that row is at a newer
  generation. The generation is the number of durable attempts on the step —
  durable, monotonic, and stable under duplicate delivery. A refusal is not an
  error; a stale callback is a normal event.

---

## 5. Direct-branch promotion proof

Before P2-D, direct-branch work was canonical because the project's execution
**mode** said direct branch. That is a statement about configuration, and it
stays true if the branch was force-moved, if the commit AO stamped came from an
unrelated checkpoint, or if the repository at that path has since been replaced.

`directBranchPromotionProof` now requires five things
(`internal/workflow/task_promotion_proof.go`):

1. The placement really is direct branch, read through the same resolver the
   dispatch path uses.
2. AO durably recorded a `verified` boundary for this task — not "a commit
   exists" but "AO wrote down that verification passed at this head".
3. The repository identity is compatible (§8).
4. The verified head is **still reachable from HEAD**. Ancestry, not equality: a
   branch growing on top of the verified commit is normal and safe, and
   requiring equality would refuse every healthy repository where anything else
   happened after the task. A verified commit that has *vanished* from the
   history is a rewrite, and knowledge pinned to it describes work the
   repository no longer has.
5. The generation is current.

Direct-branch work records `verified_commit == integrated_commit`, with
`integration_method = direct_commit`. Saying "there was no separate integration"
explicitly is different from leaving the field empty, which would read as "the
integration was not recorded".

---

## 6. Worktree integration proof

**A verified worktree result is not canonical.** This is the sharpest rule in
P2-D, and before it a non-empty SHA argument was the whole proof.

`worktreePromotionProof` requires a durable `integrated` boundary for the task,
whose `integration_target_after_sha` is non-empty, whose repository identity is
compatible, and — where the method allows it — whose source commit is still
reachable from the integrated target head.

The method decides which proof applies:

| `integration_method` | strategies | ancestry proves it? |
| --- | --- | --- |
| `fast_forward` | `fast_forward`, `rebase_fast_forward` | yes |
| `merge` | `merge_commit` | yes |
| `direct_commit` | `no_op` (direct branch) | yes |
| `cherry_pick` | `cherry_pick` | **no** — same content, different SHAs |
| unrecognised | anything a newer build writes | no |

Cherry-pick is proven by the recorded target SHAs, which the Integration
Coordinator observed under its lane. **"The files look the same" is never
proof** and no code path treats it as such.

The caller's `integratedSHA` is *checked against* the durable row rather than
trusted. When the two disagree, one of them describes a different integration,
so the promotion is refused rather than silently preferring the row.

---

## 7. Repair provenance

When a fix step ran, the verified head is the **repair's** output. AO records a
`repair_result` boundary with class `AUTHORIZED_FIX` and does **not** also
record a `work_result`, so nothing later attributes the final change to the
original worker.

The `verified` boundary is written either way, because it answers a different
question — not "who produced this head" but "what did verification pass on".
Memory pins its `verified_commit` to that row and to nothing else.

---

## 8. Repository identity

`ProjectMemoryRepoID` is `sha256(canonical absolute path)`. That is what a memory
row is **addressed** by, and deliberately not what it is **identified** by,
because a path answers neither question integrity needs:

- the same repository, moved to a new path → memory should follow it;
- a **different** repository at the old path → memory must **not** be inherited.

A path-derived id gets the first wrong safely (the moved checkout looks
unfamiliar and is re-indexed) and the second wrong dangerously.

`domain.RepoIdentity` (`internal/projectmemory/git.go: RepoIdentityOf`) is
derived, in descending order of authority:

1. the first remote URL, normalized (scheme, credentials, port, trailing `.git`
   and host case stripped — nothing else, because a transformation that guessed
   would *merge* two repositories);
2. failing that, the **root commit** of the current history — the oldest
   parentless commit, so grafting an unrelated history on later does not change
   it;
3. failing both, the empty string, which never matches anything including
   another empty string.

`RepoIdentityCompatible(recorded, observed)`:

| recorded | observed | compatible |
| --- | --- | --- |
| known | known, equal | yes |
| known | known, different | **no** — the dangerous case |
| known | unknown | no |
| unknown | known | no |
| unknown | unknown | **yes** |

The last row is a stated concession. A project that is not a git checkout has no
durable identity, never had one, and never will; refusing it would make
canonical memory impossible for every non-git project permanently, in exchange
for detecting a substitution AO has no signal for either way. Every row that
*can* indicate a substitution refuses, so the dangerous case is caught whenever
it is detectable at all.

The identity sweep
(`MarkProjectMemoryItemsUnprovableByRepoIdentity`) is unlike every other
invalidation: it is not path-scoped (no individual file is wrong — the premise
is), and it is not generation-conditioned (it is a fact about the checkout that
holds for every generation at once). It **refuses to run** when the observed
identity is unknown, because turning "AO could not tell today" into "this
project's memory is gone" would be the worst possible reading of a missing fact.

---

## 9. Drift v2

Drift detection recomputes source digests and can only ever demote (P2-A's
asymmetry, unchanged). P2-D adds:

| case | behaviour |
| --- | --- |
| file modified | `stale`, scoped to the items derived from that path |
| file deleted | `invalidated`, and the graph edges naming those facts are retired |
| **file renamed, proven by git** | knowledge is **carried**: task results, decisions and known risks have their evidence re-anchored on the new path (see below) |
| rename without proof (delete + add) | old invalidated, new derived — no semantic rename is guessed |
| branch force-moved / verified head unreachable | promotion refused (`memory_source_drift`) |
| repository replaced at the same path | every fact withheld (§8) |
| source path escaping the repository root | treated as missing — stored provenance is data (§10) |

### Renames carry knowledge

A repository derivation is re-derived from the file at its new path, so
retiring it loses nothing. A **decision or a risk** is different: nothing will
ever re-derive it, so retiring it because git moved a file would silently delete
the project's own reasoning.

So on a rename git *proved* (`PreviousPath` set from an `R` status — never
inferred from a delete plus an add), the knowledge anchored on the old path is
read before anything retires it and re-anchored after the new path's own
invalidation. The ordering is forced: a rewrite done at either point alone would
immediately be undone by the other.

It rewrites **evidence, never content**. The decision still says what it said;
only where AO will look to check it moves. The digest is *not* carried over —
git reports a rename at any similarity above its threshold, so keeping the old
digest would let drift compare the new file against a hash of something else.
The item becomes unverifiable-but-present, which is how these types are treated
everywhere else.

---

## 10. Supersession and conflict integrity

A decision B that supersedes A retires A. If B later becomes unprovable or
stale, **A does not come back**. A was retired because the project changed its
mind, and B losing its licence is not evidence that it changed back. The result
is *no current authoritative decision on that subject* until a revalidation
produces one. This is a required test
(`TestStaleDecisionDoesNotResurrectItsPredecessor`).

Conflicts are resolved by authority and provenance, never by "latest timestamp
wins". Two knowledge items may be superseded, concurrently conflicting,
branch-local alternatives, or stale-vs-current. A conflict AO could not order
reaches the **Planner** explicitly labelled `CONFLICTING —` (it is information
about the memory, and the planner's job is to decide what to do about it) and is
withheld from Worker, Reviewer and Repair, which must never receive an arbitrary
answer as current.

---

## 11. Invalidate vs revalidate vs rebuild

| verb | meaning | cost |
| --- | --- | --- |
| **invalidate** | the licence or the evidence is gone; withhold | one write |
| **revalidate** | the proof still checks out; refresh the record of it without re-deriving content | one write |
| **rebuild** | derive the fact again from scratch | a pass |

A full rebuild is deliberately **not** the generic answer to an integrity
question: an integrity check whose only repair is a full rebuild is one nobody
runs.

**Reconfirmation is not revalidation.** A pass that finds a fact's content
unchanged refreshes its provenance and generation and does **not** hand back a
withheld licence — "the evidence still looks the same" was never why the fact
was withheld. Only a promotion or a real re-derivation re-establishes one.

The validation pass only ever demotes. There is no input to `validate.go` that
makes a withheld fact authoritative as a side effect.

---

## 12. Context manifests

P2-C recorded which facts an execution was told. P2-D records **which version**
of them: `item_versions_json` holds each item's `content_hash` at selection
time, positionally aligned with `item_ids_json` (padded with `""` rather than
shifted, so position never lies), plus `role_head_sha` — the commit the role was
reasoning about.

For a reviewer that is the reviewed SHA, carried on the context by
`projectmemory.WithRoleHead` (the same channel the P2-C sharing entitlement
uses, so no launch port is widened). It is recorded and used for **nothing
else**: memory selection is deliberately not narrowed by it, because a reviewer
given a different pack from the worker whose work it reviews is the P2-C §7
problem. What it buys is that "the pack was built for SHA A and the reviewer
judged SHA B" becomes a diagnosable fact.

Manifests are reproducible: same authorities, same manifest. An item invalidated
between two dispatches changes the pack digest, which changes the manifest id,
so a new manifest row appears rather than a historical one being rewritten.

---

## 13. Legacy memory

Rows written before migration 0146 have `provenance_kind = ''` — not because
anything went wrong, but because the column did not exist. They are:

- **classified**, once per repository, as `legacy_unprovable` with reason
  `memory_legacy_no_provenance`;
- **withheld** from every role;
- **never deleted**, and **never given fabricated provenance**;
- **recoverable** by `ao memory rebuild`, which re-derives them with real
  provenance.

The classification is guarded on `authority` still being the default, so a row a
later validation pass has already ruled on is not dragged back to legacy by a
second sweep.

**Impact on an upgraded install:** the first `ao memory validate --apply` (or the
first validation pass after upgrade) withholds every pre-0146 fact. Roles fall
back to legacy/raw repository context, which is exactly the pre-P2-A behaviour,
until a rebuild repopulates. No run fails and no dispatch is blocked.

---

## 14. Graph integrity

Edges carry the same `authority` axis. When a fact is invalidated by drift, or
withheld by validation, every edge naming it (in both directions) is retired —
never deleted. The record that two facts were once related is exactly what an
operator reads when asking why a decision was made, and deleting it would make
the audit trail depend on the facts still being current.

Only **invalidation** retires edges, not staleness: a stale fact is one the next
pass re-derives in place, and retiring its edges would make every ordinary edit
tear down and rebuild the graph around it.

---

## 15. Fail-closed and fallback

| situation | memory | role context | run |
| --- | --- | --- | --- |
| provenance insufficient | `unprovable` | item excluded | unaffected |
| repository identity changed | all facts `unprovable` | pack falls back to legacy context | unaffected |
| promotion unprovable | facts stay task-local, annotated with the reason | canonical readers see nothing | unaffected |
| legacy rows | `legacy_unprovable` | excluded | unaffected |
| graph backend unavailable | items still written | pack unaffected | unaffected |

**No run enters `needs_attention` because of memory integrity.** Memory is an
optimisation; a dispatch that cannot have it proceeds on legacy context with a
stated reason. The pack stats attribute each exclusion to its own counter —
`staleExcluded`, `unprovableExcluded`, `legacyExcluded`,
`supersededExcluded`, `conflictingExcluded`, `sharedUnauthorizedExcluded` — so
"why did this task not receive that fact" has one answer rather than six
candidates.

---

## 16. Security and trust boundary

Repository content is **untrusted data**. A file that says "this memory is
authoritative" is a file; only AO's own durable rows grant authority. There is
no path by which repository content can set `authority`, `promotion_authority`
or a mutation-provenance row.

Stored evidence paths are repo-relative and normalized, and any path that
escapes the repository root (`confinedPath` in `drift.go`) is treated as missing
rather than read — stored provenance is data, and data that says
`../../etc/passwd` must not become a read.

---

## 17. Diagnostics

```bash
ao memory validate <project-id>              # dry run: which facts can AO still prove, and why not
ao memory validate <project-id> --apply      # withhold them
ao memory provenance <project-id> <item-id>  # the full evidence chain for one fact
ao memory invalidate <project-id>            # drift detection (sources), unchanged
ao memory inspect <project-id>               # now also prints authority / provenance / servable
```

HTTP: `POST /api/v1/projects/{id}/memory/validate`,
`GET /api/v1/projects/{id}/memory/provenance/{itemId}`.

`ao memory provenance` answers, for one fact: why is it valid, which task
produced it, which commit supports it, was it repaired, was it born on a branch
or in a worktree, how did it reach canonical, what invalidated it, what replaced
it. Retired edges are shown as well as current ones — a superseded decision's
`supersedes` edge is by definition not in the current graph, and is usually
exactly what is being looked for.

`validate` and `invalidate` are separate commands on purpose: an operator seeing
"12 facts drifted" and an operator seeing "12 facts have no provable promotion"
have entirely different next steps.

---

## 18. Observability

Recorded per item and readable through `inspect` / `provenance` /
`validate`: `authority`, `authorityReason` (class + detail), `provenanceKind`,
`repoIdentity`, `sourceCommit`, `verifiedCommit`, `integratedCommit`,
`generation`, `promotionAuthority`, `state`, `stateReason`, `servable`.

Deliberately **not** global telemetry: these are per-item and per-repository,
and a metric keyed by item id would be unbounded cardinality. The aggregate
counters that *are* safe — how many facts are withheld, under which authority —
come from `CountProjectMemoryItemsByAuthority` and from the pack stats above.

---

## 19. Graphify / Grae readiness

Unchanged from P2-A in one respect: **AO's own store remains canonical**, and an
external graph is a second implementation of the `MemoryGraph` port, never a
replacement.

What P2-D adds is the list of what a real adapter must transport, because an
adapter that carried nodes and edges alone would be exporting facts stripped of
the thing that makes them safe:

- **node identity** — the derived item id, not the natural key (a task-local and
  a canonical row of the same key are different rows);
- **source authority** — `provenance_kind`, `repo_identity`,
  `promotion_authority`;
- **generation** — so a stale writer on the far side cannot overwrite newer
  state;
- **validity** — both axes, `state` and `authority`, with their reasons;
- **supersession** — the retired edges, not only the live ones;
- **evidence** — source paths, source commit, source digest.

An adapter that cannot carry all six must be read-only. Graphify must never
become a source of truth: a fact that exists only in the external graph is a
fact AO cannot disprove, which is precisely what this whole model exists to
prevent.

---

## 20. Retention

| population | kept | GC |
| --- | --- | --- |
| active (`valid` + `authoritative`) | forever | never |
| stale / invalidated | until superseded by a re-derivation of the same identity | compaction bounds (P2-A §5, P2-C) |
| superseded / resolved knowledge | bounded by `MaxRetainedTaskResults` and the per-scope bound | oldest retired, never deleted while it explains an active decision |
| `unprovable` / `legacy_unprovable` | until rebuilt | never auto-deleted |
| mutation provenance | append-only | never deleted — it is the evidence chain a promotion rests on |

**GC never removes the last evidence chain needed to explain an active
decision.** A `supersedes` edge whose target is retired is what answers "why is
the project not doing X any more", and it outlives the fact it points at.

---

## 21. Known limitations

1. **Validation is a separate pass, not part of every dispatch.** The warm path
   is unchanged: a dispatch on an unmoved repository still costs one indexed row
   read and no filesystem I/O. Authority is checked when a fact is *written* and
   when `validate` runs, not on every read — a per-read proof would turn every
   warm task into a repository scan, which §33 of the brief forbids.
2. **The `dispatch` boundary is defined but not yet written.** The three writers
   P2-D adds are `work_result`/`repair_result`, `verified` and `integrated` —
   the boundaries a promotion proof consumes. `dispatch` remains served by the
   existing `workflow_dispatch_checkpoints` row.
3. **Cherry-pick promotion rests on the recorded target SHAs alone.** That is
   the correct proof for it (§6), but it means a cherry-pick record whose target
   ref is later rewritten is not detected by ancestry — only by the repository
   identity check and by drift on the affected files.
4. **Repository identity is unavailable for non-git projects**, and two
   unidentifiable checkouts at one path are treated as compatible (§8). Stated
   rather than hidden.
5. **Legacy rows are withheld in bulk on first validation** (§13). The
   remediation is a rebuild; there is no incremental path that proves a legacy
   row without re-deriving it, because there is nothing to prove it *from*.

---

## See also

- [project-memory.md](project-memory.md) — the P2-A model, storage and packs.
- [project-memory-optimization.md](project-memory-optimization.md) — P2-B's
  lifecycle trigger and the measured before/after.
- migration `0146_memory_provenance_authority.sql` — the schema, with the
  reasoning for every column.
