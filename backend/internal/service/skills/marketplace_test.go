package skills_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// marketplace_test.go -- the supply-chain tests.
//
// Every fixture here is a real registry on a real filesystem holding real
// package trees whose digests are computed from the bytes actually written. No
// digest is hard-coded and no verification is stubbed: a test that faked a hash
// would pass whether or not the verification path works, which is the one thing
// these tests exist to prove.
//
// Nothing here reaches the network, starts a provider, touches a real project,
// or executes a skill.

const (
	aoTestVersion = "0.12.0"
	// The publisher the shipped security-audit package declares. The fixtures
	// restamp that package rather than inventing one, so a test exercises the
	// manifest AO actually loads.
	shippedPublisher = "agent-orchestrator"
)

// registryFixture is a service, a marketplace and a local registry on disk.
type registryFixture struct {
	fixture
	mp   *skills.Marketplace
	root string
}

func newRegistryFixture(t *testing.T) registryFixture {
	t.Helper()
	f := newFixture(t)
	mp := skills.NewMarketplace(f.store, f.svc, skillregistry.DefaultProviderFactory{},
		aoTestVersion, f.dataDir)
	return registryFixture{fixture: f, root: t.TempDir(), mp: mp}
}

// publish writes one release into the fixture registry and returns it. The
// digests come from the bytes it just wrote.
type published struct {
	SkillID   string
	Version   string
	Publisher string
	// extraFile adds one file to the package tree BEFORE its digests are
	// computed. It is how a test publishes a genuinely different BUILD under
	// an identity that already exists -- the index stays internally
	// consistent, which is exactly what makes it the interesting case.
	extraFile string
	// mutate edits the index entry after the digests were recorded, which is
	// how a test publishes a WRONG digest without faking the package.
	mutate func(map[string]any)
}

func (rf registryFixture) publish(t *testing.T, specs ...published) {
	t.Helper()
	entries := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		version := spec.Version
		publisher := spec.Publisher
		if publisher == "" {
			publisher = shippedPublisher
		}
		artifactPath := "packages/" + spec.SkillID + "/" + version
		pkgDir := filepath.Join(rf.root, filepath.FromSlash(artifactPath))
		// The shipped security-audit package, restamped with this release's
		// id, version and publisher. Using the real one means these tests
		// exercise the manifest AO actually loads.
		stampPackage(t, pkgDir, spec.SkillID, version, publisher, spec.extraFile)

		artifactDigest, err := skillcatalog.ComputePackageDigest(pkgDir)
		if err != nil {
			t.Fatalf("ComputePackageDigest: %v", err)
		}
		manifestDigest, err := skillregistry.FileDigest(
			filepath.Join(pkgDir, skillcatalog.ManifestFileName))
		if err != nil {
			t.Fatalf("FileDigest: %v", err)
		}
		pkg, err := skillcatalog.LoadPackage(pkgDir)
		if err != nil {
			t.Fatalf("stamped package does not load: %v", err)
		}
		caps := []string{}
		for _, c := range pkg.Manifest.Capabilities {
			caps = append(caps, string(c))
		}
		entry := map[string]any{
			"skillId":               spec.SkillID,
			"name":                  "Security Audit",
			"version":               version,
			"publisher":             publisher,
			"description":           "On-demand security review of one selected project.",
			"riskLevel":             "critical",
			"manifestDigest":        manifestDigest,
			"artifactDigest":        artifactDigest,
			"requestedCapabilities": caps,
			"executionModes": []map[string]any{{
				"id": "static-code", "name": "Static code review",
				"description": "Read-only static review.", "riskLevel": "medium",
				"capabilities": []string{"repo.read", "report.write"},
			}},
			"compatibility": map[string]any{"aoMinVersion": "0.11.0"},
			"publishedAt":   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			"artifactPath":  artifactPath,
		}
		if spec.mutate != nil {
			spec.mutate(entry)
		}
		entries = append(entries, entry)
	}
	index := map[string]any{"apiVersion": skillregistry.IndexAPIVersion, "releases": entries}
	b, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rf.root, skillregistry.IndexFileName),
		append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// stampPackage copies the shipped security-audit package and rewrites its id,
