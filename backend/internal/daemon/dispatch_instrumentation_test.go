package daemon

import (
	"context"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter/wfrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory/wfdispatch"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory/wfmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// dispatch_instrumentation_test.go — Frente 3 / 3B wiring regression.
//
// The bug these guard: from P5-A 2C (2026-09-08) the worker launcher held the
// raw session manager, so AO_MEMORY_MODE / AO_CONTEXT_ROUTER reached the
// Planner and the Reviewer and never the Worker. Nothing failed; the worker
// simply received less. These tests make that shape fail loudly.

// packMarker is the text the fake provisioner's pack renders to. Finding it
// on a surface is proof that surface received project memory.
const packMarker = "AO-3B-WIRING-PACK-MARKER"

type markerProvisioner struct {
	roles []projectmemory.PackRole
}

func (m *markerProvisioner) Provision(_ context.Context, req projectmemory.ProvisionRequest) projectmemory.Provisioned {
	m.roles = append(m.roles, req.Role)
	return projectmemory.Provisioned{
		Mode: projectmemory.ModeAssisted,
		Pack: projectmemory.ContextPack{
			Role: req.Role,
			Sections: []projectmemory.PackSection{{
				Title: "Conventions", Type: domain.MemoryTypeConvention,
				Items: []projectmemory.SelectedItem{{
					Item: domain.ProjectMemoryItem{Summary: packMarker},
				}},
			}},
			Stats:  projectmemory.PackStats{SelectedItems: 1, SelectedBytes: 32},
			Digest: "digest",
		},
		Legacy: req.Legacy,
	}
}

type wiringProjects struct{}

func (wiringProjects) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	return domain.ProjectRecord{ID: "proj-1", Path: "/checkout/proj"}, true, nil
}

// recordingSpawner is the raw transport: what reaches it is what the agent
// process would be started with.
type recordingSpawner struct{ got []ports.SpawnConfig }

func (r *recordingSpawner) Spawn(_ context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	r.got = append(r.got, cfg)
	return domain.SessionRecord{ID: "sess-1"}, 0, 0, nil
}

type recordingPlanner struct{ got []workflowcore.PlannerRequest }

func (r *recordingPlanner) Generate(_ context.Context, req workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	r.got = append(r.got, req)
	return workflowcore.PlannerResponse{}, nil
}

// recordingReviewer implements Launch; every other method of the port is
// promoted from the nil embedded interface and must not be reached here.
type recordingReviewer struct {
	workflowcore.ReviewerLauncher
	got []workflowcore.ReviewerLaunchRequest
}

func (r *recordingReviewer) Launch(_ context.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	r.got = append(r.got, req)
	return workflowcore.ReviewerLaunchResult{}, nil
}

type wiringFixture struct {
	deps     workflowcore.Deps
	spawner  *recordingSpawner
	planner  *recordingPlanner
	reviewer *recordingReviewer
}

// newWiringFixture builds Deps the way workflow_wiring.go does: the worker
// launcher is constructed with the RAW spawner, before any decoration.
func newWiringFixture(t *testing.T) wiringFixture {
	t.Helper()
	sp := &recordingSpawner{}
	pl := &recordingPlanner{}
	rv := &recordingReviewer{}
	return wiringFixture{
		spawner: sp, planner: pl, reviewer: rv,
		deps: workflowcore.Deps{
			Projects:         wiringProjects{},
			Spawner:          sp,
			Planner:          pl,
			ReviewerLauncher: rv,
			WorkerLauncher:   &workflowWorkerLauncher{spawner: sp, dataDir: t.TempDir()},
		},
	}
}

func (f wiringFixture) dispatchAll(t *testing.T, deps workflowcore.Deps) {
	t.Helper()
	ctx := context.Background()
	if _, err := deps.Planner.Generate(ctx, workflowcore.PlannerRequest{
		Objective: "plan it",
		Project:   domain.ProjectRecord{ID: "proj-1", Path: "/checkout/proj"},
	}); err != nil {
		t.Fatalf("planner: %v", err)
	}
	if _, err := deps.ReviewerLauncher.Launch(ctx, workflowcore.ReviewerLaunchRequest{
		ProjectID: "proj-1", Prompt: "review it",
	}); err != nil {
		t.Fatalf("reviewer: %v", err)
	}
	if _, err := deps.WorkerLauncher.LaunchWorker(ctx, workflowcore.WorkerLaunchRequest{
		RunID: "wf-1", StepID: "wfs-1", AttemptID: "att-1",
		ProjectID: "proj-1", WorkflowRunID: "wf-1", Prompt: "do the work",
	}); err != nil {
		t.Fatalf("worker: %v", err)
	}
}

