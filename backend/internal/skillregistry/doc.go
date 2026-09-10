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
