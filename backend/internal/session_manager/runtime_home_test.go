package sessionmanager

import (
	"log/slog"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestApplyUserRuntimeEnv_IsolatesOwners is Checkpoint 8P-B's security test
// at the session-launch env layer: two different launch owners must never
// end up with the same HOME override, and applying no owner must leave the
// env untouched (today's default, since dispatch does not yet resolve an
// owner for every launch path -- see runtime_home.go's doc comment).
func TestApplyUserRuntimeEnv_IsolatesOwners(t *testing.T) {
	m := &Manager{dataDir: t.TempDir(), logger: slog.Default()}

	envA := m.applyUserRuntimeEnv(map[string]string{"EXISTING": "kept"}, domain.UserID("user-a"))
	envB := m.applyUserRuntimeEnv(map[string]string{"EXISTING": "kept"}, domain.UserID("user-b"))

	if envA["HOME"] == "" || envB["HOME"] == "" {
		t.Fatal("expected HOME to be set for a resolved owner")
	}
	if envA["HOME"] == envB["HOME"] {
		t.Fatalf("user A and B got the same HOME: %s", envA["HOME"])
	}
	if envA["EXISTING"] != "kept" {
		t.Fatal("applyUserRuntimeEnv must not drop unrelated existing keys")
	}

	noOwner := m.applyUserRuntimeEnv(map[string]string{"EXISTING": "kept"}, "")
	if _, ok := noOwner["HOME"]; ok {
		t.Fatal("applyUserRuntimeEnv must be a no-op when no owner is resolved")
	}
}
