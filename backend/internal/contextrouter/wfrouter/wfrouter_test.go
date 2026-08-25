package wfrouter

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	memory "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type recordingPlanner struct {
	got  workflowcore.PlannerRequest
	sawn int
}

func (p *recordingPlanner) Generate(_ stdctx.Context, req workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	p.got = req
	p.sawn++
	return workflowcore.PlannerResponse{Provider: "claude", Model: "opus"}, nil
}

type describingRecordingPlanner struct {
	recordingPlanner
}

func (p *describingRecordingPlanner) Descriptor() (string, string) { return "claude", "opus" }

type recordingSpawner struct {
	got  ports.SpawnConfig
	sawn int
}

func (s *recordingSpawner) Spawn(_ stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.got = cfg
	s.sawn++
	return domain.SessionRecord{ID: domain.SessionID("sess-1")}, 0, 0, nil
}

const testProjectRoot = "/checkout/proj-1"

const changedFile = "backend/internal/contextrouter/router.go"

// rootStrictDiff mirrors the one requirement the production
// contextrouter.GitDiffSource imposes: it runs git inside the checkout, so an
// empty or relative root is refused. A fake that ignored the root would let a
// wrapper that never resolves one pass its tests while losing the evidence in
// production.
type rootStrictDiff struct {
	changes []codegraph.FileChange
	// roots records the root each call was made with, so a test can assert
	// the resolved checkout actually reached the source.
	roots []string
}

func (d *rootStrictDiff) Changes(_ stdctx.Context, project contextrouter.Project) (codegraph.Diff, error) {
	d.roots = append(d.roots, project.Root)
	if strings.TrimSpace(project.Root) == "" {
		return codegraph.Diff{}, errors.New("contextrouter: diff needs a project root")
	}
	if !filepath.IsAbs(project.Root) {
		return codegraph.Diff{}, fmt.Errorf("contextrouter: project root %q must be absolute", project.Root)
	}
	return codegraph.Diff{Changes: d.changes}, nil
}

// rootStrictGraph answers only for the project root it was built for, the way
// a per-project code graph does.
type rootStrictGraph struct {
	root    string
	symbols map[string][]codegraph.Symbol
	roots   []string
}

func (g *rootStrictGraph) Query(_ stdctx.Context, req codegraph.QueryRequest) (codegraph.QueryResult, error) {
	g.roots = append(g.roots, req.ProjectRoot)
	if req.ProjectRoot != g.root {
		return codegraph.QueryResult{}, codegraph.ErrNotIndexed
	}
	return codegraph.QueryResult{ProjectRoot: req.ProjectRoot, Symbols: g.symbols[req.File]}, nil
}

type stubMemory struct{ items []memory.MemoryItem }

func (s stubMemory) List(string) ([]memory.MemoryItem, error) { return s.items, nil }

// stubProjects stands in for the workflow.Projects port the coordinator wires.
type stubProjects struct {
	record domain.ProjectRecord
	found  bool
	err    error
	calls  int
}

func (p *stubProjects) GetProject(_ stdctx.Context, id string) (domain.ProjectRecord, bool, error) {
	p.calls++
	if p.err != nil {
		return domain.ProjectRecord{}, false, p.err
	}
	if !p.found || p.record.ID != id {
		return domain.ProjectRecord{}, false, nil
	}
	return p.record, true, nil
}

func registeredProject() *stubProjects {
	return &stubProjects{record: domain.ProjectRecord{ID: "proj-1", Path: testProjectRoot}, found: true}
}

func testSources() (contextrouter.Options, *rootStrictDiff, *rootStrictGraph) {
	diff := &rootStrictDiff{changes: []codegraph.FileChange{
		{Status: codegraph.ChangeModified, Path: changedFile},
	}}
	graph := &rootStrictGraph{root: testProjectRoot, symbols: map[string][]codegraph.Symbol{
		changedFile: {{
			ID: changedFile + "#function:Select", Name: "Select", Kind: codegraph.SymbolFunction,
			File: changedFile, Line: 42,
		}},
	}}
	return contextrouter.Options{
		Diff:  diff,
		Graph: graph,
		Memory: stubMemory{items: []memory.MemoryItem{{
			ID: "mem-1", Project: "proj-1", Type: memory.TypeNote, Confidence: 0.7,
			Content: "the daemon's loopback listener stays unauthenticated",
			Source:  memory.Source{Kind: memory.SourceManual, Path: changedFile},
		}}},
	}, diff, graph
}

