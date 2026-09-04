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
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// smoke_test.go — section 37: does this work on a real project.
//
// Synthetic fixtures prove the mechanics. What they cannot prove is that a
// question phrased the way a person phrases it finds the right code in a
// repository nobody wrote for the test. So this runs against a real checkout,
// READ-ONLY: the graph is written to an isolated database in a temp directory,
// nothing under the repository is touched, and the incremental step operates on
// a copy.
//
// Opt-in, because it indexes thousands of files:
//
//	AO_CODEGRAPH_SMOKE_REPO=/path/to/checkout go test ./internal/codegraph/ -run RealProject -v
//
// Pointing it at AO's own checkout is the intended use, and is what the
// numbers in docs/code-graph.md were measured on.

func TestRealProjectSmoke(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("AO_CODEGRAPH_SMOKE_REPO"))
	if root == "" {
		t.Skip("set AO_CODEGRAPH_SMOKE_REPO to a checkout to run the real-project smoke")
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("AO_CODEGRAPH_SMOKE_REPO=%q is not a directory", root)
	}

	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	const project = domain.ProjectID("smoke")
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(project), Path: root, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	index := codegraph.NewIndex(st)
	req := codegraph.SyncRequest{
		ProjectID: project, RepoID: domain.ProjectMemoryRepoID(root), RepoPath: root,
		Commit: "smoke-1", Branch: "main",
	}

	// 1. First sync: the graph is created.
	start := time.Now()
	first, err := index.Build(ctx, req)
	if err != nil {
		t.Fatalf("initial build: %v", err)
	}
	full := time.Since(start)
	t.Logf("initial index: %d files, %d symbols, %d relations in %s (%d parsed, %d reused)",
		first.Files, first.Symbols, first.Edges, full, first.FilesParsed, first.FilesReused)
	if first.Symbols == 0 {
		t.Fatal("a real repository produced no symbols")
	}

	// 2. The architecture summary, derived rather than described.
	rendered, arch, ok, err := index.Architecture(ctx, project, req.RepoID)
	if err != nil || !ok {
		t.Fatalf("architecture: ok=%v err=%v", ok, err)
	}
	t.Logf("architecture summary (%d bytes of a %d-byte cap):\n%s",
		len(rendered), codegraph.MaxArchitectureBytes, rendered)
	if len(rendered) > codegraph.MaxArchitectureBytes {
		t.Fatalf("architecture summary is %d bytes, over its cap", len(rendered))
	}
	if len(arch.Modules) == 0 {
		t.Fatal("the architecture summary names no modules")
	}

	// 3. A task phrased the way a person would phrase it, with no architecture
	//    explained and no paths given.
	const objective = "Identify the authorization path for cancelling a workflow and the tests that cover it."
	start = time.Now()
	got, err := index.Retrieve(ctx, project, req.RepoID, codegraph.RetrieveRequest{
		Terms: []string{objective},
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	t.Logf("retrieval for %q: %d symbols selected from %d considered, %d covering tests, %d files, in %s",
		objective, got.SelectedSymbols(), got.ConsideredSymbols, len(got.Tests), len(got.Files), time.Since(start))
	for _, s := range got.Symbols {
		t.Logf("  %s:%d  %s", s.Symbol.File, s.Symbol.Line, s.Symbol.Summary)
	}
	for _, test := range got.Tests {
		t.Logf("  covered by %s:%d %s", test.File, test.Line, test.Name)
	}
	if got.Empty() {
		t.Fatal("the retrieval found nothing for a plainly-phrased objective")
	}
	if got.ConsideredSymbols <= got.SelectedSymbols() {
		t.Fatalf("considered %d and selected %d: the retrieval bounded nothing",
			got.ConsideredSymbols, got.SelectedSymbols())
	}

	// 4. A no-op sync: the commit did not move and no file changed.
	start = time.Now()
	noop, err := index.Apply(ctx, req, codegraph.Diff{})
	if err != nil {
		t.Fatalf("no-op sync: %v", err)
	}
	noopDuration := time.Since(start)
	t.Logf("no-op sync: %s (%d files scanned, %d parsed)", noopDuration, noop.FilesScanned, noop.FilesParsed)
	if noop.Kind != store.CodeGraphSyncNoop || noop.FilesParsed != 0 || noop.FilesScanned != 0 {
		t.Fatalf("a no-op sync did work: %+v", noop)
	}
	if noopDuration*20 > full {
		t.Fatalf("a no-op sync (%s) is not dramatically cheaper than a full index (%s)", noopDuration, full)
	}
}

// TestRealProjectIncrementalSmoke is step 4 of the smoke: modify ONE file and
// prove only that file's portion of the graph moves.
//
// It runs against a COPY of a subtree, never the checkout itself. The brief's
// rule is that the smoke must not modify production data, and the safest
// reading of that is that it does not write inside the repository at all.
func TestRealProjectIncrementalSmoke(t *testing.T) {
	source := strings.TrimSpace(os.Getenv("AO_CODEGRAPH_SMOKE_REPO"))
	if source == "" {
		t.Skip("set AO_CODEGRAPH_SMOKE_REPO to a checkout to run the real-project smoke")
	}
	subtree := strings.TrimSpace(os.Getenv("AO_CODEGRAPH_SMOKE_SUBTREE"))
	if subtree == "" {
		subtree = filepath.Join("backend", "internal")
	}

	fixture := filepath.Join(t.TempDir(), "isolated")
	copied := copyTree(t, filepath.Join(source, subtree), fixture)
	t.Logf("copied %d files from %s into an isolated fixture", copied, subtree)
	if copied == 0 {
		t.Skipf("%s holds no files to copy", subtree)
	}

	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	const project = domain.ProjectID("smoke-incremental")
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(project), Path: fixture, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	index := codegraph.NewIndex(st)
	req := codegraph.SyncRequest{
		ProjectID: project, RepoID: domain.ProjectMemoryRepoID(fixture), RepoPath: fixture,
		Commit: "iso-1", Branch: "main",
	}

	start := time.Now()
	first, err := index.Build(ctx, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	full := time.Since(start)
	t.Logf("isolated index: %d files, %d symbols in %s", first.Files, first.Symbols, full)

	// One scratch file changes.
	scratch := "codegraph/smoke_scratch.go"
	if err := os.WriteFile(filepath.Join(fixture, filepath.FromSlash(scratch)),
		[]byte("package codegraph\n\n// SmokeScratch exists only for the incremental smoke.\ntype SmokeScratch struct{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	next := req
	next.Commit = "iso-2"
	start = time.Now()
	incremental, err := index.Apply(ctx, next, codegraph.Diff{Changes: []codegraph.FileChange{
		{Status: codegraph.ChangeAdded, Path: scratch},
	}})
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	t.Logf("incremental (1 of %d files): %s, %d scanned, %d parsed, %d symbols now",
		first.Files, time.Since(start), incremental.FilesScanned, incremental.FilesParsed, incremental.Symbols)

	if incremental.FilesScanned != 1 || incremental.FilesParsed != 1 {
		t.Fatalf("an incremental sync looked beyond the diff: %+v", incremental)
	}
	if incremental.Symbols != first.Symbols+1 {
		t.Fatalf("symbol count moved by %d, want exactly the one declaration added",
			incremental.Symbols-first.Symbols)
	}
	if incremental.Generation != first.Generation {
		t.Fatalf("an in-place update moved the served generation")
	}

	// And the commonest shape of all: an existing file's body changes. It is
	// measured separately because it is a materially cheaper pass than the one
	// above -- adding a file can move the module census and forces the
	// architecture summary to be recomputed, and editing one usually cannot.
	body, err := os.ReadFile(filepath.Join(fixture, filepath.FromSlash(scratch)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, filepath.FromSlash(scratch)),
		append(body, []byte("\n// Second declaration, same file.\nconst SmokeScratchTwo = 2\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	third := req
	third.Commit = "iso-3"
	start = time.Now()
	modified, err := index.Apply(ctx, third, codegraph.Diff{Changes: []codegraph.FileChange{
		{Status: codegraph.ChangeModified, Path: scratch},
	}})
	if err != nil {
		t.Fatalf("modify-only incremental: %v", err)
	}
	t.Logf("incremental, modify only (1 of %d files): %s, %d parsed, %d symbols now",
		first.Files, time.Since(start), modified.FilesParsed, modified.Symbols)
	if modified.FilesParsed != 1 || modified.Symbols != incremental.Symbols+1 {
		t.Fatalf("modify-only sync = %+v", modified)
	}
}

// copyTree copies the source files of a subtree into dst, skipping the
// directories an index would skip anyway.
func copyTree(t *testing.T, src, dst string) int {
	t.Helper()
	skip := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true,
		"build": true, "coverage": true, ".ao": true, "testdata": true,
	}
	copied := 0
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skip[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".ts", ".tsx", ".js", ".py", ".sql":
		default:
			return nil
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return nil //nolint:nilerr // a path that will not relativise is skipped, not fatal
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			// One unreadable file does not invalidate a fixture of two
			// thousand; the copy is a convenience, not the subject.
			return nil //nolint:nilerr // an unreadable source file is skipped, not fatal, for a fixture copy
		}
		target := filepath.Join(dst, rel)
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o700); mkErr != nil {
			return mkErr
		}
		if writeErr := os.WriteFile(target, body, 0o600); writeErr != nil {
			return writeErr
		}
		copied++
		return nil
	})
	if err != nil {
		t.Fatalf("copy tree: %v", err)
	}
	return copied
}
