// Package tmux implements ports.Runtime using tmux sessions on Darwin/Linux.
package tmux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/ptyexec"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	defaultTimeout = 5 * time.Second
	// defaultChunkBytes is the inline send-keys chunk size. It was 16 KB, which
	// is tmux's ENTIRE per-command imsg budget — so the very first chunk of a
	// large prompt was rejected with "command too long" (Checkpoint 8P-E.13C).
	// It is now the same conservative inline budget ports.PromptTransportFor
	// switches transports at, so nothing that still travels inline can reach
	// the ceiling.
	defaultChunkBytes = ports.MaxInlinePromptBytes
	// maxInlineLaunchCommandBytes is the largest launch command AO will put
	// inside `tmux new-session` directly. Above it the command body moves into a
	// sourced script file. Larger than the message budget because a launch
	// command is mostly AO-generated env/exports whose size is known, and the
	// same 16 KB frame still has to hold it.
	maxInlineLaunchCommandBytes = 8 * 1024
	// defaultEnterDelay mirrors conpty's ptyInputEnterDelay: a pause after pasting
	// a non-empty message, before the trailing Enter, so a large multiline paste
	// does not absorb the Enter and leave the prompt unsubmitted (issue #2342).
	defaultEnterDelay = 300 * time.Millisecond
	// defaultReapGrace is how long Destroy waits between SIGTERM and SIGKILL when
	// reaping a pane's leftover background processes, giving them a chance to
	// exit cleanly (release ports) before being forced (issue #2523). It is a
	// ceiling, not a fixed wait: reapPollInterval decides how soon a pane that
	// is already empty lets Destroy return.
	defaultReapGrace = 5 * time.Second
	// reapPollInterval is how often the reap rechecks for survivors while the
	// grace runs. A plain shell exits within a tick or two, so Destroy returns
	// in roughly this long instead of always burning the full grace — which the
	// DELETE handler blocks on, and the user sees as a tab that will not close.
	reapPollInterval = 50 * time.Millisecond
	// defaultSocket names AO's own tmux server (`tmux -L <socket>`). tmux
	// starts a brand-new server per distinct -L name on first use, so this is
	// what keeps AO off the user's default tmux server (issue: AO sessions
	// were sharing the user's personal `tmux` server, a decades-old shared
	// server with dozens of unrelated sessions, and a daemon-triggered
	// operation there risked touching sessions AO never created). Callers
	// (runtimeselect, wired from config) normally pass a data-dir-derived
	// socket name via Options.Socket so distinct AO instances (distinct
	// AO_DATA_DIR) never collide; this constant is only the last-resort
	// default when Options.Socket is left empty, so AO is never accidentally
	// unisolated.
	defaultSocket = "ao"
)

var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

var getenv = os.Getenv

// Options configures a tmux Runtime. Every field has a sensible default (see
// New), so the zero value is usable.
type Options struct {
	Binary     string        // default "tmux" (resolved via exec.LookPath)
	Shell      string        // default $SHELL else /bin/sh
	Timeout    time.Duration // default 5s
	ChunkSize  int           // default ports.MaxInlinePromptBytes
	EnterDelay time.Duration // pause after pasting a non-empty message before pressing Enter; default defaultEnterDelay. Conpty already does this (ptyInputEnterDelay); tmux lacked it, so a large multiline paste could absorb the trailing Enter and leave the prompt unsubmitted (issue #2342).
	ReapGrace  time.Duration // grace between SIGTERM and SIGKILL when reaping a pane's leftover background processes on Destroy; default defaultReapGrace.
	// Socket names the tmux server (`tmux -L <Socket>`) every operation this
	// Runtime issues runs against. Every command — new-session, has-session,
	// list-sessions, attach-session, send-keys, kill-session, capture-pane —
	// is scoped to this server; AO never touches the caller's default tmux
	// server. Empty defaults to defaultSocket ("ao"), so the zero value is
	// still isolated. Callers wired from the daemon config normally pass a
	// AO_DATA_DIR-derived name instead, so distinct AO instances (distinct
	// data dirs, e.g. two profiles or a test sandbox) get distinct servers.
	Socket string
	// ScratchDir is where the runtime stages transient payload files: an
	// oversized prompt on its way into a tmux paste buffer, and an oversized
	// launch command on its way into the pane's shell (Checkpoint 8P-E.13C).
	// Both are written 0600 and removed as soon as tmux has read them. Empty
	// falls back to the OS temp dir; the daemon passes an AO-owned directory so
	// these files live under the data dir with everything else AO writes.
	ScratchDir string
}

// Runtime runs agent sessions inside tmux sessions, driving them via the tmux
// CLI. It implements ports.Runtime.
type Runtime struct {
	binary       string
	shell        string
	socket       string
	timeout      time.Duration
	chunkSize    int
	scratchDir   string
	enterDelay   time.Duration
	reapGrace    time.Duration
	runner       runner
	reapSessions func(ctx context.Context, pids []int, grace time.Duration)
}

var _ ports.Runtime = (*Runtime)(nil)
var _ ports.Attacher = (*Runtime)(nil)

type runner interface {
	Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
}

// killSessionsByPID force-terminates every process in each pid's tmux pane
// session. tmux runs each pane in its own session (pane pid == session id), so
// signaling the session reaps the pane's background children — e.g. a dev
// server a worker started with `&` — that `kill-session`'s SIGHUP leaves
// running. It SIGTERMs, waits grace for a clean exit, then
// SIGKILLs survivors. Best-effort: `pkill` is absent on Windows, where tmux is
// never the runtime, so the calls simply no-op there.
func killSessionsByPID(ctx context.Context, pids []int, grace time.Duration) {
	reapPaneSessions(ctx, pids, grace, signalSessions, sessionsHaveProcesses)
}

// reapPaneSessions is killSessionsByPID's logic with the pkill/pgrep calls
// injected, so the SIGTERM → wait → SIGKILL sequence is testable without real
// processes.
func reapPaneSessions(
	ctx context.Context,
	pids []int,
	grace time.Duration,
	signal func(ctx context.Context, pids []int, sig string) bool,
	hasProcesses func(ctx context.Context, pids []int) bool,
) {
	if len(pids) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace+5*time.Second)
	defer cancel()

	// `-s` is a Linux procps extension; BSD/macOS pkill rejects it outright. When
	// the platform cannot signal by session id, no amount of waiting reaps
	// anything — the SIGTERM never landed and the SIGKILL would not either — so
	// return instead of blocking the caller for the whole grace. Destroy runs
	// inside the shell-terminal DELETE handler, and that dead wait was the
	// several-second delay users saw when closing a terminal on macOS.
	if !signal(cleanupCtx, pids, "-TERM") {
		return
	}
	if !hasProcesses(cleanupCtx, pids) {
		return
	}

	// Poll rather than sleep the whole grace. Callers block on this (Destroy runs
	// inside the shell-terminal DELETE handler), and the common case — an
	// interactive shell with nothing behind it — is empty almost immediately. A
	// process that really needs the time still gets the full grace before SIGKILL.
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(reapPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-cleanupCtx.Done():
			return
		case <-ticker.C:
			if !hasProcesses(cleanupCtx, pids) {
				return
			}
		case <-deadline.C:
			if !hasProcesses(cleanupCtx, pids) {
				return
			}
			signal(cleanupCtx, pids, "-KILL")
			return
		}
	}
}

