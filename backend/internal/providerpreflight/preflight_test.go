package providerpreflight

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// The invariant this package lives or dies on: an inconclusive probe is never a
// refusal. Grounding every unattended dispatch because AO could not read a
// config file would be strictly worse than the incident it exists for.
func TestUnreadableConfigurationIsReadyNotRefused(t *testing.T) {
	c := &Checker{}
	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness:       "claude-code",
		WorkspacePath: "/definitely/not/a/directory",
		RuntimeEnv:    map[string]string{"CLAUDE_CONFIG_DIR": t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	// Every answer stays Unknown, which is what the dispatch path reads as
	// ready. The two must agree: a result this package calls "could not tell"
	// and a verdict the workflow package calls "refused" would be the failure
	// mode inverted.
	if !res.BinaryOK || !res.AuthUnknown || !res.TrustUnknown || !res.PermissionModeUnknown {
		t.Fatalf("an unreadable configuration produced a verdict: %+v", res)
	}
}

// The "Yes, I accept" dialog, detected from the provider's own configuration
// rather than by waiting for a worker to sit on it for ten minutes.
func TestUnacceptedBypassPermissionsIsDetected(t *testing.T) {
	dir := t.TempDir()
	writeClaudeConfig(t, dir, map[string]any{"bypassPermissionsModeAccepted": false})

	res, err := (&Checker{}).Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness:    "claude-code",
		RuntimeEnv: map[string]string{"CLAUDE_CONFIG_DIR": dir},
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if res.PermissionModeUnknown || res.PermissionModeOK {
		t.Fatalf("an unaccepted bypass-permissions posture read as ready: %+v", res)
	}
	if res.Detail == "" {
		t.Fatal("the refusal carries no explanation")
	}
}

// An accepted posture and a trusted workspace dispatch, and the trust answer is
// path-scoped: an untrusted directory that exists is a refusal, while a path AO
// cannot see is left unknown (AO's own claude-code adapter records trust for
// the launch workspace in PreLaunch, so refusing there would be wrong).
func TestWorkspaceTrustIsPathScoped(t *testing.T) {
	dir := t.TempDir()
	trusted := t.TempDir()
	untrusted := t.TempDir()
	writeClaudeConfig(t, dir, map[string]any{
		"bypassPermissionsModeAccepted": true,
		"projects": map[string]any{
			trusted: map[string]any{"hasTrustDialogAccepted": true},
		},
	})
	c := &Checker{}

	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness: "claude-code", WorkspacePath: trusted,
		RuntimeEnv: map[string]string{"CLAUDE_CONFIG_DIR": dir},
	})
	if err != nil {
		t.Fatalf("Preflight(trusted): %v", err)
	}
	if !res.TrustOK {
		t.Fatalf("a trusted workspace read as untrusted: %+v", res)
	}

	res, err = c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness: "claude-code", WorkspacePath: untrusted,
		RuntimeEnv: map[string]string{"CLAUDE_CONFIG_DIR": dir},
	})
	if err != nil {
		t.Fatalf("Preflight(untrusted): %v", err)
	}
	if res.TrustUnknown || res.TrustOK {
		t.Fatalf("an untrusted, existing workspace read as ready: %+v", res)
	}

	res, err = c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness: "claude-code", WorkspacePath: filepath.Join(dir, "not-created-yet"),
		RuntimeEnv: map[string]string{"CLAUDE_CONFIG_DIR": dir},
	})
	if err != nil {
		t.Fatalf("Preflight(future worktree): %v", err)
	}
	if !res.TrustUnknown {
		t.Fatalf("a worktree that does not exist yet was judged: %+v", res)
	}
}

// P3-D smoke A: the refusal that grounded every claude-code worker.
//
// A worker dispatch's launch records the workspace's trust itself, in the
// adapter's PreLaunch, before the agent starts — so the trust dialog cannot
// stop it however the config reads beforehand. The check nevertheless fired on
// the project path, and refused an unattended launch on a repository the person
// had simply never opened in Claude Code. On a real run that was the whole of
// "the worker never started".
//
// The exemption is scoped to the caller that earns it. The SAME untrusted,
// existing workspace is still refused for a caller whose launch does not record
// trust, which is the case the check was written for.
func TestALaunchThatRecordsTrustItselfIsNotRefusedForNotHavingItYet(t *testing.T) {
	dir := t.TempDir()
	untrusted := t.TempDir()
	writeClaudeConfig(t, dir, map[string]any{
		"bypassPermissionsModeAccepted": true,
		"projects":                      map[string]any{},
	})
	c := &Checker{}
	env := map[string]string{"CLAUDE_CONFIG_DIR": dir}

	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness: "claude-code", WorkspacePath: untrusted, RuntimeEnv: env,
		TrustRecordedAtLaunch: true,
	})
	if err != nil {
		t.Fatalf("Preflight(records trust at launch): %v", err)
	}
	if !res.TrustOK || !res.TrustUnknown {
		t.Fatalf("a launch that records its own trust was refused for not having it yet: %+v", res)
	}
	// And nothing else was waived: the posture answer is still read, because
	// no launch writes that one and accepting it for somebody is exactly what
	// this package refuses to do.
	if res.PermissionModeUnknown {
		t.Fatalf("the bypass-permissions answer was waived along with trust: %+v", res)
	}

	res, err = c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness: "claude-code", WorkspacePath: untrusted, RuntimeEnv: env,
	})
	if err != nil {
		t.Fatalf("Preflight(does not record trust): %v", err)
	}
	if res.TrustUnknown || res.TrustOK {
		t.Fatalf("the exemption leaked to a caller that never claimed it: %+v", res)
	}
}

// A harness AO has no interactive-startup knowledge of produces unknowns, which
// is ready. The preflight must never invent a verdict about a provider whose
// configuration it does not understand.
func TestUnknownHarnessIsReady(t *testing.T) {
	res, err := (&Checker{}).Preflight(context.Background(), workflowcore.WorkerPreflightRequest{
		Harness: domain.AgentHarness("some-new-cli"), WorkspacePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !res.TrustUnknown || !res.PermissionModeUnknown || !res.AuthUnknown {
		t.Fatalf("AO invented an answer about an unknown provider: %+v", res)
	}
}

// A provider that affirmatively reports unusable credentials is refused — and
// one whose probe merely errors is not.
func TestAuthIsRefusedOnlyOnAnAffirmativeAnswer(t *testing.T) {
	c := &Checker{Agents: stubResolver{agent: &stubAgent{status: ports.AgentAuthStatusUnauthorized}}}
	res, err := c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{Harness: "claude-code"})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if res.AuthUnknown || res.AuthOK {
		t.Fatalf("an affirmative 'unauthorized' did not refuse: %+v", res)
	}

	c = &Checker{Agents: stubResolver{agent: &stubAgent{status: ports.AgentAuthStatusUnknown}}}
	res, err = c.Preflight(context.Background(), workflowcore.WorkerPreflightRequest{Harness: "claude-code"})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !res.AuthUnknown {
		t.Fatalf("an inconclusive auth probe produced a verdict: %+v", res)
	}
}

func writeClaudeConfig(t *testing.T, dir string, content map[string]any) {
	t.Helper()
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

type stubResolver struct{ agent ports.Agent }

func (r stubResolver) Agent(domain.AgentHarness) (ports.Agent, bool) { return r.agent, r.agent != nil }

type stubAgent struct {
	ports.Agent
	status ports.AgentAuthStatus
}

func (a *stubAgent) AuthStatus(context.Context) (ports.AgentAuthStatus, error) { return a.status, nil }
