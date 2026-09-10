# ADR 0008 — Making `trusted` reachable: signatures, trust roots, and what the word is allowed to mean

Status: accepted (skills phase 12)
Date: 2026-09-10
Extends `docs/adr/0006` (the trust model) and `docs/adr/0007` (the transport).

## The question

Phases 10 and 11 could verify BYTES. AO fetched a package, computed both
digests itself, and refused anything that was not byte-for-byte the release it
resolved. That catches a registry that lies about a digest, and nothing else.
A registry that honestly serves malicious code under a correct digest passed
every check, and TLS did not help: **TLS authenticates the SERVER, and the
question is who wrote the CODE.**

So `trusted` was DEFINED AND UNREACHABLE, deliberately, so that `verified`
could not quietly become the top of the ladder.

This ADR decides what it takes to reach it.

## Decision 1 — TRUSTED is a claim about WHO, and the sentence is fixed

> AO verified the bytes it fetched, and verified a signature over a canonical
> description of those bytes, made by a key that chains to a trust root this
> installation configured, held by the publisher the release names.

That is the whole claim. It does **not** mean the code is safe, that it has no
vulnerabilities, that its capabilities are benign, or that anybody audited it.
It says who signed, not what they signed off on.

Every sentence a UI shows about this comes from the daemon
(`TrustExplanation`, `RegistryTrustModelStatement`, `SkillTrustModelStatement`),
for the reason ADR 0006 established: a screen that writes its own version
eventually writes a nicer one, and the nicest available lie about this subject
is "trusted means safe".

**The ladder only goes up when the step below it held.** A signature that
verifies over bytes whose digests did not match is UNVERIFIED, not trusted — a
signature over a description of bytes AO does not hold says nothing about the
bytes AO does hold. `AssessInstalled` takes both results and is the only place
in the package that may return `TrustTrusted`; a test reads the package's own
AST to hold that true.

## Decision 2 — AO signs a purpose-built encoding, not JSON

A signature is only as good as the verifier's ability to reconstruct exactly
the bytes the signer signed. JSON cannot do that: member order is unspecified,
whitespace is free, numbers have several spellings, duplicate keys are legal in
most parsers, and an unknown field vanishes silently. Every one of those lets
two parties disagree about what was signed while both believe they agree.

So `canonical.go` defines one encoding with four properties:

- **Length-prefixed.** No separator exists for a value to contain, so `"a"+"b"`
  and `"ab"+""` cannot collide. This is the classic canonicalization break and
  a test proves it is closed.
- **Fixed field order.** Fields are written in the order the code declares, not
  the order they arrived in. Reordering a payload cannot change its encoding.
- **Domain-separated.** A release signature can never verify as a key
  certificate: the two open with different purpose strings.
- **Versioned.** The magic carries a version, so a v2 payload cannot be read as
  v1 and a v1 signature cannot be replayed against v2.

The field COUNT is written before the fields, so a longer payload never starts
with a shorter valid one — a verifier reading fewer fields than were signed
reconstructs different bytes rather than an accepted prefix.

### What the signature covers

`skillId`, `name`, `version`, `publisher`, `description`, `riskLevel`, both
digests, the capability set, every execution mode with its risk level and
capabilities, the AO compatibility range, `publishedAt`, `signedAt`, the key id,
the scheme — and the **registry id**, which binds a signature to the registry
identity AO configured, so a malicious mirror cannot re-serve somebody else's
signed bytes under its own name.

### What it deliberately does NOT cover, and why that is load-bearing

`revoked` / `revocationReason` and `deprecated` / `deprecationNote`. Those are
the REGISTRY's assertions, made after publication. A signature covering
`revoked: false` would mean a compromised release could never be withdrawn
without the signer's cooperation — which is precisely the cooperation you do
not have when you need to withdraw it. Revocation is authenticated by the
registry and by AO's own record, not by the publisher.

Also uncovered: `sourceUrl`, `changelogUrl`, the inline changelog. Reference
links AO never fetches; signing them would make a corrected typo a re-signing
event.

## Decision 3 — Ed25519, versioned from the first release

`crypto/ed25519` from the Go standard library (RFC 8032). Because:

- It is in the standard library — no dependency to audit or to go stale.
- It has no parameters to get wrong. ECDSA needs a nonce and leaks the key if
  that nonce repeats or is biased; RSA needs a padding choice, and the wrong
  one is a decade-old CVE. Ed25519 has neither knob.
- Signing is deterministic — no entropy source at signing time that can fail
  quietly, which is the failure that broke ECDSA on several real devices.
