package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/secretbox"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// privateregistry_test.go -- the whole path, over a real HTTPS server.
//
// These are the tests the phase is actually for. They run AO's production
// client against a real TLS listener, through the real service, into the real
// catalog: configure a registry, test the connection, search, install, go
// offline, come back, publish a revocation, sync it.
//
// Nothing here stubs a Provider or a digest. The fixture serves bytes and AO
// hashes what lands, which is the only arrangement in which "AO verified this"
// means anything.

const privateRegistryToken = "corp-registry-token"

// privateFixture is the service, the marketplace, and a private registry on
// loopback behind its own certificate authority.
type privateFixture struct {
	fixture
	mp  *skills.Marketplace
	srv *registrytest.Server
	reg skillregistry.Registry
}

// privateSetup is what a test wants configured BEFORE the registry is saved.
//
// The order matters and is the reason this exists: SaveRegistry opens the
// registry before recording it, and opening one with a credential resolves that
// credential -- so a secret a registry names has to be sealed first, exactly as
// an administrator would do it.
type privateSetup struct {
	Registry skillregistry.Registry
	// Secrets are sealed before the registry is saved, by NAME.
	Secrets map[string]string
}

func newPrivateFixture(t *testing.T, opts ...func(*privateSetup)) privateFixture {
	t.Helper()
	f := newFixture(t)
	srv := registrytest.New(t, "corp")

	setup := privateSetup{Registry: srv.Registry("corp"), Secrets: map[string]string{}}
	for _, opt := range opts {
		opt(&setup)
	}
	// The real credential resolver over the real sealed store. A test that
	// used a stub here would not exercise the rule that a secret is reachable
	// only through the registry it was attached to.
	secrets := skills.NewRegistrySecrets(f.store, secretbox.New(f.dataDir))
	factory := skillregistry.DefaultProviderFactory{Secrets: secrets, Options: srv.Options()}
	mp := skills.NewMarketplace(f.store, f.svc, factory, aoTestVersion, f.dataDir).
		WithConnectivity(f.store, secrets)

	pf := privateFixture{fixture: f, mp: mp, srv: srv, reg: setup.Registry}
	for name, value := range setup.Secrets {
		pf.seedSecret(t, name, value)
	}
	pf.saveRegistry(t, setup.Registry)
	return pf
}

func (pf privateFixture) saveRegistry(t *testing.T, reg skillregistry.Registry) {
	t.Helper()
	if _, err := pf.mp.SaveRegistry(context.Background(), skills.RegistryRequest{
		Registry: reg, Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
}

// seedSecret registers a sealed credential the way an administrator would.
func (pf privateFixture) seedSecret(t *testing.T, name, value string) {
	t.Helper()
	auth := skills.NewSecretAuthority(pf.store, secretbox.New(pf.dataDir))
	if _, err := auth.RegisterSecret(context.Background(), name, "registry credential",
		skillsecrets.NewSecretValue(value), admin); err != nil {
		t.Fatalf("RegisterSecret: %v", err)
	}
}

func (pf privateFixture) install(
	t *testing.T, skillID, version string, edit ...func(*skills.InstallReleaseRequest),
) (skills.InstallOutcome, error) {
	t.Helper()
	req := skills.InstallReleaseRequest{
		RegistryID: pf.reg.ID, SkillID: skillID, Version: version,
		Actor: admin, ActorPermissions: adminPerms(),
	}
	for _, e := range edit {
		e(&req)
	}
	return pf.mp.InstallRelease(context.Background(), req)
}

func containsAction(actions []string, want string) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}

func (pf privateFixture) auditActions(t *testing.T) []string {
	t.Helper()
	entries, err := pf.store.ListSkillAuditForSkill(context.Background(), "security-audit")
	if err != nil {
		t.Fatalf("ListSkillAuditForSkill: %v", err)
	}
	// Registry-level rows carry no skill id, so they are read separately: the
	// trail is one table and the two questions have two queries.
	registryEntries, err := pf.store.ListSkillAuditForSkill(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSkillAuditForSkill(registry rows): %v", err)
	}
	entries = append(entries, registryEntries...)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, string(e.Action))
	}
	return out
}

