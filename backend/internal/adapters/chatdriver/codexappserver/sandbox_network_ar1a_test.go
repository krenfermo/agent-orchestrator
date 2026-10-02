package codexappserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AR-1a (Codex AR1A-04): the workspace-write sandbox keeps network access on
// for Chat, exactly as the TUI path does, at thread/start, thread/resume and on
// a per-turn sandbox override. Full bypass carries no such override.

type threadSandboxParams struct {
	Sandbox string          `json:"sandbox"`
	Config  map[string]bool `json:"config"`
}

func decodeThreadSandbox(t *testing.T, raw json.RawMessage) threadSandboxParams {
	t.Helper()
	var p threadSandboxParams
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("params: %v", err)
	}
	return p
}

func TestChatThreadStartKeepsNetworkOnInsideTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		mode        ports.PermissionMode
		wantSandbox string
		wantNetwork bool
	}{
		{ports.PermissionModeDefault, "workspace-write", true},
		{ports.PermissionModeAuto, "workspace-write", true},
		{ports.PermissionModeBypassPermissions, "danger-full-access", false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			d, srv := newTestDriver(t)
			conv, err := d.Start(context.Background(), ports.ChatStartConfig{SessionID: "ao-1", WorkspacePath: "/tmp/ws", Permissions: tc.mode})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = conv.Close() }()
			p := decodeThreadSandbox(t, srv.awaitFrame(func(f frame) bool { return f.Method == "thread/start" }).Params)
			if p.Sandbox != tc.wantSandbox || p.Config[workspaceWriteNetworkConfigKey] != tc.wantNetwork {
				t.Fatalf("thread/start sandbox=%q config=%v, want %q network=%v", p.Sandbox, p.Config, tc.wantSandbox, tc.wantNetwork)
			}
		})
	}
}

func TestChatThreadResumeKeepsNetworkOnInsideTheSandbox(t *testing.T) {
	d, srv := newTestDriver(t)
	conv, err := d.Resume(context.Background(), ports.ChatResumeConfig{SessionID: "ao-1", ProviderConversationID: "thread-1", WorkspacePath: "/tmp/ws"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = conv.Close() }()
	p := decodeThreadSandbox(t, srv.awaitFrame(func(f frame) bool { return f.Method == "thread/resume" }).Params)
	if p.Sandbox != "workspace-write" || !p.Config[workspaceWriteNetworkConfigKey] {
		t.Fatalf("thread/resume sandbox=%q config=%v, want workspace-write with network", p.Sandbox, p.Config)
	}
}

func TestChatTurnSandboxOverrideKeepsNetworkOn(t *testing.T) {
	params := map[string]any{}
	applyTurnSettings(params, ports.ChatTurnSettings{Approval: ports.PermissionModeDefault}, nil)
	policy, _ := params["sandboxPolicy"].(map[string]any)
	if policy["type"] != "workspaceWrite" || policy["networkAccess"] != true {
		t.Fatalf("turn sandboxPolicy = %v, want workspaceWrite with networkAccess", policy)
	}
	params = map[string]any{}
	applyTurnSettings(params, ports.ChatTurnSettings{Approval: ports.PermissionModeBypassPermissions}, nil)
	if policy, _ := params["sandboxPolicy"].(map[string]any); policy["type"] != "dangerFullAccess" {
		t.Fatalf("explicit bypass turn sandboxPolicy = %v, want dangerFullAccess", policy)
	}
}
