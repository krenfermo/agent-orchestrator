package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// code_graph_store.go — the durable side of the code graph (migration 0153).
//
// The rules this file enforces are the ones the schema's own comment states,
// and they are worth restating where the code is, because every method below
// is one of them:
//
//   - A full build stages. It writes at generation N+1 while readers are still
//     served N, and becomes visible only when CompleteCodeGraphBuild moves
//     served_generation forward. There is no window in which a reader can see
//     half a rebuild.
//   - An incremental update writes in place, at the served generation, inside
//     one transaction. Atomicity is what makes staging unnecessary there.
//   - Every pass transition is generation-conditioned. A build that stalls and
//     wakes after a newer one finished matches zero rows and learns it lost.
//   - One path's file row, symbols and edges are written and deleted TOGETHER.
//     That is what makes deletion exact and leaves no zombie symbol behind: a
//     path's rows cannot outlive the path.

// CodeGraphPhase is the state of the code-graph pass.
type CodeGraphPhase string

// Code graph pass phases.
const (
	// CodeGraphIdle means no build is running. The served generation, if
	// non-zero, is complete.
	CodeGraphIdle CodeGraphPhase = "idle"
	// CodeGraphBuilding means a full build is staging at `Generation`.
	CodeGraphBuilding CodeGraphPhase = "building"
	// CodeGraphFailed means the last build ended on an error. The previously
	// served generation is untouched and still being served.
	CodeGraphFailed CodeGraphPhase = "failed"
)

// CodeGraphSyncKind names what a sync did, for the P3-E measurement.
type CodeGraphSyncKind string

// Code graph sync kinds.
const (
	// CodeGraphSyncFull is a whole-repository pass.
	CodeGraphSyncFull CodeGraphSyncKind = "full"
	// CodeGraphSyncIncremental applied a diff.
	CodeGraphSyncIncremental CodeGraphSyncKind = "incremental"
	// CodeGraphSyncNoop found nothing to do.
	CodeGraphSyncNoop CodeGraphSyncKind = "noop"
)

// CodeGraphState is one repository's durable code-graph state.
type CodeGraphState struct {
	ProjectID domain.ProjectID
	RepoID    string
	RepoPath  string
	// Backend names the MemoryGraph implementation that produced the graph.
	Backend string
	// Generation is the pass allocator. ServedGeneration is what readers
	// filter on, and the two differ exactly while a build is in flight.
	Generation       int64
	ServedGeneration int64
	Phase            CodeGraphPhase
	IndexedCommit    string
	PendingCommit    string
	Branch           string
	// RepoIdentity is the repository identity the served graph was derived
	// under. A mismatch is a fail-closed condition, not a warning.
	RepoIdentity string
	FileCount    int64
	SymbolCount  int64
	EdgeCount    int64
	// The last sync's measurements, which are what makes "incremental is
	// cheaper" a recorded fact rather than a claim.
	LastSyncKind       CodeGraphSyncKind
	LastFilesParsed    int64
	LastFilesReused    int64
	LastFilesRemoved   int64
	LastSymbolsAdded   int64
	LastSymbolsRemoved int64
	LastEdgesAdded     int64
	LastEdgesRemoved   int64
	LastDuration       time.Duration
	LastError          string
	// Architecture is the rendered bounded structural summary, and
	// ArchitectureJSON its structured form.
	Architecture     string
	ArchitectureJSON string
	StartedAt        sql.NullTime
	CompletedAt      sql.NullTime
	UpdatedAt        time.Time
}

// Indexed reports whether a complete graph is being served.
func (s CodeGraphState) Indexed() bool { return s.ServedGeneration > 0 }

// Building reports whether a full build is staging right now.
func (s CodeGraphState) Building() bool { return s.Phase == CodeGraphBuilding }

// CodeGraphFileRecord is one indexed file.
type CodeGraphFileRecord struct {
	Path        string
	Language    string
	Role        string
	ContentHash string
	SizeBytes   int64
	UpdatedAt   time.Time
}

// CodeGraphSymbolRecord is one declaration.
type CodeGraphSymbolRecord struct {
	SymbolID      string
	Path          string
	Name          string
	Kind          string
	Language      string
	Line          int64
	EndLine       int64
	Signature     string
	Doc           string
	Summary       string
	SummarySource string
	Exported      bool
	BodyHash      string
}