// version and publisher, recomputing the integrity digest the manifest carries.
func stampPackage(t *testing.T, dir, id, version, publisher, extraFile string) {
	t.Helper()
	if err := skillcatalog.CopyPackage(securitySrc, dir); err != nil {
		t.Fatalf("stage package: %v", err)
	}
	if extraFile != "" {
		if err := os.WriteFile(filepath.Join(dir, "EXTRA.md"), []byte(extraFile), 0o600); err != nil {
			t.Fatalf("write extra file: %v", err)
		}
	}
	manifestPath := filepath.Join(dir, skillcatalog.ManifestFileName)
	b, err := os.ReadFile(manifestPath) //nolint:gosec // test fixture path.
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	body := string(b)
	body = strings.Replace(body, "id: security-audit", "id: "+id, 1)
	body = strings.Replace(body, "version: 0.1.0", "version: "+version, 1)
	body = strings.Replace(body, "publisher: "+shippedPublisher, "publisher: "+publisher, 1)
	if err := os.WriteFile(manifestPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	digest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	body = replaceDigestLine(body, digest)
	if err := os.WriteFile(manifestPath, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
}

func replaceDigestLine(body, digest string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "digest:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			lines[i] = indent + "digest: " + digest
		}
	}
	return strings.Join(lines, "\n")
}

func (rf registryFixture) addRegistry(t *testing.T, reg skillregistry.Registry) skillregistry.Registry {
	t.Helper()
	if reg.Location == "" {
		reg.Location = rf.root
	}
	saved, err := rf.mp.SaveRegistry(context.Background(), skills.RegistryRequest{
		Registry: reg, Actor: admin, ActorPermissions: adminPerms(),
		ActorTenants: []domain.TenantID{domain.DefaultTenantID},
	})
	if err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
	return saved
}

func defaultRegistry(root string) skillregistry.Registry {
	return skillregistry.Registry{
		ID: "ao-fixture", DisplayName: "AO Fixture", Type: skillregistry.RegistryLocal,
		Location: root, Enabled: true, TrustPolicy: skillregistry.TrustPolicyDigest, Priority: 10,
	}
}

func (rf registryFixture) install(t *testing.T, req skills.InstallReleaseRequest) (skills.InstallOutcome, error) {
	t.Helper()
	if req.RegistryID == "" {
		req.RegistryID = "ao-fixture"
	}
	if req.Actor == "" {
		req.Actor = admin
	}
	if req.ActorPermissions == nil {
		req.ActorPermissions = adminPerms()
	}
	if req.ActorTenants == nil {
		req.ActorTenants = []domain.TenantID{domain.DefaultTenantID}
	}
	return rf.mp.InstallRelease(context.Background(), req)
}

