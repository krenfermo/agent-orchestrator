package skillregistry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// verify.go -- the chain, in order, with no step that can be skipped.
//
// # What TRUSTED is allowed to mean
//
//	AO verified the bytes it fetched, and verified a signature over a
//	canonical description of those bytes, made by a key that chains to a
//	trust root this installation configured, held by the publisher the
//	release names.
//
// That is the whole claim. It does NOT mean the code is safe, that it has no
// vulnerabilities, that its capabilities are benign, or that anybody audited
// it. It means AO knows WHO signed, not WHAT they signed off on. Every
// sentence this package hands a UI says so, because a badge that says
// "trusted" next to a package nobody read is exactly the assurance a supply
// chain attack wants to inherit.
//
// # Fail closed, and say which step failed
//
// Every refusal below names the step. That is not decoration: an operator who
// is told "signature refused" goes and re-downloads, and an operator who is
// told "the key that signed this is revoked as of Tuesday" goes and reads the
// advisory. The message is the control that gets acted on.
//
// # Order
//
// Structure, then key, then cryptography, then bindings, then revocation. The
// signature is checked BEFORE the publisher and root bindings so that a forged
// payload is rejected as a forgery rather than as a policy mismatch -- the
// second reads like a configuration problem and sends somebody to change a
// setting.

// ExternalRevocationStore is what this installation has administratively
// withdrawn on a forge.
//
// It is a separate interface from TrustStore because it answers a different
// question with a different author: TrustStore is about keys and roots, and
// this is about places. A verifier that could also decide "this repository is
// banned" would be a verifier doing policy.
type ExternalRevocationStore interface {
	// ExternalRevocation reports an administrative withdrawal of an owner, a
	// repository or a commit.
	ExternalRevocation(
		ctx context.Context, subject RevocationSubject, subjectID string,
	) (TrustRevocation, bool, error)
}

// TrustStore is AO's own record of what it will verify against.
//
// It is read-only here. Nothing in the verification path writes a key, adopts
// a root, or learns anything from the release it is checking: a verifier that
// could add to its own trust store is a verifier an attacker teaches.
type TrustStore interface {
	// GetTrustRoot returns one configured root.
	GetTrustRoot(ctx context.Context, id string) (TrustRoot, bool, error)
	// GetSigningKey returns one key AO holds, by its id.
	GetSigningKey(ctx context.Context, keyID string) (SigningKey, bool, error)
	// PublisherRevocation reports an administrative revocation of a whole
	// publisher identity, independent of any one key or root.
	PublisherRevocation(ctx context.Context, publisher string) (TrustRevocation, bool, error)
}

// RevocationSubject is WHAT was withdrawn. Four different facts, with four
// different blast radii, that a single "revoked" flag would have flattened
// into one -- and an operator staring at a blocked install needs to know
// whether one release was pulled or their publisher identity was compromised.
type RevocationSubject string

const (
	// SubjectRelease withdraws one exact skillId@version.
	SubjectRelease RevocationSubject = "release"
	// SubjectSigningKey withdraws one signing key. Every release it signed is
	// refused for new installs.
	SubjectSigningKey RevocationSubject = "signing_key"
	// SubjectPublisher withdraws a publisher identity entirely.
	SubjectPublisher RevocationSubject = "publisher"
	// SubjectTrustRoot withdraws an anchor and, with it, every key under it.
	SubjectTrustRoot RevocationSubject = "trust_root"

	// The three below arrived with external registries in phase 13. They exist
	// because a forge publishes NO REVOCATION FEED: there is no endpoint AO
	// could poll to learn that a repository was compromised, and inventing one
	// would mean inventing semantics GitHub does not have.
	//
	// So external revocation is ADMINISTRATIVE and LOCAL. Somebody here reads
	// an advisory and writes down what this installation will no longer
	// install, and the three subjects are three different blast radii that a
	// single flag would have flattened.

	// SubjectExternalOwner withdraws an account or organization entirely.
	// Every repository under it, and every release in those, is refused for
	// new installs. It is the control for "that whole org was taken over".
	SubjectExternalOwner RevocationSubject = "external_owner"
	// SubjectExternalRepository withdraws one repository, named "owner/repo".
	// It is the ordinary case: one project turned out to be malicious, or was
	// transferred to somebody nobody vetted.
	SubjectExternalRepository RevocationSubject = "external_repository"
	// SubjectExternalCommit withdraws one exact commit, named
	// "owner/repo@<sha>".
	//
	// It is the narrowest and the most useful during an incident: a repository
	// that shipped one bad release stays installable at every other commit,
	// which means an administrator can act immediately without taking a
	// dependency away from everybody who is on a good version.
	SubjectExternalCommit RevocationSubject = "external_commit"
)

