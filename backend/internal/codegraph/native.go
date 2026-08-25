package codegraph

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProviderNameNative is the Name() of the in-tree indexer.
const ProviderNameNative = "native"

// defaultMaxFileSize is the largest file the native indexer will read. Beyond
// it a file is almost certainly generated, vendored, or minified: parsing it
// costs more than the symbols are worth.
const defaultMaxFileSize int64 = 1 << 20

// defaultSkipDirs are directory names never descended into. They are build
// outputs, dependency trees, and VCS internals — indexing them would bury a
// project's own symbols under its dependencies'.
var defaultSkipDirs = []string{
	".git", ".hg", ".svn", ".ao", ".idea", ".vscode",
	"node_modules", "vendor", "bower_components",
	"dist", "build", "out", "target", "coverage", ".next", ".nuxt", ".turbo",
	"__pycache__", ".venv", "venv", ".mypy_cache", ".pytest_cache",
}

// NativeIndexer is AO's in-tree CodeGraphProvider: a symbol indexer built on
// the Go standard library, with no external tool, subprocess, or network
// dependency. It persists one graph per project root through a Store.
type NativeIndexer struct {
	store       *Store
	extractors  extractorSet
	skipDirs    map[string]bool
	maxFileSize int64
	now         func() time.Time
}

// Option customizes a NativeIndexer.
type Option func(*NativeIndexer)

// WithExtractors replaces the default language extractors.
func WithExtractors(extractors ...Extractor) Option {
	return func(n *NativeIndexer) { n.extractors = newExtractorSet(extractors) }
}

// WithMaxFileSize caps the size of a file the indexer will read. A
// non-positive value restores the default.
func WithMaxFileSize(limit int64) Option {
	return func(n *NativeIndexer) {
		if limit <= 0 {
			limit = defaultMaxFileSize
		}
		n.maxFileSize = limit
	}
}

// WithSkipDirs replaces the default list of directory names that are never
// descended into.
func WithSkipDirs(names ...string) Option {
	return func(n *NativeIndexer) { n.skipDirs = nameSet(names) }
}

// WithClock replaces the source of index timestamps, so callers that need
// deterministic output can supply one.
func WithClock(now func() time.Time) Option {
	return func(n *NativeIndexer) {
		if now != nil {
			n.now = now
		}
	}
}

// NewNativeIndexer returns an indexer persisting through store.
func NewNativeIndexer(store *Store, opts ...Option) (*NativeIndexer, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: a store is required", ErrStorePath)
	}
	indexer := &NativeIndexer{
		store:       store,
		extractors:  newExtractorSet(DefaultExtractors()),
		skipDirs:    nameSet(defaultSkipDirs),
		maxFileSize: defaultMaxFileSize,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(indexer)
	}
	return indexer, nil
}

// NativeIndexer is the reference implementation of the adapter boundary; the
// assertion keeps it that way.
var _ CodeGraphProvider = (*NativeIndexer)(nil)

// Name implements CodeGraphProvider.
func (n *NativeIndexer) Name() string { return ProviderNameNative }

// Index implements CodeGraphProvider. It walks the whole project, but still
// consults the persisted graph per file: a file whose content hash is
// unchanged is counted as skipped and never re-parsed, so a repeat index over
// a quiet tree costs a read and a hash per file, not a parse.
func (n *NativeIndexer) Index(ctx context.Context, req IndexRequest) (IndexResult, error) {
	root, err := n.projectRoot(req.ProjectRoot)
	if err != nil {
		return IndexResult{}, err
	}
	graph, _, err := n.store.Load(root)
	if err != nil {
		return IndexResult{}, err
	}

	candidates, err := n.walk(ctx, root)
	if err != nil {
		return IndexResult{}, err
	}

	result := IndexResult{ProjectRoot: root, FullIndex: true}
	present := make(map[string]bool, len(candidates))
	for _, rel := range candidates {
		if err := ctx.Err(); err != nil {
			return IndexResult{}, err
		}
		present[rel] = true
		if err := n.syncFile(graph, root, rel, &result); err != nil {
			return IndexResult{}, err
		}
	}
	for _, rel := range graph.Paths() {
		if present[rel] {
			continue
		}
		if graph.Remove(rel) {
			result.FilesRemoved++
			result.RemovedFiles = append(result.RemovedFiles, rel)
		}
	}
	return n.commit(graph, result)
}

