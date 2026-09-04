package projectmemory_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	svc "github.com/aoagents/agent-orchestrator/backend/internal/service/projectmemory"
)

// graph_test.go — the code graph's operator surface, at the layer that decides
// what an operator is told.
//
// The tests that matter are about DRIFT. A graph's rows can be perfectly intact
// while the thing they describe has moved on, and an operator surface that
// reported such a graph as healthy would be the most misleading thing in the
// subsystem: it would say "yes, AO knows this project" about a checkout AO has
// not looked at since somebody rebased it.

func graphFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.service = f.service.WithGraph(codegraph.NewIndex(f.store))
	writeGo(t, f.root, "internal/service/records.go", `package service

// Records is the record service.
type Records struct{}

// MayExport decides whether a role may export records.
func (r *Records) MayExport(role string) bool { return role == "supervisor" }
`)
	return f
}

func writeGo(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGraphSyncAndStatusReportTheBackendByItsRealName(t *testing.T) {
	f := graphFixture(t)

	out, err := f.service.GraphSync(f.ctx, testProject, "", false)
	if err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if out.Kind != string(codegraph.BackendLocal) && out.Files == 0 {
		t.Fatalf("sync did nothing: %+v", out)
	}
	if out.Symbols == 0 {
		t.Fatalf("the first sync produced no symbols: %+v", out)
	}

	statuses, err := f.service.GraphStatus(f.ctx, testProject)
	if err != nil {
		t.Fatalf("GraphStatus: %v", err)
	}
	if len(statuses) == 0 {
		t.Fatal("no repository reported a graph")
	}
	got := statuses[0]
	if got.Backend != codegraph.BackendLocal {
		t.Fatalf("backend = %q, want the in-tree one by its real name", got.Backend)
	}
	if !got.Healthy || got.Drift != "" {
		t.Fatalf("a freshly synced graph is not healthy: %+v", got)
	}
	if got.Architecture == "" {
		t.Fatal("no architecture summary was stored")
	}
}

// A graph whose commit no longer matches the checkout is reported unhealthy,
// with the reason. Its rows are intact; what is missing is the proof they
// describe what is on disk.
func TestGraphStatusReportsCommitDrift(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	f := graphFixture(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = f.root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	git("add", "-A")
	git("commit", "-q", "-m", "one")

	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	before, err := f.service.GraphStatus(f.ctx, testProject)
	if err != nil || len(before) == 0 || !before[0].Healthy {
		t.Fatalf("graph is not healthy after a sync: %+v err=%v", before, err)
	}

	// The checkout moves on and nothing tells the graph.
	writeGo(t, f.root, "internal/service/audit.go", "package service\n\n// Auditor reviews.\ntype Auditor struct{}\n")
	git("add", "-A")
	git("commit", "-q", "-m", "two")

	after, err := f.service.GraphStatus(f.ctx, testProject)
	if err != nil {
		t.Fatalf("GraphStatus: %v", err)
	}
	if after[0].Healthy {
		t.Fatalf("a graph whose commit no longer matches was reported healthy: %+v", after[0])
	}
	if !strings.Contains(after[0].Drift, "run a sync") {
		t.Fatalf("drift was not explained: %q", after[0].Drift)
	}
	// The rows are still there. Drift is about provenance, not content.
	if after[0].Symbols == 0 {
		t.Fatal("drift emptied the graph")
	}

	// And a sync repairs it, incrementally.
	out, err := f.service.GraphSync(f.ctx, testProject, "", false)
	if err != nil {
		t.Fatalf("GraphSync after drift: %v", err)
	}
	if out.Kind != "incremental" || out.FilesParsed != 1 {
		t.Fatalf("the repair was not an incremental sync of the one changed file: %+v", out)
	}
	repaired, err := f.service.GraphStatus(f.ctx, testProject)
	if err != nil || !repaired[0].Healthy {
		t.Fatalf("the graph is still unhealthy after a sync: %+v err=%v", repaired, err)
	}
}

func TestGraphQueryShowsWhatADispatchWouldBeTold(t *testing.T) {
	f := graphFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}

	answer, err := f.service.GraphQuery(f.ctx, controllersGraphQuery("export permissions"))
	if err != nil {
		t.Fatalf("GraphQuery: %v", err)
	}
	found := false
	for _, sym := range answer.Symbols {
		if sym.Name == "Records.MayExport" {
			found = true
			if sym.Summary == "" || sym.Reason == "" {
				t.Fatalf("a returned symbol carries no summary or no reason: %+v", sym)
			}
		}
	}
	if !found {
		t.Fatalf("the evaluator was not returned: %+v", answer.Symbols)
	}
	if answer.ConsideredSymbols == 0 {
		t.Fatal("the answer reports nothing considered, so its bound is invisible")
	}
}

func TestGraphRoutesReportNotConfiguredWithoutAGraph(t *testing.T) {
	f := newFixture(t) // no WithGraph
	if _, err := f.service.GraphStatus(f.ctx, testProject); err == nil {
		t.Fatal("GraphStatus succeeded with no graph configured")
	}
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err == nil {
		t.Fatal("GraphSync succeeded with no graph configured")
	}
	if _, err := f.service.GraphQuery(f.ctx, controllersGraphQuery("anything")); err == nil {
		t.Fatal("GraphQuery succeeded with no graph configured")
	}
}

func TestGraphQueryOnAnUnbuiltGraphExplainsItself(t *testing.T) {
	f := graphFixture(t)
	answer, err := f.service.GraphQuery(f.ctx, controllersGraphQuery("export"))
	if err != nil {
		t.Fatalf("GraphQuery before any build: %v", err)
	}
	if len(answer.Symbols) != 0 || !strings.Contains(answer.Reason, "has not been built") {
		t.Fatalf("an unbuilt graph did not explain itself: %+v", answer)
	}
}

// controllersGraphQuery builds the query shape the controller passes down.
func controllersGraphQuery(terms string) controllers.ProjectMemoryGraphQuery {
	return controllers.ProjectMemoryGraphQuery{
		ProjectID: testProject, Terms: strings.Fields(terms),
	}
}

var _ = svc.New
