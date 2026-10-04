package providerpreflight

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerauth"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// auth_contract_test.go covers the WORKER half of the wf-4e3d187b keychain
// incident, and it exists because fixing the planner alone would have left
// every worker, reviewer and repair dispatch launching into the same dialog.
//
// The specific defect this pins: the old auth answer came from the claude-code
// adapter's AuthStatus, which reads the DAEMON's ~/.claude.json and the
// DAEMON's own ANTHROPIC_API_KEY. Under per-user isolation the launch resolves
// a different HOME, a different config dir and a different keychain, so the
// preflight was answering about a process that was never going to run.

const fakeCredential = "sk-ant-not-a-real-key-000000"

// isolatedEnv is the launch env AO's per-user isolation produces, carrying the
// legacy "login" keychain macOS reserves and never reopens with AO's password.
func isolatedEnv(t *testing.T) map[string]string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "runtime-home")
	keychains := filepath.Join(home, "Library", "Keychains")
	if err := os.MkdirAll(keychains, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keychains, "login.keychain-db"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "providers", "claude-code")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": configDir}
}

func TestPreflight_WorkerRefusesAnInteractiveKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the OS keychain is only in the launch path on macOS")
	}
	c := &Checker{}
	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness:               domain.HarnessClaudeCode,
		RuntimeEnv:            isolatedEnv(t),
		TrustRecordedAtLaunch: true,
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !res.AuthRequiresInteraction {
		t.Fatalf("worker preflight did not report an interactive credential store: %+v", res)
	}
	if res.AuthOK || res.AuthUnknown {
		t.Fatalf("an interactive credential store must be an affirmative refusal: %+v", res)
	}
	if !strings.Contains(res.Detail, "keychain") {
		t.Fatalf("detail does not name the keychain: %q", res.Detail)
	}
	if strings.Contains(res.Detail, fakeCredential) {
		t.Fatal("preflight detail leaked a credential")
	}
}

// The same launch with an unattended credential is ready: the keychain is
// never reached, so its state cannot ground the dispatch. This is what makes
// the environment mode a real remedy rather than a preference.
func TestPreflight_WorkerAcceptsAnEnvironmentCredential(t *testing.T) {
	env := isolatedEnv(t)
	env["ANTHROPIC_API_KEY"] = fakeCredential

	c := &Checker{}
	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness:               domain.HarnessClaudeCode,
		RuntimeEnv:            env,
		TrustRecordedAtLaunch: true,
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if res.AuthRequiresInteraction || !res.AuthOK {
		t.Fatalf("an environment credential was not accepted: %+v", res)
	}
	if strings.Contains(res.Detail, fakeCredential) {
		t.Fatalf("preflight detail leaked the credential: %q", res.Detail)
	}
}

func TestPreflight_WorkerAcceptsAHelperCredential(t *testing.T) {
	env := isolatedEnv(t)
	helper := filepath.Join(t.TempDir(), "get-key.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho key\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"apiKeyHelper": helper})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env["CLAUDE_CONFIG_DIR"], "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Checker{}
	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness:               domain.HarnessClaudeCode,
		RuntimeEnv:            env,
		TrustRecordedAtLaunch: true,
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if res.AuthRequiresInteraction || !res.AuthOK {
		t.Fatalf("a helper credential was not accepted: %+v", res)
	}
}

// A pinned auth mode is enforced on the worker path too, so an operator who
// declares an unattended posture cannot silently get a keychain launch.
func TestPreflight_WorkerHonoursAPinnedAuthMode(t *testing.T) {
	c := &Checker{AuthMode: providerauth.ModeEnvironment}
	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness:               domain.HarnessClaudeCode,
		RuntimeEnv:            isolatedEnv(t),
		TrustRecordedAtLaunch: true,
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if res.AuthOK {
		t.Fatalf("a pinned environment mode with no environment credential was accepted: %+v", res)
	}
}

// A launch keeping the host's own home is never refused: AO does not inspect
// the desktop user's real credential store, and refusing there would ground a
// working install. This is the guard on the "unknown is not a refusal" rule
// that the whole package depends on.
func TestPreflight_HostRuntimeIsNeverRefused(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this runner")
	}
	c := &Checker{}
	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness:               domain.HarnessClaudeCode,
		RuntimeEnv:            map[string]string{"HOME": home},
		TrustRecordedAtLaunch: true,
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if res.AuthRequiresInteraction {
		t.Fatalf("a host-runtime launch was refused over a credential store AO does not inspect: %q", res.Detail)
	}
}
