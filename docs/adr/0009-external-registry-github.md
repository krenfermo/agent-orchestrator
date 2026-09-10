# ADR 0009 — External registries: installing from a git forge without pretending hosting is trust

Status: accepted (skills phase 13)
Date: 2026-09-10
Extends `docs/adr/0006` (the trust model), `docs/adr/0007` (the transport) and
`docs/adr/0008` (signatures and what `trusted` is allowed to mean).

## The question

Phases 10 to 12 all assumed the same thing without ever saying it: **a registry
is something an administrator stood up.** A directory on this host, a company
mirror behind HTTPS, an AO Official endpoint that does not exist yet. Every one
of those serves ONE IMMUTABLE ARTIFACT per version, so "version 1.2.3" was an
identity, and two digests were enough to pin it.

A git forge breaks that assumption in one specific way, and this ADR is the
consequences of it:

> **The names are mutable.** A branch is a different tree every afternoon. A
> tag is whatever the publisher last pointed it at, and re-pointing one takes a
> single command and leaves no trace on the name.

It breaks a second, quieter one too: on a private registry the publisher and
the administrator are the same organization, and on a forge they are a
stranger. So "who published this" acquires four different answers, and they are
routinely different people.

## Decision 1 — A release is a COMMIT. A tag is a label.

`GitSource` pins a full 40-character lowercase commit SHA. Everything else is
either derived from it or is a label recorded for a human to read.

- Resolution turns a tag into a SHA **once**, records both, and from then on
  the SHA is the identity.
- An **abbreviated** SHA is refused. An abbreviation is a prefix, a prefix can
  become ambiguous as a repository grows, and "whichever commit matched first"
  is not an identity.
- A **branch** is refused outright — not discouraged, refused, with its own
  sentinel (`ErrMutableRef`). A branch has no version semantics, no publication
  moment, and no way to be re-fetched later with any confidence that the bytes
  are the ones somebody approved.

What a pinned commit buys is **integrity across time**: the bytes AO fetches
tomorrow are the bytes AO fetched today. That is a real property and it is
separate from provenance, which is Decision 4's job.

## Decision 2 — The descriptor is a FILE AT A COMMIT, and the artifact is the forge's archive OF THAT COMMIT

A repository publishes AO skills as GitHub releases. At the commit a release's
tag points at, the repository holds `.ao/release.json` — the same
`releaseBody` the private-registry protocol already defines.

**Why a file at a commit and not a release asset.** An asset can be replaced
without the tag moving and without the commit changing. A file at a commit
cannot: changing it produces a different commit, and AO is pinned to the one it
resolved. The descriptor is what names the digests AO will check the bytes
against, so it is precisely the thing that must not be swappable underneath a
resolved release.

**Why the archive and not a publisher-uploaded tarball.** Same reason, one step
further. A tarball uploaded as a release asset was built from something and
nothing about it says what. The forge's own archive of a commit is generated
FROM the commit, so "these bytes came from this commit" is a property of the
transport rather than an assertion by the publisher.

The descriptor may declare exactly one field of its own source — `path`, the
package root inside the repository. AO **re-stamps** the provider, owner,
repository, tag, commit and visibility from its own lookup. A descriptor that
could name its own repository could claim to be somebody else's code at
somebody else's commit, and every provenance row AO wrote would record the lie.

`.ao` is excluded from the package when the package root IS the repository
root, because a descriptor that declared the digest of a tree containing the
descriptor would be circular.

## Decision 3 — There is no clone, no shell and no working tree

`RegistryGitHub` is a separate type from the still-refused `RegistryGit`, and
the difference is the point. A git remote is a whole history fetched by a
program that runs hooks. This is a metadata API and one archive, fetched by
AO's own HTTP client, through the same origin guard, the same TLS verification,
the same size ceilings and the same quarantine every remote registry already
passes through.

A clone runs code the repository controls before anybody has looked at it,
which is the opposite of what a quarantine is for.

## Decision 4 — Hosting is not authorship, and no policy says otherwise

Four external trust policies, and the vocabulary does not mix with the private
one in either direction (the validator refuses both mixtures):

| policy | what it adds | what it reaches |
|---|---|---|
| `external_integrity` | AO hashed the bytes | VERIFIED |
| `external_signed` | a signature chaining to a configured root | TRUSTED |
| `external_org_allowlist` | the owner is one this installation named | VERIFIED |
| `external_deny` | nothing; the registry stays visible and installs nothing | — |

`external_org_allowlist` grants **permission to install**, not trust. An
administrator writing "we install from acme" is a statement about scope, not a
cryptographic statement about who signed anything. Conflating the two is
exactly the "trusted because GitHub" this phase exists to refuse, and a test
holds it.

`external_signed` accepts the `official` and `enterprise` tiers — the same
machinery, roots and refusals phase 12 built. It does **not** accept the
`external` TIER, which nothing in this build creates: "who may publish to
everyone" is still a decision no phase has made.

