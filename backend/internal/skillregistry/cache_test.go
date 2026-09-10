package skillregistry_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
)

// cache_test.go -- what the cache is allowed to do, and the two things it is
// not: hand back bytes it did not re-verify, and make a stale answer look
// current.

func TestMetadataCacheRevalidatesWithETag(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	opts := srv.Options()
	opts.Cache = skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	first, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("first search: %v", err)
	}
	second, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("searches returned %d and %d releases", len(first), len(second))
	}
	if first[0].Ref() != second[0].Ref() {
		t.Fatalf("the revalidated answer differs: %s vs %s", first[0].Ref(), second[0].Ref())
	}
	// The second request went out (revalidation is a request), and it was
	// answered 304 from the fixture's ETag -- both searches are in the log.
	hits := 0
	for _, path := range srv.Requests() {
		if path == "/v1/skills" {
			hits++
		}
	}
	if hits != 2 {
		t.Fatalf("expected two conditional searches, saw %d: %v", hits, srv.Requests())
	}
}

// TestOfflineFallsBackToCacheAndSaysSo is the offline contract: the answer is
// the one AO already received, and it is marked stale rather than presented as
// current.
func TestOfflineFallsBackToCacheAndSaysSo(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	opts := srv.Options()
	opts.Cache = skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("warming search: %v", err)
	}
	if got := p.Freshness(); got != skillregistry.FreshnessLive {
		t.Fatalf("a live search reported %s", got)
	}

	srv.Stop()
	rels, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("offline search: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("offline search returned %d releases", len(rels))
	}
	if got := p.Freshness(); got != skillregistry.FreshnessStale {
		t.Fatalf("an offline answer reported %s, want stale", got)
	}
}

// TestInstallResolutionNeverAnswersFromCache is the rule revocation depends on.
// A cached ResolveExactRelease would let an install act on a release that was
// withdrawn since somebody last looked.
func TestInstallResolutionNeverAnswersFromCache(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	opts := srv.Options()
	opts.Cache = skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if _, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.0.0"); err != nil {
		t.Fatalf("warming resolve: %v", err)
	}
	srv.Stop()
	if _, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.0.0"); err == nil {
		t.Fatal("an install resolved from cache while the registry was unreachable")
	}
}

// TestArtifactCacheReVerifiesBeforeReuse is the whole reason the artifact cache
// is content-addressed. An entry that was correct when it was written is not an
// entry that is correct now.
func TestArtifactCacheReVerifiesBeforeReuse(t *testing.T) {
	srv := registrytest.New(t, "corp")
	rel := srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	cache := skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})

	// Fetch once, verify, cache.
	opts := srv.Options()
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	staging := filepath.Join(t.TempDir(), "q")
	if err := p.FetchArtifact(context.Background(), rel, staging); err != nil {
		t.Fatalf("FetchArtifact: %v", err)
	}
	entry := skillregistry.ArtifactEntry{
		RegistryID: "corp", SkillID: rel.SkillID, Version: rel.Version,
		ArtifactDigest: rel.ArtifactDigest, ManifestDigest: rel.ManifestDigest,
	}
	if err := cache.PutArtifact(entry, staging); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	got, err := cache.GetArtifact("corp", rel.ArtifactDigest, rel.ManifestDigest)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if got.SkillID != rel.SkillID {
		t.Fatalf("cache returned %s", got.SkillID)
	}

	// Now tamper with the cached bytes, which is what an attacker with a
	// foothold on this host would do.
	if err := os.WriteFile(filepath.Join(got.Dir(), "extra-payload.sh"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := cache.GetArtifact("corp", rel.ArtifactDigest, rel.ManifestDigest); err == nil {
		t.Fatal("a tampered cache entry was reused")
	}
	// And the poisoned entry is gone rather than left to be found again.
	if _, err := os.Stat(got.Dir()); !os.IsNotExist(err) {
		t.Fatalf("the tampered entry survived: %v", err)
	}
}

// TestArtifactCacheRefusesAMislabelledEntry proves the cache will not store
// bytes under a digest they do not have -- so a caller that got the order wrong
// writes nothing rather than poisoning the cache for everybody.
func TestArtifactCacheRefusesAMislabelledEntry(t *testing.T) {
	cache := skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})
	dir := t.TempDir()
	registrytest.PackageDir(t, dir, map[string]string{"skill.yaml": "id: x\n"})
	wrong := registrytest.Sha256("not these bytes")
	err := cache.PutArtifact(skillregistry.ArtifactEntry{
		RegistryID: "corp", SkillID: "x", Version: "1.0.0",
		ArtifactDigest: wrong, ManifestDigest: wrong,
	}, dir)
	if err == nil {
		t.Fatal("the cache stored bytes under a digest they do not have")
	}
}

// TestArtifactCacheIsNamespacedByRegistry holds the tenant boundary: identical
// bytes cached for one registry are not served to another.
func TestArtifactCacheIsNamespacedByRegistry(t *testing.T) {
	cache := skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})
	dir := t.TempDir()
	registrytest.PackageDir(t, dir, map[string]string{
		skillcatalog.ManifestFileName: "id: x\n", "a.txt": "hello\n",
	})
	digest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	manifestDigest, err := skillregistry.FileDigest(filepath.Join(dir, skillcatalog.ManifestFileName))
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	if err := cache.PutArtifact(skillregistry.ArtifactEntry{
		RegistryID: "tenant-a-registry", SkillID: "x", Version: "1.0.0",
		ArtifactDigest: digest, ManifestDigest: manifestDigest,
	}, dir); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	if _, err := cache.GetArtifact("tenant-a-registry", digest, manifestDigest); err != nil {
		t.Fatalf("the owning registry cannot read its own entry: %v", err)
	}
	if _, err := cache.GetArtifact("tenant-b-registry", digest, manifestDigest); err == nil {
		t.Fatal("another registry read a cached artifact by digest alone")
	}
}

