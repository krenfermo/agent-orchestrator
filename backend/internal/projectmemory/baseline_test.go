package projectmemory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// writeBaselineEvidence produces real evidence files through the recording
// side's own recorder and sink. Nothing here reimplements the evidence format:
// if that package changes what it writes, this test ingests the change.
func writeBaselineEvidence(t *testing.T, repo string) string {
	t.Helper()
	dir := t.TempDir()
	sink, err := baseline.NewDirSink(dir)
	if err != nil {
		t.Fatalf("NewDirSink: %v", err)
	}
	clock := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	ticks := 0
	ids := 0
	recorder := baseline.NewRecorder(sink,
		baseline.WithClock(func() time.Time {
			t := clock.Add(time.Duration(ticks) * 250 * time.Millisecond)
			ticks++
			return t
		}),
		baseline.WithIDs(func() string {
			ids++
			return "pmb-test-" + string(rune('0'+ids))
		}),
	)

	planner := recorder.Begin(baseline.Dispatch{
		Role:          domain.WorkflowRolePlanner,
		WorkflowRunID: "wf-1",
		Harness:       "claude-code",
		Observable:    baseline.Capabilities{FileReads: true, ContextPayload: true, SourceScope: true},
	})
	planner.ObserveFileRead(filepath.Join(repo, "AGENTS.md"), 400)
	planner.ObserveFileRead("docs/architecture.md", 200)
	planner.ObserveFileRead(filepath.Join(repo, "AGENTS.md"), 400)
	planner.ObserveContextSent("planner context document")
	planner.ObserveSourceScope(baseline.SourceScan{Files: 12, Bytes: 40000})
	if _, _, err := planner.Finish(context.Background(), nil); err != nil {
		t.Fatalf("planner Finish: %v", err)
	}

	worker := recorder.Begin(baseline.Dispatch{
		Role:          domain.WorkflowRoleWorker,
		WorkflowRunID: "wf-1",
		Harness:       "claude-code",
		Observable:    baseline.Capabilities{ContextPayload: true},
	})
	worker.ObserveContextSent("worker prompt")
	if _, _, err := worker.Finish(context.Background(), nil); err != nil {
		t.Fatalf("worker Finish: %v", err)
	}
	return dir
}

// baselineRepo is a checkout holding the files the evidence above says were
// read, so the ingested items can carry real file hashes.
func baselineRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# agents\n"), 0o600); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o700); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "architecture.md"), []byte("# architecture\n"), 0o600); err != nil {
		t.Fatalf("write docs/architecture.md: %v", err)
	}
	return repo
}

func TestBaselineReaderRootIsUnderAODataDir(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AO_DATA_DIR", dataDir)
	reader, err := NewDefaultBaselineReader()
	if err != nil {
		t.Fatalf("NewDefaultBaselineReader: %v", err)
	}
	want := filepath.Join(dataDir, "project-memory", "baseline")
	if reader.Root() != want {
		t.Fatalf("baseline root = %q, want %q", reader.Root(), want)
	}
}

func TestBaselineReaderToleratesAnEmptyEvidenceTree(t *testing.T) {
	reader, err := NewBaselineReader(filepath.Join(t.TempDir(), "never-written"))
	if err != nil {
		t.Fatalf("NewBaselineReader: %v", err)
	}
	records, err := reader.Records(context.Background())
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("an unwritten evidence tree yielded %d records", len(records))
	}
}