// signalSessions sends a pkill signal flag (e.g. "-TERM") to every process in
// each pane session, matched by session id via `pkill -s`. It reports whether
// the platform supports signalling by session id at all: exit 2 is a usage
// error on both procps and BSD pkill, which is how macOS answers `-s`, and
// there the call reaches no process.
func signalSessions(ctx context.Context, pids []int, sig string) bool {
	supported := false
	for _, pid := range pids {
		err := exec.CommandContext(ctx, "pkill", sig, "-s", strconv.Itoa(pid)).Run()
		if !isUnsupportedMatcher(err) {
			supported = true
		}
	}
	return supported
}

// isUnsupportedMatcher reports whether a pgrep/pkill invocation failed because
// the platform rejects the matcher itself (exit 2, a usage error) rather than
// because nothing matched (exit 1) or the process is missing entirely.
func isUnsupportedMatcher(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() >= 2
	}
	// pkill/pgrep absent (Windows, minimal containers): equally unusable.
	return true
}

// sessionsHaveProcesses reports whether any process remains in the pane
// sessions. `pgrep` exit 1 means no matches; other failures are treated as
// survivors so Destroy stays conservative and still attempts SIGKILL.
func sessionsHaveProcesses(ctx context.Context, pids []int) bool {
	for _, pid := range pids {
		err := exec.CommandContext(ctx, "pgrep", "-s", strconv.Itoa(pid)).Run()
		if err == nil || ctx.Err() != nil {
			return true
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return true
		}
	}
	return false
}

// nestedAgentEnvVars are environment variables that mark the current process
// as itself running inside an already-active Claude Code agent session
// (CLAUDECODE/CLAUDE_CODE_*) or otherwise identify that parent agent process
// (CLAUDE_PID, CLAUDE_EFFORT). tmux's server is a persistent process that
// inherits and keeps the environment of whoever's `tmux -L <socket>` call
// first started it, for the server's entire lifetime — every later session
// and pane spawned on it inherits that same ambient environment, regardless
// of which later process asked for the new session/pane. If AO's daemon is
// itself started from inside another agent's shell (as happens routinely
// during development, or when one agent supervises another), these vars leak
// into AO's own isolated tmux server and from there into every worker and
// reviewer pane it ever spawns, misidentifying independent reviewer
// processes as nested child sessions of the parent agent with no standalone
// auth (Checkpoint 8I.1/8I.2). Deliberately narrow and evidence-based rather
// than a broad deny-list: every entry here was observed leaking during that
// investigation. Everything else in the inherited environment (PATH, HOME,
// USER, locale, credentials helpers, etc.) is preserved unchanged — sessions
// still need a normal, working shell environment to run real agent CLIs.
var nestedAgentEnvVars = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_EXECPATH",
	"CLAUDE_PID",
	"CLAUDE_EFFORT",
}

// sanitizeInheritedEnv drops nestedAgentEnvVars from a copy of environ,
// leaving every other variable untouched. Applied to every tmux CLI
// invocation (see execRunner.Run) since the very first one may be the call
// that auto-starts AO's tmux server, and that call's environment becomes the
// server's permanent ambient environment.
func sanitizeInheritedEnv(environ []string) []string {
	deny := make(map[string]struct{}, len(nestedAgentEnvVars))
	for _, name := range nestedAgentEnvVars {
		deny[name] = struct{}{}
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			name = kv[:idx]
		}
		if _, denied := deny[name]; denied {
			continue
		}
		out = append(out, kv)
	}
	return out
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(sanitizeInheritedEnv(os.Environ()), env...)
	// Run from a stable directory, not whatever the daemon process's cwd happens
	// to be. The first tmux CLI call auto-starts tmux's persistent server, which
	// inherits ITS launching process's cwd and keeps it for the server's entire
	// lifetime, regardless of what any later `new-session -c <dir>` asks for
	// (issue #2775). A packaged desktop build can start the daemon with its cwd
	// inside a Squirrel/ShipIt staging directory that the very next auto-update
	// deletes, permanently pinning the tmux server to a path that no longer
	// exists. os.TempDir() outlives app bundle swaps and update staging dirs, so
	// pinning here keeps the server cwd valid across the app's lifetime.
	cmd.Dir = stableRunDir()
	return cmd.CombinedOutput()
}

// stableRunDir returns the directory execRunner.Run pins the tmux CLI to.
//
// os.TempDir() is the preferred answer (see execRunner.Run), but it returns
// $TMPDIR verbatim without checking that it exists. A stale or bogus TMPDIR
// would then make exec fail with "chdir <dir>: no such file or directory" on
// EVERY tmux command, taking the whole runtime down for exactly the reason
// #2775 did: a cwd that no longer exists. So stat the candidates and degrade
// rather than hard-fail. The last resort is the empty string, which leaves
// cmd.Dir unset so the command inherits the daemon's own cwd: that is the
// pre-fix behavior and merely risks the poisoned-server race the pin avoids,
// which the retry in verifyPaneWorkingDirectory already tolerates.
func stableRunDir() string {
	candidates := []string{os.TempDir()}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home)
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

// New builds a tmux Runtime, filling unset Options with defaults: binary "tmux"
// (resolved via exec.LookPath), shell from $SHELL (else /bin/sh), and the
// default timeout and output chunk size.
func New(opts Options) *Runtime {
	binary := opts.Binary
	if binary == "" {
		if path, err := exec.LookPath("tmux"); err == nil {
			binary = path
		} else {
			binary = "tmux"
		}
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	shellPath := opts.Shell
	if shellPath == "" {
		shellPath = getenv("SHELL")
	}
	if shellPath == "" {
		shellPath = "/bin/sh"
	}
	chunkSize := opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultChunkBytes
	}
	enterDelay := opts.EnterDelay
	if enterDelay <= 0 {
		enterDelay = defaultEnterDelay
	}
	reapGrace := opts.ReapGrace
	if reapGrace <= 0 {
		reapGrace = defaultReapGrace
	}
	socket := opts.Socket
	if socket == "" {
		socket = defaultSocket
	}
	return &Runtime{
		binary:       binary,
		shell:        shellPath,
		socket:       socket,
		timeout:      timeout,
		chunkSize:    chunkSize,
		scratchDir:   opts.ScratchDir,
		enterDelay:   enterDelay,
		reapGrace:    reapGrace,
		runner:       execRunner{},
		reapSessions: killSessionsByPID,
	}
}

