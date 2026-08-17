package providerprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codex"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimehome"
)

// CLIProber probes Claude Code/Codex auth state by invoking their CLI
// binary with CLAUDE_CONFIG_DIR/CODEX_HOME (and HOME) pinned to the
// profile owner's runtime-home, rather than the daemon host's real
// environment. This intentionally does not reuse
// claudecode.Plugin.AuthStatus/codex.Plugin.AuthStatus -- those probe the
// daemon process's own real environment (no per-user override exists on
// that interface) and reusing them here would silently probe the wrong
// user's credentials. It deliberately never calls os.Setenv: every
// override is scoped to the one subprocess it launches.
type CLIProber struct{}

var _ Prober = CLIProber{}

// Probe runs the harness's own CLI auth-status check inside env, returning
// ProviderAuthStateUnknown (never an error) for a harness this prober
// doesn't recognize or a binary that can't be resolved -- an inability to
// probe is not evidence of anything.
func (CLIProber) Probe(ctx context.Context, harness domain.AgentHarness, env runtimehome.Environment) (domain.ProviderAuthState, error) {
	switch harness {
	case domain.HarnessClaudeCode:
		return probeClaudeCode(ctx, env)
	case domain.HarnessCodex:
		return probeCodex(ctx, env)
	default:
		return domain.ProviderAuthStateUnknown, nil
	}
}

func probeClaudeCode(ctx context.Context, env runtimehome.Environment) (domain.ProviderAuthState, error) {
	binary, err := claudecode.ResolveClaudeBinary(ctx)
	if err != nil {
		return domain.ProviderAuthStateUnknown, nil //nolint:nilerr // binary absence is advisory, not an error
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out := runIsolated(probeCtx, binary, env, "auth", "status")
	return authStateFromJSONLoggedIn(out), nil
}

func probeCodex(ctx context.Context, env runtimehome.Environment) (domain.ProviderAuthState, error) {
	binary, err := codex.ResolveCodexBinary(ctx)
	if err != nil {
		return domain.ProviderAuthStateUnknown, nil //nolint:nilerr // binary absence is advisory, not an error
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out := runIsolated(probeCtx, binary, env, "login", "status")
	text := strings.ToLower(string(out))
	switch {
	case strings.Contains(text, "not logged in"), strings.Contains(text, "logged out"):
		return domain.ProviderAuthStateUnauthenticated, nil
	case strings.Contains(text, "logged in"):
		return domain.ProviderAuthStateAuthenticated, nil
	default:
		return domain.ProviderAuthStateUnknown, nil
	}
}

// runIsolated runs binary with args, replacing (never mutating) its own env
// with the real process env overlaid by env.SubprocessEnv() -- the same
// isolation contract launched sessions get (see session_manager wiring).
// The probe is advisory: a failed/absent binary just yields empty output,
// which the caller's output parser already treats as an unknown auth state.
func runIsolated(ctx context.Context, binary string, env runtimehome.Environment, args ...string) []byte {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = mergeEnv(os.Environ(), env.SubprocessEnv())
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	return out.Bytes()
}

func mergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, override := overrides[key]; override {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func authStateFromJSONLoggedIn(out []byte) domain.ProviderAuthState {
	start := bytes.IndexByte(out, '{')
	end := bytes.LastIndexByte(out, '}')
	if start < 0 || end < start {
		return domain.ProviderAuthStateUnknown
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if json.Unmarshal(out[start:end+1], &status) != nil {
		return domain.ProviderAuthStateUnknown
	}
	if status.LoggedIn {
		return domain.ProviderAuthStateAuthenticated
	}
	return domain.ProviderAuthStateUnauthenticated
}
