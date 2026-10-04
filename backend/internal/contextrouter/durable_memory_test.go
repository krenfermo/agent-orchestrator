package contextrouter_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// durable_memory_test.go — the boundary P2-A's memory crosses to reach the
// Planner, the Worker and both Repair Agents.
//
// Two of these tests are the fail-closed rule, and they matter more than the
// happy path: a stale fact or one task's unintegrated view reaching a routed
// payload is exactly the failure the whole provenance model exists to prevent.

const memProject = domain.ProjectID("proj-1")

func durableFixture(t *testing.T) (*sqlite.Store, string, context.Context) {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	root := t.TempDir()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(memProject), Path: root, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	repoID := domain.ProjectMemoryRepoID(root)
	if err := st.EnsureProjectMemoryRepo(ctx, memProject, repoID, root, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return st, repoID, ctx
}

func durableItem(repoID, key, summary string) domain.ProjectMemoryItem {
	return domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: memProject, RepoID: repoID,
			Type: domain.MemoryTypeConvention, Scope: domain.MemoryScopeRepository, Key: key,
		},
		Summary:      summary,
		Content:      "the body of " + key,
		SourcePaths:  []string{"AGENTS.md"},
		SourceCommit: "c1",
		SourceDigest: "d1",
		Confidence:   0.9,
	}
}

func TestDurableMemorySourceServesCanonicalValidFacts(t *testing.T) {
	st, repoID, ctx := durableFixture(t)
	if _, err := st.PutProjectMemoryItem(ctx, durableItem(repoID, "surgical", "keep changes surgical"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	items, err := contextrouter.NewDurableMemorySource(st).List(string(memProject))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	got := items[0]
	switch {
	case got.Project != string(memProject):
		t.Errorf("project = %q", got.Project)
	case got.Stale:
		t.Error("a valid fact was handed over marked stale")
	case got.Source.Path != "AGENTS.md":
		t.Errorf("source path = %q, want the anchor the router matches on", got.Source.Path)
	case got.Confidence != 0.9:
		t.Errorf("confidence = %v", got.Confidence)
	case got.Content == "":
		t.Error("the fact carries no content")
	}
	_ = ctx
}

// The fail-closed rule at the dispatch boundary: a fact AO can no longer vouch
// for is not handed to the router at all.
func TestDurableMemorySourceWithholdsNonAuthoritativeFacts(t *testing.T) {
	st, repoID, ctx := durableFixture(t)
	now := time.Now().UTC()
	if _, err := st.PutProjectMemoryItem(ctx, durableItem(repoID, "good", "still true"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutProjectMemoryItem(ctx, durableItem(repoID, "bad", "no longer provable"), now); err != nil {
		t.Fatal(err)
	}
	stale := durableItem(repoID, "bad", "no longer provable")
	if _, err := st.MarkProjectMemoryItemState(ctx, stale.Normalized().ID, 0,
		domain.MemoryStateStale, "source moved", now); err != nil {
		t.Fatal(err)
	}

	items, err := contextrouter.NewDurableMemorySource(st).List(string(memProject))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Content != "" && it.Scope == "bad" {
			t.Fatal("a stale fact reached the router")
		}
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want only the one AO can still vouch for", len(items))
	}
}

// One task's view of unintegrated work must never become another task's
// premise, and the router cannot make that distinction itself.
func TestDurableMemorySourceWithholdsTaskLocalFacts(t *testing.T) {
	st, repoID, ctx := durableFixture(t)
	now := time.Now().UTC()

	local := durableItem(repoID, "t1-decision", "task t1 decided something")
	local.Key.Type = domain.MemoryTypeDecision
	local.Origin = domain.OriginTaskLocal
	local.OriginRef = "t1"
	if _, err := st.PutProjectMemoryItem(ctx, local, now); err != nil {
		t.Fatal(err)
	}

	items, err := contextrouter.NewDurableMemorySource(st).List(string(memProject))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("task-local memory reached a routed payload: %+v", items)
	}
}

// A read failure costs context quality, never a dispatch.
func TestDurableMemorySourceFailsSoft(t *testing.T) {
	items, err := contextrouter.NewDurableMemorySource(nil).List(string(memProject))
	if err != nil || len(items) != 0 {
		t.Fatalf("nil repository: items=%d err=%v, want an empty, error-free answer", len(items), err)
	}
	st, _, _ := durableFixture(t)
	empty, err := contextrouter.NewDurableMemorySource(st).List("")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty project id: items=%d err=%v", len(empty), err)
	}
}
