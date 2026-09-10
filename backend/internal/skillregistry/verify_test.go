package skillregistry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// verify_test.go -- the negative matrix.
//
// Every row here is a way a signature must fail, and the assertion is always
// the same one: NOT trusted, with a refusal code that says which step caught
// it. A supply-chain control is only as good as its refusals, and a refusal
// that lands under the wrong code sends an operator to fix the wrong thing.
//
// The private keys are generated per test from crypto/rand. Nothing is stored.

const testPublisher = "acme"

var signedAt = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

type testKey struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestKey(t *testing.T, id string) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return testKey{id: id, pub: pub, priv: priv}
}

func (k testKey) sign(rel Release, at time.Time) ReleaseSignature {
	payload := SigningPayload(rel, SchemeEd25519V1, k.id, at)
	return ReleaseSignature{
		Scheme:   SchemeEd25519V1,
		KeyID:    k.id,
		Value:    base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, payload)),
		SignedAt: at,
	}
}

// fakeTrustStore is AO's own record. It is a map because the point of these
// tests is what the VERIFIER does with what it holds, not how it reads a
// database.
type fakeTrustStore struct {
	roots      map[string]TrustRoot
	keys       map[string]SigningKey
	pubRevoked map[string]TrustRevocation
	err        error
}

func (f *fakeTrustStore) GetTrustRoot(_ context.Context, id string) (TrustRoot, bool, error) {
	if f.err != nil {
		return TrustRoot{}, false, f.err
	}
	r, ok := f.roots[id]
	return r, ok, nil
}

func (f *fakeTrustStore) GetSigningKey(_ context.Context, id string) (SigningKey, bool, error) {
	if f.err != nil {
		return SigningKey{}, false, f.err
	}
	k, ok := f.keys[id]
	return k, ok, nil
}

func (f *fakeTrustStore) PublisherRevocation(
	_ context.Context, publisher string,
) (TrustRevocation, bool, error) {
	if f.err != nil {
		return TrustRevocation{}, false, f.err
	}
	r, ok := f.pubRevoked[publisher]
	return r, ok, nil
}

// world is a working trust setup: one active root, one active key under it,
// one signed release that verifies. Every negative test starts from here and
// breaks exactly one thing, which is what makes the failure attributable.
type world struct {
	store *fakeTrustStore
	key   testKey
	root  TrustRoot
	rel   Release
	sig   ReleaseSignature
}

func newWorld(t *testing.T) *world {
	t.Helper()
	from := signedAt.Add(-30 * 24 * time.Hour)
	key := newTestKey(t, "acme-signing-key")

	root := TrustRoot{
		ID: "acme-root", Tier: TierEnterprise, DisplayName: "Acme",
		Publisher: testPublisher, Status: StatusActive,
		ValidFrom: from, CreatedAt: from, UpdatedAt: from,
	}
	if err := root.Validate(); err != nil {
		t.Fatalf("root: %v", err)
	}
	stored := SigningKey{
		KeyID: key.id, TrustRootID: root.ID, Publisher: testPublisher,
		Algorithm: SchemeEd25519V1.Algorithm(), PublicKey: EncodePublicKey(key.pub),
		Origin: OriginAdministrative, Status: StatusActive,
		ValidFrom: from, CreatedAt: from, UpdatedAt: from,
	}
	if err := stored.Validate(); err != nil {
		t.Fatalf("key: %v", err)
	}

	rel := validRelease()
	rel.RegistryID = "acme-registry"
	rel.Publisher = testPublisher
	w := &world{
		store: &fakeTrustStore{
			roots:      map[string]TrustRoot{root.ID: root},
			keys:       map[string]SigningKey{stored.KeyID: stored},
			pubRevoked: map[string]TrustRevocation{},
		},
		key: key, root: root, rel: rel,
	}
	w.sig = key.sign(rel, signedAt)
	return w
}

func (w *world) verify(t *testing.T) (Verification, error) {
	t.Helper()
	v := NewVerifier(w.store, func() time.Time { return signedAt.Add(time.Hour) })
	return v.VerifyRelease(context.Background(), w.rel, w.sig)
}