// Create starts a new tmux session in the workspace, running the agent's
// launch command with a keep-alive shell, and returns a handle to it.
func (r *Runtime) Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	id, err := tmuxSessionName(cfg.SessionID)
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	if cfg.WorkspacePath == "" {
		return ports.RuntimeHandle{}, errors.New("tmux runtime: workspace path is required")
	}
	if len(cfg.Argv) == 0 {
		return ports.RuntimeHandle{}, errors.New("tmux runtime: launch command is required")
	}
	if err := validateEnvKeys(cfg.Env); err != nil {
		return ports.RuntimeHandle{}, err
	}

	launchCmd := buildLaunchCommand(cfg)
	// Checkpoint 8P-E.13C: an agent whose prompt is delivered in its launch
	// argv (claude-code, codex, and every other adapter defaulting to
	// PromptDeliveryInCommand) puts the whole prompt inside this one tmux
	// command — which tmux carries in a single 16 KB imsg frame. A large work
	// or review prompt would therefore fail the session creation itself, the
	// same "command too long" the fix path hit. Past the inline budget the
	// command body moves into a private script the shell sources, so what tmux
	// receives is a path.
	removeScript := func() {}
	if len(launchCmd) > maxInlineLaunchCommandBytes {
		path, cleanup, err := r.writeLaunchScript(id, launchCmd)
		if err != nil {
			return ports.RuntimeHandle{}, fmt.Errorf("tmux runtime: stage launch command %s: %w", id, err)
		}
		removeScript = cleanup
		launchCmd = ". " + shellQuote(path)
	}
	args := newSessionArgs(id, cfg.WorkspacePath, r.shell, launchCmd)
	if _, err := r.run(ctx, args...); err != nil {
		removeScript()
		return ports.RuntimeHandle{}, fmt.Errorf("tmux runtime: create session %s: %w", id, err)
	}
	// From here the session exists, and AO must not unlink the staged script
	// behind the pane's back: the script removes ITSELF as its first statement
	// (see writeLaunchScript), which is the only removal that is ordered AFTER
	// the shell has opened it.
	//
	// The removal used to happen here instead, gated on verifyPaneWorkingDirectory
	// succeeding, on the reasoning that a pane reporting the workspace as its cwd
	// must already have executed the script's leading `cd` and therefore already
	// hold the file open. That reasoning is wrong: `#{pane_current_path}` is the
	// pane process's cwd, and tmux chdirs the pane to `new-session -c <cwd>`
	// BEFORE exec'ing the shell — the same path this check compares against. So
	// the probe answered "correct" from the instant the pane existed, proving
	// nothing about the script, and the unlink raced the shell's open(). When it
	// won that race, `. <path>` failed, `$SHELL -c` exited, tmux tore the session
	// down, and the very next command — set-option status off — returned
	// "no such session", failing the whole spawn (incident wf-57f90ff2,
	// agent-orchestrator-29). Every launch command over
	// maxInlineLaunchCommandBytes was exposed to it, i.e. exactly the large
	// worker/reviewer prompts.
	//
	// failCreate tears the session down and removes the staged script on every
	// post-create failure path, where the shell demonstrably never got to run it
	// (or is being killed anyway).
	failCreate := func(err error) (ports.RuntimeHandle, error) {
		_ = r.Destroy(context.Background(), ports.RuntimeHandle{ID: id})
		removeScript()
		return ports.RuntimeHandle{}, err
	}
	if err := r.verifyPaneWorkingDirectory(ctx, id, cfg.WorkspacePath); err != nil {
		return failCreate(err)
	}

	// Hide the status bar in the embedded terminal: it clutters the view and
	// was not designed for the in-browser display context.
	if _, err := r.run(ctx, setStatusOffArgs(id)...); err != nil {
		return failCreate(fmt.Errorf("tmux runtime: set status %s: %w", id, err))
	}

	// Enable mouse mode so the embedded terminal's SGR wheel reports scroll the
	// pane (see setMouseOnArgs). Without it, wheel scrolling silently no-ops.
	if _, err := r.run(ctx, setMouseOnArgs(id)...); err != nil {
		return failCreate(fmt.Errorf("tmux runtime: set mouse %s: %w", id, err))
	}

	// Size the shared window to the largest attached client, not the most recent
	// one, so a small secondary viewer (e.g. the phone) can't strip down a larger
	// client's view (see setWindowSizeLargestArgs).
	if _, err := r.run(ctx, setWindowSizeLargestArgs(id)...); err != nil {
		return failCreate(fmt.Errorf("tmux runtime: set window-size %s: %w", id, err))
	}

	handle := ports.RuntimeHandle{ID: id}
	alive, err := r.IsAlive(ctx, handle)
	if err != nil {
		return failCreate(fmt.Errorf("tmux runtime: verify session %s: %w", id, err))
	}
	if !alive {
		return failCreate(fmt.Errorf("tmux runtime: session %s exited before ready", id))
	}
	return handle, nil
}

// ContaminatedEnvVars best-effort inspects this Runtime's tmux server's
// global environment (the environment captured from whoever's `tmux -L
// <socket>` call first started it — see sanitizeInheritedEnv) for any of
// nestedAgentEnvVars, and returns the names found. It never modifies the
// server: a server that was already started before a sanitized AO build ran
// (or by an unpatched AO instance) keeps its contaminated environment for
// its whole life — only a full `tmux -L <socket> kill-server` clears it, and
// this Runtime does not do that on its own. Callers (e.g. daemon startup
// diagnostics) can use this to log a one-time warning suggesting a manual
// kill-server rather than silently running degraded. An empty result also
// covers the common "no server for this socket yet" case: tmux's own
// show-environment error in that case is treated as "nothing found", not a
// caller-visible failure.
func (r *Runtime) ContaminatedEnvVars(ctx context.Context) []string {
	out, err := r.run(ctx, showGlobalEnvironmentArgs()...)
	if err != nil {
		return nil
	}
	return contaminatedFromShowEnvironment(string(out), nestedAgentEnvVars)
}

// contaminatedFromShowEnvironment parses `tmux show-environment -g` output
// (one "NAME=value" or "-NAME" — tmux's marker for an unset-but-tracked var —
// per line) and returns which of names are present and set to a non-empty
// value.
func contaminatedFromShowEnvironment(output string, names []string) []string {
	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		want[name] = struct{}{}
	}
	var found []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		name := line
		if idx := strings.IndexByte(line, '='); idx >= 0 {
			name = line[:idx]
		}
		if _, ok := want[name]; ok {
			found = append(found, name)
		}
	}
	return found
}

