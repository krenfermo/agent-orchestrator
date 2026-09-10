package skills_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/secretbox"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/githubtest"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// externalregistry_test.go -- the whole external path, over a real TLS
// listener, through the real service, into the real catalog.
//
// Nothing here stubs a provider, a digest, a commit or a signature. The
// fixture forge serves real archives at real commits; AO resolves the tag,
// fetches over TLS, unpacks under quarantine, hashes what lands and compares
// against what the descriptor at that commit declared. That is the only
// arrangement in which "AO verified this" means anything -- and, for this
// phase, the only one in which "the tag moved" can be demonstrated rather than
// asserted.
//
// Zero internet. The fixture is a loopback TLS server with its own CA; the
// registry names a reserved .test DNS name and the resolver points it at
// 127.0.0.1. Nothing in this file can reach github.com.

const externalOwner = "acme"
const externalRepo = "skills"
const externalPublisher = "acme"

// externalFixture is the service, the marketplace, and a forge on loopback.
type externalFixture struct {
	fixture
	mp    *skills.Marketplace
	trust *skills.TrustAuthority
	forge *githubtest.Server
	repo  *githubtest.Repo
	reg   skillregistry.Registry
	// signer is the enterprise release-signing key, when the fixture seeded a
	// trust root. Its private half exists for the life of one test and is
	// never written down -- see registrytest/signing.go.
	signer *registrytest.Signer
}

type externalSetup struct {
	Registry skillregistry.Registry
	// Private makes the repository private on the forge.
	Private bool
	// Trust seeds an enterprise trust root, so a signed external release can
	// reach TRUSTED.
	Trust bool
	// Secrets are sealed BEFORE the registry is saved, by NAME -- which is the
	// order an administrator works in and the order SaveRegistry requires,
	// since it opens a registry before recording it.
	Secrets map[string]string
}

func newExternalFixture(t *testing.T, opts ...func(*externalSetup)) externalFixture {
	t.Helper()
	f := newFixture(t)
	forge := githubtest.New(t)

	setup := externalSetup{
		Registry: forge.Registry("ext", externalOwner, externalRepo),
		Secrets:  map[string]string{},
	}
	for _, opt := range opts {
		opt(&setup)
	}
	repo := forge.AddRepo(externalOwner, externalRepo, setup.Private)

	// The real credential resolver over the real sealed store, so a private
	// repository's token travels the path an administrator would configure
	// rather than a stub that cannot leak.
	secrets := skills.NewRegistrySecrets(f.store, secretbox.New(f.dataDir))
	factory := skillregistry.DefaultProviderFactory{Secrets: secrets, Options: forge.Options()}
	trust := skills.NewTrustAuthority(f.store, nil)
	mp := skills.NewMarketplace(f.store, f.svc, factory, aoTestVersion, f.dataDir).
		WithConnectivity(f.store, secrets).
		WithTrust(trust).
		// The ledger. Without it an external install is refused outright --
		// see externalAvailable: installing from a forge with no way to
		// remember where the tag pointed would mean AO could never afterwards
		// tell that it had moved.
		WithExternal(f.store)

	ef := externalFixture{fixture: f, mp: mp, trust: trust, forge: forge, repo: repo, reg: setup.Registry}
	for name, value := range setup.Secrets {
		auth := skills.NewSecretAuthority(f.store, secretbox.New(f.dataDir))
		if _, err := auth.RegisterSecret(context.Background(), name, "forge credential",
			skillsecrets.NewSecretValue(value), admin); err != nil {
			t.Fatalf("RegisterSecret: %v", err)
		}
	}
	if setup.Trust {
		ef.seedTrustRoot(t)
	}
	ef.saveRegistry(t, setup.Registry)
	return ef
}

