# ADR 0007 — Reaching a private registry: one origin, one credential, no fallback to hope

Status: accepted (skills phase 11)
Date: 2026-09-09
Extends `docs/adr/0006`, which decided the trust model. This decides the
transport.

## The question

Phase 10 shipped the contract and a provider that reads a directory. That
directory is a real deployment — an air-gapped mirror is exactly that — but it
answered a question nobody outside this host had to be trusted for. A company
registry is different: it is a machine somebody else administers, on a network,
holding a credential of ours, serving bytes that become code on this host.

So: **what is AO allowed to do with the network, and what does it refuse to
believe about the thing on the other end?**

`trusted` remains unreachable. Nothing here verifies a signature, and reaching a
registry over TLS does not make its releases better than `verified` — TLS
authenticates the SERVER, and the question ADR 0006 is about is who wrote the
CODE. Those are different questions and this ADR does not conflate them.

## Decision 1 — a registry is an ORIGIN, and AO builds every URL itself

A configured HTTPS registry authorizes exactly one scheme, one host and one
port. Every request is built from that origin plus a path this codebase chose.
No URL that arrives in a registry response is ever fetched: `sourceUrl`,
`changelogUrl` and `attestationUrl` are reference metadata for a human.

A redirect off the origin is **refused**, not followed-with-the-credential-
stripped. Following it and stripping the header would still fetch a body chosen
by whoever controls the registry, from a host nobody authorized — which is the
SSRF, with a smaller blast radius but the same shape.

The address check runs at DIAL time against the resolved address, and the
connection is made to that address rather than to the name. That is what makes
DNS rebinding ineffective: there is no second lookup for an attacker to answer
differently. Every resolved address must be acceptable, not just one — a name
answering with both a public address and `169.254.169.254` is refused, because
allowing it would leave which one gets used to chance.

**The blocked ranges are `internal/skillegress`'s, exported for this caller.**
AO already decided which addresses a skill's traffic may reach. The daemon's own
registry traffic reaching a range the skill proxy refuses would be a second,
weaker policy in the half nobody audited. `ParsePermittedCIDRs` and
`AddressBlocked` are the whole interface, and the never-re-openable link-local
rule is enforced once, in the package that owns it.

**Private ranges are re-openable per registry, and link-local never.** Refusing
every private address is right by default and wrong for an installation whose
registry genuinely lives at `10.4.0.0/16`, and a control nobody can use is a
control that gets turned off wholesale. Each exception is written down, shown in
Settings, and printed in the audit line.

## Decision 2 — the credential is a NAME, and it reaches exactly one place

`skill_registries` stores an auth TYPE and the NAME of a sealed secret. There is
no column a value could go in. The value is fetched from `internal/secretbox` at
the moment a provider is opened, wrapped in a `skillsecrets.SecretValue` whose
`String`, `GoString` and `MarshalJSON` all redact, and attached by exactly one
function — which re-checks the origin before it sets a header.

Two auth types: bearer, and an API key in a named header. Deliberately absent:
**basic** (a password with a worse cache story) and **anything in a query
string** (which lands in access logs, proxy logs and referrers — the definition
of a credential somewhere it should not be). The header name is configuration
and is printed freely; `Authorization`, `Cookie`, `Proxy-Authorization` and
`Host` are refused as API-key header names.

The auth type is **explicit** rather than inferred from the presence of a
secret. "AO guessed bearer and the registry wanted a header" is a 401 nobody can
debug from a settings screen.

### The tenant boundary is drawn around the REGISTRY

`ResolveRegistrySecret` takes a `Registry`, not a name, and re-reads the stored
row before opening anything. A caller cannot resolve a secret by naming it: it
must name a registry that an administrator attached it to, and reaching that
registry already required passing the tenant visibility rule.

**This is stated rather than overstated.** `skill_secrets` is
installation-wide, so two tenants on one installation share a secret namespace;
what they do not share is a registry that names one. Making the secret store
itself tenant-scoped is a larger change than this phase, and it is listed as a
residual risk below rather than described as solved.