// TestPrivateRegistryEndToEnd walks the phase brief's own local end-to-end
// list, in order, as one test — because the interesting failures are the ones
// that only appear when step 12 follows step 7.
func TestPrivateRegistryEndToEnd(t *testing.T) {
	// The registry demands a credential, and the credential is sealed before
	// the registry is saved -- which is the order an administrator works in,
	// and the order SaveRegistry requires, because it opens a registry before
	// recording it.
	pf := newPrivateFixture(t, func(s *privateSetup) {
		s.Registry.AuthType = skillregistry.AuthBearer
		s.Registry.CredentialSecretName = "CORP_REGISTRY_TOKEN"
		s.Secrets["CORP_REGISTRY_TOKEN"] = privateRegistryToken
	})
	pf.srv.RequireBearer(privateRegistryToken)
	rel := pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	ctx := context.Background()

	// 3. Test connection.
	probe, err := pf.mp.TestConnection(ctx, pf.reg.ID, admin, adminPerms(), nil)
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if probe.State != skillregistry.ProbeConnected {
		t.Fatalf("connection test said %s: %s", probe.State, probe.Detail)
	}

	// 4. Search metadata. 5. Confirm no artifact was downloaded.
	found, err := pf.mp.Search(ctx, skills.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 1 || found.Releases[0].Release.Ref() != rel.Ref() {
		t.Fatalf("search returned %+v with notes %+v", found.Releases, found.Notes)
	}
	if found.Releases[0].Trust != skillregistry.TrustUnverified {
		t.Fatalf("a search result reported trust %q; a search hashed nothing",
			found.Releases[0].Trust)
	}
	if pf.srv.FetchedArtifact() {
		t.Fatalf("searching downloaded an artifact: %v", pf.srv.Requests())
	}

	// 6/7. Open the release and install it.
	outcome, err := pf.install(t, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("InstallRelease: %v", err)
	}
	if !pf.srv.FetchedArtifact() {
		t.Fatal("the install did not fetch an artifact")
	}

	// 8. The bytes, the digests and the provenance are AO's own measurements.
	if outcome.Origin.ArtifactDigest != rel.ArtifactDigest ||
		outcome.Origin.ManifestDigest != rel.ManifestDigest {
		t.Fatalf("recorded digests %s/%s do not match the release",
			outcome.Origin.ArtifactDigest, outcome.Origin.ManifestDigest)
	}
	if outcome.Origin.RegistryID != pf.reg.ID || outcome.Origin.RegistryType != "https" {
		t.Fatalf("provenance names %s (%s)", outcome.Origin.RegistryID, outcome.Origin.RegistryType)
	}

	// 9. VERIFIED, never TRUSTED. AO verified no signature.
	if outcome.Origin.TrustState != skillregistry.TrustVerified {
		t.Fatalf("trust state %q, want verified", outcome.Origin.TrustState)
	}

	// 10. Installed but not enabled anywhere.
	// Installed is not enabled: no project resolves this skill, and there is
	// no call on the marketplace that would make one.
	activations, err := pf.store.ListSkillActivationsForSkillVersion(ctx, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("ListSkillActivationsForSkillVersion: %v", err)
	}
	if len(activations) != 0 {
		t.Fatalf("installing from a registry enabled it somewhere: %+v", activations)
	}

	// 11/12. Turn the fixture off and confirm offline behaviour.
	pf.srv.Stop()
	if _, err := pf.install(t, "security-audit", "1.0.0"); err == nil {
		t.Fatal("an install proceeded with the registry down and no offline request")
	} else if code := apiCode(t, err); code != "SKILL_REGISTRY_UNREACHABLE" {
		t.Fatalf("offline install refused with %s, want SKILL_REGISTRY_UNREACHABLE", code)
	}
	// With the explicit ask, the cached bytes are usable -- and they are
	// re-verified, not trusted because they are on disk.
	offline, err := pf.install(t, "security-audit", "1.0.0",
		func(r *skills.InstallReleaseRequest) { r.AllowOfflineFromCache = true })
	if err != nil {
		t.Fatalf("offline install from cache: %v", err)
	}
	if !offline.Offline || !offline.FromCache {
		t.Fatalf("the offline install reported offline=%v fromCache=%v", offline.Offline, offline.FromCache)
	}
	if offline.Origin.TrustState != skillregistry.TrustVerified {
		t.Fatalf("a cached install reported trust %q", offline.Origin.TrustState)
	}

	actions := pf.auditActions(t)
	for _, want := range []string{
		"registry_added", "registry_connection_tested", "skill_fetch_started",
		"install", "cached_artifact_used",
	} {
		if !containsAction(actions, want) {
			t.Fatalf("the audit trail has no %s: %v", want, actions)
		}
	}
}

// TestRevocationAfterInstall covers the brief's steps 14-17, plus the two
// operational cases around them: a registry that cannot be asked, and its
// recovery.
func TestRevocationAfterInstall(t *testing.T) {
	pf := newPrivateFixture(t)
	pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	ctx := context.Background()

	if _, err := pf.install(t, "security-audit", "1.0.0"); err != nil {
		t.Fatalf("install: %v", err)
	}

	// The registry withdraws it AFTER it was installed.
	pf.srv.Revoke("security-audit", "1.0.0", "a dependency shipped a backdoor")
	sync, err := pf.mp.SyncRevocations(ctx, pf.reg.ID, admin, adminPerms(), nil)
	if err != nil {
		t.Fatalf("SyncRevocations: %v", err)
	}
	if sync.Fetched != 1 || sync.NewlyRecorded != 1 {
		t.Fatalf("sync fetched %d, %d new", sync.Fetched, sync.NewlyRecorded)
	}
	if len(sync.AffectedInstalls) != 1 || sync.AffectedInstalls[0] != "security-audit@1.0.0" {
		t.Fatalf("sync affected %v", sync.AffectedInstalls)
	}

	// 16. Installed AND revoked, both visible. AO removed nothing.
	origin, ok, err := pf.mp.InstallOrigin(ctx, "security-audit", "1.0.0")
	if err != nil || !ok {
		t.Fatalf("InstallOrigin: %v (found=%v)", err, ok)
	}
	if !origin.Revoked() {
		t.Fatal("the installed release was not marked revoked")
	}
	installs, err := pf.store.ListSkillInstalls(ctx)
	if err != nil {
		t.Fatalf("ListSkillInstalls: %v", err)
	}
	if len(installs) != 1 {
		t.Fatalf("the revocation changed the installed set: %d installs", len(installs))
	}

	// 17. A new install of that exact release is blocked.
	if _, err := pf.install(t, "security-audit", "1.0.0"); err == nil {
		t.Fatal("a revoked release installed")
	} else if code := apiCode(t, err); code != "SKILL_RELEASE_REVOKED" {
		t.Fatalf("a revoked install refused with %s", code)
	}

	// And it stays blocked while the registry is unreachable, from the record
	// AO already holds. Silence is not consent.
	pf.srv.Stop()
	_, err = pf.install(t, "security-audit", "1.0.0",
		func(r *skills.InstallReleaseRequest) { r.AllowOfflineFromCache = true })
	if err == nil {
		t.Fatal("a known-revoked release installed offline from cache")
	}
	if code := apiCode(t, err); code != "SKILL_RELEASE_REVOKED" {
		t.Fatalf("the offline refusal was %s, want SKILL_RELEASE_REVOKED", code)
	}

	// A sync against a registry that is down changes nothing and says so.
	down, err := pf.mp.SyncRevocations(ctx, pf.reg.ID, admin, adminPerms(), nil)
	if err != nil {
		t.Fatalf("SyncRevocations while down: %v", err)
	}
	if down.Unreachable == "" {
		t.Fatal("a sync against a stopped registry reported success")
	}
	rows, err := pf.mp.RegistryRevocations(ctx, pf.reg.ID, nil)
	if err != nil {
		t.Fatalf("RegistryRevocations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the unreachable sync changed what AO knows: %+v", rows)
	}
}

// TestRevocationBetweenSearchAndInstall is the race the pre-install re-read
// exists for. The search saw an installable release; by the time somebody
// clicked install, it was withdrawn.
func TestRevocationBetweenSearchAndInstall(t *testing.T) {
	pf := newPrivateFixture(t)
	pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	found, err := pf.mp.Search(context.Background(), skills.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 1 {
		t.Fatalf("search returned %d releases", len(found.Releases))
	}
	pf.srv.Revoke("security-audit", "1.0.0", "withdrawn between the search and the click")

	if _, err := pf.install(t, "security-audit", "1.0.0"); err == nil {
		t.Fatal("the install acted on what the search saw")
	} else if code := apiCode(t, err); code != "SKILL_RELEASE_REVOKED" {
		t.Fatalf("refused with %s, want SKILL_RELEASE_REVOKED", code)
	}
	if pf.srv.FetchedArtifact() {
		t.Fatal("a revoked release was fetched before being refused")
	}
}

// TestRegistryDisappearsAfterSearch is the ordinary operational case, kept
// distinct from a refusal: it must not read as "this registry has nothing".
func TestRegistryDisappearsAfterSearch(t *testing.T) {
	pf := newPrivateFixture(t)
	pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	if _, err := pf.mp.Search(context.Background(), skills.SearchRequest{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	pf.srv.Remove("security-audit", "1.0.0")

	if _, err := pf.install(t, "security-audit", "1.0.0"); err == nil {
		t.Fatal("a release the registry no longer offers installed")
	} else if code := apiCode(t, err); code != "SKILL_RELEASE_NOT_FOUND" && code != "SKILL_NOT_OFFERED" {
		t.Fatalf("refused with %s", code)
	}
}

// TestOfflineCannotInstallSomethingNeverFetched is the offline rule that
// matters most: cache is evidence, not a substitute for it.
func TestOfflineCannotInstallSomethingNeverFetched(t *testing.T) {
	pf := newPrivateFixture(t)
	pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	pf.srv.Stop()

	_, err := pf.install(t, "security-audit", "1.0.0",
		func(r *skills.InstallReleaseRequest) { r.AllowOfflineFromCache = true })
	if err == nil {
		t.Fatal("a release AO never fetched installed offline")
	}
	if code := apiCode(t, err); code != "SKILL_NOT_IN_CACHE" {
		t.Fatalf("refused with %s, want SKILL_NOT_IN_CACHE", code)
	}
}

// TestSupplyChainRefusals is the table of lying registries. Each row is a real
// attacker move, and each must fail closed with the bytes never reaching the
// catalog.
func TestSupplyChainRefusals(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, pf privateFixture)
		wantAny []string
	}{
		{
			name: "the declared artifact digest is not the bytes served",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{
					SkillID: "security-audit", Version: "1.0.0",
					MutateRelease: func(r *skillregistry.Release) {
						r.ArtifactDigest = strings.Repeat("a", 64)
					},
				})
			},
			wantAny: []string{"SKILL_ARTIFACT_DIGEST_MISMATCH"},
		},
		{
			name: "the declared manifest digest is not the manifest served",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{
					SkillID: "security-audit", Version: "1.0.0",
					MutateRelease: func(r *skillregistry.Release) {
						r.ManifestDigest = strings.Repeat("b", 64)
					},
				})
			},
			wantAny: []string{"SKILL_MANIFEST_DIGEST_MISMATCH"},
		},
		{
			name: "the listing understates what the package asks for",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{
					SkillID: "security-audit", Version: "1.0.0",
					MutateRelease: func(r *skillregistry.Release) {
						r.RequestedCapabilities = []string{"repo.read"}
						r.ExecutionModes[0].Capabilities = []string{"repo.read"}
					},
				})
			},
			wantAny: []string{"SKILL_CAPABILITY_MISMATCH"},
		},
		{
			name: "the listing names a publisher the package does not",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{
					SkillID: "security-audit", Version: "1.0.0",
					MutateRelease: func(r *skillregistry.Release) { r.Publisher = "somebody-else" },
				})
			},
			wantAny: []string{"SKILL_PUBLISHER_MISMATCH"},
		},
		{
			name: "the package is a different build under the same version",
			arrange: func(t *testing.T, pf privateFixture) {
				// Installed once honestly, then the registry republishes the
				// same identity with different bytes. The catalog refuses to
				// let one version mean two things.
				pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				if _, err := pf.install(t, "security-audit", "1.0.0"); err != nil {
					t.Fatalf("the honest install failed: %v", err)
				}
				pf.srv.Publish(t, registrytest.Spec{
					SkillID: "security-audit", Version: "1.0.0", Extra: "a second build\n",
				})
			},
			wantAny: []string{"SKILL_VERSION_CONTENT_CHANGED"},
		},
		{
			name: "the artifact carries a symlink out of the package",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				pf.srv.Fail(registrytest.Failures{ArtifactSymlink: true})
			},
			wantAny: []string{"SKILL_ARTIFACT_UNFETCHABLE"},
		},
		{
			name: "the artifact escapes the quarantine",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				pf.srv.Fail(registrytest.Failures{ArtifactTraversal: true})
			},
			wantAny: []string{"SKILL_ARTIFACT_UNFETCHABLE"},
		},
		{
			name: "the artifact is a decompression bomb",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				pf.srv.Fail(registrytest.Failures{ArtifactBomb: true})
			},
			wantAny: []string{"SKILL_ARTIFACT_UNFETCHABLE"},
		},
		{
			name: "the registry redirects the download somewhere else",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				pf.srv.Fail(registrytest.Failures{
					RedirectArtifactTo: "https://evil.example.com/package.tgz",
				})
			},
			wantAny: []string{"SKILL_ARTIFACT_UNFETCHABLE"},
		},
		{
			name: "the registry redirects the download at cloud metadata",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				pf.srv.Fail(registrytest.Failures{
					RedirectArtifactTo: "http://169.254.169.254/latest/meta-data/",
				})
			},
			wantAny: []string{"SKILL_ARTIFACT_UNFETCHABLE"},
		},
		{
			name: "the registry stops accepting the credential mid-install",
			arrange: func(t *testing.T, pf privateFixture) {
				pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
				pf.srv.Fail(registrytest.Failures{Unauthorized: true})
			},
			wantAny: []string{"SKILL_RELEASE_LOOKUP_FAILED", "SKILL_ARTIFACT_UNFETCHABLE"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := newPrivateFixture(t)
			tc.arrange(t, pf)

			_, err := pf.install(t, "security-audit", "1.0.0")
			if err == nil {
				t.Fatal("the install was accepted")
			}
			code := apiCode(t, err)
			for _, want := range tc.wantAny {
				if code == want {
					return
				}
			}
			t.Fatalf("refused with %s, want one of %v (%v)", code, tc.wantAny, err)
		})
	}
}

