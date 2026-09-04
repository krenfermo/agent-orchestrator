# P4-B — Users, teams and RBAC

P4-A answered *who authenticated*. This is the layer that answers *what may
they do*, and it is the only layer that answers it.

Everything below describes what is implemented on `p4b-users-teams-rbac`. Where
something is deliberately left for a later slice it says so.

## The one evaluator

`backend/internal/service/authz` is the single authorization authority.

```go
authz.Authorize(ctx, principal, permission, resource) error
```

There is no `if user.Role == "admin"` anywhere in a service or a controller.
A reviewer who wants to know what a viewer may do reads two maps in
`service/authz/roles.go` instead of grepping the tree, and a change to those
maps changes every transport at once.

The evaluator reads `domain.Principal` — P4-A's resolved identity — and nothing
about how that identity was established. That is what makes a password login,
an SSO login and a trusted-local desktop request carry exactly the same
authority for the same account (`TestAuthorityDoesNotDependOnLoginMethod`).
OIDC says who someone is; this says what they may do, and the two never
consult each other's inputs.

### What it resolves

Per request, once (see *Performance*):

| Source | Meaning |
| --- | --- |
| `users.role` | the installation-wide role |
| `projects.owner_user_id` | an implicit **admin** grant on that project — the 8P-A compatibility bridge |
| `project_grants` where subject is the user | a direct grant |
| `project_grants` where subject is one of the user's **active** teams | an inherited grant |

Several sources combine by **maximum** (the most generous grant wins — that is
what a person means by "I also added them to the team"), and the global role
then applies a **ceiling** (a viewer granted `admin` on a project is still a
viewer there).

## Permissions

Stable string identifiers, not booleans, because they are persisted in audit
records and returned to the frontend as capabilities. Every one corresponds to
a product surface that exists today; there is no permission for a feature AO
does not ship.

**Global scope** — the installation:
`project.create`, `provider.read`, `provider.manage`, `settings.read`,
`settings.manage`, `users.read`, `users.manage`, `teams.read`, `teams.manage`,
`audit.read`.

**Project scope** — one project and everything under it:
`project.read`, `project.manage`, `project.access.read`,
`project.access.manage`, `workflow.read`, `workflow.run`, `workflow.cancel`,
`workflow.repair`, `session.read`, `session.write`, `memory.read`,
`usage.read`.

`domain.ScopeOf` decides which scope a permission belongs to. A permission
belongs to exactly one: asking "may they read *the* settings of project X" when
settings are installation-wide is a question with no honest answer.

## Roles

### Installation roles (`users.role`)

| Role | Global permissions | Project access |
| --- | --- | --- |
| `owner` | all | admin on **every** project |
| `admin` | all | admin on **every** project |
| `member` | `project.create`, `provider.read`, `settings.read` | only what is granted |
| `viewer` | `provider.read`, `settings.read` | only what is granted, capped at **viewer** |

`owner` and `member` keep their exact persisted spelling from 8P-E.8. `admin`
and `viewer` are added by migration 0152; no existing value is renamed, moved
or reinterpreted.

An **administrator holds every global permission the owner does**. The
difference is not a permission — it is the owner-safety rule below: an
administrator may not demote, disable or take over the owner's account.
Expressing that as a missing permission would be wrong; an administrator
genuinely does manage users.

A **viewer cannot create a project**, because creating one would make it that
project's administrator — a read-only account that can mint write authority for
itself is not read-only.

An **unknown role** (a forward-migrated database, a hand-edited row) resolves
to viewer, never to admin.

### Project roles (`project_grants.role`)

| Role | May |
| --- | --- |
| `viewer` | read the project, its runs, sessions, memory and usage |
| `member` | that, plus run / cancel / repair work and drive sessions |
| `admin` | that, plus manage the project and its access list |

`ProjectRole` is a separate type from `UserRole` on purpose: "owner" is an
installation-wide singleton with no meaning inside a project, and letting the
two share a type is how a project grant ends up claiming installation ownership.

## Where it is enforced

Backend, always. Frontend visibility is convenience; §"Frontend" below.

1. **`controllers.GlobalAuthzMiddleware`** — one choke point ahead of every
   REST controller, gating the installation-wide route *families*
   (`/settings`, `/provider-profiles`, `/agents`, `/users`, `/teams`,
   `/environment`, `/runtime`, `/import`, `/dev`, plus the three
   project-*creation* paths). A family table rather than a per-route list, so
   a new `/settings/...` route inherits the rule instead of arriving unguarded.