// Restart replaces the command in an existing pane while preserving the tmux
// session. This is used to resume an exited agent without discarding terminal
// history or forcing attached clients onto a new handle.
func (r *Runtime) Restart(ctx context.Context, handle ports.RuntimeHandle, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	id, err := handleID(handle)
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	expectedID, err := tmuxSessionName(cfg.SessionID)
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	if expectedID != id {
		return ports.RuntimeHandle{}, fmt.Errorf("tmux runtime: restart handle %s does not match session %s", id, cfg.SessionID)
	}
	if cfg.WorkspacePath == "" {
		return ports.RuntimeHandle{}, errors.New("tmux runtime: workspace path is required")
	}
	if len(cfg.Argv) == 0 {
		return ports.RuntimeHandle{}, errors.New("tmux runtime: launch command is required")
	}
	if err := validateEnvKeys(cfg.Env); err != nil {
		return ports.RuntimeHandle{}, err
	}

	launchCmd := buildLaunchCommand(cfg)
	if _, err := r.run(ctx, respawnPaneArgs(id, cfg.WorkspacePath, r.shell, launchCmd)...); err != nil {
		return ports.RuntimeHandle{}, fmt.Errorf("tmux runtime: restart session %s: %w", id, err)
	}
	alive, err := r.IsAlive(ctx, handle)
	if err != nil {
		return ports.RuntimeHandle{}, fmt.Errorf("tmux runtime: verify restarted session %s: %w", id, err)
	}
	if !alive {
		return ports.RuntimeHandle{}, fmt.Errorf("tmux runtime: session %s exited during restart", id)
	}
	return handle, nil
}

// paneCwdVerifyAttempts and paneCwdVerifyRetryDelay bound how long Create
// waits for the pane's working directory to settle before giving up.
// buildLaunchCommand's `cd '<workspace>' || exit;` guard corrects a pane that
// started in the tmux server's own (possibly poisoned) cwd, but only once the
// pane's shell actually runs that cd. Measured live on 2026-07-25:
// #{pane_current_path} sampled immediately after `new-session` was stale, and
// the same probe sampled 50ms later was already correct. A single-shot check
// therefore lost that race every time and turned a spawn that was actually
// going to succeed into a hard failure (issue #2775): retrying gives the cd
// guard the moment it needs to run.
const (
	paneCwdVerifyAttempts   = 5
	paneCwdVerifyRetryDelay = 50 * time.Millisecond
)

func (r *Runtime) verifyPaneWorkingDirectory(ctx context.Context, id, want string) error {
	var lastErr error
	for attempt := 0; attempt < paneCwdVerifyAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(paneCwdVerifyRetryDelay):
			}
		}
		out, err := r.run(ctx, paneCurrentPathArgs(id)...)
		if err != nil {
			// A later transient probe failure (e.g. a one-off tmux CLI hiccup)
			// must not overwrite an already-observed cwd mismatch: the mismatch
			// is the classifiable, actionable error toAPIError maps via
			// ports.ErrRuntimeWorkspaceCwdMismatch (Fix 4), and losing it here
			// would silently regress that mapping back to a bare, unclassifiable
			// 500 whenever the very last attempt happened to hit a probe error.
			if !errors.Is(lastErr, ports.ErrRuntimeWorkspaceCwdMismatch) {
				lastErr = fmt.Errorf("tmux runtime: verify working directory %s: %w", id, err)
			}
			continue
		}
		got := strings.TrimSpace(string(out))
		if sameDirectory(got, want) {
			return nil
		}
		lastErr = fmt.Errorf(
			"%w: session %s started in %q, want %q (the worktree may be missing, or the tmux server may be pinned to a stale directory)",
			ports.ErrRuntimeWorkspaceCwdMismatch, id, got, want,
		)
	}
	return lastErr
}

// Destroy kills the handle's tmux session and reaps the pane processes it
// leaves behind. `tmux kill-session` only SIGHUPs each pane's foreground
// process, so a worker's backgrounded children (e.g. a dev server started with
// `&`, later reparented to init) survive it and hold their ports indefinitely
// (issue #2523). To catch those, Destroy records each pane's session id before
// teardown and, after kill-session, signals the whole session (see
// killSessionsByPID). An already-gone session is treated as success (idempotent).
func (r *Runtime) Destroy(ctx context.Context, handle ports.RuntimeHandle) error {
	id, err := handleID(handle)
	if err != nil {
		return err
	}
	// Capture pane session ids while the session still exists; a missing
	// session lists no panes and reaps nothing. Best-effort: failures here must
	// not block the kill-session below.
	sessionIDs := r.paneSessionIDs(ctx, id)

	out, err := r.run(ctx, killSessionArgs(id)...)
	// Reap regardless of the kill-session result: orphaned children outlive the
	// session, so they must be cleaned up even when the session was already
	// gone (a benign double-kill).
	r.reapSessions(ctx, sessionIDs, r.reapGrace)

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && killSessionMissingOutput(string(out)) {
			return nil
		}
		return fmt.Errorf("tmux runtime: destroy session %s: %w", id, err)
	}
	return nil
}

// paneSessionIDs lists the pid of every pane in the session. tmux launches each
// pane in its own session (setsid), so a pane's pid is also its session id —
// the handle killSessionsByPID uses to reap the pane's descendants. Best-effort:
// any error (including a missing session) or unparseable line yields no ids,
// and pids <= 1 are skipped so we never signal init or the "current session".
func (r *Runtime) paneSessionIDs(ctx context.Context, id string) []int {
	out, err := r.run(ctx, listPanePIDsArgs(id)...)
	if err != nil {
		return nil
	}
	var ids []int
	for _, line := range strings.Split(string(out), "\n") {
		pid, convErr := strconv.Atoi(strings.TrimSpace(line))
		if convErr != nil || pid <= 1 {
			continue
		}
		ids = append(ids, pid)
	}
	return ids
}

// IsAlive reports whether the handle's session still exists via `tmux
// has-session`. Exit 0 means alive. A non-zero exit with output naming this
// session as missing is a definitive false, nil. A server-level failure ("no
// server running", "error connecting") wraps ports.ErrRuntimeUnavailable: the
// probe learned nothing about this session — the agent process may well still
// be running as an orphan of the dead server — so it must never be read as
// per-session death (issue #3475). Any other non-zero exit is a plain probe
// error so callers (the reaper feeding the LCM) treat it as a failed probe
// and never kill a session on a transient error.
func (r *Runtime) IsAlive(ctx context.Context, handle ports.RuntimeHandle) (bool, error) {
	id, err := handleID(handle)
	if err != nil {
		return false, err
	}
	out, err := r.run(ctx, hasSessionArgs(id)...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if sessionMissingOutput(string(out)) {
				return false, nil
			}
			if serverUnreachableOutput(string(out)) {
				return false, fmt.Errorf("tmux runtime: probe session %s: %w: %s",
					id, ports.ErrRuntimeUnavailable, strings.TrimSpace(string(out)))
			}
		}
		return false, fmt.Errorf("tmux runtime: probe session %s: %w", id, err)
	}
	return true, nil
}

