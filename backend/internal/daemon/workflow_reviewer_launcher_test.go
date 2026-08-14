package daemon

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeReviewerAdapter is a hand-rolled fake ports.Reviewer that records
// exactly what ReviewCommand was called with, so the test below can assert
// workflowReviewerLauncher reuses the adapter's own command/config
// resolution rather than constructing a permissive one itself (Checkpoint
// 8C, test item #18).
type fakeReviewerAdapter struct {
	lastInvocation ports.ReviewInvocation
	cmd            ports.ReviewCommandSpec
	err            error
}

func (f *fakeReviewerAdapter) ReviewCommand(_ context.Context, inv ports.ReviewInvocation) (ports.ReviewCommandSpec, error) {
	f.lastInvocation = inv
	if f.err != nil {
		return ports.ReviewCommandSpec{}, f.err
	}
	return f.cmd, nil
}

func (f *fakeReviewerAdapter) ReviewMessage(_ context.Context, _ ports.ReviewInvocation) (string, error) {
	return "", nil
}

type fakeReviewerResolver struct {
	adapter *fakeReviewerAdapter
}

func (f *fakeReviewerResolver) Reviewer(_ domain.ReviewerHarness) (ports.Reviewer, bool) {
	if f.adapter == nil {
		return nil, false
	}
	return f.adapter, true
}

type fakeWorkflowReviewerRuntime struct {
	lastCfg ports.RuntimeConfig
	calls   int
}

func (f *fakeWorkflowReviewerRuntime) Create(_ context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	f.calls++
	f.lastCfg = cfg
	return ports.RuntimeHandle{ID: "pane-1"}, nil
}

// TestWorkflowReviewerLauncherReusesAdapterCommandUnmodified asserts that
// Launch calls the real adapter's ReviewCommand (the unmodified
// claudecode.go allowlist/denylist resolution) and passes its Argv/Env
// straight through to the runtime — the launcher never builds its own tool
// configuration.
func TestWorkflowReviewerLauncherReusesAdapterCommandUnmodified(t *testing.T) {
	adapter := &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{
		Argv: []string{"claude", "--some-real-flag"},
		Env:  map[string]string{"FROM_ADAPTER": "1"},
	}}
	runtime := &fakeWorkflowReviewerRuntime{}
	l := &workflowReviewerLauncher{
		reviewers: &fakeReviewerResolver{adapter: adapter},
		runtime:   runtime,
		dataDir:   t.TempDir(),
	}

	req := workflowcore.ReviewerLaunchRequest{
		Harness:         domain.ReviewerClaudeCode,
		WorkerSessionID: "sess-1",
		ProjectID:       "proj-1",
		ReviewID:        "review-1",
		RunID:           "run-1",
		WorkspacePath:   "/ws/wf",
		Prompt:          "review this worktree via git status/git diff",
	}
	result, err := l.Launch(context.Background(), req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.HandleID != "pane-1" {
		t.Fatalf("handle id = %q, want pane-1", result.HandleID)
	}
	if len(runtime.lastCfg.Argv) != 2 || runtime.lastCfg.Argv[0] != "claude" || runtime.lastCfg.Argv[1] != "--some-real-flag" {
		t.Fatalf("runtime Argv = %v, want the adapter's own argv passed through unmodified", runtime.lastCfg.Argv)
	}
	if runtime.lastCfg.Env["FROM_ADAPTER"] != "1" {
		t.Fatalf("runtime env missing adapter-provided env var: %+v", runtime.lastCfg.Env)
	}
	if adapter.lastInvocation.Prompt == "" {
		t.Fatalf("adapter was not given the workflow-owned prompt")
	}
}
