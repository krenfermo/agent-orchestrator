# External work management: Plane (P4-E)

How AO links its work to an external planning system, what it writes there, and
the one rule the whole design rests on.

---

## 1. AO remains canonical

Plane is a **planning surface**, not a source of truth about execution. The
direction rule is enforced structurally rather than promised:

- There is exactly one function that maps an AO state to an external state
  (`domain.WorkItemSyncEventForRun` → `TargetStateGroup`). There is deliberately
  **no function anywhere** that maps an external state to an AO state. A reader
  looking for the reverse mapping finds its absence, not a helper somebody could
  reach for.
- Nothing in `internal/service/workitems` is called from the workflow engine,
  the lifecycle manager, or the reducer that decides a run's state. The only
  inbound path from execution is `Enqueue`, which writes a durable row and
  returns; it makes no network call and cannot fail a caller.
- The inbound poll (`Worker.refreshSnapshots`) writes exactly one thing —
  `TouchWorkItemLinkSnapshot` — which by construction can change a link's cached
  display title and state and nothing else. Not what the link points at, and
  certainly not any AO state.

What external state IS allowed to do is inform a person: `domain.ReadinessOf`
reports whether the plan says a piece of work is ready, deferred or done. It is
advisory, read only by human-facing surfaces, and never applied.

## 2. Which abstraction, and why not `ports.Tracker`

AO already had an issue-tracker abstraction. P4-E deliberately did not widen it,
and added `ports.WorkItems` beside it, because three things differ:

| | `ports.Tracker` | `ports.WorkItems` |
|---|---|---|
| direction | read-only by contract | read **and write** |
| shape | repository (`owner/repo#123`) | workspace + project + item |
| purpose | hydrate a prompt, enumerate intake | keep an external plan and AO's execution in step |

`ports.Tracker`'s own doc comment says richer per-provider behaviour belongs
behind a separate port, and the GitHub adapter states outright that it does not
write. Forcing Plane into it would have meant inventing a repository for
something that has none, and growing every existing adapter a write surface it
does not implement.

**What is reused rather than reinvented**: `domain.NormalizedIssueState` (the
same five words a GitHub issue is rendered in), the project-scoped authorization
model, `internal/secretbox` for the credential, the notification manager, and
the outbox/backoff patterns the rest of the tree already uses. Nothing here is
a second copy of something AO has.

A Plane implementation of `ports.Tracker` is **deliberately deferred**: its only
payoff is tracker intake — spawning workers from issues — and §6 is explicit
that AO must not act on external items automatically. Adding the read port
without wiring intake would be dead code; wiring intake was not asked for.

## 3. The Plane contract

Verified against Plane's published API reference and against the server source
that produces it (`makeplane/plane`, `apps/api/plane/api/`). Nothing below is
inferred from a plausible-looking URL.

```
Base URL   https://api.plane.so (cloud); a self-hosted origin otherwise.
           AO appends /api/v1 itself and strips a pasted one, because pasting
           the documented URL including its version prefix is the likeliest
           configuration mistake and /api/v1/api/v1 fails as a 404 that looks
           like a permissions problem.

Auth       X-API-Key: <token>. A personal access token from Profile Settings →
           Personal Access Tokens. Plane also accepts OAuth bearer tokens; AO
           implements the API-key path only, because that is the one an
           unattended daemon can hold without a browser round trip.

Paths      GET  /api/v1/workspaces/{slug}/projects/
           GET  /api/v1/workspaces/{slug}/projects/{project}/states/
           GET  /api/v1/workspaces/{slug}/projects/{project}/work-items/
           POST /api/v1/workspaces/{slug}/projects/{project}/work-items/
           GET|PATCH .../work-items/{id}/
           POST .../work-items/{id}/comments/
           GET  /api/v1/workspaces/{slug}/work-items/{PROJ}-{123}/
           Trailing slashes are required: Plane is a Django app and a request
           without one is redirected, silently dropping a POST body.

Ids        Both a UUID (every path takes it) and a human key built from the
           project's `identifier` and the item's `sequence_id` ("PROJ-123").
           AO stores both: the key breaks on a project rename, the UUID cannot
           be rendered without a fetch.

Paging     Cursor-based. Send per_page (max 100) and cursor; the envelope
           carries results, next_cursor, next_page_results, count, total_pages.

Limits     60 requests/minute per key on Plane Cloud, reported in
           X-RateLimit-Remaining and X-RateLimit-Reset (UTC epoch seconds).

States     A project defines its own states, each in one of six groups:
           backlog, unstarted, started, completed, cancelled, triage.

Externals  Work items AND comments carry external_source/external_id, indexed
           as a pair. A duplicate POST returns 409 with the existing id.
```