// IsSupervisedProcessAlive reports whether the managed workload for ref is
// still a descendant of this tmux pane. The initial launch is identified by
// its exact AO supervisor. After that supervisor exits and leaves the
// interactive shell behind, a child launched from that shell is treated as a
// manually resumed workload. Command failures remain inconclusive.
func (r *Runtime) IsSupervisedProcessAlive(ctx context.Context, handle ports.RuntimeHandle, ref ports.SupervisedProcessRef) (bool, error) {
	entries, panePID, err := r.supervisedProcessTree(ctx, handle)
	if err != nil {
		return false, err
	}
	return containsManagedWorkload(entries, panePID, string(ref.SessionID), ref.LaunchID), nil
}

// IsExactSupervisedProcessAlive reports only the AO supervisor matching ref
// while that supervisor still owns a live managed child. It deliberately
// excludes both the manual-child fallback used by the ordinary reaper probe
// and a supervisor that is merely waiting to durably report its child's exit:
// neither is proof that an agent can safely receive a continuation.
func (r *Runtime) IsExactSupervisedProcessAlive(ctx context.Context, handle ports.RuntimeHandle, ref ports.SupervisedProcessRef) (bool, error) {
	if ref.SessionID == "" || strings.TrimSpace(ref.LaunchID) == "" {
		return false, errors.New("tmux runtime: exact supervisor session and launch are required")
	}
	entries, panePID, err := r.supervisedProcessTree(ctx, handle)
	if err != nil {
		return false, err
	}
	return containsExactSupervisedWorkload(entries, panePID, string(ref.SessionID), ref.LaunchID), nil
}

func (r *Runtime) supervisedProcessTree(ctx context.Context, handle ports.RuntimeHandle) ([]processEntry, int, error) {
	id, err := handleID(handle)
	if err != nil {
		return nil, 0, err
	}
	paneOut, err := r.run(ctx, panePIDArgs(id)...)
	if err != nil {
		return nil, 0, fmt.Errorf("tmux runtime: inspect pane pid %s: %w", id, err)
	}
	panePID, err := strconv.Atoi(strings.TrimSpace(string(paneOut)))
	if err != nil || panePID <= 0 {
		return nil, 0, fmt.Errorf("tmux runtime: invalid pane pid %q", strings.TrimSpace(string(paneOut)))
	}
	processOut, err := r.runCommand(ctx, "ps", "-ww", "-axo", "pid=,ppid=,args=")
	if err != nil {
		return nil, 0, fmt.Errorf("tmux runtime: inspect process tree %s: %w", id, err)
	}
	entries, err := parseProcessTable(string(processOut))
	if err != nil {
		return nil, 0, fmt.Errorf("tmux runtime: parse process tree %s: %w", id, err)
	}
	return entries, panePID, nil
}

// SendMessage sends literal text to the session (chunked via send-keys -l) then
// presses Enter to submit. An empty message presses Enter alone (the nudge
// contract on ports.AgentMessenger).
//
// ponytail: send-keys -l chunked is simpler than load-buffer/paste-buffer; the
// ceiling is very large messages may be slower, but chunk size defaults to 16 KB
// which is ample for agent prompts.
func (r *Runtime) SendMessage(ctx context.Context, handle ports.RuntimeHandle, message string) error {
	id, err := handleID(handle)
	if err != nil {
		return err
	}
	enterCtx := ctx
	if message != "" {
		// Checkpoint 8P-E.13C: above the inline budget the payload does not go
		// into the command at all. tmux carries each command in a single imsg
		// frame capped at 16 KB, so a large prompt typed inline is rejected
		// with "command too long" — the exact failure that stranded the fix
		// step of wf-2261767d. Above that budget the bytes travel through a
		// private file and a paste buffer instead, which also keeps them out of
		// argv and away from every quoting layer.
		if ports.PromptTransportFor(len(message)) == ports.PromptTransportBufferFile {
			budget := sendCompletionBudget(2, r.timeout, r.enterDelay)
			pasteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
			defer cancel()
			if err := r.sendViaPasteBuffer(ctx, pasteCtx, id, message); err != nil {
				return err
			}
			enterCtx = pasteCtx
			return r.finishSend(enterCtx, id)
		}
		messageChunks := chunks(message, r.chunkSize)
		sendCtx := ctx
		var finishCancel context.CancelFunc
		for i, chunk := range messageChunks {
			if _, err := r.run(sendCtx, sendKeysLiteralArgs(id, chunk)...); err != nil {
				if finishCancel != nil {
					finishCancel()
				}
				if i == 0 {
					// Nothing reached the pane: tmux refused the very first
					// chunk, so a caller may re-send the whole message without
					// risking a duplicate or a spliced instruction.
					return fmt.Errorf("tmux runtime: send message %s: %w: %w", id, ports.ErrPromptUndelivered, err)
				}
				return fmt.Errorf("tmux runtime: send message %s: %w", id, err)
			}
			if i == 0 {
				completionBudget := sendCompletionBudget(len(messageChunks), r.timeout, r.enterDelay)
				enterCtx, finishCancel = context.WithTimeout(context.WithoutCancel(ctx), completionBudget)
				sendCtx = enterCtx
			}
		}
		if finishCancel != nil {
			defer finishCancel()
		}
		// Give the target TUI a moment to accept the pasted text before the
		// trailing Enter, mirroring conpty's ptyInputEnterDelay. Without it a
		// large multiline paste can absorb the Enter and leave the prompt
		// unsubmitted (issue #2342). Empty-message nudges skip this — there is
		// no paste ahead of a catch-up Enter.
		//
		// From here on the chunks are already in the pane, so the pause and
		// the Enter are detached from the caller's cancellation (bounded by
		// their own timeout instead): abandoning mid-pause would strand an
		// unsubmitted draft that a retried send would then double-paste.
		// Errors reported by tmux after it accepts a chunk still return to the
		// caller; they are not retried because AO cannot safely distinguish
		// whether tmux applied the failed command.
		if r.enterDelay > 0 {
			select {
			case <-enterCtx.Done():
				return enterCtx.Err()
			case <-time.After(r.enterDelay):
			}
		}
	}
	if _, err := r.run(enterCtx, sendEnterArgs(id)...); err != nil {
		return fmt.Errorf("tmux runtime: send enter %s: %w", id, err)
	}
	return nil
}

func sendCompletionBudget(chunkCount int, commandTimeout, enterDelay time.Duration) time.Duration {
	return time.Duration(chunkCount)*commandTimeout + enterDelay
}

