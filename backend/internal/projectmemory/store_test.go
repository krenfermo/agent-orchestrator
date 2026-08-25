package projectmemory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedClock returns a clock that advances by a minute on every read, so a
// test can tell "this write moved UpdatedAt" from "this write left it alone"
// without depending on wall-clock resolution.
func fixedClock(start time.Time) func() time.Time {
	tick := 0
	return func() time.Time {
		t := start.Add(time.Duration(tick) * time.Minute)
		tick++
		return t
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir(), WithClock(fixedClock(time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func sampleItem(project, ref, content string) MemoryItem {
	return MemoryItem{
		Project:      project,
		Scope:        "backend/internal/projectmemory",
		Type:         TypeNote,
		Content:      content,
		Source:       Source{Kind: SourceManual, Ref: ref},
		SourceCommit: "abc123",
		Confidence:   0.8,
	}
}

func TestStoreRootResolvesUnderAODataDir(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AO_DATA_DIR", dataDir)

	root, err := StoreRoot()
	if err != nil {
		t.Fatalf("StoreRoot: %v", err)
	}
	want := filepath.Join(dataDir, "project-memory", "items")
	if root != want {
		t.Fatalf("StoreRoot = %q, want %q", root, want)
	}

	store, err := NewDefaultStore()
	if err != nil {
		t.Fatalf("NewDefaultStore: %v", err)
	}
	path := store.PathFor("proj-a")
	if !strings.HasPrefix(path, want+string(filepath.Separator)) {
		t.Fatalf("project file %q is not under the AO data dir %q", path, want)
	}
	if filepath.Base(path) != projectFileName {
		t.Fatalf("project file %q does not end in %q", path, projectFileName)
	}

	if _, err := store.Upsert(context.Background(), sampleItem("proj-a", "note-1", "durable fact")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected the item file at the resolved path: %v", err)
	}
}

func TestStoreRootFallsBackToHomeAoData(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AO_DATA_DIR", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	root, err := StoreRoot()
	if err != nil {
		t.Fatalf("StoreRoot: %v", err)
	}
	want := filepath.Join(home, ".ao", "data", "project-memory", "items")
	if root != want {
		t.Fatalf("StoreRoot = %q, want %q (~/.ao is the only home for AO state)", root, want)
	}
}

func TestValidateStoreDirRejectsForbiddenLocations(t *testing.T) {
	cases := map[string]string{
		"relative":              filepath.Join("relative", "project-memory"),
		"empty":                 "   ",
		"macOS app support":     filepath.Join("/Users", "someone", "Library", "Application Support", "ao"),
		"windows appdata local": filepath.Join("C:", "Users", "someone", "AppData", "Local", "ao"),
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateStoreDir(dir); !errors.Is(err, ErrStorePath) {
				t.Fatalf("ValidateStoreDir(%q) = %v, want ErrStorePath", dir, err)
			}
		})
	}
}

func TestUpsertCreatesThenIsIdempotent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	item := sampleItem("proj-a", "note-1", "the daemon owns worktree teardown")

	created, err := store.Upsert(ctx, item)
	if err != nil {
		t.Fatalf("Upsert (create): %v", err)
	}
	if created.Created != 1 || created.Updated != 0 || created.Unchanged != 0 {
		t.Fatalf("first upsert = %+v, want one create", created)
	}
	stored := created.Items[0]
	if stored.ID == "" || stored.ContentHash == "" {
		t.Fatalf("stored item is missing derived identity: %+v", stored)
	}
	if !stored.CreatedAt.Equal(stored.UpdatedAt) {
		t.Fatalf("a new item should have CreatedAt == UpdatedAt, got %s / %s", stored.CreatedAt, stored.UpdatedAt)
	}

	again, err := store.Upsert(ctx, item)
	if err != nil {
		t.Fatalf("Upsert (re-ingest): %v", err)
	}
	if again.Unchanged != 1 || again.Created != 0 || again.Updated != 0 {
		t.Fatalf("re-ingesting unchanged content = %+v, want one unchanged", again)
	}
	if len(again.Paths) != 0 {
		t.Fatalf("an unchanged upsert must not rewrite the project file, wrote %v", again.Paths)
	}
	readBack := again.Items[0]
	if !readBack.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Fatalf("UpdatedAt moved on an unchanged re-ingest: %s -> %s", stored.UpdatedAt, readBack.UpdatedAt)
	}
	if !readBack.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("CreatedAt moved on an unchanged re-ingest: %s -> %s", stored.CreatedAt, readBack.CreatedAt)
	}

	items, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("re-ingesting the same fact produced %d rows, want 1", len(items))
	}
}