// Valid reports whether s is a declared subject.
func (s RevocationSubject) Valid() bool {
	switch s {
	case SubjectRelease, SubjectSigningKey, SubjectPublisher, SubjectTrustRoot:
		return true
	}
	return s.External()
}

// External reports whether this subject withdraws something on a forge.
func (s RevocationSubject) External() bool {
	switch s {
	case SubjectExternalOwner, SubjectExternalRepository, SubjectExternalCommit:
		return true
	}
	return false
}

// RegistryReportable reports whether a REGISTRY may report this subject in its
// own withdrawal feed.
//
// The external three may not, and the reason is the point of the whole split:
// a forge publishes no revocation feed, so a registry claiming to revoke an
// owner would be a registry inventing an authority the transport does not give
// it. An administrator here revokes those, through settings.manage, and that
// decision is this installation's rather than a stranger's.
func (s RevocationSubject) RegistryReportable() bool { return s.Valid() && !s.External() }

// Global reports whether a revocation of this subject applies across every
// registry rather than only to installs from the one that reported it.
//
// The external subjects are global for the same reason an administrative key
// revocation is: they are AO's own decision, not a claim AO accepted from
// somewhere. "We do not install anything from this account" would be a strange
// thing to scope to one registry row, especially since two rows can point at
// the same owner.
func (s RevocationSubject) Global() bool { return s.External() }

// ExternalRevocationSubjects is the vocabulary an administrator may use when
// withdrawing something on a forge.
func ExternalRevocationSubjects() []RevocationSubject {
	return []RevocationSubject{
		SubjectExternalOwner, SubjectExternalRepository, SubjectExternalCommit,
	}
}

// ExternalSubjectsFor is every administrative revocation that would block this
// source, in widening order.
//
// The order is the message: an install refused because the COMMIT was
// withdrawn is a different conversation from one refused because the whole
// ACCOUNT was, and a caller that checked them in an arbitrary order would
// sometimes report the broadest reason for the narrowest fact.
func ExternalSubjectsFor(src GitSource) []struct {
	Subject RevocationSubject
	ID      string
} {
	if !src.Declared() {
		return nil
	}
	return []struct {
		Subject RevocationSubject
		ID      string
	}{
		{SubjectExternalCommit, src.CommitRef()},
		{SubjectExternalRepository, src.Slug()},
		{SubjectExternalOwner, src.Owner},
	}
}

// TrustRevocation is one administrative withdrawal recorded by this
// installation, as opposed to one a registry reported.
//
// The distinction is deliberate and it is a defence. A registry's revocations
// are accepted because they can only ever REFUSE an install -- caching "this
// is withdrawn" is wrong in the safe direction, which is ADR 0007's asymmetry.
// But a registry that could revoke AO Official's signing key could switch off
// every trusted install on the machine, so a registry's word about a KEY, a
// PUBLISHER or a ROOT is scoped to installs from that registry and surfaced
// elsewhere as a warning. Only an administrator here, through
// settings.manage, revokes one globally -- which is what this record is.
type TrustRevocation struct {
	Subject RevocationSubject `json:"subject"`
	// SubjectID is the key id, publisher, trust root id, or "<skillId>@<version>".
	SubjectID string    `json:"subjectId"`
	Reason    string    `json:"reason"`
	RevokedAt time.Time `json:"revokedAt"`
	RevokedBy string    `json:"revokedBy,omitempty"`
}