// The one positive case. Without it every negative below could pass because
// nothing ever verifies.
func TestVerifyRelease_HappyPath(t *testing.T) {
	w := newWorld(t)
	got, err := w.verify(t)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.Verified {
		t.Fatalf("Verified = false, refusal %s: %s", got.RefusalCode, got.Refusal)
	}
	if got.TrustRootID != w.root.ID || got.TrustRootTier != TierEnterprise {
		t.Fatalf("chain = %s (%s), want %s (enterprise)", got.TrustRootID, got.TrustRootTier, w.root.ID)
	}
	if got.KeyFingerprint == "" || got.Algorithm != "ed25519" {
		t.Fatalf("provenance is incomplete: %+v", got)
	}
	// AO's own clock, not the publisher's claim.
	if !got.VerifiedAt.After(got.SignedAt) {
		t.Fatalf("verifiedAt %s is not after signedAt %s", got.VerifiedAt, got.SignedAt)
	}
}

// TestVerifyRelease_NegativeMatrix is the phase brief's fail-closed list.
//
// Each case breaks ONE thing about a working setup. The assertion is that the
// result is not trusted AND that the refusal names the step that caught it --
// because "signature refused" for a revoked key sends somebody to re-download,
// and "the key that signed this was revoked on Tuesday" sends them to read the
// advisory.
func TestVerifyRelease_NegativeMatrix(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(t *testing.T, w *world)
		wantErr error
		code    string
	}{{
		name:    "signature missing",
		break_:  func(_ *testing.T, w *world) { w.sig = ReleaseSignature{} },
		wantErr: ErrSignatureMissing, code: RefuseSignatureMissing,
	}, {
		name:    "signature malformed base64",
		break_:  func(_ *testing.T, w *world) { w.sig.Value = "not base64!!" },
		wantErr: ErrSignatureMalformed, code: RefuseSignatureMalformed,
	}, {
		name: "signature is the right base64 and the wrong length",
		break_: func(_ *testing.T, w *world) {
			w.sig.Value = base64.StdEncoding.EncodeToString([]byte("too short"))
		},
		wantErr: ErrSignatureMalformed, code: RefuseSignatureMalformed,
	}, {
		name:    "unknown algorithm is refused, never downgraded to one we know",
		break_:  func(_ *testing.T, w *world) { w.sig.Scheme = "ao-sig-rsa/v9" },
		wantErr: ErrSignatureScheme, code: RefuseSchemeUnknown,
	}, {
		name:    "unknown key",
		break_:  func(_ *testing.T, w *world) { w.sig.KeyID = "some-key-nobody-configured" },
		wantErr: ErrNoTrustPath, code: RefuseKeyUnknown,
	}, {
		name: "wrong key: a genuine signature by a key AO holds, over a payload it did not sign",
		break_: func(t *testing.T, w *world) {
			other := newTestKey(t, "acme-other-key")
			stored := w.store.keys[w.key.id]
			stored.KeyID = other.id
			stored.PublicKey = EncodePublicKey(other.pub)
			stored.Fingerprint = ""
			if err := stored.Validate(); err != nil {
				t.Fatalf("other key: %v", err)
			}
			w.store.keys[other.id] = stored
			// Signed by the ORIGINAL key, presented as the other one.
			w.sig.KeyID = other.id
		},
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "key substitution: same keyId, different bytes",
		break_: func(t *testing.T, w *world) {
			imposter := newTestKey(t, w.key.id)
			stored := w.store.keys[w.key.id]
			stored.PublicKey = EncodePublicKey(imposter.pub)
			stored.Fingerprint = ""
			if err := stored.Validate(); err != nil {
				t.Fatalf("substituted key: %v", err)
			}
			w.store.keys[w.key.id] = stored
		},
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "a root key may not sign releases",
		break_: func(_ *testing.T, w *world) {
			stored := w.store.keys[w.key.id]
			stored.IsRootKey = true
			w.store.keys[w.key.id] = stored
		},
		wantErr: ErrNoTrustPath, code: RefuseKeyNotForReleases,
	}, {
		name: "expired key",
		break_: func(_ *testing.T, w *world) {
			stored := w.store.keys[w.key.id]
			until := signedAt.Add(-time.Hour)
			stored.ValidUntil = &until
			w.store.keys[w.key.id] = stored
		},
		wantErr: ErrNoTrustPath, code: RefuseKeyUnusable,
	}, {
		name: "not-yet-valid key",
		break_: func(_ *testing.T, w *world) {
			stored := w.store.keys[w.key.id]
			stored.ValidFrom = signedAt.Add(time.Hour)
			w.store.keys[w.key.id] = stored
		},
		wantErr: ErrNoTrustPath, code: RefuseKeyUnusable,
	}, {
		name: "revoked key, even for a signature that predates the revocation",
		break_: func(_ *testing.T, w *world) {
			stored := w.store.keys[w.key.id]
			at := signedAt.Add(24 * time.Hour)
			stored.Status, stored.RevokedAt = StatusRevoked, &at
			stored.RevocationReason = "the laptop it lived on was stolen"
			w.store.keys[w.key.id] = stored
		},
		wantErr: ErrNoTrustPath, code: RefuseKeyUnusable,
	}, {
		name:   "wrong publisher: a valid signature by the wrong party",
		break_: func(_ *testing.T, w *world) { w.rel.Publisher = "somebody-else" },
		// The payload covers the publisher, so altering it after signing
		// breaks the signature FIRST. That is the correct order and the
		// stronger refusal: the release was tampered with, not merely
		// mis-attributed.
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "publisher mismatch: the key signs for somebody else",
		break_: func(t *testing.T, w *world) {
			stored := w.store.keys[w.key.id]
			stored.Publisher = "somebody-else"
			w.store.keys[w.key.id] = stored
		},
		wantErr: ErrNoTrustPath, code: RefusePublisherMismatch,
	}, {
		name: "the root anchors a different publisher than the release names",
		break_: func(_ *testing.T, w *world) {
			root := w.store.roots[w.root.ID]
			root.Publisher = "somebody-else"
			w.store.roots[w.root.ID] = root
		},
		wantErr: ErrNoTrustPath, code: RefusePublisherMismatch,
	}, {
		name:    "orphaned key: the root it names does not exist here",
		break_:  func(_ *testing.T, w *world) { delete(w.store.roots, w.root.ID) },
		wantErr: ErrNoTrustPath, code: RefuseRootUnknown,
	}, {
		name: "revoked root refuses every key under it",
		break_: func(_ *testing.T, w *world) {
			root := w.store.roots[w.root.ID]
			at := signedAt.Add(24 * time.Hour)
			root.Status, root.RevokedAt = StatusRevoked, &at
			root.RevocationReason = "the publishing organization was compromised"
			w.store.roots[w.root.ID] = root
		},
		wantErr: ErrNoTrustPath, code: RefuseRootUnusable,
	}, {
		name: "retired root anchors no new installs",
		break_: func(_ *testing.T, w *world) {
			root := w.store.roots[w.root.ID]
			root.Status = StatusRetired
			w.store.roots[w.root.ID] = root
		},
		wantErr: ErrNoTrustPath, code: RefuseRootUnusable,
	}, {
		name: "expired root",
		break_: func(_ *testing.T, w *world) {
			root := w.store.roots[w.root.ID]
			until := signedAt.Add(-time.Hour)
			root.ValidUntil = &until
			w.store.roots[w.root.ID] = root
		},
		wantErr: ErrNoTrustPath, code: RefuseRootUnusable,
	}, {
		name: "revoked publisher, whatever key or root it appears under",
		break_: func(_ *testing.T, w *world) {
			w.store.pubRevoked[testPublisher] = TrustRevocation{
				Subject: SubjectPublisher, SubjectID: testPublisher,
				Reason: "the whole publishing identity was compromised", RevokedAt: signedAt,
			}
		},
		wantErr: ErrNoTrustPath, code: RefusePublisherRevoked,
	}, {
		name:    "an unreadable trust store refuses rather than passes",
		break_:  func(_ *testing.T, w *world) { w.store.err = errors.New("database is locked") },
		wantErr: ErrTrustStore, code: RefuseStoreUnreadable,
	}, {
		name:    "modified artifact digest",
		break_:  func(_ *testing.T, w *world) { w.rel.ArtifactDigest = strings.Repeat("d", 64) },
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name:    "modified manifest digest",
		break_:  func(_ *testing.T, w *world) { w.rel.ManifestDigest = strings.Repeat("e", 64) },
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "capabilities altered after signing",
		break_: func(_ *testing.T, w *world) {
			w.rel.RequestedCapabilities = append(w.rel.RequestedCapabilities, "net.egress")
		},
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "an execution mode's risk level raised after signing",
		break_: func(_ *testing.T, w *world) {
			w.rel.ExecutionModes[0].RiskLevel = "critical"
		},
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "a mode gains a capability after signing",
		break_: func(_ *testing.T, w *world) {
			w.rel.ExecutionModes[0].Capabilities = []string{"repo.read", "report.write"}
		},
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name:    "version altered after signing",
		break_:  func(_ *testing.T, w *world) { w.rel.Version = "9.9.9" },
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "compatibility range widened after signing",
		break_: func(_ *testing.T, w *world) {
			w.rel.Compatibility.AOMaxVersion = "99.0.0"
		},
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "signature replayed onto a release from another registry",
		break_: func(_ *testing.T, w *world) {
			// A malicious mirror re-serving somebody else's signed bytes under
			// its own identity. The registry id is inside the payload, so it
			// does not verify there.
			w.rel.RegistryID = "malicious-mirror"
		},
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "signedAt moved to slip past a key's expiry",
		break_: func(_ *testing.T, w *world) {
			stored := w.store.keys[w.key.id]
			until := signedAt.Add(-time.Hour)
			stored.ValidUntil = &until
			w.store.keys[w.key.id] = stored
			// Backdating the claim would put the signature inside the window
			// -- except signedAt is inside the payload, so moving it breaks
			// the signature. The window check and the signature check are not
			// independently defeatable.
			w.sig.SignedAt = signedAt.Add(-2 * time.Hour)
		},
		wantErr: ErrSignatureInvalid, code: RefuseSignatureInvalid,
	}, {
		name: "the stored key's algorithm disagrees with the signature's scheme",
		break_: func(_ *testing.T, w *world) {
			stored := w.store.keys[w.key.id]
			stored.Algorithm = "ed448"
			w.store.keys[w.key.id] = stored
		},
		wantErr: ErrSignatureScheme, code: RefuseSchemeUnknown,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.break_(t, w)
			got, err := w.verify(t)
			if err == nil {
				t.Fatalf("verification SUCCEEDED; this case must never reach trusted")
			}
			if got.Verified {
				t.Fatalf("Verified = true alongside error %v", err)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want one wrapping %v", err, tc.wantErr)
			}
			if got.RefusalCode != tc.code {
				t.Fatalf("refusalCode = %q, want %q (refusal: %s)", got.RefusalCode, tc.code, got.Refusal)
			}
			if strings.TrimSpace(got.Refusal) == "" {
				t.Fatal("a refusal with no sentence is a refusal nobody can act on")
			}
		})
	}
}