// IncrementalUpdate implements CodeGraphProvider. Only the paths the diff
// names are touched; of those, only the ones whose content hash actually
// differs from the persisted entry are re-parsed. A rename whose bytes did
// not change is re-keyed in place, not re-parsed.
func (n *NativeIndexer) IncrementalUpdate(ctx context.Context, req UpdateRequest) (IndexResult, error) {
	root, err := n.projectRoot(req.ProjectRoot)
	if err != nil {
		return IndexResult{}, err
	}
	graph, found, err := n.store.Load(root)
	if err != nil {
		return IndexResult{}, err
	}
	if !found {
		// Nothing to update against. A full index is what the caller wants,
		// and FullIndex in the result says that is what they got.
		return n.Index(ctx, IndexRequest{ProjectRoot: root})
	}

	changes := append([]FileChange(nil), req.Diff.Changes...)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	result := IndexResult{ProjectRoot: root}
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return IndexResult{}, err
		}
		rel := normalizeRel(change.Path)
		if rel == "" {
			continue
		}
		switch change.Status {
		case ChangeDeleted:
			if graph.Remove(rel) {
				result.FilesRemoved++
				result.RemovedFiles = append(result.RemovedFiles, rel)
			}
		case ChangeRenamed:
			if err := n.applyRename(graph, root, normalizeRel(change.OldPath), rel, &result); err != nil {
				return IndexResult{}, err
			}
		case ChangeAdded, ChangeModified:
			if err := n.syncFile(graph, root, rel, &result); err != nil {
				return IndexResult{}, err
			}
		default:
			return IndexResult{}, fmt.Errorf("codegraph: unsupported change status %q for %q", change.Status, rel)
		}
	}
	return n.commit(graph, result)
}

// Query implements CodeGraphProvider. It is read-only: a project with no
// persisted graph yields ErrNotIndexed rather than a silent re-index.
func (n *NativeIndexer) Query(ctx context.Context, req QueryRequest) (QueryResult, error) {
	if err := ctx.Err(); err != nil {
		return QueryResult{}, err
	}
	root, err := n.projectRoot(req.ProjectRoot)
	if err != nil {
		return QueryResult{}, err
	}
	graph, found, err := n.store.Load(root)
	if err != nil {
		return QueryResult{}, err
	}
	if !found {
		return QueryResult{}, fmt.Errorf("%w: %s", ErrNotIndexed, root)
	}
	return graph.Query(req)
}

// applyRename moves an entry to its new path. When the bytes behind the
// rename are unchanged, the entry is re-keyed — its symbol IDs and edges are
// rewritten to the new path — and nothing is parsed.
func (n *NativeIndexer) applyRename(graph *Graph, root, oldRel, newRel string, result *IndexResult) error {
	if oldRel == "" || oldRel == newRel {
		return n.syncFile(graph, root, newRel, result)
	}
	previous, hadPrevious := graph.Lookup(oldRel)
	if !hadPrevious {
		return n.syncFile(graph, root, newRel, result)
	}

	data, ok, err := n.readCandidate(root, newRel)
	if err != nil {
		return err
	}
	if !ok {
		// The rename destination is gone or is not indexable (renamed to a
		// .txt, deleted right after). Dropping the old entry is the whole
		// update.
		if graph.Remove(oldRel) {
			result.FilesRemoved++
			result.RemovedFiles = append(result.RemovedFiles, oldRel)
		}
		return nil
	}
	if hashBytes(data) == previous.Hash {
		graph.Rekey(oldRel, newRel)
		result.FilesRenamed++
		result.FilesSkipped++
		return nil
	}
	graph.Remove(oldRel)
	return n.syncFile(graph, root, newRel, result)
}

// syncFile brings one path in the graph up to date with the working tree.
// The content hash is the gate: an unchanged file is counted as skipped and
// its persisted symbols and edges are left exactly as they are.
func (n *NativeIndexer) syncFile(graph *Graph, root, rel string, result *IndexResult) error {
	extractor, supported := n.extractors.find(rel)
	if !supported {
		if graph.Remove(rel) {
			result.FilesRemoved++
			result.RemovedFiles = append(result.RemovedFiles, rel)
		}
		return nil
	}
	data, ok, err := n.readCandidate(root, rel)
	if err != nil {
		return err
	}
	if !ok {
		if graph.Remove(rel) {
			result.FilesRemoved++
			result.RemovedFiles = append(result.RemovedFiles, rel)
		}
		return nil
	}

	hash := hashBytes(data)
	if previous, exists := graph.Lookup(rel); exists && previous.Hash == hash && previous.Language == extractor.Language() {
		result.FilesSkipped++
		return nil
	}

	extraction, err := extractor.Extract(rel, data)
	if err != nil {
		return fmt.Errorf("codegraph: extract %s: %w", rel, err)
	}
	graph.Put(FileEntry{
		Path:     rel,
		Hash:     hash,
		Language: extractor.Language(),
		Size:     int64(len(data)),
		Symbols:  dedupeSymbols(extraction.Symbols),
		Edges:    dedupeEdges(extraction.Edges),
	})
	result.FilesParsed++
	result.ParsedFiles = append(result.ParsedFiles, rel)
	return nil
}