func (ef *externalFixture) seedTrustRoot(t *testing.T) registrytest.TrustFixture {
	t.Helper()
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	fix := registrytest.NewTrustFixture(t, "acme-root", externalPublisher,
		skillregistry.TierEnterprise, from)
	ctx := context.Background()
	if _, err := ef.trust.SaveTrustRoot(ctx, skills.TrustRootRequest{
		ID: fix.Root.ID, DisplayName: fix.Root.DisplayName, Publisher: externalPublisher,
		ValidFrom: from, Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("SaveTrustRoot: %v", err)
	}
	if _, err := ef.trust.AddSigningKey(ctx, skills.SigningKeyRequest{
		KeyID: fix.RootSigner.KeyID, TrustRootID: fix.Root.ID,
		PublicKey: fix.RootSigner.PublicKey(), IsRootKey: true, ValidFrom: from,
		Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("AddSigningKey(root): %v", err)
	}
	cert := fix.RootSigner.Certify(t, fix.Signer, from, nil)
	if _, err := ef.trust.AddSigningKey(ctx, skills.SigningKeyRequest{
		KeyID: fix.Signer.KeyID, TrustRootID: fix.Root.ID,
		PublicKey: fix.Signer.PublicKey(), ValidFrom: from, Certificate: &cert,
		Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("AddSigningKey(signing): %v", err)
	}
	ef.signer = fix.Signer
	return fix
}

func (ef externalFixture) saveRegistry(t *testing.T, reg skillregistry.Registry) {
	t.Helper()
	if _, err := ef.mp.SaveRegistry(context.Background(), skills.RegistryRequest{
		Registry: reg, Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
}

func (ef externalFixture) publish(t *testing.T, spec githubtest.Spec) githubtest.Published {
	t.Helper()
	if spec.Publisher == "" {
		spec.Publisher = externalPublisher
	}
	return ef.repo.Publish(t, spec)
}

func (ef externalFixture) install(
	t *testing.T, skillID, version string, edit ...func(*skills.InstallReleaseRequest),
) (skills.InstallOutcome, error) {
	t.Helper()
	req := skills.InstallReleaseRequest{
		RegistryID: ef.reg.ID, SkillID: skillID, Version: version,
		Actor: admin, ActorPermissions: adminPerms(),
	}
	for _, e := range edit {
		e(&req)
	}
	return ef.mp.InstallRelease(context.Background(), req)
}

func (ef externalFixture) auditActions(t *testing.T) []string {
	t.Helper()
	entries, err := ef.store.ListSkillAuditForSkill(context.Background(), "security-audit")
	if err != nil {
		t.Fatalf("ListSkillAuditForSkill: %v", err)
	}
	registryEntries, err := ef.store.ListSkillAuditForSkill(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSkillAuditForSkill(registry rows): %v", err)
	}
	out := make([]string, 0, len(entries)+len(registryEntries))
	for _, e := range append(entries, registryEntries...) {
		out = append(out, string(e.Action))
	}
	return out
}

// TestExternalRegistryEndToEnd walks the phase brief's pilot, in order, as one
// test -- because the interesting failures are the ones that only appear when
// step 15 follows step 6.
func TestExternalRegistryEndToEnd(t *testing.T) {
	ef := newExternalFixture(t)
	ctx := context.Background()
	published := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	// 2-3. Search, and confirm it downloaded no archive.
	ef.forge.ResetRequests()
	found, err := ef.mp.Search(ctx, skills.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 1 {
		t.Fatalf("search returned %d releases with notes %+v", len(found.Releases), found.Notes)
	}
	if ef.forge.FetchedArchive() {
		t.Fatal("a search fetched an archive; searching must move no package bytes")
	}
	row := found.Releases[0]
	if row.Trust != skillregistry.TrustUnverified {
		t.Fatalf("a search result reported trust %q; a search hashed nothing", row.Trust)
	}

	// 4-5. The release resolves to an immutable commit, and the marketplace
	//      carries it.
	if row.Release.Source.Commit != published.Commit {
		t.Fatalf("search pinned %q, want %q", row.Release.Source.Commit, published.Commit)
	}
	if row.Release.Source.Tag != published.Tag {
		t.Fatalf("search recorded tag %q, want %q", row.Release.Source.Tag, published.Tag)
	}
	if row.Release.Source.Visibility != "public" {
		t.Fatalf("visibility is %q", row.Release.Source.Visibility)
	}

	// 6-9. Install: the commit, the artifact digest and the manifest digest
	//      are all checked against the bytes that landed.
	outcome, err := ef.install(t, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("InstallRelease: %v", err)
	}
	if outcome.Origin.Source.Commit != published.Commit {
		t.Fatalf("provenance recorded commit %q, want %q",
			outcome.Origin.Source.Commit, published.Commit)
	}
	if outcome.Origin.ArtifactDigest != published.Release.ArtifactDigest ||
		outcome.Origin.ManifestDigest != published.Release.ManifestDigest {
		t.Fatalf("provenance digests %+v do not match the published release", outcome.Origin)
	}

	// 10. Unsigned reaches VERIFIED and stops there. Hosting is not authorship.
	if outcome.Origin.TrustState != skillregistry.TrustVerified {
		t.Fatalf("an unsigned external install reached %q; being on a forge is not a signature",
			outcome.Origin.TrustState)
	}
	if outcome.Verification.Verified {
		t.Fatal("an unsigned release reported a verified signature")
	}

	// 12. The provenance carries repository, commit, publisher and the chain.
	if outcome.Origin.Source.Slug() != externalOwner+"/"+externalRepo {
		t.Fatalf("provenance repository is %q", outcome.Origin.Source.Slug())
	}
	if outcome.Origin.SourceFetchedAt == nil {
		t.Fatal("provenance recorded no fetch time for a forge install")
	}
	if outcome.Identity.CryptographicallyBound() {
		t.Fatal("an unsigned release claimed a cryptographic publisher binding")
	}

	// 20-22. Installing enabled nothing, granted nothing and ran nothing.
	activations, err := ef.store.ListSkillActivationsForSkillVersion(ctx, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("ListSkillActivationsForSkillVersion: %v", err)
	}
	if len(activations) != 0 {
		t.Fatalf("installing enabled %d project activations", len(activations))
	}
	actions := ef.auditActions(t)
	for _, forbidden := range []string{"run_executed", "enable", "grant_changed", "image_approved"} {
		if containsAction(actions, forbidden) {
			t.Fatalf("an install produced a %s audit row", forbidden)
		}
	}
	if !containsAction(actions, "external_release_pinned") {
		t.Fatalf("no external_release_pinned row; audit actions were %v", actions)
	}
}

// TestExternalMovedTagIsRefusedAndRecorded is FASE J end to end, and it is the
// test this whole phase exists for.
func TestExternalMovedTagIsRefusedAndRecorded(t *testing.T) {
	ef := newExternalFixture(t)
	ctx := context.Background()
	spec := githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"}
	first := ef.publish(t, spec)

	if _, err := ef.install(t, "security-audit", "1.0.0"); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// The force-push: same tag, same version, different commit and different
	// bytes.
	spec.Extra = "a second build of the same version"
	second := ef.repo.Republish(t, spec, githubtest.CommitSHA("compromised"))
	if second.Commit == first.Commit {
		t.Fatal("the fixture did not actually move the tag")
	}

	// 14. The marketplace says so.
	found, err := ef.mp.Search(ctx, skills.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 1 {
		t.Fatalf("search returned %d releases", len(found.Releases))
	}
	row := found.Releases[0]
	if !row.TagMoved {
		t.Fatal("the marketplace did not mark a tag that moved under it")
	}
	if row.TagMovedFrom != first.Commit {
		t.Fatalf("the marketplace says the tag moved from %q, want %q",
			row.TagMovedFrom, first.Commit)
	}

	// 15. The existing install keeps the commit it came from. Its provenance
	//     is a statement about this host and is not rewritten by somebody
	//     else's force-push.
	origin, ok, err := ef.mp.InstallOrigin(ctx, "security-audit", "1.0.0")
	if err != nil || !ok {
		t.Fatalf("InstallOrigin: %v (found=%v)", err, ok)
	}
	if origin.Source.Commit != first.Commit {
		t.Fatalf("the installed release's provenance now says %q; it must still say %q",
			origin.Source.Commit, first.Commit)
	}

	// 16. A silent reinstall is refused.
	_, err = ef.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("reinstalling through a moved tag succeeded silently")
	}
	if code := apiCode(t, err); code != "SKILL_EXTERNAL_TAG_MOVED" &&
		code != "SKILL_EXTERNAL_COMMIT_CHANGED" {
		t.Fatalf("the refusal code is %q; it must name the move", code)
	}

	// And it is audited, whether or not anything was installed.
	if !containsAction(ef.auditActions(t), "external_tag_moved") {
		t.Fatalf("no external_tag_moved row; actions were %v", ef.auditActions(t))
	}

	// And acknowledging the move does NOT let it overwrite the copy already on
	// this host. Acknowledging says "I mean to take the new commit"; it does
	// not say "and replace the bytes I already have under the same version
	// number". One version means one set of bytes here, forever.
	_, err = ef.install(t, "security-audit", "1.0.0", func(r *skills.InstallReleaseRequest) {
		r.AcknowledgeMovedTag = true
	})
	if err == nil {
		t.Fatal("acknowledging a moved tag overwrote an installed version's bytes")
	}
	if code := apiCode(t, err); code != "SKILL_EXTERNAL_COMMIT_CHANGED" {
		t.Fatalf("the refusal code is %q; it must name the commit change rather than send "+
			"somebody to look at package contents", code)
	}
	if origin.Source.Commit != first.Commit {
		t.Fatal("the provenance moved")
	}
}

// TestExternalAcknowledgedMovedTagInstallsANewVersion is the other half: a
// moved tag is not a permanent block, and taking the new commit for a version
// this host does not have is an explicit decision that works.
func TestExternalAcknowledgedMovedTagInstallsANewVersion(t *testing.T) {
	ef := newExternalFixture(t)
	ctx := context.Background()

	// A first release establishes the ledger for this repository, and a second
	// one under its own tag is what gets re-pointed.
	ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	spec := githubtest.Spec{SkillID: "security-audit", Version: "1.1.0"}
	first := ef.publish(t, spec)
	if _, err := ef.mp.Search(ctx, skills.SearchRequest{}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	spec.Extra = "a rebuilt 1.1.0"
	second := ef.repo.Republish(t, spec, githubtest.CommitSHA("rebuilt"))
	if second.Commit == first.Commit {
		t.Fatal("the fixture did not move the tag")
	}

	if _, err := ef.install(t, "security-audit", "1.1.0"); err == nil {
		t.Fatal("a moved tag installed silently")
	}
	out, err := ef.install(t, "security-audit", "1.1.0", func(r *skills.InstallReleaseRequest) {
		r.AcknowledgeMovedTag = true
	})
	if err != nil {
		t.Fatalf("acknowledged install: %v", err)
	}
	// It skips no check: the new commit's bytes are hashed against the new
	// commit's digests exactly as the first one's would have been.
	if out.Origin.Source.Commit != second.Commit {
		t.Fatalf("recorded commit %q, want %q", out.Origin.Source.Commit, second.Commit)
	}
	if out.Origin.ArtifactDigest != second.Release.ArtifactDigest {
		t.Fatal("the acknowledged install did not verify against the new commit's digest")
	}
	if out.Origin.TrustState != skillregistry.TrustVerified {
		t.Fatalf("trust state is %q", out.Origin.TrustState)
	}
}

// TestExternalSignedReachesTrusted is FASE D case 3 and pilot step 11: a
// signature chaining to a root this installation configured is what makes an
// external release trusted, and nothing else is.
func TestExternalSignedReachesTrusted(t *testing.T) {
	ef := newExternalFixture(t, func(s *externalSetup) {
		s.Trust = true
		s.Registry.TrustPolicy = skillregistry.TrustPolicyExternalSigned
	})
	published := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	signed := ef.repo.SignDescriptor(t, published.Commit, published.Release, ef.reg.ID,
		ef.signer, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))

	outcome, err := ef.install(t, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("InstallRelease: %v", err)
	}
	if outcome.Origin.TrustState != skillregistry.TrustTrusted {
		t.Fatalf("a signed external install reached %q, want trusted (refusal: %s)",
			outcome.Origin.TrustState, outcome.Verification.Refusal)
	}
	if outcome.Verification.TrustRootTier != skillregistry.TierEnterprise {
		t.Fatalf("the chain reports tier %q", outcome.Verification.TrustRootTier)
	}
	if !outcome.Identity.CryptographicallyBound() {
		t.Fatal("a verified signature did not bind the declared publisher")
	}
	if signed.Signature.KeyID == "" {
		t.Fatal("the fixture published no signature")
	}
	// And the sentence a screen renders never says the code is safe.
	explanation := outcome.Identity.Describe()
	for _, forbidden := range []string{"safe", "secure", "audited", "reviewed and"} {
		if strings.Contains(strings.ToLower(explanation), forbidden) {
			t.Fatalf("the identity sentence claims %q: %s", forbidden, explanation)
		}
	}
}

// TestExternalUnsignedUnderSignedPolicyIsRefused is the other half: a policy
// that demands a signature refuses a release that has none, before any bytes
// move.
func TestExternalUnsignedUnderSignedPolicyIsRefused(t *testing.T) {
	ef := newExternalFixture(t, func(s *externalSetup) {
		s.Trust = true
		s.Registry.TrustPolicy = skillregistry.TrustPolicyExternalSigned
	})
	ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	ef.forge.ResetRequests()

	_, err := ef.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("an unsigned release installed under external_signed")
	}
	if code := apiCode(t, err); code != "SKILL_SIGNATURE_REFUSED" {
		t.Fatalf("refusal code is %q", code)
	}
	if ef.forge.FetchedArchive() {
		t.Fatal("AO downloaded an archive it was never going to keep")
	}
}

// TestExternalPolicyMatrix is FASE E, one table.
func TestExternalPolicyMatrix(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(*externalSetup)
		wantErr string
	}{
		{
			name: "integrity installs and reaches verified",
			edit: func(s *externalSetup) {
				s.Registry.TrustPolicy = skillregistry.TrustPolicyExternalIntegrity
			},
		},
		{
			name: "deny keeps the registry visible and installs nothing",
			edit: func(s *externalSetup) {
				s.Registry.TrustPolicy = skillregistry.TrustPolicyExternalDeny
			},
			wantErr: "SKILL_EXTERNAL_DENIED",
		},
		{
			name: "org allowlist admits its own owner",
			edit: func(s *externalSetup) {
				s.Registry.TrustPolicy = skillregistry.TrustPolicyExternalOrgAllowlist
				s.Registry.AllowedOwners = []string{externalOwner}
			},
		},
		{
			name: "publisher pin refuses another name",
			edit: func(s *externalSetup) {
				s.Registry.TrustPolicy = skillregistry.TrustPolicyExternalIntegrity
				s.Registry.PinnedPublisher = "globex"
			},
			wantErr: "SKILL_EXTERNAL_PUBLISHER_MISMATCH",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ef := newExternalFixture(t, tc.edit)
			ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
			out, err := ef.install(t, "security-audit", "1.0.0")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("install: %v", err)
				}
				// The load-bearing assertion of the whole phase: none of these
				// policies reaches trusted on its own.
				if out.Origin.TrustState != skillregistry.TrustVerified {
					t.Fatalf("policy %s produced trust %q; only a signature reaches trusted",
						ef.reg.TrustPolicy, out.Origin.TrustState)
				}
				return
			}
			if err == nil {
				t.Fatalf("install succeeded and should have been refused with %s", tc.wantErr)
			}
			if code := apiCode(t, err); code != tc.wantErr {
				t.Fatalf("refusal code is %q, want %q", code, tc.wantErr)
			}
		})
	}
}

// TestExternalAllowlistIsNotTrust is the sentence FASE D refuses to let anybody
// write: an allowlisted org installs, and it installs as VERIFIED.
func TestExternalAllowlistIsNotTrust(t *testing.T) {
	ef := newExternalFixture(t, func(s *externalSetup) {
		s.Trust = true
		s.Registry.TrustPolicy = skillregistry.TrustPolicyExternalOrgAllowlist
		s.Registry.AllowedOwners = []string{externalOwner, "globex"}
	})
	ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	out, err := ef.install(t, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if out.Origin.TrustState == skillregistry.TrustTrusted {
		t.Fatal("an allowlisted owner produced TRUSTED with no signature; " +
			"an allowlist is permission to install, not a cryptographic fact")
	}
}

// TestExternalRevocationBlocksNewInstalls is FASE K: three blast radii, and
// none of them removes anything.
func TestExternalRevocationBlocksNewInstalls(t *testing.T) {
	cases := []struct {
		name    string
		subject skillregistry.RevocationSubject
		id      func(githubtest.Published) string
	}{
		{"owner", skillregistry.SubjectExternalOwner,
			func(githubtest.Published) string { return externalOwner }},
		{"repository", skillregistry.SubjectExternalRepository,
			func(githubtest.Published) string { return externalOwner + "/" + externalRepo }},
		{"commit", skillregistry.SubjectExternalCommit,
			func(p githubtest.Published) string {
				return externalOwner + "/" + externalRepo + "@" + p.Commit
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ef := newExternalFixture(t)
			ctx := context.Background()
			published := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})

			if _, err := ef.trust.RevokeExternal(ctx, skills.ExternalRevokeRequest{
				Subject: tc.subject, SubjectID: tc.id(published),
				Reason:           "an advisory somebody here read",
				Actor:            admin,
				ActorPermissions: adminPerms(),
			}); err != nil {
				t.Fatalf("RevokeExternal: %v", err)
			}

			_, err := ef.install(t, "security-audit", "1.0.0")
			if err == nil {
				t.Fatalf("installing from a revoked %s succeeded", tc.name)
			}
			if code := apiCode(t, err); code != "SKILL_EXTERNAL_REVOKED" {
				t.Fatalf("refusal code is %q", code)
			}
			// The marketplace still SHOWS it. Hiding it would answer "why can
			// I not install this" with silence.
			found, err := ef.mp.Search(ctx, skills.SearchRequest{})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(found.Releases) != 1 || !found.Releases[0].ExternalRevoked {
				t.Fatalf("the marketplace did not mark the revoked row: %+v", found.Releases)
			}
		})
	}
}

