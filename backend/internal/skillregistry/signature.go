package skillregistry

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// signature.go -- what a publisher signs, and with what.
//
// # The scheme, and why
//
// Ed25519 (RFC 8032), from crypto/ed25519 in the Go standard library.
//
//   - It is in the standard library. No dependency to audit, to pin, or to
//     have go stale, and it is the same implementation the Go security team
//     maintains for TLS.
//   - It has no parameters to get wrong. ECDSA needs a nonce and leaks the key
//     if that nonce ever repeats or is biased; RSA needs a padding choice, and
//     the wrong one is a decade-old CVE. Ed25519 has neither knob.
//   - Signing is deterministic. There is no entropy source at signing time
//     that can fail quietly, which is the failure mode that broke ECDSA on
//     several real devices.
//   - Verification is constant-time and the key is 32 bytes, which fits in a
//     column and in a settings screen.
//
// AO implements no primitive. This file computes a canonical byte string and
// hands it to the standard library.
//
// # Versioned from the first release
//
// SignatureScheme is part of the signed payload AND is checked before the
// payload is built. An unknown scheme is REFUSED, never best-effort verified:
// a verifier that falls back to "the algorithm I do know" is a verifier an
// attacker downgrades. Migration to a second scheme is a second constant here
// plus a second case in verify, with old signatures still verifiable under the
// scheme they were made with.
//
// # What the signature covers, and what it deliberately does not
//
// It covers every field that decides whether a person should install this
// package: the identity, the version, the publisher, both digests, the
// capabilities, the execution modes and their risk levels, the AO
// compatibility range, and the publication date.
//
// It does NOT cover revoked/revocationReason or deprecated/deprecationNote,
// and that is load-bearing rather than an oversight. Those fields are the
// REGISTRY's, asserted after publication and changeable without the publisher;
// a signature covering "revoked: false" would mean a compromised release could
// never be withdrawn without the signer's cooperation, which is precisely the
// cooperation you do not have when you need to withdraw it. Revocation is
// authenticated by the registry and by AO's own record, not by the publisher.
//
// It also does not cover sourceUrl, changelogUrl or the inline changelog:
// reference links for a human that AO never fetches, and signing them would
// make a corrected typo a re-signing event.

// SignatureScheme names an algorithm and its payload version together. They
// are one string because they are one decision: "which bytes, verified how".
type SignatureScheme string

const (
	// SchemeEd25519V1 is Ed25519 over the v1 canonical release payload.
	SchemeEd25519V1 SignatureScheme = "ao-sig-ed25519/v1"
)

// Valid reports whether this build can verify s. It is the only place a scheme
// becomes acceptable; every caller asks here rather than comparing strings.
func (s SignatureScheme) Valid() bool { return s == SchemeEd25519V1 }

// Algorithm is the bare primitive, for display and for the provenance row.
func (s SignatureScheme) Algorithm() string {
	if s == SchemeEd25519V1 {
		return "ed25519"
	}
	return ""
}

// Signature-material errors. They are sentinels because the marketplace maps
// each onto a distinct refusal a person can act on, and because a test that
// asserted on message text would pass a rewording that broke the mapping.
var (
	// ErrSignatureMissing means the release carries no signature at all. It is
	// separate from malformed: nothing to check is an unsigned release, and a
	// policy that requires signing must say so rather than say "invalid".
	ErrSignatureMissing = errors.New("skillregistry: release carries no signature")
	// ErrSignatureMalformed means the signature material is present and cannot
	// be parsed -- bad base64, wrong length, an empty key id.
	ErrSignatureMalformed = errors.New("skillregistry: signature material is malformed")
	// ErrSignatureScheme means the release names a scheme this build does not
	// verify. Refused, never downgraded to one it does.
	ErrSignatureScheme = errors.New("skillregistry: unsupported signature scheme")
	// ErrSignatureInvalid means the bytes were parsed and the signature does
	// not verify against the key. It is the cryptographic answer, and it is
	// deliberately the same answer for a wrong key, a tampered payload and a
	// forged signature: the verifier cannot tell them apart and must not
	// pretend it can.
	ErrSignatureInvalid = errors.New("skillregistry: signature does not verify")
)

func malformedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSignatureMalformed, fmt.Sprintf(format, args...))
}

// keyIDRe is the shape of a key identifier. It is an opaque label chosen by
// whoever runs the trust root, constrained so it can appear in a URL segment,
// a log line and a settings screen without escaping.
var keyIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// ed25519PublicKeyLen and ed25519SignatureLen are restated from the standard
// library so a length check reads as a check rather than as a magic number.
const (
	ed25519PublicKeyLen = ed25519.PublicKeySize
	ed25519SignatureLen = ed25519.SignatureSize
)

// ReleaseSignature is the detached signature a registry serves beside a
// release.
//
// It is DETACHED and lives in the registry metadata rather than inside the
// package, because the package's manifest digest is one of the things being
// signed: a signature inside the manifest would have to cover itself.
// skillcatalog.Provenance.Signature stays empty in v1 for the same reason, and
// this is the field that replaces it.
type ReleaseSignature struct {
	// Scheme names the algorithm and the payload version.
	Scheme SignatureScheme `json:"scheme"`
	// KeyID identifies which signing key made this signature. It is a LOOKUP
	// KEY into AO's own trust store, never a source of key material: a
	// signature that carried its own public key would verify against itself.
	KeyID string `json:"keyId"`
	// Value is the raw signature, base64 (standard, padded).
	Value string `json:"value"`
	// SignedAt is when the publisher says they signed. It is inside the
	// payload, so it cannot be edited without breaking the signature, and it
	// is what a key's validity window is checked against.
	SignedAt time.Time `json:"signedAt"`
}

// Declared reports whether any signature material is present.
func (s ReleaseSignature) Declared() bool {
	return strings.TrimSpace(string(s.Scheme)) != "" || strings.TrimSpace(s.Value) != ""
}

// Validate parses the material without checking it against anything. It is the
// cheap refusal that runs before AO goes looking for a key: a malformed
// signature is a malformed signature whether or not the key exists, and
// answering that first keeps a probe for "which key ids does this AO hold"
// from being a useful thing to send.
func (s ReleaseSignature) Validate() error {
	if !s.Declared() {
		return ErrSignatureMissing
	}
	if !s.Scheme.Valid() {
		return fmt.Errorf("%w: %q is not a scheme this build verifies (want %q)",
			ErrSignatureScheme, s.Scheme, SchemeEd25519V1)
	}
	if !keyIDRe.MatchString(s.KeyID) {
		return malformedf("keyId %q is not a plain key identifier", s.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s.Value))
	if err != nil {
		return malformedf("signature is not valid base64: %v", err)
	}
	if len(raw) != ed25519SignatureLen {
		return malformedf("an ed25519 signature is %d bytes and this one is %d",
			ed25519SignatureLen, len(raw))
	}
	if s.SignedAt.IsZero() {
		return malformedf("signedAt is required; a signature with no time cannot be checked " +
			"against a key's validity window")
	}
	return nil
}

// Raw returns the signature bytes. Validate must have passed.
func (s ReleaseSignature) Raw() ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s.Value))
	if err != nil || len(raw) != ed25519SignatureLen {
		return nil, malformedf("signature is not %d bytes of base64", ed25519SignatureLen)
	}
	return raw, nil
}

// releaseSignatureDomain separates a release signature from every other thing
// AO might ever sign or verify.
const releaseSignatureDomain = "ao.skill.release/v1"

