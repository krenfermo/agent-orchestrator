package skillregistry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// trustroot.go -- where trust starts, and why it cannot start anywhere else.
//
// # The one rule this file exists to enforce
//
// A TRUST ROOT NEVER ARRIVES WITH THE THING IT IS MEANT TO VOUCH FOR.
//
// Roots are not read from a release, not read from a registry response, and
// not adopted from a key certificate. They are compiled into this build (AO
// Official) or written by an administrator through an authenticated,
// audited path. A registry that could supply the root that validates it would
// be a registry that validates itself, and the signature check would be an
// expensive way of asking a stranger whether they are trustworthy.
//
// This is why AO Official's root lives in official.go as a constant rather
// than in a config file the installer writes: a file is a thing an attacker
// with local write access edits, and a constant is a thing they have to
// replace the binary to change -- at which point they did not need the root.
//
// # Two levels, because rotation needs two
//
// A ROOT is a long-lived anchor identity: a publisher, a tier, a status and a
// validity window. It holds one or more ROOT KEYS.
//
// A SIGNING KEY is what actually signs releases. It reaches a root either
// through a KeyCertificate the root key signed -- the cryptographic path -- or
// through an administrator adding it, which is recorded as such and never
// silently rendered as the same fact.
//
// One level would have made rotation impossible without re-trusting: replacing
// the key inside a root is exactly the "silent replacement of a public key
// under the same id" that FASE G forbids.

// ErrTrustStore wraps a failure to READ the trust store, as distinct from a
// verdict the store returned. They must not be confused: "AO could not open
// its trust store" is an operational fault and "AO holds no such key" is an
// answer, and treating the first as the second would make a broken database
// look like a refused signature.
var ErrTrustStore = errors.New("skillregistry: trust store is unreadable")

// ErrNoTrustPath wraps every verdict that means "this did not chain to a root
// this installation configured".
var ErrNoTrustPath = errors.New("skillregistry: no path to a configured trust root")

func noPathf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNoTrustPath, fmt.Sprintf(format, args...))
}

var trustRootIDRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// TrustTier is WHO decided this root should be trusted. It is not a ranking of
// how much a root is trusted -- an enterprise root an administrator installed
// deliberately is not weaker than AO's -- it is a record of provenance, so a
// person reading a trust screen knows which entries they chose and which
// arrived with the build.
type TrustTier string

const (
	// TierOfficial is a root compiled into this AO build. It is not editable
	// from the UI and not removable: a root a settings screen could delete is
	// a root a compromised settings screen can delete, and "AO Official" would
	// then be whatever was added next under that name.
	TierOfficial TrustTier = "official"
	// TierEnterprise is a root this installation's administrator added for
	// its own private publishing.
	TierEnterprise TrustTier = "enterprise"
	// TierExternal is a third-party root. It is DECLARED and refused: nothing
	// in this build adds one, because "who may publish to everyone" is a
	// decision no phase has made. The constant exists so external trust cannot
	// arrive by quietly reusing the enterprise tier.
	TierExternal TrustTier = "external"
)

// Valid reports whether t is a declared tier.
func (t TrustTier) Valid() bool {
	switch t {
	case TierOfficial, TierEnterprise, TierExternal:
		return true
	}
	return false
}

// Configurable reports whether an administrator may create a root in this
// tier through the API.
func (t TrustTier) Configurable() bool { return t == TierEnterprise }

// TrustStatus is whether a root or a key may still be relied on.
type TrustStatus string

const (
	// StatusActive means usable for new verifications.
	StatusActive TrustStatus = "active"
	// StatusRetired means withdrawn from NEW signing but not compromised.
	// Signatures it made before its window closed still verify, because
	// retiring a key on schedule is housekeeping and invalidating history for
	// it would punish the publisher who rotated correctly.
	StatusRetired TrustStatus = "retired"
	// StatusRevoked means compromised or repudiated. Nothing it signed may
	// produce a new trusted install, whenever it was signed: a revoked key is
	// a key somebody else may have held for an unknown length of time, so
	// "it was signed before the revocation" proves nothing about who signed it.
	StatusRevoked TrustStatus = "revoked"
)