// CodeGraphEdgeRecord is one relation.
type CodeGraphEdgeRecord struct {
	EdgeID  string
	Path    string
	Kind    string
	FromKey string
	ToKey   string
	Line    int64
}

// CodeGraphEntry is everything known about one file: what is written and
// deleted as a unit.
type CodeGraphEntry struct {
	File    CodeGraphFileRecord
	Symbols []CodeGraphSymbolRecord
	Edges   []CodeGraphEdgeRecord
}

// CodeGraphDelta is what writing or removing one path changed, so a caller can
// report symbols and edges added and removed without counting the whole graph.
type CodeGraphDelta struct {
	SymbolsBefore int64
	SymbolsAfter  int64
	EdgesBefore   int64
	EdgesAfter    int64
	FileRemoved   bool
}

// CodeGraphCompletion carries everything a finished sync records.
type CodeGraphCompletion struct {
	ProjectID domain.ProjectID
	RepoID    string
	// Generation is the staging generation for a full build, or the served
	// generation for an incremental update.
	Generation       int64
	IndexedCommit    string
	RepoIdentity     string
	FileCount        int64
	SymbolCount      int64
	EdgeCount        int64
	SyncKind         CodeGraphSyncKind
	FilesParsed      int64
	FilesReused      int64
	FilesRemoved     int64
	SymbolsAdded     int64
	SymbolsRemoved   int64
	EdgesAdded       int64
	EdgesRemoved     int64
	Duration         time.Duration
	Architecture     string
	ArchitectureJSON string
}