There is one narrow exception, and it exists because `SaveRegistry` opens a
registry before recording it: on a FIRST save there is no stored row to check
against. What remains is the check the caller already passed — `settings.manage`
plus membership of the tenant a tenant-scoped registry names. The reachable
consequence is that somebody who already holds `settings.manage` can learn
whether a secret NAME exists, by watching whether a save is accepted. They could
equally attach that name to a registry and save it, so the leak is existence, to
somebody who could have had it anyway, and never a value.

## Decision 3 — searching cannot download, and the interface is why

Four provider methods read metadata and one moves bytes. A registry cannot
smuggle a package into a search answer because there is nowhere in the metadata
schema to put one. The HTTPS fixture counts every request, and the tests assert
on what was NOT asked for — an absence is only provable if something was
counting.

`ResolveExactRelease` additionally **never answers from cache** and never falls
back. An install acting on a copy is exactly what revocation has to prevent.

## Decision 4 — the cache is two caches, and only one of them expires

**Metadata cache**: a record of an answer, carrying the moment it was fetched,
revalidated with `ETag`/`If-None-Match`. Past its TTL it is STALE, and stale is
a state the UI shows rather than one it rounds down to "current". An install
never acts on it.

**Artifact cache**: content-addressed and immutable, and re-measured on every
read. A cache directory is a file on this host, and "we checked it when we wrote
it" is not a check that holds now. A mismatch removes the entry.

**The key is BOTH digests.** This is not a theoretical nicety: it is a bug this
phase actually shipped and then caught.
`skillcatalog.ComputePackageDigest` deliberately EXCLUDES the manifest (the
manifest carries that digest and so cannot cover itself), so two releases whose
only difference is the version line hash to the same artifact digest — which is
every ordinary version bump of a skill whose code did not change. Keyed on the
artifact digest alone, installing 0.2.0 was served 0.1.0's tree and refused for
a manifest-digest mismatch: a correct refusal of a correct package, which is the
worst kind of failure because it looks like the security control working.

Entries are namespaced by registry. Identical bytes under two registries are the
same bytes, and that is fine right up until one of them is tenant-scoped.

## Decision 5 — offline is EVIDENCE, never a substitute for it

A release AO has never downloaded cannot be installed offline, whatever anybody
asks for. A release whose bytes AO holds can be, and only when:

- the caller asked explicitly (`allowOfflineFromCache`),
- both digests recompute over the cached tree,
- the release AO verified those bytes against is the one being installed, and
- AO holds no recorded revocation for it.

The "policy" here is a person's explicit act rather than a stored setting.
Everywhere else in AO an "install anyway" is somebody deciding, and a checkbox
in Settings that quietly allowed offline installs would be a setting nobody
remembers is on.

## Decision 6 — revocations ARE persisted, and the catalogue of offers is not

Migration 0164 recorded why AO caches no listing of what a registry offers: a
cached listing would let an install act on a release withdrawn since somebody
last looked. Phase 11 stores revocations, and the asymmetry is the point:

> Caching "this release is fine" can only ever be wrong in the dangerous
> direction. Caching "this release is withdrawn" can only ever be wrong in the
> safe one.

So a known revocation blocks a new install **while the registry is unreachable**.
Silence is not consent.

What a revocation still does not do, unchanged from ADR 0006: it does not
uninstall, does not delete files, does not disable a skill on any project, and
does not stop a run already under way. AO marks it, blocks new installs of it,
and puts the sentence on screen. Deciding what to do about an installed release
that was withdrawn is a human's call, one package at a time.

## Decision 7 — CONNECTED is the hardest state to reach

A connection test reads one small metadata endpoint and has six possible
answers: `CONNECTED`, `AUTH_FAILED`, `TLS_FAILED`, `UNREACHABLE`,
`INVALID_RESPONSE`, `POLICY_BLOCKED`.

