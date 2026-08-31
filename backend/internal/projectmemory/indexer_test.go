package projectmemory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// indexer_test.go — the bounded pass, against real trees on disk.
//
// The fixtures are deliberately small but *shaped* like the repositories AO
// indexes: a Go backend with a manifest and an instruction file, a frontend
// with a package.json, a multi-repo project, and a tree full of the things the
// bounds are supposed to keep out.

const testProject = domain.ProjectID("proj")

type fixture struct {
	t     *testing.T
	store *sqlite.Store
	idx   *projectmemory.Indexer
	ctx   context.Context
}

// now is the fixture's clock, so a test that needs a timestamp uses the same
// one the store does.
func (f *fixture) now() time.Time { return time.Now().UTC() }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(testProject), Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, store: st, ctx: ctx, idx: projectmemory.NewIndexer(st, nil)}
}

// writeTree materialises a map of repo-relative path to content, creating
// parent directories as needed.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func goRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.24\n\nrequire (\n\tgithub.com/spf13/cobra v1.8.0\n\tgithub.com/stretchr/testify v1.9.0\n)\n",
		"AGENTS.md": "# AGENTS.md\n\nGuidance for agents.\n\n## Coding conventions\n\n" +
			"Keep every change surgical.\n\n## Hard rules\n\nNever write outside the data dir.\n",
		"README.md":                "# App\n\nA small service that does one thing well.\n",
		"docs/architecture.md":     "# Architecture\n\nThe daemon owns all state.\n",
		"cmd/app/main.go":          "// Command app is the entry point.\npackage main\n\nimport (\n\t\"example.com/app/internal/server\"\n)\n\nfunc main() { server.Run() }\n",
		"internal/server/serve.go": "package server\n\nimport (\n\t\"example.com/app/internal/store\"\n)\n\nfunc Run() { store.Open() }\n",
		"internal/store/store.go":  "package store\n\nfunc Open() {}\n",
		"internal/store/query.go":  "package store\n\nfunc Query() {}\n",
	})
	return root
}

func TestIndexNormalGoRepositoryDerivesTheExpectedFacts(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)

	out, err := f.idx.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if out.Skipped {
		t.Fatalf("first index was skipped: %s", out.SkipReason)
	}
	if out.Generation != 1 {
		t.Fatalf("generation = %d, want 1", out.Generation)
	}
	if out.FilesAdmitted != 8 {
		t.Fatalf("admitted %d files, want 8: %+v", out.FilesAdmitted, out)
	}

	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, out.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[domain.ProjectMemoryType]int{}
	for _, it := range items {
		byType[it.Key.Type]++
		if it.SourceCommit != "c1" {
			t.Fatalf("item %s carries commit %q, want c1", it.Key, it.SourceCommit)
		}
		if it.Generation != out.Generation {
			t.Fatalf("item %s at generation %d, want %d", it.Key, it.Generation, out.Generation)
		}
	}
	for _, want := range []domain.ProjectMemoryType{
		domain.MemoryTypeProjectOverview,
		domain.MemoryTypeArchitecture,
		domain.MemoryTypeModule,
		domain.MemoryTypeDependency,
		domain.MemoryTypeInstruction,
		domain.MemoryTypeConvention,
	} {
		if byType[want] == 0 {
			t.Errorf("no %s item was derived; got %+v", want, byType)
		}
	}
	// The bound that matters: ordinary source files must NOT each become an
	// item. Four .go files are in the tree; only the entry point is notable.
	if byType[domain.MemoryTypeFileSummary] > 1 {
		t.Errorf("derived %d file summaries from 4 source files; the index is not bounded",
			byType[domain.MemoryTypeFileSummary])
	}
}

func TestIndexDerivesModuleDependencyEdges(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)

	out, err := f.idx.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1",
	})
	if err != nil {
		t.Fatal(err)
	}
	graph := projectmemory.NewLocalGraph(f.store)
	edges, err := graph.Neighbors(f.ctx, projectmemory.GraphQuery{
		ProjectID: testProject, RepoID: out.RepoID,
		Node:  domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: "internal/server"},
		Kinds: []domain.ProjectMemoryRelationKind{domain.RelationDependsOn},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].To.Key != "internal/store" {
		t.Fatalf("internal/server edges = %+v, want one depends_on internal/store", edges)
	}
}

