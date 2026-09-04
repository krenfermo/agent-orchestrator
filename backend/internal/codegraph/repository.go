package codegraph

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Package codegraph's durable port.
//
// repository.go — the durable port the project-scoped code graph is written
// through.
//
// It is declared here, at the consumer, rather than in internal/ports, the same
// local-narrow-interface pattern projectmemory/repository.go follows. The only
// implementation AO ships is the SQLite store behind migration 0153.
//
// Two guarantees belong to the CONTRACT rather than to that implementation,
// and a second implementation must honour both:
//
//   - A full build stages. Rows written at a generation above
//     served_generation are invisible to every reader until
//     CompleteCodeGraphBuild moves it. A store that made staged rows readable
//     would let a dispatch see half a rebuild, which is the one thing the
//     generation machinery exists to prevent.
//   - One path's file row, symbols and edges are written and deleted as a
//     unit. Anything else leaves zombie symbols behind a deleted file.
type Repository interface {
	// EnsureCodeGraphRepo registers a repository for indexing, idempotently.
	EnsureCodeGraphRepo(ctx context.Context, projectID domain.ProjectID, repoID, repoPath, backend string, now time.Time) error
	// GetCodeGraphState reads one repository's durable state.
	GetCodeGraphState(ctx context.Context, projectID domain.ProjectID, repoID string) (store.CodeGraphState, bool, error)
	// ListCodeGraphStates reads every registered repository of one project.
	ListCodeGraphStates(ctx context.Context, projectID domain.ProjectID) ([]store.CodeGraphState, error)

	// ClaimCodeGraphBuild allocates the next generation and claims a full
	// build, succeeding only from a terminal phase.
	ClaimCodeGraphBuild(ctx context.Context, projectID domain.ProjectID, repoID, pendingCommit, branch string, now time.Time) (store.CodeGraphState, bool, error)
	// ReclaimCodeGraphBuild takes over a build a crash left in flight.
	ReclaimCodeGraphBuild(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, pendingCommit, branch string, now time.Time) (store.CodeGraphState, bool, error)
	// CompleteCodeGraphBuild publishes a staged build and collects the
	// generation it replaces, in one transaction.
	CompleteCodeGraphBuild(ctx context.Context, c store.CodeGraphCompletion, now time.Time) (bool, error)
	// RecordCodeGraphIncremental records an in-place update.
	RecordCodeGraphIncremental(ctx context.Context, c store.CodeGraphCompletion, now time.Time) (bool, error)
	// FailCodeGraphBuild ends a build on an error, keeping the generation.
	FailCodeGraphBuild(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, reason string, now time.Time) (bool, error)

	// PutCodeGraphEntry writes one file's row, symbols and edges together.
	PutCodeGraphEntry(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, entry store.CodeGraphEntry, now time.Time) (store.CodeGraphDelta, error)
	// DeleteCodeGraphPath removes one path and everything derived from it.
	DeleteCodeGraphPath(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string) (store.CodeGraphDelta, error)
	// CopyCodeGraphPathForward carries an unchanged path into a staging
	// generation without re-parsing it.
	CopyCodeGraphPathForward(ctx context.Context, projectID domain.ProjectID, repoID string, from, to int64, path string) (bool, error)
	// DiscardCodeGraphStaging drops every row above the served generation.
	DiscardCodeGraphStaging(ctx context.Context, projectID domain.ProjectID, repoID string, served int64) error

	// GetCodeGraphFileRecord reads one path's ledger entry.
	GetCodeGraphFileRecord(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string) (store.CodeGraphFileRecord, bool, error)
	// ListCodeGraphFileRecords reads the whole ledger at a generation.
	ListCodeGraphFileRecords(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64) ([]store.CodeGraphFileRecord, error)
	// CountCodeGraph reports the row counts at a generation.
	CountCodeGraph(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64) (files, symbols, edges int64, err error)

	// SearchCodeGraphSymbols is the bounded candidate read over name, path and
	// summary. SearchCodeGraphSymbolNames narrows it to name and path, which
	// is what retrieval leads with: a name is a commitment about what a thing
	// is, and a summary is prose that happens to contain a word.
	SearchCodeGraphSymbols(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, term string, limit int64) ([]store.CodeGraphSymbolRecord, error)
	SearchCodeGraphSymbolNames(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, term string, limit int64) ([]store.CodeGraphSymbolRecord, error)
	// ListCodeGraphSymbolsForPath reads one file's declarations.
	ListCodeGraphSymbolsForPath(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string) ([]store.CodeGraphSymbolRecord, error)
	// ListCodeGraphSymbolsByName resolves a name to its declarations.
	ListCodeGraphSymbolsByName(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, name string, limit int64) ([]store.CodeGraphSymbolRecord, error)
	// ListCodeGraphEdgesFrom traverses outward from a node key.
	ListCodeGraphEdgesFrom(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, from string, limit int64) ([]store.CodeGraphEdgeRecord, error)
	// ListCodeGraphEdgesTo traverses inward to a node key.
	ListCodeGraphEdgesTo(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, to string, limit int64) ([]store.CodeGraphEdgeRecord, error)
	// ListCodeGraphEdgesFromKeys and ListCodeGraphEdgesToKeys are the batched
	// forms, and the ones the dispatch path uses. A bounded neighbourhood
	// costs one round trip in each direction rather than one per symbol --
	// which is what keeps retrieval from scaling with the size of the graph.
	ListCodeGraphEdgesFromKeys(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, keys []string) ([]store.CodeGraphEdgeRecord, error)
	ListCodeGraphEdgesToKeys(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, keys []string) ([]store.CodeGraphEdgeRecord, error)
	// ListCodeGraphSymbolsForPaths reads several files' declarations at once.
	ListCodeGraphSymbolsForPaths(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, paths []string) ([]store.CodeGraphSymbolRecord, error)
	// ListCodeGraphEdgesForPath reads one file's relations, which is how an
	// incremental update decides whether a change could have moved the
	// architecture summary.
	ListCodeGraphEdgesForPath(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string) ([]store.CodeGraphEdgeRecord, error)
	// LoadCodeGraph reads a whole generation back, for the architecture
	// summary only.
	LoadCodeGraph(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64) ([]store.CodeGraphFileRecord, []store.CodeGraphSymbolRecord, []store.CodeGraphEdgeRecord, error)

	// PurgeCodeGraph deletes every row for one repository, keeping its
	// registration.
	PurgeCodeGraph(ctx context.Context, projectID domain.ProjectID, repoID string) error
	// DeregisterCodeGraphRepo additionally forgets the registration.
	DeregisterCodeGraphRepo(ctx context.Context, projectID domain.ProjectID, repoID string) error
}

// Compile-time proof that the SQLite store satisfies the port. If a signature
// drifts, this line fails the build rather than a wiring site discovering it at
// runtime.
var _ Repository = (*store.Store)(nil)
