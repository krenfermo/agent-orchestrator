# The Skills Registry / Marketplace

**Status: phase 10 foundation. An administrator can configure a registry,
search it, review a release, and install one exact version. Nothing that
executes was added.**

This document is the operator's view. The decision behind it is
[`ADR 0006`](../adr/0006-skill-registry-trust.md); the roadmap entry it closes
is open question 6 in [`roadmap.md`](roadmap.md).

---

## The four states, and why they stay apart

| State | What it means | What moves you to it | Permission |
| --- | --- | --- | --- |
| **AVAILABLE** | A configured registry offers it. No bytes on this host. | Configuring a registry. | `settings.read` to see |
| **INSTALLED** | Verified bytes in AO's catalog. Reachable by nothing. | `Install`, on one exact version. | `settings.manage` |
| **ENABLED** | One project pinned one version, with an explicit capability grant. Runs nothing. | Project Settings → Skills → Enable. | `project.manage` |
| **EXECUTABLE** | A mode whose every required control is attested, with an approved image digest for the exact scope. | An image approval, plus a usable container runtime. | `settings.manage` for the image |

Nothing in this phase moves an artifact more than one step. Installing enables
no project, grants no capability, approves no image, and runs no publisher
hook, script or shell. There is no Run button on the Marketplace, no `run` verb
under `ao skills marketplace`, and no route that would give a client one.

## The trust model, stated exactly

Installing verifies **integrity**: AO fetches the package into a quarantine
directory, computes both digests itself, and refuses anything that is not
byte-for-byte the release it resolved.

That is not the same as knowing who wrote it. **AO verifies no publisher
signature.** A release is therefore never better than `verified`, and `verified`
means the bytes match what the registry named — not that the code is safe.

| Trust state | Meaning | Reachable today |
| --- | --- | --- |
| `revoked` | The registry says this exact release must not be installed. | yes |
| `unverified` | Nothing hashed yet, or nothing beyond the provider's word. Every search result. | yes |
| `verified` | AO fetched the bytes and matched both digests itself. | yes |
| `trusted` | A signature chained to a configured trust anchor was verified. | **no** |

`trusted` is defined and unreachable, and
`skillregistry.TestTrustedIsUnreachable` reads the package's own source to hold
that true. It exists so nothing weaker gets called "trusted".

**Residual risk, stated rather than disguised:** a registry that lies about a
digest is caught. A registry that honestly serves malicious code under a correct
digest is not.

## Configuring a registry

AO ships with **no registry and no default endpoint**. An installation that
configures nothing can install nothing from one.

```bash
ao skills registry add company-private \
  --name "Company Private" \
  --location /srv/ao/registry \
  --trust-policy digest \
  --priority 10
```

Or Settings → Skills → Registries.

| Field | What it does |
| --- | --- |
| `type` | `local` is the only type this build can read. `https` and `git` are declared in the schema so a future implementation needs no migration, and are refused at configuration time — a registry AO could store and not read would answer every search with silence. |
| `location` | For `local`, an absolute directory holding `registry.json`. AO **opens it before recording it**; an unreadable registry is refused, not stored mute. |
| `trustPolicy` | What the registry must satisfy *beyond* integrity. Integrity is never optional. |
| `pinnedPublisher` | Required by, and only valid with, `pinned_publisher`. |
| `priority` | Display ordering when two registries offer the same skill id. It never decides which registry an install comes from — an install names one. |
| `tenantId` | Limits the registry to one organization. Checked against the **caller's** memberships, never against a request field. |
| `credentialSecretName` | The **name** of a sealed secret. Never a value; a `local` registry is refused if it names one at all. |

### Trust policies

- **`digest`** (default) — verify the digests AO computes itself. Produces
  `verified`.
- **`pinned_publisher`** — additionally require the release's publisher to equal
  the configured one. This is what catches a registry that starts publishing
  somebody else's name.
- **`signed`** — additionally require a verified signature. **AO verifies none,
  so a registry on this policy installs nothing**, and every refusal says why.
  It is configurable on purpose: an administrator may legitimately want a
  registry inert until AO can verify signatures. What must not happen is the
  strictest-looking setting behaving as the weakest, so the UI, the CLI and the
  API all mark it as unenforceable.

## The registry layout

A local registry is a directory with one index file and one package tree per
release:

```
/srv/ao/registry/
  registry.json
  packages/security-audit/0.1.0/skill.yaml
  packages/security-audit/0.1.0/modes/…
```

`registry.json` declares `apiVersion: ao.registry/v1` and a list of releases.
Each release carries the two digests, the publisher, the requested capabilities,
the execution modes, the AO version range, the publication date, any
deprecation or revocation, and an `artifactPath` **relative to the registry
root**. Unknown fields are refused, a duplicate `(skillId, version)` is refused,
and an `artifactPath` that escapes the root — textually or through a symlink —
is refused.

The index is **re-read on every call**. There is no cached catalog, because a
cached listing would let an install act on a release that was revoked since
somebody last looked.

## What an install actually checks

In this order, and every refusal happens before a byte reaches the catalog:

1. The registry is visible to this caller and enabled.
2. `ResolveExactRelease` re-reads one exact `MAJOR.MINOR.PATCH` version from the
   authoritative source. There is no `latest`.
3. Revocation, re-checked here rather than trusted from a search.
4. The registry's trust policy.
5. Compatibility against the running AO version.
6. The skill id is not already bound to a different registry.
7. `--update` only: strictly newer than every installed version.
8. Fetch into quarantine. `CopyPackage` refuses any entry that is not a regular
   file, so a symlink in a registry never becomes one in AO's catalog.
