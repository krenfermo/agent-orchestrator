package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// decisionResolverRuntime is the narrow runtime-pane-creation surface
// decisionResolverLauncher needs, mirroring workflowReviewerLauncher's own
// workflowReviewerRuntime port — the same generic runtime adapter every
// other session pane in the daemon already uses, not a new mechanism.
type decisionResolverRuntime interface {
	Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error)
}

// decisionResolverAllowedTools is the read-only tool allowlist a Claude Code
// resolver session launches with — mirrors
// adapters/reviewer/claudecode.reviewerAllowedTools's shape/reasoning
// exactly, but scoped to a resolver's narrower job (read/discover repo
// evidence, then submit exactly one structured result) and with the write
// allowlist entry swapped for `ao decision resolve` instead of
// `ao review submit`. No gh/git-diff/git-log access: a resolver never posts
// anything to GitHub and is not reviewing a diff, only inspecting the
// worktree's current state.
var decisionResolverAllowedTools = []string{
	"Read",
	"Grep",
	"Glob",
	"Bash(printf:*)",
	"Bash(git status:*)",
	"Bash(git log:*)",
	"Bash(git show:*)",
	"Bash(ao decision resolve:*)",
}

// decisionResolverDisallowedTools hard-denies the write paths as defense in
// depth, mirroring reviewerDisallowedTools.
var decisionResolverDisallowedTools = []string{
	"Edit",
	"Write",
	"NotebookEdit",
	"Bash(git push:*)",
	"Bash(git commit:*)",
	"Bash(git checkout:*)",
	"Bash(git merge:*)",
	"Bash(git rebase:*)",
}

// decisionResolverLauncher is Checkpoint 8K-B pass 2's concrete
// workflowcore.DecisionResolverLauncher. It mirrors workflowReviewerLauncher
// closely (same runtime.Create call shape, same PATH/AO-shim pinning so a
// bare `ao decision resolve ...` in the resolver's prompt resolves), but
// resolves the harness through ports.AgentResolver (the SAME per-session
// agent registry session_manager uses for ordinary worker spawns) rather
// than the reviewer registry — a resolver session is a plain read-only
// worker-agent invocation, not a review pass, so it has no PR-centric
// prompt/allowlist baggage to work around in the first place.
//
// Read-only enforcement reuses each harness's own real mechanism, not a new
// one: Codex gets `--sandbox read-only` (mirroring
// adapters/reviewer/codex.codexReadOnlyArgs) inserted into its own argv;
// Claude Code launches under decisionResolverAllowedTools/
// decisionResolverDisallowedTools via the SAME ports.LaunchConfig.
// AllowedTools/DisallowedTools enforcement mechanism the reviewer adapter
// and every other tool-restricted launch in this codebase already relies on.
type decisionResolverLauncher struct {
	agents     ports.AgentResolver
	runtime    decisionResolverRuntime
	dataDir    string
	runFile    string
	executable func() (string, error) // defaults to os.Executable when nil; injectable for tests
}

var _ workflowcore.DecisionResolverLauncher = (*decisionResolverLauncher)(nil)

// Preflight resolves the agent adapter for harness and validates its binary
// is runnable, mirroring workflowReviewerLauncher.Preflight.
func (l *decisionResolverLauncher) Preflight(ctx context.Context, harness domain.AgentHarness, workspacePath string) error {
	agent, ok := l.agents.Agent(harness)
	if !ok {
		return fmt.Errorf("no agent adapter for harness %q", harness)
	}
	argv, err := l.buildArgv(ctx, agent, harness, "preflight-only", "", workspacePath, nil)
	if err != nil {
		return fmt.Errorf("resolver command: %w", err)
	}
	if len(argv) == 0 {
		return fmt.Errorf("resolver produced empty command")
	}
	bin := argv[0]
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("resolver binary %q not found: %w", bin, err)
	}
	return nil
}

// Launch resolves the agent adapter, builds a read-only-enforced launch
// command, and creates a fresh runtime pane. Single-shot, mirroring the
// reviewer launcher: each resolution attempt gets exactly one pane.
func (l *decisionResolverLauncher) Launch(ctx context.Context, req workflowcore.DecisionResolverLaunchRequest) (workflowcore.DecisionResolverLaunchResult, error) {
	agent, ok := l.agents.Agent(req.Harness)
	if !ok {
		return workflowcore.DecisionResolverLaunchResult{}, fmt.Errorf("no agent adapter for harness %q", req.Harness)
	}
	if req.ResolverSessionID == "" {
		return workflowcore.DecisionResolverLaunchResult{}, fmt.Errorf("resolver session id is required")
	}
	argv, err := l.buildArgv(ctx, agent, req.Harness, string(req.ResolverSessionID), req.Prompt, req.WorkspacePath, req.RuntimeEnv)
	if err != nil {
		return workflowcore.DecisionResolverLaunchResult{}, fmt.Errorf("resolver command: %w", err)
	}
	if len(argv) == 0 {
		return workflowcore.DecisionResolverLaunchResult{}, fmt.Errorf("resolver produced empty command")
	}

	env := l.runtimeEnv(ctx, req, argv)
	handle, err := l.runtime.Create(ctx, ports.RuntimeConfig{
		SessionID:     req.ResolverSessionID,
		WorkspacePath: req.WorkspacePath,
		Argv:          argv,
		Env:           env,
	})
	if err != nil {
		return workflowcore.DecisionResolverLaunchResult{}, fmt.Errorf("resolver runtime: %w", err)
	}
	return workflowcore.DecisionResolverLaunchResult{HandleID: handle.ID}, nil
}