func (rf registryFixture) auditActions(t *testing.T, skillID string) []string {
	t.Helper()
	entries, err := rf.svc.AuditForSkill(context.Background(), skillID)
	if err != nil {
		t.Fatalf("AuditForSkill: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, string(e.Action))
	}
	return out
}

func hasAction(actions []string, want store.SkillAuditAction) bool {
	for _, a := range actions {
		if a == string(want) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ the happy path

func TestSearchThenInstall_EnablesNothingAnywhere(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	ctx := context.Background()

	found, err := rf.mp.Search(ctx, skills.SearchRequest{
		Query:        skillregistry.Query{Text: "security"},
		ActorTenants: []domain.TenantID{domain.DefaultTenantID},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 1 {
		t.Fatalf("Search returned %#v", found.Releases)
	}
	hit := found.Releases[0]
	if hit.Installed {
		t.Fatal("a search result reported itself installed before anything was installed")
	}
	// A listing hashed nothing, so it must not claim it did.
	if hit.Trust != skillregistry.TrustUnverified {
		t.Fatalf("a search hit reported trust %q; nothing was fetched", hit.Trust)
	}

	out, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err != nil {
		t.Fatalf("InstallRelease: %v", err)
	}
	if out.Install.Manifest.ID != "security-audit" || out.Install.Manifest.Version != "0.1.0" {
		t.Fatalf("installed %#v", out.Install.Manifest)
	}
	// Verified, because AO hashed the bytes. NEVER trusted: AO verified no
	// signature, and integrity by hash is not provenance.
	if out.Origin.TrustState != skillregistry.TrustVerified {
		t.Fatalf("trust state = %q, want verified", out.Origin.TrustState)
	}
	if out.Origin.RegistryID != "ao-fixture" || out.Origin.Publisher != shippedPublisher {
		t.Fatalf("origin = %#v", out.Origin)
	}

	// The whole point of the split: installing reaches no project.
	project := rf.seedProject(t, "proj-1")
	acts, err := rf.svc.ListForProject(ctx, project)
	if err != nil {
		t.Fatalf("ListForProject: %v", err)
	}
	if len(acts) != 0 {
		t.Fatalf("installing created activations: %#v", acts)
	}
	if _, err := rf.svc.Resolve(ctx, project, "security-audit"); err == nil {
		t.Fatal("a freshly installed skill resolved on a project that never enabled it")
	}
}

// The quarantine must not survive a successful install: it holds a second copy
// of every package ever fetched otherwise.
func TestInstall_LeavesNoQuarantineBehind(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}); err != nil {
		t.Fatalf("InstallRelease: %v", err)
	}
	quarantine := filepath.Join(rf.dataDir, "skills", "quarantine")
	entries, err := os.ReadDir(quarantine)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read quarantine: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d quarantine directories survived the install", len(entries))
	}
}

func TestInstall_IsIdempotentForTheSameBytes(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	req := skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}

	first, err := rf.install(t, req)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	second, err := rf.install(t, req)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if first.Install.Digest != second.Install.Digest {
		t.Fatalf("digest changed between identical installs: %s vs %s",
			first.Install.Digest, second.Install.Digest)
	}
	recs, err := rf.svc.ListInstalledRecords(context.Background())
	if err != nil {
		t.Fatalf("ListInstalledRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("a repeated install produced %d rows", len(recs))
	}
	origins, err := rf.store.ListSkillInstallOrigins(context.Background())
	if err != nil {
		t.Fatalf("ListSkillInstallOrigins: %v", err)
	}
	if len(origins) != 1 {
		t.Fatalf("a repeated install produced %d provenance rows", len(origins))
	}
}

// ------------------------------------------------------------- supply chain

// The same skillId@version served with different bytes. The registry's own
// digest is honest here, so the ONLY thing that catches it is AO having already
// installed different bytes under that identity.
func TestInstall_RefusesTheSameVersionWithDifferentBytes(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	req := skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}
	if _, err := rf.install(t, req); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Republish 0.1.0 as a genuinely different BUILD. The index is internally
	// consistent -- honest digests over the new bytes -- so nothing on the
	// registry side is wrong. What catches it is AO having already installed
	// different bytes under that identity, and an activation pinning a version
	// that would otherwise start meaning something else.
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0", extraFile: "backdoor\n"})

	_, err := rf.install(t, req)
	if err == nil {
		t.Fatal("a version whose bytes changed was reinstalled over the approved one")
	}
	if code := apiCode(t, err); code != "SKILL_VERSION_CONTENT_CHANGED" {
		t.Fatalf("refusal code = %q", code)
	}
}

// A registry that declares a digest the bytes do not hash to.
func TestInstall_RefusesAnArtifactDigestMismatch(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0",
		mutate: func(e map[string]any) { e["artifactDigest"] = strings.Repeat("c", 64) }})
	rf.addRegistry(t, defaultRegistry(rf.root))

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("an artifact whose digest did not match was installed")
	}
	if code := apiCode(t, err); code != "SKILL_ARTIFACT_DIGEST_MISMATCH" {
		t.Fatalf("refusal code = %q", code)
	}
	if recs, _ := rf.svc.ListInstalledRecords(context.Background()); len(recs) != 0 {
		t.Fatalf("a refused install still landed %d packages", len(recs))
	}
	if !hasAction(rf.auditActions(t, "security-audit"), store.SkillAuditInstallRefused) {
		t.Fatal("a refused install left no install_refused entry")
	}
}

// The manifest digest is separate because the package digest deliberately
// excludes the manifest that carries it. Checking only one leaves the other
// uncovered.
func TestInstall_RefusesAManifestDigestMismatch(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0",
		mutate: func(e map[string]any) { e["manifestDigest"] = strings.Repeat("d", 64) }})
	rf.addRegistry(t, defaultRegistry(rf.root))

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("a manifest whose digest did not match was installed")
	}
	if code := apiCode(t, err); code != "SKILL_MANIFEST_DIGEST_MISMATCH" {
		t.Fatalf("refusal code = %q", code)
	}
}

// The listing says one publisher and the package declares another. The listing
// is what a person read before approving.
func TestInstall_RefusesAPublisherThatDisagreesWithThePackage(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0",
		mutate: func(e map[string]any) { e["publisher"] = "somebody-else" }})
	rf.addRegistry(t, defaultRegistry(rf.root))

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("a release whose publisher disagreed with its package was installed")
	}
	if code := apiCode(t, err); code != "SKILL_PUBLISHER_MISMATCH" {
		t.Fatalf("refusal code = %q", code)
	}
}

