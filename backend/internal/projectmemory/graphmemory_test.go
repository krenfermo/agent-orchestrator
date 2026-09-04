package projectmemory_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// graphmemory_test.go — the join between project memory and the code graph.
//
// The tests that matter here are the ones about DEGRADATION. A code graph that
// works is easy; what has to hold is that a dispatch is no worse off than it
// was before this phase when the graph is absent, unbuilt, broken, or over
// budget -- and that a task worktree can never contaminate the canonical one.

// failingGraph is a CodeGraph whose every method fails, which is the state an
// unreachable external backend would present.
type failingGraph struct{ err error }

func (g failingGraph) Build(context.Context, codegraph.SyncRequest) (codegraph.SyncOutcome, error) {
	return codegraph.SyncOutcome{}, g.err
}

func (g failingGraph) Apply(context.Context, codegraph.SyncRequest, codegraph.Diff) (codegraph.SyncOutcome, error) {
	return codegraph.SyncOutcome{}, g.err
}

func (g failingGraph) Retrieve(context.Context, domain.ProjectID, string, codegraph.RetrieveRequest) (codegraph.Neighborhood, error) {
	return codegraph.Neighborhood{}, g.err
}

func (g failingGraph) Architecture(context.Context, domain.ProjectID, string) (string, codegraph.Architecture, bool, error) {
	return "", codegraph.Architecture{}, false, g.err
}

func (g failingGraph) Status(context.Context, domain.ProjectID, string) (store.CodeGraphState, bool, error) {
	return store.CodeGraphState{}, false, g.err
}

// recordsGo is the fixture's authorization path: a role, a rule, and the
// method that decides it.
const recordsGo = `package service

// Role names a principal's role.
type Role string

// Supervisor supervises.
const Supervisor Role = "supervisor"

// Records is the record service.
type Records struct{}

// MayExport decides whether a role may export records.
func (r *Records) MayExport(role Role) bool { return role == Supervisor }
`

// graphRepo is a checkout with a real authorization path in it, so a retrieval
// has something true to find.
func graphRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":                      "module example.com/app\n\ngo 1.24\n",
		"README.md":                   "# App\n\nA service with roles.\n",
		"AGENTS.md":                   "# AGENTS.md\n\n## Coding conventions\n\nKeep changes surgical.\n",
		"internal/service/records.go": recordsGo,
		"internal/service/records_test.go": `package service

import "testing"

func TestRecordsMayExport(t *testing.T) {
	r := &Records{}
	if !r.MayExport(Supervisor) {
		t.Fatal("no")
	}
}
`,
	})
	return root
}

// graphFixture wires a service with a real code graph over the same store.
func graphFixture(t *testing.T, opts ...projectmemory.ServiceOption) (*fixture, *projectmemory.Service, *codegraph.Index, string) {
	t.Helper()
	f := newFixture(t)
	graph := codegraph.NewIndex(f.store)
	svc := projectmemory.NewService(f.store, append([]projectmemory.ServiceOption{
		projectmemory.WithCodeGraph(graph),
	}, opts...)...)
	return f, svc, graph, graphRepo(t)
}

func TestFirstSyncBuildsBothHalvesOfMemory(t *testing.T) {
	requireGit(t)
	f, svc, graph, root := graphFixture(t)
	repo := initGitRepo(t, root)
	syncer := projectmemory.NewSyncer(svc, projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
	})
	_ = repo

	fresh := syncer.EnsureFresh(f.ctx, testProject, root)
	if !fresh.Healthy() {
		t.Fatalf("project memory did not become usable: %+v", fresh)
	}
	if !fresh.Graph.Attempted {
		t.Fatalf("no code graph sync was attempted: %+v", fresh.Graph)
	}
	if !fresh.Graph.Usable || fresh.Graph.Symbols == 0 {
		t.Fatalf("the code graph was not built: %+v", fresh.Graph)
	}
	if fresh.Graph.Kind != store.CodeGraphSyncFull {
		t.Fatalf("first graph sync kind = %q, want a full build", fresh.Graph.Kind)
	}

	// The repository id comes from the sync, not from the raw path: the syncer
	// canonicalises the root (symlinks included), and re-deriving the id from
	// an uncanonicalised path is how a second, wrong identity gets minted.
	state, found, err := graph.Status(f.ctx, testProject, fresh.RepoID)
	if err != nil || !found {
		t.Fatalf("graph status: found=%v err=%v", found, err)
	}
	if state.Backend != codegraph.BackendLocal {
		t.Fatalf("backend = %q, want the in-tree one reported by its real name", state.Backend)
	}
	// The identity the graph recorded must be the one AO observes for this
	// checkout -- including the empty one for a directory that is not a git
	// repository, which is the honest answer rather than a fabricated id.
	if want := projectmemory.RepoIdentityOf(f.ctx, root).String(); state.RepoIdentity != want {
		t.Fatalf("graph repo identity = %q, want %q", state.RepoIdentity, want)
	}

	// A second check does no work in either half. This is the property the
	// whole phase turns on: after the first registration, an unchanged project
	// costs one row read, not a repository scan.
	again := syncer.EnsureFresh(f.ctx, testProject, root)
	if again.Graph.Kind != store.CodeGraphSyncNoop {
		t.Fatalf("a warm graph sync did work: %+v", again.Graph)
	}
	if again.Graph.FilesParsed != 0 || again.Graph.FilesReused != 0 {
		t.Fatalf("a warm graph sync touched %d files", again.Graph.FilesParsed+again.Graph.FilesReused)
	}
}

