package codegraph_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// index_test.go — the durable, project-scoped graph.
//
// These tests are about the properties a document store could not give:
// staging, CAS, in-place incremental update, exact deletion, and a no-op sync
// that costs nothing. Each is a rule from the brief, and each is the kind of
// rule that a single-threaded happy path would never notice was broken.

const testProject = domain.ProjectID("p1")

type fixture struct {
	t      *testing.T
	store  *sqlite.Store
	index  *codegraph.Index
	root   string
	repoID string
	ctx    context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project")
	mustWrite(t, root, "internal/service/records.go", serviceGo)
	mustWrite(t, root, "internal/service/records_test.go", serviceTestGo)
	mustWrite(t, root, "internal/store/records.go", storeGo)
	mustWrite(t, root, "README.md", "# not indexed\n")

	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(testProject), Path: root, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return &fixture{
		t: t, store: st, ctx: ctx, root: root,
		repoID: domain.ProjectMemoryRepoID(root),
		index:  codegraph.NewIndex(st),
	}
}

func (f *fixture) request(commit string) codegraph.SyncRequest {
	return codegraph.SyncRequest{
		ProjectID: testProject, RepoID: f.repoID, RepoPath: f.root,
		Commit: commit, Branch: "main", RepoIdentity: "origin/main",
	}
}

func (f *fixture) build(commit string) codegraph.SyncOutcome {
	f.t.Helper()
	out, err := f.index.Build(f.ctx, f.request(commit))
	if err != nil {
		f.t.Fatalf("Build: %v", err)
	}
	return out
}

func (f *fixture) apply(commit string, changes ...codegraph.FileChange) codegraph.SyncOutcome {
	f.t.Helper()
	out, err := f.index.Apply(f.ctx, f.request(commit), codegraph.Diff{Changes: changes})
	if err != nil {
		f.t.Fatalf("Apply: %v", err)
	}
	return out
}

