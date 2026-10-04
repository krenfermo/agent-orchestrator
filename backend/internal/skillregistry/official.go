package skillregistry

import (
	"strings"
	"time"
)

// official.go -- AO Official, and the one thing that makes it different.
//
// # It is NOT a second protocol
//
// AO Official speaks the HTTPS registry protocol phase 11 already shipped, over
// the same provider, with the same origin pinning, the same size limits, the
// same cache and the same refusal to follow a URL a response handed it. A
// second transport would be a second place for every one of those controls to
// be re-implemented slightly differently, and the second copy is always the one
// with the hole.
//
// What distinguishes AO Official is entirely TRUST POLICY:
//
//   - signatures are mandatory, not optional;
//   - the key must chain to a root in the OFFICIAL tier, which means a root
//     compiled into this build;
//   - the publisher must be the official publisher;
//   - revocations are mandatory rather than advisory.
//
// # Why the root is compiled in and not configured
//
// A trust root in a file is a trust root an attacker with write access to
// ~/.ao replaces. A trust root in the binary requires replacing the binary, and
// an attacker who can do that did not need the root. So the official anchor is
// a build constant, the official id and publisher are RESERVED against the
// administrative API, and no amount of settings.manage lets somebody create a
// root that claims to be AO Official.
//
// # Why this build ships NO official key, deliberately
//
// BuiltinOfficialRoots is EMPTY, and that is a decision rather than a to-do.
//
// Embedding a real-looking "AO Official" public key whose private half lived in
// this repository's test fixtures would be worse than shipping nothing: every
// reader of the repository could then mint packages that AO renders as
// officially trusted. And a key whose private half lives anywhere a build
// machine can reach is not a root key; it is a secret waiting to be exfiltrated.
//
// Publishing a genuine AO root is a release-engineering act this phase does not
// perform: it needs an offline key, a documented ceremony, a rotation schedule
// and a named holder. Until it exists, the `official` trust policy is
// CONFIGURABLE and UNSATISFIABLE, and the refusal says exactly that -- the same
// honest shape phase 11 gave the `signed` policy, with the difference that the
// verification machinery behind it now genuinely works and is exercised by
// tests against a fixture root.
//
// TRUSTED itself is NOT unreachable any more. An enterprise root an
// administrator configures reaches it today, through the full chain.

// The reserved AO Official identifiers. They are refused as user input
// everywhere a root, a publisher or a registry id is accepted, so that the one
// thing an attacker would most like to be called is the one thing they cannot
// name themselves.
const (
	// OfficialTrustRootID is the id the built-in official anchor uses.
	OfficialTrustRootID = "ao-official"
	// OfficialPublisher is the publisher AO Official releases are signed for.
	OfficialPublisher = "ao"
	// OfficialRegistryID is the conventional id for the AO Official registry
	// row. It is a convention rather than a check: what makes a registry
	// official is its trust policy and the tier of the root its releases chain
	// to, never its name.
	OfficialRegistryID = "ao-official"
)

// ReservedTrustRootID reports whether id may not be created through the
// administrative API.
//
// The comparison is case-insensitive and trims, because "AO-Official " reaching
// the store as a distinct row would put two things called AO Official on a
// settings screen and let a person pick the wrong one.
func ReservedTrustRootID(id string) bool {
	return strings.EqualFold(strings.TrimSpace(id), OfficialTrustRootID)
}

// ReservedPublisher reports whether an administratively created root may claim
// this publisher.
func ReservedPublisher(publisher string) bool {
	return strings.EqualFold(strings.TrimSpace(publisher), OfficialPublisher)
}

// BuiltinRoot is one anchor compiled into this build, with its keys.
//
// Keys travel WITH the root rather than being looked up separately, because a
// built-in root with no key is a root that anchors nothing, and separating them
// would make that state expressible by accident.
type BuiltinRoot struct {
	Root TrustRoot
	Keys []SigningKey
}

// BuiltinOfficialRoots returns the official anchors this build carries.
//
// It is EMPTY. See the header: no genuine AO root key exists yet, and a
// plausible-looking one would be a forgery this repository handed out for free.
// The function exists, is wired through every layer, and is exercised by tests
// with a fixture root, so that publishing a real key later is a change to this
// one function and to nothing else.
func BuiltinOfficialRoots() []BuiltinRoot { return nil }

// OfficialPolicyUnsatisfiable explains, in the words a person should read, why
// an official-policy install cannot complete on a build with no official root.
//
// It is a function here rather than a sentence in the service so there is one
// wording. A message this specific gets copied into a UI otherwise, and the
// copy is the one that eventually says something more comfortable.
func OfficialPolicyUnsatisfiable(registryID string) string {
	return "registry " + registryID + " requires an AO Official trust root and this build carries " +
		"none. AO's official signing key has not been published yet: embedding a placeholder would " +
		"mean anybody reading AO's source could sign packages this screen would call official. " +
		"Nothing installs from this registry until a real root ships. A registry using an " +
		"enterprise trust root your administrator configured can reach trusted today."
}

// NewOfficialRegistry builds the configuration an AO Official registry row
// should have.
//
// It is a constructor rather than a stored template so the invariants -- https,
// official policy, the official publisher, no credential -- are established in
// code that a settings form cannot route around.
func NewOfficialRegistry(location string, at time.Time) Registry {
	return Registry{
		ID:          OfficialRegistryID,
		DisplayName: "AO Official",
		Type:        RegistryHTTPS,
		Location:    strings.TrimSpace(location),
		Enabled:     true,
		TrustPolicy: TrustPolicyOfficial,
		// The official registry authenticates nothing: it is a public read,
		// and a credential attached to it would be a credential sent to a
		// public endpoint for no reason.
		AuthType:  AuthNone,
		Priority:  0,
		CreatedAt: at,
		UpdatedAt: at,
	}
}
