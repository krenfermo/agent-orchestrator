package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// incident_agent_launcher.go — Checkpoint 8P-E.19.
//
// The production launcher for the Incident Advisor's Diagnostic Agent. It is a
// close sibling of decisionResolverLauncher, deliberately: that launcher
// already solved every hard part of starting an isolated, read-only agent that
// reports back through one controlled CLI call — PATH pinning so a bare `ao`
// resolves, the AO shim fallback, runtime-home passthrough, and per-harness
// read-only enforcement using each harness's own real mechanism. Re-deriving
// any of that would have produced a second, worse copy.
//
// What differs is the mandate, and the difference is the whole point.
//
// # Read-only, and narrower than read-only
//
// A decision resolver reads the worktree to answer a question about it. A
// Diagnostic Agent must NOT: its entire evidence is the Incident Context Pack
// it was handed, bounded and recorded, so that a diagnosis can be tied to
// exactly what it saw and a second run over the same pack is comparable. An
// agent free to grep the repository produces a diagnosis nobody can reproduce
// and a bill nobody predicted.
//
// So the Claude Code allowlist here contains ONE tool: the submit call. No
// Read, no Grep, no Glob, no git. The prompt says "diagnose only from the pack";
// this is what makes that true rather than aspirational.
//
// Codex has no per-tool allowlist, so its enforcement is `--sandbox read-only`,
// the same real OS-level mechanism the reviewer and resolver adapters use. That
// is a genuine asymmetry and worth stating plainly: a Codex diagnostic agent
// cannot WRITE anything, but it could read files in its working directory. That
// is why the working directory is an empty AO-owned scratch directory rather
// than the run's worktree — under Codex the isolation is the directory, under
// Claude Code it is the toolset, and under both the agent has nothing to gain
// from looking around.

// incidentAgentRuntime is the narrow pane-creation surface this launcher needs,
// mirroring decisionResolverRuntime — the same generic runtime adapter every
// other session in the daemon uses, not a new mechanism.
type incidentAgentRuntime interface {
	Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error)
}

// incidentDiagnosticAllowedTools is the Claude Code allowlist for a diagnostic
// session: exactly one thing, the controlled way to deliver its answer.
//
// `printf` is included only because an agent composing a JSON argument
// sometimes reaches for it, and denying it produces a stuck agent rather than a
// safer one. It cannot write files.
var incidentDiagnosticAllowedTools = []string{
	"Bash(printf:*)",
	"Bash(ao incident submit:*)",
}

// incidentDiagnosticDisallowedTools hard-denies everything that could edit the
// repository, reach the network, or wander outside the pack — defence in depth
// behind the allowlist, and the explicit statement of what a diagnostician is
// not allowed to become.
var incidentDiagnosticDisallowedTools = []string{
	"Edit",
	"Write",
	"NotebookEdit",
	"Read",
	"Grep",
	"Glob",
	"WebFetch",
	"WebSearch",
	"Bash(git push:*)",
	"Bash(git commit:*)",
	"Bash(git checkout:*)",
	"Bash(git merge:*)",
	"Bash(git rebase:*)",
	"Bash(git reset:*)",
	"Bash(git stash:*)",
	"Bash(rm:*)",
}

// incidentAgentLauncher is the concrete workflowcore.IncidentAgentLauncher.
type incidentAgentLauncher struct {
	agents     ports.AgentResolver
	runtime    incidentAgentRuntime
	dataDir    string
	runFile    string
	executable func() (string, error) // defaults to os.Executable; injectable for tests
}

var _ workflowcore.IncidentAgentLauncher = (*incidentAgentLauncher)(nil)