func (f *fixture) state() store.CodeGraphState {
	f.t.Helper()
	state, found, err := f.index.Status(f.ctx, testProject, f.repoID)
	if err != nil || !found {
		f.t.Fatalf("Status: found=%v err=%v", found, err)
	}
	return state
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

const serviceGo = `package service

import "example.com/p/internal/store"

// Role names a principal's role.
type Role string

// Supervisor is the supervising role.
const Supervisor Role = "supervisor"

// Records is the record service.
type Records struct{ store *store.Records }

// MayExport decides whether a role may export records.
func (r *Records) MayExport(role Role) bool { return role == Supervisor }
`

const serviceTestGo = `package service

import "testing"

func TestRecordsMayExport(t *testing.T) {
	r := &Records{}
	if !r.MayExport(Supervisor) {
		t.Fatal("no")
	}
}
`

const storeGo = `package store

// Records persists records.
type Records struct{}
`

func TestBuildPublishesOneCompleteGenerationAndStagesTheNext(t *testing.T) {
	f := newFixture(t)

	first := f.build("commit-1")
	if first.Kind != store.CodeGraphSyncFull || first.FilesParsed != 3 {
		t.Fatalf("first build = %+v", first)
	}
	state := f.state()
	if state.ServedGeneration != 1 || state.Phase != store.CodeGraphIdle {
		t.Fatalf("state after first build = %+v", state)
	}
	if state.SymbolCount == 0 || state.EdgeCount == 0 || state.IndexedCommit != "commit-1" {
		t.Fatalf("published counts/provenance = %+v", state)
	}
	if !strings.Contains(state.Architecture, "internal/service") {
		t.Fatalf("architecture summary was not stored: %q", state.Architecture)
	}

	// A second full build over an unchanged tree parses nothing: every file's
	// content hash still matches, so each is carried forward by the database.
	second := f.build("commit-1")
	if second.FilesParsed != 0 || second.FilesReused != 3 {
		t.Fatalf("repeat build re-parsed files: %+v", second)
	}
	if got := f.state().ServedGeneration; got != 2 {
		t.Fatalf("served generation = %d, want 2", got)
	}
	// The generation it replaced is collected, not left behind.
	files, symbols, edges, err := f.store.CountCodeGraph(f.ctx, testProject, f.repoID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if files+symbols+edges != 0 {
		t.Fatalf("superseded generation still holds %d files, %d symbols, %d edges", files, symbols, edges)
	}
}

func TestConcurrentBuildsResolveToOneWinner(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")

	// Claim the pass out from under the second caller, exactly as a
	// concurrent dispatch would.
	if _, claimed, err := f.store.ClaimCodeGraphBuild(f.ctx, testProject, f.repoID, "commit-2", "main", time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	out, err := f.index.Build(f.ctx, f.request("commit-2"))
	if err != nil {
		t.Fatalf("Build while another pass holds the repository: %v", err)
	}
	if out.Kind != store.CodeGraphSyncNoop || out.Reason == "" {
		t.Fatalf("second builder did not stand down: %+v", out)
	}
	if got := f.state().ServedGeneration; got != 1 {
		t.Fatalf("a stood-down builder moved the served generation to %d", got)
	}
}

func TestIncrementalUpdateTouchesOnlyWhatTheDiffNames(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")
	before := f.state()

	mustWrite(t, f.root, "internal/service/records.go", strings.Replace(serviceGo,
		"// MayExport decides whether a role may export records.\nfunc (r *Records) MayExport(role Role) bool { return role == Supervisor }",
		"// MayExport now also allows an auditor to export within a scope.\nfunc (r *Records) MayExport(role Role, scope string) bool { return role == Supervisor }", 1))

	out := f.apply("commit-2", codegraph.FileChange{Status: codegraph.ChangeModified, Path: "internal/service/records.go"})
	if out.Kind != store.CodeGraphSyncIncremental {
		t.Fatalf("kind = %q", out.Kind)
	}
	if out.FilesScanned != 1 || out.FilesParsed != 1 {
		t.Fatalf("an incremental update looked beyond the diff: %+v", out)
	}
	after := f.state()
	if after.ServedGeneration != before.ServedGeneration {
		t.Fatalf("an in-place update moved the served generation: %d -> %d",
			before.ServedGeneration, after.ServedGeneration)
	}
	if after.IndexedCommit != "commit-2" {
		t.Fatalf("indexed commit = %q", after.IndexedCommit)
	}

	symbols, err := f.store.ListCodeGraphSymbolsForPath(f.ctx, testProject, f.repoID, after.ServedGeneration, "internal/service/records.go")
	if err != nil {
		t.Fatal(err)
	}
	var mayExport store.CodeGraphSymbolRecord
	for _, sym := range symbols {
		if sym.Name == "Records.MayExport" {
			mayExport = sym
		}
	}
	if !strings.Contains(mayExport.Signature, "scope string") {
		t.Fatalf("the changed signature did not reach the graph: %+v", mayExport)
	}
	if !strings.Contains(mayExport.Summary, "also allows an auditor") {
		t.Fatalf("the changed doc did not reach the summary: %q", mayExport.Summary)
	}

	// The blast radius is REPORTED, not chased: the test that calls it is
	// named, and nothing else was rebuilt.
	if len(out.AffectedSymbols) == 0 {
		t.Fatalf("no dependants reported for a changed exported method: %+v", out)
	}
}

func TestNoOpSyncDoesNoWork(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")

	// The commit moved but no indexed file did: a documentation-only commit,
	// a lockfile bump, a change under a skipped directory.
	out := f.apply("commit-2", codegraph.FileChange{Status: codegraph.ChangeModified, Path: "README.md"})
	if out.Kind != store.CodeGraphSyncNoop {
		t.Fatalf("kind = %q, want noop: %+v", out.Kind, out)
	}
	if out.FilesParsed != 0 || out.FilesRemoved != 0 {
		t.Fatalf("a no-op sync did work: %+v", out)
	}
	if got := f.state().IndexedCommit; got != "commit-2" {
		t.Fatalf("a no-op sync did not advance the commit: %q", got)
	}

	// A diff that names an indexed file whose bytes did not move is also a
	// no-op, and is counted as reuse rather than as a parse.
	reuse := f.apply("commit-3", codegraph.FileChange{Status: codegraph.ChangeModified, Path: "internal/store/records.go"})
	if reuse.FilesParsed != 0 || reuse.FilesReused != 1 {
		t.Fatalf("an unchanged file was re-parsed: %+v", reuse)
	}
}

func TestDeletionRemovesEverythingDerivedFromThePath(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")
	generation := f.state().ServedGeneration

	if err := os.Remove(filepath.Join(f.root, "internal", "store", "records.go")); err != nil {
		t.Fatal(err)
	}
	out := f.apply("commit-2", codegraph.FileChange{Status: codegraph.ChangeDeleted, Path: "internal/store/records.go"})
	if out.FilesRemoved != 1 || out.SymbolsRemoved == 0 {
		t.Fatalf("deletion did not remove the path's facts: %+v", out)
	}

	symbols, err := f.store.ListCodeGraphSymbolsForPath(f.ctx, testProject, f.repoID, generation, "internal/store/records.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 0 {
		t.Fatalf("zombie symbols survived a deletion: %+v", symbols)
	}
	if _, found, err := f.store.GetCodeGraphFileRecord(f.ctx, testProject, f.repoID, generation, "internal/store/records.go"); err != nil || found {
		t.Fatalf("file ledger entry survived: found=%v err=%v", found, err)
	}
}

func TestRenameIsADeleteAndACreateWithTheNewPathsIdentity(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")
	generation := f.state().ServedGeneration

	if err := os.MkdirAll(filepath.Join(f.root, "internal", "persistence"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(
		filepath.Join(f.root, "internal", "store", "records.go"),
		filepath.Join(f.root, "internal", "persistence", "records.go"),
	); err != nil {
		t.Fatal(err)
	}
	out := f.apply("commit-2", codegraph.FileChange{
		Status: codegraph.ChangeRenamed, OldPath: "internal/store/records.go", Path: "internal/persistence/records.go",
	})
	if out.FilesRemoved != 1 || out.FilesParsed != 1 {
		t.Fatalf("rename outcome = %+v", out)
	}

	old, err := f.store.ListCodeGraphSymbolsForPath(f.ctx, testProject, f.repoID, generation, "internal/store/records.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 0 {
		t.Fatalf("the old path kept its symbols: %+v", old)
	}
	moved, err := f.store.ListCodeGraphSymbolsForPath(f.ctx, testProject, f.repoID, generation, "internal/persistence/records.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) == 0 {
		t.Fatal("the new path has no symbols")
	}
	for _, sym := range moved {
		if !strings.HasPrefix(sym.SymbolID, "internal/persistence/records.go#") {
			t.Fatalf("symbol identity did not move with the file: %+v", sym)
		}
	}
}

func TestRetrieveAnswersFromIndexedReads(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")

	got, err := f.index.Retrieve(f.ctx, testProject, f.repoID, codegraph.RetrieveRequest{
		Terms: []string{"export permissions supervisor"},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	names := map[string]bool{}
	for _, s := range got.Symbols {
		names[s.Symbol.Name] = true
	}
	if !names["Records.MayExport"] || !names["Supervisor"] {
		t.Fatalf("retrieval missed the authorization path: %+v", got.Symbols)
	}
	covering := false
	for _, test := range got.Tests {
		if test.Name == "TestRecordsMayExport" {
			covering = true
		}
	}
	if !covering {
		t.Fatalf("the covering test was not retrieved: %+v", got.Tests)
	}
	if got.ConsideredSymbols == 0 {
		t.Fatal("retrieval reported nothing considered, so its contribution cannot be measured")
	}
}

func TestRetrieveOnAnUnindexedProjectIsEmptyNotAnError(t *testing.T) {
	f := newFixture(t)
	got, err := f.index.Retrieve(f.ctx, testProject, f.repoID, codegraph.RetrieveRequest{Terms: []string{"anything"}})
	if err != nil {
		t.Fatalf("Retrieve before any build: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("an unindexed project returned evidence: %+v", got)
	}
}

func TestBuildResumesAStagingGenerationLeftByACrash(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")

	// Simulate a crash mid-rebuild: the generation is claimed and one path has
	// been staged, then nothing more happens.
	crashedAt := time.Now().UTC().Add(-2 * time.Hour)
	state, claimed, err := f.store.ClaimCodeGraphBuild(f.ctx, testProject, f.repoID, "commit-2", "main", crashedAt)
	if err != nil || !claimed {
		t.Fatalf("claim: %v", err)
	}
	if _, err := f.store.CopyCodeGraphPathForward(f.ctx, testProject, f.repoID,
		state.ServedGeneration, state.Generation, "internal/store/records.go"); err != nil {
		t.Fatal(err)
	}

	// Readers still see the previous complete generation while the staged one
	// is half-built. That is the whole point of staging.
	if got := f.state().ServedGeneration; got != state.ServedGeneration {
		t.Fatalf("a partial build became visible: served generation %d", got)
	}
	before, err := f.index.Retrieve(f.ctx, testProject, f.repoID, codegraph.RetrieveRequest{Terms: []string{"supervisor"}})
	if err != nil || before.Empty() {
		t.Fatalf("the previous generation stopped answering during a rebuild: %+v err=%v", before, err)
	}

	out := f.build("commit-2")
	if out.Kind != store.CodeGraphSyncFull {
		t.Fatalf("the resumed build did not run: %+v", out)
	}
	// The path already staged is reused rather than re-parsed: that is what
	// "resume" means here.
	if out.FilesReused == 0 {
		t.Fatalf("the resumed build re-did work the crashed one had finished: %+v", out)
	}
	after := f.state()
	if after.ServedGeneration != state.Generation || after.Phase != store.CodeGraphIdle {
		t.Fatalf("state after resume = %+v", after)
	}
}

func TestAnalyzeChangedNeverWritesToTheCanonicalGraph(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")
	before := f.state()

	// A task worktree with a provisional file that is not in the repository.
	worktree := filepath.Join(t.TempDir(), "worktree")
	mustWrite(t, worktree, "internal/service/exporter.go", `package service

// Exporter writes an export bundle. It does not exist in the repository yet.
type Exporter struct{}

// Run performs the export.
func (e *Exporter) Run() error { return nil }
`)

	got, err := f.index.AnalyzeChanged(f.ctx, worktree, []string{"internal/service/exporter.go"}, codegraph.RetrieveRequest{})
	if err != nil {
		t.Fatalf("AnalyzeChanged: %v", err)
	}
	names := map[string]bool{}
	for _, s := range got.Symbols {
		names[s.Symbol.Name] = true
	}
	if !names["Exporter"] || !names["Exporter.Run"] {
		t.Fatalf("the task's own changes were not analysed: %+v", got.Symbols)
	}

	after := f.state()
	if after.ServedGeneration != before.ServedGeneration || after.SymbolCount != before.SymbolCount {
		t.Fatalf("a worktree analysis touched canonical state: %+v -> %+v", before, after)
	}
	canonical, err := f.index.Retrieve(f.ctx, testProject, f.repoID, codegraph.RetrieveRequest{Terms: []string{"exporter"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range canonical.Symbols {
		if s.Symbol.Name == "Exporter" {
			t.Fatalf("a worktree symbol was promoted into the canonical graph: %+v", s)
		}
	}
}

func TestArchitectureIsServedFromOneRow(t *testing.T) {
	f := newFixture(t)
	f.build("commit-1")

	rendered, arch, ok, err := f.index.Architecture(f.ctx, testProject, f.repoID)
	if err != nil || !ok {
		t.Fatalf("Architecture: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rendered, "internal/service") {
		t.Fatalf("rendered architecture = %q", rendered)
	}
	if arch.Files == 0 || arch.Symbols == 0 || arch.IndexedCommit != "commit-1" {
		t.Fatalf("structured architecture = %+v", arch)
	}
}