// Verification is everything AO learned while checking a signature. It is the
// provenance record: it is persisted with the install and rendered on screen,
// so that "why does this say trusted" has an answer that is not "because it
// does".
type Verification struct {
	// Verified is the whole verdict. False means nothing below may be
	// rendered as an assurance -- the fields are still filled where they were
	// established, because "the key was found and the signature did not
	// verify" is more useful than an empty struct.
	Verified bool `json:"verified"`

	Scheme    SignatureScheme `json:"scheme,omitempty"`
	Algorithm string          `json:"algorithm,omitempty"`

	KeyID          string    `json:"keyId,omitempty"`
	KeyFingerprint string    `json:"keyFingerprint,omitempty"`
	KeyOrigin      KeyOrigin `json:"keyOrigin,omitempty"`

	TrustRootID   string    `json:"trustRootId,omitempty"`
	TrustRootName string    `json:"trustRootName,omitempty"`
	TrustRootTier TrustTier `json:"trustRootTier,omitempty"`

	Publisher string `json:"publisher,omitempty"`

	// SignedAt is the publisher's claim, inside the payload. VerifiedAt is
	// AO's own clock at the moment it checked. Two facts, and only the second
	// one is AO's.
	SignedAt   time.Time `json:"signedAt,omitzero"`
	VerifiedAt time.Time `json:"verifiedAt,omitzero"`

	// RefusalCode and Refusal say which step failed and in what words. The
	// code is stable and is what the service maps onto an API error; the
	// sentence is what a person reads.
	RefusalCode string `json:"refusalCode,omitempty"`
	Refusal     string `json:"refusal,omitempty"`
}

// Refusal codes. They are exhaustive over the ways this file can say no, and
// they are stable strings because the API surfaces them and tests assert on
// them.
const (
	RefuseSignatureMissing   = "SIGNATURE_MISSING"
	RefuseSignatureMalformed = "SIGNATURE_MALFORMED"
	RefuseSchemeUnknown      = "SIGNATURE_SCHEME_UNSUPPORTED"
	RefuseKeyUnknown         = "SIGNING_KEY_UNKNOWN"
	RefuseKeyNotForReleases  = "SIGNING_KEY_IS_ROOT_KEY"
	RefuseKeyUnusable        = "SIGNING_KEY_UNUSABLE"
	RefuseSignatureInvalid   = "SIGNATURE_INVALID"
	RefusePublisherMismatch  = "PUBLISHER_MISMATCH"
	RefuseRootUnknown        = "TRUST_ROOT_UNKNOWN"
	RefuseRootUnusable       = "TRUST_ROOT_UNUSABLE"
	RefusePublisherRevoked   = "PUBLISHER_REVOKED"
	RefuseStoreUnreadable    = "TRUST_STORE_UNREADABLE"
	RefusePolicyTier         = "TRUST_ROOT_TIER_REFUSED"
)

// Verifier checks release signatures against a trust store.
type Verifier struct {
	store TrustStore
	now   func() time.Time
}

// NewVerifier builds a verifier over a trust store.
//
// A nil store makes every verification REFUSE rather than pass, which is the
// direction that matters: an installation whose trust store failed to open
// must install nothing under a signed policy, not everything.
func NewVerifier(store TrustStore, now func() time.Time) *Verifier {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Verifier{store: store, now: now}
}

// refusal builds a failed verification carrying whatever was established.
func (v *Verifier) refusal(base Verification, code string, err error) (Verification, error) {
	base.Verified = false
	base.RefusalCode = code
	base.Refusal = err.Error()
	return base, err
}

