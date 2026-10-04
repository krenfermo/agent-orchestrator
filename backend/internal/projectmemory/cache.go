package projectmemory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// cache.go — reusing a pack only where its identity is strong (P2-B §15).
//
// Four roles reaching for the same repository at the same commit will often
// ask for overlapping packs, and assembling one costs a store read, a ranking
// pass and a render. Caching that is worth doing — but only against an identity
// strong enough that a hit cannot serve memory the caller would not have got
// by assembling it fresh.
//
// The key is therefore every authority that can change the answer:
//
//	project, repository, indexed commit, memory generation,
//	the change mark (P2-D), role, budget policy version + the role's own
//	budget, and the selection scope (changed paths, modules, keywords, task ref)
//
// **Not the prompt text.** A prompt is not an authority: two dispatches with
// identical prompts against different generations must not share a pack, and
// two with different prompts against the same authority legitimately do. Keying
// on text would get both cases wrong.
//
// Generation and indexed commit are what make invalidation implicit rather than
// a job somebody has to remember: a sync that advances either one changes every
// key derived from it, so the previous entries are simply never asked for
// again. There is no invalidation path to forget to call, and no window in
// which a stale pack is reachable.
//
// **They are not sufficient on their own**, which P2-D made visible. Both move
// only when an INDEXING PASS runs, and every out-of-band demotion -- drift
// invalidation, `ao memory invalidate`, an authority pass, a promotion
// recording a refusal -- changes what a reader should be served without
// touching either. A pack cached moments before such a demotion stayed
// reachable and kept serving a fact AO had just withheld.
//
// ChangeMark closes that. It is the newest `updated_at` of the repository's
// facts, which advances on any write that can alter what is servable and on
// nothing else, so the same implicit-invalidation argument now covers the
// authority axis too. It costs one indexed-prefix row read per provision,
// which is the same order as the freshness check beside it.

// cacheCapacity bounds the cache. It is small on purpose: the working set is
// "the roles of the runs active right now", and a cache that grows past that is
// holding packs for generations nothing will ask about again.
const cacheCapacity = 128

// CacheKey is the authority a pack is cached under.
type CacheKey struct {
	ProjectID     domain.ProjectID
	RepoID        string
	IndexedCommit string
	Generation    int64
	// ChangeMark is the instant this repository's memory last changed in any
	// way that can alter what is servable. Nanoseconds, so two demotions in one
	// millisecond are still two keys.
	ChangeMark int64
	// GraphGeneration is the code graph's served generation. It is part of the
	// key because graph evidence is part of the pack: a graph rebuilt between
	// two otherwise identical dispatches is entitled to a different answer, and
	// a cache that ignored it would serve the first dispatch's structure to the
	// second.
	GraphGeneration int64
	Role            PackRole
	PolicyVersion   int
	Budget          RoleBudget
	// Scope is the digest of everything about the REQUEST that changes
	// selection: the changed paths, the modules, the keywords and the task
	// ref. It is a digest rather than the values so the key stays fixed-width
	// however long a change set is.
	Scope string
}

// String renders the key as a stable digest, which is what the metrics report
// and what a map is keyed by.
func (k CacheKey) String() string {
	h := sha256.New()
	// hash.Hash.Write never errors; the discard matches the idiom used
	// throughout this package rather than a swallowed failure.
	_, _ = fmt.Fprintf(h, "v%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%d/%d/%d\x00%s",
		k.PolicyVersion, k.ProjectID, k.RepoID, k.IndexedCommit, k.Generation, k.ChangeMark,
		k.GraphGeneration, k.Role,
		k.Budget.MaxBytes, k.Budget.MaxItems, k.Budget.MaxDocuments, k.Scope)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Cacheable reports whether this key names an authority strong enough to cache
// against.
//
// A pack built with no indexed commit or no generation is a pack AO cannot
// prove the provenance of, and caching it would mean serving it again after
// the repository moved. Such packs are assembled fresh every time — which
// costs a little and cannot be wrong.
func (k CacheKey) Cacheable() bool {
	return k.IndexedCommit != "" && k.Generation > 0 && k.RepoID != "" && k.Role.Valid()
}

// ScopeDigest folds a request's selection inputs into the key's Scope.
//
// The inputs are normalised (trimmed, de-duplicated, sorted) before hashing, so
// two callers that named the same changed paths in different orders share a
// cache entry — they would have got the same pack, because selection itself is
// order-independent.
func ScopeDigest(changedPaths, modules, keywords []string, taskRef string) string {
	h := sha256.New()
	for _, group := range [][]string{changedPaths, modules, keywords} {
		for _, v := range normalizeScope(group) {
			h.Write([]byte(v))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	h.Write([]byte(strings.TrimSpace(taskRef)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func normalizeScope(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = normalizePath(v)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// PackCache is a bounded in-process cache of assembled packs.
type PackCache struct {
	mu      sync.Mutex
	enabled bool
	entries map[string]ContextPack
	// order is insertion order, used for the eviction sweep. A pack is cheap
	// to rebuild, so eviction is a simple bounded sweep rather than an LRU
	// with its own bookkeeping.
	order []string
	hits  int64
	miss  int64
}

// NewPackCache builds the cache. A disabled cache answers every lookup with a
// miss and stores nothing, so the caller's code path is identical either way.
func NewPackCache(enabled bool) *PackCache {
	return &PackCache{enabled: enabled, entries: map[string]ContextPack{}}
}

// Get returns a cached pack for the key.
func (c *PackCache) Get(key CacheKey) (ContextPack, bool) {
	if c == nil || !c.enabled || !key.Cacheable() {
		return ContextPack{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pack, ok := c.entries[key.String()]
	if ok {
		c.hits++
		return pack, true
	}
	c.miss++
	return ContextPack{}, false
}

// Put stores a pack under the key. A pack that fell back — one with a stated
// reason and no items — is not cached: the reason is usually transient (a sync
// in flight, a repository momentarily unreadable), and caching it would make a
// passing condition stick until the generation moved.
func (c *PackCache) Put(key CacheKey, pack ContextPack) {
	if c == nil || !c.enabled || !key.Cacheable() || pack.Empty() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := key.String()
	if _, exists := c.entries[id]; !exists {
		c.order = append(c.order, id)
	}
	c.entries[id] = pack
	for len(c.order) > cacheCapacity {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

// CacheStats reports what the cache has done over the process's lifetime.
type CacheStats struct {
	Hits    int64
	Misses  int64
	Entries int
	Enabled bool
}

// Stats returns a snapshot.
func (c *PackCache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{Hits: c.hits, Misses: c.miss, Entries: len(c.entries), Enabled: c.enabled}
}

// Reset empties the cache. It exists for tests and for an operator's
// `memory rebuild`, which changes the store underneath every cached pack.
func (c *PackCache) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]ContextPack{}
	c.order = nil
}
