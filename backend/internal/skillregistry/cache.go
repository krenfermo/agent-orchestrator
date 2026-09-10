package skillregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// cache.go -- two caches, because there are two questions and only one of them
// has a right answer that expires.
//
// # Metadata cache: a RECORD OF AN ANSWER, never a substitute for one
//
// What a registry says about a release changes: a version is deprecated, a
// release is withdrawn, a new one appears. So a cached metadata entry carries
// the moment it was fetched and is always rendered with it. Past its TTL it is
// STALE, and stale is a state the UI must show, not a state the UI may round
// down to "current". An install never acts on it: InstallRelease re-resolves
// from the registry, and when the registry cannot be reached the install path
// checks the persisted revocation record instead of assuming silence means
// consent.
//
// # Artifact cache: CONTENT-ADDRESSED, and therefore not a cache of a name
//
// An entry is keyed by the artifact digest AO itself computed over the unpacked
// tree -- never by a skill id, a version or a tag, all of which are mutable
// names a registry controls. That is what makes reuse safe: the key IS the
// verification, and the digest is recomputed from the bytes on disk before they
// are handed to an install anyway, because a cache directory is a file on this
// host and files on this host get edited.
//
// # Why the artifact cache is namespaced by registry
//
// Identical bytes under two registries are the same bytes, so content-
// addressing alone would let one registry's install be served from another
// registry's download. That is fine right up until one of them is tenant-scoped
// and the other is not. Namespacing costs disk and buys "a tenant-scoped
// registry's artifacts are never served to anything else", which is a promise
// worth more than the disk.
//
// # What is deliberately NOT here
//
// A secret. No credential appears in a cache key, a filename, or a stored
// body: the key is derived from the registry id and the request path, and the
// credential is a header that is never part of either.

// ErrCacheMiss means the cache holds nothing for that key.
var ErrCacheMiss = errors.New("skillregistry: cache miss")

// CacheLimits bound what a cache may keep.
type CacheLimits struct {
	// MetadataTTL is how long a metadata entry is FRESH. Past it the entry is
	// still returned -- that is the point of offline mode -- and it is
	// returned marked stale.
	MetadataTTL time.Duration
	// MaxArtifactBytes is the total the artifact cache may occupy.
	MaxArtifactBytes int64
	// MaxArtifactEntries is how many packages it may hold.
	MaxArtifactEntries int
	// MaxMetadataEntries is how many metadata answers it may hold.
	MaxMetadataEntries int
	// Retention drops an entry nothing has touched for this long. It is the
	// only time-based removal, it is explicit, and it runs when GC is called
	// rather than on a timer: AO deletes nothing on a schedule nobody asked
	// for.
	Retention time.Duration
}

// DefaultCacheLimits are the limits an installation gets without saying
// anything. They are sized for "several private registries, a few dozen
// packages", not for mirroring a marketplace.
func DefaultCacheLimits() CacheLimits {
	return CacheLimits{
		MetadataTTL:        10 * time.Minute,
		MaxArtifactBytes:   2 << 30, // 2 GiB
		MaxArtifactEntries: 256,
		MaxMetadataEntries: 4096,
		Retention:          30 * 24 * time.Hour,
	}
}

// Normalized fills in any zero field from the defaults, so a partially
// configured limit set cannot accidentally mean "unbounded".
func (l CacheLimits) Normalized() CacheLimits {
	d := DefaultCacheLimits()
	if l.MetadataTTL <= 0 {
		l.MetadataTTL = d.MetadataTTL
	}
	if l.MaxArtifactBytes <= 0 {
		l.MaxArtifactBytes = d.MaxArtifactBytes
	}
	if l.MaxArtifactEntries <= 0 {
		l.MaxArtifactEntries = d.MaxArtifactEntries
	}
	if l.MaxMetadataEntries <= 0 {
		l.MaxMetadataEntries = d.MaxMetadataEntries
	}
	if l.Retention <= 0 {
		l.Retention = d.Retention
	}
	return l
}