// A nil trust store must refuse, not pass. An installation whose trust store
// failed to open installs nothing under a signed policy.
func TestVerifyRelease_NilStoreRefuses(t *testing.T) {
	w := newWorld(t)
	v := NewVerifier(nil, nil)
	got, err := v.VerifyRelease(context.Background(), w.rel, w.sig)
	if err == nil || got.Verified {
		t.Fatalf("a verifier with no trust store passed: %+v", got)
	}
	if got.RefusalCode != RefuseStoreUnreadable {
		t.Fatalf("refusalCode = %q", got.RefusalCode)
	}
}

// RequireTier is POLICY on top of cryptography, and a VERIFIED result must not
// satisfy a policy it does not meet.
func TestRequireTier(t *testing.T) {
	v := Verification{Verified: true, TrustRootID: "acme-root", TrustRootTier: TierEnterprise}
	if err := RequireTier(v, TierOfficial); err == nil {
		t.Fatal("an enterprise root satisfied an official-only policy")
	}
	if err := RequireTier(v, TierOfficial, TierEnterprise); err != nil {
		t.Fatalf("signed policy refused an enterprise root: %v", err)
	}
}

// TestTrustPolicy_VerifiedNeverSatisfiesSigned is FASE N stated as a test.
func TestTrustPolicy_VerifiedNeverSatisfiesSigned(t *testing.T) {
	for _, p := range []TrustPolicy{TrustPolicySigned, TrustPolicyOfficial} {
		if !p.RequiresSignature() {
			t.Fatalf("%q does not require a signature", p)
		}
		if len(p.AcceptedTiers()) == 0 {
			t.Fatalf("%q accepts no tier, so nothing could ever satisfy it", p)
		}
	}
	for _, p := range []TrustPolicy{TrustPolicyDigest, TrustPolicyPinnedPublisher} {
		if p.RequiresSignature() {
			t.Fatalf("%q requires a signature", p)
		}
	}
	// The official policy must never accept an enterprise root: that is the
	// entire difference between the two settings.
	for _, tier := range TrustPolicyOfficial.AcceptedTiers() {
		if tier != TierOfficial {
			t.Fatalf("official policy accepts %q", tier)
		}
	}
}

