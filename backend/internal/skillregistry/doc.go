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
//	trusted     a signature chained to a configured anchor was verified
//
// TrustTrusted is DEFINED AND UNREACHABLE. AO verifies no signature, so nothing
// here returns it, and TestTrustedIsUnreachable holds that true. It exists so
// that "verified" cannot quietly become the top of the ladder and get rendered
// as trust: integrity checked by hash is not provenance, and this package's
// whole job is to keep those two words apart. See docs/adr/0006.
//
// # What this package does not do
//
// It does not execute, and it does not decide capabilities. Installing a
// release approves no image, grants no capability, enables no project and runs
// no publisher hook or script. It also opens no socket: the only implementation
// here reads a directory, and Search is structurally unable to move bytes
// because fetching is a separate method on the Provider.
//
// # Deliberate isolation
//
// A leaf package. It imports internal/domain for the existing TenantID
// vocabulary and internal/skillcatalog for the version/digest primitives the
// catalog already defines, and nothing else from AO. It adds no migration, no
// HTTP route and no CLI command; that wiring is internal/service/skills.
package skillregistry
