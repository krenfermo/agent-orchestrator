package projectmemory_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	svc "github.com/aoagents/agent-orchestrator/backend/internal/service/projectmemory"
)

// intelligence_test.go -- P4-G's own behaviour, at the layer that decides what
// a person is shown and what the reconciler acts on.
//
// The properties that matter are the ones a reader would not be able to check
// by eye: that the traversal's ceilings actually bind on a graph big enough to
// exceed them, that "stale" is derived from evidence rather than asserted, and
// that the automatic indexer is idempotent -- because an indexer that reindexes
// an unchanged repository every minute is indistinguishable from a working one
// until somebody's laptop fan tells them otherwise.

// intelFixture is a real project with a real git repository, because every
// state P4-G derives is a comparison against a real HEAD.
func intelFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.service = f.service.WithGraph(codegraph.NewIndex(f.store)).WithExplorer(f.store)
	writeGo(t, f.root, "internal/service/records.go", `package service

// Records is the record service.
type Records struct{}

// MayExport decides whether a role may export records.
func (r *Records) MayExport(role string) bool { return role == "supervisor" }

// Export writes the records out.
func (r *Records) Export(role string) error {
	if !r.MayExport(role) {
		return nil
	}
	return nil
}
`)
	gitInit(t, f.root)
	return f
}

func gitInit(t *testing.T, root string) string {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.test",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Skipf("git is unavailable in this environment: %v (%s)", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	run("add", "-A")
	run("commit", "-m", "initial")
	return run("rev-parse", "HEAD")
}

func gitCommitAll(t *testing.T, root, message string) string {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", message}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.test",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// A project nobody has indexed reports pending rather than an empty list. The
// difference matters: an empty list renders as "this project has no
// repositories", which is a different and wrong claim.
func TestIntelligenceReportsPendingBeforeAnythingIsIndexed(t *testing.T) {
	f := intelFixture(t)

	out, err := f.service.Intelligence(f.ctx, testProject)
	if err != nil {
		t.Fatalf("Intelligence: %v", err)
	}
	if len(out.Repos) == 0 {
		t.Fatal("an unindexed project reported no repositories at all")
	}
	if out.Repos[0].State != "pending" {
		t.Fatalf("state = %q, want pending", out.Repos[0].State)
	}
	if out.Repos[0].HeadCommit == "" {
		t.Fatal("pending must still report where the checkout actually is")
	}
}

// After a sync the state is ready, and the two commits agree. This is the
// baseline every other state is a departure from.
func TestIntelligenceReportsReadyAfterASync(t *testing.T) {
	f := intelFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}

	out, err := f.service.Intelligence(f.ctx, testProject)
	if err != nil {
		t.Fatalf("Intelligence: %v", err)
	}
	repo := findRepo(t, out, f.root)
	if repo.State != "ready" {
		t.Fatalf("state = %q (drift %q), want ready", repo.State, repo.Drift)
	}
	if repo.IndexedCommit == "" || repo.IndexedCommit != repo.HeadCommit {
		t.Fatalf("ready must mean the commits agree: indexed %q head %q", repo.IndexedCommit, repo.HeadCommit)
	}
	if repo.Symbols == 0 {
		t.Fatal("a ready graph with no symbols is not ready")
	}
}

// The headline honesty property: once the checkout moves on, the state says
// stale and says why. Presenting this graph as current is the one failure the
// subsystem refuses to make.
func TestIntelligenceReportsStaleWhenTheCheckoutMovesOn(t *testing.T) {
	f := intelFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	writeGo(t, f.root, "internal/service/audit.go", `package service

// Audit records an access.
func Audit(role string) string { return role }
`)
	head := gitCommitAll(t, f.root, "add audit")

	out, err := f.service.Intelligence(f.ctx, testProject)
	if err != nil {
		t.Fatalf("Intelligence: %v", err)
	}
	repo := findRepo(t, out, f.root)
	if repo.State != "stale" {
		t.Fatalf("state = %q, want stale after a new commit", repo.State)
	}
	if repo.Drift == "" {
		t.Fatal("stale must explain itself")
	}
	if repo.HeadCommit != head {
		t.Fatalf("head = %q, want the new commit %q", repo.HeadCommit, head)
	}
	if repo.IndexedCommit == head {
		t.Fatal("the indexed commit must still be the old one")
	}
}