// buildArgv builds the launch command for one resolver session: the worker
// agent adapter's own GetLaunchCommand (Permissions=Auto, the resolver's own
// read-only tool allowlist for Claude Code), then, for Codex, inserts the
// same real OS-level `--sandbox read-only` flag the reviewer adapter uses.
func (l *decisionResolverLauncher) buildArgv(ctx context.Context, agent ports.Agent, harness domain.AgentHarness, sessionID, prompt, workspacePath string, runtimeEnv map[string]string) ([]string, error) {
	argv, err := agent.GetLaunchCommand(ctx, ports.LaunchConfig{
		SessionID:       sessionID,
		WorkspacePath:   workspacePath,
		Prompt:          prompt,
		Permissions:     ports.PermissionModeAuto,
		AllowedTools:    decisionResolverAllowedTools,
		DisallowedTools: decisionResolverDisallowedTools,
	})
	if err != nil {
		return nil, err
	}
	if harness == domain.HarnessCodex {
		extra, err := decisionResolverCodexReadOnlyArgs(runtimeEnv)
		if err != nil {
			return nil, err
		}
		argv = insertBeforeDoubleDash(argv, extra...)
	}
	return argv, nil
}

// decisionResolverCodexReadOnlyArgs mirrors
// adapters/reviewer/codex.codexReadOnlyArgs exactly: real OS-level read-only
// sandboxing plus the AO location env passthrough a shell command inside the
// sandbox needs to resolve `ao decision resolve`.
func decisionResolverCodexReadOnlyArgs(runtimeEnv map[string]string) ([]string, error) {
	extra := []string{"--sandbox", "read-only"}
	values := map[string]string{
		"AO_PORT":     os.Getenv("AO_PORT"),
		"AO_DATA_DIR": os.Getenv("AO_DATA_DIR"),
		"AO_RUN_FILE": os.Getenv("AO_RUN_FILE"),
		// Checkpoint 8P-B.1: CODEX_HOME must survive codex's own
		// `--sandbox read-only` shell environment policy the same way
		// AO_DATA_DIR does above, or an isolated runtime-home resolved for
		// this launch would be silently dropped once the sandbox strips
		// the ambient environment.
		"CODEX_HOME": runtimeEnv["CODEX_HOME"],
	}
	names := []string{"AO_PORT", "AO_DATA_DIR", "AO_RUN_FILE", "CODEX_HOME"}
	for _, name := range names {
		value := values[name]
		if value == "" {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", name, err)
		}
		extra = append(extra, "-c", "shell_environment_policy.set."+name+"="+string(encoded))
	}
	return extra, nil
}

// insertBeforeDoubleDash mirrors adapters/reviewer/codex.insertBeforePrompt:
// codex's argv carries the initial prompt after a literal "--" separator, so
// extra flags must land before it (or at the end, if the argv has no "--").
func insertBeforeDoubleDash(argv []string, extra ...string) []string {
	for i, arg := range argv {
		if arg == "--" {
			out := make([]string, 0, len(argv)+len(extra))
			out = append(out, argv[:i]...)
			out = append(out, extra...)
			return append(out, argv[i:]...)
		}
	}
	return append(argv, extra...)
}

func (l *decisionResolverLauncher) runtimeEnv(ctx context.Context, req workflowcore.DecisionResolverLaunchRequest, argv []string) map[string]string {
	env := map[string]string{}
	delete(env, sessionmanager.EnvSessionID)
	env["AO_DECISION_RESOLUTION_ID"] = req.ResolutionID
	// P3-E: the usage subject this pane's tokens belong to. It is the RESOLUTION
	// -- the durable authority for one resolver attempt -- so a cross-provider
	// resolution's spend lands under role=decision_resolver rather than being
	// invisible, and never under the worker that asked the question.
	if subject := usageSubjectEnvValue(domain.RuntimePaneSubject(req.ResolutionID)); subject != "" {
		env[usageSubjectEnvName] = subject
	}
	env["AO_DECISION_RESOLVER_SESSION_ID"] = string(req.ResolverSessionID)
	env["AO_DECISION_ASKING_SESSION_ID"] = string(req.AskingSessionID)
	env[sessionmanager.EnvProjectID] = string(req.ProjectID)
	env[sessionmanager.EnvDataDir] = l.dataDir
	if strings.TrimSpace(l.runFile) != "" {
		env["AO_RUN_FILE"] = l.runFile
	}
	executable := l.executable
	if executable == nil {
		executable = os.Executable
	}
	// Same PATH pinning / AO-shim fallback as workflowReviewerLauncher: the
	// resolver's prompt tells it to run `ao decision resolve ...` verbatim,
	// so a bare `ao` must resolve on PATH regardless of what the daemon
	// binary is actually named.
	path, pinned, err := sessionmanager.HookPATH(executable, os.Getenv, env)
	if err != nil {
		env["PATH"] = sessionmanager.EnsureSystemPathDirs(env["PATH"])
	} else {
		env["PATH"] = path
		if !pinned {
			if shimDir, shimErr := sessionmanager.EnsureAOShim(l.dataDir, executable); shimErr == nil {
				env["PATH"] = sessionmanager.PrependPathDir(shimDir, env["PATH"])
			}
		}
	}
	sessionmanager.AugmentRuntimePATHForLaunchBinary(ctx, env, argv, exec.LookPath)
	// Checkpoint 8P-B.1: applied last so the workflow owner's isolated
	// runtime-home always wins. For Codex, buildArgv's
	// decisionResolverCodexReadOnlyArgs already threads CODEX_HOME through
	// the sandbox's own shell_environment_policy separately -- this also
	// sets it (and HOME/CLAUDE_CONFIG_DIR/etc.) directly on RuntimeConfig.Env
	// for every harness, sandboxed or not.
	for k, v := range req.RuntimeEnv {
		env[k] = v
	}
	return env
}