// A registry pinned to one publisher must refuse a release from another, even
// when the package agrees with the listing.
func TestInstall_RefusesAPublisherThePinnedRegistryDoesNotAllow(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0", Publisher: "impostor"})
	reg := defaultRegistry(rf.root)
	reg.TrustPolicy = skillregistry.TrustPolicyPinnedPublisher
	reg.PinnedPublisher = shippedPublisher
	rf.addRegistry(t, reg)

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("a pinned registry served a release from another publisher")
	}
	if code := apiCode(t, err); code != "SKILL_PUBLISHER_MISMATCH" {
		t.Fatalf("refusal code = %q", code)
	}
}

// The listing understates what the package will ask for. This is the attack the
// capability comparison exists for.
func TestInstall_RefusesAListingThatUnderstatesCapabilities(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0",
		mutate: func(e map[string]any) {
			e["requestedCapabilities"] = []string{"repo.read"}
			e["executionModes"] = []map[string]any{{
				"id": "static-code", "name": "Static code review",
				"description": "Read-only.", "riskLevel": "low",
				"capabilities": []string{"repo.read"},
			}}
		}})
	rf.addRegistry(t, defaultRegistry(rf.root))

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("a package asking for more than its listing was installed")
	}
	if code := apiCode(t, err); code != "SKILL_CAPABILITY_MISMATCH" {
		t.Fatalf("refusal code = %q", code)
	}
	if !strings.Contains(err.Error(), "which the release listing does not mention") {
		t.Fatalf("the refusal did not name the extra capabilities: %v", err)
	}
}

func TestInstall_RefusesARevokedRelease(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0",
		mutate: func(e map[string]any) {
			e["revoked"] = true
			e["revocationReason"] = "signing key compromised"
		}})
	rf.addRegistry(t, defaultRegistry(rf.root))

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("a revoked release was installed")
	}
	if code := apiCode(t, err); code != "SKILL_RELEASE_REVOKED" {
		t.Fatalf("refusal code = %q", code)
	}
	if !strings.Contains(err.Error(), "signing key compromised") {
		t.Fatalf("the refusal did not carry the reason: %v", err)
	}
}

func TestInstall_RefusesAnIncompatibleRelease(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t,
		published{SkillID: "needs-future-ao", Version: "0.1.0",
			mutate: func(e map[string]any) {
				e["compatibility"] = map[string]any{"aoMinVersion": "9.0.0"}
			}},
		published{SkillID: "needs-old-ao", Version: "0.1.0",
			mutate: func(e map[string]any) {
				e["compatibility"] = map[string]any{"aoMinVersion": "0.1.0", "aoMaxVersion": "0.2.0"}
			}},
	)
	rf.addRegistry(t, defaultRegistry(rf.root))

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "needs-future-ao", Version: "0.1.0"})
	if code := apiCode(t, err); code != "SKILL_AO_TOO_OLD" {
		t.Fatalf("too-new release refusal code = %q (err %v)", code, err)
	}
	_, err = rf.install(t, skills.InstallReleaseRequest{SkillID: "needs-old-ao", Version: "0.1.0"})
	if code := apiCode(t, err); code != "SKILL_AO_TOO_NEW" {
		t.Fatalf("too-old release refusal code = %q (err %v)", code, err)
	}
}

// A registry requiring a signature must install NOTHING on an installation
// that holds no trust root, and say why.
//
// Phase 11 refused here because AO verified no signature at all. Phase 12
// verifies them, so the reason changes and the outcome does not: this fixture
// wires no trust authority, so there is nothing for a signature to chain to,
// and the strictest-looking setting must not quietly behave as the weakest.
// The satisfiable path is covered in trustedinstall_test.go.
func TestInstall_RefusesUnderSignedPolicyWithNoTrustStore(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	reg := defaultRegistry(rf.root)
	reg.TrustPolicy = skillregistry.TrustPolicySigned
	rf.addRegistry(t, reg)

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("a signature-requiring registry installed something AO cannot verify")
	}
	if code := apiCode(t, err); code != "SKILL_TRUST_POLICY_UNSATISFIABLE" {
		t.Fatalf("refusal code = %q", code)
	}
	if !strings.Contains(err.Error(), "no trust store") {
		t.Fatalf("the refusal did not say why: %v", err)
	}
}

// The official policy is configurable and unsatisfiable on a build that ships
// no AO Official root -- which this one does not, deliberately.
func TestInstall_RefusesUnderOfficialPolicyWithNoOfficialRoot(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	reg := defaultRegistry(rf.root)
	reg.TrustPolicy = skillregistry.TrustPolicyOfficial
	rf.addRegistry(t, reg)

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("an official-policy registry installed something with no official root configured")
	}
	if code := apiCode(t, err); code != "SKILL_TRUST_POLICY_UNSATISFIABLE" {
		t.Fatalf("refusal code = %q", code)
	}
}