- 32-byte keys and 64-byte signatures fit in a column and on a screen.

AO implements no primitive. The scheme string `ao-sig-ed25519/v1` names the
algorithm and the payload version together, is inside the signed payload, and
is checked before the payload is built. **An unknown scheme is REFUSED, never
best-effort verified**: a verifier that falls back to the algorithm it does know
is a verifier an attacker downgrades. Migration is a second constant plus a
second case, with old signatures still verifiable under the scheme that made
them.

## Decision 4 — A trust root never arrives with the thing it vouches for

Roots are not read from a release, not read from a registry response, and not
adopted from a key certificate. They are **compiled into the build** (AO
Official) or written by an administrator through an authenticated, audited path
(enterprise). A registry that could supply the root that validates it would be a
registry that validates itself.

The official anchor is a build constant rather than a config file because a
file is a thing an attacker with write access to `~/.ao` edits, and a constant
requires replacing the binary — at which point they did not need the root. The
built-in set is **overlaid on top of the store**, so a database row cannot
shadow a compiled-in anchor even if somebody writes one directly.

The id `ao-official` and the publisher `ao` are RESERVED against the
administrative API: the one name worth impersonating is the one nobody can
claim.

### Two levels, because rotation needs two

A ROOT is a long-lived anchor identity: publisher, tier, status, validity
window. A SIGNING KEY is what signs releases and gets replaced on a schedule.
One level would have made rotation mean overwriting a root's public key under
the same id — which is exactly the silent key substitution the scheme exists to
detect.

A signing key reaches a root two ways, and they are recorded distinctly because
they are different assurances:

- **`certificate`** — AO verified a `KeyCertificate` signed by a root key it
  already holds. The chain is cryptographic end to end.
- **`administrative`** — an administrator added the key. A real and necessary
  path (a root that keeps its key offline cannot sign on demand), and the
  honest description of what it is.

**A root key may not sign releases and a release key may not mint keys.** That
separation is what stops one leaked signing key from becoming a permanent
foothold.

## Decision 5 — This build ships NO official root, deliberately

`BuiltinOfficialRoots()` returns nothing, and that is a decision rather than a
to-do.

Embedding a real-looking "AO Official" public key whose private half lived in
this repository's test fixtures would be **worse than shipping nothing**: every
reader of the repository could then mint packages AO renders as officially
trusted. And a key whose private half is reachable from a build machine is not
a root key; it is a secret waiting to be exfiltrated.

Publishing a genuine AO root is a release-engineering act this phase does not
perform: it needs an offline key, a documented ceremony, a rotation schedule and
a named holder. Until then the `official` trust policy is **configurable and
unsatisfiable**, and the refusal says exactly that — the same honest shape phase
11 gave `signed`, with the difference that the machinery behind it now genuinely
verifies and is exercised by tests against a fixture root.

**`trusted` is reachable today** through an enterprise root an administrator
configures. That is the real deliverable; the official tier is the same
machinery waiting for a key.

## Decision 6 — Key validity is checked against the SIGNING time; revocation is not

- **Expired / not-yet-valid** are measured against the moment of signing.
  Checking against *now* would mean every historical release stopped verifying
  the day its key expired — a system that punishes the passage of time rather
  than one that detects compromise.
- **Retired** is time-bounded and keeps history: signatures inside the window
  still verify. That is what makes rotating on schedule cheaper than not
  rotating.
- **Revoked is absolute and ignores the signing time.** A revoked key may have
  been in somebody else's hands for an unknown period before anybody noticed, so
  "the signature predates the revocation" establishes nothing about who made it.

`signedAt` is inside the signed payload, so backdating a claim to slip inside a
closed window breaks the signature. The window check and the signature check are
not independently defeatable.

## Decision 7 — Revocation has four subjects and two authors

Subjects: `release`, `signing_key`, `publisher`, `trust_root`. Four different
facts with four different blast radii that one boolean would have flattened —
an operator staring at a blocked install needs to know whether one release was
pulled or their whole publishing identity was compromised.

Authors, and this is a defence rather than bookkeeping:

- **A registry's word** (`skill_registry_revocations`) is accepted because it
  can only ever REFUSE an install — ADR 0007's asymmetry: caching "this is
  withdrawn" is wrong in the safe direction. But a registry that could revoke
  AO's official signing key *globally* could switch off every trusted install
  on the machine, so a registry's claim about a key, a publisher or a root is
  **scoped to installs from that registry**.
- **An administrator here** (`skill_trust_revocations`), through
  `settings.manage`, revokes globally.

