# ADR 0006 — The Skill Registry: a trust model for packages that arrive from outside

Status: accepted (skills phase 10)
Date: 2026-09-09
Supersedes nothing. Answers roadmap open question 6.

## The question

`docs/skills/roadmap.md` open question 6 states it exactly:

> **Install sources.** Only a local absolute directory is supported. Adding a
> git or registry source means deciding a trust root for PACKAGES. Phase 8's
> per-digest administrative approval is the precedent to follow, but it is not
> the same decision: an image is bytes an administrator can inspect once, and a
> package is source that changes with every version.

Phase 8 decided who may publish an **image** this installation executes:
explicit administrative approval, per immutable digest, per full scope
(`docs/adr/0005`, migration 0163). This ADR decides the analogous question for
**packages**, and deliberately answers it differently, because the two artifacts
differ in the way the roadmap says they do.

## The four states, and why "trusted" is a word this build may not use

The phase brief asked for four distinguishable states and added one constraint:
do not say "trusted" if all that happened was a hash comparison. That constraint
is the whole design.

| State | What it means | Produced by this build |
| --- | --- | --- |
| `revoked` | The registry says this exact release must not be installed. | Yes |
| `unverified` | Nothing has been hashed yet, or the release carries no assurance beyond the provider's own words. Every release looks like this in a search result. | Yes |
| `verified` | AO fetched the bytes and computed, itself, that the manifest digest and the artifact digest both equal what the pinned release declared. Integrity, checked locally. | Yes |
| `trusted` | A signature chained to a trust anchor this installation configured was verified. | **Never** |

`trusted` is defined and unreachable. `TestTrustedIsUnreachable` asserts that no
code path returns it. It exists so that nothing else gets called "trusted": a
three-value enum would have left `verified` as the top of the ladder and invited
a UI to render it as "trusted", which is precisely the overstatement the phase
brief forbids and precisely what `skillcatalog` already refuses when it rejects
a `provenance.signature` field it cannot check.

**What `verified` does and does not mean.** It means: the bytes AO installed are
the bytes the release named. It does not mean AO knows who produced them, that
the publisher is who they claim, or that the code is safe. A registry that lies
about a digest is caught; a registry that honestly serves malicious code under a
correct digest is not. That is the residual risk, and it is stated rather than
disguised.

## The trust root for packages

**An administrator's decision to install one exact release from one configured
registry, recorded with its provenance.** Not a signature (AO verifies none, and
`signed` as a policy therefore refuses everything). Not "whatever a registry
serves", which is what an unpinned install would be.

Three controls carry it:

1. **The registry is configured, not discovered.** AO ships with no registry.
   An administrator adds one with `settings.manage`, names its type, its
   location, its trust policy and its priority. There is no default endpoint and
   no auto-discovery, so an installation that configures nothing can install
   nothing from a registry.

2. **Installation resolves an exact release.** `ResolveExactRelease` is a
   separate method from `Get` for one reason: it is the only result an install
   may act on, and it re-reads from the authoritative source rather than from
   anything a search populated. There is no "latest" install target, exactly as
   there is no "latest" activation target.

3. **Verification happens over the bytes AO holds, not the metadata it was
   sent.** The artifact is copied into a quarantine directory with
   `skillcatalog.CopyPackage` (which refuses any entry that is not a regular
   file, so a symlink never becomes an install), then hashed there, then
   compared, then loaded. A mismatch removes the quarantine and installs
   nothing.

## What installing still is not

Installing remains four states away from running. The phase brief named them and
the code keeps them separate:

```
AVAILABLE   a release a configured registry offers.        No bytes on this host.
INSTALLED   verified bytes in AO's catalog.                Reachable by nothing.
ENABLED     one project pinned one version, with a grant.  project.manage. Runs nothing.
EXECUTABLE  a mode whose every required control is         Additionally needs an
            attested, with an approved image digest.       image approval per scope.
```

Nothing in this phase moves an artifact rightward on its own. Install approves
no image, grants no capability, enables no project, and runs no publisher hook,
script or shell. The Marketplace UI has no Run button, and the API has no route
that would give it one.

## Registry-crossing identity

A skill id installed from registry A is **bound** to registry A. Installing any
version of that id from registry B is refused while any version from A remains
installed. Without that rule, two registries could take turns publishing
`security-audit`, and a project pinned to `0.1.0` from a vetted registry would
sit beside `0.2.0` from an unvetted one under the same name — the collision the
phase brief asks about. The binding is recorded per install and released only
when the last version from the first registry is uninstalled.

## Revocation

A revoked release blocks **new** installs immediately and is re-resolved at the
last point before an install proceeds. It does **not** uninstall anything
already on this host, and it does not disable a project's activation. Both
non-promises are stated in the API response and on screen, for the same reason
phase 8 states its own: a runbook needs the non-promises as much as the promise.
An installed release that AO later observes as revoked is marked, audited once
as `release_revoked_seen`, and surfaced as a decision waiting for a human.

Provenance survives the registry. An installed release keeps its recorded
origin, digests and publisher even when the release disappears from the registry
entirely — the row is AO's, not the registry's.

## Registry secrets

A private registry's credential is stored as the **name** of a sealed secret,
never as a value. `internal/secretbox` and the `skill_secrets` table already
seal values with an administrator-managed key (phase 5); adding a second, weaker
home for a credential would be adding a way to leak one. The column is validated
to hold an `UPPER_SNAKE_CASE` name and nothing else, and the one registry type
this build supports — a local directory — is refused if it names one at all.

## What is deliberately not built

- **No network provider.** `type: local` is the only implementation. The
  interface is shaped so an AO official registry, a company-private one, a
  GitHub-backed one and an offline mirror are each a new implementation rather
  than a rewrite, and no code here couples AO to any specific marketplace.
- **No automatic synchronization.** AO polls nothing and mirrors nothing. An
  update check is a request a person made.
- **No signature verification.** Which is why `signed` refuses rather than
  pretends, and why `trusted` is unreachable.
- **No new execution surface.** `process.exec`, `net.active_scan`,
  `repo.write` and `net.egress` are exactly as blocked as they were before this
  phase.