That last row is what makes AO's writes idempotent **without AO remembering
anything**: the provider itself deduplicates on a key AO derives from its own
scope and id.

## 4. Configuration

Per project, because two AO projects legitimately map to two Plane projects —
often in two workspaces and, under P4-C, two organizations. Environment
variables supply installation-wide defaults; a stored value always wins.

```
AO_PLANE_BASE_URL    origin, no /api/v1. Empty means Plane Cloud.
AO_PLANE_API_TOKEN   fallback token for projects that stored none.
AO_PLANE_WORKSPACE   default workspace slug.
AO_PLANE_PROJECT     default Plane project id.
```

The token is stored encrypted by `internal/secretbox` — the same mechanism and
the same key file as the SMTP password — so a copied `~/.ao/data` does not yield
a credential that can write to somebody's board. An installation that prefers
to keep the credential in the process environment can, and then the database
holds no secret at all.

**Enabling is never automatic.** An environment-only configuration still leaves
every project switched off: "AO writes to your planning board" is not a decision
an environment variable makes on somebody's behalf.

A configuration may be saved incomplete — somebody half-way through a form must
keep what they typed — and only **enabling** requires a workspace, a project and
a token. Clearing the credential while the connection is on is refused for the
same reason: an enabled connection with no credential is a promise AO cannot
keep.

## 5. Links

`work_item_links` stores identifiers only, never a title AO would have to keep
true. Three scopes, because they are linked for different reasons and have
different lifetimes:

- `project` — the standing mapping every other link resolves against.
- `run` — this execution delivers that planned work.
- `task` — the finest granularity a plan can be tracked at.

One AO thing links to at most one external item (a unique index on
`project_id, scope, scope_id`); re-linking replaces. Two links on one run would
make "which item does this run update" a question with two answers and post
every note twice.

`last_seen_title` / `last_seen_state` / `last_seen_at` are an explicitly-labelled
**display cache**: what the UI renders when the provider is unreachable, with an
age so nobody mistakes it for current.

**Links are never inferred from titles.** `FindByExternalID` uses Plane's own
`?external_id=&external_source=` filter, and there is no code path that compares
a title to anything.

## 6. Status mapping

Explicit, never string equality. The switch over `WorkflowRunState` is total, so
a new AO state forces a decision here rather than silently producing nothing.

| AO state | event | external state |
|---|---|---|
| running | started | → `started` |
| needs_attention | needs_attention | **unchanged** |
| completed | completed | → `completed` |
| failed | failed | **unchanged** |
| cancelled | cancelled | → `cancelled` |
| pending, waiting | *(none)* | — |

Two of those deserve their reasoning:

**needs_attention moves no state.** No external group means "a human must
decide": `started` would be a lie about progress and `cancelled` a lie about
intent. It posts a comment instead, which is the honest way to say something a
state machine cannot.

**A failed run does not cancel the item.** The work is still wanted and somebody
will look at it. Moving it to `cancelled` would delete it from the board's
active plan on AO's say-so — precisely the authority P4-E refuses to take.

Within a group, the concrete state is chosen deterministically (project default,
then lowest sequence, then name), so two AO instances driving one project agree
and a person can predict where work lands.

## 7. Comments

One short comment per milestone, rendered in one place (`commentFor`): a lead
sentence naming what happened, plus the caller's own single-line detail,
bounded at 400 characters. Never terminal output, never a transcript, never a
stack trace. Everything AO writes is HTML-escaped, because commit subjects and
stop reasons routinely contain characters that would otherwise close a tag.

The dedupe key travels in Plane's own comment `external_id`, so the same note is
never posted twice even across an AO restart that lost its outbox row.

## 8. Delivery: an outbox, not a call

AO's lifecycle observes a state change and writes a row. That row is:

- **durable** — a restart resumes it; the queue IS the checkpoint, which is why
  there is no separate cursor table.
- **uniquely keyed** on the real-world moment, so five observations of one
  completion enqueue once.
