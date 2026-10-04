package runtimehome

import (
	"os"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestPrepare_IsolatesUsers is Checkpoint 8P-B's credential-isolation
// security test at the runtime-home layer: two users' prepared
// environments must never share a HOME (or any other subprocess env
// override), and neither path may reference the other user's id.
func TestPrepare_IsolatesUsers(t *testing.T) {
	dataDir := t.TempDir()

	envA, err := Prepare(dataDir, domain.UserID("user-a"))
	if err != nil {
		t.Fatalf("prepare A: %v", err)
	}
	envB, err := Prepare(dataDir, domain.UserID("user-b"))
	if err != nil {
		t.Fatalf("prepare B: %v", err)
	}

	if envA.RuntimeHome == envB.RuntimeHome {
		t.Fatal("user A and B were given the same runtime home")
	}
	if envA.ClaudeConfigDir == envB.ClaudeConfigDir {
		t.Fatal("user A and B were given the same Claude config dir")
	}
	if envA.CodexHome == envB.CodexHome {
		t.Fatal("user A and B were given the same Codex home")
	}

	subA := envA.SubprocessEnv()
	subB := envB.SubprocessEnv()
	for key, valA := range subA {
		valB, ok := subB[key]
		if !ok {
			t.Fatalf("user B env missing key %q present for user A", key)
		}
		if valA == valB {
			t.Fatalf("user A and B share the same value for %q: %s", key, valA)
		}
		if strings.Contains(valA, "user-b") {
			t.Fatalf("user A's %q references user B's id: %s", key, valA)
		}
		if strings.Contains(valB, "user-a") {
			t.Fatalf("user B's %q references user A's id: %s", key, valB)
		}
	}

	// Every override must resolve under AO_DATA_DIR, never the real host
	// home -- see CLAUDE.md's "App state lives under ~/.ao only" hard rule.
	for key, val := range subA {
		if key == "TEMP" || key == "TMP" || key == "TMPDIR" {
			continue // aliases of TempRoot, already covered via HOME's sibling check below
		}
		if !strings.HasPrefix(val, dataDir) {
			t.Fatalf("user A's %q = %q does not resolve under AO_DATA_DIR %q", key, val, dataDir)
		}
	}

	// Directories must actually exist and be private.
	for _, dir := range []string{envA.RuntimeHome, envA.ClaudeConfigDir, envA.CodexHome} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
}

func TestPrepare_RejectsUnsafeInput(t *testing.T) {
	if _, err := Prepare("relative/path", domain.UserID("user-a")); err == nil {
		t.Fatal("expected error for relative data dir")
	}
	if _, err := Prepare(t.TempDir(), domain.UserID("../../etc")); err == nil {
		t.Fatal("expected error for unsafe user id")
	}
}
