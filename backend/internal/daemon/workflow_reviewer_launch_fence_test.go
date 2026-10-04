package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// The launch fence is consulted at the last moment before the runtime makes
// the reviewer exist, and a refusal creates nothing.
func TestWorkflowReviewerLauncherLaunchFenceRefusalCreatesNothing(t *testing.T) {
	fenced := errors.New("fenced: generation superseded")
	runtime := &fakeWorkflowReviewerRuntime{}
	l := &workflowReviewerLauncher{
		reviewers: &fakeReviewerResolver{adapter: &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{Argv: []string{"codex"}}}},
		runtime:   runtime,
		dataDir:   t.TempDir(),
	}
	fenceCalls := 0
	req := workflowcore.ReviewerLaunchRequest{
		Harness: domain.ReviewerCodex, WorkerSessionID: "sess-1", ProjectID: "proj-1",
		ReviewID: "review-1", RunID: "run-1", WorkspacePath: "/ws/wf",
		LaunchFence: func(context.Context) error { fenceCalls++; return fenced },
	}
	if _, err := l.Launch(context.Background(), req); !errors.Is(err, fenced) {
		t.Fatalf("Launch error = %v, want the fence's refusal returned as-is", err)
	}
	if fenceCalls != 1 {
		t.Fatalf("fence consulted %d times, want once", fenceCalls)
	}
	if runtime.calls != 0 || runtime.lastCfg.SessionID != "" {
		t.Fatalf("the runtime was asked to create a reviewer past a refused fence (calls=%d)", runtime.calls)
	}

	// An authorizing fence launches normally, and is consulted before Create.
	createdBeforeFence := -1
	req.LaunchFence = func(context.Context) error { createdBeforeFence = runtime.calls; return nil }
	if _, err := l.Launch(context.Background(), req); err != nil {
		t.Fatalf("Launch with an authorizing fence: %v", err)
	}
	if createdBeforeFence != 0 || runtime.calls != 1 {
		t.Fatalf("fence saw %d creates before it ran, runtime calls=%d; want 0 then 1", createdBeforeFence, runtime.calls)
	}
}