func TestSecondTaskSyncsOnlyWhatTheCommitChanged(t *testing.T) {
	requireGit(t)
	f, svc, _, root := graphFixture(t)
	repo := initGitRepo(t, root)
	syncer := projectmemory.NewSyncer(svc, projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
	})
	first := syncer.EnsureFresh(f.ctx, testProject, root)
	if first.Graph.Kind != store.CodeGraphSyncFull {
		t.Fatalf("first sync = %+v", first.Graph)
	}
	symbolsBefore := first.Graph.Symbols

	// One file changes, and a new one appears -- the shape of an ordinary task.
	writeTree(t, root, map[string]string{
		"internal/service/records.go": strings.Replace(recordsGo,
			"func (r *Records) MayExport(role Role) bool { return role == Supervisor }",
			"func (r *Records) MayExport(role Role, scope string) bool { return role == Supervisor }", 1),
		"internal/service/audit.go": "package service\n\n// Auditor reviews exports.\ntype Auditor struct{}\n",
	})
	repo.commit(t, "widen the export permission")

	second := syncer.EnsureFresh(f.ctx, testProject, root)
	if second.Graph.Kind != store.CodeGraphSyncIncremental {
		t.Fatalf("second sync = %+v, want an incremental graph update", second.Graph)
	}
	// Two paths changed; the rest of the repository was never opened.
	if second.Graph.FilesParsed != 2 {
		t.Fatalf("an incremental graph sync parsed %d files, want the 2 the commit named: %+v",
			second.Graph.FilesParsed, second.Graph)
	}
	if second.Graph.Symbols <= symbolsBefore {
		t.Fatalf("the new declaration did not reach the graph: %d -> %d", symbolsBefore, second.Graph.Symbols)
	}
	if second.Graph.Generation != first.Graph.Generation {
		t.Fatalf("an in-place update moved the served generation: %d -> %d",
			first.Graph.Generation, second.Graph.Generation)
	}
}

