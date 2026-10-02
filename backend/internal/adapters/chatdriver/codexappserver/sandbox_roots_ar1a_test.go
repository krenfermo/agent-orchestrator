package codexappserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AR-1a (Codex AR1A-INT-01): a workspace-write Codex Chat thread gets the
// explicit extra writable roots -- the workspace project's child roots and the
// git directories of its repositories -- at thread/start, thread/resume and on
// every per-turn sandbox override; an invalid root refuses the session.

type rootsParams struct {
	Sandbox string         `json:"sandbox"`
	Config  map[string]any `json:"config"`
}

func configRoots(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var p rootsParams
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("params: %v", err)
	}
	list, _ := p.Config[workspaceWriteRootsConfigKey].([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// recordingRoots returns fixed roots and records what it was asked.
type recordingRoots struct {
	roots      []string
	err        error
	workspace  string
	additional []string
}

func (r *recordingRoots) fn(_ context.Context, ws string, add []string) ([]string, error) {
	r.workspace, r.additional = ws, append([]string(nil), add...)
	return r.roots, r.err
}

func TestChatStartAndResumeSendTheExplicitWritableRoots(t *testing.T) {
	roots := []string{"/ws/child-a", "/ws/.git", "/repos/a/.git"}
	rec := &recordingRoots{roots: roots}

	d, srv := newTestDriver(t)
	d.writableRoots = rec.fn
	conv, err := d.Start(context.Background(), ports.ChatStartConfig{
		SessionID: "ao-1", WorkspacePath: "/ws", AdditionalDirectories: []string{"/ws/child-a", "/ws/child-a"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = conv.Close() }()
	if rec.workspace != "/ws" || !slices.Equal(rec.additional, []string{"/ws/child-a", "/ws/child-a"}) {
		t.Fatalf("resolver saw workspace=%q additional=%v", rec.workspace, rec.additional)
	}
	if got := configRoots(t, srv.awaitFrame(func(f frame) bool { return f.Method == "thread/start" }).Params); !slices.Equal(got, roots) {
		t.Fatalf("thread/start writable roots = %v, want %v", got, roots)
	}

	d2, srv2 := newTestDriver(t)
	d2.writableRoots = rec.fn
	conv2, err := d2.Resume(context.Background(), ports.ChatResumeConfig{
		SessionID: "ao-1", ProviderConversationID: "thread-1", WorkspacePath: "/ws", AdditionalDirectories: []string{"/ws/child-a"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = conv2.Close() }()
	if got := configRoots(t, srv2.awaitFrame(func(f frame) bool { return f.Method == "thread/resume" }).Params); !slices.Equal(got, roots) {
		t.Fatalf("thread/resume writable roots = %v, want %v", got, roots)
	}
}

func TestChatTurnOverrideCarriesTheSameWritableRoots(t *testing.T) {
	roots := []string{"/ws/child-a", "/ws/.git"}
	params := map[string]any{}
	applyTurnSettings(params, ports.ChatTurnSettings{Approval: ports.PermissionModeDefault}, roots)
	policy, _ := params["sandboxPolicy"].(map[string]any)
	got, _ := policy["writableRoots"].([]string)
	if policy["type"] != "workspaceWrite" || !slices.Equal(got, roots) || policy["networkAccess"] != true {
		t.Fatalf("turn sandboxPolicy = %v, want workspaceWrite with roots %v and network", policy, roots)
	}
	params = map[string]any{}
	applyTurnSettings(params, ports.ChatTurnSettings{Approval: ports.PermissionModeBypassPermissions}, roots)
	if policy, _ := params["sandboxPolicy"].(map[string]any); policy["type"] != "dangerFullAccess" || policy["writableRoots"] != nil {
		t.Fatalf("explicit bypass turn sandboxPolicy = %v", policy)
	}
}

// Fail closed: roots that cannot be established refuse the session before any
// app-server process is spawned.
func TestChatRefusesASessionWhoseWritableRootsAreInvalid(t *testing.T) {
	d, _ := newTestDriver(t)
	spawned := false
	inner := d.spawn
	d.spawn = func(ctx context.Context, bin, dir string, env []string) (*process, error) {
		spawned = true
		return inner(ctx, bin, dir, env)
	}
	d.writableRoots = (&recordingRoots{err: errors.New("additional directory outside the workspace")}).fn
	if _, err := d.Start(context.Background(), ports.ChatStartConfig{SessionID: "ao-1", WorkspacePath: "/ws", AdditionalDirectories: []string{"/elsewhere"}}); err == nil {
		t.Fatalf("Start accepted invalid writable roots")
	}
	if _, err := d.Resume(context.Background(), ports.ChatResumeConfig{SessionID: "ao-1", ProviderConversationID: "thread-1", WorkspacePath: "/ws", AdditionalDirectories: []string{"/elsewhere"}}); err == nil {
		t.Fatalf("Resume accepted invalid writable roots")
	}
	if spawned {
		t.Fatalf("an app-server was spawned for a session refused on its roots")
	}
}