- **drained by a worker that may fail freely**, because nothing waits on it.

Retry is classified at the adapter, once, while the HTTP status is still in
scope. Auth, not-found and invalid are terminal (retrying a bad token is how an
account gets locked); rate-limited and unavailable retry with capped
exponential backoff to a hard ceiling of six attempts. An **unclassified** error
is treated as retryable: it is most often transport, and the attempt ceiling
bounds the cost of being wrong, while the opposite mistake silently drops the
event.

The backoff is a durable timestamp, not a sleep, so it survives a restart and a
single backed-off row does not hold up the rest of the queue.

## 9. Webhooks: why not

§9 prefers webhooks where they are clean. They are not, for AO specifically:

- AO's primary listener binds `127.0.0.1` and is unauthenticated by a hard rule
  in `AGENTS.md`. There is nowhere for a hosted Plane to deliver to without
  either exposing it or adding a second network-facing bind, which the same rule
  forbids.
- The one LAN listener AO may run is opt-in, plaintext, home-network-only, and
  explicitly not for control routes.
- A webhook needs a stable reachable URL and a per-installation shared secret.
  An unattended desktop daemon behind NAT has neither.

So the **outbound** direction — the one that matters, because AO's state is the
authoritative one — is event-driven and does not poll at all. The **inbound**
direction is a bounded 15-minute refresh of the display cache, which reconciles
nothing into AO.

## 10. Tenancy and authorization

Every table hangs off `project_id` with `ON DELETE CASCADE`, and **none carries
`tenant_id`**. That is P4-C's rule, not an omission: only `projects` and `teams`
carry a tenant, and everything reached through a project resolves tenancy from
`projects.tenant_id`. Migration 0156 states why — a denormalized copy that can
drift from the authority it copies is a second thing to keep true, not a safety
property.

So §10's isolation is the isolation the project already has, enforced by the
same `authz.Authorize()` call as everything else. A guessed project id answers
404 on every route, exactly as `/projects/{id}` does.

Three permissions, because they are three different authorities:

| permission | viewer | member | admin |
|---|---|---|---|
| `workitems.read` | ✓ | ✓ | ✓ |
| `workitems.link` | | ✓ | ✓ |
| `workitems.manage` | | | ✓ |

A member links the work they are doing; only a project admin decides which
external organization this project reports into and holds its credential.

## 11. Failure mode

Plane being down costs AO nothing:

| | effect |
|---|---|
| workflows | continue, unaffected |
| AO execution state | unchanged; nothing reads Plane |
| queued syncs | deferred with backoff, retried later |
| the links list | renders from cache, marked `stale`, with the reason |
| the settings panel | shows **Sync degraded**, never green, never blank |
| notifications | one, and only when a sync fails **permanently** |

No workflow ever becomes `needs_attention` because Plane is unavailable. There
is no code path by which it could: nothing in the workflow engine reads this
package.

## 12. Observability

`work_item_sync_audit` records the provider, the operation, the target item, the
outcome, the classified error kind, the attempt count and the duration. It never
records the token, a request header, or an untruncated provider body — error
messages are built from the adapter's own sanitised, truncated text, and the
credential is not in scope at any call site that writes a row.

`GET /projects/{id}/workitems/health` reports configured / enabled / connected /
degraded plus queue counts, and makes **no** provider call: a status endpoint
that probes is slow exactly when the thing it reports on is broken.

## 13. What AO deliberately does not do

- Create labels, or any other taxonomy, in somebody else's planning tool.
- Add or remove assignees.
- Touch cycles, modules, estimates or custom properties.
- Delete anything, ever — including on unlink, which forgets an association and
  leaves the item alone.
- Create items for internal repair, reviewer or recovery work. Item creation is
  explicit, per §6.

## 14. Status

**REAL E2E: BLOCKED BY ENVIRONMENT.** No Plane credentials, workspace or
configuration exist on this machine, and none of the `AO_PLANE_*` variables is
set. The adapter is covered by 20 contract tests against an httptest server
shaped like Plane's documented responses, and the lifecycle by 24 tests against
a real SQLite database. That proves AO behaves correctly *given* the contract;
it cannot prove Plane implements it, and this document does not claim otherwise.

To run the real check, set the four environment variables against a
**dedicated test project**, connect one AO project, link a run, and drive a
state change.