// A key certificate is what makes an added key a CHAIN rather than an
// assertion. It must not verify against the wrong root, the wrong root key, or
// a key that is not a root key at all.
func TestVerifyKeyCertificate(t *testing.T) {
	from := signedAt.Add(-30 * 24 * time.Hour)
	rootKeyPair := newTestKey(t, "acme-root-key")
	subject := newTestKey(t, "acme-signing-key")

	rootKey := SigningKey{
		KeyID: rootKeyPair.id, TrustRootID: "acme-root", Publisher: testPublisher,
		IsRootKey: true, Algorithm: SchemeEd25519V1.Algorithm(),
		PublicKey: EncodePublicKey(rootKeyPair.pub), Origin: OriginAdministrative,
		Status: StatusActive, ValidFrom: from, CreatedAt: from, UpdatedAt: from,
	}
	if err := rootKey.Validate(); err != nil {
		t.Fatalf("root key: %v", err)
	}

	makeCert := func() KeyCertificate {
		cert := KeyCertificate{
			Scheme: SchemeEd25519V1, TrustRootID: "acme-root", RootKeyID: rootKeyPair.id,
			KeyID: subject.id, PublicKey: EncodePublicKey(subject.pub),
			Publisher: testPublisher, ValidFrom: from,
		}
		cert.Value = base64.StdEncoding.EncodeToString(
			ed25519.Sign(rootKeyPair.priv, cert.SigningPayload()))
		return cert
	}

	if err := VerifyKeyCertificate(makeCert(), rootKey, signedAt); err != nil {
		t.Fatalf("a well-formed certificate was refused: %v", err)
	}

	t.Run("a non-root key cannot authorize another key", func(t *testing.T) {
		notRoot := rootKey
		notRoot.IsRootKey = false
		if err := VerifyKeyCertificate(makeCert(), notRoot, signedAt); err == nil {
			t.Fatal("a release-signing key minted another key")
		}
	})

	t.Run("the public key inside the certificate is covered", func(t *testing.T) {
		imposter := newTestKey(t, subject.id)
		cert := makeCert()
		cert.PublicKey = EncodePublicKey(imposter.pub)
		if err := VerifyKeyCertificate(cert, rootKey, signedAt); !errors.Is(err, ErrSignatureInvalid) {
			t.Fatalf("swapping the certified key gave %v", err)
		}
	})

	t.Run("a certificate cannot name a root it was not signed under", func(t *testing.T) {
		cert := makeCert()
		cert.TrustRootID = "some-other-root"
		if err := VerifyKeyCertificate(cert, rootKey, signedAt); err == nil {
			t.Fatal("a certificate claimed a root its signer does not belong to")
		}
	})

	t.Run("a revoked root key authorizes nothing", func(t *testing.T) {
		revoked := rootKey
		at := signedAt.Add(-time.Hour)
		revoked.Status, revoked.RevokedAt = StatusRevoked, &at
		revoked.RevocationReason = "compromised"
		if err := VerifyKeyCertificate(makeCert(), revoked, signedAt); !errors.Is(err, ErrNoTrustPath) {
			t.Fatalf("a revoked root key minted a key: %v", err)
		}
	})

	t.Run("a key cannot be its own anchor", func(t *testing.T) {
		cert := makeCert()
		cert.KeyID = cert.RootKeyID
		if err := VerifyKeyCertificate(cert, rootKey, signedAt); !errors.Is(err, ErrSignatureMalformed) {
			t.Fatalf("a self-authorizing certificate gave %v", err)
		}
	})
}