// TestExternalRevocationCanBeLiftedAndKeysCannot is the asymmetry FASE K
// implies and that matters more than it looks.
func TestExternalRevocationCanBeLiftedAndKeysCannot(t *testing.T) {
	ef := newExternalFixture(t)
	ctx := context.Background()
	ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	if _, err := ef.trust.RevokeExternal(ctx, skills.ExternalRevokeRequest{
		Subject:   skillregistry.SubjectExternalRepository,
		SubjectID: externalOwner + "/" + externalRepo, Reason: "under investigation",
		Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("RevokeExternal: %v", err)
	}
	if err := ef.trust.LiftExternalRevocation(ctx, skillregistry.SubjectExternalRepository,
		externalOwner+"/"+externalRepo, admin, adminPerms()); err != nil {
		t.Fatalf("LiftExternalRevocation: %v", err)
	}
	if _, err := ef.install(t, "security-audit", "1.0.0"); err != nil {
		t.Fatalf("install after lifting: %v", err)
	}

	// A signing key is not liftable, and the refusal says why.
	err := ef.trust.LiftExternalRevocation(ctx, skillregistry.SubjectSigningKey,
		"some-key", admin, adminPerms())
	if err == nil {
		t.Fatal("a signing-key revocation was lifted through the external surface")
	}
	if code := apiCode(t, err); code != "SKILL_EXTERNAL_LIFT_SUBJECT" {
		t.Fatalf("refusal code is %q", code)
	}
}

// TestExternalNegativeMatrix is FASE N: every remaining way an external
// install must fail closed, in one table.
func TestExternalNegativeMatrix(t *testing.T) {
	cases := []struct {
		name string
		// setup publishes and then breaks something.
		setup func(t *testing.T, ef externalFixture) githubtest.Published
		// wantCode is the apierr code, or empty when any refusal will do.
		wantCode string
	}{
		{
			name: "artifact digest mismatch",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				return ef.publish(t, githubtest.Spec{
					SkillID: "security-audit", Version: "1.0.0",
					MutateRelease: func(rel *skillregistry.Release) {
						rel.ArtifactDigest = strings.Repeat("b", 64)
					},
				})
			},
			wantCode: "SKILL_ARTIFACT_DIGEST_MISMATCH",
		},
		{
			name: "manifest digest mismatch",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				return ef.publish(t, githubtest.Spec{
					SkillID: "security-audit", Version: "1.0.0",
					MutateRelease: func(rel *skillregistry.Release) {
						rel.ManifestDigest = strings.Repeat("c", 64)
					},
				})
			},
			wantCode: "SKILL_MANIFEST_DIGEST_MISMATCH",
		},
		{
			name: "publisher declared in the release disagrees with the manifest",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				return ef.publish(t, githubtest.Spec{
					SkillID: "security-audit", Version: "1.0.0",
					MutateRelease: func(rel *skillregistry.Release) {
						rel.Publisher = "globex"
					},
				})
			},
			wantCode: "SKILL_PUBLISHER_MISMATCH",
		},
		{
			name: "the descriptor understates a capability",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				return ef.publish(t, githubtest.Spec{
					SkillID: "security-audit", Version: "1.0.0",
					MutateRelease: func(rel *skillregistry.Release) {
						rel.RequestedCapabilities = []string{"repo.read"}
						rel.ExecutionModes[0].Capabilities = []string{"repo.read"}
					},
				})
			},
			wantCode: "SKILL_CAPABILITY_MISMATCH",
		},
		{
			name: "the forge serves an archive of a different commit",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				good := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				other := ef.publish(t, githubtest.Spec{
					SkillID: "security-audit", Version: "1.1.0", Extra: "different bytes",
				})
				ef.forge.Fail(githubtest.Failures{ArchiveOfCommit: other.Commit})
				return good
			},
			wantCode: "SKILL_ARTIFACT_DIGEST_MISMATCH",
		},
		{
			name: "the archive hides a symlink",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				p := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				ef.forge.Fail(githubtest.Failures{ArchiveSymlink: true})
				return p
			},
			wantCode: "SKILL_ARTIFACT_UNFETCHABLE",
		},
		{
			name: "the archive traverses out of the package",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				p := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				ef.forge.Fail(githubtest.Failures{ArchiveTraversal: true})
				return p
			},
			wantCode: "SKILL_ARTIFACT_UNFETCHABLE",
		},
		{
			name: "the archive is a decompression bomb",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				p := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				ef.forge.Fail(githubtest.Failures{ArchiveBomb: true})
				return p
			},
			wantCode: "SKILL_ARTIFACT_UNFETCHABLE",
		},
		{
			name: "the forge rate-limits the install",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				p := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				ef.forge.Fail(githubtest.Failures{RateLimited: true})
				return p
			},
		},
		{
			name: "the forge is down",
			setup: func(t *testing.T, ef externalFixture) githubtest.Published {
				p := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				ef.forge.Stop()
				return p
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ef := newExternalFixture(t)
			tc.setup(t, ef)
			_, err := ef.install(t, "security-audit", "1.0.0")
			if err == nil {
				t.Fatal("the install succeeded and every case here must fail closed")
			}
			if tc.wantCode != "" {
				if code := apiCode(t, err); code != tc.wantCode {
					t.Fatalf("refusal code is %q, want %q (%v)", code, tc.wantCode, err)
				}
			}
			// Nothing reached the catalog, whichever way it failed.
			installs, listErr := ef.store.ListSkillInstalls(context.Background())
			if listErr != nil {
				t.Fatalf("ListSkillInstalls: %v", listErr)
			}
			for _, rec := range installs {
				if rec.Manifest.ID == "security-audit" && rec.Manifest.Version == "1.0.0" {
					t.Fatal("a refused install reached the catalog")
				}
			}
		})
	}
}

