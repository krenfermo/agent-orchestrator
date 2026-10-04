package skillregistry

import (
	"bytes"
	"testing"
	"time"
)

// canonical_test.go -- the properties the encoding exists to have.
//
// These are the tests that would have caught the classic canonicalization
// breaks: a separator that a value can contain, a field order that travels
// with the data, a domain that lets one signed thing be replayed as another.

// Reordering a payload must not change its bytes. A registry that shuffled a
// mode list or a capability list would otherwise break every signature.
func TestSigningPayload_IsOrderIndependentWhereOrderIsNotMeaning(t *testing.T) {
	a := validRelease()
	a.RequestedCapabilities = []string{"repo.read", "report.write"}
	a.ExecutionModes = []ReleaseMode{
		{ID: "quick", Name: "Quick", RiskLevel: "low", Capabilities: []string{"repo.read"}},
		{ID: "deep", Name: "Deep", RiskLevel: "medium", Capabilities: []string{"repo.read", "report.write"}},
	}
	b := a
	b.RequestedCapabilities = []string{"report.write", "repo.read"}
	b.ExecutionModes = []ReleaseMode{a.ExecutionModes[1], a.ExecutionModes[0]}

	at := time.Now().UTC()
	if !bytes.Equal(
		SigningPayload(a, SchemeEd25519V1, "k", at),
		SigningPayload(b, SchemeEd25519V1, "k", at),
	) {
		t.Fatal("reordering a capability or mode list changed the signed payload; a registry that " +
			"serialized its lists differently would break every signature")
	}
}

// The one that a naive "join the fields with a colon" encoding gets wrong.
func TestSigningPayload_NoSeparatorCollision(t *testing.T) {
	a := validRelease()
	a.Name = "Example"
	a.Version = "1.2.3"

	b := a
	// Move a character across the field boundary. Under any separator-based
	// encoding these two can be made to produce identical bytes; under
	// length-prefixing they cannot.
	b.Name = "Exampl"
	b.Description = "e" + a.Description

	at := time.Now().UTC()
	if bytes.Equal(
		SigningPayload(a, SchemeEd25519V1, "k", at),
		SigningPayload(b, SchemeEd25519V1, "k", at),
	) {
		t.Fatal("two different releases produced identical signed payloads; the encoding has a " +
			"separator a value can cross, which is a signature forged by choosing field values")
	}
}

// A one-element list and the scalar of the same value must not collide.
func TestCanonicalEncode_ScalarAndListAreDistinct(t *testing.T) {
	scalar := canonicalEncode("d", []canonicalField{scalarField("f", "x")})
	list := canonicalEncode("d", []canonicalField{listField("f", []string{"x"})})
	if bytes.Equal(scalar, list) {
		t.Fatal("a scalar and a one-element list encode identically")
	}
}

// Domain separation is what stops a release signature being replayed as a key
// certificate, which would let anybody holding one signed release mint a key.
func TestCanonicalEncode_DomainsDoNotCollide(t *testing.T) {
	fields := []canonicalField{scalarField("f", "x")}
	if bytes.Equal(
		canonicalEncode(releaseSignatureDomain, fields),
		canonicalEncode(keyCertificateDomain, fields),
	) {
		t.Fatal("two purposes encode identically; one signature would verify as the other")
	}
}

// Appending a field must not produce bytes a shorter reader accepts as a valid
// prefix -- which is why the field COUNT is written before the fields.
func TestCanonicalEncode_IsNotExtensibleByAppending(t *testing.T) {
	short := canonicalEncode("d", []canonicalField{scalarField("a", "1")})
	long := canonicalEncode("d", []canonicalField{scalarField("a", "1"), scalarField("b", "2")})
	if bytes.HasPrefix(long, short) {
		t.Fatal("a longer payload starts with a shorter valid one; a verifier reading fewer " +
			"fields than were signed would reconstruct an accepted prefix")
	}
}

// Absent and empty must encode identically, and both must differ from the
// field being gone -- which cannot happen, because the field list is fixed.
func TestSigningPayload_EmptyOptionalIsExplicit(t *testing.T) {
	a := validRelease()
	a.Compatibility.AOMaxVersion = ""
	b := a
	b.Compatibility.AOMaxVersion = "   "
	at := time.Now().UTC()
	if !bytes.Equal(
		SigningPayload(a, SchemeEd25519V1, "k", at),
		SigningPayload(b, SchemeEd25519V1, "k", at),
	) {
		t.Fatal("whitespace in an optional field changed the payload; a publisher and a verifier " +
			"that trim differently would disagree about what was signed")
	}
}

// The publisher's claimed time is inside the payload, so it cannot be moved to
// slip a signature into a key's validity window.
func TestSigningPayload_CoversSignedAt(t *testing.T) {
	rel := validRelease()
	one := SigningPayload(rel, SchemeEd25519V1, "k", time.Unix(1000, 0).UTC())
	two := SigningPayload(rel, SchemeEd25519V1, "k", time.Unix(2000, 0).UTC())
	if bytes.Equal(one, two) {
		t.Fatal("signedAt is outside the payload; backdating a signature would be free")
	}
}

// Sub-second precision is truncated, deliberately: a signer and a verifier that
// disagree about a trailing nanosecond disagree about the payload.
func TestCanonicalTime_TruncatesToSeconds(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if canonicalTime(base) != canonicalTime(base.Add(999*time.Millisecond)) {
		t.Fatal("sub-second precision reaches the payload")
	}
	if canonicalTime(time.Time{}) != "" {
		t.Fatal("a zero time must encode as empty, not as year 1")
	}
	// A non-UTC instant must encode as the same UTC moment.
	loc := time.FixedZone("test", 3600)
	if canonicalTime(base) != canonicalTime(base.In(loc)) {
		t.Fatal("the encoding depends on the signer's timezone")
	}
}