func TestATaskWorktreeNeverBecomesASecondCanonicalGraph(t *testing.T) {
	requireGit(t)
	f, svc, graph, root := graphFixture(t)
	repo := initGitRepo(t, root)
	syncer := projectmemory.NewSyncer(svc, projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
	})
	fresh := syncer.EnsureFresh(f.ctx, testProject, root)
	if !fresh.Graph.Usable {
		t.Fatalf("the repository's graph was not built: %+v", fresh.Graph)
	}

	worktree := filepath.Join(t.TempDir(), "task-worktree")
	repo.run(t, "worktree", "add", "-q", "-b", "ao/task-1", worktree)
	if err := os.WriteFile(filepath.Join(worktree, "internal", "service", "leak.go"),
		[]byte("package service\n\n// Leak must never become canonical.\ntype Leak struct{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A caller that mistakenly passes the worktree is refused outright, and no
	// second graph is minted for it.
	viaWorktree := syncer.EnsureFresh(f.ctx, testProject, worktree)
	if viaWorktree.Kind != projectmemory.SyncSkipped {
		t.Fatalf("a linked worktree was accepted as a repository: %+v", viaWorktree)
	}
	states, err := graph.StatusAll(f.ctx, testProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("a worktree minted a second canonical graph: %d graphs registered", len(states))
	}

	// And the worktree's own provisional symbol is reachable ONLY through the
	// on-demand analysis, never through the canonical graph.
	canonical, err := graph.Retrieve(f.ctx, testProject, fresh.RepoID, codegraph.RetrieveRequest{Terms: []string{"leak"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range canonical.Symbols {
		if s.Symbol.Name == "Leak" {
			t.Fatalf("a worktree symbol was promoted into the canonical graph: %+v", s)
		}
	}
	overlay, err := graph.AnalyzeChanged(f.ctx, worktree, []string{"internal/service/leak.go"}, codegraph.RetrieveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range overlay.Symbols {
		if s.Symbol.Name == "Leak" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the task's own change was not analysable: %+v", overlay.Symbols)
	}
	after, err := graph.StatusAll(f.ctx, testProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].SymbolCount != states[0].SymbolCount {
		t.Fatalf("the overlay analysis changed canonical state: %+v -> %+v", states, after)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
}

func TestPackCarriesGraphEvidenceAsItsOwnSourceCategory(t *testing.T) {
	f, svc, _, root := graphFixture(t)
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	syncer := projectmemory.NewSyncer(svc, projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
	})
	syncer.EnsureFresh(f.ctx, testProject, root)

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		Keywords: []string{"export", "supervisor", "permission"},
	})
	if pack.Graph.Empty() {
		t.Fatalf("the pack carried no graph evidence: %+v", pack.Graph)
	}
	if pack.Graph.SelectedSymbols == 0 || pack.Graph.ConsideredSymbols == 0 {
		t.Fatalf("graph contribution is not measurable: %+v", pack.Graph)
	}
	if pack.Graph.Backend != codegraph.BackendLocal || pack.Graph.Generation == 0 {
		t.Fatalf("graph evidence carries no provenance: %+v", pack.Graph)
	}

	rendered := pack.Render()
	if !strings.Contains(rendered, "(code graph)") {
		t.Fatalf("graph evidence was not rendered under its own heading:\n%s", rendered)
	}
	if !strings.Contains(rendered, "MayExport") {
		t.Fatalf("the retrieved evaluator did not reach the pack:\n%s", rendered)
	}
	// The two source categories stay separable: the graph's bytes are a
	// subset of the pack's, and both are reported.
	if pack.Graph.Bytes == 0 || pack.Graph.Bytes > len(rendered) {
		t.Fatalf("graph bytes = %d against a %d-byte pack", pack.Graph.Bytes, len(rendered))
	}
}

func TestPlannerGetsTheArchitectureMapAndWorkerDoesNot(t *testing.T) {
	f, svc, _, root := graphFixture(t)
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	projectmemory.NewSyncer(svc, projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
	}).EnsureFresh(f.ctx, testProject, root)

	planner := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RolePlanner,
		Keywords: []string{"export"},
	})
	if !strings.Contains(planner.Graph.Architecture, "internal/service") {
		t.Fatalf("the planner did not receive a module map: %q", planner.Graph.Architecture)
	}

	worker := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		Keywords: []string{"export"},
	})
	if worker.Graph.Architecture != "" {
		t.Fatalf("the worker was sent a whole-project map it did not need: %q", worker.Graph.Architecture)
	}
}

func TestGraphFailureDegradesToProjectMemoryAlone(t *testing.T) {
	f := newFixture(t)
	root := graphRepo(t)
	svc := projectmemory.NewService(f.store,
		projectmemory.WithCodeGraph(failingGraph{err: errors.New("backend unreachable")}))

	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	syncer := projectmemory.NewSyncer(svc, projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
	})
	fresh := syncer.EnsureFresh(f.ctx, testProject, root)
	if !fresh.Healthy() {
		t.Fatalf("a broken code graph made project memory unusable: %+v", fresh)
	}
	if fresh.Graph.Reason == "" {
		t.Fatalf("the degradation was silent: %+v", fresh.Graph)
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		Keywords: []string{"export"},
	})
	if pack.Empty() {
		t.Fatal("a broken code graph emptied the whole pack")
	}
	if !pack.Graph.Empty() || pack.Graph.Reason == "" {
		t.Fatalf("a failing graph contributed content, or contributed nothing without saying why: %+v", pack.Graph)
	}
	if strings.Contains(pack.Render(), "(code graph)") {
		t.Fatal("a failing graph still rendered a section")
	}
}

func TestNoCodeGraphConfiguredIsThePrePhaseBehaviour(t *testing.T) {
	f := newFixture(t)
	root := graphRepo(t)
	svc := projectmemory.NewService(f.store)

	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	fresh := projectmemory.NewSyncer(svc, projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
	}).EnsureFresh(f.ctx, testProject, root)
	if fresh.Graph.Attempted {
		t.Fatalf("a sync attempted a graph nobody wired: %+v", fresh.Graph)
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if pack.Empty() {
		t.Fatal("a project with no code graph got no memory at all")
	}
	if !pack.Graph.Empty() {
		t.Fatalf("graph evidence appeared without a graph: %+v", pack.Graph)
	}
}