// SigningPayload is the exact byte string a release signature covers.
//
// It is exported because the fixture that produces signed releases must build
// it the same way the verifier does. One function, two callers: a fixture with
// its own copy of the field list would drift, and a drifted fixture makes
// every signature test assert that two bugs agree.
//
// The release's RegistryID is included, which binds a signature to the
// registry identity AO configured. A signed release copied from one registry
// to another does not verify there, so a malicious mirror cannot re-serve
// somebody else's signed bytes under its own identity.
func SigningPayload(rel Release, scheme SignatureScheme, keyID string, signedAt time.Time) []byte {
	caps := append([]string(nil), rel.RequestedCapabilities...)
	sort.Strings(caps)

	modes := make([]string, 0, len(rel.ExecutionModes))
	for _, m := range rel.ExecutionModes {
		mc := append([]string(nil), m.Capabilities...)
		sort.Strings(mc)
		// Each mode is its own canonical blob under its own domain, so a mode
		// list cannot be re-parsed as anything else and two modes cannot be
		// merged into one by choosing names with separators in them.
		modes = append(modes, string(canonicalEncode("ao.skill.release.mode/v1", []canonicalField{
			scalarField("id", m.ID),
			scalarField("name", m.Name),
			scalarField("riskLevel", m.RiskLevel),
			listField("capabilities", mc),
		})))
	}
	// Modes are sorted by their ENCODED form, which sorts by id first because
	// id is the first field. Sorting here rather than trusting the order the
	// registry sent is what makes a reordered mode list verify identically --
	// and a mode list that differs in content, not.
	sort.Strings(modes)

	return canonicalEncode(releaseSignatureDomain, []canonicalField{
		scalarField("scheme", string(scheme)),
		scalarField("keyId", keyID),
		scalarField("registryId", rel.RegistryID),
		scalarField("skillId", rel.SkillID),
		scalarField("name", rel.Name),
		scalarField("version", rel.Version),
		scalarField("publisher", rel.Publisher),
		scalarField("description", rel.Description),
		scalarField("riskLevel", rel.RiskLevel),
		scalarField("manifestDigest", rel.ManifestDigest),
		scalarField("artifactDigest", rel.ArtifactDigest),
		listField("capabilities", caps),
		nestedField("executionModes", modes),
		scalarField("aoMinVersion", rel.Compatibility.AOMinVersion),
		scalarField("aoMaxVersion", strings.TrimSpace(rel.Compatibility.AOMaxVersion)),
		scalarField("publishedAt", canonicalTime(rel.PublishedAt)),
		scalarField("signedAt", canonicalTime(signedAt)),
	})
}

// ------------------------------------------------------------------ key certs

// keyCertificateDomain separates a key authorization from a release signature.
// Without it, a payload crafted to be valid under both domains could let a
// release signature be replayed as a key certificate, which would let anybody
// holding one signed release mint a key.
const keyCertificateDomain = "ao.trust.keycert/v1"

// KeyCertificate is a trust root authorizing one signing key.
//
// This is the link that makes the chain cryptographic rather than
// administrative. A root's own key signs a statement binding a signing key id,
// its public key, its publisher and its validity window; AO verifies that
// statement against the root key it holds, and only then will it accept a
// release signature made by that signing key.
//
// The alternative -- an administrator typing a public key into a form -- is
// also supported and is recorded as such, because an enterprise root that
// keeps its root key offline needs it. The two are deliberately
// distinguishable in the store and on screen: "AO verified a certificate from
// the root" and "somebody here said this key is fine" are different facts.
type KeyCertificate struct {
	Scheme SignatureScheme `json:"scheme"`
	// TrustRootID is the root this certificate is issued under. It is checked
	// against the root AO resolved the ROOT KEY from, so a certificate cannot
	// name a root it was not signed by.
	TrustRootID string `json:"trustRootId"`
	// RootKeyID identifies which of the root's keys signed this certificate.
	RootKeyID string `json:"rootKeyId"`

	// The key being authorized.
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
	Publisher string `json:"publisher"`

	ValidFrom  time.Time  `json:"validFrom"`
	ValidUntil *time.Time `json:"validUntil,omitempty"`

	// Value is the root's signature over this certificate, base64.
	Value string `json:"value"`
}

