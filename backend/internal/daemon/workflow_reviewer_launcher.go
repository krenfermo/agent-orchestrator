package daemon

import (
	"context"
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

// workflowReviewerRuntime is the narrow runtime-pane-creation surface
// workflowReviewerLauncher needs. runtimeselect.Runtime (the same tmux/conpty
// adapter every session pane in the daemon already uses) satisfies it.
type workflowReviewerRuntime interface {
	Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error)
}

// workflowReviewerLauncher is Checkpoint 8C's concrete workflowcore.ReviewerLauncher.
//
// It deliberately does NOT wrap internal/review.Launcher (built by
// reviewcore.NewLauncher and already wired above in startSession for AO's
// normal PR-based review flow): that launcher's Spawn/Notify always build
// their prompt/system-prompt internally via the unexported reviewTexts()
// (backend/internal/review/prompt.go), which unconditionally instructs the
// reviewer to post a GitHub PR review via `gh api .../pulls/{number}/reviews`
// and diff against a PR's base branch — there is no override hook, and 8B's
// workflow-spawned worker sessions are explicitly told never to open a PR, so
// that prompt is fundamentally incompatible with a workflow-triggered review
// (see backend/internal/workflow/review_prompt.go's doc comment for the full
// reasoning; this is a documented, deliberate adaptation, the same way 8B
// documented its own CDC deviation).
//
// What IS reused unmodified: the reviewer registry/resolver
// (ports.ReviewerResolver, resolving "claude-code" to the exact same adapter
// instance internal/review's own engine uses) and that adapter's own
// ReviewCommand method, which alone builds the read-only tool allowlist/
// denylist and permission mode (adapters/reviewer/claudecode/claudecode.go)
// from whatever ports.ReviewInvocation it is given — the adapter itself has
// no PR assumption baked in, only review/launcher.go's invocation-building
// wrapper does. This type builds its own ReviewInvocation (carrying
// workflow's own prompt) and calls the adapter directly, then spawns the
// runtime pane through the exact same generic runtime port every other
// session pane in the daemon already uses (workflowReviewerRuntime above) —
// not a new or more permissive mechanism. PATH pinning for the reviewer's
// bare `ao` command reuses session_manager's own exported HookPATH/
// EnsureAOShim/AugmentRuntimePATHForLaunchBinary helpers — the same ones
// review/launcher.go itself calls, not a separate re-implementation
// (Checkpoint 8I.2 closed a gap where this launcher had HookPATH but not the
// EnsureAOShim fallback, so a daemon binary not literally named "ao" left
// `ao review submit` unresolvable here even though review/launcher.go's
// equivalent path already handled it).
type workflowReviewerLauncher struct {
	reviewers  ports.ReviewerResolver
	runtime    workflowReviewerRuntime
	dataDir    string
	runFile    string
	auth       reviewerAgentAuth
	executable func() (string, error) // defaults to os.Executable when nil; injectable for tests
}

var _ workflowcore.ReviewerLauncher = (*workflowReviewerLauncher)(nil)

// Preflight mirrors internal/review.agentLauncher's own Preflight: resolve
// the adapter, build its real command, and validate the binary is runnable.
func (l *workflowReviewerLauncher) Preflight(ctx context.Context, harness domain.ReviewerHarness, workspacePath string) error {
	reviewer, ok := l.reviewers.Reviewer(harness)
	if !ok {
		return fmt.Errorf("no reviewer adapter for harness %q", harness)
	}
	cmd, err := reviewer.ReviewCommand(ctx, ports.ReviewInvocation{WorkspacePath: workspacePath})
	if err != nil {
		return fmt.Errorf("reviewer command: %w", err)
	}
	if len(cmd.Argv) == 0 {
		return fmt.Errorf("reviewer produced empty command")
	}
	bin := cmd.Argv[0]
	if filepath.Base(bin) == "env" {
		for _, arg := range cmd.Argv[1:] {
			if !strings.Contains(arg, "=") {
				bin = arg
				break
			}
		}
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("reviewer binary %q not found: %w", bin, err)
	}
	if l.auth.agents != nil {
		status, ok, err := l.auth.AuthStatus(ctx, harness)
		if err != nil {
			return fmt.Errorf("agent auth catalog for reviewer harness %q: %w", harness, err)
		}
		if ok && status == ports.AgentAuthStatusUnauthorized {
			return fmt.Errorf("agent auth catalog reports reviewer harness %q is unauthorized", harness)
		}
	}
	return nil
}

