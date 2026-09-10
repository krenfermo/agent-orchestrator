package registrytest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
)

// signing.go -- the AO Official trust fixture: roots, keys and signed releases.
//
// # THE PRIVATE KEYS IN THIS FILE ARE GENERATED, NOT STORED
//
// Every keypair here comes from crypto/rand at the moment a test asks for one.
// Nothing is checked in, nothing is derived from a fixed seed, and there is no
// path from this package into the daemon: it is imported only by _test files.
// A repository that shipped a private key whose public half AO trusted would be
// a repository that handed every reader the ability to sign packages AO renders
// as trusted, which is worse than shipping no signing at all.
//
// It is also why skillregistry.BuiltinOfficialRoots() is empty. The official
// path is exercised HERE, against a fixture root, so that publishing a real AO
// key later changes one function and no behaviour.
//
// # Everything a release-signing test needs to go wrong
//
// The negative matrix in the phase brief is twenty-odd distinct ways a
// signature must fail, and each one has to be constructible without hand-
// editing base64. So the fixture signs what it is given, and the tests mutate
// the release AFTER signing -- which is exactly what a tampering registry does.

// Signer is one keypair the fixture holds. The private half exists only in
// this process, for the lifetime of one test.
type Signer struct {
	KeyID string
	// Publisher is who this key signs for.
	Publisher string
	// TrustRootID is the root it chains to.
	TrustRootID string
	// IsRootKey marks a key that authorizes other keys and signs no releases.
	IsRootKey bool

	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// PublicKey is the base64 spelling AO stores.
func (s *Signer) PublicKey() string { return skillregistry.EncodePublicKey(s.pub) }

// Fingerprint is what a trust screen shows.
func (s *Signer) Fingerprint(t *testing.T) string {
	t.Helper()
	fp, err := skillregistry.Fingerprint(s.PublicKey())
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return fp
}

// TrustFixture is a root plus the keys under it, ready to seed a trust store.
type TrustFixture struct {
	Root skillregistry.TrustRoot
	// RootSigner authorizes signing keys by certificate. It signs no releases.
	RootSigner *Signer
	// Signer is the ordinary release-signing key.
	Signer *Signer
}

// NewSigner generates one keypair.
func NewSigner(t *testing.T, keyID, publisher, rootID string, isRoot bool) *Signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return &Signer{
		KeyID: keyID, Publisher: publisher, TrustRootID: rootID,
		IsRootKey: isRoot, priv: priv, pub: pub,
	}
}

// Key is the SigningKey row AO would store for this signer.
func (s *Signer) Key(t *testing.T, origin skillregistry.KeyOrigin, from time.Time) skillregistry.SigningKey {
	t.Helper()
	key := skillregistry.SigningKey{
		KeyID:       s.KeyID,
		TrustRootID: s.TrustRootID,
		Publisher:   s.Publisher,
		IsRootKey:   s.IsRootKey,
		Algorithm:   skillregistry.SchemeEd25519V1.Algorithm(),
		PublicKey:   s.PublicKey(),
		Origin:      origin,
		Status:      skillregistry.StatusActive,
		ValidFrom:   from,
		CreatedAt:   from,
		UpdatedAt:   from,
	}
	if err := key.Validate(); err != nil {
		t.Fatalf("fixture key %s is invalid: %v", s.KeyID, err)
	}
	return key
}

// NewTrustFixture builds a root with a root key and one release-signing key.
//
// tier is a parameter because the OFFICIAL path has to be testable: the whole
// point of compiling the anchor in is that production cannot create one, and
// the whole point of this fixture is that a test can.
func NewTrustFixture(
	t *testing.T, rootID, publisher string, tier skillregistry.TrustTier, from time.Time,
) TrustFixture {
	t.Helper()
	root := skillregistry.TrustRoot{
		ID:          rootID,
		Tier:        tier,
		DisplayName: "Fixture root " + rootID,
		Publisher:   publisher,
		Status:      skillregistry.StatusActive,
		ValidFrom:   from,
		CreatedAt:   from,
		UpdatedAt:   from,
	}
	if err := root.Validate(); err != nil {
		t.Fatalf("fixture root %s is invalid: %v", rootID, err)
	}
	return TrustFixture{
		Root:       root,
		RootSigner: NewSigner(t, rootID+"-root-key", publisher, rootID, true),
		Signer:     NewSigner(t, rootID+"-signing-key", publisher, rootID, false),
	}
}