// Valid reports whether s is a declared status.
func (s TrustStatus) Valid() bool {
	switch s {
	case StatusActive, StatusRetired, StatusRevoked:
		return true
	}
	return false
}

// TrustRoot is one configured anchor.
//
// There is no private key here and no column one could go in. The daemon
// verifies; it does not sign, and a consumer that held a signing key would be
// a consumer that could mint its own trusted releases -- at which point the
// signature proves that this host trusts itself.
type TrustRoot struct {
	ID   string    `json:"trustRootId"`
	Tier TrustTier `json:"tier"`
	// DisplayName is what a settings screen shows.
	DisplayName string `json:"displayName"`
	// Publisher is the identity this root vouches for. A release whose
	// publisher differs from the publisher on the key's root does not become
	// trusted, however good its signature: a valid signature by the wrong
	// party is the publisher-spoofing attack, not a pass.
	Publisher string `json:"publisher"`

	Status     TrustStatus `json:"status"`
	ValidFrom  time.Time   `json:"validFrom"`
	ValidUntil *time.Time  `json:"validUntil,omitempty"`

	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
	RevocationReason string     `json:"revocationReason,omitempty"`

	CreatedAt time.Time `json:"createdAt,omitzero"`
	CreatedBy string    `json:"createdBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
}

// KeyOrigin is HOW a signing key came to be associated with its root. It is
// recorded and shown because the two are different assurances and a screen
// that rendered them identically would be overstating one of them.
type KeyOrigin string

const (
	// OriginCertificate means AO verified a KeyCertificate signed by a root
	// key it holds. The chain is cryptographic end to end.
	OriginCertificate KeyOrigin = "certificate"
	// OriginAdministrative means an administrator added the key against this
	// root through the authenticated API. The chain is an organizational
	// decision recorded in the audit trail, which is the honest description of
	// what it is.
	OriginAdministrative KeyOrigin = "administrative"
	// OriginBuiltIn means the key arrived with this AO build, alongside its
	// root. Only official roots have them.
	OriginBuiltIn KeyOrigin = "built-in"
)

// Valid reports whether o is a declared origin.
func (o KeyOrigin) Valid() bool {
	switch o {
	case OriginCertificate, OriginAdministrative, OriginBuiltIn:
		return true
	}
	return false
}

// SigningKey is one public key AO will check signatures against.
type SigningKey struct {
	KeyID       string `json:"keyId"`
	TrustRootID string `json:"trustRootId"`
	// Publisher is the identity this key may sign for. It is stored per key
	// rather than only per root so that a root can, in principle, authorize
	// keys for more than one publisher; today the store refuses a key whose
	// publisher differs from its root's, and that refusal is the check rather
	// than the absence of the field.
	Publisher string `json:"publisher"`
	// IsRootKey marks a key that may sign KEY CERTIFICATES rather than
	// releases. The two roles are separated so a release-signing key that
	// leaks cannot be used to mint more keys, which is what turns one
	// compromised release into a permanent foothold.
	IsRootKey bool `json:"isRootKey"`

	Algorithm string `json:"algorithm"`
	// PublicKey is base64. There is no private counterpart in this process.
	PublicKey string `json:"publicKey"`
	// Fingerprint is sha256 over the raw key bytes, filled by Validate so the
	// store never holds a fingerprint that disagrees with its key.
	Fingerprint string `json:"fingerprint"`

	Origin KeyOrigin   `json:"origin"`
	Status TrustStatus `json:"status"`

	ValidFrom  time.Time  `json:"validFrom"`
	ValidUntil *time.Time `json:"validUntil,omitempty"`

	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
	RevocationReason string     `json:"revocationReason,omitempty"`

	// RotatedFromKeyID names the key this one replaced, when it replaced one.
	// It is what makes a rotation legible as a rotation rather than as two
	// unrelated keys that happened to overlap.
	RotatedFromKeyID string `json:"rotatedFromKeyId,omitempty"`

	CreatedAt time.Time `json:"createdAt,omitzero"`
	CreatedBy string    `json:"createdBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
}

