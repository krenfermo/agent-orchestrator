package codegraph_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// index_bench_test.go — section 36: is incremental actually dramatically
// cheaper than full indexing.
//
// The claim this phase rests on is not "the graph is fast". It is that after
// the first registration, an ordinary task costs a file's work rather than a
// repository's. That is a RATIO, and a ratio needs both numbers measured the
// same way on the same tree, which is what these do.
//
// They are written as tests rather than only as Go benchmarks so the ratio is
// asserted in CI and not merely reported to whoever remembers to run -bench.
// The thresholds are deliberately loose: what has to hold is an order of
// magnitude, not a millisecond.

// synthProject writes a repository of n Go files across ten packages, each
// with a type, a constructor, two methods and a test. It is synthetic, and it
// is shaped like real code -- imports across packages, methods on receivers,
// tests that actually call what they name -- because a fixture of empty files
// would measure the walk and nothing else.
func synthProject(tb testing.TB, root string, n int) {
	tb.Helper()
	for i := 0; i < n; i++ {
		pkg := fmt.Sprintf("pkg%02d", i%10)
		rel := filepath.Join("internal", pkg, fmt.Sprintf("unit%04d.go", i))
		body := fmt.Sprintf(`package %s

import (
	"fmt"
	"os"
)

// Unit%04d holds one unit of work.
type Unit%04d struct{ ID string }

// NewUnit%04d builds one.
func NewUnit%04d(id string) *Unit%04d { return &Unit%04d{ID: id} }

// Describe renders the unit. It reads the environment.
func (u *Unit%04d) Describe() string {
	return fmt.Sprintf("%%s/%%s", os.Getenv("AO_UNIT_LABEL"), u.ID)
}

// Validate reports whether the unit is usable.
func (u *Unit%04d) Validate() error {
	if u.ID == "" {
		return fmt.Errorf("unit %04d has no id")
	}
	return nil
}
`, pkg, i, i, i, i, i, i, i, i, i)
		writeAt(tb, root, rel, body)
		writeAt(tb, root, filepath.Join("internal", pkg, fmt.Sprintf("unit%04d_test.go", i)), fmt.Sprintf(`package %s

import "testing"

func TestUnit%04dValidate(t *testing.T) {
	if err := NewUnit%04d("x").Validate(); err != nil {
		t.Fatal(err)
	}
}
`, pkg, i, i))
	}
}

func writeAt(tb testing.TB, root, rel, body string) {
	tb.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		tb.Fatal(err)
	}
}

// benchFixture builds a project of n source files (2n files counting tests).
func benchFixture(tb testing.TB, n int) (*sqlite.Store, *codegraph.Index, codegraph.SyncRequest, context.Context) {
	tb.Helper()
	st := sqlitetest.MustOpen(tb)
	ctx := context.Background()
	root := filepath.Join(tb.TempDir(), "synth")
	synthProject(tb, root, n)
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: "bench", Path: root, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		tb.Fatal(err)
	}
	req := codegraph.SyncRequest{
		ProjectID: domain.ProjectID("bench"), RepoID: domain.ProjectMemoryRepoID(root),
		RepoPath: root, Commit: "c1", Branch: "main",
	}
	return st, codegraph.NewIndex(st), req, ctx
}

// TestIncrementalIsDramaticallyCheaperThanAFullIndex is the phase's central
// performance claim, asserted rather than asserted about.
func TestIncrementalIsDramaticallyCheaperThanAFullIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a thousand-file synthetic repository")
	}
	const files = 500 // 1000 files once tests are counted
	_, index, req, ctx := benchFixture(t, files)

	start := time.Now()
	first, err := index.Build(ctx, req)
	if err != nil {
		t.Fatalf("initial build: %v", err)
	}
	fullDuration := time.Since(start)
	if first.FilesParsed != files*2 {
		t.Fatalf("initial build parsed %d of %d files", first.FilesParsed, files*2)
	}
	t.Logf("initial index: %d files, %d symbols, %d edges in %s",
		first.Files, first.Symbols, first.Edges, fullDuration)

	// A no-op sync: the commit did not move and no file changed.
	start = time.Now()
	noop, err := index.Apply(ctx, req, codegraph.Diff{})
	if err != nil {
		t.Fatalf("no-op sync: %v", err)
	}
	noopDuration := time.Since(start)
	if noop.FilesParsed != 0 || noop.FilesScanned != 0 {
		t.Fatalf("a no-op sync did work: %+v", noop)
	}
	t.Logf("no-op sync: %s", noopDuration)

	// One file changes, which is the shape of an ordinary task.
	changed := filepath.Join("internal", "pkg03", "unit0003.go")
	body, err := os.ReadFile(filepath.Join(req.RepoPath, changed))
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, req.RepoPath, changed, string(body)+"\n// Trailing declaration.\nconst Extra0003 = 1\n")

	next := req
	next.Commit = "c2"
	start = time.Now()
	incremental, err := index.Apply(ctx, next, codegraph.Diff{Changes: []codegraph.FileChange{
		{Status: codegraph.ChangeModified, Path: filepath.ToSlash(changed)},
	}})
	if err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	incrementalDuration := time.Since(start)
	if incremental.FilesParsed != 1 || incremental.FilesScanned != 1 {
		t.Fatalf("an incremental sync looked beyond the diff: %+v", incremental)
	}
	t.Logf("incremental (1 of %d files): %s", files*2, incrementalDuration)

	// The claim. A one-file update must be an order of magnitude cheaper than
	// a full pass over a thousand files; the threshold is loose on purpose,
	// because what matters is the shape of the curve and not a millisecond.
	if incrementalDuration*8 > fullDuration {
		t.Fatalf("incremental sync (%s) is not dramatically cheaper than a full index (%s)",
			incrementalDuration, fullDuration)
	}
	if noopDuration*8 > fullDuration {
		t.Fatalf("no-op sync (%s) is not dramatically cheaper than a full index (%s)",
			noopDuration, fullDuration)
	}

	// A repeat FULL build still reads and hashes every file -- a digest cannot
	// be computed without reading -- but must parse none of them.
	start = time.Now()
	repeat, err := index.Build(ctx, next)
	if err != nil {
		t.Fatalf("repeat build: %v", err)
	}
	t.Logf("repeat full build (all reused): %s", time.Since(start))
	if repeat.FilesParsed != 0 {
		t.Fatalf("a repeat full build re-parsed %d files", repeat.FilesParsed)
	}
	if repeat.FilesReused != files*2 {
		t.Fatalf("a repeat full build reused %d of %d files", repeat.FilesReused, files*2)
	}
}