// finishSend performs the paste-settling pause and the submitting Enter shared
// by both transports. Like the inline path, it is deliberately detached from
// the caller's cancellation: the payload is already in the pane, and giving up
// here would strand an unsubmitted draft that a retry would then double-paste.
func (r *Runtime) finishSend(enterCtx context.Context, id string) error {
	if r.enterDelay > 0 {
		select {
		case <-enterCtx.Done():
			return enterCtx.Err()
		case <-time.After(r.enterDelay):
		}
	}
	if _, err := r.run(enterCtx, sendEnterArgs(id)...); err != nil {
		return fmt.Errorf("tmux runtime: send enter %s: %w", id, err)
	}
	return nil
}

// sendViaPasteBuffer delivers message through a file and a named tmux paste
// buffer instead of through the command itself (Checkpoint 8P-E.13C).
//
// Three properties matter, and each is why this is not just "a bigger chunk":
//
//   - Size: the command AO issues names a path, so the prompt's size is bounded
//     by the filesystem rather than by tmux's 16 KB command frame.
//   - Fidelity: the bytes go file -> tmux buffer -> pane. They are never a
//     shell word, never escaped, never quoted, so newlines, quotes, backticks,
//     $(...) and non-ASCII text arrive exactly as written.
//   - Exposure: the payload never appears in argv, so it is not visible in
//     `ps` output or a shell history, and the file itself is created 0600 and
//     removed as soon as tmux has read it.
//
// loadCtx failures happen before any byte reaches the pane and are therefore
// wrapped with ports.ErrPromptUndelivered so a caller may safely re-send.
func (r *Runtime) sendViaPasteBuffer(loadCtx, pasteCtx context.Context, id, message string) error {
	path, cleanup, err := r.writePromptFile(id, message)
	if err != nil {
		return fmt.Errorf("tmux runtime: stage message %s: %w: %w", id, ports.ErrPromptUndelivered, err)
	}
	defer cleanup()

	buffer := promptBufferName(id)
	if _, err := r.run(loadCtx, loadBufferArgs(buffer, path)...); err != nil {
		return fmt.Errorf("tmux runtime: load message buffer %s: %w: %w", id, ports.ErrPromptUndelivered, err)
	}
	if _, err := r.run(pasteCtx, pasteBufferArgs(buffer, id)...); err != nil {
		// The buffer exists but its contents may or may not have reached the
		// pane. Drop the buffer so it cannot be pasted later by anything else,
		// and report an ordinary (non-retryable) delivery failure.
		_, _ = r.run(context.WithoutCancel(pasteCtx), deleteBufferArgs(buffer)...)
		return fmt.Errorf("tmux runtime: paste message %s: %w", id, err)
	}
	return nil
}