// TestReservedIdentifiers holds the one name an attacker would most like.
func TestReservedIdentifiers(t *testing.T) {
	for _, id := range []string{"ao-official", "AO-Official", " ao-official "} {
		if !ReservedTrustRootID(id) {
			t.Fatalf("%q is not reserved as a trust root id", id)
		}
	}
	for _, p := range []string{"ao", "AO", " Ao "} {
		if !ReservedPublisher(p) {
			t.Fatalf("%q is not reserved as a publisher", p)
		}
	}
	if ReservedTrustRootID("acme-root") || ReservedPublisher("acme") {
		t.Fatal("an ordinary identifier was treated as reserved")
	}
}

// This build ships no official root, deliberately. If one ever appears, it must
// appear because somebody minted a real key -- and this test is what makes that
// a considered change rather than a merge nobody noticed.
func TestBuiltinOfficialRootsIsEmpty(t *testing.T) {
	if got := BuiltinOfficialRoots(); len(got) != 0 {
		t.Fatalf("this build carries %d official trust root(s).\n"+
			"Embedding one means a real AO signing key exists, held offline, with a published\n"+
			"fingerprint and a rotation story. If that is now true, change this test deliberately.\n"+
			"If it is not, the key in the build is a forgery this repository is handing out.", len(got))
	}
}