// EnsureCodeGraphRepo registers a repository for code-graph indexing,
// idempotently, and refreshes the explanatory path and backend.
func (s *Store) EnsureCodeGraphRepo(ctx context.Context, projectID domain.ProjectID, repoID, repoPath, backend string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.qw.EnsureCodeGraphIndex(ctx, gen.EnsureCodeGraphIndexParams{
		ProjectID: string(projectID), RepoID: repoID, RepoPath: repoPath,
		Backend: backend, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("ensure code graph repo: %w", err)
	}
	if _, err := s.qw.UpdateCodeGraphRepoPath(ctx, gen.UpdateCodeGraphRepoPathParams{
		RepoPath: repoPath, Backend: backend, UpdatedAt: now,
		ProjectID: string(projectID), RepoID: repoID,
	}); err != nil {
		return fmt.Errorf("refresh code graph repo path: %w", err)
	}
	return nil
}

// GetCodeGraphState reads one repository's state.
func (s *Store) GetCodeGraphState(ctx context.Context, projectID domain.ProjectID, repoID string) (CodeGraphState, bool, error) {
	row, err := s.qr.GetCodeGraphIndex(ctx, gen.GetCodeGraphIndexParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CodeGraphState{}, false, nil
	}
	if err != nil {
		return CodeGraphState{}, false, fmt.Errorf("read code graph state: %w", err)
	}
	return codeGraphStateFromRow(row), true, nil
}

// ListCodeGraphStates reads every registered repository of one project.
func (s *Store) ListCodeGraphStates(ctx context.Context, projectID domain.ProjectID) ([]CodeGraphState, error) {
	rows, err := s.qr.ListCodeGraphIndexes(ctx, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("list code graph states: %w", err)
	}
	out := make([]CodeGraphState, 0, len(rows))
	for _, row := range rows {
		out = append(out, codeGraphStateFromRow(row))
	}
	return out, nil
}

// ClaimCodeGraphBuild allocates the next generation and claims a full build.
//
// It succeeds only from a terminal phase. Two dispatches that start at the same
// moment therefore resolve to exactly one builder, and the loser reads the
// state back to find out who won -- the same fence project_memory_index uses,
// for the same reason: two concurrent full rebuilds of one repository are pure
// waste, and the loser has nothing useful to do with a second answer.
func (s *Store) ClaimCodeGraphBuild(
	ctx context.Context, projectID domain.ProjectID, repoID, pendingCommit, branch string, now time.Time,
) (CodeGraphState, bool, error) {
	s.writeMu.Lock()
	rows, err := s.qw.ClaimCodeGraphBuild(ctx, gen.ClaimCodeGraphBuildParams{
		PendingCommit: pendingCommit, Branch: branch, StartedAt: sql.NullTime{Time: now, Valid: true}, UpdatedAt: now,
		ProjectID: string(projectID), RepoID: repoID,
	})
	s.writeMu.Unlock()
	if err != nil {
		return CodeGraphState{}, false, fmt.Errorf("claim code graph build: %w", err)
	}
	state, found, err := s.GetCodeGraphState(ctx, projectID, repoID)
	if err != nil || !found {
		return CodeGraphState{}, false, err
	}
	return state, rows == 1, nil
}

// ReclaimCodeGraphBuild takes over a build left in flight by a crash,
// conditional on the generation the caller read.
func (s *Store) ReclaimCodeGraphBuild(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
	pendingCommit, branch string, now time.Time,
) (CodeGraphState, bool, error) {
	s.writeMu.Lock()
	rows, err := s.qw.ReclaimCodeGraphBuild(ctx, gen.ReclaimCodeGraphBuildParams{
		PendingCommit: pendingCommit, Branch: branch, StartedAt: sql.NullTime{Time: now, Valid: true}, UpdatedAt: now,
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	})
	s.writeMu.Unlock()
	if err != nil {
		return CodeGraphState{}, false, fmt.Errorf("reclaim code graph build: %w", err)
	}
	state, found, err := s.GetCodeGraphState(ctx, projectID, repoID)
	if err != nil || !found {
		return CodeGraphState{}, false, err
	}
	return state, rows == 1, nil
}

// CompleteCodeGraphBuild publishes a staged build and collects the generation
// it replaces.
//
// The order is the whole of the restart-safety argument. The visibility flip
// and the prune of the previous generation happen in ONE transaction: a crash
// between them is impossible, so there is no state in which served_generation
// names rows that have been deleted. The prune is conditional on the flip
// having applied, so a superseded builder deletes nothing.
func (s *Store) CompleteCodeGraphBuild(ctx context.Context, c CodeGraphCompletion, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	applied := false
	err := s.inTx(ctx, "complete code graph build", func(q *gen.Queries) error {
		rows, err := q.CompleteCodeGraphBuild(ctx, gen.CompleteCodeGraphBuildParams{
			ServedGeneration:   c.Generation,
			IndexedCommit:      c.IndexedCommit,
			RepoIdentity:       c.RepoIdentity,
			FileCount:          c.FileCount,
			SymbolCount:        c.SymbolCount,
			EdgeCount:          c.EdgeCount,
			LastSyncKind:       string(c.SyncKind),
			LastFilesParsed:    c.FilesParsed,
			LastFilesReused:    c.FilesReused,
			LastFilesRemoved:   c.FilesRemoved,
			LastSymbolsAdded:   c.SymbolsAdded,
			LastSymbolsRemoved: c.SymbolsRemoved,
			LastEdgesAdded:     c.EdgesAdded,
			LastEdgesRemoved:   c.EdgesRemoved,
			LastDurationMs:     c.Duration.Milliseconds(),
			Architecture:       c.Architecture,
			ArchitectureJson:   c.ArchitectureJSON,
			CompletedAt:        sql.NullTime{Time: now, Valid: true},
			UpdatedAt:          now,
			ProjectID:          string(c.ProjectID),
			RepoID:             c.RepoID,
			Generation:         c.Generation,
		})
		if err != nil {
			return err
		}
		if rows != 1 {
			return nil
		}
		applied = true
		return pruneCodeGraphBelow(ctx, q, c.ProjectID, c.RepoID, c.Generation)
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// RecordCodeGraphIncremental records what an in-place update did. It does not
// move served_generation: the update wrote at the generation already served.
func (s *Store) RecordCodeGraphIncremental(ctx context.Context, c CodeGraphCompletion, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RecordCodeGraphIncremental(ctx, gen.RecordCodeGraphIncrementalParams{
		IndexedCommit:      c.IndexedCommit,
		RepoIdentity:       c.RepoIdentity,
		FileCount:          c.FileCount,
		SymbolCount:        c.SymbolCount,
		EdgeCount:          c.EdgeCount,
		LastSyncKind:       string(c.SyncKind),
		LastFilesParsed:    c.FilesParsed,
		LastFilesReused:    c.FilesReused,
		LastFilesRemoved:   c.FilesRemoved,
		LastSymbolsAdded:   c.SymbolsAdded,
		LastSymbolsRemoved: c.SymbolsRemoved,
		LastEdgesAdded:     c.EdgesAdded,
		LastEdgesRemoved:   c.EdgesRemoved,
		LastDurationMs:     c.Duration.Milliseconds(),
		Architecture:       c.Architecture,
		ArchitectureJson:   c.ArchitectureJSON,
		CompletedAt:        sql.NullTime{Time: now, Valid: true},
		UpdatedAt:          now,
		ProjectID:          string(c.ProjectID),
		RepoID:             c.RepoID,
		ServedGeneration:   c.Generation,
	})
	if err != nil {
		return false, fmt.Errorf("record code graph incremental: %w", err)
	}
	return rows == 1, nil
}

// FailCodeGraphBuild ends a build on an error, keeping the generation.
func (s *Store) FailCodeGraphBuild(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, reason string, now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.FailCodeGraphBuild(ctx, gen.FailCodeGraphBuildParams{
		LastError: truncateReason(reason), CompletedAt: sql.NullTime{Time: now, Valid: true}, UpdatedAt: now,
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	})
	if err != nil {
		return false, fmt.Errorf("fail code graph build: %w", err)
	}
	return rows == 1, nil
}

// PutCodeGraphEntry writes one file's row, symbols and edges at a generation,
// replacing whatever that path had there.
//
// Delete-then-insert rather than upsert-and-sweep, and in one transaction: a
// symbol that the new version of a file no longer declares must be GONE, not
// left behind at an older timestamp for a later pass to notice. That is the
// difference between a graph that answers "what is in this file" correctly and
// one that accumulates zombies.
func (s *Store) PutCodeGraphEntry(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
	entry CodeGraphEntry, now time.Time,
) (CodeGraphDelta, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var delta CodeGraphDelta
	err := s.inTx(ctx, "put code graph entry", func(q *gen.Queries) error {
		var err error
		if delta.SymbolsBefore, delta.EdgesBefore, err = countCodeGraphPath(ctx, q, projectID, repoID, generation, entry.File.Path); err != nil {
			return err
		}
		if _, err := q.DeleteCodeGraphSymbolsForPath(ctx, gen.DeleteCodeGraphSymbolsForPathParams{
			ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: entry.File.Path,
		}); err != nil {
			return err
		}
		if _, err := q.DeleteCodeGraphEdgesForPath(ctx, gen.DeleteCodeGraphEdgesForPathParams{
			ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: entry.File.Path,
		}); err != nil {
			return err
		}
		if err := q.PutCodeGraphFile(ctx, gen.PutCodeGraphFileParams{
			ProjectID: string(projectID), RepoID: repoID, Generation: generation,
			Path: entry.File.Path, Language: entry.File.Language, Role: entry.File.Role,
			ContentHash: entry.File.ContentHash, SizeBytes: entry.File.SizeBytes, UpdatedAt: now,
		}); err != nil {
			return err
		}
		for _, sym := range entry.Symbols {
			if err := q.PutCodeGraphSymbol(ctx, gen.PutCodeGraphSymbolParams{
				ProjectID: string(projectID), RepoID: repoID, Generation: generation,
				SymbolID: sym.SymbolID, Path: sym.Path, Name: sym.Name, Kind: sym.Kind,
				Language: sym.Language, Line: sym.Line, EndLine: sym.EndLine,
				Signature: sym.Signature, Doc: sym.Doc, Summary: sym.Summary,
				SummarySource: sym.SummarySource, Exported: boolToInt(sym.Exported),
				BodyHash: sym.BodyHash, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		for _, edge := range entry.Edges {
			if err := q.PutCodeGraphEdge(ctx, gen.PutCodeGraphEdgeParams{
				ProjectID: string(projectID), RepoID: repoID, Generation: generation,
				EdgeID: edge.EdgeID, Path: edge.Path, Kind: edge.Kind,
				FromKey: edge.FromKey, ToKey: edge.ToKey, Line: edge.Line, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		delta.SymbolsAfter = int64(len(entry.Symbols))
		delta.EdgesAfter = int64(len(entry.Edges))
		return nil
	})
	if err != nil {
		return CodeGraphDelta{}, err
	}
	return delta, nil
}

// DeleteCodeGraphPath removes one path and everything derived from it.
func (s *Store) DeleteCodeGraphPath(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string,
) (CodeGraphDelta, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var delta CodeGraphDelta
	err := s.inTx(ctx, "delete code graph path", func(q *gen.Queries) error {
		var err error
		if delta.SymbolsBefore, delta.EdgesBefore, err = countCodeGraphPath(ctx, q, projectID, repoID, generation, path); err != nil {
			return err
		}
		if _, err := q.DeleteCodeGraphSymbolsForPath(ctx, gen.DeleteCodeGraphSymbolsForPathParams{
			ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: path,
		}); err != nil {
			return err
		}
		if _, err := q.DeleteCodeGraphEdgesForPath(ctx, gen.DeleteCodeGraphEdgesForPathParams{
			ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: path,
		}); err != nil {
			return err
		}
		removed, err := q.DeleteCodeGraphFile(ctx, gen.DeleteCodeGraphFileParams{
			ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: path,
		})
		if err != nil {
			return err
		}
		delta.FileRemoved = removed > 0
		return nil
	})
	if err != nil {
		return CodeGraphDelta{}, err
	}
	return delta, nil
}

// CopyCodeGraphPathForward carries one unchanged path from the served
// generation into a staging one without re-reading or re-parsing it.
//
// This is what makes a full rebuild of a quiet repository cheap, and it is also
// what makes such a rebuild RESUMABLE: a restart finds the paths already
// carried forward present at the staging generation and skips them.
func (s *Store) CopyCodeGraphPathForward(
	ctx context.Context, projectID domain.ProjectID, repoID string, from, to int64, path string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	copied := false
	err := s.inTx(ctx, "copy code graph path forward", func(q *gen.Queries) error {
		rows, err := q.CopyCodeGraphFileForward(ctx, gen.CopyCodeGraphFileForwardParams{
			ToGeneration: to, ProjectID: string(projectID), RepoID: repoID,
			FromGeneration: from, Path: path,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return nil
		}
		copied = true
		if _, err := q.CopyCodeGraphSymbolsForward(ctx, gen.CopyCodeGraphSymbolsForwardParams{
			ToGeneration: to, ProjectID: string(projectID), RepoID: repoID,
			FromGeneration: from, Path: path,
		}); err != nil {
			return err
		}
		_, err = q.CopyCodeGraphEdgesForward(ctx, gen.CopyCodeGraphEdgesForwardParams{
			ToGeneration: to, ProjectID: string(projectID), RepoID: repoID,
			FromGeneration: from, Path: path,
		})
		return err
	})
	if err != nil {
		return false, err
	}
	return copied, nil
}

// DiscardCodeGraphStaging drops every row above the served generation. It is
// the collect half of the staging rule: an abandoned build's rows were never
// visible and never will be.
func (s *Store) DiscardCodeGraphStaging(ctx context.Context, projectID domain.ProjectID, repoID string, served int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "discard code graph staging", func(q *gen.Queries) error {
		for _, drop := range []func() (int64, error){
			func() (int64, error) {
				return q.PruneCodeGraphEdgesAboveGeneration(ctx, gen.PruneCodeGraphEdgesAboveGenerationParams{
					ProjectID: string(projectID), RepoID: repoID, Generation: served,
				})
			},
			func() (int64, error) {
				return q.PruneCodeGraphSymbolsAboveGeneration(ctx, gen.PruneCodeGraphSymbolsAboveGenerationParams{
					ProjectID: string(projectID), RepoID: repoID, Generation: served,
				})
			},
			func() (int64, error) {
				return q.PruneCodeGraphFilesAboveGeneration(ctx, gen.PruneCodeGraphFilesAboveGenerationParams{
					ProjectID: string(projectID), RepoID: repoID, Generation: served,
				})
			},
		} {
			if _, err := drop(); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetCodeGraphFileRecord reads one path's ledger entry at a generation.
func (s *Store) GetCodeGraphFileRecord(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string,
) (CodeGraphFileRecord, bool, error) {
	row, err := s.qr.GetCodeGraphFile(ctx, gen.GetCodeGraphFileParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: path,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CodeGraphFileRecord{}, false, nil
	}
	if err != nil {
		return CodeGraphFileRecord{}, false, fmt.Errorf("read code graph file: %w", err)
	}
	return CodeGraphFileRecord{
		Path: row.Path, Language: row.Language, Role: row.Role,
		ContentHash: row.ContentHash, SizeBytes: row.SizeBytes, UpdatedAt: row.UpdatedAt,
	}, true, nil
}

// ListCodeGraphFileRecords reads the whole file ledger at a generation.
func (s *Store) ListCodeGraphFileRecords(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
) ([]CodeGraphFileRecord, error) {
	rows, err := s.qr.ListCodeGraphFiles(ctx, gen.ListCodeGraphFilesParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph files: %w", err)
	}
	out := make([]CodeGraphFileRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, CodeGraphFileRecord{
			Path: row.Path, Language: row.Language, Role: row.Role,
			ContentHash: row.ContentHash, SizeBytes: row.SizeBytes, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// CountCodeGraph reports the row counts at a generation.
func (s *Store) CountCodeGraph(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
) (files, symbols, edges int64, err error) {
	row, err := s.qr.CountCodeGraphRows(ctx, gen.CountCodeGraphRowsParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
		ProjectID_2: string(projectID), RepoID_2: repoID, Generation_2: generation,
		ProjectID_3: string(projectID), RepoID_3: repoID, Generation_3: generation,
	})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count code graph: %w", err)
	}
	return row.FileCount, row.SymbolCount, row.EdgeCount, nil
}

// ListCodeGraphSymbolsForPath reads one file's declarations.
func (s *Store) ListCodeGraphSymbolsForPath(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string,
) ([]CodeGraphSymbolRecord, error) {
	rows, err := s.qr.ListCodeGraphSymbolsForPath(ctx, gen.ListCodeGraphSymbolsForPathParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: path,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph symbols for path: %w", err)
	}
	return codeGraphSymbolsFromRows(rows), nil
}

// ListCodeGraphSymbolsByName resolves a symbol name to its declarations. The
// answer may be several: a name is not unique across a repository, and
// pretending it is would be the resolution guess this package refuses to make.
func (s *Store) ListCodeGraphSymbolsByName(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, name string, limit int64,
) ([]CodeGraphSymbolRecord, error) {
	rows, err := s.qr.ListCodeGraphSymbolsByName(ctx, gen.ListCodeGraphSymbolsByNameParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Name: name, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph symbols by name: %w", err)
	}
	return codeGraphSymbolsFromRows(rows), nil
}

// SearchCodeGraphSymbols is the bounded candidate read behind retrieval. The
// term is matched literally and case-insensitively against name, path and
// summary; the caller supplies it already lowercased.
func (s *Store) SearchCodeGraphSymbols(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, term string, limit int64,
) ([]CodeGraphSymbolRecord, error) {
	rows, err := s.qr.SearchCodeGraphSymbols(ctx, gen.SearchCodeGraphSymbolsParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
		Term: strings.ToLower(term), Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("search code graph symbols: %w", err)
	}
	return codeGraphSymbolsFromRows(rows), nil
}

// ListCodeGraphEdgesFrom traverses outward from a node key.
func (s *Store) ListCodeGraphEdgesFrom(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, from string, limit int64,
) ([]CodeGraphEdgeRecord, error) {
	rows, err := s.qr.ListCodeGraphEdgesFrom(ctx, gen.ListCodeGraphEdgesFromParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, FromKey: from, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph edges from: %w", err)
	}
	return codeGraphEdgesFromRows(rows), nil
}

// ListCodeGraphEdgesTo traverses inward to a node key.
func (s *Store) ListCodeGraphEdgesTo(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, to string, limit int64,
) ([]CodeGraphEdgeRecord, error) {
	rows, err := s.qr.ListCodeGraphEdgesTo(ctx, gen.ListCodeGraphEdgesToParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, ToKey: to, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph edges to: %w", err)
	}
	return codeGraphEdgesFromRows(rows), nil
}

// PurgeCodeGraph deletes every graph row for one repository, keeping its
// registration so a later build can rebuild it.
func (s *Store) PurgeCodeGraph(ctx context.Context, projectID domain.ProjectID, repoID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "purge code graph", func(q *gen.Queries) error {
		params := gen.PurgeCodeGraphEdgesParams{ProjectID: string(projectID), RepoID: repoID}
		if _, err := q.PurgeCodeGraphEdges(ctx, params); err != nil {
			return err
		}
		if _, err := q.PurgeCodeGraphSymbols(ctx, gen.PurgeCodeGraphSymbolsParams(params)); err != nil {
			return err
		}
		_, err := q.PurgeCodeGraphFiles(ctx, gen.PurgeCodeGraphFilesParams(params))
		return err
	})
}

// DeregisterCodeGraphRepo additionally forgets the registration, for a
// "repository" that never was one -- see P2-E's worktree prune.
func (s *Store) DeregisterCodeGraphRepo(ctx context.Context, projectID domain.ProjectID, repoID string) error {
	if err := s.PurgeCodeGraph(ctx, projectID, repoID); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.qw.DeleteCodeGraphIndex(ctx, gen.DeleteCodeGraphIndexParams{
		ProjectID: string(projectID), RepoID: repoID,
	}); err != nil {
		return fmt.Errorf("deregister code graph repo: %w", err)
	}
	return nil
}

// pruneCodeGraphBelow collects every generation older than the one just
// published. Edges and symbols go before files so an interrupted prune can
// never leave an edge whose file row is gone.
func pruneCodeGraphBelow(ctx context.Context, q *gen.Queries, projectID domain.ProjectID, repoID string, generation int64) error {
	if _, err := q.PruneCodeGraphEdgesBelowGeneration(ctx, gen.PruneCodeGraphEdgesBelowGenerationParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	}); err != nil {
		return err
	}
	if _, err := q.PruneCodeGraphSymbolsBelowGeneration(ctx, gen.PruneCodeGraphSymbolsBelowGenerationParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	}); err != nil {
		return err
	}
	_, err := q.PruneCodeGraphFilesBelowGeneration(ctx, gen.PruneCodeGraphFilesBelowGenerationParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	})
	return err
}

func countCodeGraphPath(
	ctx context.Context, q *gen.Queries, projectID domain.ProjectID, repoID string, generation int64, path string,
) (symbols, edges int64, err error) {
	symbols, err = q.CountCodeGraphSymbolsForPath(ctx, gen.CountCodeGraphSymbolsForPathParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: path,
	})
	if err != nil {
		return 0, 0, err
	}
	edges, err = q.CountCodeGraphEdgesForPath(ctx, gen.CountCodeGraphEdgesForPathParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: path,
	})
	if err != nil {
		return 0, 0, err
	}
	return symbols, edges, nil
}

func codeGraphStateFromRow(row gen.CodeGraphIndex) CodeGraphState {
	return CodeGraphState{
		ProjectID:          domain.ProjectID(row.ProjectID),
		RepoID:             row.RepoID,
		RepoPath:           row.RepoPath,
		Backend:            row.Backend,
		Generation:         row.Generation,
		ServedGeneration:   row.ServedGeneration,
		Phase:              CodeGraphPhase(row.Phase),
		IndexedCommit:      row.IndexedCommit,
		PendingCommit:      row.PendingCommit,
		Branch:             row.Branch,
		RepoIdentity:       row.RepoIdentity,
		FileCount:          row.FileCount,
		SymbolCount:        row.SymbolCount,
		EdgeCount:          row.EdgeCount,
		LastSyncKind:       CodeGraphSyncKind(row.LastSyncKind),
		LastFilesParsed:    row.LastFilesParsed,
		LastFilesReused:    row.LastFilesReused,
		LastFilesRemoved:   row.LastFilesRemoved,
		LastSymbolsAdded:   row.LastSymbolsAdded,
		LastSymbolsRemoved: row.LastSymbolsRemoved,
		LastEdgesAdded:     row.LastEdgesAdded,
		LastEdgesRemoved:   row.LastEdgesRemoved,
		LastDuration:       time.Duration(row.LastDurationMs) * time.Millisecond,
		LastError:          row.LastError,
		Architecture:       row.Architecture,
		ArchitectureJSON:   row.ArchitectureJson,
		StartedAt:          row.StartedAt,
		CompletedAt:        row.CompletedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

func codeGraphSymbolsFromRows(rows []gen.CodeGraphSymbol) []CodeGraphSymbolRecord {
	out := make([]CodeGraphSymbolRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, CodeGraphSymbolRecord{
			SymbolID: row.SymbolID, Path: row.Path, Name: row.Name, Kind: row.Kind,
			Language: row.Language, Line: row.Line, EndLine: row.EndLine,
			Signature: row.Signature, Doc: row.Doc, Summary: row.Summary,
			SummarySource: row.SummarySource, Exported: row.Exported != 0, BodyHash: row.BodyHash,
		})
	}
	return out
}

func codeGraphEdgesFromRows(rows []gen.CodeGraphEdge) []CodeGraphEdgeRecord {
	out := make([]CodeGraphEdgeRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, CodeGraphEdgeRecord{
			EdgeID: row.EdgeID, Path: row.Path, Kind: row.Kind,
			FromKey: row.FromKey, ToKey: row.ToKey, Line: row.Line,
		})
	}
	return out
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// maxCodeGraphError bounds a stored failure reason so one pathological error
// cannot make the status row expensive to read.
const maxCodeGraphError = 500

func truncateReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) <= maxCodeGraphError {
		return reason
	}
	return reason[:maxCodeGraphError-3] + "..."
}

// LoadCodeGraph reads a whole generation back into the in-memory shape the
// architecture summary is computed from.
//
// It is deliberately the one unbounded read in this file, and it is safe
// because of WHERE it runs: once per completed build that actually changed
// something, never on a dispatch. The alternative -- recomputing a census from
// aggregate queries -- would need the import edges anyway, so it would read the
// same rows through a worse interface.
func (s *Store) LoadCodeGraph(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
) ([]CodeGraphFileRecord, []CodeGraphSymbolRecord, []CodeGraphEdgeRecord, error) {
	files, err := s.ListCodeGraphFileRecords(ctx, projectID, repoID, generation)
	if err != nil {
		return nil, nil, nil, err
	}
	symbolRows, err := s.qr.ListCodeGraphSymbols(ctx, gen.ListCodeGraphSymbolsParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load code graph symbols: %w", err)
	}
	edgeRows, err := s.qr.ListCodeGraphEdges(ctx, gen.ListCodeGraphEdgesParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load code graph edges: %w", err)
	}
	return files, codeGraphSymbolsFromRows(symbolRows), codeGraphEdgesFromRows(edgeRows), nil
}

// ListCodeGraphEdgesFromKeys is the batched traversal outward. Retrieval uses
// it in place of one query per symbol: a bounded neighbourhood is one round
// trip, whatever its size.
func (s *Store) ListCodeGraphEdgesFromKeys(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, keys []string,
) ([]CodeGraphEdgeRecord, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := s.qr.ListCodeGraphEdgesFromKeys(ctx, gen.ListCodeGraphEdgesFromKeysParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Keys: keys,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph edges from keys: %w", err)
	}
	return codeGraphEdgesFromRows(rows), nil
}

// ListCodeGraphEdgesToKeys is the batched traversal inward.
func (s *Store) ListCodeGraphEdgesToKeys(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, keys []string,
) ([]CodeGraphEdgeRecord, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := s.qr.ListCodeGraphEdgesToKeys(ctx, gen.ListCodeGraphEdgesToKeysParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Keys: keys,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph edges to keys: %w", err)
	}
	return codeGraphEdgesFromRows(rows), nil
}

// ListCodeGraphSymbolsForPaths reads several files' declarations at once, so
// resolving a set of edges back to their declarations costs one statement.
func (s *Store) ListCodeGraphSymbolsForPaths(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, paths []string,
) ([]CodeGraphSymbolRecord, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	rows, err := s.qr.ListCodeGraphSymbolsForPaths(ctx, gen.ListCodeGraphSymbolsForPathsParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Paths: paths,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph symbols for paths: %w", err)
	}
	return codeGraphSymbolsFromRows(rows), nil
}

// ListCodeGraphEdgesForPath reads one file's relations. An incremental update
// uses it to decide whether a change could have moved the ARCHITECTURE, which
// is a different and much cheaper question than recomputing the architecture.
func (s *Store) ListCodeGraphEdgesForPath(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string,
) ([]CodeGraphEdgeRecord, error) {
	rows, err := s.qr.ListCodeGraphEdgesForPath(ctx, gen.ListCodeGraphEdgesForPathParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation, Path: path,
	})
	if err != nil {
		return nil, fmt.Errorf("list code graph edges for path: %w", err)
	}
	return codeGraphEdgesFromRows(rows), nil
}