// Builtin renders this fixture as a compiled-in root set, which is how a test
// stands in for skillregistry.BuiltinOfficialRoots.
func (f TrustFixture) Builtin(t *testing.T) skillregistry.BuiltinRoot {
	t.Helper()
	return skillregistry.BuiltinRoot{
		Root: f.Root,
		Keys: []skillregistry.SigningKey{
			f.RootSigner.Key(t, skillregistry.OriginBuiltIn, f.Root.ValidFrom),
			f.Signer.Key(t, skillregistry.OriginBuiltIn, f.Root.ValidFrom),
		},
	}
}

// SignRelease signs rel and returns the signature, leaving rel untouched.
//
// The payload is built by skillregistry.SigningPayload -- the SAME function the
// verifier calls. A fixture with its own copy of the field list would drift,
// and a drifted fixture makes every signature test assert that two bugs agree.
func (s *Signer) SignRelease(
	t *testing.T, rel skillregistry.Release, at time.Time,
) skillregistry.ReleaseSignature {
	t.Helper()
	scheme := skillregistry.SchemeEd25519V1
	payload := skillregistry.SigningPayload(rel, scheme, s.KeyID, at)
	sig := ed25519.Sign(s.priv, payload)
	return skillregistry.ReleaseSignature{
		Scheme:   scheme,
		KeyID:    s.KeyID,
		Value:    base64.StdEncoding.EncodeToString(sig),
		SignedAt: at.UTC().Truncate(time.Second),
	}
}

// Certify issues a KeyCertificate authorizing subject under this root key.
func (s *Signer) Certify(
	t *testing.T, subject *Signer, from time.Time, until *time.Time,
) skillregistry.KeyCertificate {
	t.Helper()
	if !s.IsRootKey {
		t.Fatalf("Certify: %s is not a root key", s.KeyID)
	}
	cert := skillregistry.KeyCertificate{
		Scheme:      skillregistry.SchemeEd25519V1,
		TrustRootID: s.TrustRootID,
		RootKeyID:   s.KeyID,
		KeyID:       subject.KeyID,
		PublicKey:   subject.PublicKey(),
		Publisher:   subject.Publisher,
		ValidFrom:   from.UTC().Truncate(time.Second),
	}
	if until != nil {
		u := until.UTC().Truncate(time.Second)
		cert.ValidUntil = &u
	}
	cert.Value = base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, cert.SigningPayload()))
	return cert
}

// PublishSigned publishes a release and attaches a signature made over it.
//
// The signature is computed from the release AS PUBLISHED, including the
// digests the fixture derived from the bytes it will serve, and the registry id
// the server was configured with -- so the payload a test signs is the payload
// the daemon will reconstruct. A test that wants a signature that does NOT
// match mutates the release afterwards with Mutate, which is what a tampering
// registry does.
func (s *Server) PublishSigned(
	t *testing.T, spec Spec, signer *Signer, signedAt time.Time,
) skillregistry.Release {
	t.Helper()
	rel := s.Publish(t, spec)
	// The verifier sets RegistryID from AO's configuration before checking, so
	// the fixture has to sign the same value rather than the empty one Publish
	// leaves behind.
	rel.RegistryID = s.RegistryID()
	sig := signer.SignRelease(t, rel, signedAt)
	s.Mutate(spec.SkillID, spec.Version, func(e *Entry) { e.Release.Signature = sig })
	rel.Signature = sig
	return rel
}