func TestIndexIsBoundedByLimits(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	files := map[string]string{
		"node_modules/left-pad/index.js": "module.exports = 1",
		"vendor/dep/dep.go":              "package dep",
		"dist/bundle.js":                 "console.log(1)",
		"assets/logo.png":                "not really a png but the extension is ignored",
		"big.txt":                        strings.Repeat("x", 4096),
		"src/a.go":                       "package src",
	}
	files["bin.dat2"] = "head\x00tail" // binary sniff, not extension
	writeTree(t, root, files)

	out, err := f.idx.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1",
		Limits: projectmemory.IndexLimits{MaxFileBytes: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.FilesAdmitted != 1 {
		t.Fatalf("admitted %d files, want only src/a.go: %+v", out.FilesAdmitted, out)
	}
	ledger, err := f.store.ListProjectMemoryFiles(f.ctx, testProject, out.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 1 || ledger[0].Path != "src/a.go" {
		t.Fatalf("ledger = %+v, want only src/a.go", ledger)
	}
}

func TestIndexTwiceReconfirmsRatherThanRewrites(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	req := projectmemory.IndexRequest{ProjectID: testProject, RepoPath: root, Commit: "c1"}

	first, err := f.idx.Index(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.idx.Index(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.FilesSkipped != first.FilesAdmitted {
		t.Fatalf("second pass skipped %d of %d unchanged files", second.FilesSkipped, first.FilesAdmitted)
	}
	if second.ItemsWritten != 0 {
		t.Fatalf("second pass wrote %d items over an unchanged tree; expected only reconfirmations", second.ItemsWritten)
	}
	if second.ItemsReconfirmed == 0 {
		t.Fatal("second pass reconfirmed nothing")
	}
	if second.ItemsRetired != 0 {
		t.Fatalf("second pass retired %d items over an unchanged tree", second.ItemsRetired)
	}
	status, _, err := f.store.GetProjectMemoryStatus(f.ctx, testProject, second.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Counts.Stale != 0 || status.Counts.Invalidated != 0 {
		t.Fatalf("an unchanged re-index produced non-valid items: %+v", status.Counts)
	}
}

func TestIndexDetectsDeletionThroughTheLedger(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	req := projectmemory.IndexRequest{ProjectID: testProject, RepoPath: root, Commit: "c1"}

	first, err := f.idx.Index(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "docs", "architecture.md")); err != nil {
		t.Fatal(err)
	}
	req.Commit = "c2"
	second, err := f.idx.Index(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.FilesRemoved != 1 {
		t.Fatalf("removed %d files, want 1", second.FilesRemoved)
	}

	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, first.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Key.Key == "docs/architecture.md" {
			if it.State != domain.MemoryStateInvalidated {
				t.Fatalf("the deleted document's fact is still %s", it.State)
			}
			if !strings.Contains(it.StateReason, "no longer present") {
				t.Fatalf("invalidation reason = %q", it.StateReason)
			}
			return
		}
	}
	t.Fatal("no fact for the deleted document was found at all")
}

func TestIndexEmptyProjectSucceedsWithNoFacts(t *testing.T) {
	f := newFixture(t)
	out, err := f.idx.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: t.TempDir(), Commit: "",
	})
	if err != nil {
		t.Fatalf("indexing an empty project failed: %v", err)
	}
	if out.FilesAdmitted != 0 {
		t.Fatalf("admitted %d files from an empty tree", out.FilesAdmitted)
	}
	status, ok, err := f.store.GetProjectMemoryStatus(f.ctx, testProject, out.RepoID)
	if err != nil || !ok {
		t.Fatalf("status: ok=%v err=%v", ok, err)
	}
	// An empty project still gets an overview: "this repository has nothing in
	// it" is a fact, and it is one a Planner benefits from knowing.
	if status.Counts.Valid != 1 {
		t.Fatalf("valid items = %d, want the overview only: %+v", status.Counts.Valid, status.ByType)
	}
	if status.Index.Phase != domain.IndexPhaseIdle {
		t.Fatalf("phase = %q after a completed pass", status.Index.Phase)
	}
}

func TestIndexMultiRepoProjectKeepsMemoriesIsolated(t *testing.T) {
	f := newFixture(t)
	backend := goRepo(t)
	frontend := t.TempDir()
	writeTree(t, frontend, map[string]string{
		"package.json": `{"name":"web","scripts":{"build":"vite build","test":"vitest"},` +
			`"dependencies":{"react":"^18.0.0"},"devDependencies":{"vite":"^5.0.0"}}`,
		"src/index.tsx":     "import { App } from './app'\nexport default App\n",
		"src/app.tsx":       "export const App = () => null\n",
		"src/lib/format.ts": "export const fmt = (s: string) => s\n",
		"tsconfig.json":     `{"compilerOptions":{"strict":true}}`,
		"README.md":         "# Web\n\nThe browser client.\n",
	})

	back, err := f.idx.Index(f.ctx, projectmemory.IndexRequest{ProjectID: testProject, RepoPath: backend, Commit: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	front, err := f.idx.Index(f.ctx, projectmemory.IndexRequest{ProjectID: testProject, RepoPath: frontend, Commit: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if back.RepoID == front.RepoID {
		t.Fatal("two repositories resolved to one identity")
	}

	frontItems, err := f.store.ListProjectMemoryItems(f.ctx, testProject, front.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	sawBuild := false
	for _, it := range frontItems {
		if it.Key.RepoID != front.RepoID {
			t.Fatalf("item %s leaked from another repository", it.Key)
		}
		if it.Key.Type == domain.MemoryTypeBuildTest && strings.Contains(it.Content, "npm run build") {
			sawBuild = true
		}
	}
	if !sawBuild {
		t.Error("the frontend's npm scripts were not captured as a build/test fact")
	}

	// The project-wide read spans both; the per-repository read does not.
	all, err := f.store.ListProjectMemoryItemsForProject(f.ctx, testProject)
	if err != nil {
		t.Fatal(err)
	}
	repos := map[string]struct{}{}
	for _, it := range all {
		repos[it.Key.RepoID] = struct{}{}
	}
	if len(repos) != 2 {
		t.Fatalf("project-wide read spans %d repositories, want 2", len(repos))
	}
}

func TestIndexResumesAfterACrashedPass(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)

	// Simulate a pass that claimed a generation and died before completing:
	// the row is left non-terminal with a cursor.
	repoID := domain.ProjectMemoryRepoID(mustEval(t, root))
	if err := f.store.EnsureProjectMemoryRepo(f.ctx, testProject, repoID, mustEval(t, root), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := f.store.ClaimProjectMemoryIndexPass(f.ctx, testProject, repoID, "c1", "main", time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	claimed.Cursor = "cmd/app/main.go"
	claimed.Phase = domain.IndexPhaseScanning
	if ok, err := f.store.AdvanceProjectMemoryIndexPass(f.ctx, claimed, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("seed cursor: ok=%v err=%v", ok, err)
	}

	out, err := f.idx.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !out.Resumed {
		t.Fatal("the pass did not resume the in-flight generation")
	}
	if out.Generation != claimed.Generation {
		t.Fatalf("resume took generation %d, want the abandoned %d", out.Generation, claimed.Generation)
	}
	// Paths at or before the cursor were already derived by the dead pass and
	// must not be re-derived; paths after it must be.
	if out.FilesSkipped == 0 {
		t.Fatal("the resumed pass re-derived everything, ignoring the cursor")
	}
	if out.FilesIndexed == 0 {
		t.Fatal("the resumed pass derived nothing after the cursor")
	}
	status, _, err := f.store.GetProjectMemoryStatus(f.ctx, testProject, out.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Index.Phase != domain.IndexPhaseIdle || status.Index.IndexedCommit != "c1" {
		t.Fatalf("resumed pass did not complete: %+v", status.Index)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