// writePromptFile stages the exact prompt bytes in a private file. The returned
// cleanup removes it; callers defer it unconditionally, so no prompt outlives
// its delivery even when tmux fails midway.
func (r *Runtime) writePromptFile(id, message string) (string, func(), error) {
	dir, err := r.scratchDirFor("prompts")
	if err != nil {
		return "", func() {}, err
	}
	f, err := os.CreateTemp(dir, "ao-prompt-"+id+"-*.txt")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	// os.CreateTemp already creates 0600; state it anyway so a restrictive
	// umask is not the only thing standing between a prompt and other users.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if _, err := f.WriteString(message); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// scratchDirFor returns (creating if needed) the directory AO stages transient
// runtime payloads in. It defaults to the OS temp dir; the daemon passes an
// AO-owned directory so these files live under the data dir with everything
// else AO writes.
func (r *Runtime) scratchDirFor(kind string) (string, error) {
	base := r.scratchDir
	if base == "" {
		return os.TempDir(), nil
	}
	dir := filepath.Join(base, kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// writeLaunchScript stages an oversized launch command as a shell script the
// pane's shell sources. Same staging rules as writePromptFile: AO-owned
// directory, 0600, and gone from disk as soon as it has been read — but the
// removal is the script's OWN first statement rather than something AO does
// from the outside.
//
// That is load-bearing, not a style choice. AO has no signal that tells it the
// pane's shell has opened this file (Create's cwd probe reads the pane's start
// directory, which tmux sets before exec'ing the shell — see Create), so any
// unlink AO issues races the shell's open() and, when it wins, kills the
// session it was supposed to be launching. Placing `rm` inside the script makes
// the removal causally ordered after the open by construction: the shell cannot
// execute the line without having read the file. POSIX keeps an unlinked file's
// contents readable through the descriptor that is still open, so the rest of
// the script — including the entire prompt — is unaffected.
//
// `/bin/rm` is the fallback because the script's own `export PATH` has not run
// yet at that point; either form removing the file is enough, and a failure to
// remove leaks a 0600 file rather than breaking the launch.
func (r *Runtime) writeLaunchScript(id, launchCmd string) (string, func(), error) {
	dir, err := r.scratchDirFor("launch")
	if err != nil {
		return "", func() {}, err
	}
	f, err := os.CreateTemp(dir, "ao-launch-"+id+"-*.sh")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	quoted := shellQuote(path)
	selfRemove := "rm -f -- " + quoted + " 2>/dev/null || /bin/rm -f -- " + quoted + " 2>/dev/null\n"
	if _, err := f.WriteString(selfRemove + launchCmd + "\n"); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// promptBufferName names the tmux paste buffer a session's prompt is loaded
// into. Per-session rather than global so two concurrent sends can never paste
// each other's payload, and always overwritten rather than appended.
func promptBufferName(id string) string {
	return "ao-prompt-" + id
}

// Interrupt sends Ctrl-C to the foreground process without destroying the tmux
// session, keeping the terminal available for inspection and reuse.
func (r *Runtime) Interrupt(ctx context.Context, handle ports.RuntimeHandle) error {
	id, err := handleID(handle)
	if err != nil {
		return err
	}
	if _, err := r.run(ctx, sendInterruptArgs(id)...); err != nil {
		return fmt.Errorf("tmux runtime: interrupt session %s: %w", id, err)
	}
	return nil
}

// SendInput sends raw terminal input without appending Enter. It is intended
// for TUI keybindings such as Escape rather than prompt text.
func (r *Runtime) SendInput(ctx context.Context, handle ports.RuntimeHandle, input string) error {
	id, err := handleID(handle)
	if err != nil {
		return err
	}
	args := sendKeysLiteralArgs(id, input)
	if _, err := r.run(ctx, args...); err != nil {
		return fmt.Errorf("tmux runtime: send input %s: %w", id, err)
	}
	return nil
}

// GetOutput returns the last `lines` lines of the session pane's captured
// output.
func (r *Runtime) GetOutput(ctx context.Context, handle ports.RuntimeHandle, lines int) (string, error) {
	id, err := handleID(handle)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		return "", errors.New("tmux runtime: lines must be positive")
	}
	out, err := r.run(ctx, capturePaneArgs(id, lines)...)
	if err != nil {
		return "", fmt.Errorf("tmux runtime: capture output %s: %w", id, err)
	}
	return tailLines(trimTrailingBlankLines(string(out)), lines), nil
}

// GetStyledOutput is GetOutput with tmux's -e flag so SGR styling is retained.
func (r *Runtime) GetStyledOutput(ctx context.Context, handle ports.RuntimeHandle, lines int) (string, error) {
	id, err := handleID(handle)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		return "", errors.New("tmux runtime: lines must be positive")
	}
	out, err := r.run(ctx, capturePaneStyledArgs(id, lines)...)
	if err != nil {
		return "", fmt.Errorf("tmux runtime: capture styled output %s: %w", id, err)
	}
	return tailLines(trimTrailingBlankLines(string(out)), lines), nil
}

// Attach opens a fresh attach Stream by spawning `tmux attach-session` on a
// local PTY, sized rows x cols from birth when known. ctx cancellation closes
// the PTY.
func (r *Runtime) Attach(ctx context.Context, handle ports.RuntimeHandle, rows, cols uint16) (ports.Stream, error) {
	argv, err := r.attachCommand(handle)
	if err != nil {
		return nil, err
	}
	return ptyexec.Spawn(ctx, argv, attachEnv(os.Environ()), rows, cols)
}

// attachCommand returns the argv to attach a terminal to the session.
// tmux needs no per-session env block.
//
// -u forces tmux's client-side CLIENT_UTF8 flag on. Without it, tmux infers
// UTF-8 capability from LC_ALL/LC_CTYPE/LANG in the attaching process's env
// (see tmux's main()); AO's daemon is typically started without an
// interactive shell's locale, so that inference silently fails. A non-UTF8
// client makes tmux's tty_check_codeset (tty.c) replace any character it
// can't map through the legacy ACS table with underscores matching the
// glyph's display width. Box-drawing glyphs are in that ACS table so they
// still looked fine; agent CLI status icons outside it (e.g. Claude Code's
// spinner "✻" U+273B, its "⎿" U+23BF continuation marker) were silently
// rewritten to "_", which is the underscore corruption reported in #2484.
// Confirmed byte-for-byte: attaching with a stripped, locale-less env
// reproduces "_ _ _" for those glyphs; adding -u fixes it, with no observable
// difference for the still-correct box-drawing case. AO already treats the
// PTY byte stream as UTF-8 end to end, so forcing the flag is always
// correct here regardless of the daemon's own environment.
func (r *Runtime) attachCommand(handle ports.RuntimeHandle) ([]string, error) {
	id, err := handleID(handle)
	if err != nil {
		return nil, err
	}
	// The embedded xterm renderer supports 24-bit SGR colors. Tell this tmux
	// client explicitly so tmux forwards RGB instead of quantizing it to the
	// xterm-256color palette. -T is available in AO's minimum tmux version (3.2).
	//
	// -L pins the attach to AO's own server (see Options.Socket / r.socket) so
	// attaching never falls through to the caller's default tmux server.
	return []string{r.binary, "-L", r.socket, "-u", "-T", "RGB", "attach-session", "-t", id}, nil
}

func attachEnv(base []string) []string {
	env := append([]string(nil), base...)
	hasTerm := false
	hasColorTerm := false
	for i, kv := range env {
		switch {
		case strings.HasPrefix(kv, "TERM="):
			env[i] = "TERM=xterm-256color"
			hasTerm = true
		case strings.HasPrefix(kv, "COLORTERM="):
			env[i] = "COLORTERM=truecolor"
			hasColorTerm = true
		}
	}
	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
	}
	if !hasColorTerm {
		env = append(env, "COLORTERM=truecolor")
	}
	return env
}

// run wraps runner.Run with a per-call timeout context. Every tmux
// subcommand goes through here, so prepending `-L <socket>` here is what
// scopes all of new-session/has-session/list-panes/send-keys/kill-session/
// capture-pane/etc. to AO's own isolated tmux server instead of the
// caller's default one (see Options.Socket).
func (r *Runtime) run(ctx context.Context, args ...string) ([]byte, error) {
	serverArgs := make([]string, 0, len(args)+2)
	serverArgs = append(serverArgs, "-L", r.socket)
	serverArgs = append(serverArgs, args...)
	return r.runCommand(ctx, r.binary, serverArgs...)
}

func (r *Runtime) runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	out, err := r.runner.Run(cmdCtx, nil, name, args...)
	if cmdCtx.Err() != nil {
		return out, cmdCtx.Err()
	}
	if err != nil {
		return out, commandError{err: err, output: strings.TrimSpace(string(out))}
	}
	return out, nil
}

type processEntry struct {
	pid     int
	ppid    int
	command string
}

func parseProcessTable(out string) ([]processEntry, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	entries := make([]processEntry, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("invalid pid in %q", line)
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("invalid parent pid in %q", line)
		}
		entries = append(entries, processEntry{pid: pid, ppid: ppid, command: strings.Join(fields[2:], " ")})
	}
	return entries, nil
}

func descendantPIDs(entries []processEntry, rootPID int) map[int]bool {
	descendants := map[int]bool{rootPID: true}
	for changed := true; changed; {
		changed = false
		for _, entry := range entries {
			if descendants[entry.pid] || !descendants[entry.ppid] {
				continue
			}
			descendants[entry.pid] = true
			changed = true
		}
	}
	return descendants
}

func containsManagedWorkload(entries []processEntry, rootPID int, sessionID, launchID string) bool {
	descendants := descendantPIDs(entries, rootPID)
	hasChild := false
	hasSupervisor := false
	for _, entry := range entries {
		if entry.pid == rootPID || !descendants[entry.pid] {
			continue
		}
		hasChild = true
		if !isAnySupervisorCommand(entry.command) {
			continue
		}
		hasSupervisor = true
		if isSupervisorCommand(entry.command, sessionID, launchID) {
			return true
		}
	}

	// A supervisor in the pane tree must match the current generation. Once no
	// supervisor remains, the pane root is the preserved interactive shell and
	// any child is a workload the operator launched from that shell.
	return hasChild && !hasSupervisor
}

func containsExactSupervisedWorkload(entries []processEntry, rootPID int, sessionID, launchID string) bool {
	descendants := descendantPIDs(entries, rootPID)
	supervisorPID := 0
	for _, entry := range entries {
		if entry.pid != rootPID && descendants[entry.pid] && isSupervisorCommand(entry.command, sessionID, launchID) {
			supervisorPID = entry.pid
			break
		}
	}
	if supervisorPID == 0 {
		return false
	}
	workloadDescendants := descendantPIDs(entries, supervisorPID)
	for _, entry := range entries {
		if entry.pid != supervisorPID && workloadDescendants[entry.pid] {
			return true
		}
	}
	return false
}

func isAnySupervisorCommand(command string) bool {
	fields := strings.Fields(command)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "agent-process" && fields[i+1] == "supervise" {
			return true
		}
	}
	return false
}