// VerifyRelease runs the chain over one release and its signature.
//
// It returns the verification AND the error. Both, because two different
// callers need different halves: the install path needs the error to refuse
// with, and the provenance record needs the struct to persist -- including
// when the answer was no, since "AO checked and refused" is a fact worth
// keeping.
func (v *Verifier) VerifyRelease(ctx context.Context, rel Release, sig ReleaseSignature) (Verification, error) {
	out := Verification{VerifiedAt: v.now().UTC(), Publisher: rel.Publisher}

	// 1. Structure. The cheapest refusals, and the ones that must come before
	//    AO goes looking for a key: answering "malformed" without a lookup
	//    keeps a malformed signature from being a probe for which key ids
	//    this installation holds.
	if err := sig.Validate(); err != nil {
		switch {
		case errors.Is(err, ErrSignatureMissing):
			return v.refusal(out, RefuseSignatureMissing,
				fmt.Errorf("%w: this release is unsigned, and the policy in force requires a "+
					"signature", err))
		case errors.Is(err, ErrSignatureScheme):
			return v.refusal(out, RefuseSchemeUnknown, err)
		default:
			return v.refusal(out, RefuseSignatureMalformed, err)
		}
	}
	out.Scheme = sig.Scheme
	out.Algorithm = sig.Scheme.Algorithm()
	out.KeyID = sig.KeyID
	out.SignedAt = sig.SignedAt

	if v.store == nil {
		return v.refusal(out, RefuseStoreUnreadable,
			fmt.Errorf("%w: this installation has no trust store, so no signature can chain to "+
				"anything", ErrTrustStore))
	}

	// 2. Locate the key by the id the signature names. The signature supplies
	//    the id and NEVER the key: material that arrived with the thing being
	//    checked would let a release verify against itself.
	key, ok, err := v.store.GetSigningKey(ctx, sig.KeyID)
	if err != nil {
		return v.refusal(out, RefuseStoreUnreadable, fmt.Errorf("%w: %w", ErrTrustStore, err))
	}
	if !ok {
		return v.refusal(out, RefuseKeyUnknown,
			noPathf("no signing key %q is configured on this installation. AO does not learn keys "+
				"from the registries it reads: a key reaches the trust store through a certificate "+
				"a root signed, or through an administrator adding it", sig.KeyID))
	}
	out.KeyFingerprint = key.Fingerprint
	out.KeyOrigin = key.Origin
	out.TrustRootID = key.TrustRootID

	// 3. A ROOT key may authorize other keys and may not sign releases. The
	//    separation is what stops one leaked release-signing key from being
	//    used to mint more keys, which is the difference between an incident
	//    and a permanent foothold.
	if key.IsRootKey {
		return v.refusal(out, RefuseKeyNotForReleases,
			noPathf("key %s is a trust-root key: it authorizes signing keys and does not sign "+
				"releases", key.KeyID))
	}
	if key.Algorithm != sig.Scheme.Algorithm() {
		return v.refusal(out, RefuseSchemeUnknown,
			fmt.Errorf("%w: key %s is %s and the signature claims %s",
				ErrSignatureScheme, key.KeyID, key.Algorithm, sig.Scheme.Algorithm()))
	}

	// 4. Is the key usable for a signature made WHEN this one says it was?
	//    Revoked is absolute; expired and not-yet-valid are measured against
	//    the signing time, not against now.
	if err := key.UsableAt(sig.SignedAt); err != nil {
		return v.refusal(out, RefuseKeyUnusable, err)
	}

	// 5. The cryptography. This is where a tampered artifact digest, a swapped
	//    capability, an altered version and a forged signature all land, and
	//    they land identically because the verifier genuinely cannot tell them
	//    apart -- it can only say that these bytes were not signed by this key.
	pub, err := DecodePublicKey(key.PublicKey)
	if err != nil {
		return v.refusal(out, RefuseStoreUnreadable,
			fmt.Errorf("%w: stored key %s is unusable: %w", ErrTrustStore, key.KeyID, err))
	}
	raw, err := sig.Raw()
	if err != nil {
		return v.refusal(out, RefuseSignatureMalformed, err)
	}
	payload := SigningPayload(rel, sig.Scheme, sig.KeyID, sig.SignedAt)
	if err := verifyEd25519(pub, payload, raw); err != nil {
		return v.refusal(out, RefuseSignatureInvalid,
			fmt.Errorf("%w: %s was not signed by key %s. Either the release was altered after "+
				"signing -- its digests, capabilities, modes, version or compatibility range -- or "+
				"it was signed by a different key", ErrSignatureInvalid, rel.Ref(), key.KeyID))
	}

	// 6. Publisher binding. A VALID signature by the WRONG party is the
	//    publisher-spoofing attack, not a pass: whoever signed had a real key,
	//    and the question is whether that key speaks for the name on the
	//    package.
	if key.Publisher != rel.Publisher {
		return v.refusal(out, RefusePublisherMismatch,
			noPathf("%s is published by %q and key %s signs for %q. The signature is genuine and "+
				"it is not this publisher's", rel.Ref(), rel.Publisher, key.KeyID, key.Publisher))
	}

	// 7. The root the key chains to. Resolved from AO's store by the id the
	//    KEY carries -- never by anything in the release, and never adopted
	//    from a registry.
	root, ok, err := v.store.GetTrustRoot(ctx, key.TrustRootID)
	if err != nil {
		return v.refusal(out, RefuseStoreUnreadable, fmt.Errorf("%w: %w", ErrTrustStore, err))
	}
	if !ok {
		return v.refusal(out, RefuseRootUnknown,
			noPathf("key %s names trust root %q and this installation has no such root",
				key.KeyID, key.TrustRootID))
	}
	out.TrustRootName = root.DisplayName
	out.TrustRootTier = root.Tier
	if root.Publisher != rel.Publisher {
		return v.refusal(out, RefusePublisherMismatch,
			noPathf("trust root %s anchors publisher %q and %s is published by %q",
				root.ID, root.Publisher, rel.Ref(), rel.Publisher))
	}

	// 8. Root revocation and validity, again against the signing time.
	if err := root.UsableAt(sig.SignedAt); err != nil {
		return v.refusal(out, RefuseRootUnusable, err)
	}

	// 9. Publisher revocation, which is broader than either: it withdraws an
	//    identity whatever key or root it appears under, and it is the control
	//    for "that whole publishing organization is compromised".
	rev, ok, err := v.store.PublisherRevocation(ctx, rel.Publisher)
	if err != nil {
		return v.refusal(out, RefuseStoreUnreadable, fmt.Errorf("%w: %w", ErrTrustStore, err))
	}
	if ok {
		return v.refusal(out, RefusePublisherRevoked,
			noPathf("publisher %q was revoked on %s: %s. Every key and root under that identity "+
				"is refused", rel.Publisher, rev.RevokedAt.UTC().Format(time.RFC3339), rev.Reason))
	}

	out.Verified = true
	return out, nil
}

