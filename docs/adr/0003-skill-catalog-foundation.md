# 3. Skill catalog foundation: versioned manifests, explicit activation, fail-closed capabilities

Date: 2026-09-08
Status: Accepted. Phase 2 (2026-09-09) moved the registry into SQLite and added
the API, CLI and UI; **no runner exists, so nothing executes**. See the
"Phase 2" section at the end for what changed.

## Context

AO needs a Skills catalog: install a skill once, enable it explicitly on chosen
projects, and let it run only with the permissions somebody actually granted.
The first consumer is an on-demand security audit skill that a user points at
one project, in one mode, with an explicit scope — never an automatic audit on
every change.

`docs/skills/inventory.md` records what already exists. In short:

- `internal/skillassets` ships exactly one skill, clobbers it on every boot, and
  has no version, registry, per-project state or permission model — correct for
  a CLI reference guide, unusable as a catalog.
- `domain.Permission` is a complete, enforced RBAC vocabulary with role tables
  and an audit trail. This is the permission system; a second one would be a
  bug.
- `ports.LaunchConfig.AllowedTools`/`DisallowedTools` is the only tool boundary
  AO enforces today, it lives at the agent CLI, and it is void under
  `bypassPermissions`.
- **There is no sandbox and no egress control anywhere in the backend.** ADR
  0002 already hit this wall for reviewers and recorded it in its own status
  line rather than shipping a boundary that was not one.

The stabilization workstream is concurrently changing the planner, workflow
lifecycle, worker recovery, placement and SQLite migrations. This work must not
touch any of them.

## Decision

### 1. A new leaf package, `backend/internal/skillcatalog`

It imports `internal/domain` (for `Permission` and `ProjectID`) and nothing else
from AO. It adds no migration, no HTTP route, no CLI command, and no lifecycle
hook. It does not touch `skillassets`, which keeps `<dataDir>/skills/using-ao`;
the catalog owns the sibling `<dataDir>/skills/catalog`.

### 2. A versioned manifest, validated strictly

`skill.yaml` declares `apiVersion: ao.skill/v1`, id, name, semver version,
description, origin, risk level, requested capabilities, tool allow/deny,
scope (files, repos, network, secrets), inputs, an output JSON Schema,
authorization (AO permissions + approval mode), compatibility, integrity
digest, provenance, install/update policy, and one or more modes.

Decoding uses `KnownFields(true)`. An unknown key is an **error**, not an
ignored field: `capabilties: []` silently read as "declares nothing" is the
worst possible reading of a typo in a security-relevant key.

Three validation rules carry most of the weight:

- **`policy.autoEnable` must be false.** Installing is not enabling. The field
  exists so an author who tries it gets an error rather than believing an
  ignored key worked.
- **`provenance.signature` must be empty.** AO cannot verify signatures. A
  package that advertises an assurance nothing checks is worse than one that
  claims nothing.
- **A mode may only raise the approval bar, never lower it**, and its
  capabilities must be a subset of the skill's. This is what lets one package
  offer both a read-only review and an active scan without the review
  inheriting the scan's permissions.

### 3. Capability policy lives in code, not in the manifest

`capabilitySpecs` fixes, per capability: risk, minimum approval mode, whether it
requires an isolated runner, whether it requires egress control, and the
`domain.Permission` it is gated on. A package cannot describe its own capability
as cheaper than it is.

### 4. Install, enable and run are three separate acts

- **Install** copies a verified package into the catalog and records it. It
  enables nothing anywhere; a freshly installed skill is reachable by nothing.
- **Enable** activates one skill on one project, **pinned to one version**, with
  an explicit capability grant and a named approver. Grants are checked against
  the approver's own permissions, so a grant can never be broader than the
  person who made it. There is no "latest" activation target: an install must
  not silently change what a project already approved.
- **Run** re-resolves everything and authorizes fail-closed.

`Disable` drops the grant rather than parking it, so re-enabling means granting
again. `Uninstall` refuses while any project still has that version enabled.

### 5. Authorization is fail-closed, and one denial fails the whole run

`Authorize` denies unless *every* check passes: granted by the project, gated
permission held by the principal, approval mode at least the capability's floor,
runner attests the required isolation, runner attests egress control, and
per-target capabilities have an explicitly authorized target. Any single denial
clears the granted set entirely — a half-authorized run is a run whose declared
scope no longer describes it.

### 6. The runner is an interface, and the shipped implementation refuses

`Runner` is declared so the contract is fixed before anything can execute.
`PlanRun` resolves, validates and authorizes a run **without running it**, which
is genuinely useful on its own: a UI can show exactly which capabilities a run
would need and why each missing one is missing.

The only `Runner` this phase ships is `UnavailableRunner`, which attests nothing
and returns `ErrNoRunner`. `NoRunner()` is the attestation AO can honestly make
today.

## Consequences

### What this actually buys, today

Read-only modes (`static-code`, `secret-scan`, `authz-review`) authorize and
plan now. Every capability needing containment — `repo.write`, `process.exec`,
`secrets.read`, `net.egress`, `net.active_scan` — is **refused**, with a reason
naming what is missing. The security-audit tests demonstrate the same mode
refused on two different projects for two different correct reasons: one lacks
the grant, the other lacks the runner.

### What is explicitly not claimed

**A manifest is not a security boundary, and neither is a prompt.**
`scope.files`, `scope.network` and `tools` describe intent. Nothing in this
package enforces them. The enforcement points are, in order of how real they
are today:

1. `ports.LaunchConfig.AllowedTools`/`DisallowedTools` — real, at the agent CLI,
   and void under `bypassPermissions`.