// Validate enforces the root contract.
func (r TrustRoot) Validate() error {
	if !trustRootIDRe.MatchString(r.ID) {
		return badConfigf("trustRootId %q must be lowercase kebab-case", r.ID)
	}
	if len(r.ID) > 64 {
		return badConfigf("trustRootId %q is longer than 64 characters", r.ID)
	}
	if !r.Tier.Valid() {
		return badConfigf("tier %q is not one of official, enterprise, external", r.Tier)
	}
	if strings.TrimSpace(r.DisplayName) == "" {
		return badConfigf("a trust root must have a display name; an anonymous anchor is one " +
			"nobody can review")
	}
	if !publisherRe.MatchString(r.Publisher) {
		return badConfigf("trust root publisher %q must be a plain identifier", r.Publisher)
	}
	if !r.Status.Valid() {
		return badConfigf("trust root status %q is not one of active, retired, revoked", r.Status)
	}
	if r.ValidFrom.IsZero() {
		return badConfigf("a trust root must say when it becomes valid")
	}
	if r.ValidUntil != nil && !r.ValidUntil.After(r.ValidFrom) {
		return badConfigf("trust root %s expires at or before it becomes valid", r.ID)
	}
	if r.Status == StatusRevoked {
		if r.RevokedAt == nil {
			return badConfigf("a revoked trust root must record when")
		}
		if strings.TrimSpace(r.RevocationReason) == "" {
			return badConfigf("a revoked trust root must say why; one that does not is " +
				"indistinguishable from a mistake, and it is about to block every install under it")
		}
	}
	return nil
}

// Validate enforces the key contract and fills the fingerprint.
//
// It is a pointer method because it WRITES the fingerprint. Deriving it here,
// rather than accepting one from a caller, is what makes "the fingerprint on
// screen is this key" a property instead of a hope: there is no path that
// stores a key and a fingerprint that were never compared.
func (k *SigningKey) Validate() error {
	if !keyIDRe.MatchString(k.KeyID) {
		return badConfigf("keyId %q is not a plain key identifier", k.KeyID)
	}
	if !trustRootIDRe.MatchString(k.TrustRootID) {
		return badConfigf("key %s names trustRootId %q, which is not a plain identifier",
			k.KeyID, k.TrustRootID)
	}
	if !publisherRe.MatchString(k.Publisher) {
		return badConfigf("key %s publisher %q must be a plain identifier", k.KeyID, k.Publisher)
	}
	if k.Algorithm != SchemeEd25519V1.Algorithm() {
		return badConfigf("key %s algorithm %q is not one this build verifies (want %q)",
			k.KeyID, k.Algorithm, SchemeEd25519V1.Algorithm())
	}
	fp, err := Fingerprint(k.PublicKey)
	if err != nil {
		return badConfigf("key %s: %v", k.KeyID, err)
	}
	if existing := strings.TrimSpace(k.Fingerprint); existing != "" && existing != fp {
		// A caller supplying a fingerprint that does not match its key is
		// either confused or substituting one, and both end the same way.
		return badConfigf("key %s carries fingerprint %s and its public key hashes to %s",
			k.KeyID, existing, fp)
	}
	k.Fingerprint = fp
	if !k.Origin.Valid() {
		return badConfigf("key %s origin %q is not one of certificate, administrative, built-in",
			k.KeyID, k.Origin)
	}
	if !k.Status.Valid() {
		return badConfigf("key %s status %q is not one of active, retired, revoked", k.KeyID, k.Status)
	}
	if k.ValidFrom.IsZero() {
		return badConfigf("key %s must say when it becomes valid", k.KeyID)
	}
	if k.ValidUntil != nil && !k.ValidUntil.After(k.ValidFrom) {
		return badConfigf("key %s expires at or before it becomes valid", k.KeyID)
	}
	if k.Status == StatusRevoked {
		if k.RevokedAt == nil {
			return badConfigf("a revoked key must record when")
		}
		if strings.TrimSpace(k.RevocationReason) == "" {
			return badConfigf("revoked key %s must say why", k.KeyID)
		}
	}
	if k.RotatedFromKeyID != "" && !keyIDRe.MatchString(k.RotatedFromKeyID) {
		return badConfigf("key %s names rotatedFromKeyId %q, which is not a plain key identifier",
			k.KeyID, k.RotatedFromKeyID)
	}
	if k.RotatedFromKeyID == k.KeyID {
		return badConfigf("key %s cannot be a rotation of itself", k.KeyID)
	}
	return nil
}