// Two registries taking turns publishing one skill id.
func TestInstall_RefusesASkillIDBoundToAnotherRegistry(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// A second registry offering the same id at a different version.
	other := newRegistryFixture(t)
	other.publish(t, published{SkillID: "security-audit", Version: "0.2.0"})
	rf.addRegistry(t, skillregistry.Registry{
		ID: "impostor", DisplayName: "Impostor", Type: skillregistry.RegistryLocal,
		Location: other.root, Enabled: true, TrustPolicy: skillregistry.TrustPolicyDigest, Priority: 20,
	})

	_, err := rf.install(t, skills.InstallReleaseRequest{
		RegistryID: "impostor", SkillID: "security-audit", Version: "0.2.0",
	})
	if err == nil {
		t.Fatal("a skill id was installed from a second registry")
	}
	if code := apiCode(t, err); code != "SKILL_REGISTRY_COLLISION" {
		t.Fatalf("refusal code = %q", code)
	}
	if !strings.Contains(err.Error(), "ao-fixture") {
		t.Fatalf("the refusal did not name the registry the id is bound to: %v", err)
	}
}

// A registry naming a path outside its own root, and a package carrying a
// symlink, both refused before anything reaches the catalog.
func TestInstall_RefusesASymlinkInThePackage(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	pkgDir := filepath.Join(rf.root, "packages", "security-audit", "0.1.0")
	if err := os.Symlink("/etc/passwd", filepath.Join(pkgDir, "secrets.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rf.addRegistry(t, defaultRegistry(rf.root))

	_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"})
	if err == nil {
		t.Fatal("a package containing a symlink was installed")
	}
	if code := apiCode(t, err); code != "SKILL_ARTIFACT_UNFETCHABLE" {
		t.Fatalf("refusal code = %q (err %v)", code, err)
	}
	if recs, _ := rf.svc.ListInstalledRecords(context.Background()); len(recs) != 0 {
		t.Fatal("a symlinked package still landed in the catalog")
	}
}

func TestInstall_RefusesAReleaseThatIsNotOffered(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))

	// There is no "latest": a version is required and exact.
	for _, version := range []string{"", "latest", "^0.1.0", "0.9.9"} {
		_, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: version})
		if err == nil {
			t.Fatalf("version %q was accepted as an install target", version)
		}
		if code := apiCode(t, err); code != "SKILL_RELEASE_NOT_FOUND" {
			t.Fatalf("version %q refusal code = %q", version, code)
		}
	}
}

// -------------------------------------------------------------------- RBAC

func TestInstall_RequiresSettingsManage(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))

	_, err := rf.install(t, skills.InstallReleaseRequest{
		SkillID: "security-audit", Version: "0.1.0", ActorPermissions: readerPerms(),
	})
	if err == nil {
		t.Fatal("a caller without settings.manage installed a package")
	}
	if code := apiCode(t, err); code != "SKILL_REGISTRY_REFUSED" {
		t.Fatalf("refusal code = %q", code)
	}
}

func TestSaveRegistry_RequiresSettingsManage(t *testing.T) {
	rf := newRegistryFixture(t)
	_, err := rf.mp.SaveRegistry(context.Background(), skills.RegistryRequest{
		Registry: defaultRegistry(rf.root), Actor: "someone", ActorPermissions: readerPerms(),
	})
	if err == nil {
		t.Fatal("a caller without settings.manage configured a registry")
	}
	if code := apiCode(t, err); code != "SKILL_REGISTRY_REFUSED" {
		t.Fatalf("refusal code = %q", code)
	}
}