// readCandidate reads a project-relative file if it is one the indexer will
// consider: an existing regular file within the size cap. ok=false means "not
// indexable", which is a normal outcome, not an error.
func (n *NativeIndexer) readCandidate(root, rel string) (data []byte, ok bool, err error) {
	abs, exists, err := n.resolve(root, rel)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("codegraph: stat %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() || info.Size() > n.maxFileSize {
		return nil, false, nil
	}
	data, err = os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("codegraph: read %s: %w", rel, err)
	}
	return data, true, nil
}

// resolve turns a project-relative path into an absolute one, refusing any
// path that does not genuinely live beneath the project root. A diff is data
// from outside the process; neither a "../../etc/passwd" entry nor a
// "linked/secret.go" entry that reaches another checkout through a symlinked
// directory inside this one may make the indexer read — or file under this
// project's graph — a file the caller did not ask about.
//
// Lexical cleaning alone cannot decide this: it sees no ".." in
// "linked/secret.go", and os.Lstat only ever inspects the final component. So
// the parent directory is symlink-resolved and proven to be inside the
// canonical root before anything is opened. The final component is left
// unresolved on purpose — readCandidate's Lstat then sees a symlink for what
// it is and declines it as a non-regular file.
//
// exists=false means the path (or a directory on the way to it) is simply not
// there, which is a normal outcome for a stale or partially-applied diff, not
// an error.
func (n *NativeIndexer) resolve(root, rel string) (abs string, exists bool, err error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("%w: path %q escapes the project root", ErrProjectRoot, rel)
	}

	candidate := filepath.Join(root, clean)
	parent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("codegraph: resolve %s: %w", rel, err)
	}
	if !containedIn(root, parent) {
		return "", false, fmt.Errorf("%w: path %q leaves the project root through a symlink", ErrProjectRoot, rel)
	}
	return filepath.Join(parent, filepath.Base(candidate)), true, nil
}

// containedIn reports whether path is root itself or sits beneath it. Both
// must already be absolute and symlink-resolved for the answer to mean
// anything.
func containedIn(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// walk collects every indexable project-relative path under root.
func (n *NativeIndexer) walk(ctx context.Context, root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable directory is skipped rather than fatal: one
			// permission-denied subtree should not cost the whole index.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && n.skipDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativize %s: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		if _, supported := n.extractors.find(rel); !supported {
			return nil
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("codegraph: walk %s: %w", root, err)
	}
	sort.Strings(found)
	return found, nil
}

// commit stamps, persists, and finishes a result.
func (n *NativeIndexer) commit(graph *Graph, result IndexResult) (IndexResult, error) {
	now := n.now().UTC()
	graph.IndexedAt = now
	path, err := n.store.Save(graph)
	if err != nil {
		return IndexResult{}, err
	}
	symbols, edges := graph.Counts()
	result.SymbolCount = symbols
	result.EdgeCount = edges
	result.IndexedAt = now
	result.StorePath = path
	sort.Strings(result.ParsedFiles)
	sort.Strings(result.RemovedFiles)
	return result, nil
}

// projectRoot canonicalizes and validates the requested root.
func (n *NativeIndexer) projectRoot(raw string) (string, error) {
	root, err := CanonicalRoot(raw)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrProjectRoot, raw, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %q is not a directory", ErrProjectRoot, raw)
	}
	return root, nil
}

func dedupeSymbols(symbols []Symbol) []Symbol {
	if len(symbols) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(symbols))
	out := make([]Symbol, 0, len(symbols))
	for _, sym := range symbols {
		if seen[sym.ID] {
			continue
		}
		seen[sym.ID] = true
		out = append(out, sym)
	}
	return out
}

func dedupeEdges(edges []Edge) []Edge {
	if len(edges) == 0 {
		return nil
	}
	seen := make(map[Edge]bool, len(edges))
	out := make([]Edge, 0, len(edges))
	for _, edge := range edges {
		if seen[edge] {
			continue
		}
		seen[edge] = true
		out = append(out, edge)
	}
	return out
}

func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}