// RequireTier refuses a verification whose root is not in one of the accepted
// tiers.
//
// It is separate from VerifyRelease because it is POLICY and the rest is
// CRYPTOGRAPHY. A signature is valid or it is not, independently of whether
// this registry is configured to accept enterprise roots -- and keeping the
// two apart is what makes "official" a setting rather than a second, subtly
// different verifier.
func RequireTier(v Verification, accepted ...TrustTier) error {
	for _, t := range accepted {
		if v.TrustRootTier == t {
			return nil
		}
	}
	names := make([]string, 0, len(accepted))
	for _, t := range accepted {
		names = append(names, string(t))
	}
	return noPathf("this registry accepts only %s trust roots and %s is %s",
		strings.Join(names, " or "), v.TrustRootID, v.TrustRootTier)
}

// VerifyKeyCertificate checks a root's authorization of a signing key.
//
// It is what makes an added key a CHAIN rather than an assertion, and it is
// used when an administrator supplies a certificate instead of typing a key in
// by hand. The rootKey argument is resolved by the CALLER from AO's store,
// under the trust root the certificate names, so a certificate cannot nominate
// the key that validates it.
func VerifyKeyCertificate(cert KeyCertificate, rootKey SigningKey, at time.Time) error {
	if err := cert.Validate(); err != nil {
		return err
	}
	if !rootKey.IsRootKey {
		return noPathf("key %s is not a trust-root key and cannot authorize %s",
			rootKey.KeyID, cert.KeyID)
	}
	if rootKey.KeyID != cert.RootKeyID {
		return noPathf("certificate for %s names root key %s and was checked against %s",
			cert.KeyID, cert.RootKeyID, rootKey.KeyID)
	}
	if rootKey.TrustRootID != cert.TrustRootID {
		return noPathf("certificate for %s names trust root %s and root key %s belongs to %s",
			cert.KeyID, cert.TrustRootID, rootKey.KeyID, rootKey.TrustRootID)
	}
	if err := rootKey.UsableAt(at); err != nil {
		return err
	}
	pub, err := DecodePublicKey(rootKey.PublicKey)
	if err != nil {
		return err
	}
	raw, err := ReleaseSignature{Value: cert.Value}.Raw()
	if err != nil {
		return err
	}
	if err := verifyEd25519(pub, cert.SigningPayload(), raw); err != nil {
		return fmt.Errorf("%w: the certificate for key %s was not signed by root key %s",
			ErrSignatureInvalid, cert.KeyID, rootKey.KeyID)
	}
	return nil
}
