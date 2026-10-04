package skillregistry_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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
// the one AO already received, and it is marked OFFLINE rather than presented
// as current.
//
// The cached copy here is seconds old -- inside the TTL -- and that is exactly
// the case that used to read as "live" to everything above the provider,
// because a young cache and a fresh answer are indistinguishable once the
// signal is thrown away. Young is not confirmed.
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
	live := p.MetadataState()
	if live.Freshness != skillregistry.FreshnessLive || live.Offline {
		t.Fatalf("a live search reported %+v", live)
	}
	if !live.Current() {
		t.Fatal("a live search did not report itself current")
	}

	srv.Stop()
	rels, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("offline search: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("offline search returned %d releases", len(rels))
	}
	state := p.MetadataState()
	if state.Freshness != skillregistry.FreshnessOffline {
		t.Fatalf("an offline answer reported %s, want offline", state.Freshness)
	}
	if !state.Offline {
		t.Fatal("an offline answer did not set Offline")
	}
	if state.Current() {
		t.Fatal("an offline answer reported itself current")
	}
	if state.FetchedAt.IsZero() {
		t.Fatal("an offline answer carried no as-of time")
	}
	// The freshness signal must never move an artifact. Nothing was fetched.
	if srv.FetchedArtifact() {
		t.Fatal("the offline search fetched an artifact")
	}
}

// TestOfflineWithFreshCacheIsOfflineNotStale and its sibling below are the two
// halves of "explicitly not current": a cached copy inside the TTL says the
// registry could not be REACHED, and one past the TTL says that AND that the
// copy is old. Neither is ever live.
func TestOfflineWithFreshCacheIsOfflineNotStale(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	clock := &testClock{at: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	opts := srv.Options()
	opts.Cache = skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{MetadataTTL: time.Hour})
	opts.Now = clock.now
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("warming search: %v", err)
	}
	fetchedAt := p.MetadataState().FetchedAt

	srv.Stop()
	// Well inside the one-hour TTL.
	clock.advance(10 * time.Minute)
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("offline search: %v", err)
	}
	state := p.MetadataState()
	if state.Freshness != skillregistry.FreshnessOffline || !state.Offline {
		t.Fatalf("a fresh cached copy served offline reported %+v", state)
	}
	// The as-of time is when the REGISTRY answered, not when AO looked at its
	// own cache. A clock that moved is not a registry that spoke.
	if !state.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("as-of moved from %s to %s without the registry answering", fetchedAt, state.FetchedAt)
	}
}

// TestOfflinePastTTLIsStale is the other half.
func TestOfflinePastTTLIsStale(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	clock := &testClock{at: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	opts := srv.Options()
	opts.Cache = skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{MetadataTTL: time.Minute})
	opts.Now = clock.now
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("warming search: %v", err)
	}

	srv.Stop()
	clock.advance(48 * time.Hour)
	rels, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("offline search: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("a stale offline search returned %d releases", len(rels))
	}
	state := p.MetadataState()
	if state.Freshness != skillregistry.FreshnessStale {
		t.Fatalf("a copy 48h past a one-minute TTL reported %s, want stale", state.Freshness)
	}
	// Stale must not hide the outage: an operator reading "stale" still needs
	// to know AO could not ask.
	if !state.Offline {
		t.Fatal("a stale offline answer did not set Offline")
	}
	if state.Current() {
		t.Fatal("a stale answer reported itself current")
	}
}

// TestUnreachableWithNoCacheIsOffline: a registry that failed BECAUSE the
// network was down is a different row from one that answered "nothing
// matched", and the state has to say so even though the call returns an error.
func TestUnreachableWithNoCacheIsOffline(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	opts := srv.Options()
	opts.Cache = skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	srv.Stop()
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err == nil {
		t.Fatal("a search with no cache and no registry succeeded")
	}
	state := p.MetadataState()
	if state.Freshness != skillregistry.FreshnessOffline || !state.Offline {
		t.Fatalf("an unreachable registry with no cache reported %+v", state)
	}
	if !state.FetchedAt.IsZero() {
		t.Fatalf("a registry that never answered carried an as-of time: %s", state.FetchedAt)
	}
}

// TestFreshnessRecoversWhenTheRegistryReturns is the state-not-latch contract.
// A provider that reported offline during an outage must report live again
// once the registry answers, without being reconfigured.
func TestFreshnessRecoversWhenTheRegistryReturns(t *testing.T) {
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

	srv.Pause()
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("offline search: %v", err)
	}
	if got := p.MetadataState(); !got.Offline {
		t.Fatalf("the search during the outage reported %+v", got)
	}

	srv.Resume()
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("search after recovery: %v", err)
	}
	state := p.MetadataState()
	if state.Freshness != skillregistry.FreshnessLive || state.Offline {
		t.Fatalf("after the registry returned, the search reported %+v", state)
	}
	if !state.Current() {
		t.Fatal("a recovered search did not report itself current")
	}
}

// TestCachedFreshnessIsUnreachable holds the doc comment true. Every display
// read revalidates against the registry, so "served from cache without asking"
// is a state this build never produces -- and the value exists so a future
// cache-first read has an honest name rather than borrowing "live".
func TestCachedFreshnessIsUnreachable(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	clock := &testClock{at: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	opts := srv.Options()
	opts.Cache = skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{MetadataTTL: time.Hour})
	opts.Now = clock.now
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
		clock.advance(time.Second)
		if got := p.MetadataState().Freshness; got != skillregistry.FreshnessLive {
			t.Fatalf("a revalidated search reported %s; the registry answered every time", got)
		}
	}
	// And the reason it is live every time: the request went out every time.
	hits := 0
	for _, path := range srv.Requests() {
		if path == "/v1/skills" {
			hits++
		}
	}
	if hits != 3 {
		t.Fatalf("expected three revalidating searches, saw %d: %v", hits, srv.Requests())
	}
}

// TestFreshnessNeverTouchesTrust is the boundary the phase brief names: a
// cached row is not less trusted, and an offline read may not downgrade or
// upgrade what AO says it verified.
func TestFreshnessNeverTouchesTrust(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	opts := srv.Options()
	opts.Cache = skillregistry.NewCache(t.TempDir(), skillregistry.CacheLimits{})
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	live, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("warming search: %v", err)
	}
	srv.Stop()
	offline, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("offline search: %v", err)
	}
	if len(live) != 1 || len(offline) != 1 {
		t.Fatalf("searches returned %d and %d releases", len(live), len(offline))
	}
	if got := skillregistry.AssessAvailable(offline[0]); got != skillregistry.AssessAvailable(live[0]) {
		t.Fatalf("an offline read changed the trust state to %s", got)
	}
	if got := skillregistry.AssessAvailable(offline[0]); got == skillregistry.TrustTrusted {
		t.Fatal("an offline read reported trusted")
	}
	// And the other direction the brief names: an unknown compatibility
	// verdict stays unknown when the answer came from cache. "AO could not
	// ask" is not evidence that a release fits.
	if got := skillregistry.CheckCompatibility(offline[0], "not-a-version"); got != skillregistry.CompatibilityUnknown {
		t.Fatalf("an offline read turned an unknown compatibility into %s", got)
	}
	if live, off := skillregistry.CheckCompatibility(live[0], "1.0.0"),
		skillregistry.CheckCompatibility(offline[0], "1.0.0"); live != off {
		t.Fatalf("an offline read changed compatibility from %s to %s", live, off)
	}
}

// testClock is a clock a test moves by hand, so staleness is a decision rather
// than a sleep.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
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
