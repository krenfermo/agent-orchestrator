package codegraph_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// incremental_equivalence_test.go — Frente 3 / 3B incremental correctness.
//
// The strongest statement an incremental update can make: after applying a
// change set that creates, modifies (removing a function AND a call), deletes
// and renames files, the served graph is IDENTICAL -- symbol for symbol, edge
// for edge -- to a full build of the same tree. Anything else is a ghost node,
// a ghost edge, a duplicate symbol or a stale dependency.

const eqA1 = "package service\n\nfunc Keep() { Gone(); Stay() }\n\nfunc Gone() {}\n\nfunc Stay() {}\n"
const eqA2 = "package service\n\nfunc Keep() { Stay() }\n\nfunc Stay() {}\n"
const eqB = "package service\n\nfunc B() {}\n"
const eqC = "package service\n\nfunc C() { B() }\n"
const eqD = "package service\n\nfunc D() { Stay() }\n"

func graphSnapshot(t *testing.T, f *fixture) (symbols, edges []string) {
	t.Helper()
	st := f.state()
	_, syms, eds, err := f.store.LoadCodeGraph(f.ctx, testProject, f.repoID, st.ServedGeneration)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		symbols = append(symbols, s.Path+"|"+s.SymbolID+"|"+s.Signature+"|"+s.Summary)
	}
	for _, e := range eds {
		edges = append(edges, e.Path+"|"+e.Kind+"|"+e.FromKey+"|"+e.ToKey)
	}
	sort.Strings(symbols)
	sort.Strings(edges)
	return symbols, edges
}

func TestIncrementalUpdateEqualsAFullBuildOfTheSameTree(t *testing.T) {
	inc := newFixture(t)
	for rel, body := range map[string]string{"svc/a.go": eqA1, "svc/b.go": eqB, "svc/c.go": eqC} {
		mustWrite(t, inc.root, rel, body)
	}
	inc.build("c1")

	// create + modify (drop Gone and its call) + delete + rename, in one diff.
	mustWrite(t, inc.root, "svc/a.go", eqA2)
	mustWrite(t, inc.root, "svc/d.go", eqD)
	if err := os.Remove(filepath.Join(inc.root, "svc", "b.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(inc.root, "svc", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(inc.root, "svc", "c.go"), filepath.Join(inc.root, "svc", "sub", "c2.go")); err != nil {
		t.Fatal(err)
	}
	out := inc.apply("c2",
		codegraph.FileChange{Status: codegraph.ChangeModified, Path: "svc/a.go"},
		codegraph.FileChange{Status: codegraph.ChangeAdded, Path: "svc/d.go"},
		codegraph.FileChange{Status: codegraph.ChangeDeleted, Path: "svc/b.go"},
		codegraph.FileChange{Status: codegraph.ChangeRenamed, OldPath: "svc/c.go", Path: "svc/sub/c2.go"},
	)
	if out.Kind != store.CodeGraphSyncIncremental {
		t.Fatalf("kind = %q, want incremental (no full rescan)", out.Kind)
	}
	if out.FilesScanned > 4 {
		t.Fatalf("an incremental update scanned %d files for a 4-path diff", out.FilesScanned)
	}
	incSyms, incEdges := graphSnapshot(t, inc)
	if len(incSyms) < 5 || !strings.Contains(strings.Join(incEdges, "\n"), "|Stay") {
		t.Fatalf("snapshot is too thin to prove anything: %d symbols, edges %v", len(incSyms), incEdges)
	}

	// A full build of the identical tree, in a separate store.
	full := newFixture(t)
	full.root, full.repoID = inc.root, inc.repoID
	full.build("c2")
	fullSyms, fullEdges := graphSnapshot(t, full)

	if strings.Join(incSyms, "\n") != strings.Join(fullSyms, "\n") {
		t.Fatalf("symbols differ from a full build\nincremental:\n%s\nfull:\n%s",
			strings.Join(incSyms, "\n"), strings.Join(fullSyms, "\n"))
	}
	if strings.Join(incEdges, "\n") != strings.Join(fullEdges, "\n") {
		t.Fatalf("edges differ from a full build\nincremental:\n%s\nfull:\n%s",
			strings.Join(incEdges, "\n"), strings.Join(fullEdges, "\n"))
	}

	// And the specific ghosts, named.
	seen := map[string]int{}
	for _, s := range incSyms {
		parts := strings.SplitN(s, "|", 3)
		seen[parts[1]]++
		if strings.Contains(parts[1], "Gone") || strings.HasPrefix(s, "svc/b.go|") || strings.HasPrefix(s, "svc/c.go|") {
			t.Fatalf("ghost symbol survived: %s", s)
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate symbol %s (%d copies)", id, n)
		}
	}
	for _, e := range incEdges {
		if strings.HasSuffix(e, "|Gone") || strings.HasPrefix(e, "svc/b.go|") || strings.HasPrefix(e, "svc/c.go|") {
			t.Fatalf("ghost edge survived: %s", e)
		}
	}
}