// Launch resolves the adapter, builds a workflow-owned ReviewInvocation, and
// creates a fresh runtime pane — a single-shot launch (workflow never resumes
// or re-notifies an existing reviewer pane; each review step gets exactly one
// review_run and one pane, matching Checkpoint 8C's "review once, stop" design).
func (l *workflowReviewerLauncher) Launch(ctx context.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	reviewer, ok := l.reviewers.Reviewer(req.Harness)
	if !ok {
		return workflowcore.ReviewerLaunchResult{}, fmt.Errorf("no reviewer adapter for harness %q", req.Harness)
	}
	handleID := "workflow-review-" + req.RunID
	inv := ports.ReviewInvocation{
		ReviewerID:      handleID,
		RunID:           req.RunID,
		WorkerSessionID: req.WorkerSessionID,
		WorkspacePath:   req.WorkspacePath,
		DataDir:         l.dataDir,
		RunFilePath:     l.runFile,
		Prompt:          req.Prompt,
		SystemPrompt:    req.SystemPrompt,
	}
	// Mirror internal/review's own launcher exactly (launcher.go's
	// launchReviewerTerminalWithMode): PreLaunch is an optional capability
	// (not part of the core ports.Reviewer contract) that, for Claude Code,
	// installs the reviewer's hooks and — critically — records the worktree
	// as trusted in Claude's own config before the pane starts. Skipping
	// this call is what caused the real E2E run to hang on Claude Code's
	// interactive "do you trust this folder?" dialog: a workflow-owned
	// worktree is always brand new to Claude, so it has no prior trust
	// entry and blocks forever without this step (no such prompt applies to
	// Codex's reviewer adapter, which enforces read-only via a sandbox flag
	// instead, so this was invisible until testing against real Claude).
	if pl, ok := reviewer.(interface {
		PreLaunch(context.Context, ports.ReviewInvocation) error
	}); ok {
		if err := pl.PreLaunch(ctx, inv); err != nil {
			return workflowcore.ReviewerLaunchResult{}, fmt.Errorf("reviewer pre-launch: %w", err)
		}
	}
	cmd, err := reviewer.ReviewCommand(ctx, inv)
	if err != nil {
		return workflowcore.ReviewerLaunchResult{}, fmt.Errorf("reviewer command: %w", err)
	}
	if len(cmd.Argv) == 0 {
		return workflowcore.ReviewerLaunchResult{}, fmt.Errorf("reviewer produced empty command")
	}
	workingDirectory := cmd.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory = req.WorkspacePath
	}
	env := l.runtimeEnv(ctx, req, cmd.Argv, cmd.Env)
	handle, err := l.runtime.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(handleID),
		WorkspacePath: workingDirectory,
		Argv:          cmd.Argv,
		Env:           env,
	})
	if err != nil {
		return workflowcore.ReviewerLaunchResult{}, fmt.Errorf("reviewer runtime: %w", err)
	}
	return workflowcore.ReviewerLaunchResult{HandleID: handle.ID}, nil
}

func (l *workflowReviewerLauncher) runtimeEnv(ctx context.Context, req workflowcore.ReviewerLaunchRequest, argv []string, base map[string]string) map[string]string {
	env := make(map[string]string, len(base)+4)
	// Checkpoint 8M.1: skip Python's .pyc bytecode cache for reviewer tool
	// execution (e.g. pytest run read-only during review). base can still
	// override this per-adapter below.
	env["PYTHONDONTWRITEBYTECODE"] = "1"
	for k, v := range base {
		env[k] = v
	}
	delete(env, sessionmanager.EnvSessionID)
	env["AO_REVIEW_SESSION_ID"] = req.ReviewID
	env["AO_REVIEW_WORKER_SESSION_ID"] = string(req.WorkerSessionID)
	env["AO_REVIEW_HARNESS"] = string(req.Harness)
	env[sessionmanager.EnvProjectID] = string(req.ProjectID)
	env[sessionmanager.EnvDataDir] = l.dataDir
	if strings.TrimSpace(l.runFile) != "" {
		env["AO_RUN_FILE"] = l.runFile
	}
	executable := l.executable
	if executable == nil {
		executable = os.Executable
	}
	// HookPATH now always returns a usable PATH (base/inherited PATH plus
	// required system dirs) once the daemon's own executable path resolves at
	// all — err here means that resolution itself failed, not merely that the
	// binary isn't named "ao" (see HookPATH's doc comment). Previously this
	// silently left PATH unset on either failure mode, which could collapse a
	// reviewer pane's PATH down to nothing but its own agent binary's
	// directory and break that agent CLI's own auth lookup (Checkpoint 8I.1).
	path, pinned, err := sessionmanager.HookPATH(executable, os.Getenv, env)
	if err != nil {
		env["PATH"] = sessionmanager.EnsureSystemPathDirs(env["PATH"])
	} else {
		env["PATH"] = path
		if !pinned {
			// The daemon binary isn't named "ao" — the reviewer prompt tells
			// Claude to run `ao review submit ...` verbatim, so without a real
			// `ao` on PATH that command fails with "command not found" and the
			// reviewer has to self-correct onto an absolute path (Checkpoint
			// 8I.2's residual gap). EnsureAOShim is the same mechanism
			// review/launcher.go uses for its own reviewer pane: a tiny shim
			// script that execs the resolved daemon binary, prepended on top of
			// the already-good PATH above rather than replacing it.
			if shimDir, shimErr := sessionmanager.EnsureAOShim(l.dataDir, executable); shimErr == nil {
				env["PATH"] = sessionmanager.PrependPathDir(shimDir, env["PATH"])
			}
		}
	}
	sessionmanager.AugmentRuntimePATHForLaunchBinary(ctx, env, argv, exec.LookPath)
	return env
}