// TestExternalTokenNeverLeaks is FASE G's leakage half, at the service level.
func TestExternalTokenNeverLeaks(t *testing.T) {
	const token = "ghp-do-not-print-me"
	ef := newExternalFixture(t, func(s *externalSetup) {
		s.Private = true
		s.Registry.AuthType = skillregistry.AuthBearer
		s.Registry.CredentialSecretName = "FORGE_TOKEN"
		s.Secrets["FORGE_TOKEN"] = token
	})
	ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	// The right credential reads a private repository, and the provenance
	// records it as private.
	ef.forge.RequireBearer(token)
	out, err := ef.install(t, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("install from a private repository: %v", err)
	}
	if out.Origin.Source.Visibility != "private" {
		t.Fatalf("visibility recorded as %q", out.Origin.Source.Visibility)
	}

	// The WRONG credential is a refusal that names the ref and never the value.
	ef.forge.RequireBearer("some-other-token")
	_, err = ef.mp.Search(context.Background(), skills.SearchRequest{})
	found, _ := ef.mp.Search(context.Background(), skills.SearchRequest{})
	messages := []string{}
	if err != nil {
		messages = append(messages, err.Error())
	}
	for _, note := range found.Notes {
		messages = append(messages, note.Reason)
	}
	if len(messages) == 0 {
		t.Fatal("a rejected credential produced neither an error nor a note")
	}
	for _, msg := range messages {
		if strings.Contains(msg, token) {
			t.Fatalf("a message quoted the credential value: %s", msg)
		}
	}
	// And every audit line, which is the other place a token would surface.
	entries, auditErr := ef.store.ListSkillAuditForSkill(context.Background(), "security-audit")
	if auditErr != nil {
		t.Fatalf("ListSkillAuditForSkill: %v", auditErr)
	}
	for _, e := range entries {
		if strings.Contains(e.Detail, token) {
			t.Fatalf("an audit row quoted the credential value: %s", e.Detail)
		}
	}
}

