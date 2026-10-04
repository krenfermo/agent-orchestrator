package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// project_memory_store_test.go — the CAS fence and the provenance index, at
// the layer that actually enforces them.
//
// These tests exist for the same reason the capacity ones do: the correctness
// of a generation-conditioned write lives in a WHERE clause. If the clause is
// wrong, every higher-level test still passes (a single-threaded indexer never
// notices) and a stalled pass silently overwrites newer memory in production.

const memProject = domain.ProjectID("p")

func memoryFixture(t *testing.T) (*sqlite.Store, string, context.Context) {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	repoPath := t.TempDir()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(memProject), Path: repoPath, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	repoID := domain.ProjectMemoryRepoID(repoPath)
	if err := st.EnsureProjectMemoryRepo(ctx, memProject, repoID, repoPath, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return st, repoID, ctx
}

func memoryItem(repoID string, gen int64, summary string) domain.ProjectMemoryItem {
	return domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: memProject, RepoID: repoID,
			Type: domain.MemoryTypeModule, Scope: domain.MemoryScopeModule, Key: "internal/workflow",
		},
		Summary:      summary,
		Content:      "the workflow coordinator",
		SourcePaths:  []string{"internal/workflow/coordinator.go"},
		SourceCommit: "abc123",
		SourceDigest: "digest-1",
		Generation:   gen,
		Confidence:   0.8,
	}
}

