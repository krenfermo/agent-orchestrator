package skills_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// trustedinstall_test.go -- the whole trusted path, over a real HTTPS server,
// through the real service, into the real catalog.
//
// These are the tests phase 12 is actually for. Nothing here stubs a provider,
// a digest or a signature: the fixture generates a keypair, signs what it will
// serve, AO fetches over TLS, hashes what lands and verifies the signature
// against a trust store it was configured with. That is the only arrangement
// in which "AO verified this" means anything.
//
// The private keys exist for the lifetime of one test and are never written
// down. See registrytest/signing.go.

// trustedFixture is a private HTTPS registry plus a configured trust root.
type trustedFixture struct {
	privateFixture
	trust *skills.TrustAuthority
	fix   registrytest.TrustFixture
}

const trustedPublisher = "corp"

func newTrustedFixture(
	t *testing.T, tier skillregistry.TrustTier, policy skillregistry.TrustPolicy,
) trustedFixture {
	t.Helper()
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	fix := registrytest.NewTrustFixture(t, "corp-root", trustedPublisher, tier, from)

	pf := newPrivateFixture(t, func(s *privateSetup) {
		s.Registry.TrustPolicy = policy
	})

	// An OFFICIAL-tier root cannot be created through the administrative API,
	// by design -- production gets its official anchors from the build. A test
	// stands in for that build by passing them as the built-in set, which is
	// the same seam a real release would use.
	var builtin []skillregistry.BuiltinRoot
	if tier == skillregistry.TierOfficial {
		builtin = append(builtin, fix.Builtin(t))
	}
	trust := skills.NewTrustAuthority(pf.store, builtin)

	if tier != skillregistry.TierOfficial {
		ctx := context.Background()
		if _, err := trust.SaveTrustRoot(ctx, skills.TrustRootRequest{
			ID: fix.Root.ID, DisplayName: fix.Root.DisplayName, Publisher: trustedPublisher,
			ValidFrom: from, Actor: admin, ActorPermissions: adminPerms(),
		}); err != nil {
			t.Fatalf("SaveTrustRoot: %v", err)
		}
		// The root key first, so the signing key can arrive with a certificate
		// the root actually signed -- which is what makes the chain
		// cryptographic rather than an administrator's assertion.
		if _, err := trust.AddSigningKey(ctx, skills.SigningKeyRequest{
			KeyID: fix.RootSigner.KeyID, TrustRootID: fix.Root.ID,
			PublicKey: fix.RootSigner.PublicKey(), IsRootKey: true, ValidFrom: from,
			Actor: admin, ActorPermissions: adminPerms(),
		}); err != nil {
			t.Fatalf("AddSigningKey(root): %v", err)
		}
		cert := fix.RootSigner.Certify(t, fix.Signer, from, nil)
		key, err := trust.AddSigningKey(ctx, skills.SigningKeyRequest{
			KeyID: fix.Signer.KeyID, TrustRootID: fix.Root.ID,
			PublicKey: fix.Signer.PublicKey(), ValidFrom: from, Certificate: &cert,
			Actor: admin, ActorPermissions: adminPerms(),
		})
		if err != nil {
			t.Fatalf("AddSigningKey(signing): %v", err)
		}
		if key.Origin != skillregistry.OriginCertificate {
			t.Fatalf("a key added with a valid certificate was recorded as %q; the two paths must "+
				"stay distinguishable", key.Origin)
		}
	}

	pf.mp = pf.mp.WithTrust(trust)
	return trustedFixture{privateFixture: pf, trust: trust, fix: fix}
}

