package sessionmanager

import (
	"context"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AR-1a (Codex AR1A-INT-01): a terminal spawn of a workspace project hands the
// agent its child roots, so a sandboxed Codex launch can make them (and their
// repositories' git data) explicitly writable. A single-repo project hands
// none.
func TestWorkspaceProjectSpawnHandsTheAgentItsChildRoots(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: "/repo/mer", Kind: domain.ProjectKindWorkspace, Config: testRoleAgents()}
	st.workspaceRepo["mer"] = []domain.WorkspaceRepoRecord{{Name: "api", RelativePath: "api"}, {Name: "web", RelativePath: "web"}}
	agent := &recordingAgent{}
	m := New(Deps{
		Runtime: &fakeRuntime{}, Agents: singleAgent{agent: agent}, Workspace: &fakeWorkspace{},
		Store: st, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st},
		LookPath: func(string) (string, error) { return "/bin/true", nil },
	})
	if _, _, _, err := m.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Prompt: "fix it"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	want := []string{"/ws/mer-1/api", "/ws/mer-1/web"}
	if got := agent.lastLaunch.AdditionalDirectories; !slices.Equal(got, want) {
		t.Fatalf("launch AdditionalDirectories = %v, want %v", got, want)
	}
	if agent.lastLaunch.WorkspacePath != "/ws/mer-1" {
		t.Fatalf("workspace = %q", agent.lastLaunch.WorkspacePath)
	}
}

func TestSingleRepoSpawnHandsTheAgentNoChildRoots(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: "/repo/mer", Config: testRoleAgents()}
	agent := &recordingAgent{}
	m := New(Deps{
		Runtime: &fakeRuntime{}, Agents: singleAgent{agent: agent}, Workspace: &fakeWorkspace{},
		Store: st, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st},
		LookPath: func(string) (string, error) { return "/bin/true", nil },
	})
	if _, _, _, err := m.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Prompt: "fix it"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := agent.lastLaunch.AdditionalDirectories; len(got) != 0 {
		t.Fatalf("single-repo launch AdditionalDirectories = %v, want none", got)
	}
}
