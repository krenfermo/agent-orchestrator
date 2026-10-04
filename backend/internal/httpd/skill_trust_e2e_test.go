package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/authz"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// skill_trust_e2e_test.go -- the phase-12 pilot, through the production router.
//
// It is the phase brief's local pilot written down as a test, in order, as one
// function: search, confirm nothing is trusted before any byte moves, install,
// verify both digests and the signature, confirm the chain, then break each
// link in turn and confirm the refusal. The interesting failures in a trust
// chain are the ones that only appear when step 11 follows step 4, so the steps
// stay in one test rather than becoming twenty independent ones.
//
// Everything is real: a TLS listener, AO's production HTTP client, the real
// router with the real permission gate, the real SQLite store and the real
// catalog install. The only thing a test supplies is the fixture's certificate
// authority and its own generated keypair, neither of which any configuration
// can reach.

const trustPilotPublisher = "corp"

type trustWorld struct {
	*marketplaceWorld
	registry *registrytest.Server
	fixture  registrytest.TrustFixture
}

func newTrustWorld(t *testing.T, tier skillregistry.TrustTier) *trustWorld {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dataDir)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })

	mk := func(name string, role domain.UserRole) domain.User {
		u, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
			DisplayName: name, Email: name + "@example.test", Username: name,
			Password: "correct-horse-" + name, Role: role,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return u
	}
	owner := mk("owner", domain.UserRoleOwner)
	member := mk("member", domain.UserRoleMember)
	viewer := mk("viewer", domain.UserRoleViewer)

	registry := registrytest.New(t, "company-private")
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	fixture := registrytest.NewTrustFixture(t, "corp-root", trustPilotPublisher, tier, from)

	// An OFFICIAL root cannot be created through the API, by design. A test
	// stands in for the build that would carry one, through the same seam a
	// real AO release would use.
	var builtin []skillregistry.BuiltinRoot
	if tier == skillregistry.TierOfficial {
		builtin = append(builtin, fixture.Builtin(t))
	}
	trust := skills.NewTrustAuthority(st, builtin)

	skillsSvc := skills.New(st, dataDir)
	factory := skillregistry.DefaultProviderFactory{Options: registry.Options()}
	marketplace := skills.NewMarketplace(st, skillsSvc, factory, "0.12.0", dataDir).
		WithConnectivity(st, nil).
		WithTrust(trust)

	deps := APIDeps{
		Auth:             authMgr,
		Projects:         &fakeProjectManager{items: map[domain.ProjectID]projectsvc.Project{}},
		ProjectOwnership: st,
		SessionOwnership: st,
		Authz:            authz.New(st),
		ProjectScope:     st,
		RBAC:             rbac.New(st, nil, rbac.NoopAudit{}, nil),
		Skills:           skillsSvc,
		SkillImages:      skillsSvc,
		SkillMarketplace: marketplace,
		SkillTrust:       trust,
		SkillTenancy:     st,
	}
	srv := httptest.NewServer(NewRouterWithControl(
		config.Config{TrustedLocalMode: false}, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)

	return &trustWorld{
		marketplaceWorld: &marketplaceWorld{
			t: t, store: st, srv: srv, client: &http.Client{},
			owner: owner, member: member, viewer: viewer,
		},
		registry: registry,
		fixture:  fixture,
	}
}

// configure saves the fixture as a private HTTPS registry on the given policy.
func (w *trustWorld) configure(cookie *http.Cookie, policy skillregistry.TrustPolicy) {
	w.t.Helper()
	w.expect(http.MethodPut, "/api/v1/skills/registries/company-private", cookie, `{
		"displayName": "Company Private",
		"type": "https",
		"location": "`+w.registry.BaseURL()+`",
		"enabled": true,
		"trustPolicy": "`+string(policy)+`",
		"permittedPrivateCidrs": ["127.0.0.0/8"]
	}`, http.StatusOK)
}

