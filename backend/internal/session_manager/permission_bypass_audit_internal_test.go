package sessionmanager

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AR-1a / D-SEC-4: an explicit bypass leaves an audit trace; nothing else does.
func TestPermissionBypassIsAudited(t *testing.T) {
	for _, tc := range []struct {
		perm ports.PermissionMode
		want bool
	}{
		{ports.PermissionModeBypassPermissions, true},
		{ports.PermissionModeDefault, false},
		{"", false},
		{ports.PermissionModeAuto, false},
		{ports.PermissionModeAcceptEdits, false},
		{"yolo", false},
	} {
		var buf bytes.Buffer
		m := &Manager{logger: slog.New(slog.NewTextHandler(&buf, nil))}
		m.auditPermissionBypass("spawn", "ao-1", "proj-1", domain.AgentHarness("codex"), ports.AgentConfig{Permissions: tc.perm})
		got := strings.Contains(buf.String(), "audit=agent_permission_bypass")
		if got != tc.want {
			t.Fatalf("permission %q audited=%v, want %v (log: %s)", tc.perm, got, tc.want, buf.String())
		}
		if got && (!strings.Contains(buf.String(), "session=ao-1") || !strings.Contains(buf.String(), "harness=codex")) {
			t.Fatalf("audit line lacks identifiers: %s", buf.String())
		}
	}
}