9. **Over the bytes that landed:** the artifact digest, the manifest digest, the
   manifest itself, and the manifest's agreement with the listing — id, version,
   publisher, and the capability set, compared exactly in both directions.
10. The ordinary catalog install, which re-verifies from the installed copy and
    refuses a version whose bytes changed.
11. The provenance row.

The quarantine directory is removed whichever way the install goes.

## Registry-crossing identity

**A skill id belongs to the registry that first installed it.** Installing any
version of that id from a second registry is refused while any version from the
first remains installed. Without that rule two registries could take turns
publishing `security-audit`, and a project pinned to a vetted `0.1.0` would sit
beside an unvetted `0.2.0` under one name.

## Revocation

A release the registry has since withdrawn:

- **blocks new installs** immediately, re-checked at resolve time;
- **is marked** on the installed row, audited once as `release_revoked_seen`;
- **is not uninstalled**, **is not disabled on any project**, and **does not
  stop a run already under way**.

Deciding what to do about an installed release that was withdrawn is a human's
call, one package at a time. Deleting somebody's installed package because a
remote registry changed its mind would be AO acting on an instruction from
outside.

## Updates

`ao skills marketplace updates`, or opening Settings → Skills → Registries.

**AO polls nothing.** There is no background synchronization and no mirror; an
update check is a request a person made. It reports a newer installable version
where one exists, names a registry it could not reach without touching that
install's recorded provenance, and marks anything the registry has revoked.

`--update` refuses a version that is not strictly newer, so clicking Update can
never hand you an older release. Installing an older version *by name* stays
allowed: versions live side by side and an activation pins one, so an install
can never change what a project already resolves to.

## Provenance

Recorded per installed version in `skill_install_origins`, and it is **AO's
row, not the registry's**. The registry's id, display name, type and location
are copied at install time rather than joined, so the provenance survives the
registry being disabled, removed, or the release disappearing from it entirely.
Removing a compromised registry must not erase the evidence of what it served.

## Audit

In the same trail as the install/enable/image actions, because it is the same
reviewer asking the same kind of question:

`registry_added` · `registry_updated` · `registry_removed` · `install` ·
`install_refused` · `update_available` · `update_installed` ·
`release_revoked_seen`

**Deliberately absent: `skill_searched` and `skill_release_viewed`.** A search
query is text a person typed; it can name an internal package or a vulnerability
they are hunting, and a row per search buys an auditor nothing the install trail
does not already carry. The audit `CHECK` has no action for either, so the
decision is enforced rather than merely intended. No secret and no artifact
content is ever written to the trail.

## Permissions

| Action | Permission |
| --- | --- |
| Search the marketplace, read a release, list registries, check updates | `settings.read` |
| Configure or remove a registry, install or update from one | `settings.manage` |
| Enable a skill on a project | `project.manage` (unchanged) |
| Run a skill | unchanged |

There is no administrative bypass. `settings.manage` is checked at the route
*and* in the service, the way the image trust root already does it, so a
mis-wired route is not the only thing between a member and the installation's
package set. A tenant-scoped registry is invisible to a caller outside its
tenant, and invisible means **404, not 403** — a 403 on a private registry tells
a stranger which organizations have one.

## Residual risks

1. **AO verifies no signature.** Integrity is checked; provenance is not. A
   registry that honestly serves malicious code under a correct digest passes
   every check here. The mitigation is the layers after this one: a capability
   grant, an image approval, and a container with no network.
2. **An administrator who installs a malicious package.** Same shape as the
   image trust root's residual risk, and equally undisguised.
3. **The compatibility check fails open.** A build with no version reports
   `unknown` and the install proceeds, recorded as unchecked. This is deliberate
   and it is the one check here that does not fail closed: what it protects
   against is a bad afternoon, not an attacker, and refusing every install on
   every source build would teach people to route around the checks that do
   matter.
4. **A local registry is a directory on this host.** Anyone who can write to it
   can publish to it. That is the operator's boundary, not AO's.
5. **`priority` orders a listing only.** Two registries offering one skill id are
   both shown; the collision rule stops the *second install*, not the confusion
   of seeing both.

## What is missing before a real public Registry

1. **A network provider.** `RegistryProvider` is shaped for it —
   `Search`/`Get`/`ListVersions`/`ResolveExactRelease` move metadata and
   `FetchArtifact` is the only method that moves bytes — but no HTTPS or git
   implementation exists. Adding one means deciding TLS trust, retries,
   timeouts, response size caps and an offline story.
2. **Signature verification.** The one thing that would make `trusted`
   reachable: a trust anchor an installation configures, a signature format AO
   validates, and a revocation path for the key rather than for the release.
   Until then `signed` refuses and `trusted` is unreachable.
3. **Registry authentication.** The column holds the *name* of a sealed secret;
   nothing reads it yet, because the only implemented type needs no credential.
4. **A publishing story.** AO can read a registry and cannot produce one. There
   is no `ao skills publish`, no index generator and no reproducible package
   build.
5. **Transparency / revocation distribution.** Revocation is whatever the
   registry says at the moment it is asked. A registry that goes silent stops
   being able to revoke anything.
6. **Rate limits and abuse controls** for a registry AO does not operate.

None of that is required for the delivery goal below, and none of it is
implied by it.

## The delivery goal, end to end

```
Settings → Skills → Marketplace → search "security" → open security-audit
  → review version / capabilities / provenance → Install
     "This installs the Skill but does not enable it on any project."
     "Installed. Choose a project to enable it."

Project → Settings → Skills → Enable (with an explicit grant)
  → Dry Run → Run
```

Two flows, two permissions, two decisions. That separation is the feature.