What revocation still does not do, unchanged since ADR 0006: it does not
uninstall, delete files, disable a skill on any project, or stop a run under
way. It blocks NEW trusted installs and marks what is already here as affected.
AO removing somebody's installed package because a key was revoked would be AO
acting destructively on a security event, which is how a revocation notice
becomes an outage.

Built-in roots and keys cannot be revoked from this host: a revocation a local
attacker could write would be a way to turn off official trust.

## Decision 8 — Provenance is persisted, and "checked and refused" is not "never checked"

`skill_install_origins` gains the verified chain: scheme, algorithm, key id, key
fingerprint, key origin, trust root id and tier, signed-at, verified-at,
verification result, the revocation state observed at install, and the metadata
timestamp.

`signature_result` has three states — `verified`, `refused`, empty — because
"AO checked and refused" and "AO never checked" are different facts about an
installed package and a boolean would merge them into the more comfortable one.
`revocation_state_observed` is a sentence for the same reason: a reader a year
from now must be able to tell "AO asked and nothing was withdrawn" from "AO
could not ask".

**Not stored:** any private key (there is no column), any auth secret, the
signed payload itself (reconstructible from the release; a second copy is a
second thing that can disagree with the first), and the public key (it lives in
`skill_signing_keys` addressed by key id — copying it here would be a row whose
key could differ from the store's, which is substitution with extra steps).

## Decision 9 — Policy is separate from cryptography

`digest` and `pinned_publisher` require no signature. `signed` requires
`TrustTrusted` and accepts official OR enterprise roots — an administrator who
configured their own root meant it, and refusing it would make "signed" mean
"signed by us", which is what `official` is for. `official` accepts only the
official tier.

**A `verified` result never satisfies `signed` or `official`.** That is the
whole distinction the two states carry.

The tier narrowing runs AFTER the cryptography, so a genuine signature under
the wrong tier reads as a policy refusal rather than as a forgery — the second
would send somebody to change a setting when the setting is right.

Under a **digest** policy a failed signature is RECORDED and does not block the
install: that registry never promised a signature, and refusing there would make
attaching a broken signature a way to break somebody else's working registry.
The provenance row keeps the refusal, so nobody later reads "nothing checked"
where AO checked and it failed.

## Residual risks, stated

1. **`trusted` still says nothing about the code.** A publisher whose key is
   intact and who signs malicious code produces a trusted install. Signing
   authenticates authorship, not quality. Capability approval, image approval
   and per-project activation remain the controls that decide what runs.
2. **No official root exists**, so the `official` policy installs nothing and
   AO Official is machinery rather than a service. See Decision 5.
3. **First-key trust is out of band.** Adding an enterprise root's first key is
   an administrator asserting it is the right key. AO shows the full fingerprint
   so it can be compared against a channel AO does not control; nothing here
   verifies that comparison happened.
4. **An administratively added key is an organizational assurance, not a
   cryptographic one.** It is recorded and rendered as such, and it is the path
   an offline root has to use.
5. **No transparency log.** There is no third party attesting that a given key
   signed a given release at a given time, so a publisher who signs, ships and
   repudiates leaves only AO's local record. `attestationUrl` remains a link for
   a human that AO never fetches.
6. **No revocation freshness requirement.** AO polls nothing; a person syncs.
   An operator who never syncs has the live pre-install re-read and nothing more
   — unchanged from ADR 0007.
7. **Compromise of this host defeats the trust store's usefulness, not its
   integrity model.** An attacker with write access to `~/.ao` can add an
   enterprise root; they cannot add an official one, and they had easier options
   already.
8. **The trust store is installation-wide.** Trust roots are not tenant-scoped,
   so two tenants on one installation share the set of publishers AO will
   accept. Scoping them is a larger change than this phase.

## What would have to be true for AO Official as a service

- A signing key generated and held offline, with a named holder and a ceremony.
- A published fingerprint on a channel independent of the registry.
- A rotation schedule, and a rehearsed compromise procedure.
- A decision about who may publish under `ao` and how a compromised publisher
  is handled.
- `BuiltinOfficialRoots()` returning that root. Nothing else changes: the
  policy, the verifier, the UI and the tests are already in place and exercised.

## What would have to be true for External / GitHub

Unchanged from ADR 0007, plus:

- A trust model for a publisher AO does not administer — the `external` tier is
  declared and refused precisely because "who may publish to everyone" is a
  decision no phase has made.
- An answer to key discovery at scale: an administrator adding every third-party
  key by hand does not survive an open marketplace, and the alternative is a
  transparency log or a certificate authority, which is a system this repository
  has not designed.
- A revocation story for a host with no concept of one.
