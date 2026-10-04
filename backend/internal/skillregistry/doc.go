// Package skillregistry is AO's contract for skill packages that arrive from
// OUTSIDE this repository: what a registry may offer, what AO checks before
// installing one, and what it refuses to claim afterwards.
//
// # Relationship to skillcatalog
//
// internal/skillcatalog owns what is INSTALLED — the manifest contract, the
// capability table, per-project activation, and the fail-closed resolution a
// run passes through. This package owns what is AVAILABLE, which is a
// different thing with a different trust story: a catalog entry is bytes on
// this host that AO hashed itself, and a registry entry is a description
// somebody else wrote.
//
// The two are deliberately not merged. A search result is not an install, and a
// type that could be either would make "is this on my machine" a field rather
// than a fact.
//
// # The four states this package can report
//
//	revoked     the registry says this exact release must not be installed
//	unverified  nothing has been hashed yet, or nothing beyond the provider's word
//	verified    AO fetched the bytes and computed both digests itself, and they matched
//	trusted     and a signature over those bytes chained to a configured root
//
// TrustTrusted became REACHABLE in phase 12 (docs/adr/0008). It requires
// everything verified requires and then a signature in AO's own scheme, made
// by a key AO holds, chaining to a trust root this installation configured,
// bound to the publisher the release names.
//
// # Where a release can come from, and the one that is different
//
//	local    a directory on this host
//	https    a company-private registry (phase 11)
//	github   a repository on a git forge (phase 13, docs/adr/0009)
//	git      declared and refused: a clone runs code before anybody looks
//
// The first two serve ONE IMMUTABLE ARTIFACT per version, so a version string
// is an identity there and two digests pin it. A forge is different in one way
// that everything about the github type follows from: ITS NAMES ARE MUTABLE. A
// branch is a different tree every afternoon and a tag is whatever the
// publisher last pointed it at. So an external release is identified by a
// COMMIT SHA, the tag is a label recorded beside it, and AO keeps a ledger of
// where each tag pointed so that one moving is a fact somebody is told about
// rather than a substitution nobody sees.
//
// # Hosting is not authorship
//
// None of the four external trust policies reaches trusted on its own. That is
// not temporary. A repository being on a well-known forge says who HOSTS the
// bytes, and the ladder above is about who WROTE them -- which only a
// signature chaining to a configured root can answer. An allowlisted
// organization is permission to install, not a cryptographic fact, and
// ExternalIdentity keeps the four different answers to "who published this"
// (the host account, the name in the package, the key that signed it, and what
// this installation expected) apart rather than merging them into a name.
//
// The two states stay strictly apart, and the ladder only goes up when the
// step below it held. Integrity checked by hash is not provenance; a signature
// over a description of bytes AO does not hold is not integrity. A release
// that passes one and fails the other never reads as though it passed both.
//
// # What this package does not do
//
// It does not execute, and it does not decide capabilities. Installing a
// release approves no image, grants no capability, enables no project and runs
// no publisher hook or script. Search is structurally unable to move bytes
// because fetching is a separate method on the Provider.
//
// It does not CLONE, either. The github type is an HTTP metadata API and one
// archive of one commit, read by the same client, behind the same origin
// guard, under the same ceilings and into the same quarantine as every other
// remote registry. There is no git binary, no checkout, no hook and no working
// tree anywhere in this package.
//
// It also does not SIGN. There is no private key in this package, in the
// daemon, or anywhere this process can reach: a consumer that could sign is a
// consumer that can mint its own trusted releases, at which point the
// signature proves that this host trusts itself. Signing happens elsewhere,
// offline, by whoever holds the root -- and in this repository only inside
// test fixtures.
//
// # Deliberate isolation
//
// A leaf package. It imports internal/domain for the existing TenantID
// vocabulary and internal/skillcatalog for the version/digest primitives the
// catalog already defines, and nothing else from AO. The cryptography is
// crypto/ed25519 from the standard library; this package implements no
// primitive. It adds no migration, no
// HTTP route and no CLI command; that wiring is internal/service/skills.
package skillregistry
