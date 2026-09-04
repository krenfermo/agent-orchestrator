package codegraph

import (
	"context"
	"fmt"
	"os"
	"sort"
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
	store *Store
	// scanner is the shared filesystem half: which paths are admitted, how
	// they are read, and the containment proof that keeps a diff from reaching
	// outside the project. The durable, project-scoped Index uses the same
	// one, so "what AO will look at" has a single definition.
	scanner scanner
	now     func() time.Time
}

// Option customizes a NativeIndexer.
type Option func(*NativeIndexer)

// WithExtractors replaces the default language extractors.
func WithExtractors(extractors ...Extractor) Option {
	return func(n *NativeIndexer) { n.scanner.extractors = newExtractorSet(extractors) }
}

// WithMaxFileSize caps the size of a file the indexer will read. A
// non-positive value restores the default.
func WithMaxFileSize(limit int64) Option {
	return func(n *NativeIndexer) {
		if limit <= 0 {
			limit = defaultMaxFileSize
		}
		n.scanner.maxFileSize = limit
	}
}

// WithSkipDirs replaces the default list of directory names that are never
// descended into.
func WithSkipDirs(names ...string) Option {
	return func(n *NativeIndexer) { n.scanner.skipDirs = nameSet(names) }
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
		store:   store,
		scanner: newScanner(),
		now:     time.Now,
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

	candidates, err := n.scanner.walk(ctx, root)
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
	return n.commit(graph, result, req.Commit)
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
		return n.Index(ctx, IndexRequest{ProjectRoot: root, Commit: req.Commit})
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
	return n.commit(graph, result, req.Commit)
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

	data, ok, err := n.scanner.readCandidate(root, newRel)
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
	extractor, supported := n.scanner.extractors.find(rel)
	// DeniedPath is checked here as well as in the walk, and that is the
	// point: an incremental update names its own paths, so a diff mentioning
	// `secrets.py` would otherwise reach a file the walk would never have
	// visited. Refusing in both places closes the whole class.
	if supported && DeniedPath(rel) {
		supported = false
	}
	if !supported {
		if graph.Remove(rel) {
			result.FilesRemoved++
			result.RemovedFiles = append(result.RemovedFiles, rel)
		}
		return nil
	}
	data, ok, err := n.scanner.readCandidate(root, rel)
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
		Role:     ClassifyFile(rel, data),
		Size:     int64(len(data)),
		Symbols:  dedupeSymbols(extraction.Symbols),
		Edges:    dedupeEdges(extraction.Edges),
	})
	result.FilesParsed++
	result.ParsedFiles = append(result.ParsedFiles, rel)
	return nil
}

// commit stamps, persists, and finishes a result.
func (n *NativeIndexer) commit(graph *Graph, result IndexResult, atCommit string) (IndexResult, error) {
	now := n.now().UTC()
	graph.IndexedAt = now
	if atCommit != "" {
		graph.IndexedCommit = atCommit
	}
	result.IndexedCommit = graph.IndexedCommit
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