func (f wiringFixture) plannerGotMemory() bool {
	for _, req := range f.planner.got {
		for _, doc := range req.Context.Documents {
			if strings.Contains(doc.Content, packMarker) {
				return true
			}
		}
	}
	return false
}

func (f wiringFixture) reviewerGotMemory() bool {
	for _, req := range f.reviewer.got {
		if strings.Contains(req.SystemPrompt, packMarker) {
			return true
		}
	}
	return false
}

func (f wiringFixture) workerGotMemory() bool {
	for _, cfg := range f.spawner.got {
		if strings.Contains(cfg.IssueContext, packMarker) {
			return true
		}
	}
	return false
}

// The contract: when memory is on, every role that receives it receives it --
// in particular, never Planner/Reviewer without the Worker.
func TestMemoryReachesWorkerWheneverItReachesPlannerOrReviewer(t *testing.T) {
	f := newWiringFixture(t)
	prov := &markerProvisioner{}
	deps := instrumentAgentDispatch(f.deps, nil, nil, prov, nil)
	f.dispatchAll(t, deps)

	planner, reviewer, worker := f.plannerGotMemory(), f.reviewerGotMemory(), f.workerGotMemory()
	if !planner || !reviewer {
		t.Fatalf("fixture broken: planner=%v reviewer=%v should both receive memory", planner, reviewer)
	}
	if !worker {
		t.Fatalf("Planner and Reviewer received project memory but the Worker did not: " +
			"the worker launcher is bypassing the decorated Spawner")
	}
	if len(f.spawner.got) != 1 {
		t.Fatalf("worker spawns reaching the transport = %d, want exactly 1 (no double spawn)", len(f.spawner.got))
	}
	gotWorkerRole := false
	for _, r := range prov.roles {
		if r == projectmemory.RoleWorker {
			gotWorkerRole = true
		}
	}
	if !gotWorkerRole {
		t.Fatalf("provisioner roles = %v, want a %q request from the worker launch", prov.roles, projectmemory.RoleWorker)
	}
}

// The pre-3B composition, reproduced verbatim: decorate, and do NOT re-bind.
// This is the regression the test above exists for; asserting it here proves
// the test above can fail, rather than passing because the fixture is inert.
func TestPreFixCompositionLeftTheWorkerWithoutMemory(t *testing.T) {
	f := newWiringFixture(t)
	deps := wfdispatch.Instrument(f.deps, nil, nil)
	deps = wfrouter.Instrument(deps, nil, nil)
	deps = wfmemory.Instrument(deps, &markerProvisioner{}, nil)
	f.dispatchAll(t, deps)

	if !f.plannerGotMemory() || !f.reviewerGotMemory() {
		t.Fatal("fixture broken: planner and reviewer should receive memory under either composition")
	}
	if f.workerGotMemory() {
		t.Fatal("the pre-fix composition unexpectedly delivered memory to the worker; " +
			"this test no longer reproduces the bug and the regression test above proves nothing")
	}
}

// Memory OFF (the default) must leave every surface undecorated: the worker's
// spawn reaches the transport with no memory and the provisioner is never asked.
func TestMemoryOffLeavesDispatchUntouched(t *testing.T) {
	f := newWiringFixture(t)
	deps := instrumentAgentDispatch(f.deps, nil, nil, nil, nil)
	f.dispatchAll(t, deps)

	if f.plannerGotMemory() || f.reviewerGotMemory() || f.workerGotMemory() {
		t.Fatal("memory off must attach nothing to any surface")
	}
	if deps.Planner != f.deps.Planner || deps.ReviewerLauncher != f.deps.ReviewerLauncher || deps.Spawner != f.deps.Spawner {
		t.Fatal("memory off must hand the dispatch surfaces back unwrapped")
	}
	if len(f.spawner.got) != 1 || strings.TrimSpace(f.spawner.got[0].IssueContext) != "" {
		t.Fatalf("worker spawn with memory off = %+v, want one spawn with an empty issue context", f.spawner.got)
	}
}