2. **`controllers.Guard`** — the per-resource gate every project-, session- and
   run-scoped handler calls: `AllowProject`, `AllowSession`,
   `AllowWorkflowRun`. A session or run is authorized by way of **its project**.
3. **List filtering** — `/projects`, `/workflows`, `/sessions` and
   `/questions/pending` filter per row against the caller's resolved subject,
   so an inaccessible project never appears in a list.

`TestEveryAPIRouteHasAnAuthorizationDecision` walks the real router and fails
if a mounted `/api/v1` route is neither globally gated, nor project-scoped, nor
listed as unauthorized-by-design. An unguarded endpoint has to be a decision
somebody wrote down.

### Denial semantics

| Situation | Answer |
| --- | --- |
| No identity resolved at all | **401** `NOT_AUTHENTICATED` |
| Authenticated, lacks a global permission | **403** `FORBIDDEN` |
| Authenticated, lacks a permission on a project they **can read** | **403** — they can see it, so pretending it is missing would be a lie that costs an hour of debugging |
| Authenticated, lacks a permission on a project they **cannot read** | **404** — existence must not leak |

## Owner safety

The installation can never lock itself out.

- The owner **cannot be demoted**. Ownership moves by **transfer**: promoting
  another account demotes the sitting owner in the *same transaction*, so there
  is no moment — even under concurrent requests — when there is no owner.
  `Store.TransferOwnership` does both statements in one transaction, and
  `ux_users_single_owner` picks the winner of a race; the loser's demotion
  rolls back with its promotion.
- The owner **cannot be disabled**.
- **Only the owner may transfer ownership.** An administrator manages accounts;
  an administrator who could also seize the installation would make the owner
  role decorative.
- **No account can disable itself.**
- Ownership is **never assignable at creation** — only transferred to an
  existing account. There is no second path to two owners.
- An installation with **zero owners** (a legacy multi-user install) can appoint
  one: any actor with `users.manage` may, and a trusted-local request is treated
  as the owner until one exists (below).

## Trusted local

The primary loopback listener is unauthenticated by design (`AGENTS.md`), and
P4-B does not change that. Trusted-local mode still synthesizes the bootstrap
admin for a cookie-less request, exactly as 8P-A shipped.

Authorization then derives from **that account's actual role**, not from the
method — which is what keeps §27's cross-identity rule true. On the ordinary
single-user desktop the bootstrap admin *is* the owner (Bootstrap,
`RegisterFirstUser` and `EnsureOwnerExists` all guarantee that), so the desktop
keeps full authority and nothing about it changes.

The **one** exception is a recovery rule: a `trusted_local` principal on an
installation with **no owner at all** is treated as the owner. Refusing the
local operator on a box where they can already run `ao admin reset-password`
would lock the installation without protecting anything. The rule stops the
moment an owner exists — it is a recovery path, not a backdoor
(`TestTrustedLocalRecoversAnOwnerlessInstallation`).

Before the **first account exists**, authorization is not yet meaningful and
every route behaves exactly as it did pre-P4-B, so a fresh install reaches its
first-run screen. The check is latched after the first account (AO never
deletes the last one), so it costs one query per daemon lifetime.

## CLI

`ao` is a thin HTTP client and carries **no session cookie**. Its authorization
model is therefore the loopback one:

- On a normal (trusted-local) install it resolves to the bootstrap admin and
  keeps its existing authority — no CLI command changes behavior.
- In `AO_AUTH_MODE=oidc`, trusted-local synthesis is off by construction, so a
  cookie-less CLI request resolves no principal and a gated route answers 401.
  That is correct and deliberate: an installation that requires SSO has asked
  for exactly that. Issuing CLI tokens is a later slice; it is not smuggled in
  here.
- `ao admin reset-password` stays a loopback-only recovery tool, unchanged.

## Electron

No authorization logic lives in the main process. The renderer receives the
backend's own capability list from `GET /api/v1/auth/me` and renders from it.
The desktop's local owner is unaffected (above).

## Frontend

`MeResponse.permissions` is the contract: the installation-wide permissions the
backend says this identity holds. The renderer **never** derives authority from
`user.role` — a role-to-buttons map in React is a second authorization
implementation that can disagree with the real one.

- `useCan(permission)` / `useCanAny(...)` gate navigation and controls.
- Settings → **Users & teams** appears only with `users.read` or `teams.read`;
  its create/disable/role controls appear only with the matching `.manage`.
- Project settings → **Access** renders from `permissions` in the access
  response — the caller's effective permissions *on that project* — so a
  project administrator manages their own project without any installation-wide
  authority, and the panel degrades to read-only otherwise.

