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

// workflowReviewerRuntime is the narrow runtime surface workflowReviewerLauncher
// needs. runtimeselect.Runtime (the same tmux/conpty adapter every session pane
// in the daemon already uses) satisfies it.
//
// It is create-plus-reconcile rather than create-only, because a launch that can
// only be created is a launch that cannot be recovered: after a crash AO must be
// able to ask whether the reviewer it may or may not have started actually
// exists, and to terminate one it no longer owns. Those two questions are what
// make the deterministic identity useful — see ReviewerIdentity below.
type workflowReviewerRuntime interface {
	Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error)
	IsAlive(ctx context.Context, handle ports.RuntimeHandle) (bool, error)
	Destroy(ctx context.Context, handle ports.RuntimeHandle) error
}

// workflowReviewerLauncher must satisfy the ensure/probe/cancel contract, not
// merely the launch one. The assertion is compile-time on purpose: the protocol
// consumes it through a type assertion, so a drifted signature would not fail to
// build — it would silently disable deterministic recovery in production while
// every test kept passing against the fake that still implemented it.
var _ workflowcore.ReviewerEnsurer = (*workflowReviewerLauncher)(nil)

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

// ReviewerIdentity is the deterministic external identity of one reviewer
// launch: derived purely from the review run id, which AO generates and makes
// durable before anything is created.
//
// It was already the handle Launch used; naming it makes it something AO can
// persist BEFORE the launch and ask about afterwards, which is what turns an
// uncertain replay from a guess into a probe. It must stay pure and stable —
// deriving it from a clock, a counter or a random value would reintroduce the
// exact ambiguity it exists to remove.
func (l *workflowReviewerLauncher) ReviewerIdentity(req workflowcore.ReviewerLaunchRequest) string {
	return "workflow-review-" + req.RunID
}

// ProbeReviewer answers whether the reviewer with that deterministic identity
// exists right now. It is how recovery tells "never launched" from "launched and
// the confirmation was lost".
//
// `known` false is AO admitting it could not tell, never "it is not there": the
// caller must treat that as uncertainty, not as absence.
func (l *workflowReviewerLauncher) ProbeReviewer(ctx context.Context, ref workflowcore.ReviewerRef) (workflowcore.ReviewerObservation, error) {
	presence, instance, err := l.probeReviewerInstance(ctx, ref)
	return workflowcore.ReviewerObservation{Presence: presence, InstanceID: instance}, err
}

// probeReviewerInstance classifies the reviewer AND returns the exact session
// incarnation the classification is about.
//
// The instance travels with the verdict because a verdict alone is not
// actionable: by the time a caller acts on `owned`, the name may belong to a
// different session. Anything destructive must name the instance that was
// actually verified, which is why this is the shape every caller here uses.
func (l *workflowReviewerLauncher) probeReviewerInstance(
	ctx context.Context, ref workflowcore.ReviewerRef,
) (workflowcore.ReviewerPresence, string, error) {
	handleID := ref.HandleID
	if handleID == "" || l.runtime == nil {
		return workflowcore.ReviewerPresenceUnknown, "", nil
	}
	reader, ok := l.sessionFactsReader()
	if !ok {
		// A runtime that cannot answer about a session as ONE INSTANCE cannot
		// support adoption or termination safely: every fact it returns is keyed
		// by a reusable name. Uncertainty is the only honest verdict, and it
		// licenses nothing.
		return workflowcore.ReviewerPresenceUnknown, "", nil
	}

	// The DURABLE instance, when the launch recorded one, is what this asks
	// about. Resolving the name instead would let a replacement answer for a
	// reviewer AO launched — which is precisely what persisting the instance
	// through the launch confirmation exists to prevent.
	facts, exists, err := reader.SessionFacts(ctx, ports.RuntimeHandle{
		ID: handleID, InstanceID: ref.InstanceID,
	})
	if err != nil {
		// Includes ErrRuntimeSessionReplaced: the observation spanned two
		// different sessions, so it is a fact about neither.
		return workflowcore.ReviewerPresenceUnknown, "", err
	}
	if !exists {
		// tmux distinguishes "no such session" from "cannot reach the server"
		// and only the former reaches here, so absence is genuinely proven.
		return workflowcore.ReviewerPresenceAbsent, "", nil
	}

	// A NAME EXISTS. That is the weakest fact available, and on its own it
	// proves nothing: a collision, a stale shell, and AO's own live reviewer are
	// indistinguishable by name.
	//
	// Ownership is proven by the token the runtime attaches AS PART OF creating
	// the session, and the token is correlated — it names the review run this
	// identity belongs to, so it cannot be satisfied by a session that merely
	// echoes its own name back.
	if !facts.OwnerKnown {
		return workflowcore.ReviewerPresenceUnknown, "", nil
	}
	if facts.Owner != reviewerOwnerToken(handleID) {
		return workflowcore.ReviewerPresenceForeign, "", nil
	}

	// PROVEN OURS. One question remains: is the reviewer still running? Session
	// existence does not answer it — AO's own launch command execs a keep-alive
	// when the reviewer exits, so the session outlives the work.
	if !facts.WorkloadKnown {
		// Ownership is proven, but claiming it is running would be a guess, and
		// the caller must not adopt on a guess.
		return workflowcore.ReviewerPresenceUnknown, facts.InstanceID, nil
	}
	if !facts.WorkloadAlive {
		return workflowcore.ReviewerPresenceExited, facts.InstanceID, nil
	}
	return workflowcore.ReviewerPresenceOwned, facts.InstanceID, nil
}

