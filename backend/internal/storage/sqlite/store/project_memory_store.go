package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// project_memory_store.go — the durable side of P2-A project memory
// (migration 0144).
//
// Two rules govern every write here, and they are the reason this store exists
// rather than a JSON file:
//
//   - Generation-conditioned CAS. No fact is written unconditionally. A writer
//     names the generation it believes is current, and a write whose generation
//     is behind the stored row's is refused rather than applied. That is what
//     stops an indexing pass that stalled for a minute from overwriting the
//     memory a newer pass already produced.
//   - One transaction per fact. An item, the rows recording which paths it was
//     derived from, and the ledger entry for the file it came from are written
//     together. A partial write cannot leave a fact whose provenance says it
//     came from nowhere, which would then be un-invalidatable.
//
// The store deliberately marks rather than deletes. Ordinary invalidation
// moves an item to stale/invalidated and keeps it: re-deriving a fact from a
// known previous answer is cheaper than from nothing, and "this went stale at
// commit X" is itself evidence an operator can read.

// ErrProjectMemoryStaleGeneration is returned when a write is refused because
// the stored row is already at a newer generation. It is a sentinel rather
// than a bool so a caller that forgets to check cannot silently treat a
// refused write as an applied one.
var ErrProjectMemoryStaleGeneration = errors.New("store: project memory write refused, stored generation is newer")

// ProjectMemoryWriteOutcome says what a write actually did.
type ProjectMemoryWriteOutcome string

// Project memory write outcomes.
const (
	// MemoryWriteCreated is a fact that did not exist before.
	MemoryWriteCreated ProjectMemoryWriteOutcome = "created"
	// MemoryWriteUpdated is a fact whose content or provenance moved.
	MemoryWriteUpdated ProjectMemoryWriteOutcome = "updated"
	// MemoryWriteReconfirmed is a fact whose content did NOT move: only its
	// provenance and generation were refreshed, and updated_at was left alone
	// so freshness ranking still reflects when the fact last really changed.
	MemoryWriteReconfirmed ProjectMemoryWriteOutcome = "reconfirmed"
	// MemoryWriteRefused is a write the CAS rejected. The stored row is
	// newer; the caller's pass has been superseded.
	MemoryWriteRefused ProjectMemoryWriteOutcome = "refused"
)