func TestUpsertUpdatesInPlaceWhenContentChanges(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	first, err := store.Upsert(ctx, sampleItem("proj-a", "note-1", "original fact"))
	if err != nil {
		t.Fatalf("Upsert (create): %v", err)
	}
	original := first.Items[0]

	second, err := store.Upsert(ctx, sampleItem("proj-a", "note-1", "revised fact"))
	if err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}
	if second.Updated != 1 || second.Created != 0 {
		t.Fatalf("changed content = %+v, want one update", second)
	}
	updated := second.Items[0]
	if updated.ID != original.ID {
		t.Fatalf("a source with a stable ref must keep its identity: %s -> %s", original.ID, updated.ID)
	}
	if updated.ContentHash == original.ContentHash {
		t.Fatal("content hash did not change with the content")
	}
	if !updated.CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("CreatedAt must survive an update: %s -> %s", original.CreatedAt, updated.CreatedAt)
	}
	if !updated.UpdatedAt.After(original.UpdatedAt) {
		t.Fatalf("UpdatedAt must move on a real change: %s -> %s", original.UpdatedAt, updated.UpdatedAt)
	}

	items, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("updating a fact produced %d rows, want 1", len(items))
	}
	if items[0].Content != "revised fact" {
		t.Fatalf("stored content = %q, want the revision", items[0].Content)
	}
}

func TestUpsertClearsStalenessWhenAFactIsReDerived(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.Upsert(ctx, sampleItem("proj-a", "note-1", "original fact")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	items, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	items[0].Stale = true
	items[0].StaleReason = "source commit abc123 is no longer reachable from HEAD"
	if _, err := store.Replace("proj-a", items); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	refreshed, err := store.Upsert(ctx, sampleItem("proj-a", "note-1", "re-derived fact"))
	if err != nil {
		t.Fatalf("Upsert (re-derive): %v", err)
	}
	if refreshed.Items[0].Stale || refreshed.Items[0].StaleReason != "" {
		t.Fatalf("a re-derived fact must not stay stale: %+v", refreshed.Items[0])
	}
}

func TestUpsertKeepsProjectsIsolated(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Identical scope, type, source ref and content in two projects: if
	// anything but the project id keyed the store, these would collide.
	a := sampleItem("proj-a", "note-1", "shared-looking fact")
	b := sampleItem("proj-b", "note-1", "shared-looking fact")

	result, err := store.Upsert(ctx, a, b)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("upsert of two projects = %+v, want two creates", result)
	}
	if result.Items[0].ID == result.Items[1].ID {
		t.Fatal("two projects produced one identity for the same content")
	}
	if store.PathFor("proj-a") == store.PathFor("proj-b") {
		t.Fatal("two projects share one memory file")
	}

	itemsA, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List(proj-a): %v", err)
	}
	itemsB, err := store.List("proj-b")
	if err != nil {
		t.Fatalf("List(proj-b): %v", err)
	}
	if len(itemsA) != 1 || len(itemsB) != 1 {
		t.Fatalf("expected one item per project, got %d and %d", len(itemsA), len(itemsB))
	}
	if itemsA[0].Project != "proj-a" || itemsB[0].Project != "proj-b" {
		t.Fatalf("items leaked across projects: %q / %q", itemsA[0].Project, itemsB[0].Project)
	}

	// Changing one project's fact must not be visible from the other.
	if _, err := store.Upsert(ctx, sampleItem("proj-a", "note-1", "only project A learned this")); err != nil {
		t.Fatalf("Upsert(proj-a): %v", err)
	}
	itemsB, err = store.List("proj-b")
	if err != nil {
		t.Fatalf("List(proj-b): %v", err)
	}
	if len(itemsB) != 1 || itemsB[0].Content != "shared-looking fact" {
		t.Fatalf("project B changed when project A was updated: %+v", itemsB)
	}
	if _, found, err := store.Get("proj-b", itemsA[0].ID); err != nil || found {
		t.Fatalf("project A's item is readable from project B (found=%v, err=%v)", found, err)
	}
}

func TestUpsertRejectsInvalidItems(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	cases := map[string]MemoryItem{
		"no project":        {Type: TypeNote, Content: "x", Source: Source{Kind: SourceManual}},
		"no type":           {Project: "proj-a", Content: "x", Source: Source{Kind: SourceManual}},
		"no content":        {Project: "proj-a", Type: TypeNote, Source: Source{Kind: SourceManual}},
		"bad confidence":    {Project: "proj-a", Type: TypeNote, Content: "x", Confidence: 1.5},
		"stale without why": {Project: "proj-a", Type: TypeNote, Content: "x", Stale: true, StaleReason: "  "},
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Upsert(ctx, item); !errors.Is(err, ErrItemInvalid) {
				t.Fatalf("Upsert(%s) = %v, want ErrItemInvalid", name, err)
			}
		})
	}
}