func isSupervisorCommand(command, sessionID, launchID string) bool {
	fields := strings.Fields(command)
	for i := 0; i+6 < len(fields); i++ {
		if fields[i] == "agent-process" && fields[i+1] == "supervise" &&
			fields[i+2] == "--session" && fields[i+3] == sessionID &&
			fields[i+4] == "--launch" && fields[i+5] == launchID && fields[i+6] == "--" {
			return true
		}
	}
	return false
}

// -- session name helpers --

func tmuxSessionName(id domain.SessionID) (string, error) {
	raw := string(id)
	if raw == "" {
		return "", errors.New("tmux runtime: session id is required")
	}
	return SessionName(raw), nil
}

// SessionName returns the tmux session name the runtime registers for a given
// session id, applying the same sanitisation Create does. Callers that print an
// attach hint must use this rather than the raw id.
func SessionName(id string) string {
	if sessionIDPattern.MatchString(id) && len(id) <= 48 {
		return id
	}
	return sanitizedSessionName(id)
}

func sanitizedSessionName(raw string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "session"
	}
	if len(base) > 32 {
		base = strings.TrimRight(base[:32], "-")
	}
	sum := sha256.Sum256([]byte(raw))
	return base + "-" + hex.EncodeToString(sum[:4])
}

func handleID(handle ports.RuntimeHandle) (string, error) {
	id := handle.ID
	if id == "" {
		return "", errors.New("tmux runtime: session id is required")
	}
	if !sessionIDPattern.MatchString(id) {
		return "", fmt.Errorf("tmux runtime: invalid handle id %q", id)
	}
	return id, nil
}

// -- output detection helpers --

// sessionMissingOutput reports whether a non-zero `tmux has-session` exit is
// definitively "this session does not exist" — evidence about the probed
// session itself. Server-level failures deliberately do not match: "no server
// running" describes the whole server and "error connecting" is a transient
// socket failure; neither says anything about one session, so treating them as
// per-session death let a single server outage archive every session on the
// board (issue #3475).
func sessionMissingOutput(out string) bool {
	s := strings.ToLower(out)
	return strings.Contains(s, "can't find session") ||
		strings.Contains(s, "session not found")
}

// serverUnreachableOutput reports whether a non-zero tmux exit means the
// server itself could not be reached, which is inconclusive for any single
// session's liveness.
func serverUnreachableOutput(out string) bool {
	s := strings.ToLower(out)
	return strings.Contains(s, "no server running") ||
		strings.Contains(s, "error connecting")
}

// killSessionMissingOutput reports whether a non-zero `tmux kill-session`
// failed because the session was already gone. Teardown stays generous: a
// missing server also means there is nothing left to kill, so it shares the
// server-level patterns that liveness probing must not use.
func killSessionMissingOutput(out string) bool {
	return sessionMissingOutput(out) || serverUnreachableOutput(out)
}

// -- text helpers --

func chunks(s string, maxBytes int) []string {
	if s == "" {
		return []string{""}
	}
	if maxBytes <= 0 || len(s) <= maxBytes {
		return []string{s}
	}
	parts := []string{}
	for s != "" {
		if len(s) <= maxBytes {
			parts = append(parts, s)
			break
		}
		end := maxBytes
		for end > 0 && !utf8.ValidString(s[:end]) {
			end--
		}
		if end == 0 {
			_, size := utf8.DecodeRuneInString(s)
			end = size
		}
		parts = append(parts, s[:end])
		s = s[end:]
	}
	return parts
}

func tailLines(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "")
}

func trimTrailingBlankLines(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && strings.TrimRight(lines[len(lines)-1], "\r\n") == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "")
}

// -- env / quoting helpers --

func validateEnvKeys(env map[string]string) error {
	for key := range env {
		if !validEnvKey(key) {
			return fmt.Errorf("tmux runtime: invalid env key %q", key)
		}
	}
	return nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// buildLaunchCommand builds the shell command string passed to `sh -c`. It
// exports env vars, runs argv, then keeps the tmux session alive. Supervised
// launches park on a non-interpreting stdin sink after exit so bytes racing a
// process exit can never become shell commands; legacy/unsupervised launches
// retain the interactive-shell fallback used by manual recovery.
//
// PATH from cfg.Env is exported last, after all other keys, so an explicit
// override takes effect.
func buildLaunchCommand(cfg ports.RuntimeConfig) string {
	path := cfg.Env["PATH"]
	if path == "" {
		path = getenv("PATH")
	}

	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(cfg.WorkspacePath))
	b.WriteString(" || exit; ")
	if _, configured := cfg.Env["NO_COLOR"]; !configured {
		// The daemon may be launched from another agent or CI environment that
		// sets NO_COLOR for its own captured output. Do not leak that ambient
		// preference into an interactive terminal session. A project can still
		// opt out of color explicitly through its configured environment.
		b.WriteString("unset NO_COLOR; ")
	}
	for _, key := range sortedKeys(cfg.Env) {
		if key == "PATH" || key == "COLORTERM" {
			continue
		}
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(cfg.Env[key]))
		b.WriteString("; ")
	}
	// The AO web terminal and tmux attach client both support 24-bit SGR color.
	// Export this after caller env so agent color detection cannot accidentally
	// downgrade rich syntax/diff colors to ANSI-256.
	b.WriteString("export COLORTERM='truecolor'; ")
	if path != "" {
		b.WriteString("export PATH=")
		b.WriteString(shellQuote(path))
		b.WriteString("; ")
	}
	// Quote each argv word so spaces inside a word are preserved.
	parts := make([]string, len(cfg.Argv))
	for i, a := range cfg.Argv {
		parts[i] = shellQuote(a)
	}
	b.WriteString(strings.Join(parts, " "))
	if cfg.Env["AO_SUPERVISED_PROCESS"] == "1" {
		// cat consumes and discards any input that arrived while the supervised
		// child was exiting. Runtime Restart/Destroy replaces or kills the pane.
		b.WriteString(`; exec cat >/dev/null`)
	} else {
		// Keep the tmux session alive after an unsupervised agent exits so the
		// operator can inspect it and use the historical manual-recovery shell.
		b.WriteString(`; exec "${SHELL:-/bin/sh}" -i`)
	}
	return b.String()
}

func sameDirectory(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	if errA == nil {
		a = absA
	}
	absB, errB := filepath.Abs(b)
	if errB == nil {
		b = absB
	}
	if realA, err := filepath.EvalSymlinks(a); err == nil {
		a = realA
	}
	if realB, err := filepath.EvalSymlinks(b); err == nil {
		b = realB
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// -- error type --

type commandError struct {
	err    error
	output string
}

func (e commandError) Error() string {
	if e.output == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + e.output
}

func (e commandError) Unwrap() error { return e.err }
