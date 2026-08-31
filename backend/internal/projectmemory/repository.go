package projectmemory

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// repository.go — the durable port the P2-A indexer, drift detector, graph and
// pack builder are written against.
//
// This file is the boundary between "what project memory does" and "where
// project memory is kept". Everything above it (indexer.go, drift.go,
// graph.go, pack.go, service.go) is pure logic over this interface; the only
// implementation AO ships is the SQLite store behind migration 0144. The
// interface is declared here, at the consumer, rather than in internal/ports,
// following the same local-narrow-interface pattern
// internal/workflow/evidence_snapshot.go uses.
//
// Note what this file is NOT. The older JSON-file Store in this package
// (store.go) and the baseline evidence reader (baseline.go) are still the
// Phase-0 measurement path and are untouched; they were never a durable
// project-memory *writer* in production (docs/p2-project-memory-audit.md
// §4.2). Repository is the durable writer that audit found missing, and it is
// deliberately a second surface rather than a rewrite of the first — the
// baseline recorder must keep measuring exactly what it measured before, or
// the before/after in docs/project-memory-baseline.md means nothing.

// Repository is the durable state project memory is kept in.
//
// Two guarantees are part of the contract rather than of any one
// implementation, and a second implementation must honour both:
//
//   - Every write is generation-conditioned. A Put whose generation is behind
//     the stored row's must be refused with store.ErrProjectMemoryStaleGeneration
//     and must not modify the row.
//   - Invalidation marks; it does not delete. A caller that asks for an item
//     to go stale must still be able to read it back, because a stale fact is
//     the cheapest starting point for re-deriving the current one.
type Repository interface {
	// EnsureProjectMemoryRepo registers a repository for indexing,
	// idempotently.
	EnsureProjectMemoryRepo(ctx context.Context, projectID domain.ProjectID, repoID, repoPath string, now time.Time) error

	// GetProjectMemoryIndexState reads one repository's pass state.
	GetProjectMemoryIndexState(ctx context.Context, projectID domain.ProjectID, repoID string) (domain.ProjectMemoryIndexState, bool, error)
	// ListProjectMemoryIndexStates reads every repository of one project.
	ListProjectMemoryIndexStates(ctx context.Context, projectID domain.ProjectID) ([]domain.ProjectMemoryIndexState, error)

	// ClaimProjectMemoryIndexPass allocates the next generation and claims the
	// pass, succeeding only from a terminal phase.
	ClaimProjectMemoryIndexPass(ctx context.Context, projectID domain.ProjectID, repoID, pendingCommit, branch string, now time.Time) (domain.ProjectMemoryIndexState, bool, error)
	// ResumeProjectMemoryIndexPass takes over a pass left in flight by a crash,
	// conditional on the generation the caller read.
	ResumeProjectMemoryIndexPass(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, phase domain.ProjectMemoryIndexPhase, pendingCommit, branch string, now time.Time) (domain.ProjectMemoryIndexState, bool, error)
	// AdvanceProjectMemoryIndexPass records progress and the resume cursor.
	AdvanceProjectMemoryIndexPass(ctx context.Context, st domain.ProjectMemoryIndexState, now time.Time) (bool, error)
	// CompleteProjectMemoryIndexPass promotes the pending commit and returns
	// the repository to idle.
	CompleteProjectMemoryIndexPass(ctx context.Context, st domain.ProjectMemoryIndexState, now time.Time) (bool, error)
	// FailProjectMemoryIndexPass ends a pass on an error, keeping the
	// generation.
	FailProjectMemoryIndexPass(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, reason string, now time.Time) (bool, error)

	// PutProjectMemoryItem writes one fact under generation-conditioned CAS.
	PutProjectMemoryItem(ctx context.Context, item domain.ProjectMemoryItem, now time.Time) (store.ProjectMemoryWriteOutcome, error)
	// PutProjectMemoryRelation writes one edge under the same fence.
	PutProjectMemoryRelation(ctx context.Context, rel domain.ProjectMemoryRelation, now time.Time) (store.ProjectMemoryWriteOutcome, error)

	// GetProjectMemoryItem reads one fact by its derived identity.
	GetProjectMemoryItem(ctx context.Context, id string) (domain.ProjectMemoryItem, bool, error)
	// ListProjectMemoryItems reads every fact of one repository.
	ListProjectMemoryItems(ctx context.Context, projectID domain.ProjectID, repoID string) ([]domain.ProjectMemoryItem, error)
	// ListProjectMemoryItemsByState narrows that read to one state.
	ListProjectMemoryItemsByState(ctx context.Context, projectID domain.ProjectID, repoID string, state domain.ProjectMemoryState) ([]domain.ProjectMemoryItem, error)
	// ListProjectMemoryItemsForProject spans every repository of a project.
	ListProjectMemoryItemsForProject(ctx context.Context, projectID domain.ProjectID) ([]domain.ProjectMemoryItem, error)
	// ListProjectMemoryItemsForTask reads one task's unintegrated facts.
	ListProjectMemoryItemsForTask(ctx context.Context, projectID domain.ProjectID, taskRef string) ([]domain.ProjectMemoryItem, error)
	// ListProjectMemoryItemsByPath is the reverse provenance lookup.
	ListProjectMemoryItemsByPath(ctx context.Context, projectID domain.ProjectID, repoID, path string) ([]domain.ProjectMemoryItem, error)

	// MarkProjectMemoryItemState moves one fact between states.
	MarkProjectMemoryItemState(ctx context.Context, id string, generation int64, state domain.ProjectMemoryState, reason string, now time.Time) (bool, error)
	// InvalidateProjectMemoryByPath retires everything derived from one path.
	InvalidateProjectMemoryByPath(ctx context.Context, projectID domain.ProjectID, repoID, path string, state domain.ProjectMemoryState, reason string, now time.Time) (int64, int64, error)
	// RetireProjectMemoryBelowGeneration retires what a completed full pass
	// did not re-confirm.
	RetireProjectMemoryBelowGeneration(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, reason string, now time.Time) (int64, int64, error)

	// ListProjectMemoryRelations reads every edge of one repository.
	ListProjectMemoryRelations(ctx context.Context, projectID domain.ProjectID, repoID string) ([]domain.ProjectMemoryRelation, error)
	// ListProjectMemoryRelationsFrom traverses outward from a node.
	ListProjectMemoryRelationsFrom(ctx context.Context, projectID domain.ProjectID, repoID string, from domain.ProjectMemoryNode, state domain.ProjectMemoryState) ([]domain.ProjectMemoryRelation, error)
	// ListProjectMemoryRelationsTo traverses inward to a node.
	ListProjectMemoryRelationsTo(ctx context.Context, projectID domain.ProjectID, repoID string, to domain.ProjectMemoryNode, state domain.ProjectMemoryState) ([]domain.ProjectMemoryRelation, error)

	// UpsertProjectMemoryFile records one path's digest under one generation.
	UpsertProjectMemoryFile(ctx context.Context, projectID domain.ProjectID, repoID, path, digest string, size, generation int64, commit string, now time.Time) error
	// GetProjectMemoryFile reads one path's ledger entry.
	GetProjectMemoryFile(ctx context.Context, projectID domain.ProjectID, repoID, path string) (store.ProjectMemoryFileRecord, bool, error)
	// ListProjectMemoryFiles reads the whole ledger for one repository.
	ListProjectMemoryFiles(ctx context.Context, projectID domain.ProjectID, repoID string) ([]store.ProjectMemoryFileRecord, error)
	// ListProjectMemoryFilesBelowGeneration names paths a pass did not
	// re-observe.
	ListProjectMemoryFilesBelowGeneration(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64) ([]store.ProjectMemoryFileRecord, error)
	// DeleteProjectMemoryFile drops one path's ledger entry.
	DeleteProjectMemoryFile(ctx context.Context, projectID domain.ProjectID, repoID, path string) (bool, error)
	// PruneProjectMemoryFilesBelowGeneration drops those ledger entries.
	PruneProjectMemoryFilesBelowGeneration(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64) (int64, error)

	// GetProjectMemoryStatus assembles the operator-facing view.
	GetProjectMemoryStatus(ctx context.Context, projectID domain.ProjectID, repoID string) (domain.ProjectMemoryStatus, bool, error)
	// PurgeProjectMemoryRepo deletes everything for one repository.
	PurgeProjectMemoryRepo(ctx context.Context, projectID domain.ProjectID, repoID string) error
	// DiscardProjectMemoryForTask retires one task's unintegrated facts.
	DiscardProjectMemoryForTask(ctx context.Context, projectID domain.ProjectID, taskRef string) (int64, int64, error)
}

// Compile-time proof that the SQLite store satisfies the port. If a method
// signature drifts, this line fails the build rather than a wiring site
// discovering it at runtime.
var _ Repository = (*store.Store)(nil)
