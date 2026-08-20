// Package providersetup implements Checkpoint 8P-E.8.4's zero-terminal
// provider onboarding: a server-launched, server-owned PTY that runs a
// provider CLI's own login flow directly inside the profile owner's isolated
// runtime-home (see runtimehome.Prepare), so a desktop-app user never has to
// type CLAUDE_CONFIG_DIR/HOME overrides by hand. The PTY itself is opened
// through shellterm.Service.OpenProviderSetupTerminal -- this package only
// decides WHAT to launch (Launcher) and tracks the one live setup session per
// profile (Service).
package providersetup

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codex"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrProviderNotSupported is returned by a Launcher when a harness has no
// automatable setup path yet. Service turns this into a 4xx the frontend
// renders as "manual setup only" -- never a crash or a silently-opened
// terminal running nothing useful.
var ErrProviderNotSupported = errors.New("providersetup: provider has no automatable setup path")

// Launcher resolves what to run inside a provider's setup terminal: the argv
// to exec, and human-readable instructions to show above the embedded
// terminal. It deliberately returns no environment -- Service always launches
// under the profile owner's runtimehome.Environment.SubprocessEnv(), which is
// never a Launcher's decision to make.
type Launcher interface {
	Launch(ctx context.Context, harness domain.AgentHarness) (argv []string, instructions string, err error)
}

// CLILauncher is the Launcher for CLI-bootstrap providers (Claude Code,
// Codex today -- domain.AuthMethodCLIBootstrap in the provider registry).
type CLILauncher struct{}

var _ Launcher = CLILauncher{}

// Launch resolves the harness's CLI binary the same way probe.go's prober
// does (claudecode.ResolveClaudeBinary / codex.ResolveCodexBinary), so a
// setup terminal and a Test Connection probe always agree on which binary is
// in play.
func (CLILauncher) Launch(ctx context.Context, harness domain.AgentHarness) ([]string, string, error) {
	switch harness {
	case domain.HarnessClaudeCode:
		// Claude Code's /login is a REPL slash command, not a CLI flag or
		// subcommand -- it cannot be driven non-interactively, so the best
		// automatable step is starting the CLI itself inside the isolated
		// runtime-home and asking the user to type /login. This is the
		// checkpoint's own documented fallback ("the user should at most
		// need to type /login").
		binary, err := claudecode.ResolveClaudeBinary(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("providersetup: resolve claude binary: %w", err)
		}
		return []string{binary}, "Run /login and complete sign-in in the browser that opens.", nil
	case domain.HarnessCodex:
		// codex login mirrors the `codex login status` subcommand the
		// prober already shells out to (probe.go's probeCodex). Unverified:
		// this repo has not confirmed live whether `codex login` completes
		// entirely non-interactively (e.g. a device code / browser
		// redirect) or needs a further prompt inside the terminal -- see
		// the checkpoint's final report for this as an explicit, called-out
		// gap rather than a claimed-working path.
		binary, err := codex.ResolveCodexBinary(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("providersetup: resolve codex binary: %w", err)
		}
		return []string{binary, "login"}, "Follow the prompts to complete sign-in. If nothing happens, run `codex login`.", nil
	default:
		return nil, "", ErrProviderNotSupported
	}
}