Hiding a control is convenience. Every route behind one is enforced again on
the way in; `TestP4BAdministrationSurfacesAreEnforcedServerSide` calls them
directly to prove it.

## Audit

`service/rbac` emits an event for every meaningful authorization change, with
the acting principal and the target:

`ao.authz.user.created`, `ao.authz.user.enabled`, `ao.authz.user.disabled`,
`ao.authz.user.role_changed`, `ao.authz.team.created`, `ao.authz.team.updated`,
`ao.authz.team.deleted`, `ao.authz.team.member_added`,
`ao.authz.team.member_removed`, `ao.authz.project.access_granted`,
`ao.authz.project.access_revoked`.

What is recorded is bounded on purpose: ids, roles and status values. No
password, no session token, no provider token, no email local part — the same
discipline `AuthAudit` follows for the identity events.

## Performance

One resolution per request. `authz.WithCache` is installed by a router
middleware; the first authorization question resolves the subject in **at most
three bounded, indexed queries** (owned projects, active teams, grants) and
every later question in that request is answered from memory. An administrator
resolves in **zero** grant queries — their access is universal, so loading
grants would scale with the installation and change no answer. List handlers
resolve once and decide each row from the project id already on it.

## Migrations

`0152_users_teams_rbac.sql` — the only migration this branch adds.

- rebuilds `users` to widen the role CHECK to `('owner','admin','member','viewer')`,
  preserving every row and recreating `ux_users_single_owner` unchanged;
- adds `teams`, `team_memberships`, `project_grants`.

## What P4-C extends

P4-C introduces organization/tenant scope. The shape that makes it cheap is
already here:

1. **`domain.AuthzScope` is an enum with a resource.** Authorization already
   takes a `(permission, scope, resource)` triple, so a tenant is a **third
   case** in `ScopeOf` and in `Subject.Allows` — not a rewrite of every call
   site. Nothing in the tree asks "which project" outside `AuthzResource`.
2. **`project_grants` is one table with a polymorphic subject.** A tenant
   column goes beside `project_id` there, and a tenant-scoped grant is a new
   `AuthzScope` value on the same row shape rather than a parallel table.
3. **`Subject` is the whole answer.** It already carries a universal role, a
   per-project map and a ceiling; a per-organization map is one more field and
   one more line in `ProjectRole`'s resolution.
4. **No speculative `tenant_id`.** Nothing carries a column every row would set
   to the same value, which would be a migration to undo rather than one to
   build on.

The one thing P4-C must decide that P4-B deliberately did not: whether the
installation-wide roles become per-organization roles, or stay installation-wide
above a new organization role. `users.role` is untouched by grants today, so
either answer is reachable without rewriting the evaluator.

## Known limitations

- **`team_memberships.role`** (`maintainer` / `member`) is stored and displayed
  but decides nothing yet: every team mutation is gated on the
  installation-wide `teams.manage`. Delegating team administration to a
  maintainer is the extension point the column exists for.
- **No email invitations.** AO has no mail delivery it can rely on until P4-D
  lands, and an "invite sent" button that silently sends nothing is worse than
  no button. Account creation is local (email + password) or federated; the
  extension point is `rbac.Service.CreateUser`.
- **No account deletion**, only disable. A disabled account keeps everything it
  owns and stops authenticating, which is the reversible operation; deleting an
  account that owns projects and runs is a data question, not an authorization
  one.
- **`audit.read` gates nothing yet.** The audit trail is real — `service/rbac`
  and `AuthAudit` both emit to it — but AO has no route that reads it back, so
  the permission is declared and held only by the owner and administrators,
  waiting for the reader it will gate. It is declared now rather than later so
  the role tables are decided once.
- **The LAN bridge carries no AO identity.** The "Connect Mobile" listener
  authenticates with a bearer password, not an AO session, so a LAN request
  resolves whatever the identity middleware resolves for a cookie-less one: the
  bootstrap admin in trusted-local mode (unchanged behavior), and no principal
  at all in `AO_AUTH_MODE=oidc`, where permission-gated routes then answer 401.
  Giving the bridge a real AO identity is its own slice.
- **Loopback surfaces stay unauthenticated.** `/events`, `/notifications`,
  `/browser`, `/decisions`, `/push`, `/shell-terminals`, `/capacity`,
  `/scheduler` and the hook callbacks are served as they always were on
  127.0.0.1. Narrowing them is a later slice with its own migration of desktop
  behavior, not a silent change here. They are listed explicitly in
  `authz_route_coverage_test.go` so the omission is a decision, not an oversight.