// bindWorkerTransport copies; it never rebinds the launcher it was handed.
func TestBindWorkerTransportDoesNotMutateTheOriginal(t *testing.T) {
	raw := &recordingSpawner{}
	original := &workflowWorkerLauncher{spawner: raw}
	decorated := &recordingSpawner{}
	deps := bindWorkerTransport(workflowcore.Deps{Spawner: decorated, WorkerLauncher: original})
	if original.spawner != raw {
		t.Fatal("bindWorkerTransport mutated the launcher it was given")
	}
	bound, ok := deps.WorkerLauncher.(*workflowWorkerLauncher)
	if !ok || bound.spawner != decorated {
		t.Fatalf("bound launcher = %#v, want a copy transporting through deps.Spawner", deps.WorkerLauncher)
	}
}

// launchMethods are the method names that start, re-start or message an agent
// process. A Deps field whose interface carries one of them is a dispatch
// surface and must be classified in dispatchSurfaces.
var launchMethods = map[string]bool{
	"Spawn": true, "Launch": true, "LaunchWorker": true, "LaunchDiagnostic": true,
	"LaunchRepair": true, "Send": true, "SwitchAgent": true, "Generate": true, "Run": true,
}

// Any new launcher must be classified: decorated, transported, or exempt with
// a reason. This is what stops a future launcher from silently avoiding the
// context provider the way the worker launcher did.
func TestEveryAgentDispatchSurfaceIsClassified(t *testing.T) {
	depsType := reflect.TypeOf(workflowcore.Deps{})
	seen := map[string]bool{}
	var unclassified []string
	for i := 0; i < depsType.NumField(); i++ {
		field := depsType.Field(i)
		if field.Type.Kind() != reflect.Interface {
			continue
		}
		launches := false
		for m := 0; m < field.Type.NumMethod(); m++ {
			if launchMethods[field.Type.Method(m).Name] {
				launches = true
				break
			}
		}
		if !launches {
			continue
		}
		seen[field.Name] = true
		surface, ok := dispatchSurfaces[field.Name]
		if !ok {
			unclassified = append(unclassified, field.Name)
			continue
		}
		if strings.TrimSpace(surface.reason) == "" {
			t.Errorf("dispatch surface %s is classified %q with no reason", field.Name, surface.class)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf("workflowcore.Deps has agent dispatch surfaces with no context classification: %v. "+
			"Add each to dispatchSurfaces as decorated, transported (and bind it in bindWorkerTransport) "+
			"or exempt with a reason", unclassified)
	}
	for name := range dispatchSurfaces {
		if !seen[name] {
			t.Errorf("dispatchSurfaces lists %s, which is not an agent dispatch surface of workflowcore.Deps", name)
		}
	}
}

// Every "transported" surface must actually be rebound by bindWorkerTransport,
// and every "decorated" one must be wrapped when memory is on.
func TestClassifiedSurfacesAreActuallyInstrumented(t *testing.T) {
	f := newWiringFixture(t)
	deps := instrumentAgentDispatch(f.deps, nil, nil, &markerProvisioner{}, nil)
	for name, surface := range dispatchSurfaces {
		switch surface.class {
		case surfaceDecorated:
			before := reflect.ValueOf(f.deps).FieldByName(name).Interface()
			after := reflect.ValueOf(deps).FieldByName(name).Interface()
			if before == after {
				t.Errorf("%s is classified decorated but memory-on instrumentation left it unwrapped", name)
			}
		case surfaceTransported:
			if _, ok := reflect.ValueOf(deps).FieldByName(name).Interface().(spawnerTransport); !ok {
				t.Errorf("%s is classified transported but is not a spawnerTransport", name)
			}
		}
	}
}

// The daemon must compose the dispatch decorators in exactly one place. A
// second, direct call to any of them would decorate a surface without the
// worker re-bind that follows -- the P5-A 2C bug by another route.
func TestDecoratorsAreComposedOnlyByInstrumentAgentDispatch(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{"wfdispatch.Instrument(", "wfrouter.Instrument(", "wfmemory.Instrument("}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			name == "dispatch_instrumentation.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range calls {
			if strings.Contains(string(src), call) {
				t.Errorf("%s calls %s directly; compose decorators only through instrumentAgentDispatch", name, call)
			}
		}
	}
	wiring, err := os.ReadFile("workflow_wiring.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wiring), "instrumentAgentDispatch(") {
		t.Fatal("workflow_wiring.go no longer calls instrumentAgentDispatch: dispatch surfaces are undecorated")
	}
}
