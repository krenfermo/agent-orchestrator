package wfmemory_test

import (
	stdctx "context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory/wfmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// wfmemory_test.go — the wrappers that make memory automatic.
//
// They are tested against a fake provisioner rather than a real store, because
// what is under test here is the wrapping contract: does the memory reach the
// right channel, is the instruction left alone, does a dispatch survive a
// provisioner that has nothing to give, and are the metrics carried forward.

// fakeProvisioner returns a fixed answer and records what it was asked.
type fakeProvisioner struct {
	pack     projectmemory.ContextPack
	legacy   []projectmemory.LegacyDocument
	metrics  baseline.MemoryMetrics
	requests []projectmemory.ProvisionRequest
}

func (f *fakeProvisioner) Provision(_ stdctx.Context, req projectmemory.ProvisionRequest) projectmemory.Provisioned {
	f.requests = append(f.requests, req)
	legacy := f.legacy
	if legacy == nil {
		legacy = req.Legacy
	}
	return projectmemory.Provisioned{
		Mode: projectmemory.ModeAssisted, Pack: f.pack, Legacy: legacy, Metrics: f.metrics,
	}
}

// attachingProvisioner builds a provisioner whose pack carries one fact, which
// is enough for Attached() to be true and for Render() to produce text.
func attachingProvisioner() *fakeProvisioner {
	pack := projectmemory.ContextPack{
		Role: projectmemory.RoleWorker,
		Sections: []projectmemory.PackSection{{
			Title: "Conventions", Type: domain.MemoryTypeConvention,
			Items: []projectmemory.SelectedItem{{
				Item: domain.ProjectMemoryItem{
					Summary: "AGENTS.md: keep every change surgical", Content: "the body",
				},
				BodyIncluded: true,
			}},
		}},
		Stats:  projectmemory.PackStats{SelectedItems: 1, SelectedBytes: 64},
		Digest: "packdigest",
	}
	return &fakeProvisioner{
		pack:    pack,
		metrics: baseline.MemoryMetrics{Mode: "assisted", PackItems: 1, PackBytes: 64},
	}
}

type stubProjects struct {
	record domain.ProjectRecord
	found  bool
}

func (s *stubProjects) GetProject(stdctx.Context, string) (domain.ProjectRecord, bool, error) {
	return s.record, s.found, nil
}

func registeredProject() *stubProjects {
	return &stubProjects{record: domain.ProjectRecord{ID: "proj-1", Path: "/checkout/proj"}, found: true}
}

// --- planner ---------------------------------------------------------------

type recordingPlanner struct {
	got workflowcore.PlannerRequest
}

func (p *recordingPlanner) Generate(_ stdctx.Context, req workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	p.got = req
	return workflowcore.PlannerResponse{}, nil
}

func plannerRequest() workflowcore.PlannerRequest {
	return workflowcore.PlannerRequest{
		Objective: "Add a durable capacity scheduler to the workflow package.",
		Project:   domain.ProjectRecord{ID: "proj-1", Path: "/checkout/proj"},
		Context: workflowcore.PlannerContext{
			Version:   workflowcore.PlannerContextVersion,
			ProjectID: "proj-1",
			Documents: []workflowcore.PlannerDocument{
				{Path: "AGENTS.md", SHA256: "aaa", Content: "repository conventions"},
				{Path: "README.md", SHA256: "bbb", Content: "how to run the daemon"},
			},
		},
	}
}

func TestPlannerReceivesMemoryAsADocument(t *testing.T) {
	next := &recordingPlanner{}
	prov := attachingProvisioner()
	wrapped := wfmemory.InstrumentPlanner(next, prov, nil)

	if _, err := wrapped.Generate(stdctx.Background(), plannerRequest()); err != nil {
		t.Fatal(err)
	}
	var memoryDoc *workflowcore.PlannerDocument
	for i, doc := range next.got.Context.Documents {
		if strings.HasPrefix(doc.Path, "ao://project-memory/") {
			memoryDoc = &next.got.Context.Documents[i]
		}
	}
	if memoryDoc == nil {
		t.Fatalf("the planner received no memory document: %+v", next.got.Context.Documents)
	}
	if !strings.Contains(memoryDoc.Content, "keep every change surgical") {
		t.Errorf("the memory document carries no facts:\n%s", memoryDoc.Content)
	}
	if memoryDoc.SHA256 != "packdigest" {
		t.Errorf("the memory document is not stamped with the pack digest: %q", memoryDoc.SHA256)
	}
	if next.got.Objective != plannerRequest().Objective {
		t.Error("the wrapper modified the objective, which carries the instruction")
	}
	// The legacy documents the provisioner kept must survive, in order.
	if len(next.got.Context.Documents) != 3 {
		t.Fatalf("documents = %d, want the two legacy plus one memory", len(next.got.Context.Documents))
	}
}

// In preferred mode the provisioner drops a covered document, and the wrapper
// must actually stop sending it.
func TestPlannerDropsTheDocumentsMemoryReplaced(t *testing.T) {
	next := &recordingPlanner{}
	prov := attachingProvisioner()
	prov.legacy = []projectmemory.LegacyDocument{{Path: "README.md", SHA256: "bbb", Content: "how to run the daemon"}}
	wrapped := wfmemory.InstrumentPlanner(next, prov, nil)

	if _, err := wrapped.Generate(stdctx.Background(), plannerRequest()); err != nil {
		t.Fatal(err)
	}
	for _, doc := range next.got.Context.Documents {
		if doc.Path == "AGENTS.md" {
			t.Fatal("a document memory replaced was still sent")
		}
	}
	if len(next.got.Context.Documents) != 2 {
		t.Fatalf("documents = %d, want README.md plus the memory pack", len(next.got.Context.Documents))
	}
}

// A provisioner with nothing to give leaves the request exactly as it was.
func TestPlannerWithNoMemoryIsUnchanged(t *testing.T) {
	next := &recordingPlanner{}
	wrapped := wfmemory.InstrumentPlanner(next, &fakeProvisioner{
		metrics: baseline.MemoryMetrics{Mode: "assisted", FallbackReason: "not indexed"},
	}, nil)

	before := plannerRequest()
	if _, err := wrapped.Generate(stdctx.Background(), before); err != nil {
		t.Fatal(err)
	}
	if len(next.got.Context.Documents) != len(before.Context.Documents) {
		t.Fatalf("documents = %d, want the original %d", len(next.got.Context.Documents), len(before.Context.Documents))
	}
	for i, doc := range next.got.Context.Documents {
		if doc.Path != before.Context.Documents[i].Path || doc.Content != before.Context.Documents[i].Content {
			t.Fatalf("document %d was modified", i)
		}
	}
}

// The optional descriptor capability must survive wrapping, or every plan
// would be recorded against an unknown provider.
type describingPlanner struct{ recordingPlanner }

func (d *describingPlanner) Descriptor() (string, string) { return "claude", "opus" }

func TestPlannerWrapperPreservesTheDescriptor(t *testing.T) {
	wrapped := wfmemory.InstrumentPlanner(&describingPlanner{}, attachingProvisioner(), nil)
	desc, ok := wrapped.(workflowcore.PlannerDescriptor)
	if !ok {
		t.Fatal("the wrapper dropped the PlannerDescriptor capability")
	}
	if provider, model := desc.Descriptor(); provider != "claude" || model != "opus" {
		t.Fatalf("descriptor = %s/%s", provider, model)
	}
}

// --- spawner ---------------------------------------------------------------

type recordingSpawner struct{ got ports.SpawnConfig }

func (s *recordingSpawner) Spawn(_ stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.got = cfg
	return domain.SessionRecord{}, 0, 0, nil
}

func TestSpawnerAppendsMemoryToTheIssueContext(t *testing.T) {
	next := &recordingSpawner{}
	wrapped := wfmemory.InstrumentSpawner(next, attachingProvisioner(), registeredProject(), nil)

	cfg := ports.SpawnConfig{
		ProjectID: "proj-1", Prompt: "implement the scheduler",
		IssueContext: "the tracker says the queue is unbounded",
	}
	if _, _, _, err := wrapped.Spawn(stdctx.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next.got.IssueContext, "the tracker says") {
		t.Error("the pre-fetched issue context was lost")
	}
	if !strings.Contains(next.got.IssueContext, "keep every change surgical") {
		t.Error("memory did not reach the worker's issue context")
	}
	if next.got.Prompt != cfg.Prompt {
		t.Error("the wrapper modified the prompt, which carries the instruction")
	}
}

// A spawn whose project has no resolvable root goes out untouched — the same
// rule the router's spawner applies.
func TestSpawnerWithoutAProjectRootIsUnchanged(t *testing.T) {
	next := &recordingSpawner{}
	wrapped := wfmemory.InstrumentSpawner(next, attachingProvisioner(), &stubProjects{}, nil)

	cfg := ports.SpawnConfig{ProjectID: "proj-1", Prompt: "do the thing", IssueContext: "original"}
	if _, _, _, err := wrapped.Spawn(stdctx.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if next.got.IssueContext != "original" {
		t.Fatalf("issue context = %q, want it untouched", next.got.IssueContext)
	}
}

// --- reviewer --------------------------------------------------------------

type recordingReviewer struct {
	got workflowcore.ReviewerLaunchRequest
}

func (r *recordingReviewer) Preflight(stdctx.Context, domain.ReviewerHarness, string) error {
	return nil
}

func (r *recordingReviewer) Launch(_ stdctx.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	r.got = req
	return workflowcore.ReviewerLaunchResult{}, nil
}

func (r *recordingReviewer) ReviewerIdentity(req workflowcore.ReviewerLaunchRequest) string {
	return "reviewer:" + req.RunID
}

func (r *recordingReviewer) ProbeReviewer(stdctx.Context, workflowcore.ReviewerRef) (workflowcore.ReviewerObservation, error) {
	return workflowcore.ReviewerObservation{}, nil
}

func (r *recordingReviewer) CancelReviewer(stdctx.Context, workflowcore.ReviewerRef) error {
	return nil
}

func TestReviewerReceivesMemoryAsItsSystemPrompt(t *testing.T) {
	next := &recordingReviewer{}
	wrapped := wfmemory.InstrumentReviewerLauncher(next, attachingProvisioner(), registeredProject(), nil)

	req := workflowcore.ReviewerLaunchRequest{
		ProjectID: "proj-1", RunID: "run-1", WorkspacePath: "/checkout/proj",
		Prompt: "review the change against its acceptance criteria",
	}
	if _, err := wrapped.Launch(stdctx.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next.got.SystemPrompt, "keep every change surgical") {
		t.Fatalf("the reviewer got no standing memory:\n%s", next.got.SystemPrompt)
	}
	if next.got.Prompt != req.Prompt {
		t.Error("the wrapper modified the review prompt")
	}
}

// An existing system prompt is an instruction, and must never be replaced.
func TestReviewerNeverReplacesAnExistingSystemPrompt(t *testing.T) {
	next := &recordingReviewer{}
	wrapped := wfmemory.InstrumentReviewerLauncher(next, attachingProvisioner(), registeredProject(), nil)

	req := workflowcore.ReviewerLaunchRequest{
		ProjectID: "proj-1", RunID: "run-1", WorkspacePath: "/checkout/proj",
		SystemPrompt: "You are a reviewer. Do exactly this.",
	}
	if _, err := wrapped.Launch(stdctx.Background(), req); err != nil {
		t.Fatal(err)
	}
	if next.got.SystemPrompt != req.SystemPrompt {
		t.Fatalf("system prompt = %q, want it untouched", next.got.SystemPrompt)
	}
}

// The recovery half of the launcher must pass through untouched: identity in
// particular has to stay byte-stable across restarts.
func TestReviewerIdentityIsUnaffectedByMemory(t *testing.T) {
	next := &recordingReviewer{}
	wrapped := wfmemory.InstrumentReviewerLauncher(next, attachingProvisioner(), registeredProject(), nil)
	req := workflowcore.ReviewerLaunchRequest{ProjectID: "proj-1", RunID: "run-1"}
	if got, want := wrapped.ReviewerIdentity(req), next.ReviewerIdentity(req); got != want {
		t.Fatalf("identity = %q, want the wrapped launcher's %q", got, want)
	}
}

// --- instrument ------------------------------------------------------------

// A nil provisioner is memory switched off, and must hand every dependency
// back untouched rather than wrap it in a pass-through.
func TestInstrumentIsANoOpWithoutAProvisioner(t *testing.T) {
	planner := &recordingPlanner{}
	spawner := &recordingSpawner{}
	reviewer := &recordingReviewer{}
	deps := workflowcore.Deps{Planner: planner, Spawner: spawner, ReviewerLauncher: reviewer}

	out := wfmemory.Instrument(deps, nil, nil)
	if out.Planner != workflowcore.Planner(planner) ||
		out.Spawner != workflowcore.Spawner(spawner) ||
		out.ReviewerLauncher != workflowcore.ReviewerLauncher(reviewer) {
		t.Fatal("a nil provisioner still wrapped the dependencies")
	}
}

// The verify surface carries a command to run, not context, and must stay out
// of this.
func TestInstrumentLeavesTheVerifierAlone(t *testing.T) {
	deps := workflowcore.Deps{
		Planner: &recordingPlanner{}, Spawner: &recordingSpawner{},
		Projects: registeredProject(),
	}
	before := deps
	out := wfmemory.Instrument(deps, attachingProvisioner(), nil)
	if out.Verifier != before.Verifier {
		t.Error("Instrument touched the verifier")
	}
	if out.MessageSender != before.MessageSender {
		t.Error("Instrument touched the fix message sender")
	}
	if out.Planner == before.Planner || out.Spawner == before.Spawner {
		t.Error("Instrument did not wrap the surfaces it provisions")
	}
}

// Every wrapper must leave its metrics on the context, or the evidence record
// cannot answer why a role received what it did.
func TestWrappersCarryTheirMetricsOnTheContext(t *testing.T) {
	prov := attachingProvisioner()
	prov.metrics.FallbackReason = "carried"

	var seen bool
	probe := &metricProbe{onCall: func(ctx stdctx.Context) {
		metrics, ok := baseline.MemoryFromContext(ctx)
		seen = ok && metrics.FallbackReason == "carried"
	}}
	wrapped := wfmemory.InstrumentSpawner(probe, prov, registeredProject(), nil)
	if _, _, _, err := wrapped.Spawn(stdctx.Background(), ports.SpawnConfig{ProjectID: "proj-1"}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("the dispatch did not carry the memory metrics")
	}
}

type metricProbe struct{ onCall func(stdctx.Context) }

func (m *metricProbe) Spawn(ctx stdctx.Context, _ ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	m.onCall(ctx)
	return domain.SessionRecord{}, 0, 0, nil
}