// publishSigned puts a signed release on the fixture registry.
func (tf trustedFixture) publishSigned(t *testing.T, skillID, version string) skillregistry.Release {
	t.Helper()
	return tf.srv.PublishSigned(t, registrytest.Spec{
		SkillID: skillID, Version: version, Publisher: trustedPublisher,
	}, tf.fix.Signer, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
}

// TestTrustedInstallEndToEnd walks the phase brief's pilot, in order, as one
// test -- because the interesting failures are the ones that only appear when
// step 11 follows step 4.
func TestTrustedInstallEndToEnd(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	rel := tf.publishSigned(t, "security-audit", "1.0.0")
	ctx := context.Background()

	// 1-2. Search finds it, and BEFORE any bytes move its state is not
	//      trusted. A listing that showed trusted before an install would be
	//      describing a check nobody ran.
	found, err := tf.mp.Search(ctx, skills.SearchRequest{
		Query: skillregistry.Query{Text: "security"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 1 {
		t.Fatalf("search returned %d releases", len(found.Releases))
	}
	if got := found.Releases[0].Trust; got != skillregistry.TrustUnverified {
		t.Fatalf("a search result reported trust %q; a search moves no bytes and verifies "+
			"no signature, so there is nothing for it to have checked", got)
	}
	if !tf.srv.FetchedArtifact() == false {
		t.Fatal("searching fetched an artifact")
	}

	// 3-8. Install: both digests, then the signature, then the publisher, then
	//      the root -- and only then trusted.
	out, err := tf.install(t, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("InstallRelease: %v", err)
	}
	if !out.Verification.Verified {
		t.Fatalf("install did not verify the signature: %s", out.Verification.Refusal)
	}
	if out.Origin.TrustState != skillregistry.TrustTrusted {
		t.Fatalf("trust state = %q, want trusted", out.Origin.TrustState)
	}
	if out.Origin.Verification.KeyID != tf.fix.Signer.KeyID {
		t.Fatalf("provenance key = %q, want %q", out.Origin.Verification.KeyID, tf.fix.Signer.KeyID)
	}
	if out.Origin.Verification.TrustRootID != tf.fix.Root.ID {
		t.Fatalf("provenance root = %q", out.Origin.Verification.TrustRootID)
	}
	if out.Origin.Verification.KeyFingerprint != tf.fix.Signer.Fingerprint(t) {
		t.Fatal("the recorded fingerprint is not this key's")
	}
	if out.Origin.Verification.VerifiedAt.IsZero() {
		t.Fatal("provenance records no verification time")
	}
	if out.Origin.Verification.KeyOrigin != skillregistry.OriginCertificate {
		t.Fatalf("key origin = %q; the chain was certificate-based", out.Origin.Verification.KeyOrigin)
	}
	if !strings.Contains(out.Origin.RevocationStateObserved, "none-known") {
		t.Fatalf("revocation state observed = %q", out.Origin.RevocationStateObserved)
	}
	if out.Release.Version != rel.Version {
		t.Fatalf("installed %s, resolved %s", out.Release.Version, rel.Version)
	}

	// 9-10. Installed is not enabled, and no capability was granted. A
	//       trusted install moves an artifact exactly one state.
	assertInstalledNotEnabled(t, tf.privateFixture, "security-audit")

	// The audit trail says what happened, in the words an auditor looks for.
	actions := tf.auditActions(t)
	for _, want := range []string{"signing_key_seen", "signature_verified", "trusted_install"} {
		if !containsAction(actions, want) {
			t.Fatalf("audit is missing %q; recorded: %v", want, actions)
		}
	}
	// And it never says anything a private key could hide in.
	assertNoKeyMaterialInAudit(t, tf.privateFixture)
}

// A signature is not a substitute for the hash, and the hash is not a
// substitute for the signature. Each is checked over what it actually covers.
func TestTrustedInstall_RefusesTamperedBytesEvenWithAGoodSignature(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	tf.publishSigned(t, "security-audit", "1.0.0")

	// The registry serves different bytes than the ones it signed a digest
	// for. The signature still verifies -- it covers the DECLARED digest --
	// and the byte check catches it.
	tf.srv.Mutate("security-audit", "1.0.0", func(e *registrytest.Entry) {
		e.Files["extra.txt"] = "an extra file the publisher never signed for\n"
	})

	_, err := tf.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("tampered bytes installed under a good signature")
	}
	if code := apiCode(t, err); code != "SKILL_ARTIFACT_DIGEST_MISMATCH" {
		t.Fatalf("refusal code = %q (err %v)", code, err)
	}
}

// The registry alters the release's declared capabilities after the publisher
// signed it. The signature covers them, so it does not verify.
func TestTrustedInstall_RefusesAlteredCapabilities(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	tf.publishSigned(t, "security-audit", "1.0.0")
	tf.srv.Mutate("security-audit", "1.0.0", func(e *registrytest.Entry) {
		e.Release.RequestedCapabilities = []string{"repo.read", "report.write", "net.egress"}
	})

	_, err := tf.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("a release whose capabilities changed after signing was installed")
	}
	if code := apiCode(t, err); code != "SKILL_SIGNATURE_REFUSED" {
		t.Fatalf("refusal code = %q (err %v)", code, err)
	}
	if !containsAction(tf.auditActions(t), "trusted_install_refused") {
		t.Fatal("a refused trusted install left no trail")
	}
}

// An unsigned release under a signed policy is refused, and the refusal says
// the release is unsigned rather than that something failed.
func TestTrustedInstall_RefusesUnsignedUnderSignedPolicy(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	tf.srv.Publish(t, registrytest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: trustedPublisher,
	})
	_, err := tf.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("an unsigned release satisfied a signed policy")
	}
	if !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("the refusal did not say the release is unsigned: %v", err)
	}
	// Nothing was downloaded: the signature is checked over metadata, before
	// any bytes move.
	if tf.srv.FetchedArtifact() {
		t.Fatal("AO downloaded a package it was never going to keep")
	}
}