func newRouter(t *testing.T, opts contextrouter.Options) *contextrouter.Router {
	t.Helper()
	router, err := contextrouter.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return router
}

func testRouter(t *testing.T) *contextrouter.Router {
	t.Helper()
	opts, _, _ := testSources()
	return newRouter(t, opts)
}

func plannerRequest() workflowcore.PlannerRequest {
	return workflowcore.PlannerRequest{
		Objective: "Add a role-aware context router.",
		Project:   domain.ProjectRecord{ID: "proj-1", Path: testProjectRoot},
		Context: workflowcore.PlannerContext{
			Version:   workflowcore.PlannerContextVersion,
			ProjectID: "proj-1",
			Documents: []workflowcore.PlannerDocument{
				{Path: "AGENTS.md", SHA256: "aaa", Content: strings.Repeat("repository conventions. ", 50)},
				{Path: "README.md", SHA256: "bbb", Content: "how to run the daemon"},
			},
		},
		MaxSteps: 12,
	}
}

// The load-bearing default: with the feature flag off the wiring hands back a
// nil router, Instrument returns the dependencies untouched, and every
// dispatch surface keeps the exact payload it had before this package existed.
func TestNilRouterLeavesDispatchUntouched(t *testing.T) {
	if contextrouter.Enabled() {
		t.Fatalf("%s must default to off for this to be the legacy path", contextrouter.FlagEnv)
	}
	planner := &recordingPlanner{}
	spawner := &recordingSpawner{}
	deps := workflowcore.Deps{Planner: planner, Spawner: spawner}

	routed := Instrument(deps, nil, nil)
	if routed.Planner != workflowcore.Planner(planner) {
		t.Fatal("Instrument wrapped the planner with routing disabled")
	}
	if routed.Spawner != workflowcore.Spawner(spawner) {
		t.Fatal("Instrument wrapped the spawner with routing disabled")
	}
	if InstrumentPlanner(planner, nil, nil) != workflowcore.Planner(planner) {
		t.Fatal("InstrumentPlanner wrapped with a nil router")
	}
	if InstrumentSpawner(spawner, nil, registeredProject(), nil) != workflowcore.Spawner(spawner) {
		t.Fatal("InstrumentSpawner wrapped with a nil router")
	}

	// And the payloads themselves are byte-for-byte what the caller supplied.
	want := plannerRequest()
	if _, err := routed.Planner.Generate(stdctx.Background(), want); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(planner.got.Context.Documents) != len(want.Context.Documents) {
		t.Fatalf("the unrouted planner saw %d documents, want %d", len(planner.got.Context.Documents), len(want.Context.Documents))
	}
	for i, doc := range planner.got.Context.Documents {
		if doc != want.Context.Documents[i] {
			t.Fatalf("document %d was altered with routing disabled: %+v", i, doc)
		}
	}

	cfg := ports.SpawnConfig{ProjectID: "proj-1", IssueID: "issue-7", Prompt: "implement it", IssueContext: "the issue body"}
	if _, _, _, err := routed.Spawner.Spawn(stdctx.Background(), cfg); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if spawner.got.IssueContext != cfg.IssueContext || spawner.got.Prompt != cfg.Prompt {
		t.Fatalf("the unrouted spawn config was altered: %+v", spawner.got)
	}
}