// TestGCEnforcesTheBudget proves the limits are enforced and that GC removes
// cache entries only.
func TestGCEnforcesTheBudget(t *testing.T) {
	root := t.TempDir()
	cache := skillregistry.NewCache(root, skillregistry.CacheLimits{MaxArtifactEntries: 1})
	for _, name := range []string{"one", "two"} {
		dir := t.TempDir()
		registrytest.PackageDir(t, dir, map[string]string{
			skillcatalog.ManifestFileName: "id: " + name + "\n", "body.txt": name,
		})
		digest, err := skillcatalog.ComputePackageDigest(dir)
		if err != nil {
			t.Fatalf("ComputePackageDigest: %v", err)
		}
		manifestDigest, err := skillregistry.FileDigest(filepath.Join(dir, skillcatalog.ManifestFileName))
		if err != nil {
			t.Fatalf("FileDigest: %v", err)
		}
		if err := cache.PutArtifact(skillregistry.ArtifactEntry{
			RegistryID: "corp", SkillID: name, Version: "1.0.0",
			ArtifactDigest: digest, ManifestDigest: manifestDigest,
		}, dir); err != nil {
			t.Fatalf("PutArtifact %s: %v", name, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	report, err := cache.GC()
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if report.ArtifactsKept != 1 || report.ArtifactRemoved != 1 {
		t.Fatalf("GC kept %d and removed %d, want 1 and 1", report.ArtifactsKept, report.ArtifactRemoved)
	}
}

// TestNilCacheIsAWorkingNoCache keeps the zero value honest: an installation
// with no cache behaves like one whose cache is always empty.
func TestNilCacheIsAWorkingNoCache(t *testing.T) {
	var cache *skillregistry.Cache
	if _, err := cache.GetMetadata("corp", "key"); err == nil {
		t.Fatal("a nil cache reported a hit")
	}
	cache.PutMetadata("corp", "key", skillregistry.MetadataEntry{Body: []byte("{}")})
	if _, err := cache.GetArtifact("corp", registrytest.Sha256("x"), registrytest.Sha256("y")); err == nil {
		t.Fatal("a nil cache reported an artifact")
	}
	if err := cache.PutArtifact(skillregistry.ArtifactEntry{}, t.TempDir()); err != nil {
		t.Fatalf("a nil cache errored on a write: %v", err)
	}
	if _, err := cache.GC(); err != nil {
		t.Fatalf("a nil cache errored on GC: %v", err)
	}
}

// TestTwoVersionsWithIdenticalCodeDoNotShareACacheEntry is the bug that keying
// the artifact cache on the package digest alone actually produced.
//
// skillcatalog.ComputePackageDigest deliberately EXCLUDES the manifest -- the
// manifest carries that digest and so cannot cover itself -- so two releases
// whose only difference is the version line hash to the same artifact digest.
// That is every ordinary version bump of a skill whose code did not change.
//
// Keyed on the artifact digest alone, installing 0.2.0 would have been served
// 0.1.0's tree and refused for a manifest-digest mismatch: a correct refusal of
// a correct package, which is the worst kind of failure because it looks like
// the security control working.
func TestTwoVersionsWithIdenticalCodeDoNotShareACacheEntry(t *testing.T) {
	srv := registrytest.New(t, "corp")
	oldRel := srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "0.1.0"})
	newRel := srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "0.2.0"})

	if oldRel.ArtifactDigest != newRel.ArtifactDigest {
		t.Fatalf("this fixture no longer reproduces the collision; the two artifact digests differ: "+
			"%s vs %s", oldRel.ArtifactDigest, newRel.ArtifactDigest)
	}
	if oldRel.ManifestDigest == newRel.ManifestDigest {
		t.Fatal("the two manifests hash the same, so the versions are indistinguishable")
	}

	cache := skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, srv.Options())
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	for _, rel := range []skillregistry.Release{oldRel, newRel} {
		dir := filepath.Join(t.TempDir(), "q")
		if err := p.FetchArtifact(context.Background(), rel, dir); err != nil {
			t.Fatalf("FetchArtifact %s: %v", rel.Ref(), err)
		}
		if err := cache.PutArtifact(skillregistry.ArtifactEntry{
			RegistryID: "corp", SkillID: rel.SkillID, Version: rel.Version,
			ArtifactDigest: rel.ArtifactDigest, ManifestDigest: rel.ManifestDigest, Release: rel,
		}, dir); err != nil {
			t.Fatalf("PutArtifact %s: %v", rel.Ref(), err)
		}
	}
	for _, rel := range []skillregistry.Release{oldRel, newRel} {
		got, err := cache.GetArtifact("corp", rel.ArtifactDigest, rel.ManifestDigest)
		if err != nil {
			t.Fatalf("GetArtifact %s: %v", rel.Ref(), err)
		}
		if got.Version != rel.Version {
			t.Fatalf("the cache served %s for a %s lookup", got.Version, rel.Version)
		}
	}
}