// Freshness is how current a piece of metadata is. It is three values rather
// than a boolean because "AO asked and this is the answer", "AO asked before
// and could not ask again" and "AO did not ask at all" are three different
// things to put in front of a person.
type Freshness string

const (
	// FreshnessLive means this came from the registry during this request.
	FreshnessLive Freshness = "live"
	// FreshnessCached means it came from cache and is inside the TTL.
	FreshnessCached Freshness = "cached"
	// FreshnessStale means it came from cache and the TTL has passed, or the
	// registry could not be reached. It must be rendered as such.
	FreshnessStale Freshness = "stale"
)

// Current reports whether this freshness may be shown without a stale marker.
func (f Freshness) Current() bool { return f == FreshnessLive || f == FreshnessCached }

// MetadataEntry is one cached registry answer.
type MetadataEntry struct {
	// Body is the raw response. It is stored verbatim and re-validated on
	// read, because a cache file is a file on this host.
	Body []byte `json:"body"`
	// ETag and LastModified are the registry's validators, replayed on the
	// next request so an unchanged answer costs a 304 rather than a body.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	// FetchedAt is when the registry actually answered. Every rendering of a
	// cached entry carries it: "as of" is the only honest way to show
	// something that may have changed.
	FetchedAt time.Time `json:"fetchedAt"`
}

// Fresh reports whether the entry is inside the TTL at now.
func (e MetadataEntry) Fresh(now time.Time, ttl time.Duration) bool {
	return !e.FetchedAt.IsZero() && now.Sub(e.FetchedAt) < ttl
}

// ArtifactEntry is one cached package tree.
type ArtifactEntry struct {
	RegistryID string `json:"registryId"`
	SkillID    string `json:"skillId"`
	Version    string `json:"version"`
	// ArtifactDigest is the key. It is skillcatalog's package digest over the
	// unpacked tree, which is what an install compares against, so the cache
	// is addressed by the same measurement the install verifies.
	ArtifactDigest string `json:"artifactDigest"`
	// ManifestDigest covers the manifest, which the package digest
	// deliberately excludes. Both are recorded so an offline install can check
	// both without asking anybody.
	ManifestDigest string `json:"manifestDigest"`
	// Bytes is the unpacked size, for the GC budget.
	Bytes int64 `json:"bytes"`
	// VerifiedAt is when AO last computed both digests over these bytes and
	// they matched. An entry with no verifiedAt is never reused.
	VerifiedAt time.Time `json:"verifiedAt"`
	// LastUsedAt drives retention.
	LastUsedAt time.Time `json:"lastUsedAt"`
	// Release is the exact release these bytes were verified AGAINST.
	//
	// It is here rather than only in the metadata cache because the two answer
	// different questions. The metadata cache holds whatever the registry last
	// said; this holds what AO measured bytes against and found to match. An
	// offline install acts on the second, so an offline install can never be
	// steered by a stale listing.
	Release Release `json:"release"`
	dir     string
}

// Dir is where the package tree lives.
func (e ArtifactEntry) Dir() string { return e.dir }

// Cache is the on-disk registry cache. It lives under AO's data dir, like
// everything else AO stores.
type Cache struct {
	root   string
	limits CacheLimits
	now    func() time.Time
}

// NewCache opens the cache rooted at dir. A nil Cache is a working "no cache":
// every method on it misses, stores nothing and reports nothing, so an
// installation with no cache configured behaves like one whose cache is always
// empty rather than failing.
func NewCache(dir string, limits CacheLimits) *Cache {
	return &Cache{root: dir, limits: limits.Normalized(), now: func() time.Time { return time.Now().UTC() }}
}

// Limits returns the effective limits.
func (c *Cache) Limits() CacheLimits {
	if c == nil {
		return CacheLimits{}
	}
	return c.limits
}

func (c *Cache) metadataDir(registryID string) string {
	return filepath.Join(c.root, "metadata", safeSegment(registryID))
}

func (c *Cache) artifactDir(registryID, contentKey string) string {
	return filepath.Join(c.root, "artifacts", safeSegment(registryID), contentKey)
}

