package sessionmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// fakeRelaunchIsolation is a hand-rolled fake for RelaunchRuntimeIsolation.
type fakeRelaunchIsolation struct {
	env   map[string]string
	err   error
	calls []struct {
		owner   domain.UserID
		harness domain.AgentHarness
	}
}

func (f *fakeRelaunchIsolation) ResolveForOwner(_ context.Context, owner domain.UserID, harness domain.AgentHarness) (map[string]string, error) {
	f.calls = append(f.calls, struct {
		owner   domain.UserID
		harness domain.AgentHarness
	}{owner, harness})
	if f.err != nil {
		return nil, f.err
	}
	return f.env, nil
}

// TestRestore_ReDerivesIsolatedRuntimeEnvFromPersistedOwner is Checkpoint
// 8P-B.2's relaunch-isolation proof: restoring a session re-derives the
// SAME isolated runtime env a fresh Spawn would have used, from the
// session's own persisted owner_user_id -- never a daemon-global default.
func TestRestore_ReDerivesIsolatedRuntimeEnvFromPersistedOwner(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
	seedTerminal(st, "mer-1", domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "b", AgentSessionID: "agent-x"})
	if _, err := st.SetSessionOwner(context.Background(), "mer-1", "user-a"); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	agent := &recordingAgent{}
	rt := &fakeRuntime{}
	isolation := &fakeRelaunchIsolation{env: map[string]string{"HOME": "/ao/users/user-a/runtime-home", "CLAUDE_CONFIG_DIR": "/ao/users/user-a/providers/claude-code"}}
	m := New(Deps{
		Runtime:          rt,
		Agents:           singleAgent{agent: agent},
		Workspace:        &fakeWorkspace{},
		Store:            st,
		Messenger:        &fakeMessenger{},
		Lifecycle:        &fakeLCM{store: st},
		DataDir:          t.TempDir(),
		LookPath:         func(string) (string, error) { return "/bin/true", nil },
		RuntimeIsolation: isolation,
	})

	if _, err := m.RestoreWithMode(ctx, "mer-1"); err != nil {
		t.Fatalf("RestoreWithMode: %v", err)
	}
	if len(isolation.calls) == 0 {
		t.Fatal("expected RuntimeIsolation.ResolveForOwner to be called during restore")
	}
	if isolation.calls[0].owner != "user-a" {
		t.Fatalf("resolved owner = %q, want user-a", isolation.calls[0].owner)
	}
	if rt.lastCfg.Env["HOME"] != "/ao/users/user-a/runtime-home" {
		t.Fatalf("relaunch env HOME = %q, want isolated runtime-home", rt.lastCfg.Env["HOME"])
	}
	if rt.lastCfg.Env["CLAUDE_CONFIG_DIR"] != "/ao/users/user-a/providers/claude-code" {
		t.Fatalf("relaunch env CLAUDE_CONFIG_DIR missing isolated override: %v", rt.lastCfg.Env)
	}
}

// TestRestore_TwoUsers_NeverShareRelaunchEnv proves two different sessions
// owned by different users never resolve to the same relaunch HOME.
func TestRestore_TwoUsers_NeverShareRelaunchEnv(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
	seedTerminal(st, "mer-1", domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "b", AgentSessionID: "agent-x"})
	seedTerminal(st, "mer-2", domain.SessionMetadata{WorkspacePath: "/ws/mer-2", Branch: "b", AgentSessionID: "agent-y"})
	if _, err := st.SetSessionOwner(context.Background(), "mer-1", "user-a"); err != nil {
		t.Fatalf("seed owner A: %v", err)
	}
	if _, err := st.SetSessionOwner(context.Background(), "mer-2", "user-b"); err != nil {
		t.Fatalf("seed owner B: %v", err)
	}
	rt := &fakeRuntime{}
	isolation := &fakeRelaunchIsolation{}
	isolation.env = map[string]string{"HOME": "/ao/users/user-a/runtime-home"}
	m := New(Deps{
		Runtime: rt, Agents: singleAgent{agent: &recordingAgent{}}, Workspace: &fakeWorkspace{},
		Store: st, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st}, DataDir: t.TempDir(),
		LookPath: func(string) (string, error) { return "/bin/true", nil }, RuntimeIsolation: isolation,
	})
	if _, err := m.RestoreWithMode(ctx, "mer-1"); err != nil {
		t.Fatalf("restore mer-1: %v", err)
	}
	homeA := rt.lastCfg.Env["HOME"]

	isolation.env = map[string]string{"HOME": "/ao/users/user-b/runtime-home"}
	if _, err := m.RestoreWithMode(ctx, "mer-2"); err != nil {
		t.Fatalf("restore mer-2: %v", err)
	}
	homeB := rt.lastCfg.Env["HOME"]

	if homeA == "" || homeB == "" || homeA == homeB {
		t.Fatalf("expected distinct isolated HOMEs, got A=%q B=%q", homeA, homeB)
	}
	if isolation.calls[0].owner != "user-a" || isolation.calls[1].owner != "user-b" {
		t.Fatalf("resolved owners = %v, want [user-a user-b]", isolation.calls)
	}
}

// TestRestore_ProfileNowMissing_BlocksRelaunch is Checkpoint 8P-B.2's
// disabled/missing-profile-during-relaunch proof (§23): no subprocess is
// created and the caller gets an actionable error, never a silent
// daemon-global fallback.
func TestRestore_ProfileNowMissing_BlocksRelaunch(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
	seedTerminal(st, "mer-1", domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "b", AgentSessionID: "agent-x"})
	if _, err := st.SetSessionOwner(context.Background(), "mer-1", "user-a"); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	rt := &fakeRuntime{}
	isolation := &fakeRelaunchIsolation{err: ports.ErrProviderProfileRequired}
	m := New(Deps{
		Runtime: rt, Agents: singleAgent{agent: &recordingAgent{}}, Workspace: &fakeWorkspace{},
		Store: st, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st}, DataDir: t.TempDir(),
		LookPath: func(string) (string, error) { return "/bin/true", nil }, RuntimeIsolation: isolation,
	})
	if _, err := m.RestoreWithMode(ctx, "mer-1"); !errors.Is(err, ports.ErrProviderProfileRequired) {
		t.Fatalf("RestoreWithMode error = %v, want ports.ErrProviderProfileRequired", err)
	}
	if rt.created != 0 {
		t.Fatalf("runtime.Create was called %d times, want 0 -- must never launch without a valid profile", rt.created)
	}
}