// seedTrust configures the root and its two keys over HTTP, as an
// administrator would: the root key first, then the signing key with a
// certificate the root key actually signed.
func (w *trustWorld) seedTrust(cookie *http.Cookie) {
	w.t.Helper()
	from := w.fixture.Root.ValidFrom
	w.expect(http.MethodPut, "/api/v1/skills/trust/roots/corp-root", cookie, `{
		"displayName": "Corp Publishing",
		"publisher": "`+trustPilotPublisher+`",
		"validFrom": "`+from.Format(time.RFC3339)+`"
	}`, http.StatusOK)

	// validFrom is passed EXPLICITLY. Omitting it means "now", which is the
	// fail-closed default -- a key added today does not retroactively vouch
	// for everything ever signed under its publisher's name, because that is
	// the backdating surface the window exists to close. An administrator
	// adopting an existing key sets the window deliberately, which is what
	// this does.
	w.expect(http.MethodPut, "/api/v1/skills/trust/keys/"+w.fixture.RootSigner.KeyID, cookie, `{
		"trustRootId": "corp-root",
		"publicKey": "`+w.fixture.RootSigner.PublicKey()+`",
		"isRootKey": true,
		"validFrom": "`+from.Format(time.RFC3339)+`"
	}`, http.StatusOK)

	cert := w.fixture.RootSigner.Certify(w.t, w.fixture.Signer, from, nil)
	certJSON, err := json.Marshal(cert)
	if err != nil {
		w.t.Fatalf("marshal certificate: %v", err)
	}
	w.expect(http.MethodPut, "/api/v1/skills/trust/keys/"+w.fixture.Signer.KeyID, cookie, `{
		"trustRootId": "corp-root",
		"publicKey": "`+w.fixture.Signer.PublicKey()+`",
		"validFrom": "`+from.Format(time.RFC3339)+`",
		"certificate": `+string(certJSON)+`
	}`, http.StatusOK)
}

func (w *trustWorld) publishSigned(skillID, version string) skillregistry.Release {
	w.t.Helper()
	return w.registry.PublishSigned(w.t, registrytest.Spec{
		SkillID: skillID, Version: version, Publisher: trustPilotPublisher,
	}, w.fixture.Signer, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
}

func (w *trustWorld) install(cookie *http.Cookie, skillID, version string, want int) string {
	w.t.Helper()
	return w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", cookie,
		`{"registryId":"company-private","skillId":"`+skillID+`","version":"`+version+`"}`, want)
}