// The reconciler is what makes indexing automatic. It must index a project
// nobody asked it to, and then leave it alone -- an indexer that reindexes an
// unchanged repository every tick is a busy loop with a status page.
func TestReconcilerIndexesAutomaticallyAndThenStaysIdle(t *testing.T) {
	f := intelFixture(t)
	r := svc.NewReconciler(f.service, f.store, svc.ReconcilerConfig{
		Interval: time.Hour, MaxPerTick: 4,
	})

	r.ReconcileOnce(f.ctx)

	out, err := f.service.Intelligence(f.ctx, testProject)
	if err != nil {
		t.Fatalf("Intelligence: %v", err)
	}
	repo := findRepo(t, out, f.root)
	if repo.State != "ready" {
		t.Fatalf("the reconciler left the project %q; want ready", repo.State)
	}
	first := repo.Generation
	if first == 0 {
		t.Fatal("nothing was indexed")
	}

	// A second pass on an unchanged repository must not build anything. The
	// cooldown holds it off, and even without one the state is no longer
	// pending or stale.
	r.ReconcileOnce(f.ctx)
	out, err = f.service.Intelligence(f.ctx, testProject)
	if err != nil {
		t.Fatalf("Intelligence: %v", err)
	}
	if second := findRepo(t, out, f.root).Generation; second != first {
		t.Fatalf("an unchanged repository was reindexed: generation %d -> %d", first, second)
	}
}

// A repository that has gone stale is picked up by the reconciler without
// anybody running a sync, and the pass it chooses is incremental -- the whole
// economic argument for a graph is that a one-file change costs one file.
func TestReconcilerRefreshesStaleIncrementally(t *testing.T) {
	f := intelFixture(t)
	r := svc.NewReconciler(f.service, f.store, svc.ReconcilerConfig{
		Interval: time.Nanosecond, MaxPerTick: 4,
	})
	r.ReconcileOnce(f.ctx)

	writeGo(t, f.root, "internal/service/audit.go", `package service

// Audit records an access.
func Audit(role string) string { return role }
`)
	gitCommitAll(t, f.root, "add audit")

	r.ReconcileOnce(f.ctx)

	out, err := f.service.Intelligence(f.ctx, testProject)
	if err != nil {
		t.Fatalf("Intelligence: %v", err)
	}
	repo := findRepo(t, out, f.root)
	if repo.State != "ready" {
		t.Fatalf("state = %q (drift %q), want ready after the refresh", repo.State, repo.Drift)
	}
	if repo.LastSyncKind != "incremental" {
		t.Fatalf("last sync was %q; a one-file change must not force a full build", repo.LastSyncKind)
	}
	// The economic claim, measured: a one-file commit costs one file. An
	// incremental pass does not "reuse" the others -- it never looks at them
	// at all, which is the point -- so the property to assert is that it
	// parsed strictly fewer files than the graph holds.
	if repo.FilesParsed != 1 {
		t.Fatalf("the incremental pass parsed %d files; a one-file commit must cost one", repo.FilesParsed)
	}
	if repo.Files < 2 {
		t.Fatalf("the graph holds %d files; the new one was not added", repo.Files)
	}
	// And the new symbol is actually in the graph.
	sub, err := f.service.Subgraph(f.ctx, svc.SubgraphRequest{
		ProjectID: testProject, Symbol: "Audit", Depth: 1,
	})
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	if len(sub.Nodes) == 0 {
		t.Fatal("the incrementally indexed symbol is not in the graph")
	}
}

// A traversal must name a seed. "Start anywhere" is the whole-graph export the
// endpoint exists in order not to be.
func TestSubgraphRefusesToStartAnywhere(t *testing.T) {
	f := intelFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.Subgraph(f.ctx, svc.SubgraphRequest{ProjectID: testProject}); err == nil {
		t.Fatal("a traversal with no symbol and no path was allowed")
	}
}

// The ceilings bind. This is the performance rule the brief calls critical,
// and it is the one property that cannot be checked by reading the code.
func TestSubgraphCeilingsBind(t *testing.T) {
	f := intelFixture(t)
	// A file with many interconnected declarations, so a two-hop walk from any
	// of them would exceed a small ceiling.
	var b strings.Builder
	b.WriteString("package wide\n\n")
	for i := 0; i < 60; i++ {
		fmtSym(&b, i)
	}
	writeGo(t, f.root, "internal/wide/wide.go", b.String())
	gitCommitAll(t, f.root, "wide")
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}

	sub, err := f.service.Subgraph(f.ctx, svc.SubgraphRequest{
		ProjectID: testProject, Path: "internal/wide/wide.go",
		Depth: 2, MaxNodes: 5, MaxEdges: 5,
	})
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	if len(sub.Nodes) > 5 {
		t.Fatalf("the node ceiling did not bind: %d nodes", len(sub.Nodes))
	}
	if len(sub.Edges) > 5 {
		t.Fatalf("the edge ceiling did not bind: %d edges", len(sub.Edges))
	}
	if !sub.Truncated {
		t.Fatal("a walk that hit a ceiling must say so rather than look complete")
	}
}