// TestExternalRegistryConfigurationRefusals is FASE M's validation, which is
// where a dangerous allowlist is stopped rather than in a screen.
func TestExternalRegistryConfigurationRefusals(t *testing.T) {
	ef := newExternalFixture(t)
	base := ef.forge.Registry("ext2", externalOwner, externalRepo)
	cases := []struct {
		name string
		edit func(*skillregistry.Registry)
	}{
		{"a wildcard allowlist", func(r *skillregistry.Registry) {
			r.TrustPolicy = skillregistry.TrustPolicyExternalOrgAllowlist
			r.AllowedOwners = []string{"*"}
		}},
		{"a regular expression allowlist", func(r *skillregistry.Registry) {
			r.TrustPolicy = skillregistry.TrustPolicyExternalOrgAllowlist
			r.AllowedOwners = []string{"^acme.*$"}
		}},
		{"an empty allowlist", func(r *skillregistry.Registry) {
			r.TrustPolicy = skillregistry.TrustPolicyExternalOrgAllowlist
		}},
		{"a private-registry policy on a forge", func(r *skillregistry.Registry) {
			r.TrustPolicy = skillregistry.TrustPolicyDigest
		}},
		{"no owner", func(r *skillregistry.Registry) { r.Owner = "" }},
		{"an allowlist nothing enforces", func(r *skillregistry.Registry) {
			r.AllowedOwners = []string{"acme"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := base
			tc.edit(&reg)
			_, err := ef.mp.SaveRegistry(context.Background(), skills.RegistryRequest{
				Registry: reg, Actor: admin, ActorPermissions: adminPerms(),
			})
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if apiCode(t, err) == "" {
				t.Fatalf("%s was refused without a code: %v", tc.name, err)
			}
		})
	}
}

// TestExternalCrossTenantRegistryIsInvisible holds the tenant boundary for the
// new registry type.
//
// Invisible means NOT FOUND rather than forbidden, deliberately: a
// tenant-scoped registry's existence is itself information, and 403 on one
// tells a stranger which organizations are reading which forges.
func TestExternalCrossTenantRegistryIsInvisible(t *testing.T) {
	ef := newExternalFixture(t)
	ctx := context.Background()
	ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	other := domain.TenantID("another-org")
	_, _, err := ef.mp.GetRelease(ctx, ef.reg.ID, "security-audit", "1.0.0", admin,
		[]domain.TenantID{other})
	if err != nil {
		// An installation-wide registry stays visible to everybody, which is
		// the case here: the assertion below is the one that matters.
		t.Fatalf("an installation-wide registry was hidden from a tenant member: %v", err)
	}

	// Now scope it, and it disappears for anybody outside.
	now := time.Now().UTC().Truncate(time.Second)
	if _, tErr := ef.store.InsertTenant(ctx, domain.Tenant{
		ID: "acme-org", Name: "Acme", Slug: "acme", Status: domain.TenantStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); tErr != nil {
		t.Fatalf("InsertTenant: %v", tErr)
	}
	scoped := ef.reg
	scoped.TenantID = "acme-org"
	if _, saveErr := ef.mp.SaveRegistry(ctx, skills.RegistryRequest{
		Registry: scoped, Actor: admin, ActorPermissions: adminPerms(),
		ActorTenants: []domain.TenantID{"acme-org"},
	}); saveErr != nil {
		t.Fatalf("SaveRegistry(tenant-scoped): %v", saveErr)
	}
	_, _, err = ef.mp.GetRelease(ctx, ef.reg.ID, "security-audit", "1.0.0", admin,
		[]domain.TenantID{other})
	if err == nil {
		t.Fatal("a tenant-scoped external registry was readable from another tenant")
	}
	if code := apiCode(t, err); code != "SKILL_REGISTRY_NOT_FOUND" {
		t.Fatalf("the refusal is %q; a private registry's existence must not leak", code)
	}
}

// TestExternalSignatureByAnUnknownKeyIsRefused is FASE N's "unknown signing
// key" and "wrong root", which land in the same place and must not be
// mistakable for a forgery of the bytes.
func TestExternalSignatureByAnUnknownKeyIsRefused(t *testing.T) {
	ef := newExternalFixture(t, func(s *externalSetup) {
		s.Trust = true
		s.Registry.TrustPolicy = skillregistry.TrustPolicyExternalSigned
	})
	published := ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	// A key AO has never been given, under a root AO does not hold. The
	// signature is genuine; it simply chains to nothing here.
	stranger := registrytest.NewSigner(t, "stranger-key", externalPublisher, "stranger-root", false)
	ef.repo.SignDescriptor(t, published.Commit, published.Release, ef.reg.ID,
		stranger, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))

	_, err := ef.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("a signature by a key AO does not hold produced an install")
	}
	if code := apiCode(t, err); code != "SKILL_SIGNATURE_REFUSED" {
		t.Fatalf("refusal code is %q", code)
	}
	// AO does not learn keys from what it reads. A verifier that adopted the
	// key it was handed would be a verifier an attacker teaches.
	if _, ok, keyErr := ef.trust.GetSigningKey(context.Background(), "stranger-key"); ok ||
		keyErr != nil {
		t.Fatalf("AO adopted a key from a release it was checking (found=%v err=%v)", ok, keyErr)
	}
}