// TestTrustRoutesPilot is the phase brief's twenty-step local pilot.
func TestTrustRoutesPilot(t *testing.T) {
	w := newTrustWorld(t, skillregistry.TierEnterprise)
	owner := w.login("owner")
	w.seedTrust(owner)
	w.configure(owner, skillregistry.TrustPolicySigned)
	w.publishSigned("security-audit", "1.0.0")

	// 1-2. Search finds it, and its state before any fetch is NOT trusted.
	//      A listing hashed nothing and checked no signature.
	body := w.expect(http.MethodGet, "/api/v1/skills/marketplace?q=security", owner, "", http.StatusOK)
	if !strings.Contains(body, `"trust":"unverified"`) {
		t.Fatalf("a search result claims more than unverified: %s", body)
	}
	if !strings.Contains(body, `"signed":true`) {
		t.Fatalf("the listing does not report that signature material is present: %s", body)
	}
	if w.registry.FetchedArtifact() {
		t.Fatalf("searching downloaded an artifact: %v", w.registry.Requests())
	}

	// 3-8. Install: both digests, the signature, the publisher, the root.
	body = w.install(owner, "security-audit", "1.0.0", http.StatusCreated)
	var outcome struct {
		Origin struct {
			Trust                   string
			TrustExplanation        string
			RevocationStateObserved string
			Provenance              struct {
				Verified                                    bool
				Algorithm, KeyID, KeyFingerprint, KeyOrigin string
				TrustRootID, TrustRootTier, Publisher       string
				VerifiedAt                                  time.Time
			}
		}
	}
	decodeInto(t, body, &outcome)

	if outcome.Origin.Trust != string(skillregistry.TrustTrusted) {
		t.Fatalf("trust = %q, want trusted: %s", outcome.Origin.Trust, body)
	}
	p := outcome.Origin.Provenance
	if !p.Verified || p.Algorithm != "ed25519" {
		t.Fatalf("provenance is not a verified ed25519 chain: %+v", p)
	}
	if p.KeyID != w.fixture.Signer.KeyID || p.KeyFingerprint != w.fixture.Signer.Fingerprint(t) {
		t.Fatalf("provenance names the wrong key: %+v", p)
	}
	if p.TrustRootID != "corp-root" || p.TrustRootTier != string(skillregistry.TierEnterprise) {
		t.Fatalf("provenance names the wrong root: %+v", p)
	}
	if p.Publisher != trustPilotPublisher {
		t.Fatalf("provenance names publisher %q", p.Publisher)
	}
	// The chain arrived through a certificate the root key signed, not through
	// somebody here saying so. The two are different assurances.
	if p.KeyOrigin != string(skillregistry.OriginCertificate) {
		t.Fatalf("key origin = %q", p.KeyOrigin)
	}
	if p.VerifiedAt.IsZero() {
		t.Fatal("provenance records no verification time")
	}
	if !strings.Contains(outcome.Origin.RevocationStateObserved, "none-known") {
		t.Fatalf("revocation state observed = %q", outcome.Origin.RevocationStateObserved)
	}
	// The word "trusted" must arrive with the sentence that bounds it.
	for _, phrase := range []string{"who signed", "does not say the code is safe"} {
		if !strings.Contains(strings.ToLower(outcome.Origin.TrustExplanation), phrase) {
			t.Fatalf("the trust explanation does not say what trusted is NOT: %q",
				outcome.Origin.TrustExplanation)
		}
	}

	// 9-10. Installed is not enabled anywhere, and no capability was granted.
	activations, err := w.store.ListSkillActivationsForSkillVersion(
		context.Background(), "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("ListSkillActivationsForSkillVersion: %v", err)
	}
	if len(activations) != 0 {
		t.Fatalf("a trusted install enabled the skill on %d project(s)", len(activations))
	}

	// 11a. Tampering with bytes AO has ALREADY verified cannot poison a
	//      re-install: the artifact cache is keyed on both digests and
	//      re-measured on every read, so AO serves its own verified copy and
	//      never notices the registry changed its mind about the bytes.
	//      That is the correct outcome and it is worth pinning: the failure
	//      mode it rules out is a registry swapping bytes under a release
	//      somebody already approved.
	w.registry.Mutate("security-audit", "1.0.0", func(e *registrytest.Entry) {
		e.Files["extra.txt"] = "bytes the publisher never signed for\n"
	})
	body = w.install(owner, "security-audit", "1.0.0", http.StatusCreated)
	if !strings.Contains(body, `"fromCache":true`) || !strings.Contains(body, `"trust":"trusted"`) {
		t.Fatalf("a re-install after the registry swapped its bytes did not come from AO's "+
			"verified cache: %s", body)
	}

	// 11b. The same tampering on a release AO has NEVER fetched. Here the
	//      bytes really do arrive, the signature still verifies -- it covers
	//      the DECLARED digest, which did not change -- and the byte check is
	//      what catches it. Integrity and provenance are separate controls and
	//      this is the case that proves neither substitutes for the other.
	w.publishSigned("security-audit", "1.1.0")
	w.registry.Mutate("security-audit", "1.1.0", func(e *registrytest.Entry) {
		e.Files["extra.txt"] = "bytes the publisher never signed for\n"
	})
	body = w.install(owner, "security-audit", "1.1.0", http.StatusForbidden)
	if !strings.Contains(body, "SKILL_ARTIFACT_DIGEST_MISMATCH") {
		t.Fatalf("tampered bytes were not refused on the digest: %s", body)
	}

	// 12-14. Alter the SIGNED description of the release: a digest, a
	//        capability, the version. Each breaks the signature.
	for _, tc := range []struct {
		name string
		edit func(*registrytest.Entry)
	}{
		{"manifest digest", func(e *registrytest.Entry) {
			e.Release.ManifestDigest = strings.Repeat("a", 64)
		}},
		{"capabilities", func(e *registrytest.Entry) {
			e.Release.RequestedCapabilities = []string{"repo.read", "report.write", "net.egress"}
		}},
		{"execution mode risk", func(e *registrytest.Entry) {
			e.Release.ExecutionModes[0].RiskLevel = "critical"
		}},
	} {
		t.Run("refuses altered "+tc.name, func(t *testing.T) {
			w.publishSigned("security-audit", "2.0.0")
			w.registry.Mutate("security-audit", "2.0.0", func(e *registrytest.Entry) { tc.edit(e) })
			out := w.install(owner, "security-audit", "2.0.0", http.StatusForbidden)
			if !strings.Contains(out, "SKILL_SIGNATURE_REFUSED") {
				t.Fatalf("altered %s was not refused on the signature: %s", tc.name, out)
			}
		})
	}

	// 15. Sign with a key AO does not hold.
	stranger := registrytest.NewSigner(t, "stranger-key", trustPilotPublisher, "corp-root", false)
	w.registry.PublishSigned(t, registrytest.Spec{
		SkillID: "security-audit", Version: "3.0.0", Publisher: trustPilotPublisher,
	}, stranger, time.Now().UTC().Truncate(time.Second))
	body = w.install(owner, "security-audit", "3.0.0", http.StatusForbidden)
	if !strings.Contains(body, "SKILL_SIGNATURE_REFUSED") {
		t.Fatalf("a release signed by an unknown key was not refused: %s", body)
	}

	// 16. Rotate correctly: a new key certified by the same root, and a
	//     release it signed becomes trusted.
	next := registrytest.NewSigner(t, "corp-signing-key-2", trustPilotPublisher, "corp-root", false)
	cert := w.fixture.RootSigner.Certify(t, next, w.fixture.Root.ValidFrom, nil)
	certJSON, err := json.Marshal(cert)
	if err != nil {
		t.Fatalf("marshal certificate: %v", err)
	}
	w.expect(http.MethodPut, "/api/v1/skills/trust/keys/"+next.KeyID, owner, `{
		"trustRootId": "corp-root",
		"publicKey": "`+next.PublicKey()+`",
		"validFrom": "`+w.fixture.Root.ValidFrom.Format(time.RFC3339)+`",
		"rotatedFromKeyId": "`+w.fixture.Signer.KeyID+`",
		"certificate": `+string(certJSON)+`
	}`, http.StatusOK)

	w.registry.PublishSigned(t, registrytest.Spec{
		SkillID: "security-audit", Version: "4.0.0", Publisher: trustPilotPublisher,
	}, next, time.Now().UTC().Truncate(time.Second))
	body = w.install(owner, "security-audit", "4.0.0", http.StatusCreated)
	if !strings.Contains(body, `"trust":"trusted"`) {
		t.Fatalf("a release signed by the rotated key did not reach trusted: %s", body)
	}

	// 17. Revoke the OLD key. A release signed by it is refused, including one
	//     signed before the revocation.
	w.expect(http.MethodPost, "/api/v1/skills/trust/revocations", owner, `{
		"subject": "signing_key",
		"subjectId": "`+w.fixture.Signer.KeyID+`",
		"reason": "the signing laptop was stolen"
	}`, http.StatusOK)

	w.publishSigned("security-audit", "5.0.0")
	body = w.install(owner, "security-audit", "5.0.0", http.StatusForbidden)
	if !strings.Contains(body, "SKILL_SIGNATURE_REFUSED") {
		t.Fatalf("a release signed by a revoked key was installed: %s", body)
	}

	// 18-19. The install that already succeeded is still here, and its
	//        provenance still records what AO verified at the time. AO marks;
	//        it never removes.
	origin, ok, err := w.store.GetSkillInstallOrigin(context.Background(), "security-audit", "1.0.0")
	if err != nil || !ok {
		t.Fatalf("the existing install lost its provenance: %v (ok=%v)", err, ok)
	}
	if origin.Verification.KeyID != w.fixture.Signer.KeyID || !origin.Verification.Verified {
		t.Fatalf("revoking a key rewrote history: %+v", origin.Verification)
	}

	// 20. Revoke the ROOT: every key under it, including the rotated one.
	w.expect(http.MethodPost, "/api/v1/skills/trust/revocations", owner, `{
		"subject": "trust_root",
		"subjectId": "corp-root",
		"reason": "the publishing organization was compromised"
	}`, http.StatusOK)

	w.registry.PublishSigned(t, registrytest.Spec{
		SkillID: "security-audit", Version: "6.0.0", Publisher: trustPilotPublisher,
	}, next, time.Now().UTC().Truncate(time.Second))
	body = w.install(owner, "security-audit", "6.0.0", http.StatusForbidden)
	if !strings.Contains(body, "SKILL_SIGNATURE_REFUSED") {
		t.Fatalf("a release chaining to a revoked root was installed: %s", body)
	}

	// Nothing in the pilot executed a skill. There is no Run verb on this
	// surface and no route that would give a client one.
	for _, req := range w.registry.Requests() {
		if strings.Contains(req, "/run") {
			t.Fatalf("the pilot reached a run endpoint: %v", w.registry.Requests())
		}
	}
}