// UsableAt reports whether this key may be relied on for a signature made at
// t, and says why not when it may not.
//
// The rules, and the asymmetry that matters:
//
//   - REVOKED is absolute and ignores t. A revoked key may have been in
//     somebody else's hands for an unknown period before anybody noticed, so
//     "the signature predates the revocation" establishes nothing about who
//     made it.
//   - RETIRED is time-bounded. A key withdrawn on schedule keeps its history:
//     signatures inside its window still verify, and signatures after it do
//     not. That is what makes rotating on time cheaper than not rotating.
//   - The window is checked against the moment of SIGNING, not against now.
//     Checking against now would mean every historical release stopped
//     verifying the day its key expired, which is a system that punishes the
//     passage of time rather than a system that detects compromise.
func (k SigningKey) UsableAt(t time.Time) error {
	if k.Status == StatusRevoked {
		when := "an unrecorded time"
		if k.RevokedAt != nil {
			when = k.RevokedAt.UTC().Format(time.RFC3339)
		}
		return noPathf("signing key %s was revoked on %s: %s. A revoked key's earlier signatures "+
			"are refused too, because a key somebody else may have held proves nothing about who "+
			"used it and when", k.KeyID, when, k.RevocationReason)
	}
	if t.Before(k.ValidFrom) {
		return noPathf("signing key %s is not valid until %s and the signature is dated %s",
			k.KeyID, k.ValidFrom.UTC().Format(time.RFC3339), t.UTC().Format(time.RFC3339))
	}
	if k.ValidUntil != nil && t.After(*k.ValidUntil) {
		return noPathf("signing key %s expired on %s and the signature is dated %s",
			k.KeyID, k.ValidUntil.UTC().Format(time.RFC3339), t.UTC().Format(time.RFC3339))
	}
	return nil
}

// UsableAt reports whether this root may anchor a signature made at t.
//
// A revoked root is absolute for the same reason a revoked key is, and it is
// stricter in consequence: revoking a root refuses every key under it at once,
// which is the control for "our whole publishing identity was compromised".
func (r TrustRoot) UsableAt(t time.Time) error {
	if r.Status == StatusRevoked {
		when := "an unrecorded time"
		if r.RevokedAt != nil {
			when = r.RevokedAt.UTC().Format(time.RFC3339)
		}
		return noPathf("trust root %s was revoked on %s: %s. Every key under it is refused",
			r.ID, when, r.RevocationReason)
	}
	if r.Status == StatusRetired {
		// A retired ROOT differs from a retired key: it stops anchoring NEW
		// trusted installs outright. A root is retired when an organization
		// stops publishing under it, and continuing to accept installs under
		// a retired identity is how a dormant identity gets revived by
		// somebody else.
		return noPathf("trust root %s is retired and no longer anchors new installs", r.ID)
	}
	if t.Before(r.ValidFrom) {
		return noPathf("trust root %s is not valid until %s and the signature is dated %s",
			r.ID, r.ValidFrom.UTC().Format(time.RFC3339), t.UTC().Format(time.RFC3339))
	}
	if r.ValidUntil != nil && t.After(*r.ValidUntil) {
		return noPathf("trust root %s expired on %s and the signature is dated %s",
			r.ID, r.ValidUntil.UTC().Format(time.RFC3339), t.UTC().Format(time.RFC3339))
	}
	return nil
}