A socket opening proves a socket opened. TLS verifying proves a certificate
verified. A 200 proves something answered. None is sufficient: a load balancer,
a captive portal and an unrelated service on the right port all produce one. So
`CONNECTED` requires the registry to identify itself, under the id this
installation configured, speaking a protocol version this build knows.

`TLS_FAILED` is deliberately not merged into `UNREACHABLE`. An unreachable
registry is an operations problem; a certificate that does not verify is a
misconfiguration or somebody in the middle.

`POLICY_BLOCKED` is deliberately not `UNREACHABLE` either: nothing was sent, AO
itself refused, and "unreachable" would send an operator to look at the network
instead of at the configuration in front of them.

**A connection test requires `settings.manage`, not `settings.read`.** It makes
AO open a connection and present a stored credential. That is not a read of AO's
own state.

## Decision 8 — nothing is unbounded

Timeouts, response size, artifact size, uncompressed size, per-file size, entry
count and redirect count all have a ceiling, and none of them is per-registry
configuration — a per-registry override is a per-registry way to turn a limit
off. A link or a device node in an archive is refused rather than unpacked, and
an entry naming a path above the package root is refused before anything is
written.

There is no "skip TLS verification" option and no "allow http" option, and there
is no configuration field that reaches the two test-only seams (a trust anchor
and a resolver) the HTTPS fixture uses. That is what makes "AO verifies the
certificate" a property of the build rather than a default somebody can flip.

## What this ADR does NOT decide

- **An open public marketplace.** Not built, and not implied by any of this.
- **AO Official.** The transport exists; the trust root does not. See below.
- **A GitHub or git provider.** Still declared and refused: a git remote is a
  fetch of a whole history where AO wants one immutable release, and "clone and
  trust the working tree" is a different trust story nobody has written down.
- **Publishing.** AO reads registries. It has no verb that writes to one.
- **Signature verification.** `trusted` stays unreachable, and a registry on the
  `signed` trust policy still installs nothing.

## Residual risks, stated

1. **A registry that honestly serves malicious code under a correct digest is
   not caught.** Unchanged from ADR 0006, and unchanged by TLS: authenticating
   the server says nothing about who wrote the package.
2. **The secret store is installation-wide.** The tenant boundary is around the
   registry, not the secret (see Decision 2).
3. **A `settings.manage` holder can probe for the existence of a secret NAME**
   through a first save. Existence only, to somebody who could attach it anyway.
4. **The metadata cache can serve a stale listing offline.** It is labelled
   STALE and no install acts on it, but a person reading a stale listing can
   still be misled about what a registry offers today.
5. **A revocation AO has never synced does not block anything.** AO polls
   nothing; a person has to ask. An operator who never syncs has the protection
   of the live pre-install re-read and nothing more.
6. **Compromise of this host defeats the artifact cache's usefulness, not its
   safety.** A tampered entry is detected and removed, but an attacker with
   write access to `~/.ao` has more direct options than poisoning a cache.

## What would have to be true for AO Official

The transport in this phase is reusable as-is. What is missing is entirely trust
root, not plumbing:

- a signing key, a publication process, and a documented key-rotation story;
- signature verification in `skillregistry`, which is the only thing that could
  ever make `TrustTrusted` reachable;
- a decision about who may publish and how a compromised publisher is handled;
- transparency-log or attestation checking if `AttestationURL` is ever to mean
  more than a link for a human.

Until those exist, an AO Official registry would be a private registry with a
nicer name, and calling it more than that would be the overstatement ADR 0006
was written to prevent.

## What would have to be true for External / GitHub

- a decision about what a "release" is in a git remote (a tag is mutable; a
  commit is not a package);
- a fetch that produces one immutable artifact rather than a working tree;
- rate limiting, authentication against a host AO does not administer, and a
  network policy for an origin that is somebody else's CDN;
- an answer to "what does revocation mean when the registry is a git host that
  has no concept of one".
