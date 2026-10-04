package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baselineevidence "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type fenceChainSink struct{ n int }

func (s *fenceChainSink) Write(_ context.Context, r baselineevidence.EvidenceRecord) (string, error) {
	s.n++
	return "/dev/null/" + r.RecordID, nil
}

// The fence has to survive the WHOLE production chain, not just the concrete
// launcher: instrumentAgentDispatch wraps the reviewer launcher in the
// baseline recorder, the context router and the memory provisioner, every one
// of them switched ON here. Each decorator rebuilds or forwards the request;
// if any dropped LaunchFence, a stale dispatch would reach runtime.Create
// with no last-moment check (Codex, 3D cycle 3, P2).
func TestReviewerLaunchFenceSurvivesTheProductionDecoratorChain(t *testing.T) {
	runtime := &fakeWorkflowReviewerRuntime{}
	concrete := &workflowReviewerLauncher{
		reviewers: &fakeReviewerResolver{adapter: &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{Argv: []string{"codex"}}}},
		runtime:   runtime,
		dataDir:   t.TempDir(),
	}
	router, err := contextrouter.Default(nil)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	sink := &fenceChainSink{}
	recorder := baselineevidence.NewRecorder(sink, baselineevidence.WithClock(func() time.Time {
		return time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	}))
	prov := &markerProvisioner{}
	deps := instrumentAgentDispatch(workflowcore.Deps{
		Projects:         wiringProjects{},
		ReviewerLauncher: concrete,
	}, recorder, router, prov, nil)
	if deps.ReviewerLauncher == workflowcore.ReviewerLauncher(concrete) {
		t.Fatal("fixture broken: the decorators are off, the chain under test is just the launcher")
	}

	fenced := errors.New("fenced: generation superseded")
	fenceCalls := 0
	req := workflowcore.ReviewerLaunchRequest{
		Harness: domain.ReviewerCodex, WorkerSessionID: "sess-1", ProjectID: "proj-1",
		ReviewID: "review-1", RunID: "run-1", WorkspacePath: "/ws/wf", Prompt: "review it",
		LaunchFence: func(context.Context) error { fenceCalls++; return fenced },
	}
	if _, err := deps.ReviewerLauncher.Launch(context.Background(), req); !errors.Is(err, fenced) {
		t.Fatalf("decorated Launch error = %v, want the fence's refusal", err)
	}
	if fenceCalls != 1 {
		t.Fatalf("fence consulted %d times through the chain, want once (a decorator dropped or duplicated it)", fenceCalls)
	}
	if runtime.calls != 0 || runtime.lastCfg.SessionID != "" {
		t.Fatalf("runtime.Create reached past a refused fence through the chain (calls=%d)", runtime.calls)
	}
	if len(prov.roles) == 0 {
		t.Fatal("fixture broken: the memory decorator never ran, so it was not part of the chain")
	}

	// An authorizing fence launches through the same chain, consulted before
	// the runtime creates anything.
	createsBeforeFence := -1
	req.LaunchFence = func(context.Context) error { createsBeforeFence = runtime.calls; return nil }
	if _, err := deps.ReviewerLauncher.Launch(context.Background(), req); err != nil {
		t.Fatalf("decorated Launch with an authorizing fence: %v", err)
	}
	if createsBeforeFence != 0 || runtime.calls != 1 {
		t.Fatalf("fence saw %d creates before it ran, runtime calls=%d; want 0 then 1", createsBeforeFence, runtime.calls)
	}
}