// The Trust surface is a read for settings.read and a write for
// settings.manage, checked at the route as well as in the service.
func TestTrustRoutesPermissionGate(t *testing.T) {
	w := newTrustWorld(t, skillregistry.TierEnterprise)
	owner := w.login("owner")
	w.seedTrust(owner)

	viewer := w.login("viewer")
	// A viewer may look. The keys are public and their fingerprints are meant
	// to be compared out of band.
	body := w.expect(http.MethodGet, "/api/v1/skills/trust", viewer, "", http.StatusOK)
	if !strings.Contains(body, `"trustRootId":"corp-root"`) {
		t.Fatalf("a settings.read caller could not list trust roots: %s", body)
	}
	// And the response must never carry anything but public material.
	if strings.Contains(strings.ToLower(body), "privatekey") {
		t.Fatalf("the trust listing has a private-key field: %s", body)
	}

	member := w.login("member")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/v1/skills/trust/roots/sneaky", `{"displayName":"S","publisher":"s"}`},
		{http.MethodPut, "/api/v1/skills/trust/keys/sneaky-key",
			`{"trustRootId":"corp-root","publicKey":"AAAA"}`},
		{http.MethodPost, "/api/v1/skills/trust/revocations",
			`{"subject":"signing_key","subjectId":"corp-signing-key","reason":"no"}`},
	} {
		if status, out := w.do(tc.method, tc.path, member, tc.body); status != http.StatusForbidden {
			t.Fatalf("%s %s as a member returned %d: %s", tc.method, tc.path, status, out)
		}
	}
}

