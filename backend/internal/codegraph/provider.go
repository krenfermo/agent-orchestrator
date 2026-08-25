// Package codegraph defines AO's provider-agnostic code-graph boundary and
// ships a native indexer that implements it.
//
// The boundary exists so AO is never hard-wired to one graph tool. A
// third-party tool (Graphify, an LSP-backed indexer, a hosted service) can be
// plugged in by implementing CodeGraphProvider; callers keep talking to the
// same three verbs — index a project, apply a git diff, ask a question — and
// stay unaware of which implementation is behind them.
//
// NativeIndexer is the in-tree implementation: it parses source with the Go
// standard library (no external process, no daemon, no network), hashes every
// file and symbol so an incremental update reprocesses only what actually
// changed, and persists each project's graph under AO's data dir keyed by the
// project root, so two checkouts never share or leak entries.
package codegraph

import (
	"context"
	"errors"
	"time"
)

// Errors returned across the provider boundary. Adapters should wrap these
// where the meaning matches so callers can branch on the cause rather than on
// an implementation's message text.
var (
	// ErrProjectRoot means the requested project root is unusable: empty,
	// relative, or not a directory.
	ErrProjectRoot = errors.New("codegraph: invalid project root")
	// ErrEmptyQuery means a query named neither a symbol nor a file.
	ErrEmptyQuery = errors.New("codegraph: query requires a symbol or a file")
	// ErrNotIndexed means the project has no persisted graph yet. Providers
	// that transparently fall back to a full index (NativeIndexer does) never
	// return it; providers that cannot are expected to.
	ErrNotIndexed = errors.New("codegraph: project has not been indexed")
)

// CodeGraphProvider is the adapter interface every code-graph implementation
// satisfies. It is deliberately small — a project root in, a graph fact out —
// so an external tool can be wrapped without exposing its internal model.
//
// Implementation contract for third-party adapters:
//
//   - All three methods must be safe to call with a cancelled context and must
//     abandon work promptly when ctx is done.
//   - ProjectRoot is an absolute path to a checkout. Every method must key its
//     state by that root: indexing project A must never observe, mutate, or
//     return entries belonging to project B, even when both are indexed by the
//     same provider instance. Return ErrProjectRoot for a root that is empty,
//     relative, or not a directory.
//   - Index performs a full pass over the project. It is idempotent: running it
//     twice over an unchanged tree must produce the same graph. Implementations
//     that can tell a file is unchanged (by content hash) should skip
//     re-parsing it and report it in IndexResult.FilesSkipped.
//   - IncrementalUpdate applies a git diff to the persisted graph. It must
//     handle added, modified, deleted, and renamed paths, and must not
//     re-scan paths the diff does not mention. Providers with no persisted
//     graph either fall back to a full index (setting IndexResult.FullIndex)
//     or return ErrNotIndexed.
//   - Query is read-only and must never mutate or lazily rebuild the graph.
//     A query naming neither a symbol nor a file returns ErrEmptyQuery; a
//     query against a project with no persisted graph returns ErrNotIndexed;
//     a query that matches nothing returns an empty result and a nil error.
//   - Persisted state, if any, must live under AO's data dir (see StoreRoot)
//     and never in an OS-default application-data location.
type CodeGraphProvider interface { //nolint:revive // the name is this boundary's published contract; a bare "Provider" would read as any provider.
	// Name identifies the implementation ("native", "graphify", ...). It is
	// used in logs and in operator-facing output, so it must be stable.
	Name() string

	// Index builds (or refreshes) the whole graph for a project root.
	Index(ctx context.Context, req IndexRequest) (IndexResult, error)

	// IncrementalUpdate applies the file-level changes of a git diff to an
	// already-persisted graph.
	IncrementalUpdate(ctx context.Context, req UpdateRequest) (IndexResult, error)

	// Query answers a symbol- or file-scoped question about the graph.
	Query(ctx context.Context, req QueryRequest) (QueryResult, error)
}

// IndexRequest asks a provider to index one project.
type IndexRequest struct {
	// ProjectRoot is the absolute path of the checkout to index.
	ProjectRoot string
}

// UpdateRequest asks a provider to apply one git diff to a project's graph.
type UpdateRequest struct {
	// ProjectRoot is the absolute path of the checkout the diff belongs to.
	ProjectRoot string
	// Diff lists the file-level changes to apply. An empty diff is valid and
	// is a no-op.
	Diff Diff
}

// QueryRequest is a read-only question about an indexed project. At least one
// of Symbol and File must be set.
type QueryRequest struct {
	// ProjectRoot is the absolute path of the indexed checkout.
	ProjectRoot string
	// Symbol matches a symbol by name, by fully-qualified ID, or — for a
	// method recorded as "Receiver.Method" — by its bare method name.
	Symbol string
	// File matches a file by project-relative path, by path suffix, or by
	// base name.
	File string
	// Limit caps the number of matched symbols and files returned. Zero or
	// negative means no cap.
	Limit int
}

// IndexResult reports what an index or incremental update did. The counts are
// the audit trail for "unchanged files were not reprocessed": FilesParsed is
// the number of files actually read and analyzed, FilesSkipped the number
// whose content hash matched the persisted entry.
type IndexResult struct {
	// ProjectRoot is the canonical root the graph is keyed by.
	ProjectRoot string
	// FullIndex reports that a full pass ran. IncrementalUpdate sets it when
	// it had no persisted graph to update and fell back to indexing.
	FullIndex bool
	// FilesParsed counts files read and analyzed during this call.
	FilesParsed int
	// FilesSkipped counts files left untouched because their content hash
	// still matched the persisted entry.
	FilesSkipped int
	// FilesRemoved counts entries dropped because the file is gone.
	FilesRemoved int
	// FilesRenamed counts entries moved to a new path without re-parsing,
	// because the content behind the rename was byte-identical.
	FilesRenamed int
	// SymbolCount and EdgeCount describe the whole graph after the call, not
	// just the touched part.
	SymbolCount int
	EdgeCount   int
	// ParsedFiles and RemovedFiles name the affected paths, sorted, so a
	// caller (or a test) can assert exactly which files were reprocessed.
	ParsedFiles  []string
	RemovedFiles []string
	// IndexedAt is when the graph was written.
	IndexedAt time.Time
	// StorePath is the file the graph was persisted to, empty for providers
	// that keep no local state.
	StorePath string
}