// ContentKey is the artifact cache's address: BOTH digests, together.
//
// The artifact digest alone is not a release identity, and this is not a
// theoretical point -- skillcatalog.ComputePackageDigest deliberately EXCLUDES
// the manifest, because the manifest carries that digest and so cannot cover
// itself. Two releases whose only difference is their manifest -- which is
// every ordinary version bump of a skill whose code did not change -- therefore
// hash to the SAME artifact digest.
//
// Keying on it alone would have served 0.1.0's tree for a 0.2.0 install, and
// the manifest check downstream would have caught it as a digest mismatch: a
// correct refusal of a correct package, which is the worst kind of bug because
// it looks like the security control working.
//
// So the key is both, and the two digests together cover every byte.
func ContentKey(artifactDigest, manifestDigest string) string {
	sum := sha256.Sum256([]byte(artifactDigest + ":" + manifestDigest))
	return hex.EncodeToString(sum[:])
}

// safeSegment reduces an identifier to something that cannot escape the cache
// root. Registry ids are already kebab-case by validation; this is the second
// check, because a path built from an identifier is exactly where the first
// check turns out to have been somewhere else.
func safeSegment(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "_"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// ------------------------------------------------------------ metadata cache

// GetMetadata returns the cached answer for one request key.
func (c *Cache) GetMetadata(registryID, key string) (MetadataEntry, error) {
	if c == nil {
		return MetadataEntry{}, ErrCacheMiss
	}
	b, err := os.ReadFile(c.metadataPath(registryID, key)) //nolint:gosec // path built from a hashed key under the cache root.
	if err != nil {
		return MetadataEntry{}, ErrCacheMiss
	}
	var entry MetadataEntry
	if err := json.Unmarshal(b, &entry); err != nil || entry.FetchedAt.IsZero() {
		// A corrupt entry is a miss, not an error. The registry is the
		// authority; a damaged copy of an old answer buys nothing.
		return MetadataEntry{}, ErrCacheMiss
	}
	return entry, nil
}

// PutMetadata stores one answer. A failure to write is not an error the caller
// needs: the request succeeded, and a cache that could not record it only costs
// the next request a round trip.
func (c *Cache) PutMetadata(registryID, key string, entry MetadataEntry) {
	if c == nil || len(entry.Body) == 0 {
		return
	}
	dir := c.metadataDir(registryID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = writeFileAtomic(c.metadataPath(registryID, key), b)
}

func (c *Cache) metadataPath(registryID, key string) string {
	return filepath.Join(c.metadataDir(registryID), hashKey(key)+".json")
}

// ------------------------------------------------------------ artifact cache

// GetArtifact returns a cached package tree, RE-VERIFIED.
//
// It recomputes both digests over the bytes on disk before returning. That is
// the whole value of the entry: a cache directory is a file on this host, and
// an entry that was verified when it was written is not an entry that is
// verified now. A mismatch removes the entry and reports a miss.
func (c *Cache) GetArtifact(registryID, artifactDigest, manifestDigest string) (ArtifactEntry, error) {
	if c == nil || !digestRe.MatchString(artifactDigest) || !digestRe.MatchString(manifestDigest) {
		return ArtifactEntry{}, ErrCacheMiss
	}
	dir := c.artifactDir(registryID, ContentKey(artifactDigest, manifestDigest))
	b, err := os.ReadFile(filepath.Join(dir, artifactMetaFile)) //nolint:gosec // path under the cache root.
	if err != nil {
		return ArtifactEntry{}, ErrCacheMiss
	}
	var entry ArtifactEntry
	if err := json.Unmarshal(b, &entry); err != nil || entry.VerifiedAt.IsZero() {
		return ArtifactEntry{}, ErrCacheMiss
	}
	entry.dir = filepath.Join(dir, artifactTreeDir)
	if entry.ArtifactDigest != artifactDigest || entry.ManifestDigest != manifestDigest ||
		entry.RegistryID != registryID {
		// A sidecar that disagrees with the path it is stored at is a tampered
		// sidecar, and the path is the part AO controls.
		c.dropArtifact(dir)
		return ArtifactEntry{}, ErrCacheMiss
	}
	actual, err := skillcatalog.ComputePackageDigest(entry.dir)
	if err != nil || actual != artifactDigest {
		c.dropArtifact(dir)
		return ArtifactEntry{}, ErrCacheMiss
	}
	manifest, err := FileDigest(filepath.Join(entry.dir, skillcatalog.ManifestFileName))
	if err != nil || manifest != manifestDigest {
		c.dropArtifact(dir)
		return ArtifactEntry{}, ErrCacheMiss
	}
	return entry, nil
}

const (
	artifactMetaFile = "entry.json"
	artifactTreeDir  = "package"
)

// PutArtifact copies a verified package tree into the cache.
//
// The caller has already verified srcDir against the release; this records
// that. It refuses to store anything whose digest does not match the key, so a
// caller that got the order wrong writes nothing rather than poisoning the
// cache.
func (c *Cache) PutArtifact(entry ArtifactEntry, srcDir string) error {
	if c == nil {
		return nil
	}
	if !digestRe.MatchString(entry.ArtifactDigest) || !digestRe.MatchString(entry.ManifestDigest) {
		return fmt.Errorf("skillregistry: cache key must be two sha256 digests")
	}
	actual, err := skillcatalog.ComputePackageDigest(srcDir)
	if err != nil {
		return err
	}
	if actual != entry.ArtifactDigest {
		return fmt.Errorf("skillregistry: refusing to cache %s under %s; the bytes hash to %s",
			entry.SkillID, entry.ArtifactDigest, actual)
	}
	// The manifest is checked too, because the package digest above cannot
	// cover it and a sidecar naming the wrong manifest digest would make the
	// entry unusable at read time in a way nobody could diagnose.
	manifest, err := FileDigest(filepath.Join(srcDir, skillcatalog.ManifestFileName))
	if err != nil {
		return err
	}
	if manifest != entry.ManifestDigest {
		return fmt.Errorf("skillregistry: refusing to cache %s; its manifest hashes to %s and the "+
			"entry declares %s", entry.SkillID, manifest, entry.ManifestDigest)
	}
	dir := c.artifactDir(entry.RegistryID, ContentKey(entry.ArtifactDigest, entry.ManifestDigest))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tree := filepath.Join(dir, artifactTreeDir)
	if err := skillcatalog.CopyPackage(srcDir, tree); err != nil {
		return err
	}
	now := c.now()
	entry.VerifiedAt = now
	entry.LastUsedAt = now
	entry.Bytes = treeBytes(tree)
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, artifactMetaFile), b)
}