// SigningPayload is the byte string a key certificate's signature covers.
func (c KeyCertificate) SigningPayload() []byte {
	until := ""
	if c.ValidUntil != nil {
		until = canonicalTime(*c.ValidUntil)
	}
	return canonicalEncode(keyCertificateDomain, []canonicalField{
		scalarField("scheme", string(c.Scheme)),
		scalarField("trustRootId", c.TrustRootID),
		scalarField("rootKeyId", c.RootKeyID),
		scalarField("keyId", c.KeyID),
		scalarField("publicKey", c.PublicKey),
		scalarField("publisher", c.Publisher),
		scalarField("validFrom", canonicalTime(c.ValidFrom)),
		scalarField("validUntil", until),
	})
}

// Validate parses the certificate without checking its signature.
func (c KeyCertificate) Validate() error {
	if !c.Scheme.Valid() {
		return fmt.Errorf("%w: certificate scheme %q", ErrSignatureScheme, c.Scheme)
	}
	if !trustRootIDRe.MatchString(c.TrustRootID) {
		return malformedf("certificate trustRootId %q is not a plain identifier", c.TrustRootID)
	}
	if !keyIDRe.MatchString(c.RootKeyID) {
		return malformedf("certificate rootKeyId %q is not a plain key identifier", c.RootKeyID)
	}
	if !keyIDRe.MatchString(c.KeyID) {
		return malformedf("certificate keyId %q is not a plain key identifier", c.KeyID)
	}
	if c.RootKeyID == c.KeyID {
		return malformedf("certificate %q authorizes itself; a key cannot be its own anchor", c.KeyID)
	}
	if _, err := DecodePublicKey(c.PublicKey); err != nil {
		return err
	}
	if !publisherRe.MatchString(c.Publisher) {
		return malformedf("certificate publisher %q must be a plain identifier", c.Publisher)
	}
	if c.ValidFrom.IsZero() {
		return malformedf("certificate %q must say when it becomes valid", c.KeyID)
	}
	if c.ValidUntil != nil && !c.ValidUntil.After(c.ValidFrom) {
		return malformedf("certificate %q expires at or before it becomes valid", c.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(c.Value))
	if err != nil || len(raw) != ed25519SignatureLen {
		return malformedf("certificate signature is not %d bytes of base64", ed25519SignatureLen)
	}
	return nil
}

// ----------------------------------------------------------------- key material

// DecodePublicKey parses a base64 ed25519 public key.
//
// The length check is the whole point: ed25519.Verify PANICS on a key that is
// not 32 bytes, so every path that reaches it goes through here first. A
// verifier that can be crashed by a field in a registry response is a denial
// of service with a registry response.
func DecodePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, malformedf("public key is not valid base64: %v", err)
	}
	if len(raw) != ed25519PublicKeyLen {
		return nil, malformedf("an ed25519 public key is %d bytes and this one is %d",
			ed25519PublicKeyLen, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// EncodePublicKey is the one spelling AO stores and compares.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// Fingerprint is sha256 over the RAW key bytes, lowercase hex.
//
// It is computed from the decoded key rather than from the base64 text so that
// two spellings of one key cannot produce two fingerprints -- which would let
// a re-encoded key look like a different key on screen while behaving as the
// same one.
func Fingerprint(encoded string) (string, error) {
	pub, err := DecodePublicKey(encoded)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:]), nil
}

// ShortFingerprint is the first eight bytes of a fingerprint, grouped, for a
// settings screen. The full value is always available beside it: a truncated
// fingerprint is for recognising a key you already know, never for deciding
// that two keys are the same one.
func ShortFingerprint(fp string) string {
	if len(fp) < 16 {
		return fp
	}
	var b strings.Builder
	for i := 0; i < 16; i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(fp[i : i+4])
	}
	return b.String()
}

// verifyEd25519 is the only call into the primitive in this package.
func verifyEd25519(pub ed25519.PublicKey, payload, sig []byte) error {
	if len(pub) != ed25519PublicKeyLen || len(sig) != ed25519SignatureLen {
		return ErrSignatureMalformed
	}
	if !ed25519.Verify(pub, payload, sig) {
		return ErrSignatureInvalid
	}
	return nil
}