// A tenant-scoped registry is invisible to a caller outside its tenant, and
// invisible means NOT FOUND: 403 on a private registry tells a stranger which
// organizations have one.
func TestTenantScopedRegistryIsInvisibleAndUninstallableAcrossTenants(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	reg := defaultRegistry(rf.root)
	reg.ID = "acme-private"
	reg.TenantID = domain.DefaultTenantID
	rf.addRegistry(t, reg)
	ctx := context.Background()

	outsider := []domain.TenantID{"tnt_other"}
	regs, err := rf.mp.ListRegistries(ctx, outsider)
	if err != nil {
		t.Fatalf("ListRegistries: %v", err)
	}
	if len(regs) != 0 {
		t.Fatalf("a tenant-scoped registry was listed to an outsider: %#v", regs)
	}
	found, err := rf.mp.Search(ctx, skills.SearchRequest{ActorTenants: outsider})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 0 {
		t.Fatalf("an outsider searched a private registry: %#v", found.Releases)
	}
	_, err = rf.install(t, skills.InstallReleaseRequest{
		RegistryID: "acme-private", SkillID: "security-audit", Version: "0.1.0",
		ActorTenants: outsider,
	})
	if err == nil {
		t.Fatal("an outsider installed from a private registry")
	}
	if code := apiCode(t, err); code != "SKILL_REGISTRY_NOT_FOUND" {
		t.Fatalf("cross-tenant refusal code = %q; existence must not leak as 403", code)
	}
	// The member still sees it.
	member := []domain.TenantID{domain.DefaultTenantID}
	if regs, err := rf.mp.ListRegistries(ctx, member); err != nil || len(regs) != 1 {
		t.Fatalf("a member saw %d registries (err %v)", len(regs), err)
	}
}

func TestSaveRegistry_RefusesATenantTheActorIsNotIn(t *testing.T) {
	rf := newRegistryFixture(t)
	reg := defaultRegistry(rf.root)
	reg.TenantID = "tnt_elsewhere"
	_, err := rf.mp.SaveRegistry(context.Background(), skills.RegistryRequest{
		Registry: reg, Actor: admin, ActorPermissions: adminPerms(),
		ActorTenants: []domain.TenantID{domain.DefaultTenantID},
	})
	if err == nil {
		t.Fatal("settings.manage was used to plant a registry inside another organization")
	}
	if code := apiCode(t, err); code != "SKILL_REGISTRY_TENANT_REFUSED" {
		t.Fatalf("refusal code = %q", code)
	}
}

// ------------------------------------------------------------------ registries

// A configuration AO cannot read must be refused at save time: a registry that
// answers every search with silence reads as "this registry has nothing".
func TestSaveRegistry_RefusesAnUnreadableLocation(t *testing.T) {
	rf := newRegistryFixture(t)
	reg := defaultRegistry(filepath.Join(t.TempDir(), "not-a-registry"))
	_, err := rf.mp.SaveRegistry(context.Background(), skills.RegistryRequest{
		Registry: reg, Actor: admin, ActorPermissions: adminPerms(),
	})
	if err == nil {
		t.Fatal("a registry with no index was accepted")
	}
	if code := apiCode(t, err); code != "SKILL_REGISTRY_UNREADABLE" {
		t.Fatalf("refusal code = %q", code)
	}
}