// EnsureProjectMemoryRepo registers a repository for indexing. It is
// idempotent: a repeated registration inserts nothing and never disturbs a
// pass in flight. When the repository has moved on disk, the explanatory
// repo_path is refreshed while the hashed identity stays put — the identity is
// what the caller resolved the row by.
func (s *Store) EnsureProjectMemoryRepo(ctx context.Context, projectID domain.ProjectID, repoID, repoPath string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.qw.EnsureProjectMemoryIndex(ctx, gen.EnsureProjectMemoryIndexParams{
		ProjectID: string(projectID), RepoID: repoID, RepoPath: repoPath, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("ensure project memory repo: %w", err)
	}
	if _, err := s.qw.UpdateProjectMemoryIndexRepoPath(ctx, gen.UpdateProjectMemoryIndexRepoPathParams{
		RepoPath: repoPath, UpdatedAt: now, ProjectID: string(projectID), RepoID: repoID,
	}); err != nil {
		return fmt.Errorf("refresh project memory repo path: %w", err)
	}
	return nil
}

// GetProjectMemoryIndexState reads one repository's indexing state.
func (s *Store) GetProjectMemoryIndexState(ctx context.Context, projectID domain.ProjectID, repoID string) (domain.ProjectMemoryIndexState, bool, error) {
	row, err := s.qr.GetProjectMemoryIndex(ctx, gen.GetProjectMemoryIndexParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProjectMemoryIndexState{}, false, nil
	}
	if err != nil {
		return domain.ProjectMemoryIndexState{}, false, fmt.Errorf("get project memory index: %w", err)
	}
	return projectMemoryIndexFromRow(row), true, nil
}

// ListProjectMemoryIndexStates reads every repository registered under one
// project, in repository order.
func (s *Store) ListProjectMemoryIndexStates(ctx context.Context, projectID domain.ProjectID) ([]domain.ProjectMemoryIndexState, error) {
	rows, err := s.qr.ListProjectMemoryIndexes(ctx, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("list project memory indexes: %w", err)
	}
	out := make([]domain.ProjectMemoryIndexState, 0, len(rows))
	for _, r := range rows {
		out = append(out, projectMemoryIndexFromRow(r))
	}
	return out, nil
}

// ClaimProjectMemoryIndexPass takes the next generation and claims the pass.
//
// It succeeds only while the repository is in a terminal phase, so two callers
// racing to index the same repository resolve to exactly one winner and the
// loser reads false as "somebody else is indexing" rather than as an error.
// Resuming a pass that is still marked in flight is a different, deliberate
// operation — see ResumeProjectMemoryIndexPass.
func (s *Store) ClaimProjectMemoryIndexPass(
	ctx context.Context, projectID domain.ProjectID, repoID, pendingCommit, branch string, now time.Time,
) (domain.ProjectMemoryIndexState, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ClaimProjectMemoryIndexPass(ctx, gen.ClaimProjectMemoryIndexPassParams{
		Phase:         string(domain.IndexPhaseScanning),
		PendingCommit: pendingCommit,
		Branch:        branch,
		StartedAt:     sql.NullTime{Time: now, Valid: true},
		UpdatedAt:     now,
		ProjectID:     string(projectID),
		RepoID:        repoID,
	})
	if err != nil {
		return domain.ProjectMemoryIndexState{}, false, fmt.Errorf("claim project memory pass: %w", err)
	}
	if n == 0 {
		return domain.ProjectMemoryIndexState{}, false, nil
	}
	row, err := s.qw.GetProjectMemoryIndex(ctx, gen.GetProjectMemoryIndexParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return domain.ProjectMemoryIndexState{}, false, fmt.Errorf("read claimed project memory pass: %w", err)
	}
	return projectMemoryIndexFromRow(row), true, nil
}

// ResumeProjectMemoryIndexPass takes over a pass that is still marked in
// flight — the state a crash leaves behind.
//
// It is conditional on the generation the caller read, so two restarts racing
// to recover the same abandoned pass cannot both proceed. The resume cursor
// and the counters are deliberately not reset: continuing from where the dead
// pass stopped is the entire point.
func (s *Store) ResumeProjectMemoryIndexPass(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
	phase domain.ProjectMemoryIndexPhase, pendingCommit, branch string, now time.Time,
) (domain.ProjectMemoryIndexState, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ReclaimProjectMemoryIndexPass(ctx, gen.ReclaimProjectMemoryIndexPassParams{
		Phase:         string(phase),
		PendingCommit: pendingCommit,
		Branch:        branch,
		UpdatedAt:     now,
		ProjectID:     string(projectID),
		RepoID:        repoID,
		Generation:    generation,
	})
	if err != nil {
		return domain.ProjectMemoryIndexState{}, false, fmt.Errorf("resume project memory pass: %w", err)
	}
	if n == 0 {
		return domain.ProjectMemoryIndexState{}, false, nil
	}
	row, err := s.qw.GetProjectMemoryIndex(ctx, gen.GetProjectMemoryIndexParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return domain.ProjectMemoryIndexState{}, false, fmt.Errorf("read resumed project memory pass: %w", err)
	}
	return projectMemoryIndexFromRow(row), true, nil
}

// AdvanceProjectMemoryIndexPass records progress: the phase reached, the
// resume cursor, and the counters. A false result means the pass has been
// superseded and the caller should stop.
func (s *Store) AdvanceProjectMemoryIndexPass(ctx context.Context, st domain.ProjectMemoryIndexState, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.AdvanceProjectMemoryIndexPass(ctx, gen.AdvanceProjectMemoryIndexPassParams{
		Phase:            string(st.Phase),
		ResumeCursor:     st.Cursor,
		FilesSeen:        int64(st.FilesSeen),
		FilesIndexed:     int64(st.FilesIndexed),
		FilesSkipped:     int64(st.FilesSkipped),
		ItemsWritten:     int64(st.ItemsWritten),
		RelationsWritten: int64(st.RelationsWritten),
		UpdatedAt:        now,
		ProjectID:        string(st.ProjectID),
		RepoID:           st.RepoID,
		Generation:       st.Generation,
	})
	if err != nil {
		return false, fmt.Errorf("advance project memory pass: %w", err)
	}
	return n > 0, nil
}

// CompleteProjectMemoryIndexPass promotes the pending commit to the indexed
// commit and returns the repository to idle.
//
// This is the only place indexed_commit advances. A pass that died leaves it
// where it was, so the changes that pass never reached stay visible to the
// next one instead of being skipped forever.
func (s *Store) CompleteProjectMemoryIndexPass(ctx context.Context, st domain.ProjectMemoryIndexState, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CompleteProjectMemoryIndexPass(ctx, gen.CompleteProjectMemoryIndexPassParams{
		FilesSeen:        int64(st.FilesSeen),
		FilesIndexed:     int64(st.FilesIndexed),
		FilesSkipped:     int64(st.FilesSkipped),
		ItemsWritten:     int64(st.ItemsWritten),
		RelationsWritten: int64(st.RelationsWritten),
		CompletedAt:      sql.NullTime{Time: now, Valid: true},
		UpdatedAt:        now,
		ProjectID:        string(st.ProjectID),
		RepoID:           st.RepoID,
		Generation:       st.Generation,
	})
	if err != nil {
		return false, fmt.Errorf("complete project memory pass: %w", err)
	}
	return n > 0, nil
}

// FailProjectMemoryIndexPass ends a pass on an error. The generation is kept,
// so the failure stays diagnosable and a stale writer still cannot pass the
// CAS afterwards.
func (s *Store) FailProjectMemoryIndexPass(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, reason string, now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.FailProjectMemoryIndexPass(ctx, gen.FailProjectMemoryIndexPassParams{
		LastError:   reason,
		CompletedAt: sql.NullTime{Time: now, Valid: true},
		UpdatedAt:   now,
		ProjectID:   string(projectID),
		RepoID:      repoID,
		Generation:  generation,
	})
	if err != nil {
		return false, fmt.Errorf("fail project memory pass: %w", err)
	}
	return n > 0, nil
}

// PutProjectMemoryItem writes one fact under generation-conditioned CAS.
//
// The three outcomes are distinct on purpose. A brand-new fact is created; a
// fact whose content moved is updated; a fact whose content did NOT move is
// *reconfirmed* — its provenance and generation are refreshed while its
// updated_at is left alone. That last case is what makes re-indexing an
// unchanged repository nearly free, and it is what preserves the "this fact
// has not really changed since March" signal a pack's freshness ranking uses.
//
// A write whose generation is behind the stored row's returns
// MemoryWriteRefused with ErrProjectMemoryStaleGeneration.
func (s *Store) PutProjectMemoryItem(ctx context.Context, item domain.ProjectMemoryItem, now time.Time) (ProjectMemoryWriteOutcome, error) {
	item = item.Normalized()
	if err := item.Validate(); err != nil {
		return MemoryWriteRefused, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	outcome := MemoryWriteRefused
	err := s.inTx(ctx, "put project memory item", func(q *gen.Queries) error {
		existing, err := q.GetProjectMemoryItem(ctx, item.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return s.insertProjectMemoryItem(ctx, q, item, now, &outcome)
		case err != nil:
			return fmt.Errorf("read existing item: %w", err)
		}
		if existing.Generation > item.Generation {
			outcome = MemoryWriteRefused
			return ErrProjectMemoryStaleGeneration
		}
		stored, err := projectMemoryItemFromRow(existing)
		if err != nil {
			// A row this build cannot decode is not evidence of anything, and
			// refusing to overwrite it would wedge the indexer against its own
			// corruption. Treat it as an update: the incoming fact is the one
			// that can be shown to hold.
			stored = domain.ProjectMemoryItem{}
		}
		if stored.SameFactAs(item) && stored.State == item.State {
			return s.reconfirmProjectMemoryItem(ctx, q, item, &outcome)
		}
		return s.updateProjectMemoryItem(ctx, q, item, now, &outcome)
	})
	if err != nil {
		if errors.Is(err, ErrProjectMemoryStaleGeneration) {
			return MemoryWriteRefused, ErrProjectMemoryStaleGeneration
		}
		return MemoryWriteRefused, err
	}
	return outcome, nil
}

func (s *Store) insertProjectMemoryItem(
	ctx context.Context, q *gen.Queries, item domain.ProjectMemoryItem, now time.Time, outcome *ProjectMemoryWriteOutcome,
) error {
	created, updated := item.CreatedAt, item.UpdatedAt
	if created.IsZero() {
		created = now
	}
	if updated.IsZero() {
		updated = now
	}
	paths, meta, err := marshalProjectMemoryPayload(item.SourcePaths, item.Metadata)
	if err != nil {
		return err
	}
	n, err := q.InsertProjectMemoryItem(ctx, gen.InsertProjectMemoryItemParams{
		ID: item.ID, ProjectID: string(item.Key.ProjectID), RepoID: item.Key.RepoID,
		ItemType: string(item.Key.Type), Scope: string(item.Key.Scope), ItemKey: item.Key.Key,
		Origin: string(item.Origin), OriginRef: item.OriginRef,
		Summary: item.Summary, Content: item.Content,
		SourcePathsJson: paths, SourceCommit: item.SourceCommit, SourceDigest: item.SourceDigest,
		Generation: item.Generation, State: string(item.State), StateReason: item.StateReason,
		Confidence: item.Confidence, MetadataJson: meta, ContentHash: item.ContentHash,
		CreatedAt: created, UpdatedAt: updated,
		InvalidatedAt: nullTimeOrZero(item.InvalidatedAt),
	})
	if err != nil {
		return fmt.Errorf("insert item: %w", err)
	}
	if n == 0 {
		// Lost an insert race inside the same transaction window. The row now
		// exists; the generation-conditioned update is the correct path.
		return s.updateProjectMemoryItem(ctx, q, item, now, outcome)
	}
	if err := s.writeProjectMemorySources(ctx, q, "item", item.ID, item.Key.ProjectID, item.Key.RepoID, item.SourcePaths); err != nil {
		return err
	}
	*outcome = MemoryWriteCreated
	return nil
}

func (s *Store) updateProjectMemoryItem(
	ctx context.Context, q *gen.Queries, item domain.ProjectMemoryItem, now time.Time, outcome *ProjectMemoryWriteOutcome,
) error {
	updated := item.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	paths, meta, err := marshalProjectMemoryPayload(item.SourcePaths, item.Metadata)
	if err != nil {
		return err
	}
	n, err := q.UpdateProjectMemoryItem(ctx, gen.UpdateProjectMemoryItemParams{
		ItemType: string(item.Key.Type), Scope: string(item.Key.Scope), ItemKey: item.Key.Key,
		Summary: item.Summary, Content: item.Content,
		SourcePathsJson: paths, SourceCommit: item.SourceCommit, SourceDigest: item.SourceDigest,
		Generation: item.Generation, State: string(item.State), StateReason: item.StateReason,
		Confidence: item.Confidence, MetadataJson: meta, ContentHash: item.ContentHash,
		UpdatedAt: updated, InvalidatedAt: nullTimeOrZero(item.InvalidatedAt),
		ID: item.ID, Generation_2: item.Generation,
	})
	if err != nil {
		return fmt.Errorf("update item: %w", err)
	}
	if n == 0 {
		*outcome = MemoryWriteRefused
		return ErrProjectMemoryStaleGeneration
	}
	if err := s.writeProjectMemorySources(ctx, q, "item", item.ID, item.Key.ProjectID, item.Key.RepoID, item.SourcePaths); err != nil {
		return err
	}
	*outcome = MemoryWriteUpdated
	return nil
}

func (s *Store) reconfirmProjectMemoryItem(
	ctx context.Context, q *gen.Queries, item domain.ProjectMemoryItem, outcome *ProjectMemoryWriteOutcome,
) error {
	paths, err := json.Marshal(item.SourcePaths)
	if err != nil {
		return fmt.Errorf("encode source paths: %w", err)
	}
	n, err := q.TouchProjectMemoryItemProvenance(ctx, gen.TouchProjectMemoryItemProvenanceParams{
		SourceCommit: item.SourceCommit, SourceDigest: item.SourceDigest,
		SourcePathsJson: string(paths), Generation: item.Generation,
		ID: item.ID, Generation_2: item.Generation,
	})
	if err != nil {
		return fmt.Errorf("reconfirm item: %w", err)
	}
	if n == 0 {
		*outcome = MemoryWriteRefused
		return ErrProjectMemoryStaleGeneration
	}
	if err := s.writeProjectMemorySources(ctx, q, "item", item.ID, item.Key.ProjectID, item.Key.RepoID, item.SourcePaths); err != nil {
		return err
	}
	*outcome = MemoryWriteReconfirmed
	return nil
}

// writeProjectMemorySources rewrites the reverse provenance index for one
// owner. It deletes and re-inserts rather than diffing: the list is tiny
// (capped at MaxProjectMemorySourcePaths), it runs inside the owner's own
// transaction, and a diff would be a second place for the two representations
// to disagree.
func (s *Store) writeProjectMemorySources(
	ctx context.Context, q *gen.Queries, ownerKind, ownerID string,
	projectID domain.ProjectID, repoID string, paths []string,
) error {
	if err := q.DeleteProjectMemorySourcesForOwner(ctx, gen.DeleteProjectMemorySourcesForOwnerParams{
		OwnerKind: ownerKind, OwnerID: ownerID,
	}); err != nil {
		return fmt.Errorf("clear %s sources: %w", ownerKind, err)
	}
	for _, p := range paths {
		if err := q.InsertProjectMemorySource(ctx, gen.InsertProjectMemorySourceParams{
			OwnerKind: ownerKind, OwnerID: ownerID,
			ProjectID: string(projectID), RepoID: repoID, Path: p,
		}); err != nil {
			return fmt.Errorf("record %s source %q: %w", ownerKind, p, err)
		}
	}
	return nil
}

// PutProjectMemoryRelation writes one edge under the same CAS discipline as an
// item. Edges have no "reconfirmed" case: an edge carries no body, so there is
// nothing whose freshness a re-confirmation would need to preserve.
func (s *Store) PutProjectMemoryRelation(ctx context.Context, rel domain.ProjectMemoryRelation, now time.Time) (ProjectMemoryWriteOutcome, error) {
	rel = rel.Normalized()
	if err := rel.Validate(); err != nil {
		return MemoryWriteRefused, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	outcome := MemoryWriteRefused
	err := s.inTx(ctx, "put project memory relation", func(q *gen.Queries) error {
		created, updated := rel.CreatedAt, rel.UpdatedAt
		if created.IsZero() {
			created = now
		}
		if updated.IsZero() {
			updated = now
		}
		paths, meta, err := marshalProjectMemoryPayload(rel.SourcePaths, rel.Metadata)
		if err != nil {
			return err
		}
		n, err := q.InsertProjectMemoryRelation(ctx, gen.InsertProjectMemoryRelationParams{
			ID: rel.ID, ProjectID: string(rel.ProjectID), RepoID: rel.RepoID,
			FromKind: string(rel.From.Kind), FromKey: rel.From.Key,
			RelationKind: string(rel.Kind),
			ToKind:       string(rel.To.Kind), ToKey: rel.To.Key,
			Origin: string(rel.Origin), OriginRef: rel.OriginRef,
			SourcePathsJson: paths, SourceCommit: rel.SourceCommit, SourceDigest: rel.SourceDigest,
			Generation: rel.Generation, State: string(rel.State), StateReason: rel.StateReason,
			Confidence: rel.Confidence, MetadataJson: meta,
			CreatedAt: created, UpdatedAt: updated,
			InvalidatedAt: nullTimeOrZero(rel.InvalidatedAt),
		})
		if err != nil {
			return fmt.Errorf("insert relation: %w", err)
		}
		if n > 0 {
			outcome = MemoryWriteCreated
			return s.writeProjectMemorySources(ctx, q, "relation", rel.ID, rel.ProjectID, rel.RepoID, rel.SourcePaths)
		}
		m, err := q.UpdateProjectMemoryRelation(ctx, gen.UpdateProjectMemoryRelationParams{
			SourcePathsJson: paths, SourceCommit: rel.SourceCommit, SourceDigest: rel.SourceDigest,
			Generation: rel.Generation, State: string(rel.State), StateReason: rel.StateReason,
			Confidence: rel.Confidence, MetadataJson: meta, UpdatedAt: updated,
			InvalidatedAt: nullTimeOrZero(rel.InvalidatedAt),
			ID:            rel.ID, Generation_2: rel.Generation,
		})
		if err != nil {
			return fmt.Errorf("update relation: %w", err)
		}
		if m == 0 {
			return ErrProjectMemoryStaleGeneration
		}
		outcome = MemoryWriteUpdated
		return s.writeProjectMemorySources(ctx, q, "relation", rel.ID, rel.ProjectID, rel.RepoID, rel.SourcePaths)
	})
	if err != nil {
		if errors.Is(err, ErrProjectMemoryStaleGeneration) {
			return MemoryWriteRefused, ErrProjectMemoryStaleGeneration
		}
		return MemoryWriteRefused, err
	}
	return outcome, nil
}

// GetProjectMemoryItem reads one fact by its derived identity.
func (s *Store) GetProjectMemoryItem(ctx context.Context, id string) (domain.ProjectMemoryItem, bool, error) {
	row, err := s.qr.GetProjectMemoryItem(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProjectMemoryItem{}, false, nil
	}
	if err != nil {
		return domain.ProjectMemoryItem{}, false, fmt.Errorf("get project memory item: %w", err)
	}
	item, err := projectMemoryItemFromRow(row)
	if err != nil {
		return domain.ProjectMemoryItem{}, false, err
	}
	return item, true, nil
}

// ListProjectMemoryItems reads every fact for one repository, in the
// deterministic selection order the query defines.
func (s *Store) ListProjectMemoryItems(ctx context.Context, projectID domain.ProjectID, repoID string) ([]domain.ProjectMemoryItem, error) {
	rows, err := s.qr.ListProjectMemoryItems(ctx, gen.ListProjectMemoryItemsParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return nil, fmt.Errorf("list project memory items: %w", err)
	}
	return projectMemoryItemsFromRows(rows)
}

// ListProjectMemoryItemsByState reads the facts of one repository in one
// state — the read behind `ao memory inspect --state stale`.
func (s *Store) ListProjectMemoryItemsByState(
	ctx context.Context, projectID domain.ProjectID, repoID string, state domain.ProjectMemoryState,
) ([]domain.ProjectMemoryItem, error) {
	rows, err := s.qr.ListProjectMemoryItemsByState(ctx, gen.ListProjectMemoryItemsByStateParams{
		ProjectID: string(projectID), RepoID: repoID, State: string(state),
	})
	if err != nil {
		return nil, fmt.Errorf("list project memory items by state: %w", err)
	}
	return projectMemoryItemsFromRows(rows)
}

// ListProjectMemoryItemsForProject reads every fact across every repository of
// one project. It is the multi-repo read: a Planner pack spans repositories,
// a Worker pack does not.
func (s *Store) ListProjectMemoryItemsForProject(ctx context.Context, projectID domain.ProjectID) ([]domain.ProjectMemoryItem, error) {
	rows, err := s.qr.ListProjectMemoryItemsForProject(ctx, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("list project memory items for project: %w", err)
	}
	return projectMemoryItemsFromRows(rows)
}

// ListProjectMemoryItemsForTask reads one task's own unintegrated facts. They
// are visible only to that task, which is what stops an isolated worktree from
// leaking its unmerged opinion into another task's premise.
func (s *Store) ListProjectMemoryItemsForTask(ctx context.Context, projectID domain.ProjectID, taskRef string) ([]domain.ProjectMemoryItem, error) {
	rows, err := s.qr.ListProjectMemoryItemsByOriginRef(ctx, gen.ListProjectMemoryItemsByOriginRefParams{
		ProjectID: string(projectID), Origin: string(domain.OriginTaskLocal), OriginRef: taskRef,
	})
	if err != nil {
		return nil, fmt.Errorf("list task-local project memory items: %w", err)
	}
	return projectMemoryItemsFromRows(rows)
}

// ListProjectMemoryItemsByPath reads every fact derived from one path — the
// reverse provenance lookup incremental update is built on.
func (s *Store) ListProjectMemoryItemsByPath(ctx context.Context, projectID domain.ProjectID, repoID, path string) ([]domain.ProjectMemoryItem, error) {
	rows, err := s.qr.ListProjectMemoryItemsByPath(ctx, gen.ListProjectMemoryItemsByPathParams{
		ProjectID: string(projectID), RepoID: repoID, Path: path,
	})
	if err != nil {
		return nil, fmt.Errorf("list project memory items by path: %w", err)
	}
	return projectMemoryItemsFromRows(rows)
}

// MarkProjectMemoryItemState moves one fact out of (or back into) validity,
// under the same generation fence as every other write.
func (s *Store) MarkProjectMemoryItemState(
	ctx context.Context, id string, generation int64,
	state domain.ProjectMemoryState, reason string, now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	invalidated := sql.NullTime{}
	if state != domain.MemoryStateValid {
		invalidated = sql.NullTime{Time: now, Valid: true}
	}
	n, err := s.qw.MarkProjectMemoryItemState(ctx, gen.MarkProjectMemoryItemStateParams{
		State: string(state), StateReason: reason, InvalidatedAt: invalidated,
		UpdatedAt: now, ID: id, Generation: generation,
	})
	if err != nil {
		return false, fmt.Errorf("mark project memory item state: %w", err)
	}
	return n > 0, nil
}

// InvalidateProjectMemoryByPath is the incremental-update workhorse: every
// currently-valid fact and edge derived from one changed path stops being
// authoritative, in two indexed statements.
//
// It marks rather than deletes, and it takes no generation: a source that
// moved on disk disproves what was derived from it regardless of which pass
// wrote it. Refusing to invalidate a newer pass's fact would be exactly
// backwards — the newer the fact, the more likely it is the one the change
// affects.
func (s *Store) InvalidateProjectMemoryByPath(
	ctx context.Context, projectID domain.ProjectID, repoID, path string,
	state domain.ProjectMemoryState, reason string, now time.Time,
) (items, relations int64, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	at := sql.NullTime{Time: now, Valid: true}
	err = s.inTx(ctx, "invalidate project memory by path", func(q *gen.Queries) error {
		n, err := q.MarkProjectMemoryItemsStaleByPath(ctx, gen.MarkProjectMemoryItemsStaleByPathParams{
			State: string(state), StateReason: reason, InvalidatedAt: at, UpdatedAt: now,
			ProjectID: string(projectID), RepoID: repoID, Path: path,
		})
		if err != nil {
			return fmt.Errorf("mark items by path: %w", err)
		}
		items = n
		m, err := q.MarkProjectMemoryRelationsStaleByPath(ctx, gen.MarkProjectMemoryRelationsStaleByPathParams{
			State: string(state), StateReason: reason, InvalidatedAt: at, UpdatedAt: now,
			ProjectID: string(projectID), RepoID: repoID, Path: path,
		})
		if err != nil {
			return fmt.Errorf("mark relations by path: %w", err)
		}
		relations = m
		return nil
	})
	return items, relations, err
}

// RetireProjectMemoryBelowGeneration invalidates what a COMPLETED full pass
// did not re-confirm.
//
// The caller must only reach this after a pass that walked the whole
// repository: a canonical fact left behind at an older generation was not
// re-derived by a walk that saw everything, which means its subject is gone.
// After a partial or incremental pass the same call would invalidate facts the
// pass simply never looked at, which is why the indexer gates it on the full
// walk having completed.
//
// Task-local facts are untouched: they are not produced by the walk, so the
// walk's silence says nothing about them.
func (s *Store) RetireProjectMemoryBelowGeneration(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, reason string, now time.Time,
) (items, relations int64, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	at := sql.NullTime{Time: now, Valid: true}
	err = s.inTx(ctx, "retire project memory below generation", func(q *gen.Queries) error {
		n, err := q.MarkProjectMemoryItemsStaleBelowGeneration(ctx, gen.MarkProjectMemoryItemsStaleBelowGenerationParams{
			State: string(domain.MemoryStateInvalidated), StateReason: reason,
			InvalidatedAt: at, UpdatedAt: now,
			ProjectID: string(projectID), RepoID: repoID,
			State_2:    string(domain.MemoryStateInvalidated),
			Generation: generation,
		})
		if err != nil {
			return fmt.Errorf("retire items: %w", err)
		}
		items = n
		m, err := q.MarkProjectMemoryRelationsStaleBelowGeneration(ctx, gen.MarkProjectMemoryRelationsStaleBelowGenerationParams{
			State: string(domain.MemoryStateInvalidated), StateReason: reason,
			InvalidatedAt: at, UpdatedAt: now,
			ProjectID: string(projectID), RepoID: repoID,
			State_2:    string(domain.MemoryStateInvalidated),
			Generation: generation,
		})
		if err != nil {
			return fmt.Errorf("retire relations: %w", err)
		}
		relations = m
		return nil
	})
	return items, relations, err
}

// ListProjectMemoryRelations reads every edge for one repository.
func (s *Store) ListProjectMemoryRelations(ctx context.Context, projectID domain.ProjectID, repoID string) ([]domain.ProjectMemoryRelation, error) {
	rows, err := s.qr.ListProjectMemoryRelations(ctx, gen.ListProjectMemoryRelationsParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return nil, fmt.Errorf("list project memory relations: %w", err)
	}
	return projectMemoryRelationsFromRows(rows)
}

// ListProjectMemoryRelationsFrom traverses outward from one node.
func (s *Store) ListProjectMemoryRelationsFrom(
	ctx context.Context, projectID domain.ProjectID, repoID string,
	from domain.ProjectMemoryNode, state domain.ProjectMemoryState,
) ([]domain.ProjectMemoryRelation, error) {
	from = from.Normalized()
	rows, err := s.qr.ListProjectMemoryRelationsFrom(ctx, gen.ListProjectMemoryRelationsFromParams{
		ProjectID: string(projectID), RepoID: repoID,
		FromKind: string(from.Kind), FromKey: from.Key, State: string(state),
	})
	if err != nil {
		return nil, fmt.Errorf("list project memory relations from: %w", err)
	}
	return projectMemoryRelationsFromRows(rows)
}

// ListProjectMemoryRelationsTo traverses inward to one node.
func (s *Store) ListProjectMemoryRelationsTo(
	ctx context.Context, projectID domain.ProjectID, repoID string,
	to domain.ProjectMemoryNode, state domain.ProjectMemoryState,
) ([]domain.ProjectMemoryRelation, error) {
	to = to.Normalized()
	rows, err := s.qr.ListProjectMemoryRelationsTo(ctx, gen.ListProjectMemoryRelationsToParams{
		ProjectID: string(projectID), RepoID: repoID,
		ToKind: string(to.Kind), ToKey: to.Key, State: string(state),
	})
	if err != nil {
		return nil, fmt.Errorf("list project memory relations to: %w", err)
	}
	return projectMemoryRelationsFromRows(rows)
}

// UpsertProjectMemoryFile records that one path was observed at one digest
// under one generation. The ledger is what makes a later pass able to skip an
// unchanged file without asking git, and what makes a deletion detectable as
// "a path the newest completed pass never saw".
func (s *Store) UpsertProjectMemoryFile(
	ctx context.Context, projectID domain.ProjectID, repoID, path, digest string,
	size int64, generation int64, commit string, now time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.UpsertProjectMemoryFile(ctx, gen.UpsertProjectMemoryFileParams{
		ProjectID: string(projectID), RepoID: repoID, Path: path,
		ContentDigest: digest, SizeBytes: size, Generation: generation,
		IndexedCommit: commit, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("upsert project memory file: %w", err)
	}
	return nil
}

// ProjectMemoryFileRecord is one path's entry in the digest ledger.
type ProjectMemoryFileRecord struct {
	Path       string
	Digest     string
	Size       int64
	Generation int64
	Commit     string
	UpdatedAt  time.Time
}

// GetProjectMemoryFile reads one path's ledger entry.
func (s *Store) GetProjectMemoryFile(ctx context.Context, projectID domain.ProjectID, repoID, path string) (ProjectMemoryFileRecord, bool, error) {
	row, err := s.qr.GetProjectMemoryFile(ctx, gen.GetProjectMemoryFileParams{
		ProjectID: string(projectID), RepoID: repoID, Path: path,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectMemoryFileRecord{}, false, nil
	}
	if err != nil {
		return ProjectMemoryFileRecord{}, false, fmt.Errorf("get project memory file: %w", err)
	}
	return projectMemoryFileFromRow(row), true, nil
}

// ListProjectMemoryFiles reads the whole digest ledger for one repository, in
// path order.
func (s *Store) ListProjectMemoryFiles(ctx context.Context, projectID domain.ProjectID, repoID string) ([]ProjectMemoryFileRecord, error) {
	rows, err := s.qr.ListProjectMemoryFiles(ctx, gen.ListProjectMemoryFilesParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return nil, fmt.Errorf("list project memory files: %w", err)
	}
	out := make([]ProjectMemoryFileRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, projectMemoryFileFromRow(r))
	}
	return out, nil
}

// ListProjectMemoryFilesBelowGeneration names the paths a completed pass did
// not re-observe: deleted, renamed away, or newly excluded by the bounds.
//
// Detecting deletions this way rather than from a git diff is deliberate — it
// works for an untracked file, a non-git checkout, and a bounds change, none
// of which a diff would report.
func (s *Store) ListProjectMemoryFilesBelowGeneration(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
) ([]ProjectMemoryFileRecord, error) {
	rows, err := s.qr.ListProjectMemoryFilesBelowGeneration(ctx, gen.ListProjectMemoryFilesBelowGenerationParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	})
	if err != nil {
		return nil, fmt.Errorf("list stale project memory files: %w", err)
	}
	out := make([]ProjectMemoryFileRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, projectMemoryFileFromRow(r))
	}
	return out, nil
}

// DeleteProjectMemoryFile drops one path's ledger entry, for a deletion or a
// rename a diff reported directly. Without it the next full pass would
// "discover" the same deletion a second time.
func (s *Store) DeleteProjectMemoryFile(ctx context.Context, projectID domain.ProjectID, repoID, path string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.DeleteProjectMemoryFile(ctx, gen.DeleteProjectMemoryFileParams{
		ProjectID: string(projectID), RepoID: repoID, Path: path,
	})
	if err != nil {
		return false, fmt.Errorf("delete project memory file: %w", err)
	}
	return n > 0, nil
}

// PruneProjectMemoryFilesBelowGeneration drops ledger entries a completed pass
// did not re-observe, once the facts derived from them have been invalidated.
func (s *Store) PruneProjectMemoryFilesBelowGeneration(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.DeleteProjectMemoryFilesBelowGeneration(ctx, gen.DeleteProjectMemoryFilesBelowGenerationParams{
		ProjectID: string(projectID), RepoID: repoID, Generation: generation,
	})
	if err != nil {
		return 0, fmt.Errorf("prune project memory files: %w", err)
	}
	return n, nil
}

// GetProjectMemoryStatus assembles the operator-facing view of one
// repository's memory: its indexing state, its per-state and per-type census,
// and when it last changed.
func (s *Store) GetProjectMemoryStatus(ctx context.Context, projectID domain.ProjectID, repoID string) (domain.ProjectMemoryStatus, bool, error) {
	idx, ok, err := s.GetProjectMemoryIndexState(ctx, projectID, repoID)
	if err != nil || !ok {
		return domain.ProjectMemoryStatus{}, ok, err
	}
	status := domain.ProjectMemoryStatus{
		ProjectID: projectID, RepoID: repoID, RepoPath: idx.RepoPath, Index: idx,
		ByType:        map[domain.ProjectMemoryType]int{},
		LastIndexedAt: idx.CompletedAt,
	}
	byState, err := s.qr.CountProjectMemoryItemsByState(ctx, gen.CountProjectMemoryItemsByStateParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return domain.ProjectMemoryStatus{}, false, fmt.Errorf("count project memory items by state: %w", err)
	}
	for _, r := range byState {
		n := int(r.Total)
		status.Counts.Total += n
		switch domain.ProjectMemoryState(r.State) {
		case domain.MemoryStateValid:
			status.Counts.Valid = n
		case domain.MemoryStateStale:
			status.Counts.Stale = n
		case domain.MemoryStateInvalidated:
			status.Counts.Invalidated = n
		case domain.MemoryStateRebuilding:
			status.Counts.Rebuilding = n
		}
	}
	byType, err := s.qr.CountProjectMemoryItemsByType(ctx, gen.CountProjectMemoryItemsByTypeParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return domain.ProjectMemoryStatus{}, false, fmt.Errorf("count project memory items by type: %w", err)
	}
	for _, r := range byType {
		status.ByType[domain.ProjectMemoryType(r.ItemType)] = int(r.Total)
	}
	taskLocal, err := s.qr.CountProjectMemoryTaskLocalItems(ctx, gen.CountProjectMemoryTaskLocalItemsParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return domain.ProjectMemoryStatus{}, false, fmt.Errorf("count task-local project memory items: %w", err)
	}
	status.Counts.TaskLocal = int(taskLocal)
	rels, err := s.qr.CountProjectMemoryRelations(ctx, gen.CountProjectMemoryRelationsParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return domain.ProjectMemoryStatus{}, false, fmt.Errorf("count project memory relations: %w", err)
	}
	status.Counts.Relations = int(rels)
	last, err := s.qr.LatestProjectMemoryItemUpdatedAt(ctx, gen.LatestProjectMemoryItemUpdatedAtParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A repository with no facts yet has no freshness to report, which is
		// a normal state rather than a failure.
	case err != nil:
		return domain.ProjectMemoryStatus{}, false, fmt.Errorf("read project memory freshness: %w", err)
	default:
		status.LastUpdatedAt = last
	}
	return status, true, nil
}

// PurgeProjectMemoryRepo deletes every fact, edge and ledger entry for one
// repository. It is reserved for an explicit rebuild --purge and for
// de-registration; ordinary invalidation never deletes.
func (s *Store) PurgeProjectMemoryRepo(ctx context.Context, projectID domain.ProjectID, repoID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "purge project memory repo", func(q *gen.Queries) error {
		if _, err := q.DeleteProjectMemoryItemsForRepo(ctx, gen.DeleteProjectMemoryItemsForRepoParams{
			ProjectID: string(projectID), RepoID: repoID,
		}); err != nil {
			return fmt.Errorf("delete items: %w", err)
		}
		if _, err := q.DeleteProjectMemoryRelationsForRepo(ctx, gen.DeleteProjectMemoryRelationsForRepoParams{
			ProjectID: string(projectID), RepoID: repoID,
		}); err != nil {
			return fmt.Errorf("delete relations: %w", err)
		}
		if _, err := q.DeleteProjectMemoryFilesForRepo(ctx, gen.DeleteProjectMemoryFilesForRepoParams{
			ProjectID: string(projectID), RepoID: repoID,
		}); err != nil {
			return fmt.Errorf("delete files: %w", err)
		}
		return nil
	})
}

// DiscardProjectMemoryForTask retires one task's unintegrated facts and edges.
//
// This is what keeps an isolated worktree from leaving a permanent parallel
// memory behind: task-local rows exist for the life of the task and are
// deleted when it ends, unless an authority promoted them to canonical first.
func (s *Store) DiscardProjectMemoryForTask(ctx context.Context, projectID domain.ProjectID, taskRef string) (items, relations int64, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err = s.inTx(ctx, "discard task-local project memory", func(q *gen.Queries) error {
		n, err := q.DeleteProjectMemoryTaskLocalItems(ctx, gen.DeleteProjectMemoryTaskLocalItemsParams{
			ProjectID: string(projectID), OriginRef: taskRef,
		})
		if err != nil {
			return fmt.Errorf("delete task-local items: %w", err)
		}
		items = n
		m, err := q.DeleteProjectMemoryTaskLocalRelations(ctx, gen.DeleteProjectMemoryTaskLocalRelationsParams{
			ProjectID: string(projectID), OriginRef: taskRef,
		})
		if err != nil {
			return fmt.Errorf("delete task-local relations: %w", err)
		}
		relations = m
		return nil
	})
	return items, relations, err
}

// --- row mapping ------------------------------------------------------------

func marshalProjectMemoryPayload(paths []string, meta map[string]string) (pathsJSON, metaJSON string, err error) {
	p, err := json.Marshal(paths)
	if err != nil {
		return "", "", fmt.Errorf("encode source paths: %w", err)
	}
	if meta == nil {
		meta = map[string]string{}
	}
	m, err := json.Marshal(meta)
	if err != nil {
		return "", "", fmt.Errorf("encode metadata: %w", err)
	}
	return string(p), string(m), nil
}

func projectMemoryIndexFromRow(r gen.ProjectMemoryIndex) domain.ProjectMemoryIndexState {
	return domain.ProjectMemoryIndexState{
		ProjectID:        domain.ProjectID(r.ProjectID),
		RepoID:           r.RepoID,
		RepoPath:         r.RepoPath,
		Generation:       r.Generation,
		Phase:            domain.ProjectMemoryIndexPhase(r.Phase),
		IndexedCommit:    r.IndexedCommit,
		PendingCommit:    r.PendingCommit,
		Branch:           r.Branch,
		Cursor:           r.ResumeCursor,
		FilesSeen:        int(r.FilesSeen),
		FilesIndexed:     int(r.FilesIndexed),
		FilesSkipped:     int(r.FilesSkipped),
		ItemsWritten:     int(r.ItemsWritten),
		RelationsWritten: int(r.RelationsWritten),
		LastError:        r.LastError,
		StartedAt:        r.StartedAt.Time,
		CompletedAt:      r.CompletedAt.Time,
		UpdatedAt:        r.UpdatedAt,
	}
}

func projectMemoryItemFromRow(r gen.ProjectMemoryItem) (domain.ProjectMemoryItem, error) {
	var paths []string
	if r.SourcePathsJson != "" {
		if err := json.Unmarshal([]byte(r.SourcePathsJson), &paths); err != nil {
			return domain.ProjectMemoryItem{}, fmt.Errorf("decode source paths for item %s: %w", r.ID, err)
		}
	}
	meta := map[string]string{}
	if r.MetadataJson != "" {
		if err := json.Unmarshal([]byte(r.MetadataJson), &meta); err != nil {
			return domain.ProjectMemoryItem{}, fmt.Errorf("decode metadata for item %s: %w", r.ID, err)
		}
	}
	if len(meta) == 0 {
		meta = nil
	}
	item := domain.ProjectMemoryItem{
		ID: r.ID,
		Key: domain.ProjectMemoryKey{
			ProjectID: domain.ProjectID(r.ProjectID),
			RepoID:    r.RepoID,
			Type:      domain.ProjectMemoryType(r.ItemType),
			Scope:     domain.ProjectMemoryScope(r.Scope),
			Key:       r.ItemKey,
		},
		Origin:        domain.ProjectMemoryOrigin(r.Origin),
		OriginRef:     r.OriginRef,
		Summary:       r.Summary,
		Content:       r.Content,
		SourcePaths:   paths,
		SourceCommit:  r.SourceCommit,
		SourceDigest:  r.SourceDigest,
		Generation:    r.Generation,
		State:         domain.ProjectMemoryState(r.State),
		StateReason:   r.StateReason,
		Confidence:    r.Confidence,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		InvalidatedAt: r.InvalidatedAt.Time,
		Metadata:      meta,
		ContentHash:   r.ContentHash,
	}
	return item, nil
}

func projectMemoryItemsFromRows(rows []gen.ProjectMemoryItem) ([]domain.ProjectMemoryItem, error) {
	out := make([]domain.ProjectMemoryItem, 0, len(rows))
	for _, r := range rows {
		item, err := projectMemoryItemFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func projectMemoryRelationFromRow(r gen.ProjectMemoryRelation) (domain.ProjectMemoryRelation, error) {
	var paths []string
	if r.SourcePathsJson != "" {
		if err := json.Unmarshal([]byte(r.SourcePathsJson), &paths); err != nil {
			return domain.ProjectMemoryRelation{}, fmt.Errorf("decode source paths for relation %s: %w", r.ID, err)
		}
	}
	meta := map[string]string{}
	if r.MetadataJson != "" {
		if err := json.Unmarshal([]byte(r.MetadataJson), &meta); err != nil {
			return domain.ProjectMemoryRelation{}, fmt.Errorf("decode metadata for relation %s: %w", r.ID, err)
		}
	}
	if len(meta) == 0 {
		meta = nil
	}
	return domain.ProjectMemoryRelation{
		ID:            r.ID,
		ProjectID:     domain.ProjectID(r.ProjectID),
		RepoID:        r.RepoID,
		From:          domain.ProjectMemoryNode{Kind: domain.ProjectMemoryNodeKind(r.FromKind), Key: r.FromKey},
		Kind:          domain.ProjectMemoryRelationKind(r.RelationKind),
		To:            domain.ProjectMemoryNode{Kind: domain.ProjectMemoryNodeKind(r.ToKind), Key: r.ToKey},
		Origin:        domain.ProjectMemoryOrigin(r.Origin),
		OriginRef:     r.OriginRef,
		SourcePaths:   paths,
		SourceCommit:  r.SourceCommit,
		SourceDigest:  r.SourceDigest,
		Generation:    r.Generation,
		State:         domain.ProjectMemoryState(r.State),
		StateReason:   r.StateReason,
		Confidence:    r.Confidence,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		InvalidatedAt: r.InvalidatedAt.Time,
		Metadata:      meta,
	}, nil
}

func projectMemoryRelationsFromRows(rows []gen.ProjectMemoryRelation) ([]domain.ProjectMemoryRelation, error) {
	out := make([]domain.ProjectMemoryRelation, 0, len(rows))
	for _, r := range rows {
		rel, err := projectMemoryRelationFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, nil
}

func projectMemoryFileFromRow(r gen.ProjectMemoryFile) ProjectMemoryFileRecord {
	return ProjectMemoryFileRecord{
		Path: r.Path, Digest: r.ContentDigest, Size: r.SizeBytes,
		Generation: r.Generation, Commit: r.IndexedCommit, UpdatedAt: r.UpdatedAt,
	}
}

func nullTimeOrZero(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}