// With a router installed the planner receives the routed selection instead of
// every document, and each routed document carries a checksum of what was
// actually delivered.
func TestPlannerContextIsRouted(t *testing.T) {
	planner := &recordingPlanner{}
	wrapped := InstrumentPlanner(planner, testRouter(t), nil)
	request := plannerRequest()
	if _, err := wrapped.Generate(stdctx.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if planner.sawn != 1 {
		t.Fatalf("the wrapped planner ran %d time(s), want 1", planner.sawn)
	}
	got := planner.got
	if got.Objective != request.Objective || got.MaxSteps != request.MaxSteps || got.Project.ID != request.Project.ID {
		t.Fatalf("routing altered something other than the context: %+v", got)
	}
	if len(got.Context.Documents) == 0 {
		t.Fatal("routing produced no documents")
	}

	var sawEvidence, sawAgents bool
	for _, doc := range got.Context.Documents {
		sum := sha256.Sum256([]byte(doc.Content))
		if doc.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("document %q carries a checksum of content it does not hold", doc.Path)
		}
		if strings.HasPrefix(doc.Path, evidencePathPrefix) {
			sawEvidence = true
		}
		if doc.Path == "AGENTS.md" {
			sawAgents = true
		}
		if strings.Contains(doc.Content, request.Objective) {
			t.Fatalf("the objective was duplicated into document %q; it already travels in PlannerRequest.Objective", doc.Path)
		}
	}
	if !sawAgents {
		t.Fatal("the caller's own documents did not survive routing")
	}
	if !sawEvidence {
		t.Fatal("routing contributed no evidence documents")
	}
}

