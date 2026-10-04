package daemon

// Checkpoint 8P-E.19 — the production Diagnostic Agent launcher.
//
// What is pinned here is the read-only mandate, because it is the property that
// makes it safe to point a language model at a stopped production run. The
// prompt also asks for it, but a prompt is not enforcement; these are the real
// mechanisms: Claude Code's tool allowlist, Codex's OS-level sandbox, and a
// working directory that contains nothing worth reading.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func newIncidentLauncher(t *testing.T, harness domain.AgentHarness) (*incidentAgentLauncher, *fakeResolverAgent, *fakeResolverRuntime, string) {
	t.Helper()
	agent := &fakeResolverAgent{argv: []string{string(harness), "--flag", "--", "prompt"}}
	runtime := &fakeResolverRuntime{}
	dir := t.TempDir()
	return &incidentAgentLauncher{
		agents:  &fakeResolverAgentResolver{byHarness: map[domain.AgentHarness]*fakeResolverAgent{harness: agent}},
		runtime: runtime,
		dataDir: dir,
	}, agent, runtime, dir
}

func diagnosticRequest() workflowcore.IncidentAgentRequest {
	return workflowcore.IncidentAgentRequest{
		IncidentID: "inc-abc123", RunID: "wf-1", ProjectID: "proj-1",
		Prompt: "diagnose this incident from the pack", PackDigest: "digest-1",
		Harness: string(domain.HarnessClaudeCode), ReadOnly: true,
	}
}

// A Claude Code diagnostic session may do exactly one thing: submit. It has no
// Read, no Grep, no Glob — the pack is its whole evidence, and an agent free to
// wander the repository produces a diagnosis nobody can reproduce.
func TestIncidentLauncherClaudeCanOnlySubmit(t *testing.T) {
	l, agent, _, _ := newIncidentLauncher(t, domain.HarnessClaudeCode)

	if _, err := l.LaunchDiagnostic(context.Background(), diagnosticRequest()); err != nil {
		t.Fatalf("LaunchDiagnostic: %v", err)
	}
	cfg := agent.lastCfg
	if cfg.Permissions != ports.PermissionModeAuto {
		t.Fatalf("Permissions = %v, want PermissionModeAuto (never bypassPermissions)", cfg.Permissions)
	}
	for _, banned := range []string{"Read", "Grep", "Glob", "Edit", "Write"} {
		for _, allowed := range cfg.AllowedTools {
			if allowed == banned {
				t.Fatalf("%q is in the diagnostic allowlist; the pack is meant to be its only evidence", banned)
			}
		}
	}
	submit := false
	for _, allowed := range cfg.AllowedTools {
		if strings.Contains(allowed, "ao incident submit") {
			submit = true
		}
	}
	if !submit {
		t.Fatal("the diagnostic agent has no way to deliver its answer")
	}
	for _, mustDeny := range []string{"Edit", "Write", "Read", "Grep", "WebFetch"} {
		found := false
		for _, denied := range cfg.DisallowedTools {
			if denied == mustDeny {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q is not explicitly denied", mustDeny)
		}
	}
}

// Codex has no per-tool allowlist, so its read-only enforcement is the real
// OS-level sandbox — the same mechanism the reviewer and resolver adapters use.
func TestIncidentLauncherCodexRunsSandboxedReadOnly(t *testing.T) {
	l, _, runtime, _ := newIncidentLauncher(t, domain.HarnessCodex)
	req := diagnosticRequest()
	req.Harness = string(domain.HarnessCodex)

	if _, err := l.LaunchDiagnostic(context.Background(), req); err != nil {
		t.Fatalf("LaunchDiagnostic: %v", err)
	}
	argv := strings.Join(runtime.lastCfg.Argv, " ")
	if !strings.Contains(argv, "--sandbox read-only") {
		t.Fatalf("codex argv is not sandboxed read-only: %s", argv)
	}
}

// The working directory is an empty AO-owned scratch directory, never the run's
// worktree. Under Codex the directory IS the isolation, so pointing it at the
// code under investigation would hand back exactly the unbounded evidence the
// context pack exists to replace.
func TestIncidentLauncherRunsInAnIsolatedEmptyWorkspace(t *testing.T) {
	l, _, runtime, dir := newIncidentLauncher(t, domain.HarnessClaudeCode)

	if _, err := l.LaunchDiagnostic(context.Background(), diagnosticRequest()); err != nil {
		t.Fatalf("LaunchDiagnostic: %v", err)
	}
	ws := runtime.lastCfg.WorkspacePath
	if !strings.HasPrefix(ws, filepath.Join(dir, "incidents")) {
		t.Fatalf("workspace = %q, want an AO-owned scratch directory under the data dir", ws)
	}
	entries, err := readDirNames(ws)
	if err != nil {
		t.Fatalf("read scratch dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the diagnostic workspace is not empty: %v", entries)
	}
}

// A launch that arrives without the read-only mandate is refused rather than
// quietly downgraded. Honouring a safety flag only when convenient is how it
// becomes a comment.
func TestIncidentLauncherRefusesAWritableDiagnostic(t *testing.T) {
	l, _, runtime, _ := newIncidentLauncher(t, domain.HarnessClaudeCode)
	req := diagnosticRequest()
	req.ReadOnly = false

	if _, err := l.LaunchDiagnostic(context.Background(), req); err == nil {
		t.Fatal("a diagnostic launch without ReadOnly was accepted")
	}
	if runtime.calls != 0 {
		t.Fatalf("runtime pane creations = %d, want 0", runtime.calls)
	}
}

// The repair launcher is deliberately absent until the diagnostic path is
// proven end to end, and it says so rather than falling back to something
// weaker.
func TestIncidentLauncherRepairIsNotEnabledYet(t *testing.T) {
	l, _, runtime, _ := newIncidentLauncher(t, domain.HarnessClaudeCode)
	if _, err := l.LaunchRepair(context.Background(), diagnosticRequest()); err == nil {
		t.Fatal("LaunchRepair silently succeeded; it must refuse until it is implemented")
	}
	if runtime.calls != 0 {
		t.Fatalf("runtime pane creations = %d, want 0", runtime.calls)
	}
}

// The submit call needs the incident's identity without the agent being told to
// remember it, so the launcher puts it in the environment.
func TestIncidentLauncherExportsTheIncidentIdentity(t *testing.T) {
	l, _, runtime, _ := newIncidentLauncher(t, domain.HarnessClaudeCode)
	if _, err := l.LaunchDiagnostic(context.Background(), diagnosticRequest()); err != nil {
		t.Fatalf("LaunchDiagnostic: %v", err)
	}
	env := runtime.lastCfg.Env
	for k, want := range map[string]string{
		"AO_INCIDENT_ID":          "inc-abc123",
		"AO_INCIDENT_RUN_ID":      "wf-1",
		"AO_INCIDENT_PACK_DIGEST": "digest-1",
	} {
		if env[k] != want {
			t.Fatalf("env[%s] = %q, want %q", k, env[k], want)
		}
	}
}

// readDirNames lists a directory's entries, so the emptiness assertion above is
// about the real filesystem rather than about what the launcher believes.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}