// TouchArtifact records a reuse, which is what retention is measured against.
func (c *Cache) TouchArtifact(registryID, artifactDigest, manifestDigest string) {
	if c == nil {
		return
	}
	dir := c.artifactDir(registryID, ContentKey(artifactDigest, manifestDigest))
	path := filepath.Join(dir, artifactMetaFile)
	b, err := os.ReadFile(path) //nolint:gosec // path under the cache root.
	if err != nil {
		return
	}
	var entry ArtifactEntry
	if err := json.Unmarshal(b, &entry); err != nil {
		return
	}
	entry.LastUsedAt = c.now()
	if out, err := json.Marshal(entry); err == nil {
		_ = writeFileAtomic(path, out)
	}
}

func (c *Cache) dropArtifact(dir string) { _ = os.RemoveAll(dir) }

// FindArtifactRelease returns the release recorded for one cached (skill,
// version) of one registry, RE-VERIFIED against the bytes on disk.
//
// It walks the registry's entries rather than being keyed by name, because the
// cache is keyed by digest on purpose: a name-keyed lookup would be a lookup a
// registry could steer, and steering an offline install is precisely what a
// compromised registry would want.
func (c *Cache) FindArtifactRelease(registryID, skillID, version string) (Release, bool) {
	if c == nil {
		return Release{}, false
	}
	for _, entry := range c.listArtifacts() {
		if entry.RegistryID != registryID || entry.SkillID != skillID || entry.Version != version {
			continue
		}
		// Re-verify before handing it back. GetArtifact recomputes both
		// digests and removes a tampered entry, so this is the same check the
		// install itself will make -- run early so a poisoned entry cannot
		// even be OFFERED as an offline candidate.
		verified, err := c.GetArtifact(registryID, entry.ArtifactDigest, entry.ManifestDigest)
		if err != nil {
			continue
		}
		if verified.Release.SkillID != skillID || verified.Release.Version != version {
			continue
		}
		return verified.Release, true
	}
	return Release{}, false
}

