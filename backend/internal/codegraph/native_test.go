package codegraph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const mainGo = `package app

import (
	"fmt"
	"strings"
)

// Greeter renders greetings.
type Greeter struct {
	Prefix string
}

const DefaultPrefix = "hello"

func (g *Greeter) Greet(name string) string {
	return fmt.Sprintf("%s %s", g.Prefix, strings.ToUpper(name))
}

func Run() {
	g := &Greeter{Prefix: DefaultPrefix}
	fmt.Println(g.Greet("world"))
}
`

const helperGo = `package app

func Helper() int {
	return 41
}
`

const appTS = `import { useState } from "react";
import { helper } from "./helper";

export interface Props {
	name: string;
}

export const Widget = (props: Props) => {
	return helper(props.name);
};

export function render() {
	return Widget;
}
`

// newProject writes a small multi-language checkout and returns its root.
func newProject(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	writeFile(t, root, "main.go", mainGo)
	writeFile(t, root, "helper.go", helperGo)
	writeFile(t, root, "web/app.ts", appTS)
	writeFile(t, root, "README.md", "# not indexed\n")
	writeFile(t, root, "node_modules/dep/index.js", "export function ignored() {}\n")
	return root
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func newIndexer(t *testing.T) *NativeIndexer {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "codegraph"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	indexer, err := NewNativeIndexer(store, WithClock(func() time.Time {
		return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	}))
	if err != nil {
		t.Fatalf("NewNativeIndexer: %v", err)
	}
	return indexer
}

func symbolIDs(t *testing.T, indexer *NativeIndexer, root string) []string {
	t.Helper()
	canonical, err := CanonicalRoot(root)
	if err != nil {
		t.Fatalf("CanonicalRoot: %v", err)
	}
	graph, found, err := indexer.store.Load(canonical)
	if err != nil || !found {
		t.Fatalf("load graph: found=%v err=%v", found, err)
	}
	var ids []string
	for _, rel := range graph.Paths() {
		for _, sym := range graph.Files[rel].Symbols {
			ids = append(ids, sym.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func TestIndexBuildsGraphAndSkipsUnindexableTrees(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")

	result, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if !result.FullIndex {
		t.Fatal("Index did not report a full index")
	}
	want := []string{"helper.go", "main.go", "web/app.ts"}
	if !reflect.DeepEqual(result.ParsedFiles, want) {
		t.Fatalf("ParsedFiles = %v, want %v (README.md and node_modules must not be indexed)", result.ParsedFiles, want)
	}
	if result.FilesParsed != 3 || result.FilesSkipped != 0 || result.FilesRemoved != 0 {
		t.Fatalf("counts = parsed %d skipped %d removed %d, want 3/0/0", result.FilesParsed, result.FilesSkipped, result.FilesRemoved)
	}
	if result.SymbolCount == 0 || result.EdgeCount == 0 {
		t.Fatalf("graph has %d symbols and %d edges, want both non-zero", result.SymbolCount, result.EdgeCount)
	}
	if _, err := os.Stat(result.StorePath); err != nil {
		t.Fatalf("graph not persisted at %q: %v", result.StorePath, err)
	}

	ids := symbolIDs(t, indexer, root)
	for _, want := range []string{
		"main.go#type:Greeter",
		"main.go#method:Greeter.Greet",
		"main.go#function:Run",
		"main.go#constant:DefaultPrefix",
		"helper.go#function:Helper",
		"web/app.ts#type:Props",
		"web/app.ts#function:Widget",
		"web/app.ts#function:render",
	} {
		if !contains(ids, want) {
			t.Fatalf("symbol %q missing from %v", want, ids)
		}
	}
}

func TestIndexSkipsFilesWhoseHashDidNotChange(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")

	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	second, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root})
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if second.FilesParsed != 0 || second.FilesSkipped != 3 {
		t.Fatalf("re-index parsed %d / skipped %d, want 0/3 — unchanged files were reprocessed", second.FilesParsed, second.FilesSkipped)
	}
}

func TestIncrementalUpdateReprocessesOnlyTheChangedFile(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	writeFile(t, root, "helper.go", "package app\n\nfunc Helper() int {\n\treturn 42\n}\n\nfunc Extra() {}\n")

	// The diff claims main.go changed too, but its bytes did not: the hash
	// gate, not the diff, decides what gets reprocessed.
	diff := Diff{Changes: []FileChange{
		{Status: ChangeModified, Path: "helper.go"},
		{Status: ChangeModified, Path: "main.go"},
	}}
	result, err := indexer.IncrementalUpdate(context.Background(), UpdateRequest{ProjectRoot: root, Diff: diff})
	if err != nil {
		t.Fatalf("IncrementalUpdate: %v", err)
	}
	if result.FullIndex {
		t.Fatal("IncrementalUpdate fell back to a full index")
	}
	if !reflect.DeepEqual(result.ParsedFiles, []string{"helper.go"}) {
		t.Fatalf("ParsedFiles = %v, want [helper.go]", result.ParsedFiles)
	}
	if result.FilesParsed != 1 || result.FilesSkipped != 1 {
		t.Fatalf("counts = parsed %d skipped %d, want 1/1", result.FilesParsed, result.FilesSkipped)
	}

	ids := symbolIDs(t, indexer, root)
	if !contains(ids, "helper.go#function:Extra") {
		t.Fatalf("new symbol missing after incremental update: %v", ids)
	}
	if !contains(ids, "web/app.ts#function:render") {
		t.Fatalf("untouched file's symbols were dropped: %v", ids)
	}
}

func TestIncrementalUpdateRekeysAPureRenameWithoutReparsing(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	if err := os.Rename(filepath.Join(root, "helper.go"), filepath.Join(root, "util.go")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	diff := Diff{Changes: []FileChange{{Status: ChangeRenamed, Path: "util.go", OldPath: "helper.go"}}}
	result, err := indexer.IncrementalUpdate(context.Background(), UpdateRequest{ProjectRoot: root, Diff: diff})
	if err != nil {
		t.Fatalf("IncrementalUpdate: %v", err)
	}
	if result.FilesParsed != 0 || result.FilesRenamed != 1 {
		t.Fatalf("counts = parsed %d renamed %d, want 0/1 — identical bytes must not be re-parsed", result.FilesParsed, result.FilesRenamed)
	}

	ids := symbolIDs(t, indexer, root)
	if contains(ids, "helper.go#function:Helper") {
		t.Fatalf("old path still present after rename: %v", ids)
	}
	if !contains(ids, "util.go#function:Helper") {
		t.Fatalf("symbol not re-keyed to the new path: %v", ids)
	}
}

func TestIncrementalUpdateReparsesARenameThatAlsoChanged(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	if err := os.Remove(filepath.Join(root, "helper.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	writeFile(t, root, "util.go", "package app\n\nfunc Renamed() int { return 1 }\n")

	diff := Diff{Changes: []FileChange{{Status: ChangeRenamed, Path: "util.go", OldPath: "helper.go"}}}
	result, err := indexer.IncrementalUpdate(context.Background(), UpdateRequest{ProjectRoot: root, Diff: diff})
	if err != nil {
		t.Fatalf("IncrementalUpdate: %v", err)
	}
	if result.FilesParsed != 1 || result.FilesRenamed != 0 {
		t.Fatalf("counts = parsed %d renamed %d, want 1/0", result.FilesParsed, result.FilesRenamed)
	}
	ids := symbolIDs(t, indexer, root)
	if contains(ids, "helper.go#function:Helper") || !contains(ids, "util.go#function:Renamed") {
		t.Fatalf("rename+edit left the graph wrong: %v", ids)
	}
}

func TestIncrementalUpdateDropsDeletedFiles(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "helper.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	diff := Diff{Changes: []FileChange{{Status: ChangeDeleted, Path: "helper.go"}}}
	result, err := indexer.IncrementalUpdate(context.Background(), UpdateRequest{ProjectRoot: root, Diff: diff})
	if err != nil {
		t.Fatalf("IncrementalUpdate: %v", err)
	}
	if result.FilesRemoved != 1 || !reflect.DeepEqual(result.RemovedFiles, []string{"helper.go"}) {
		t.Fatalf("removal = %d %v, want 1 [helper.go]", result.FilesRemoved, result.RemovedFiles)
	}

	found, err := indexer.Query(context.Background(), QueryRequest{ProjectRoot: root, Symbol: "Helper"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(found.Symbols) != 0 {
		t.Fatalf("deleted file's symbols still queryable: %+v", found.Symbols)
	}
	// The delete must not have disturbed anything else.
	if len(symbolIDs(t, indexer, root)) == 0 {
		t.Fatal("deleting one file emptied the graph")
	}
}

func TestIncrementalUpdateFallsBackToAFullIndex(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")

	result, err := indexer.IncrementalUpdate(context.Background(), UpdateRequest{
		ProjectRoot: root,
		Diff:        Diff{Changes: []FileChange{{Status: ChangeModified, Path: "helper.go"}}},
	})
	if err != nil {
		t.Fatalf("IncrementalUpdate: %v", err)
	}
	if !result.FullIndex || result.FilesParsed != 3 {
		t.Fatalf("fallback = fullIndex %v parsed %d, want true/3", result.FullIndex, result.FilesParsed)
	}
}

func TestIncrementalUpdateRejectsAPathEscapingTheProject(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	_, err := indexer.IncrementalUpdate(context.Background(), UpdateRequest{
		ProjectRoot: root,
		Diff:        Diff{Changes: []FileChange{{Status: ChangeModified, Path: "../outside.go"}}},
	})
	if !errors.Is(err, ErrProjectRoot) {
		t.Fatalf("IncrementalUpdate with an escaping path = %v, want ErrProjectRoot", err)
	}
}

func TestIncrementalUpdateRefusesAPathThroughASymlinkedDirectory(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")

	// A second checkout, reachable from inside the first only through a
	// symlinked directory. Nothing in it is this project's business.
	outside := filepath.Join(t.TempDir(), "other-project")
	writeFile(t, outside, "secret.go", "package other\n\nfunc Secret() string { return \"token\" }\n")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	ctx := context.Background()
	if _, err := indexer.Index(ctx, IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	assertNoForeignSymbols(t, indexer, root)

	// "linked/secret.go" is lexically innocent — no ".." anywhere — and only
	// the directory component is a symlink, so a final-component check would
	// wave it through.
	for name, diff := range map[string]Diff{
		"modified": {Changes: []FileChange{{Status: ChangeModified, Path: "linked/secret.go"}}},
		"added":    {Changes: []FileChange{{Status: ChangeAdded, Path: "linked/secret.go"}}},
		"renamed":  {Changes: []FileChange{{Status: ChangeRenamed, Path: "linked/secret.go", OldPath: "helper.go"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := indexer.IncrementalUpdate(ctx, UpdateRequest{ProjectRoot: root, Diff: diff}); !errors.Is(err, ErrProjectRoot) {
				t.Fatalf("IncrementalUpdate through a symlinked directory = %v, want ErrProjectRoot", err)
			}
			assertNoForeignSymbols(t, indexer, root)
		})
	}
}

// assertNoForeignSymbols fails if anything from outside the project reached
// the project's graph.
func assertNoForeignSymbols(t *testing.T, indexer *NativeIndexer, root string) {
	t.Helper()
	for _, id := range symbolIDs(t, indexer, root) {
		if strings.Contains(id, "linked/") || strings.Contains(id, "Secret") {
			t.Fatalf("a file outside the project was indexed into it: %q", id)
		}
	}
}

func TestProjectsWithDifferentRootsNeverShareEntries(t *testing.T) {
	indexer := newIndexer(t)
	base := t.TempDir()

	// Same directory name, same file names, different content: only the root
	// keeps them apart.
	alpha := filepath.Join(base, "orgA", "api")
	beta := filepath.Join(base, "orgB", "api")
	writeFile(t, alpha, "main.go", "package api\n\nfunc AlphaOnly() {}\n")
	writeFile(t, beta, "main.go", "package api\n\nfunc BetaOnly() {}\n")

	ctx := context.Background()
	alphaResult, err := indexer.Index(ctx, IndexRequest{ProjectRoot: alpha})
	if err != nil {
		t.Fatalf("index alpha: %v", err)
	}
	betaResult, err := indexer.Index(ctx, IndexRequest{ProjectRoot: beta})
	if err != nil {
		t.Fatalf("index beta: %v", err)
	}
	if alphaResult.StorePath == betaResult.StorePath {
		t.Fatalf("both projects persisted to %q", alphaResult.StorePath)
	}

	if got := symbolIDs(t, indexer, alpha); !reflect.DeepEqual(got, []string{"main.go#function:AlphaOnly"}) {
		t.Fatalf("alpha symbols = %v", got)
	}
	if got := symbolIDs(t, indexer, beta); !reflect.DeepEqual(got, []string{"main.go#function:BetaOnly"}) {
		t.Fatalf("beta symbols = %v", got)
	}

	// A query scoped to one project must not see the other's symbols, even
	// though both live at the same relative path.
	leaked, err := indexer.Query(ctx, QueryRequest{ProjectRoot: alpha, Symbol: "BetaOnly"})
	if err != nil {
		t.Fatalf("query alpha: %v", err)
	}
	if len(leaked.Symbols) != 0 {
		t.Fatalf("project alpha returned project beta's symbols: %+v", leaked.Symbols)
	}

	// Editing one project must leave the other's graph untouched.
	writeFile(t, alpha, "main.go", "package api\n\nfunc AlphaOnly() {}\n\nfunc AlphaExtra() {}\n")
	if _, err := indexer.IncrementalUpdate(ctx, UpdateRequest{
		ProjectRoot: alpha,
		Diff:        Diff{Changes: []FileChange{{Status: ChangeModified, Path: "main.go"}}},
	}); err != nil {
		t.Fatalf("update alpha: %v", err)
	}
	if got := symbolIDs(t, indexer, beta); !reflect.DeepEqual(got, []string{"main.go#function:BetaOnly"}) {
		t.Fatalf("beta graph changed after an alpha update: %v", got)
	}
}

func TestQueryReturnsSymbolsFilesAndEdges(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")
	ctx := context.Background()
	if _, err := indexer.Index(ctx, IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	bySymbol, err := indexer.Query(ctx, QueryRequest{ProjectRoot: root, Symbol: "Run"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(bySymbol.Symbols) != 1 || bySymbol.Symbols[0].ID != "main.go#function:Run" {
		t.Fatalf("symbol query = %+v", bySymbol.Symbols)
	}
	if !hasEdge(bySymbol.Outgoing, Edge{Kind: EdgeCall, From: "main.go#function:Run", To: "fmt.Println"}) {
		t.Fatalf("call edges missing from %+v", bySymbol.Outgoing)
	}

	byFile, err := indexer.Query(ctx, QueryRequest{ProjectRoot: root, File: "web/app.ts"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(byFile.Files) != 1 || byFile.Files[0].Language != "typescript" {
		t.Fatalf("file query = %+v", byFile.Files)
	}
	if !hasEdge(byFile.Outgoing, Edge{Kind: EdgeImport, From: "web/app.ts", To: "react"}) {
		t.Fatalf("import edges missing from %+v", byFile.Outgoing)
	}

	if _, err := indexer.Query(ctx, QueryRequest{ProjectRoot: root}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("empty query = %v, want ErrEmptyQuery", err)
	}
	if _, err := indexer.Query(ctx, QueryRequest{ProjectRoot: t.TempDir(), Symbol: "Run"}); !errors.Is(err, ErrNotIndexed) {
		t.Fatalf("query of an unindexed project = %v, want ErrNotIndexed", err)
	}
}

func TestIndexRejectsAnUnusableProjectRoot(t *testing.T) {
	indexer := newIndexer(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for name, root := range map[string]string{"empty": "", "missing": filepath.Join(t.TempDir(), "nope"), "file": file} {
		t.Run(name, func(t *testing.T) {
			if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); !errors.Is(err, ErrProjectRoot) {
				t.Fatalf("Index(%q) = %v, want ErrProjectRoot", root, err)
			}
		})
	}
}

func TestProviderRejectsARelativeProjectRoot(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")

	// Chdir so the relative root names a real, indexable directory: it must be
	// refused because it is relative, not because it is missing. The contract
	// requires an absolute root, and honoring a relative one would key the
	// persisted index by the process working directory.
	t.Chdir(filepath.Dir(root))
	const relative = "app"

	ctx := context.Background()
	if _, err := indexer.Index(ctx, IndexRequest{ProjectRoot: relative}); !errors.Is(err, ErrProjectRoot) {
		t.Fatalf("Index(%q) = %v, want ErrProjectRoot", relative, err)
	}
	if _, err := indexer.IncrementalUpdate(ctx, UpdateRequest{
		ProjectRoot: relative,
		Diff:        Diff{Changes: []FileChange{{Status: ChangeModified, Path: "helper.go"}}},
	}); !errors.Is(err, ErrProjectRoot) {
		t.Fatalf("IncrementalUpdate(%q) = %v, want ErrProjectRoot", relative, err)
	}
	if _, err := indexer.Query(ctx, QueryRequest{ProjectRoot: relative, Symbol: "Helper"}); !errors.Is(err, ErrProjectRoot) {
		t.Fatalf("Query(%q) = %v, want ErrProjectRoot", relative, err)
	}

	// Nothing was written for the relative spelling, and the absolute root
	// still works.
	if _, err := indexer.Index(ctx, IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index with an absolute root: %v", err)
	}
}

func TestIndexHonorsContextCancellation(t *testing.T) {
	indexer := newIndexer(t)
	root := newProject(t, "app")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := indexer.Index(ctx, IndexRequest{ProjectRoot: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Index with a cancelled context = %v, want context.Canceled", err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasEdge(edges []Edge, want Edge) bool {
	for _, edge := range edges {
		if edge == want {
			return true
		}
	}
	return false
}