// A disabled registry serves nothing and hides nothing: its installs and their
// provenance stay exactly where they are.
func TestDisabledRegistryServesNothingAndKeepsProvenance(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	reg := defaultRegistry(rf.root)
	reg.Enabled = false
	rf.addRegistry(t, reg)
	ctx := context.Background()

	found, err := rf.mp.Search(ctx, skills.SearchRequest{
		ActorTenants: []domain.TenantID{domain.DefaultTenantID},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 0 {
		t.Fatalf("a disabled registry answered a search: %#v", found.Releases)
	}
	origin, ok, err := rf.mp.InstallOrigin(ctx, "security-audit", "0.1.0")
	if err != nil || !ok {
		t.Fatalf("provenance disappeared with the registry: ok=%v err=%v", ok, err)
	}
	if origin.RegistryID != "ao-fixture" || origin.Publisher != shippedPublisher {
		t.Fatalf("provenance = %#v", origin)
	}
}

// Removing a registry keeps the packages and their provenance. Removing a
// compromised registry must not erase the evidence of what it served.
func TestRemoveRegistry_KeepsInstallsAndProvenance(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	ctx := context.Background()
	if err := rf.mp.RemoveRegistry(ctx, "ao-fixture", admin, adminPerms(),
		[]domain.TenantID{domain.DefaultTenantID}); err != nil {
		t.Fatalf("RemoveRegistry: %v", err)
	}
	recs, err := rf.svc.ListInstalledRecords(ctx)
	if err != nil || len(recs) != 1 {
		t.Fatalf("removing a registry uninstalled its packages: %d rows (err %v)", len(recs), err)
	}
	origin, ok, err := rf.mp.InstallOrigin(ctx, "security-audit", "0.1.0")
	if err != nil || !ok {
		t.Fatalf("provenance was erased with the registry: ok=%v err=%v", ok, err)
	}
	// Copied at install time, so it still names the registry that is gone.
	if origin.RegistryID != "ao-fixture" || origin.RegistryName != "AO Fixture" {
		t.Fatalf("provenance = %#v", origin)
	}
	if !hasAction(rf.auditActions(t, ""), store.SkillAuditRegistryRemoved) {
		t.Fatal("removing a registry left no audit entry")
	}
}

// An unreadable registry must be NAMED, not silently absent from a search.
func TestSearch_NamesARegistryItCouldNotRead(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	// Break it after it was accepted, which is what a registry directory being
	// unmounted or corrupted looks like.
	if err := os.Remove(filepath.Join(rf.root, skillregistry.IndexFileName)); err != nil {
		t.Fatalf("remove index: %v", err)
	}
	found, err := rf.mp.Search(context.Background(), skills.SearchRequest{
		ActorTenants: []domain.TenantID{domain.DefaultTenantID},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Releases) != 0 {
		t.Fatalf("an unreadable registry returned releases: %#v", found.Releases)
	}
	if len(found.Notes) != 1 || found.Notes[0].RegistryID != "ao-fixture" {
		t.Fatalf("an unreadable registry vanished into an empty result: %#v", found.Notes)
	}
}

// --------------------------------------------------------------------- updates

func TestCheckUpdates_ReportsANewerReleaseAndInstallsIt(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	ctx := context.Background()
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	rf.publish(t,
		published{SkillID: "security-audit", Version: "0.1.0"},
		published{SkillID: "security-audit", Version: "0.2.0"},
	)

	statuses, err := rf.mp.CheckUpdates(ctx, admin, []domain.TenantID{domain.DefaultTenantID})
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(statuses) != 1 || !statuses[0].UpdateAvailable || statuses[0].LatestVersion != "0.2.0" {
		t.Fatalf("CheckUpdates = %#v", statuses)
	}
	if !hasAction(rf.auditActions(t, "security-audit"), store.SkillAuditUpdateAvailable) {
		t.Fatal("an available update left no audit entry")
	}

	out, err := rf.install(t, skills.InstallReleaseRequest{
		SkillID: "security-audit", Version: "0.2.0", AsUpdate: true,
	})
	if err != nil {
		t.Fatalf("update install: %v", err)
	}
	if !out.Updated {
		t.Fatal("an update did not report itself as one")
	}
	if !hasAction(rf.auditActions(t, "security-audit"), store.SkillAuditUpdateInstalled) {
		t.Fatal("an installed update left no update_installed entry")
	}
	// Versions live side by side: the older one is still there for whatever
	// project pinned it.
	recs, err := rf.svc.ListInstalledRecords(ctx)
	if err != nil || len(recs) != 2 {
		t.Fatalf("after an update the catalog holds %d versions (err %v)", len(recs), err)
	}
}

// Clicking "Update" must never hand somebody an older release.
func TestUpdate_RefusesADowngrade(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t,
		published{SkillID: "security-audit", Version: "0.1.0"},
		published{SkillID: "security-audit", Version: "0.2.0"},
	)
	rf.addRegistry(t, defaultRegistry(rf.root))
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.2.0"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	_, err := rf.install(t, skills.InstallReleaseRequest{
		SkillID: "security-audit", Version: "0.1.0", AsUpdate: true,
	})
	if err == nil {
		t.Fatal("an older release was accepted as an update")
	}
	if code := apiCode(t, err); code != "SKILL_UPDATE_NOT_NEWER" {
		t.Fatalf("refusal code = %q", code)
	}
	// Naming the older version deliberately is a rollback, and it is allowed:
	// versions live side by side and an activation pins one, so an install can
	// never change what a project already resolves to.
	if _, err := rf.install(t, skills.InstallReleaseRequest{
		SkillID: "security-audit", Version: "0.1.0",
	}); err != nil {
		t.Fatalf("a deliberate rollback install was refused: %v", err)
	}
}