2. A container or equivalent with a filesystem view and a default-deny egress
   policy — **does not exist**.

`Authorize` trusts exactly one value for containment: `RunnerAttestation`. A
runner that returns `Isolated: true` without a real boundary defeats every check
in the package. That is stated in the `Runner` doc comment, and any
implementation must re-run `Authorize` against its own attestation rather than
trust a `Plan` built elsewhere.

### Why file-backed, not SQLite

The registry is one JSON file under the data dir, written atomically. This phase
adds no migration because the stabilization workstream is actively changing the
schema, and a catalog that cannot ship without a migration cannot ship now.
Moving it into SQLite is a deliberate later step (roadmap subfase 5), and the
registry's shape was chosen to map onto two tables cleanly.

*(Phase 2 did exactly that — see below. `skillcatalog.Registry` remains, and is
still the file-backed implementation the package's own tests exercise.)*

### Costs accepted

- No concurrency control on the registry file. It is safe for the single-writer
  daemon it will eventually live in; a second writer would need locking or the
  SQLite move.
- Package versions are stored side by side and never garbage collected.
- Capability specs are compiled in, so changing one is a code change and a
  release. That is the intent.

## Alternatives rejected

**Extend `skillassets`.** Its contract is clobber-on-boot with the binary as the
version. Adding a registry to it would break the property that makes it
correct.

**Put activation in `ProjectConfig`.** No migration needed (it is a JSON blob),
but it would edit a domain file the stabilization workstream may be touching,
and its own rule is "only fields with a live consumer" — there is no consumer
until the catalog is wired.

**Ship a runner now.** Any runner that could be built without touching lifecycle
would execute in the daemon's process or a worker worktree. Calling that a
sandbox would make every check in this package cosmetic, which is precisely the
failure ADR 0002 refused for reviewers.


---

## Phase 2 (2026-09-09): durable catalog, administrative surface, dry run

Everything above stands. This section records what changed and the three
decisions that were not implied by the original ADR.

### Persistence moved to SQLite

Migration 0161 adds `skill_installs`, `skill_activations` and `skill_audit`.
The package FILES stay on disk under `<dataDir>/skills/catalog/packages`; the
row carries the manifest and the digest verified at install, and resolution
re-reads the files and re-verifies the digest, so a package edited underneath AO
fails closed rather than resolving from a stale row.

`skill_activations` carries a composite foreign key to `(skill_id, version)`, so
"an activation cannot outlive the exact version it pinned" is an invariant of
the schema and not only of the service that usually enforces it.

`internal/service/skills` sequences verify → write → audit. It re-implements no
rule: `skillcatalog.ValidateGrant`, `skillcatalog.Authorize` and
`skillcatalog.ValidateInputs` are exported and shared with the file-backed
`Registry`, because a second copy of the capability table is how the two would
come to disagree.

### Installs are installation-wide; activations are per project

A package is an artifact an administrator vetted once, and reach is granted per
project on top of it. Tenant isolation therefore rides on project access — every
activation route resolves through the same project authorization as the rest of
AO — so there is deliberately no `tenant_id` column. A second scope here would
be a second answer to a question projects already answer. (This closes open
question 5 in the roadmap.)

### Three authorization decisions

**`/skills` joins the global rule table under `settings.read` /
`settings.manage`.** Installing a package puts code on this host that projects
can then be granted reach with, so it sits with settings rather than with any one
project. `/projects/{id}/skills` is gated per project in the controller, exactly
as `/projects/{id}/access` is, so a project administrator activates a skill on
their own project without holding installation authority.

**The audit trail is gated on `audit.read`, stricter than its family's
`settings.read` floor.** The trail names actors and carries the host path each
package came from, and a member holds `settings.read`. This gives
`domain.PermAuditRead` the enforced consumer its own doc comment says it has
been waiting for.

**A disabled guard yields the full permission vocabulary, not an empty set.**
On the default single-user desktop AGENTS.md keeps the loopback listener
unauthenticated and trusted, and `Guard.Subject` resolves nothing there. An
empty set would refuse every grant and block every dry run — a new, silent trust
boundary on the one listener AO deliberately does not gate, and a feature broken
on the default install. When the guard IS enabled, an unresolvable subject
yields nothing, which is the fail-closed answer for a multi-user install.

### The dry run

`POST /api/v1/projects/{id}/skills/{skillId}/dry-run` resolves, authorizes and
reports — skill and version, mode, which capabilities are satisfied, which
permissions the caller lacks by name, what approval is outstanding, and what the
runner does and does not provide — then answers `executable`,
`requires_approval` or `blocked`. It starts no process, opens no socket and
writes no row; a test asserts that by snapshotting the catalog files, the audit
and the activations across four dry runs.

It takes **no attestation from its caller**. A self-declared `Isolated: true` is
exactly the claim this design refuses to accept as proof, so the only
attestation this surface uses is the one AO can make truthfully, which is
`NoRunner()`. Read-only modes come back executable; every capability needing
containment comes back blocked with the reason.

`CapabilityDecision.Satisfied` is per-capability diagnostics, not the verdict.
Authorization stays fail-closed — one unsatisfied capability refuses the whole
run — but reporting the others as unsatisfied would tell a user to grant
something that is not the problem. `Verdict` is the field that answers "can this
run".

### Still not claimed

No runner. Nothing executes. Everything under "What is explicitly not claimed"
above is unchanged, and subfase 3 of `docs/skills/roadmap.md` remains the gate
on every executable capability.