// LaunchDiagnostic starts one read-only diagnostic pane for one incident
// generation.
//
// Single-shot by contract: the caller (workflow) owns the single-flight claim,
// so this function's job is to start exactly one pane or return an error that
// releases that claim. It never retries and never adopts an existing pane.
func (l *incidentAgentLauncher) LaunchDiagnostic(ctx context.Context, req workflowcore.IncidentAgentRequest) (workflowcore.IncidentAgentResult, error) {
	if !req.ReadOnly {
		// The caller asked for a diagnostic launch without the read-only
		// mandate. Refuse rather than quietly launching a writable agent: the
		// flag is a contract, and honouring it only when convenient is how a
		// safety property becomes a comment.
		return workflowcore.IncidentAgentResult{}, errors.New("incident diagnostic launch must be read-only")
	}
	harness := domain.AgentHarness(strings.TrimSpace(req.Harness))
	if harness == "" {
		return workflowcore.IncidentAgentResult{}, errors.New("incident diagnostic launch needs a routed harness")
	}
	agent, ok := l.agents.Agent(harness)
	if !ok {
		return workflowcore.IncidentAgentResult{}, fmt.Errorf("no agent adapter for harness %q", harness)
	}

	sessionID := incidentAgentSessionID(req.IncidentID)
	workspace, err := l.scratchWorkspace(req.IncidentID)
	if err != nil {
		return workflowcore.IncidentAgentResult{}, fmt.Errorf("incident scratch workspace: %w", err)
	}

	argv, err := l.buildArgv(ctx, agent, harness, sessionID, req.Prompt, workspace)
	if err != nil {
		return workflowcore.IncidentAgentResult{}, fmt.Errorf("incident diagnostic command: %w", err)
	}
	if len(argv) == 0 {
		return workflowcore.IncidentAgentResult{}, errors.New("incident diagnostic produced an empty command")
	}

	env := l.runtimeEnv(ctx, req, sessionID, argv)
	handle, err := l.runtime.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(sessionID),
		WorkspacePath: workspace,
		Argv:          argv,
		Env:           env,
	})
	if err != nil {
		return workflowcore.IncidentAgentResult{}, fmt.Errorf("incident diagnostic runtime: %w", err)
	}
	return workflowcore.IncidentAgentResult{SessionID: handle.ID, Harness: string(harness)}, nil
}

// LaunchRepair is not implemented yet, and says so rather than falling back to
// something weaker.
//
// A Repair Agent writes to AO's own source. Wiring it before the diagnostic
// path is demonstrated end to end would put the dangerous half of this feature
// into production on the strength of tests alone, which is exactly the order of
// operations this checkpoint was asked to avoid. The port, the policy, the
// human approval gate and the prompt are all in place; only this launch is
// deliberately absent.
func (l *incidentAgentLauncher) LaunchRepair(_ context.Context, _ workflowcore.IncidentAgentRequest) (workflowcore.IncidentAgentResult, error) {
	return workflowcore.IncidentAgentResult{}, errors.New("the repair agent launcher is not enabled yet; approve and run the repair manually for now")
}

// incidentAgentSessionID names the pane after the incident, so one incident has
// at most one diagnostic session and an operator can find it by eye.
func incidentAgentSessionID(incidentID string) string {
	return "incident-" + strings.TrimPrefix(incidentID, "inc-")
}

// scratchWorkspace is the empty, AO-owned directory a diagnostic agent runs in.
//
// It is emphatically NOT the run's worktree. Under Codex — whose read-only
// enforcement is a sandbox rather than a toolset — the working directory is the
// isolation, and pointing it at the code under investigation would hand the
// agent exactly the unbounded evidence the context pack exists to replace.
func (l *incidentAgentLauncher) scratchWorkspace(incidentID string) (string, error) {
	base := l.dataDir
	if strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "incidents", strings.TrimPrefix(incidentID, "inc-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// buildArgv builds the launch command, applying each harness's own real
// read-only mechanism.
func (l *incidentAgentLauncher) buildArgv(ctx context.Context, agent ports.Agent, harness domain.AgentHarness, sessionID, prompt, workspace string) ([]string, error) {
	argv, err := agent.GetLaunchCommand(ctx, ports.LaunchConfig{
		SessionID:       sessionID,
		WorkspacePath:   workspace,
		Prompt:          prompt,
		Permissions:     ports.PermissionModeAuto,
		AllowedTools:    incidentDiagnosticAllowedTools,
		DisallowedTools: incidentDiagnosticDisallowedTools,
	})
	if err != nil {
		return nil, err
	}
	if harness == domain.HarnessCodex {
		extra, err := decisionResolverCodexReadOnlyArgs(nil)
		if err != nil {
			return nil, err
		}
		argv = insertBeforeDoubleDash(argv, extra...)
	}
	return argv, nil
}

// runtimeEnv mirrors decisionResolverLauncher.runtimeEnv: the same PATH pinning
// and AO-shim fallback, so the bare `ao incident submit ...` the prompt asks for
// resolves regardless of what the daemon binary is called.
func (l *incidentAgentLauncher) runtimeEnv(ctx context.Context, req workflowcore.IncidentAgentRequest, sessionID string, argv []string) map[string]string {
	env := map[string]string{}
	env["AO_INCIDENT_ID"] = req.IncidentID
	env["AO_INCIDENT_RUN_ID"] = req.RunID
	env["AO_INCIDENT_PACK_DIGEST"] = req.PackDigest
	env[sessionmanager.EnvProjectID] = req.ProjectID
	env[sessionmanager.EnvDataDir] = l.dataDir
	if strings.TrimSpace(l.runFile) != "" {
		env["AO_RUN_FILE"] = l.runFile
	}
	executable := l.executable
	if executable == nil {
		executable = os.Executable
	}
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
	return env
}