// The revocation contract, in full: mark, block, keep, and say so.
func TestRevocationAfterInstall_MarksAndBlocksButNeverUninstalls(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	ctx := context.Background()
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The registry withdraws it after the fact.
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0",
		mutate: func(e map[string]any) {
			e["revoked"] = true
			e["revocationReason"] = "signing key compromised"
		}})

	statuses, err := rf.mp.CheckUpdates(ctx, admin, []domain.TenantID{domain.DefaultTenantID})
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(statuses) != 1 || !statuses[0].RevokedNow {
		t.Fatalf("a revoked installed release was not reported: %#v", statuses)
	}
	if statuses[0].RevocationReason != "signing key compromised" {
		t.Fatalf("revocation reason = %q", statuses[0].RevocationReason)
	}
	// Marked...
	origin, ok, err := rf.mp.InstallOrigin(ctx, "security-audit", "0.1.0")
	if err != nil || !ok || !origin.Revoked() {
		t.Fatalf("origin = %#v ok=%v err=%v", origin, ok, err)
	}
	if origin.TrustState != skillregistry.TrustRevoked {
		t.Fatalf("trust state after revocation = %q", origin.TrustState)
	}
	// ...and NOT uninstalled. Deleting somebody's installed package because a
	// remote registry changed its mind would be AO acting on an instruction
	// from outside.
	recs, err := rf.svc.ListInstalledRecords(ctx)
	if err != nil || len(recs) != 1 {
		t.Fatalf("a revoked release was uninstalled automatically: %d rows (err %v)", len(recs), err)
	}
	if !hasAction(rf.auditActions(t, "security-audit"), store.SkillAuditReleaseRevokedSeen) {
		t.Fatal("an observed revocation left no audit entry")
	}
	// A second check does not audit it again.
	before := len(rf.auditActions(t, "security-audit"))
	if _, err := rf.mp.CheckUpdates(ctx, admin, []domain.TenantID{domain.DefaultTenantID}); err != nil {
		t.Fatalf("second CheckUpdates: %v", err)
	}
	if after := len(rf.auditActions(t, "security-audit")); after != before {
		t.Fatalf("a repeated check added %d audit rows", after-before)
	}
	// And a NEW install of it is refused from here on.
	if _, err := rf.install(t, skills.InstallReleaseRequest{
		SkillID: "security-audit", Version: "0.1.0",
	}); err == nil {
		t.Fatal("a revoked release could still be installed")
	}
}

// Provenance survives the release disappearing from the registry entirely.
func TestProvenanceSurvivesTheReleaseVanishing(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	ctx := context.Background()
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The registry now offers something else entirely.
	rf.publish(t, published{SkillID: "other-skill", Version: "1.0.0"})

	statuses, err := rf.mp.CheckUpdates(ctx, admin, []domain.TenantID{domain.DefaultTenantID})
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Unreachable == "" {
		t.Fatalf("a vanished release was not reported as unreachable: %#v", statuses)
	}
	origin := statuses[0].Origin
	if origin.RegistryID != "ao-fixture" || origin.ArtifactDigest == "" || origin.Publisher != shippedPublisher {
		t.Fatalf("provenance was lost with the release: %#v", origin)
	}
	if origin.TrustState != skillregistry.TrustVerified {
		t.Fatalf("a vanished release's recorded trust changed to %q", origin.TrustState)
	}
}

// Persistence: everything above must survive a restart, because a marketplace
// whose registries evaporate on reboot is a marketplace nobody can rely on.
func TestRegistriesAndProvenanceSurviveARestart(t *testing.T) {
	rf := newRegistryFixture(t)
	rf.publish(t, published{SkillID: "security-audit", Version: "0.1.0"})
	rf.addRegistry(t, defaultRegistry(rf.root))
	ctx := context.Background()
	if _, err := rf.install(t, skills.InstallReleaseRequest{SkillID: "security-audit", Version: "0.1.0"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	// A genuine reopen of the same file on disk, the way the daemon does it.
	if err := rf.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := sqlite.Open(rf.dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	svc := skills.New(reopened, rf.dataDir)
	mp := skills.NewMarketplace(reopened, svc,
		skillregistry.DefaultProviderFactory{}, aoTestVersion, rf.dataDir)

	regs, err := mp.ListRegistries(ctx, []domain.TenantID{domain.DefaultTenantID})
	if err != nil || len(regs) != 1 || regs[0].ID != "ao-fixture" {
		t.Fatalf("registries after restart = %#v (err %v)", regs, err)
	}
	if regs[0].TrustPolicy != skillregistry.TrustPolicyDigest || !regs[0].Enabled {
		t.Fatalf("registry configuration changed across a restart: %#v", regs[0])
	}
	origin, ok, err := mp.InstallOrigin(ctx, "security-audit", "0.1.0")
	if err != nil || !ok {
		t.Fatalf("provenance after restart: ok=%v err=%v", ok, err)
	}
	if origin.TrustState != skillregistry.TrustVerified || origin.Publisher != shippedPublisher {
		t.Fatalf("provenance after restart = %#v", origin)
	}
	// And the marketplace still works against the same registry.
	found, err := mp.Search(ctx, skills.SearchRequest{
		ActorTenants: []domain.TenantID{domain.DefaultTenantID},
	})
	if err != nil || len(found.Releases) != 1 || !found.Releases[0].Installed {
		t.Fatalf("search after restart = %#v (err %v)", found, err)
	}
}