func TestIngestBaselineEvidenceIsIdempotentAndCarriesProvenance(t *testing.T) {
	repo := baselineRepo(t)
	reader, err := NewBaselineReader(writeBaselineEvidence(t, repo))
	if err != nil {
		t.Fatalf("NewBaselineReader: %v", err)
	}
	store, err := NewStore(t.TempDir(), WithClock(fixedClock(time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()
	opts := IngestOptions{Project: "proj-a", Repo: repo, SourceCommit: "c1"}

	first, err := reader.Ingest(ctx, store, opts)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if first.Records != 2 {
		t.Fatalf("read %d evidence records, want 2", first.Records)
	}
	if first.Upsert.Created == 0 {
		t.Fatalf("ingest created nothing: %+v", first.Upsert)
	}

	items, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var dispatch, fileUsage []MemoryItem
	for _, item := range items {
		if item.Source.Kind != SourceBaselineEvidence {
			t.Fatalf("item %s did not record where it came from: %+v", item.ID, item.Source)
		}
		if item.SourceCommit != "c1" {
			t.Fatalf("item %s carries source commit %q, want c1", item.ID, item.SourceCommit)
		}
		switch item.Type {
		case TypeBaselineDispatch:
			dispatch = append(dispatch, item)
		case TypeFileUsage:
			fileUsage = append(fileUsage, item)
		default:
			t.Fatalf("unexpected item type %q", item.Type)
		}
	}
	if len(dispatch) != 2 {
		t.Fatalf("got %d dispatch items, want one per evidence record", len(dispatch))
	}
	if len(fileUsage) != 2 {
		t.Fatalf("got %d file-usage items, want one per inspected path", len(fileUsage))
	}

	var planner MemoryItem
	for _, item := range dispatch {
		if item.Scope == "dispatch/"+string(domain.WorkflowRolePlanner) {
			planner = item
		}
	}
	if planner.ID == "" {
		t.Fatalf("no planner dispatch item among %+v", dispatch)
	}
	if !strings.Contains(planner.Content, "context.filesInspected: 2 (measured)") {
		t.Fatalf("planner content lost the measured file count:\n%s", planner.Content)
	}
	if !strings.Contains(planner.Content, "providerTokens.prompt: unavailable (") {
		t.Fatalf("an unavailable metric must stay labelled as unavailable:\n%s", planner.Content)
	}
	if planner.Confidence <= 0 || planner.Confidence > 1 {
		t.Fatalf("planner confidence %v is not in (0,1]", planner.Confidence)
	}

	byPath := map[string]MemoryItem{}
	for _, item := range fileUsage {
		byPath[item.Source.Path] = item
	}
	agents, ok := byPath["AGENTS.md"]
	if !ok {
		t.Fatalf("an absolute evidence path was not relativised to the checkout: %+v", byPath)
	}
	if agents.Source.FileHash == "" {
		t.Fatal("a file-usage item must record the hash it was read at")
	}
	if agents.Scope != "." {
		t.Fatalf("a root-level file should scope to \".\", got %q", agents.Scope)
	}
	if docs, ok := byPath["docs/architecture.md"]; !ok || docs.Scope != "docs" {
		t.Fatalf("docs item scoped to %q (found=%v), want \"docs\"", docs.Scope, ok)
	}

	// Re-ingesting the same evidence at the same commit must change nothing.
	second, err := reader.Ingest(ctx, store, opts)
	if err != nil {
		t.Fatalf("Ingest (again): %v", err)
	}
	if second.Upsert.Created != 0 || second.Upsert.Updated != 0 {
		t.Fatalf("re-ingesting unchanged evidence = %+v, want everything unchanged", second.Upsert)
	}
	if second.Upsert.Unchanged != len(items) {
		t.Fatalf("re-ingest reported %d unchanged, want %d", second.Upsert.Unchanged, len(items))
	}
	after, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List (again): %v", err)
	}
	if len(after) != len(items) {
		t.Fatalf("re-ingest changed the row count: %d -> %d", len(items), len(after))
	}
	for i := range items {
		if !after[i].UpdatedAt.Equal(items[i].UpdatedAt) {
			t.Fatalf("re-ingest moved UpdatedAt on %s: %s -> %s", items[i].ID, items[i].UpdatedAt, after[i].UpdatedAt)
		}
	}
}

func TestIngestedBaselineItemsGoStaleWhenTheirFileChanges(t *testing.T) {
	repo := baselineRepo(t)
	reader, err := NewBaselineReader(writeBaselineEvidence(t, repo))
	if err != nil {
		t.Fatalf("NewBaselineReader: %v", err)
	}
	store, err := NewStore(t.TempDir(), WithClock(fixedClock(time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()
	if _, err := reader.Ingest(ctx, store, IngestOptions{Project: "proj-a", Repo: repo, SourceCommit: "c1"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	git := fakeGit{head: "head", known: map[string]string{"c1": "c1"}, reachable: map[string]bool{"c1": true}}
	check := StaleCheck{Repo: repo, Git: git, Hash: HashFile}
	if result, err := store.RefreshStaleness(ctx, "proj-a", check); err != nil {
		t.Fatalf("RefreshStaleness: %v", err)
	} else if result.MarkedStale != 0 {
		t.Fatalf("nothing moved yet, but %d items went stale", result.MarkedStale)
	}

	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# agents\n\n# edited\n"), 0o600); err != nil {
		t.Fatalf("edit AGENTS.md: %v", err)
	}
	result, err := store.RefreshStaleness(ctx, "proj-a", check)
	if err != nil {
		t.Fatalf("RefreshStaleness (after edit): %v", err)
	}
	if result.MarkedStale != 1 {
		t.Fatalf("editing one file marked %d items stale, want 1", result.MarkedStale)
	}
	items, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range items {
		if item.Source.Path == "AGENTS.md" {
			if !item.Stale || !strings.Contains(item.StaleReason, "changed") {
				t.Fatalf("the item read from the edited file is not stale: %+v", item)
			}
			continue
		}
		if item.Stale {
			t.Fatalf("an unrelated item went stale: %+v", item)
		}
	}
}

func TestIngestKeepsTwoProjectsApart(t *testing.T) {
	repo := baselineRepo(t)
	reader, err := NewBaselineReader(writeBaselineEvidence(t, repo))
	if err != nil {
		t.Fatalf("NewBaselineReader: %v", err)
	}
	store, err := NewStore(t.TempDir(), WithClock(fixedClock(time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()

	a, err := reader.Ingest(ctx, store, IngestOptions{Project: "proj-a", Repo: repo, SourceCommit: "c1"})
	if err != nil {
		t.Fatalf("Ingest(proj-a): %v", err)
	}
	b, err := reader.Ingest(ctx, store, IngestOptions{Project: "proj-b", Repo: repo, SourceCommit: "c1"})
	if err != nil {
		t.Fatalf("Ingest(proj-b): %v", err)
	}
	if b.Upsert.Created != a.Upsert.Created {
		t.Fatalf("the same evidence created %d items for project B and %d for project A", b.Upsert.Created, a.Upsert.Created)
	}
	if b.Upsert.Unchanged != 0 {
		t.Fatalf("project B reused %d of project A's rows", b.Upsert.Unchanged)
	}

	itemsA, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List(proj-a): %v", err)
	}
	itemsB, err := store.List("proj-b")
	if err != nil {
		t.Fatalf("List(proj-b): %v", err)
	}
	if len(itemsA) != len(itemsB) || len(itemsA) == 0 {
		t.Fatalf("expected the same non-zero item count per project, got %d and %d", len(itemsA), len(itemsB))
	}
	idsA := map[string]bool{}
	for _, item := range itemsA {
		if item.Project != "proj-a" {
			t.Fatalf("project A holds an item for %q", item.Project)
		}
		idsA[item.ID] = true
	}
	for _, item := range itemsB {
		if item.Project != "proj-b" {
			t.Fatalf("project B holds an item for %q", item.Project)
		}
		if idsA[item.ID] {
			t.Fatalf("identity %s is shared by both projects", item.ID)
		}
	}
}

func TestIngestUsesTheRecordsOwnProjectWhenItHasOne(t *testing.T) {
	repo := baselineRepo(t)
	dir := t.TempDir()
	sink, err := baseline.NewDirSink(dir)
	if err != nil {
		t.Fatalf("NewDirSink: %v", err)
	}
	recorder := baseline.NewRecorder(sink)
	span := recorder.Begin(baseline.Dispatch{
		Role:       domain.WorkflowRoleReviewer,
		ProjectID:  "proj-from-record",
		Harness:    "claude-code",
		Observable: baseline.Capabilities{ContextPayload: true},
	})
	span.ObserveContextSent("review prompt")
	if _, _, err := span.Finish(context.Background(), nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	reader, err := NewBaselineReader(dir)
	if err != nil {
		t.Fatalf("NewBaselineReader: %v", err)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := reader.Ingest(context.Background(), store, IngestOptions{Project: "fallback", Repo: repo}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	items, err := store.List("proj-from-record")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items under the record's own project, want 1", len(items))
	}
	if items[0].SourceCommit != "" {
		t.Fatalf("with no commit supplied and no git, provenance must stay empty, got %q", items[0].SourceCommit)
	}
	fallback, err := store.List("fallback")
	if err != nil {
		t.Fatalf("List(fallback): %v", err)
	}
	if len(fallback) != 0 {
		t.Fatalf("the record's own project id was ignored: %d items filed under the fallback", len(fallback))
	}
}

func TestIngestRefusesARecordWithNoProject(t *testing.T) {
	repo := baselineRepo(t)
	reader, err := NewBaselineReader(writeBaselineEvidence(t, repo))
	if err != nil {
		t.Fatalf("NewBaselineReader: %v", err)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := reader.Ingest(context.Background(), store, IngestOptions{Repo: repo}); err == nil {
		t.Fatal("ingesting evidence with no project id and no fallback must fail rather than invent one")
	}
}