// TestExternalOfflineForgeDoesNotSilentlyInstall is the offline half. An
// install acts on what the forge says NOW, and a copy is not consent.
func TestExternalOfflineForgeDoesNotSilentlyInstall(t *testing.T) {
	ef := newExternalFixture(t)
	ctx := context.Background()
	ef.publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	// Warm every cache the read path has.
	if _, err := ef.mp.Search(ctx, skills.SearchRequest{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	ef.forge.Stop()

	_, err := ef.install(t, "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("an install proceeded against a forge that could not be reached")
	}
	// And a search still ANSWERS, from cache, marked offline -- because "AO
	// could not ask" and "there is nothing there" are different answers and
	// only one of them means somebody should go and look.
	found, err := ef.mp.Search(ctx, skills.SearchRequest{})
	if err != nil {
		t.Fatalf("an offline search failed outright: %v", err)
	}
	if !found.Offline() && len(found.Notes) == 0 {
		t.Fatal("an offline search reported neither offline nor a note")
	}
	for _, row := range found.Releases {
		if !row.Metadata.Offline {
			t.Fatalf("a cached row was rendered as current: %+v", row.Metadata)
		}
	}
}

// TestExternalOfflineInstallIsPinnedByTheLedger closes the one path where a
// substituted commit could otherwise be taken silently.
//
// The artifact cache is addressed by digest, which is right, but the OFFLINE
// lookup asks for "whatever I have for skillId@version" -- and on a forge a
// moved tag means that question has two honest answers.
func TestExternalOfflineInstallIsPinnedByTheLedger(t *testing.T) {
	ef := newExternalFixture(t)
	ctx := context.Background()
	spec := githubtest.Spec{SkillID: "security-audit", Version: "1.0.0"}
	first := ef.publish(t, spec)

	// Install once so AO holds verified bytes for 1.0.0 and a ledger row that
	// says which commit they came from.
	if _, err := ef.install(t, "security-audit", "1.0.0"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := ef.svc.Uninstall(ctx, "security-audit", "1.0.0", admin); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// With the forge up, the tag moves and AO records it.
	spec.Extra = "a second build of the same version"
	second := ef.repo.Republish(t, spec, githubtest.CommitSHA("second-offline"))
	if _, err := ef.mp.Search(ctx, skills.SearchRequest{}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Now the forge is unreachable and the person asks for the cached bytes.
	// Those bytes are the FIRST commit; the ledger says the tag now means the
	// second. AO refuses rather than installing one while calling it the other.
	ef.forge.Stop()
	_, err := ef.install(t, "security-audit", "1.0.0", func(r *skills.InstallReleaseRequest) {
		r.AllowOfflineFromCache = true
	})
	if err == nil {
		t.Fatal("an offline install took cached bytes from a commit the tag no longer means")
	}
	if code := apiCode(t, err); code != "SKILL_EXTERNAL_TAG_MOVED" {
		t.Fatalf("refusal code is %q (%v)", code, err)
	}
	if !strings.Contains(err.Error(), first.Commit) ||
		!strings.Contains(err.Error(), second.Commit) {
		t.Fatalf("the refusal does not name both commits: %v", err)
	}
}