// A signature by a key that chains to a DIFFERENT publisher is a genuine
// signature by the wrong party, and that is publisher spoofing rather than a
// pass.
func TestTrustedInstall_RefusesWrongPublisher(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	// Signed correctly, then re-attributed to somebody else by the registry.
	tf.publishSigned(t, "security-audit", "1.0.0")
	tf.srv.Mutate("security-audit", "1.0.0", func(e *registrytest.Entry) {
		e.Release.Publisher = "somebody-else"
	})
	_, err := tf.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("a re-attributed release was installed")
	}
	if code := apiCode(t, err); code != "SKILL_SIGNATURE_REFUSED" {
		t.Fatalf("refusal code = %q", code)
	}
}

// An enterprise root satisfies `signed` and must NOT satisfy `official`.
func TestTrustedInstall_OfficialPolicyRefusesAnEnterpriseRoot(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	tf.publishSigned(t, "security-audit", "1.0.0")

	// Narrow the registry to official after the trust root was configured.
	reg := tf.reg
	reg.TrustPolicy = skillregistry.TrustPolicyOfficial
	tf.saveRegistry(t, reg)

	_, err := tf.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("an enterprise root satisfied the official policy")
	}
	if code := apiCode(t, err); code != "SKILL_TRUST_POLICY_UNSATISFIABLE" {
		t.Fatalf("refusal code = %q (err %v)", code, err)
	}
}

// The official path, exercised against a fixture root standing in for the
// build-embedded one this build deliberately does not carry.
func TestTrustedInstall_OfficialPolicyWithAnOfficialRoot(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierOfficial, skillregistry.TrustPolicyOfficial)
	tf.publishSigned(t, "security-audit", "1.0.0")

	out, err := tf.install(t, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("InstallRelease under the official policy: %v", err)
	}
	if out.Origin.TrustState != skillregistry.TrustTrusted {
		t.Fatalf("trust state = %q", out.Origin.TrustState)
	}
	if out.Origin.Verification.TrustRootTier != skillregistry.TierOfficial {
		t.Fatalf("tier = %q", out.Origin.Verification.TrustRootTier)
	}
	// A built-in root's keys are built-in, and that is a different assurance
	// from an administrator's say-so.
	if out.Origin.Verification.KeyOrigin != skillregistry.OriginBuiltIn {
		t.Fatalf("key origin = %q", out.Origin.Verification.KeyOrigin)
	}
}

// Key rotation: a new key overlaps the old one, both verify inside their
// windows, and retiring the old one leaves its earlier signatures intact.
func TestTrustedInstall_KeyRotation(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	ctx := context.Background()
	from := tf.fix.Root.ValidFrom

	// 1.0.0 signed by the original key.
	tf.publishSigned(t, "security-audit", "1.0.0")
	if _, err := tf.install(t, "security-audit", "1.0.0"); err != nil {
		t.Fatalf("install under the outgoing key: %v", err)
	}

	// The new key, authorized by the same root, overlapping the old one.
	next := registrytest.NewSigner(t, "corp-signing-key-2", trustedPublisher, tf.fix.Root.ID, false)
	cert := tf.fix.RootSigner.Certify(t, next, from, nil)
	if _, err := tf.trust.AddSigningKey(ctx, skills.SigningKeyRequest{
		KeyID: next.KeyID, TrustRootID: tf.fix.Root.ID, PublicKey: next.PublicKey(),
		ValidFrom: from, RotatedFromKeyID: tf.fix.Signer.KeyID, Certificate: &cert,
		Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("AddSigningKey(rotated): %v", err)
	}
	if !containsAction(tf.auditActions(t), "signing_key_rotated") {
		t.Fatal("a rotation was recorded as an ordinary key addition")
	}

	// 2.0.0 signed by the NEW key installs.
	tf.srv.PublishSigned(t, registrytest.Spec{
		SkillID: "security-audit", Version: "2.0.0", Publisher: trustedPublisher,
	}, next, time.Now().UTC().Add(-time.Minute).Truncate(time.Second))
	out, err := tf.install(t, "security-audit", "2.0.0")
	if err != nil {
		t.Fatalf("install under the incoming key: %v", err)
	}
	if out.Origin.Verification.KeyID != next.KeyID {
		t.Fatalf("2.0.0 verified against %q", out.Origin.Verification.KeyID)
	}

	// Retiring the outgoing key with a window that already closed refuses NEW
	// signatures made after it -- but a retirement, unlike a revocation, does
	// not repudiate what the key signed inside its window.
	if err := tf.trust.RetireSigningKey(
		ctx, tf.fix.Signer.KeyID, time.Now().UTC().Add(-30*time.Minute),
		admin, adminPerms(),
	); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}
	tf.srv.PublishSigned(t, registrytest.Spec{
		SkillID: "security-audit", Version: "3.0.0", Publisher: trustedPublisher,
	}, tf.fix.Signer, time.Now().UTC().Truncate(time.Second))
	if _, err := tf.install(t, "security-audit", "3.0.0"); err == nil {
		t.Fatal("a retired key signed a new release and AO accepted it")
	}
}