func TestPutProjectMemoryItemCreatesUpdatesAndReconfirms(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	outcome, err := st.PutProjectMemoryItem(ctx, memoryItem(repoID, 1, "workflow module"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if outcome != store.MemoryWriteCreated {
		t.Fatalf("first write outcome = %q, want created", outcome)
	}

	// Re-writing the identical fact at the same generation must NOT move
	// updated_at: freshness ranking depends on "when did this fact really
	// change", and a re-index that touched every timestamp would destroy it.
	later := now.Add(time.Hour)
	outcome, err = st.PutProjectMemoryItem(ctx, memoryItem(repoID, 1, "workflow module"), later)
	if err != nil {
		t.Fatalf("reconfirm: %v", err)
	}
	if outcome != store.MemoryWriteReconfirmed {
		t.Fatalf("unchanged write outcome = %q, want reconfirmed", outcome)
	}
	stored, ok, err := st.GetProjectMemoryItem(ctx, memoryItem(repoID, 1, "").Key.ID())
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if !stored.UpdatedAt.Equal(now) {
		t.Fatalf("reconfirmation moved updated_at to %v, want %v", stored.UpdatedAt, now)
	}

	// A changed body at a newer generation is an update, and it does move the
	// timestamp.
	outcome, err = st.PutProjectMemoryItem(ctx, memoryItem(repoID, 2, "workflow coordinator module"), later)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if outcome != store.MemoryWriteUpdated {
		t.Fatalf("changed write outcome = %q, want updated", outcome)
	}
	stored, _, err = st.GetProjectMemoryItem(ctx, memoryItem(repoID, 2, "").Key.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !stored.UpdatedAt.Equal(later) || stored.Generation != 2 {
		t.Fatalf("stored = updatedAt %v gen %d, want %v / 2", stored.UpdatedAt, stored.Generation, later)
	}
}

// TestPutProjectMemoryItemRefusesStaleGeneration is the load-bearing one: the
// completion bar for P2-A says a stale generation must not be able to
// overwrite newer memory.
func TestPutProjectMemoryItemRefusesStaleGeneration(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	if _, err := st.PutProjectMemoryItem(ctx, memoryItem(repoID, 5, "current"), now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	outcome, err := st.PutProjectMemoryItem(ctx, memoryItem(repoID, 3, "from a pass that stalled"), now)
	if !errors.Is(err, store.ErrProjectMemoryStaleGeneration) {
		t.Fatalf("stale write err = %v, want ErrProjectMemoryStaleGeneration", err)
	}
	if outcome != store.MemoryWriteRefused {
		t.Fatalf("stale write outcome = %q, want refused", outcome)
	}
	stored, _, err := st.GetProjectMemoryItem(ctx, memoryItem(repoID, 5, "").Key.ID())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Summary != "current" || stored.Generation != 5 {
		t.Fatalf("stale writer changed the row: summary=%q gen=%d", stored.Summary, stored.Generation)
	}
}

func TestInvalidateProjectMemoryByPathUsesTheProvenanceIndex(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	if _, err := st.PutProjectMemoryItem(ctx, memoryItem(repoID, 1, "touched"), now); err != nil {
		t.Fatal(err)
	}
	untouched := memoryItem(repoID, 1, "untouched")
	untouched.Key.Key = "internal/review"
	untouched.SourcePaths = []string{"internal/review/prompt.go"}
	if _, err := st.PutProjectMemoryItem(ctx, untouched, now); err != nil {
		t.Fatal(err)
	}

	items, _, err := st.InvalidateProjectMemoryByPath(ctx, memProject, repoID,
		"internal/workflow/coordinator.go", domain.MemoryStateStale, "source changed", now)
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if items != 1 {
		t.Fatalf("invalidated %d items, want exactly the one derived from the changed path", items)
	}

	valid, err := st.ListProjectMemoryItemsByState(ctx, memProject, repoID, domain.MemoryStateValid)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 || valid[0].Summary != "untouched" {
		t.Fatalf("valid items = %+v, want only the untouched one", valid)
	}
}

// TestClaimProjectMemoryIndexPassAdmitsOneWriter pins the concurrent-index
// rule: two callers racing to index one repository resolve to one winner, and
// the loser reads a plain false rather than an error.
func TestClaimProjectMemoryIndexPassAdmitsOneWriter(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	first, ok, err := st.ClaimProjectMemoryIndexPass(ctx, memProject, repoID, "head1", "main", now)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if first.Generation != 1 {
		t.Fatalf("first claim generation = %d, want 1", first.Generation)
	}
	if _, ok, err = st.ClaimProjectMemoryIndexPass(ctx, memProject, repoID, "head1", "main", now); err != nil {
		t.Fatalf("second claim: %v", err)
	} else if ok {
		t.Fatal("second claim succeeded while a pass was in flight")
	}

	// A pass that died is recoverable, but only by a caller that knows which
	// generation it is taking over.
	if _, ok, err = st.ResumeProjectMemoryIndexPass(ctx, memProject, repoID, 99,
		domain.IndexPhaseScanning, "head1", "main", now); err != nil {
		t.Fatalf("resume with wrong generation: %v", err)
	} else if ok {
		t.Fatal("resume succeeded against a generation the caller did not hold")
	}
	if _, ok, err = st.ResumeProjectMemoryIndexPass(ctx, memProject, repoID, first.Generation,
		domain.IndexPhaseScanning, "head1", "main", now); err != nil || !ok {
		t.Fatalf("resume with the held generation: ok=%v err=%v", ok, err)
	}
}

// TestCompleteProjectMemoryIndexPassPromotesTheCommit pins the rule that a
// failed pass must not advance the commit incremental update diffs from.
func TestCompleteProjectMemoryIndexPassPromotesTheCommit(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	claimed, _, err := st.ClaimProjectMemoryIndexPass(ctx, memProject, repoID, "head1", "main", now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.FailProjectMemoryIndexPass(ctx, memProject, repoID, claimed.Generation, "boom", now); err != nil || !ok {
		t.Fatalf("fail pass: ok=%v err=%v", ok, err)
	}
	state, _, err := st.GetProjectMemoryIndexState(ctx, memProject, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if state.IndexedCommit != "" {
		t.Fatalf("a failed pass advanced indexed_commit to %q", state.IndexedCommit)
	}

	claimed, _, err = st.ClaimProjectMemoryIndexPass(ctx, memProject, repoID, "head2", "main", now)
	if err != nil {
		t.Fatal(err)
	}
	claimed.FilesIndexed = 3
	if ok, err := st.CompleteProjectMemoryIndexPass(ctx, claimed, now); err != nil || !ok {
		t.Fatalf("complete pass: ok=%v err=%v", ok, err)
	}
	state, _, err = st.GetProjectMemoryIndexState(ctx, memProject, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if state.IndexedCommit != "head2" || state.Phase != domain.IndexPhaseIdle {
		t.Fatalf("after completion: commit=%q phase=%q", state.IndexedCommit, state.Phase)
	}
}

// TestRetireProjectMemoryBelowGenerationSparesTaskLocalItems pins the
// multi-repo/worktree rule at the storage layer: a repository walk's silence
// says nothing about a task's own unintegrated facts, because the walk never
// produced them.
func TestRetireProjectMemoryBelowGenerationSparesTaskLocalItems(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	if _, err := st.PutProjectMemoryItem(ctx, memoryItem(repoID, 1, "canonical"), now); err != nil {
		t.Fatal(err)
	}
	local := memoryItem(repoID, 1, "what task t1 changed")
	local.Key.Type = domain.MemoryTypeTaskResult
	local.Key.Scope = domain.MemoryScopeTask
	local.Key.Key = "t1"
	local.Origin = domain.OriginTaskLocal
	local.OriginRef = "t1"
	if _, err := st.PutProjectMemoryItem(ctx, local, now); err != nil {
		t.Fatal(err)
	}

	items, _, err := st.RetireProjectMemoryBelowGeneration(ctx, memProject, repoID, 2, "not re-derived by a full pass", now)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if items != 1 {
		t.Fatalf("retired %d items, want only the canonical one", items)
	}
	taskLocal, err := st.ListProjectMemoryItemsForTask(ctx, memProject, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(taskLocal) != 1 || taskLocal[0].State != domain.MemoryStateValid {
		t.Fatalf("task-local item was retired by a repository walk: %+v", taskLocal)
	}
}

func TestProjectMemoryStatusReportsCensusAndIndexState(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	claimed, _, err := st.ClaimProjectMemoryIndexPass(ctx, memProject, repoID, "head1", "main", now)
	if err != nil {
		t.Fatal(err)
	}
	item := memoryItem(repoID, claimed.Generation, "workflow module")
	if _, err := st.PutProjectMemoryItem(ctx, item, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CompleteProjectMemoryIndexPass(ctx, claimed, now); err != nil {
		t.Fatal(err)
	}

	status, ok, err := st.GetProjectMemoryStatus(ctx, memProject, repoID)
	if err != nil || !ok {
		t.Fatalf("status: ok=%v err=%v", ok, err)
	}
	if status.Counts.Total != 1 || status.Counts.Valid != 1 {
		t.Fatalf("counts = %+v", status.Counts)
	}
	if status.ByType[domain.MemoryTypeModule] != 1 {
		t.Fatalf("byType = %+v", status.ByType)
	}
	if !status.Healthy() {
		t.Fatalf("status not healthy after a completed pass with valid items: %+v", status)
	}
	if status.LastUpdatedAt.IsZero() {
		t.Fatal("status did not report a last-updated time")
	}
}

func TestPutProjectMemoryRelationIsGenerationFenced(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	rel := domain.ProjectMemoryRelation{
		ProjectID: memProject, RepoID: repoID,
		From:        domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: "internal/workflow"},
		Kind:        domain.RelationDependsOn,
		To:          domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: "internal/domain"},
		SourcePaths: []string{"internal/workflow/coordinator.go"},
		Generation:  4,
		Confidence:  0.9,
	}
	if outcome, err := st.PutProjectMemoryRelation(ctx, rel, now); err != nil || outcome != store.MemoryWriteCreated {
		t.Fatalf("create relation: outcome=%q err=%v", outcome, err)
	}

	stale := rel
	stale.Generation = 2
	stale.Confidence = 0.1
	if _, err := st.PutProjectMemoryRelation(ctx, stale, now); !errors.Is(err, store.ErrProjectMemoryStaleGeneration) {
		t.Fatalf("stale relation write err = %v, want ErrProjectMemoryStaleGeneration", err)
	}

	edges, err := st.ListProjectMemoryRelationsFrom(ctx, memProject, repoID,
		domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: "internal/workflow"}, domain.MemoryStateValid)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Confidence != 0.9 {
		t.Fatalf("edges = %+v, want the generation-4 edge intact", edges)
	}
}

func TestDiscardProjectMemoryForTaskRemovesOnlyThatTask(t *testing.T) {
	st, repoID, ctx := memoryFixture(t)
	now := time.Now().UTC()

	for _, task := range []string{"t1", "t2"} {
		item := memoryItem(repoID, 1, "what "+task+" changed")
		item.Key.Type = domain.MemoryTypeTaskResult
		item.Key.Scope = domain.MemoryScopeTask
		item.Key.Key = task
		item.Origin = domain.OriginTaskLocal
		item.OriginRef = task
		if _, err := st.PutProjectMemoryItem(ctx, item, now); err != nil {
			t.Fatal(err)
		}
	}

	items, _, err := st.DiscardProjectMemoryForTask(ctx, memProject, "t1")
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if items != 1 {
		t.Fatalf("discarded %d items, want 1", items)
	}
	if remaining, err := st.ListProjectMemoryItemsForTask(ctx, memProject, "t2"); err != nil {
		t.Fatal(err)
	} else if len(remaining) != 1 {
		t.Fatalf("t2's memory = %+v, want it untouched", remaining)
	}
}