// The one name worth impersonating is the one nobody can claim, and the
// refusal happens at the API rather than only in the store.
func TestTrustRoutesRefuseReservedIdentity(t *testing.T) {
	w := newTrustWorld(t, skillregistry.TierEnterprise)
	owner := w.login("owner")

	body := w.expect(http.MethodPut, "/api/v1/skills/trust/roots/ao-official", owner,
		`{"displayName":"Not AO","publisher":"acme"}`, http.StatusForbidden)
	if !strings.Contains(body, "SKILL_TRUST_ROOT_RESERVED") {
		t.Fatalf("ao-official was not refused as reserved: %s", body)
	}

	body = w.expect(http.MethodPut, "/api/v1/skills/trust/roots/impostor", owner,
		`{"displayName":"Impostor","publisher":"ao"}`, http.StatusForbidden)
	if !strings.Contains(body, "SKILL_TRUST_PUBLISHER_RESERVED") {
		t.Fatalf("publisher ao was not refused as reserved: %s", body)
	}
}

// A private key pasted into the public-key field is refused on length, and the
// listing tells a person the official policy does not work on this build
// rather than leaving them to guess.
func TestTrustRoutesRefusePrivateKeyAndExplainOfficial(t *testing.T) {
	w := newTrustWorld(t, skillregistry.TierEnterprise)
	owner := w.login("owner")
	w.seedTrust(owner)

	// 88 base64 characters decode to 64 bytes: an ed25519 PRIVATE key.
	status, body := w.do(http.MethodPut, "/api/v1/skills/trust/keys/pasted-wrong-half", owner,
		`{"trustRootId":"corp-root","publicKey":"`+strings.Repeat("A", 88)+`"}`)
	if status == http.StatusOK {
		t.Fatalf("a 64-byte key was accepted into the public-key field: %s", body)
	}
	if !strings.Contains(body, "32 bytes") {
		t.Fatalf("the refusal does not say what a public key is: %s", body)
	}

	body = w.expect(http.MethodGet, "/api/v1/skills/trust", owner, "", http.StatusOK)
	if !strings.Contains(body, `"officialRootAvailable":false`) {
		t.Fatalf("this build claims an official root: %s", body)
	}
	if !strings.Contains(body, "officialRootNote") {
		t.Fatalf("the absence of an official root is not explained: %s", body)
	}
	if !strings.Contains(body, "trustModel") {
		t.Fatalf("the trust model is not served from the daemon: %s", body)
	}
}

