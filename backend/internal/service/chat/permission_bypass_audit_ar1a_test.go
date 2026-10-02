package chat_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// AR-1a (D-SEC-4, Codex AR1A-05): escalating a Chat conversation's per-turn
// approval to the full bypass is audited exactly like launching with it.
func TestEscalatingTurnApprovalToBypassIsAudited(t *testing.T) {
	st := openStore(t)
	conv := newFakeConversation()
	var buf bytes.Buffer
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Sessions: st,
		Drivers: fakeRegistry{driver: fakeDriver{conv: conv}},
		Log:     slog.New(slog.NewTextHandler(&buf, nil)),
		NewID:   func() string { return "conversation-audit" },
	})
	t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })
	if _, err := svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := svc.SetTurnSettings(context.Background(), testSession, domain.ConversationSettings{ApprovalMode: ports.PermissionModeAuto}); err != nil {
		t.Fatalf("SetTurnSettings auto: %v", err)
	}
	if strings.Contains(buf.String(), "audit=agent_permission_bypass") {
		t.Fatalf("a non-bypass setting was audited as a bypass: %s", buf.String())
	}
	if _, err := svc.SetTurnSettings(context.Background(), testSession, domain.ConversationSettings{ApprovalMode: ports.PermissionModeBypassPermissions}); err != nil {
		t.Fatalf("SetTurnSettings bypass: %v", err)
	}
	if !strings.Contains(buf.String(), "audit=agent_permission_bypass") || !strings.Contains(buf.String(), "operation=chat-turn-settings") {
		t.Fatalf("escalation to the bypass left no audit line: %s", buf.String())
	}
}