// ------------------------------------------------------------------- garbage

// GCReport is what one collection removed.
type GCReport struct {
	MetadataRemoved int   `json:"metadataRemoved"`
	ArtifactRemoved int   `json:"artifactsRemoved"`
	BytesReclaimed  int64 `json:"bytesReclaimed"`
	ArtifactsKept   int   `json:"artifactsKept"`
	BytesKept       int64 `json:"bytesKept"`
}

// GC enforces the limits. It is EXPLICIT: nothing calls it on a timer, and it
// removes only cache entries -- never an install, never a catalog package,
// never a provenance row. A cache miss costs a download; the things it will not
// touch cost a decision.
func (c *Cache) GC() (GCReport, error) {
	var report GCReport
	if c == nil {
		return report, nil
	}
	now := c.now()
	report.MetadataRemoved = c.gcMetadata(now)

	entries := c.listArtifacts()
	// Oldest use first, so the budget removes what nobody has wanted.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].LastUsedAt.Before(entries[j].LastUsedAt) })
	var keptBytes int64
	kept := 0
	// Walk newest-first to decide what survives, then remove the rest.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		expired := !e.LastUsedAt.IsZero() && now.Sub(e.LastUsedAt) > c.limits.Retention
		overBudget := kept+1 > c.limits.MaxArtifactEntries || keptBytes+e.Bytes > c.limits.MaxArtifactBytes
		if expired || overBudget {
			c.dropArtifact(filepath.Dir(e.dir))
			report.ArtifactRemoved++
			report.BytesReclaimed += e.Bytes
			continue
		}
		kept++
		keptBytes += e.Bytes
	}
	report.ArtifactsKept = kept
	report.BytesKept = keptBytes
	return report, nil
}

func (c *Cache) gcMetadata(now time.Time) int {
	root := filepath.Join(c.root, "metadata")
	registries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	type meta struct {
		path string
		at   time.Time
	}
	var all []meta
	for _, reg := range registries {
		if !reg.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, reg.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			p := filepath.Join(root, reg.Name(), f.Name())
			info, err := f.Info()
			if err != nil {
				continue
			}
			all = append(all, meta{path: p, at: info.ModTime()})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	removed := 0
	for i, m := range all {
		if i >= c.limits.MaxMetadataEntries || now.Sub(m.at) > c.limits.Retention {
			if os.Remove(m.path) == nil {
				removed++
			}
		}
	}
	return removed
}

func (c *Cache) listArtifacts() []ArtifactEntry {
	root := filepath.Join(c.root, "artifacts")
	registries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []ArtifactEntry
	for _, reg := range registries {
		if !reg.IsDir() {
			continue
		}
		digests, err := os.ReadDir(filepath.Join(root, reg.Name()))
		if err != nil {
			continue
		}
		for _, d := range digests {
			if !d.IsDir() {
				continue
			}
			dir := filepath.Join(root, reg.Name(), d.Name())
			b, err := os.ReadFile(filepath.Join(dir, artifactMetaFile)) //nolint:gosec // under the cache root.
			if err != nil {
				continue
			}
			var entry ArtifactEntry
			if err := json.Unmarshal(b, &entry); err != nil {
				continue
			}
			entry.dir = filepath.Join(dir, artifactTreeDir)
			out = append(out, entry)
		}
	}
	return out
}

func treeBytes(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil //nolint:nilerr // a partial size is better than no GC budget.
		}
		total += info.Size()
		return nil
	})
	return total
}

// hashKey is the filename a request key gets. It is a hash rather than the key
// itself so a query string a person typed never becomes a filename on this
// host, and so a key can be any length.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func writeFileAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