func TestMemoryOffAttachesNoGraphEvidence(t *testing.T) {
	f, svc, _, root := graphFixture(t)
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	provisioner := projectmemory.NewProvisioner(svc, projectmemory.Config{
		Mode: projectmemory.ModeOff, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
		Budgets: projectmemory.DefaultBudgets(),
	})
	got := provisioner.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		Keywords: []string{"export"},
	})
	if got.Attached() || got.Render() != "" {
		t.Fatalf("memory-off attached something: %+v", got.Pack.Graph)
	}
	if got.Metrics.GraphBytes != 0 {
		t.Fatalf("memory-off reported graph bytes: %d", got.Metrics.GraphBytes)
	}
}

func TestProvisionReportsTheGraphsContributionSeparately(t *testing.T) {
	f, svc, _, root := graphFixture(t)
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
		Budgets: projectmemory.DefaultBudgets(),
	}
	provisioner := projectmemory.NewProvisioner(svc, cfg)

	got := provisioner.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleReviewer,
		ChangedPaths: []string{"internal/service/records.go"},
		Keywords:     []string{"export", "permission"},
	})
	m := got.Metrics
	if m.GraphBackend != codegraph.BackendLocal {
		t.Fatalf("graph backend not reported by its real name: %q", m.GraphBackend)
	}
	if m.GraphSymbolsSelected == 0 || m.GraphSymbolsConsidered == 0 {
		t.Fatalf("graph selection is not measured: %+v", m)
	}
	if m.GraphSymbolsConsidered < m.GraphSymbolsSelected {
		t.Fatalf("considered %d < selected %d", m.GraphSymbolsConsidered, m.GraphSymbolsSelected)
	}
	if m.GraphBytes == 0 {
		t.Fatalf("the graph contributed nothing measurable: %+v", m)
	}
	// The graph is an addition to the durable facts, and the honest total
	// counts both.
	if m.ContextBytes < m.PackBytes+m.GraphBytes {
		t.Fatalf("context bytes %d omit part of what was sent (pack %d + graph %d)",
			m.ContextBytes, m.PackBytes, m.GraphBytes)
	}
	if m.EstimatedGraphTokens == 0 {
		t.Fatalf("no token estimate for the graph's contribution: %+v", m)
	}
	if m.GraphSyncKind == "" {
		t.Fatalf("the graph sync was not recorded: %+v", m)
	}
}

func TestZeroGraphBudgetSuppressesEvidenceWithAReason(t *testing.T) {
	f := newFixture(t)
	root := graphRepo(t)
	graph := codegraph.NewIndex(f.store)
	svc := projectmemory.NewService(f.store,
		projectmemory.WithCodeGraph(graph),
		projectmemory.WithGraphBudgets(projectmemory.GraphBudgetSet{
			projectmemory.RoleWorker: {MaxBytes: 0},
		}))
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	projectmemory.NewSyncer(svc, projectmemory.Config{
		Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
	}).EnsureFresh(f.ctx, testProject, root)

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		Keywords: []string{"export"},
	})
	if !pack.Graph.Empty() {
		t.Fatalf("a zero budget still spent bytes: %+v", pack.Graph)
	}
	if !strings.Contains(pack.Graph.Reason, "budget is zero") {
		t.Fatalf("the suppression was not explained: %q", pack.Graph.Reason)
	}
}

func TestGraphEvidenceIsAbsentBeforeTheFirstBuild(t *testing.T) {
	f, svc, _, root := graphFixture(t)
	// Project memory indexed, code graph never built.
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		Keywords: []string{"export"},
	})
	if !pack.Graph.Empty() {
		t.Fatalf("evidence appeared from a graph that was never built: %+v", pack.Graph)
	}
	if !strings.Contains(pack.Graph.Reason, "not been built") {
		t.Fatalf("reason = %q", pack.Graph.Reason)
	}
	if pack.Empty() {
		t.Fatal("an unbuilt graph emptied a pack that had durable facts")
	}
}