// Revoking a key blocks NEW trusted installs and removes nothing that is
// already here. That promise is the whole reason revocation is safe to act on.
func TestTrustedInstall_RevokedKeyBlocksNewInstallsAndKeepsExistingOnes(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	ctx := context.Background()
	tf.publishSigned(t, "security-audit", "1.0.0")
	if _, err := tf.install(t, "security-audit", "1.0.0"); err != nil {
		t.Fatalf("install: %v", err)
	}

	if _, err := tf.trust.Revoke(ctx, skills.RevokeRequest{
		Subject: skillregistry.SubjectSigningKey, SubjectID: tf.fix.Signer.KeyID,
		Reason: "the signing laptop was stolen", Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// A new release signed by the revoked key is refused -- including one
	// signed BEFORE the revocation, because a key somebody else may have held
	// proves nothing about who used it.
	tf.publishSigned(t, "security-audit", "2.0.0")
	if _, err := tf.install(t, "security-audit", "2.0.0"); err == nil {
		t.Fatal("a release signed by a revoked key was installed")
	}

	// The existing install is still there, untouched.
	installs, err := tf.store.ListSkillInstalls(ctx)
	if err != nil {
		t.Fatalf("ListSkillInstalls: %v", err)
	}
	var found bool
	for _, in := range installs {
		if in.Manifest.ID == "security-audit" && in.Manifest.Version == "1.0.0" {
			found = true
		}
	}
	if !found {
		t.Fatal("revoking a key UNINSTALLED an existing package; AO must mark, never remove")
	}
	// And its provenance still records what AO verified at the time.
	origin, ok, err := tf.store.GetSkillInstallOrigin(ctx, "security-audit", "1.0.0")
	if err != nil || !ok {
		t.Fatalf("GetSkillInstallOrigin: %v (ok=%v)", err, ok)
	}
	if origin.Verification.KeyID != tf.fix.Signer.KeyID {
		t.Fatal("revoking a key erased the provenance of what it had signed")
	}
}

// Revoking the ROOT refuses every key under it at once.
func TestTrustedInstall_RevokedRootBlocksEveryKeyUnderIt(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	ctx := context.Background()
	tf.publishSigned(t, "security-audit", "1.0.0")

	if _, err := tf.trust.Revoke(ctx, skills.RevokeRequest{
		Subject: skillregistry.SubjectTrustRoot, SubjectID: tf.fix.Root.ID,
		Reason: "the publishing organization was compromised",
		Actor:  admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := tf.install(t, "security-audit", "1.0.0"); err == nil {
		t.Fatal("a release chaining to a revoked root was installed")
	}
	if !containsAction(tf.auditActions(t), "trust_root_revoked") {
		t.Fatal("revoking a root left no trail")
	}
}

// The administrative surface must refuse to become AO Official, and must never
// accept key material it cannot use.
func TestTrustAuthority_ReservedAndMalformedInput(t *testing.T) {
	tf := newTrustedFixture(t, skillregistry.TierEnterprise, skillregistry.TrustPolicySigned)
	ctx := context.Background()

	if _, err := tf.trust.SaveTrustRoot(ctx, skills.TrustRootRequest{
		ID: skillregistry.OfficialTrustRootID, DisplayName: "Not AO", Publisher: "acme",
		Actor: admin, ActorPermissions: adminPerms(),
	}); err == nil {
		t.Fatal("an administrator created a root named ao-official")
	}
	if _, err := tf.trust.SaveTrustRoot(ctx, skills.TrustRootRequest{
		ID: "impostor", DisplayName: "Impostor", Publisher: skillregistry.OfficialPublisher,
		Actor: admin, ActorPermissions: adminPerms(),
	}); err == nil {
		t.Fatal("an administrator created a root publishing as ao")
	}

	// A private key pasted into the public-key field is refused on length,
	// rather than stored and regretted.
	if _, err := tf.trust.AddSigningKey(ctx, skills.SigningKeyRequest{
		KeyID: "pasted-wrong-half", TrustRootID: tf.fix.Root.ID,
		PublicKey: strings.Repeat("A", 88), // 64 bytes: an ed25519 PRIVATE key
		Actor:     admin, ActorPermissions: adminPerms(),
	}); err == nil {
		t.Fatal("a 64-byte key was accepted into the public-key field")
	}

	// Replacing the material under an existing key id is the substitution
	// attack with better manners.
	other := registrytest.NewSigner(t, tf.fix.Signer.KeyID, trustedPublisher, tf.fix.Root.ID, false)
	if _, err := tf.trust.AddSigningKey(ctx, skills.SigningKeyRequest{
		KeyID: tf.fix.Signer.KeyID, TrustRootID: tf.fix.Root.ID,
		PublicKey: other.PublicKey(), Actor: admin, ActorPermissions: adminPerms(),
	}); err == nil {
		t.Fatal("a key's material was replaced under the same key id")
	}

	// A settings.read holder cannot write.
	if _, err := tf.trust.SaveTrustRoot(ctx, skills.TrustRootRequest{
		ID: "reader-root", DisplayName: "Reader", Publisher: "reader",
		Actor: "member", ActorPermissions: nil,
	}); err == nil {
		t.Fatal("a caller without settings.manage configured a trust root")
	}
}

// assertInstalledNotEnabled holds the four-state ladder: installing moves an
// artifact exactly one step and grants nothing.
func assertInstalledNotEnabled(t *testing.T, pf privateFixture, skillID string) {
	t.Helper()
	ctx := context.Background()
	installs, err := pf.store.ListSkillInstalls(ctx)
	if err != nil {
		t.Fatalf("ListSkillInstalls: %v", err)
	}
	for _, in := range installs {
		if in.Manifest.ID != skillID {
			continue
		}
		activations, err := pf.store.ListSkillActivationsForSkillVersion(
			ctx, skillID, in.Manifest.Version)
		if err != nil {
			t.Fatalf("ListSkillActivationsForSkillVersion: %v", err)
		}
		if len(activations) > 0 {
			t.Fatalf("installing %s@%s enabled it on %d project(s) and granted capabilities; "+
				"an install moves an artifact exactly one state",
				skillID, in.Manifest.Version, len(activations))
		}
	}
}

// assertNoKeyMaterialInAudit is the leakage guard for this phase's trail.
//
// The audit records key IDs and fingerprints, both derived from public bytes
// and both meant to be compared out of band. What it must never carry is a
// private key or a registry credential, and the way that regresses is somebody
// widening a detail string to "include more context".
func assertNoKeyMaterialInAudit(t *testing.T, pf privateFixture) {
	t.Helper()
	entries, err := pf.store.ListSkillAuditForSkill(context.Background(), "security-audit")
	if err != nil {
		t.Fatalf("ListSkillAuditForSkill: %v", err)
	}
	registryEntries, err := pf.store.ListSkillAuditForSkill(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSkillAuditForSkill(registry rows): %v", err)
	}
	for _, e := range append(entries, registryEntries...) {
		if strings.Contains(e.Detail, privateRegistryToken) {
			t.Fatalf("audit row %q carries the registry credential", e.Action)
		}
		// An ed25519 private key is 64 bytes; base64 of one is 88 characters.
		// Nothing in this trail has a reason to carry a blob that long.
		for _, field := range strings.Fields(e.Detail) {
			if len(field) >= 80 && !strings.Contains(field, "/") {
				t.Fatalf("audit row %q carries an %d-character opaque blob: key material and "+
					"signatures do not belong in the trail", e.Action, len(field))
			}
		}
	}
}

var _ = store.SkillAuditTrustedInstall