// Depth is capped server-side. A caller asking for ten hops gets two, because
// a limit the caller chooses is not a limit.
func TestSubgraphDepthIsCappedServerSide(t *testing.T) {
	f := intelFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	sub, err := f.service.Subgraph(f.ctx, svc.SubgraphRequest{
		ProjectID: testProject, Symbol: "MayExport", Depth: 10,
	})
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	for _, n := range sub.Nodes {
		if n.Depth > svc.MaxSubgraphDepth {
			t.Fatalf("node %s was reached at depth %d, past the cap", n.Key, n.Depth)
		}
	}
}

// Search answers from both authorities and says which produced each row.
func TestSearchAnswersFromTheGraphWithProvenance(t *testing.T) {
	f := intelFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	res, err := f.service.Search(f.ctx, svc.SearchRequest{
		ProjectID: testProject, Query: "donde se implementan los permisos MayExport",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("search found nothing for a symbol that exists")
	}
	found := false
	for _, hit := range res.Hits {
		if hit.Kind == "" {
			t.Fatal("every hit must say which authority produced it")
		}
		if hit.Kind == "symbol" && strings.Contains(hit.Title, "MayExport") {
			found = true
			if hit.Path == "" {
				t.Fatal("a symbol hit must name the file it is in")
			}
		}
	}
	if !found {
		t.Fatalf("the named symbol was not among the hits: %+v", res.Hits)
	}
}

// An empty question is refused rather than answered with everything.
func TestSearchRefusesAnEmptyQuestion(t *testing.T) {
	f := intelFixture(t)
	if _, err := f.service.Search(f.ctx, svc.SearchRequest{ProjectID: testProject}); err == nil {
		t.Fatal("an empty search was allowed")
	}
}

// The context preview reports what it selected and what it left out, and never
// claims a saving. The vocabulary is the point: AO cannot see what the harness
// reads, so "avoided" is the strongest honest word.
func TestContextPreviewMeasuresSelectionNotSavings(t *testing.T) {
	f := intelFixture(t)
	if _, err := f.service.Rebuild(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}

	preview, err := f.service.ContextPreview(f.ctx, svc.ContextPreviewRequest{
		ProjectID: testProject, Role: "planner",
	})
	if err != nil {
		t.Fatalf("ContextPreview: %v", err)
	}
	if preview.Role != "planner" {
		t.Fatalf("role = %q", preview.Role)
	}
	if preview.CandidateItems < preview.SelectedItems {
		t.Fatalf("selected %d of %d candidates, which is impossible",
			preview.SelectedItems, preview.CandidateItems)
	}
	if preview.SelectedBytes > 0 && preview.EstimatedTokens == 0 {
		t.Fatal("a pack with bytes must carry a token estimate")
	}
}

// An unknown role is refused rather than silently previewing a planner pack.
func TestContextPreviewRefusesAnUnknownRole(t *testing.T) {
	f := intelFixture(t)
	_, err := f.service.ContextPreview(f.ctx, svc.ContextPreviewRequest{
		ProjectID: testProject, Role: "wroker",
	})
	if err == nil {
		t.Fatal("a misspelled role was accepted")
	}
}

func findRepo(t *testing.T, out controllers.ProjectIntelligenceOverview, root string) controllers.ProjectIntelligenceRepoStatus {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolved = root
	}
	for _, repo := range out.Repos {
		if repo.RepoPath == resolved || repo.RepoPath == root {
			return repo
		}
	}
	if len(out.Repos) > 0 {
		return out.Repos[0]
	}
	t.Fatalf("no repository in %+v", out)
	return controllers.ProjectIntelligenceRepoStatus{}
}

func fmtSym(b *strings.Builder, i int) {
	b.WriteString("// Sym" + itoa(i) + " is a declaration.\n")
	b.WriteString("func Sym" + itoa(i) + "() int { return Sym" + itoa((i+1)%60) + "Helper() }\n\n")
	b.WriteString("// Sym" + itoa(i) + "Helper helps.\n")
	b.WriteString("func Sym" + itoa(i) + "Helper() int { return " + itoa(i) + " }\n\n")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

var _ = context.Background