// TestContextComparisonWithAndWithoutTheGraph is section 38: what does adding
// the graph actually cost and add.
//
// It is a COMPARISON, not a savings claim. The brief's rule applies exactly:
// AO does not observe what a coding harness reads inside a worktree, so nothing
// here is a count of agent-side reads avoided. What can honestly be said is
// what AO ASSEMBLED, with and without graph evidence, from the same repository
// at the same commit for the same role -- which is what this asserts.
func TestContextComparisonWithAndWithoutTheGraph(t *testing.T) {
	requireGit(t)
	root := graphRepo(t)

	measure := func(t *testing.T, withGraph bool) projectmemory.Provisioned {
		t.Helper()
		f := newFixture(t)
		opts := []projectmemory.ServiceOption{}
		if withGraph {
			opts = append(opts, projectmemory.WithCodeGraph(codegraph.NewIndex(f.store)))
		}
		svc := projectmemory.NewService(f.store, opts...)
		cfg := projectmemory.Config{
			Mode: projectmemory.ModeAssisted, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
			Budgets: projectmemory.DefaultBudgets(),
		}
		provisioner := projectmemory.NewProvisioner(svc, cfg)
		return provisioner.Provision(f.ctx, projectmemory.ProvisionRequest{
			ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleReviewer,
			ChangedPaths: []string{"internal/service/records.go"},
			Keywords:     []string{"export", "permission", "supervisor"},
		})
	}

	without := measure(t, false)
	with := measure(t, true)

	t.Logf("without the graph: %d context bytes (%d pack, %d legacy, %d graph), ~%d tokens",
		without.Metrics.ContextBytes, without.Metrics.PackBytes, without.Metrics.LegacyBytes,
		without.Metrics.GraphBytes, without.Metrics.EstimatedInputTokens)
	t.Logf("with the graph:    %d context bytes (%d pack, %d legacy, %d graph), ~%d tokens; "+
		"%d symbols selected from %d considered",
		with.Metrics.ContextBytes, with.Metrics.PackBytes, with.Metrics.LegacyBytes,
		with.Metrics.GraphBytes, with.Metrics.EstimatedInputTokens,
		with.Metrics.GraphSymbolsSelected, with.Metrics.GraphSymbolsConsidered)

	if without.Metrics.GraphBytes != 0 {
		t.Fatalf("a service with no graph reported %d graph bytes", without.Metrics.GraphBytes)
	}
	if with.Metrics.GraphBytes == 0 {
		t.Fatal("the graph contributed nothing to compare")
	}
	// The durable-fact half must be IDENTICAL. If adding the graph changed
	// which memory items were selected, the comparison would be measuring two
	// different things and the graph's contribution could not be isolated.
	if with.Metrics.PackItems != without.Metrics.PackItems {
		t.Fatalf("the graph changed durable-fact selection: %d items with, %d without",
			with.Metrics.PackItems, without.Metrics.PackItems)
	}
	// And the whole cost of adding it is exactly what it rendered.
	if got, want := with.Metrics.ContextBytes-without.Metrics.ContextBytes, with.Metrics.GraphBytes; got != want {
		t.Fatalf("context grew by %d bytes but the graph reported %d", got, want)
	}
	if with.Metrics.EstimatedGraphTokens == 0 {
		t.Fatal("no token estimate for the graph's contribution")
	}
}

// TestPreferredModeAttachesGraphEvidenceAndStillPointsAtTheWorkingTree is
// section 16's third mode.
//
// The rule the brief states directly is the one worth a test: **Preferred must
// not be incapable of checking source code.** Preferred ranks memory first and
// may replace legacy documents it demonstrably covers; it does not, and must
// not, tell an agent that the graph is the truth. The pack says the opposite in
// its own preamble, and graph evidence does not change that.
func TestPreferredModeAttachesGraphEvidenceAndStillPointsAtTheWorkingTree(t *testing.T) {
	f, svc, _, root := graphFixture(t)
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := projectmemory.Config{
		Mode: projectmemory.ModePreferred, SyncTimeout: projectmemory.DefaultConfig().SyncTimeout,
		Budgets: projectmemory.DefaultBudgets(),
	}
	got := projectmemory.NewProvisioner(svc, cfg).Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		ChangedPaths: []string{"internal/service/records.go"},
		Keywords:     []string{"export", "supervisor"},
	})

	if got.Pack.Graph.Empty() {
		t.Fatalf("preferred mode attached no graph evidence: %+v", got.Pack.Graph)
	}
	rendered := got.Render()
	if !strings.Contains(rendered, "(code graph)") {
		t.Fatalf("graph evidence is not in the rendered pack:\n%s", rendered)
	}
	// The working tree stays the authority, in the same words every other mode
	// uses. A mode that quietly dropped that sentence would be telling an
	// agent not to look.
	if !strings.Contains(rendered, "the working tree is correct and this is out of date") {
		t.Fatalf("preferred mode stopped saying the working tree is authoritative:\n%s", rendered)
	}
	// Graph evidence is never used to justify dropping a document. A symbol
	// summary does not cover a README, and claiming it did would remove a
	// source AO cannot replace.
	if got.Metrics.DedupeSavedBytes > 0 && got.Metrics.PackItems == 0 {
		t.Fatalf("documents were replaced with nothing but graph evidence: %+v", got.Metrics)
	}
}