// reviewerOwnerToken is the ownership token for one reviewer identity.
//
// It deliberately is not the identity itself. A marker whose value is the
// session's own name proves only that SOMETHING wrote the name back, which any
// process could do and which carries no information a constant would not. This
// binds the token to AO and to the review run, so a session can be checked
// against what it claims to be.
func reviewerOwnerToken(handleID string) string {
	return "ao-reviewer:" + handleID
}

// sessionFactsReader is the runtime capability that answers about a session as
// one coherent incarnation and can destroy that exact incarnation.
func (l *workflowReviewerLauncher) sessionFactsReader() (ports.SessionFactsReader, bool) {
	reader, ok := l.runtime.(ports.SessionFactsReader)
	return reader, ok
}

// CancelReviewer terminates the reviewer with that identity, idempotently.
//
// Idempotence is the property crash-interrupted cancellation rests on: a replay
// that finds the reviewer already gone must succeed, so the durable
// cancel-intent can always be driven to a confirmation.
//
// COMPARE BEFORE DESTROY. Ownership is proven of a specific session INSTANCE and
// the kill is aimed at that instance, never at the name. Verifying a name and
// then killing a name is a window a replacement can walk into: AO would prove
// its own session owned, that session would exit, a stranger would take the
// freed name, and the kill would land on the stranger. Targeting `$N` makes that
// unreachable — the id belongs to one incarnation for the life of the server.
func (l *workflowReviewerLauncher) CancelReviewer(ctx context.Context, ref workflowcore.ReviewerRef) error {
	handleID := ref.HandleID
	if handleID == "" || l.runtime == nil {
		return nil
	}
	presence, instance, perr := l.probeReviewerInstance(ctx, ref)
	if perr != nil {
		return perr
	}
	if presence == workflowcore.ReviewerPresenceAbsent {
		return nil
	}
	// Both `owned` and `exited` are proofs of AO's own ownership, and both are
	// safe to destroy — an exited reviewer still holds its session name, and
	// reclaiming it is the only way that identity becomes reusable. `foreign`
	// and `unknown` are never destroyed: AO cannot prove the session is its own,
	// and killing something on that basis is the one failure worse than leaving
	// an orphan.
	if !presence.LicensesTermination() {
		// Deterministic, and deterministically refused: AO cannot prove this
		// session is its own, so it will not kill it -- now or on any retry.
		// Marked unrecoverable so the wake scheduler stops re-driving the run
		// into the identical refusal instead of parking it for a person.
		return fmt.Errorf("%w: reviewer %s is %s; refusing to terminate a session AO cannot prove it owns",
			workflowcore.ErrUnrecoverable, handleID, presence)
	}
	reader, ok := l.sessionFactsReader()
	if !ok || instance == "" {
		return fmt.Errorf(
			"%w: reviewer %s cannot be terminated: AO has no stable identity for the session it verified",
			workflowcore.ErrUnrecoverable, handleID)
	}

	// REVALIDATE IMMEDIATELY BEFORE THE DESTRUCTIVE ACT. The verdict above was
	// true when it was taken; this proves the same incarnation is still there,
	// so the kill cannot be redirected by a replacement in between.
	// Addressed to the INSTANCE, not the name. Re-resolving the name here would
	// hand the decision back to whatever holds it now — the exact window the
	// verdict above was taken to close.
	verified := ports.RuntimeHandle{ID: handleID, InstanceID: instance}
	current, exists, cerr := reader.SessionFacts(ctx, verified)
	if cerr != nil {
		return cerr
	}
	if !exists {
		// That incarnation is gone on its own. Nothing to do, and nothing to
		// kill — and emphatically not whatever now answers to its name.
		return nil
	}
	if current.InstanceID != instance {
		return fmt.Errorf(
			"reviewer %s: the session behind this name changed from %s to %s before termination; refusing to destroy it",
			handleID, instance, current.InstanceID)
	}
	if err := reader.DestroyInstance(ctx, instance); err != nil {
		return err
	}
	// Confirm THAT instance is gone — not merely that the name is free, which a
	// replacement would also satisfy.
	after, stillThere, aerr := reader.SessionFacts(ctx, verified)
	if aerr != nil {
		// Cannot verify. The caller retries rather than recording a
		// confirmation AO cannot stand behind.
		return aerr
	}
	if stillThere && after.InstanceID == instance {
		return fmt.Errorf("reviewer %s: session instance %s survived termination", handleID, instance)
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
	handleID := l.ReviewerIdentity(req)
	inv := ports.ReviewInvocation{
		ReviewerID:      handleID,
		RunID:           req.RunID,
		WorkerSessionID: req.WorkerSessionID,
		WorkspacePath:   req.WorkspacePath,
		DataDir:         l.dataDir,
		RunFilePath:     l.runFile,
		Prompt:          req.Prompt,
		SystemPrompt:    req.SystemPrompt,
		// Checkpoint 8P-E.3.1: the same isolated per-user runtime env (8P-B.1)
		// already resolved into req.RuntimeEnv below for the runtime pane
		// itself must also reach PreLaunch, or a Claude reviewer's trust
		// record lands in a config file the isolated subprocess never reads
		// -- the exact bug 8P-E.3 fixed for workers, reproduced here for
		// reviewers.
		Env: req.RuntimeEnv,
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
	// OWNERSHIP TRAVELS WITH CREATION.
	//
	// This used to be a marker written after Create returned, and that ordering
	// was the defect: a crash — or merely a failed write, since its error was
	// discarded — left a live reviewer AO could not identify, and therefore
	// could neither adopt nor terminate, forever. Handing the token to the
	// runtime makes ownership part of the same operation that makes the session
	// exist, and the runtime destroys anything it cannot stamp rather than
	// returning a handle to it.
	handle, err := l.runtime.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(handleID),
		WorkspacePath: workingDirectory,
		Argv:          cmd.Argv,
		Env:           env,
		Owner:         reviewerOwnerToken(handleID),
	})
	if err != nil {
		return workflowcore.ReviewerLaunchResult{}, fmt.Errorf("reviewer runtime: %w", err)
	}
	// The INSTANCE the runtime just created travels back with the result, so the
	// confirmation can persist it. Without it the launch would be addressable
	// only by a reusable name, and a replacement could answer for it forever
	// after.
	return workflowcore.ReviewerLaunchResult{HandleID: handle.ID, InstanceID: handle.InstanceID}, nil
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
	// SUPERVISED, so that the reviewer's EXIT is observable.
	//
	// Without this the runtime replaces a finished process with an interactive
	// keep-alive shell (buildLaunchCommand), and every liveness probe then
	// answers about that shell instead of the reviewer: a review that ended
	// hours ago still reads as running, and AO adopts it forever. Supervised
	// mode swaps that arbitrary shell for AO's own deterministic sentinel, which
	// is what the workload-liveness probe recognises and reports as exited.
	env[sessionmanager.EnvSupervisedProcess] = "1"
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
	// Checkpoint 8P-B.1: applied last, after every other env source (PATH
	// pin/shim included), so the workflow owner's isolated runtime-home
	// always wins. Nil req.RuntimeEnv is a no-op.
	for k, v := range req.RuntimeEnv {
		env[k] = v
	}
	return env
}