// TestCorruptCacheIsNotReused proves an entry is re-measured every time. A
// cache directory is a file on this host, and "we checked it when we wrote it"
// is not a check that holds now.
func TestCorruptCacheIsNotReused(t *testing.T) {
	pf := newPrivateFixture(t)
	rel := pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	if _, err := pf.install(t, "security-audit", "1.0.0"); err != nil {
		t.Fatalf("install: %v", err)
	}

	cache := skillregistry.NewCache(
		filepath.Join(pf.dataDir, "skills", "registry-cache"), skillregistry.CacheLimits{})
	entry, err := cache.GetArtifact(pf.reg.ID, rel.ArtifactDigest, rel.ManifestDigest)
	if err != nil {
		t.Fatalf("the install did not cache the verified bytes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entry.Dir(), "payload.sh"), []byte("#!/bin/sh\ncurl evil\n"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := cache.GetArtifact(pf.reg.ID, rel.ArtifactDigest, rel.ManifestDigest); err == nil {
		t.Fatal("a tampered cache entry was served")
	}
	// And with the registry down, the tampered entry cannot rescue an offline
	// install either: it is gone, so there is nothing verified to install.
	pf.srv.Stop()
	if _, err := pf.install(t, "security-audit", "1.0.0",
		func(r *skills.InstallReleaseRequest) { r.AllowOfflineFromCache = true }); err == nil {
		t.Fatal("a tampered cache entry served an offline install")
	}
}

// TestTenantCannotBorrowAnotherTenantsRegistryOrCredential holds the boundary
// this phase draws: around the REGISTRY, which is where the tenant check lives,
// and therefore around the secret only a registry can name.
func TestTenantCannotBorrowAnotherTenantsRegistryOrCredential(t *testing.T) {
	pf := newPrivateFixture(t)
	ctx := context.Background()
	// The default tenant already exists in a migrated store; using it means
	// the test exercises the visibility rule rather than tenant creation.
	tenantA := domain.DefaultTenantID

	pf.srv.RequireBearer(privateRegistryToken)
	pf.seedSecret(t, "TENANT_A_TOKEN", privateRegistryToken)

	private := pf.srv.Registry("tenant-a-private")
	private.TenantID = tenantA
	private.AuthType = skillregistry.AuthBearer
	private.CredentialSecretName = "TENANT_A_TOKEN"
	if _, err := pf.mp.SaveRegistry(ctx, skills.RegistryRequest{
		Registry: private, Actor: admin, ActorPermissions: adminPerms(),
		ActorTenants: []domain.TenantID{tenantA},
	}); err != nil {
		t.Fatalf("SaveRegistry for tenant A: %v", err)
	}

	// A caller outside tenant A does not see it, and "not found" rather than
	// "forbidden": a private registry's existence is itself information.
	if _, err := pf.mp.TestConnection(ctx, "tenant-a-private", admin, adminPerms(), nil); err == nil {
		t.Fatal("an outsider tested a tenant-scoped registry")
	} else if code := apiCode(t, err); code != "SKILL_REGISTRY_NOT_FOUND" {
		t.Fatalf("refused with %s, want SKILL_REGISTRY_NOT_FOUND", code)
	}

	// And the credential itself is not reachable by naming it from another
	// registry: the resolver re-reads the stored row.
	secrets := skills.NewRegistrySecrets(pf.store, secretbox.New(pf.dataDir))
	borrowed := pf.srv.Registry("corp")
	borrowed.AuthType = skillregistry.AuthBearer
	borrowed.CredentialSecretName = "TENANT_A_TOKEN"
	if _, err := secrets.ResolveRegistrySecret(ctx, borrowed); err == nil {
		t.Fatal("a registry resolved a credential attached to a different registry")
	}
	// Nor by claiming to be tenant A's registry from outside it.
	impostor := private
	impostor.TenantID = ""
	if _, err := secrets.ResolveRegistrySecret(ctx, impostor); err == nil {
		t.Fatal("a request that dropped the tenant resolved the credential")
	}
}

// TestConnectionTestChangesNothing is the promise the button makes.
func TestConnectionTestChangesNothing(t *testing.T) {
	pf := newPrivateFixture(t)
	pf.srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	ctx := context.Background()

	before, err := pf.store.ListSkillInstalls(ctx)
	if err != nil {
		t.Fatalf("ListSkillInstalls: %v", err)
	}
	if _, err := pf.mp.TestConnection(ctx, pf.reg.ID, admin, adminPerms(), nil); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	after, err := pf.store.ListSkillInstalls(ctx)
	if err != nil {
		t.Fatalf("ListSkillInstalls: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("a connection test changed the installed set: %d -> %d", len(before), len(after))
	}
	if pf.srv.FetchedArtifact() {
		t.Fatalf("a connection test downloaded something: %v", pf.srv.Requests())
	}
	activations, err := pf.store.ListSkillActivationsForSkillVersion(ctx, "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("ListSkillActivationsForSkillVersion: %v", err)
	}
	if len(activations) != 0 {
		t.Fatal("a connection test enabled something")
	}
	// It IS recorded, so the settings screen can say when it last answered.
	status, err := pf.mp.RegistryStatus(ctx, pf.reg.ID)
	if err != nil {
		t.Fatalf("RegistryStatus: %v", err)
	}
	if status.LastProbeState != skillregistry.ProbeConnected {
		t.Fatalf("the probe was not recorded: %+v", status)
	}
}

// TestConnectionTestRequiresSettingsManage holds the gate. A test makes AO open
// a connection and present a stored credential, which is not a read of AO's own
// state.
func TestConnectionTestRequiresSettingsManage(t *testing.T) {
	pf := newPrivateFixture(t)
	for _, held := range [][]domain.Permission{nil, {domain.PermSettingsRead}} {
		if _, err := pf.mp.TestConnection(context.Background(), pf.reg.ID, admin, held, nil); err == nil {
			t.Fatalf("a caller holding %v tested a connection", held)
		} else if code := apiCode(t, err); code != "SKILL_REGISTRY_REFUSED" {
			t.Fatalf("refused with %s", code)
		}
	}
	for _, held := range [][]domain.Permission{nil, {domain.PermSettingsRead}} {
		if _, err := pf.mp.SyncRevocations(context.Background(), pf.reg.ID, admin, held, nil); err == nil {
			t.Fatalf("a caller holding %v synced revocations", held)
		}
	}
}