// The official policy is configurable and unsatisfiable on a build with no
// official root, and the refusal says exactly why.
func TestTrustRoutesOfficialPolicyIsHonestlyUnsatisfiable(t *testing.T) {
	w := newTrustWorld(t, skillregistry.TierEnterprise)
	owner := w.login("owner")
	w.seedTrust(owner)
	w.configure(owner, skillregistry.TrustPolicyOfficial)
	w.publishSigned("security-audit", "1.0.0")

	body := w.install(owner, "security-audit", "1.0.0", http.StatusForbidden)
	if !strings.Contains(body, "SKILL_TRUST_POLICY_UNSATISFIABLE") {
		t.Fatalf("the official policy installed something: %s", body)
	}
	if !strings.Contains(body, "official signing key has not been published") {
		t.Fatalf("the refusal does not say why: %s", body)
	}

	// And the registry listing reports the policy as unenforceable, so a
	// settings screen does not present it as working.
	body = w.expect(http.MethodGet, "/api/v1/skills/registries", owner, "", http.StatusOK)
	if !strings.Contains(body, `"trustPolicyEnforceable":false`) {
		t.Fatalf("the registry listing claims the official policy is enforceable: %s", body)
	}
}

// With an official root present -- as a real AO release would carry -- the
// official policy reaches trusted through the same machinery.
func TestTrustRoutesOfficialPolicyWithABuiltInRoot(t *testing.T) {
	w := newTrustWorld(t, skillregistry.TierOfficial)
	owner := w.login("owner")
	w.configure(owner, skillregistry.TrustPolicyOfficial)
	w.publishSigned("security-audit", "1.0.0")

	body := w.install(owner, "security-audit", "1.0.0", http.StatusCreated)
	if !strings.Contains(body, `"trust":"trusted"`) {
		t.Fatalf("the official policy did not reach trusted: %s", body)
	}
	if !strings.Contains(body, `"trustRootTier":"official"`) {
		t.Fatalf("the provenance does not name the official tier: %s", body)
	}

	// A built-in root is visible and not administrable.
	body = w.expect(http.MethodGet, "/api/v1/skills/trust", owner, "", http.StatusOK)
	if !strings.Contains(body, `"builtIn":true`) {
		t.Fatalf("the built-in root is not marked as built in: %s", body)
	}
	out := w.expect(http.MethodPost, "/api/v1/skills/trust/revocations", owner, `{
		"subject": "trust_root", "subjectId": "corp-root", "reason": "trying it on"
	}`, http.StatusForbidden)
	if !strings.Contains(out, "SKILL_TRUST_ROOT_IMMUTABLE") {
		t.Fatalf("a built-in root was revocable from this host: %s", out)
	}
}