// A request the router cannot answer leaves the dispatch exactly as it was:
// the router exists to send less, not to prevent a dispatch.
func TestPlannerFallsBackWhenRoutingFails(t *testing.T) {
	planner := &recordingPlanner{}
	wrapped := InstrumentPlanner(planner, testRouter(t), nil)
	request := plannerRequest()
	request.Objective = "" // no objective and no title: the router refuses
	if _, err := wrapped.Generate(stdctx.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(planner.got.Context.Documents) != len(request.Context.Documents) {
		t.Fatalf("a failed routing changed the payload: %d documents, want %d", len(planner.got.Context.Documents), len(request.Context.Documents))
	}
	for i, doc := range planner.got.Context.Documents {
		if doc != request.Context.Documents[i] {
			t.Fatalf("a failed routing altered document %d: %+v", i, doc)
		}
	}
}

// The optional PlannerDescriptor capability survives the wrapper, so wiring
// that asks the planner which provider it is does not start getting "unknown"
// the moment routing is switched on.
func TestPlannerDescriptorSurvivesTheWrapper(t *testing.T) {
	wrapped := InstrumentPlanner(&describingRecordingPlanner{}, testRouter(t), nil)
	descriptor, ok := wrapped.(workflowcore.PlannerDescriptor)
	if !ok {
		t.Fatal("the wrapper dropped the PlannerDescriptor capability")
	}
	if provider, model := descriptor.Descriptor(); provider != "claude" || model != "opus" {
		t.Fatalf("Descriptor() = %q,%q", provider, model)
	}
}

// The worker's pre-fetched issue context is routed; the prompt, which carries
// the instruction rather than the evidence, is not touched.
func TestSpawnIssueContextIsRoutedAndPromptIsNot(t *testing.T) {
	spawner := &recordingSpawner{}
	opts, diff, graph := testSources()
	projects := registeredProject()
	wrapped := InstrumentSpawner(spawner, newRouter(t, opts), projects, nil)
	cfg := ports.SpawnConfig{
		ProjectID:    "proj-1",
		IssueID:      "issue-7",
		Prompt:       "Implement the router.",
		IssueContext: strings.Repeat("tracker issue body. ", 200),
	}
	if _, _, _, err := wrapped.Spawn(stdctx.Background(), cfg); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got := spawner.got
	if got.Prompt != cfg.Prompt {
		t.Fatalf("the prompt was rewritten: %q", got.Prompt)
	}
	if got.IssueContext == cfg.IssueContext {
		t.Fatal("the issue context was passed through unrouted")
	}
	if !strings.Contains(got.IssueContext, "issue context") {
		t.Fatalf("the routed issue context lost the caller's own document: %q", got.IssueContext)
	}
	if !strings.Contains(got.IssueContext, "Changed files") || !strings.Contains(got.IssueContext, changedFile) {
		t.Fatalf("the routed issue context carries no diff evidence: %q", got.IssueContext)
	}
	if !strings.Contains(got.IssueContext, "Impacted symbols") || !strings.Contains(got.IssueContext, "Select") {
		t.Fatalf("the routed issue context carries no graph evidence: %q", got.IssueContext)
	}
	// The evidence above is only reachable because the wrapper resolved the
	// checkout root and passed it down: both sources refuse an empty one, the
	// way the production diff source and per-project graph do.
	if projects.calls == 0 {
		t.Fatal("the wrapper never resolved the project root")
	}
	for _, root := range diff.roots {
		if root != testProjectRoot {
			t.Fatalf("the diff source was called with root %q, want %q", root, testProjectRoot)
		}
	}
	if len(graph.roots) == 0 {
		t.Fatal("the code graph was never queried")
	}
	for _, root := range graph.roots {
		if root != testProjectRoot {
			t.Fatalf("the code graph was queried with root %q, want %q", root, testProjectRoot)
		}
	}
	if strings.Contains(got.IssueContext, cfg.Prompt) {
		t.Fatalf("the prompt was duplicated into the issue context: %q", got.IssueContext)
	}
	budget := contextrouter.DefaultBudgets().For(contextrouter.RoleWorker)
	if tokens := (len(got.IssueContext) + 3) / 4; tokens > budget.HardCapTokens {
		t.Fatalf("the routed issue context (%d tokens) exceeds the worker hard cap of %d", tokens, budget.HardCapTokens)
	}
}

// A spawn that carries neither a prompt nor an issue context has nothing to
// route and is left alone.
func TestSpawnWithoutContextIsUntouched(t *testing.T) {
	spawner := &recordingSpawner{}
	wrapped := InstrumentSpawner(spawner, testRouter(t), registeredProject(), nil)
	cfg := ports.SpawnConfig{ProjectID: "proj-1", IssueID: "issue-7"}
	if _, _, _, err := wrapped.Spawn(stdctx.Background(), cfg); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if spawner.got.IssueContext != "" {
		t.Fatalf("an empty spawn config gained an issue context: %q", spawner.got.IssueContext)
	}
}

// Instrument leaves every surface it does not route alone: the reviewer, fix,
// and verify paths carry instructions, not assembled context.
func TestInstrumentLeavesUnroutedSurfacesAlone(t *testing.T) {
	deps := workflowcore.Deps{
		Planner:  &recordingPlanner{},
		Spawner:  &recordingSpawner{},
		Projects: registeredProject(),
	}
	before := deps
	routed := Instrument(deps, testRouter(t), nil)
	if routed.ReviewerLauncher != before.ReviewerLauncher {
		t.Fatal("Instrument touched the reviewer launcher")
	}
	if routed.MessageSender != before.MessageSender {
		t.Fatal("Instrument touched the fix message sender")
	}
	if routed.Verifier != before.Verifier {
		t.Fatal("Instrument touched the verifier")
	}
	if routed.Planner == before.Planner || routed.Spawner == before.Spawner {
		t.Fatal("Instrument did not wrap the surfaces it routes")
	}
}

// A nil dependency stays nil rather than becoming a wrapper around nothing.
func TestInstrumentToleratesNilDependencies(t *testing.T) {
	routed := Instrument(workflowcore.Deps{}, testRouter(t), nil)
	if routed.Planner != nil || routed.Spawner != nil {
		t.Fatalf("nil dependencies were wrapped: planner=%v spawner=%v", routed.Planner, routed.Spawner)
	}
}

// Routing a worker spawn requires the checkout root, because the production
// diff source runs git in it and the code graph is keyed by it. Every reason
// the root cannot be resolved leaves the spawn on its original full context
// rather than on a routed payload with that evidence silently missing.
func TestSpawnIsUnroutedWithoutAResolvableRoot(t *testing.T) {
	cases := map[string]workflowcore.Projects{
		"no resolver wired": nil,
		"lookup failed":     &stubProjects{err: errors.New("database is locked")},
		"not registered":    &stubProjects{found: false},
		"no path recorded":  &stubProjects{record: domain.ProjectRecord{ID: "proj-1"}, found: true},
		"relative path":     &stubProjects{record: domain.ProjectRecord{ID: "proj-1", Path: "checkouts/proj-1"}, found: true},
	}
	for name, projects := range cases {
		t.Run(name, func(t *testing.T) {
			spawner := &recordingSpawner{}
			opts, diff, graph := testSources()
			wrapped := InstrumentSpawner(spawner, newRouter(t, opts), projects, nil)
			cfg := ports.SpawnConfig{
				ProjectID:    "proj-1",
				IssueID:      "issue-7",
				Prompt:       "Implement the router.",
				IssueContext: strings.Repeat("tracker issue body. ", 200),
			}
			if _, _, _, err := wrapped.Spawn(stdctx.Background(), cfg); err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if spawner.got.IssueContext != cfg.IssueContext || spawner.got.Prompt != cfg.Prompt {
				t.Fatalf("an unroutable spawn was rewritten anyway: %+v", spawner.got)
			}
			if len(diff.roots) != 0 {
				t.Fatalf("the diff source was consulted without a root: %v", diff.roots)
			}
			if len(graph.roots) != 0 {
				t.Fatalf("the code graph was consulted without a root: %v", graph.roots)
			}
		})
	}
}

// Instrument is what hands the spawner its project resolver; without that
// wiring every enabled worker dispatch would silently fall back to full
// context.
func TestInstrumentWiresTheProjectResolverIntoTheSpawner(t *testing.T) {
	cfg := ports.SpawnConfig{
		ProjectID:    "proj-1",
		IssueID:      "issue-7",
		Prompt:       "Implement the router.",
		IssueContext: strings.Repeat("tracker issue body. ", 200),
	}

	spawner := &recordingSpawner{}
	opts, diff, _ := testSources()
	routed := Instrument(workflowcore.Deps{Spawner: spawner, Projects: registeredProject()}, newRouter(t, opts), nil)
	if _, _, _, err := routed.Spawner.Spawn(stdctx.Background(), cfg); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.Contains(spawner.got.IssueContext, changedFile) {
		t.Fatalf("Instrument did not pass the resolver through; routing lost its diff evidence: %q", spawner.got.IssueContext)
	}
	if len(diff.roots) == 0 || diff.roots[0] != testProjectRoot {
		t.Fatalf("the resolved root did not reach the diff source: %v", diff.roots)
	}

	// Without a Projects port in Deps there is no root to resolve, so the
	// spawn keeps its original context.
	bare := &recordingSpawner{}
	bareOpts, bareDiff, _ := testSources()
	unresolvable := Instrument(workflowcore.Deps{Spawner: bare}, newRouter(t, bareOpts), nil)
	if _, _, _, err := unresolvable.Spawner.Spawn(stdctx.Background(), cfg); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if bare.got.IssueContext != cfg.IssueContext {
		t.Fatalf("a spawn with no project resolver was routed anyway: %q", bare.got.IssueContext)
	}
	if len(bareDiff.roots) != 0 {
		t.Fatalf("the diff source was consulted with no resolver: %v", bareDiff.roots)
	}
}

// The planner carries its own checkout path, and the same rule applies to it:
// no absolute root, no routing.
func TestPlannerIsUnroutedWithoutAnAbsoluteRoot(t *testing.T) {
	for name, path := range map[string]string{"empty": "", "relative": "checkouts/proj-1"} {
		t.Run(name, func(t *testing.T) {
			planner := &recordingPlanner{}
			opts, diff, _ := testSources()
			wrapped := InstrumentPlanner(planner, newRouter(t, opts), nil)
			request := plannerRequest()
			request.Project.Path = path
			if _, err := wrapped.Generate(stdctx.Background(), request); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if len(planner.got.Context.Documents) != len(request.Context.Documents) {
				t.Fatalf("an unroutable planner request was rewritten: %d documents, want %d", len(planner.got.Context.Documents), len(request.Context.Documents))
			}
			for i, doc := range planner.got.Context.Documents {
				if doc != request.Context.Documents[i] {
					t.Fatalf("document %d was altered without a checkout root: %+v", i, doc)
				}
			}
			if len(diff.roots) != 0 {
				t.Fatalf("the diff source was consulted without a root: %v", diff.roots)
			}
		})
	}
}