// TestRetrievalStaysBoundedAsTheGraphGrows is the other half: a dispatch-path
// query must not get slower as the repository gets bigger. It is bounded reads
// against indexes, so it should not.
func TestRetrievalStaysBoundedAsTheGraphGrows(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two synthetic repositories")
	}
	measure := func(files int) (time.Duration, int) {
		_, index, req, ctx := benchFixture(t, files)
		if _, err := index.Build(ctx, req); err != nil {
			t.Fatalf("build: %v", err)
		}
		state, _, err := index.Status(ctx, req.ProjectID, req.RepoID)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		got, err := index.Retrieve(ctx, req.ProjectID, req.RepoID, codegraph.RetrieveRequest{
			Terms: []string{"validate", "unit"},
		})
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		if got.Empty() {
			t.Fatal("retrieval found nothing in a synthetic repository full of matches")
		}
		if got.SelectedSymbols() > codegraph.DefaultRetrievalSymbols {
			t.Fatalf("retrieval returned %d symbols, over its own default bound", got.SelectedSymbols())
		}
		return time.Since(start), int(state.SymbolCount)
	}

	smallDuration, smallSymbols := measure(50)
	largeDuration, largeSymbols := measure(500)
	t.Logf("retrieval: %s over %d symbols, %s over %d symbols",
		smallDuration, smallSymbols, largeDuration, largeSymbols)

	if largeSymbols < smallSymbols*5 {
		t.Fatalf("the large fixture is not materially larger: %d vs %d symbols", largeSymbols, smallSymbols)
	}
	// Ten times the symbols must not cost ten times the query. The bound is
	// generous because a few milliseconds of noise dominates at this size.
	if largeDuration > smallDuration*4+20*time.Millisecond {
		t.Fatalf("retrieval scaled with the graph: %s over %d symbols vs %s over %d",
			largeDuration, largeSymbols, smallDuration, smallSymbols)
	}
}

func BenchmarkFullIndex(b *testing.B) {
	for _, files := range []int{50, 500} {
		b.Run(fmt.Sprintf("files=%d", files*2), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				_, index, req, ctx := benchFixture(b, files)
				b.StartTimer()
				if _, err := index.Build(ctx, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkIncrementalOneFile(b *testing.B) {
	_, index, req, ctx := benchFixture(b, 500)
	if _, err := index.Build(ctx, req); err != nil {
		b.Fatal(err)
	}
	changed := filepath.ToSlash(filepath.Join("internal", "pkg03", "unit0003.go"))
	diff := codegraph.Diff{Changes: []codegraph.FileChange{
		{Status: codegraph.ChangeModified, Path: changed},
	}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := index.Apply(ctx, req, diff); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNoOpSync(b *testing.B) {
	_, index, req, ctx := benchFixture(b, 500)
	if _, err := index.Build(ctx, req); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := index.Apply(ctx, req, codegraph.Diff{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRetrieve(b *testing.B) {
	_, index, req, ctx := benchFixture(b, 500)
	if _, err := index.Build(ctx, req); err != nil {
		b.Fatal(err)
	}
	query := codegraph.RetrieveRequest{Terms: []string{"validate", "unit"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := index.Retrieve(ctx, req.ProjectID, req.RepoID, query); err != nil {
			b.Fatal(err)
		}
	}
}

var _ = store.CodeGraphSyncFull