## Decision 5 — Four publisher identities, kept apart

`ExternalIdentity` carries all four and renders all four:

- the **host** identity (the account the repository sits under) — anybody can
  create one, and names are recycled;
- the **declared** publisher (the string in `skill.yaml`) — typing "anthropic"
  costs nothing;
- the **signing** publisher (the identity a trust root anchors) — the only
  cryptographic one;
- the **configured expectation** (`pinnedPublisher` on the registry).

A repository at `github.com/acme` declaring publisher `acme` is exactly as
unverified as one declaring `globex`. Where they match, that is a coincidence
worth SHOWING and never a check that passed. The sentence a UI renders comes
from the daemon, because the comfortable version — "published by acme" — is the
impersonation.

## Decision 6 — A moved tag is a fact, recorded, and never followed silently

`skill_external_tags` records, per (registry, owner, repository, tag), the
commit AO saw. Every later resolution compares.

A disagreement is a MOVED TAG, and a moved tag is:

- **shown** in the marketplace, on the row, permanently;
- **audited** as `external_tag_moved`, with both SHAs in full, whether or not
  anything was installed afterwards;
- **never followed silently** — installing through it requires
  `acknowledgeMovedTag`, which acknowledges the MOVE and skips no check;
- **never retroactive** — an existing install keeps the commit it came from.

The ledger keeps the commit the tag moved AWAY from rather than overwriting it,
because "this tag moved in March" is exactly the fact an incident review needs
and exactly the one an overwrite destroys.

**An installed release's recorded commit is never rewritten.** The bytes on
this host came from commit A and AO verified them against A's digests; that
record is true forever. Updating it because a stranger moved a pointer would be
AO editing its own history on somebody else's instruction, and it would erase
the single most useful piece of evidence there is. And because one version
means one set of bytes on this host forever, `acknowledgeMovedTag` does **not**
let a moved tag overwrite a version already installed: uninstall it, or the
publisher publishes a new version.

## Decision 7 — External revocation is administrative and local

A forge publishes **no revocation feed**. There is no endpoint AO could poll to
learn a repository was compromised, and inventing one would mean inventing
semantics GitHub does not have and then rendering them as if the forge had said
them.

So three new subjects join `skill_trust_revocations` — the table that already
holds what an ADMINISTRATOR HERE decided, globally, through `settings.manage`:
`external_owner`, `external_repository`, `external_commit`. They are refused in
a registry's own feed: a registry claiming to withdraw an account would be
claiming an authority the transport never gave it.

Three blast radii, and the narrowest is usually the right one during an
incident: withdrawing one commit leaves everybody on a good version working.

An external withdrawal **can be lifted** and a key revocation cannot. The
asymmetry is deliberate: a key somebody else may have held is compromised
forever, and a repository transferred to somebody who was then vetted is a
judgement about a place. Places get re-vetted.

## Decision 8 — Everything is bounded, including the scan

Requests per operation, repositories scanned, releases scanned, descriptor
size, archive size, entry count, backoff. "The forge is well behaved" is the
assumption each of those exists to remove — and the repository is not the
forge, it is a stranger.

A rate limit is its own sentinel (`ErrRateLimited`) and is NOT merged into
"unreachable". The forge answers 403 for both "your credential may not see
this" and "you have asked too often", and telling them apart is the difference
between sending an operator to rotate a token and sending them to wait ten
minutes. AO retries **once**, only when the requested backoff is short enough
that a person watching a settings screen would not call it a hang.

## What this phase deliberately does NOT do

- No open marketplace, and no publishing surface.
- No auto-trust of any GitHub account or organization.
- No new capability: `process.exec`, `net.active_scan` and pentest modes are
  exactly as refused as they were.
- No automatic installation, no background polling, no mirroring.
- No `external`-tier trust root: nothing in this build creates one.

## What is still missing for a marketplace open to arbitrary publishers

1. **A publisher identity system.** Today TRUSTED requires a root an
   administrator configured by hand. An open marketplace needs a way for a
   stranger's key to become trustworthy without every installation vetting it
   individually — which means an AO Official root (not yet published), a
   published policy for what AO signs, and a revocation feed AO can poll.
2. **A revocation channel that is not local.** Decision 7 is honest precisely
   because it is administrative; an open marketplace needs a signed,
   fetchable, monotonic revocation source and a policy for what a stale one
   means.
3. **Review, not just verification.** Every sentence in this phase says AO
   knows WHO signed and not WHAT they signed off on. An open marketplace is the
   setting where that gap starts mattering to people who cannot read the code
   themselves.
4. **Capability review at scale**, and a story for a package that asks for more
   on an update.
5. **Namespace ownership.** A skill id belongs to the registry that first
   installed it, which works because registries are few. It does not scale to a
   world where anybody can publish `security-audit`.
6. **Abuse handling**: takedowns, impersonation reports, typosquatting, and a
   party responsible for acting on them.
