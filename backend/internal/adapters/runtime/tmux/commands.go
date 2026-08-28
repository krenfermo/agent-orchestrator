package tmux

import (
	"fmt"
	"strings"
)

// newSessionArgs builds args for `tmux new-session -d -s <id> -x 220 -y 50
// -c <cwd> <shell> -c <launchCmd>`. The shell -c form runs the launch command
// inside the configured shell so exported env vars and quoting work correctly.
// newSessionArgs builds the creation command.
//
// When owner is set, `-e AO_SESSION_OWNER=<token>` makes the ownership token
// part of the very command that creates the session: tmux applies it while
// constructing the session, so the session cannot become visible to a probe
// before it carries its identity. Ownership is therefore a property of
// creation, not a follow-up write.
func newSessionArgs(id, cwd, shellPath, launchCmd, owner string) []string {
	args := []string{
		"new-session", "-d",
		// -P -F prints the NEW session's immutable instance id as part of the
		// creation command itself. Discovering it afterwards by name would defeat
		// the purpose: between creating and looking it up, the session could
		// vanish and a stranger take the name, and AO would adopt the stranger's
		// id as "the session I just made" — and later destroy it.
		"-P", "-F", "#{session_id}",
		"-s", id,
		"-x", "220",
		"-y", "50",
		"-c", cwd,
	}
	if owner != "" {
		args = append(args, "-e", ownerEnvKey+"="+owner)
	}
	return append(args, shellPath, "-c", launchCmd)
}

// respawnPaneArgs replaces the process in the session's only pane while keeping
// the tmux session and terminal handle intact.
func respawnPaneArgs(id, cwd, shellPath, launchCmd string) []string {
	return []string{
		"respawn-pane", "-k",
		"-t", id + ":0.0",
		"-c", cwd,
		shellPath, "-c", launchCmd,
	}
}

// showGlobalEnvironmentArgs lists the tmux server's global environment — the
// environment captured from whoever's `tmux -L <socket>` call first started
// the server, which every session/pane it later spawns inherits. Used only
// for best-effort drift detection (Runtime.ContaminatedEnvVars), never to
// mutate a running server.
func showGlobalEnvironmentArgs() []string {
	return []string{"show-environment", "-g"}
}

// setStatusOffArgs hides the tmux status bar for the given session.
// set-option uses pane-targeting syntax which does not accept the `=` prefix,
// so we pass the session name directly.
func setStatusOffArgs(id string) []string {
	return []string{"set-option", "-t", id, "status", "off"}
}

// setMouseOnArgs enables tmux mouse mode so the terminal's SGR mouse-wheel
// reports scroll the pane via copy-mode; without it, wheel scrolling no-ops.
// Pane-targeting, so no `=` prefix (see setStatusOffArgs).
func setMouseOnArgs(id string) []string {
	return []string{"set-option", "-t", id, "mouse", "on"}
}

// setWindowSizeLargestArgs makes tmux size the session's window to the LARGEST
// attached client rather than the most recently active one (the default is
// "latest"). A session can be viewed by several clients at once — e.g. the
// desktop app and the phone. Under "latest", a small phone attaching (or
// becoming active on a session switch) shrinks the shared window for the desktop
// too, giving the desktop a stripped-down view. "largest" ignores smaller
// viewers while a bigger one is attached, so a secondary client can never strip
// down the primary's view; when the big client detaches, tmux recomputes and the
// window follows the remaining largest client. Pane-targeting, so no `=` prefix
// (see setStatusOffArgs).
func setWindowSizeLargestArgs(id string) []string {
	return []string{"set-option", "-t", id, "window-size", "largest"}
}

// panePIDArgs returns the pid of tmux's direct pane process. AO walks its
// descendants to find the exact supervisor for the current launch.
func panePIDArgs(id string) []string {
	return []string{"display-message", "-p", "-t", id + ":0.0", "#{pane_pid}"}
}

// paneCurrentPathArgs prints tmux's cwd for the session's active pane. Create
// uses this after new-session so a poisoned tmux server that ignores -c fails
// loudly instead of silently starting the agent in the wrong directory.
func paneCurrentPathArgs(id string) []string {
	return []string{"display-message", "-p", "-t", id, "#{pane_current_path}"}
}

// killSessionArgs builds args for `tmux kill-session -t =<id>`. The `=` prefix
// requests exact-name matching so a session "foo" does not accidentally match
// "foobar" (tmux otherwise does unique-prefix matching).
func killSessionArgs(id string) []string {
	return []string{"kill-session", "-t", exactSessionTarget(id)}
}

// hasSessionArgs builds args for `tmux has-session -t =<id>`. The `=` prefix
// requests exact-name matching (see killSessionArgs).
func hasSessionArgs(id string) []string {
	return []string{"has-session", "-t", exactSessionTarget(id)}
}

// exactSessionTarget wraps id in tmux's exact-match prefix `=` so session-
// selection commands (-t) target only the session with that precise name.
// Session-selection commands like kill-session, has-session, and list-panes
// support this prefix; pane-targeting commands (send-keys, capture-pane,
// set-option) use a plain session name.
func exactSessionTarget(id string) string {
	if isSessionInstanceID(id) {
		// An instance id is ALREADY exact — it names one incarnation and cannot
		// be reassigned. Prefixing it with `=` would make tmux look for a
		// session literally named "$3".
		return id
	}
	return "=" + id
}

// isSessionInstanceID reports whether a target is tmux's immutable `$N` session
// id rather than a reusable name.
//
// The distinction is the whole ownership model: a NAME is a discovery key and
// nothing more, while an instance id is the authority key that every fact and
// every destructive action must be addressed to once discovery is done.
func isSessionInstanceID(target string) bool {
	return strings.HasPrefix(target, "$")
}

// listPanePIDsArgs builds args for `tmux list-panes -s -t =<id> -F #{pane_pid}`.
// -s lists every pane in the whole session (not just the active window); the
// exact-match target `=` avoids prefix collisions (see killSessionArgs). Each
// #{pane_pid} is the pane's session-leader pid, used to reap the pane's
// descendants when the session is destroyed.
func listPanePIDsArgs(id string) []string {
	return []string{"list-panes", "-s", "-t", exactSessionTarget(id), "-F", "#{pane_pid}"}
}

// sendKeysLiteralArgs builds args for `tmux send-keys -t <id> -l <chunk>`.
// The -l flag stops tmux interpreting words like "Enter" as key names so the
// text is sent verbatim.
func sendKeysLiteralArgs(id, chunk string) []string {
	return []string{"send-keys", "-t", id, "-l", chunk}
}

// loadBufferArgs builds args for `tmux load-buffer -b <buffer> <path>`, which
// reads the file's bytes into a named tmux paste buffer. The payload travels
// through the filesystem, so the command itself stays small regardless of how
// large the prompt is (Checkpoint 8P-E.13C).
func loadBufferArgs(buffer, path string) []string {
	return []string{"load-buffer", "-b", buffer, path}
}

// pasteBufferArgs builds args for
// `tmux paste-buffer -d -p -b <buffer> -t <id>`, which delivers the buffer's
// bytes to the pane as input. -d deletes the buffer once pasted, so a prompt
// never lingers in the tmux server's buffer stack.
//
// -p IS THE PROMPT. Without it, tmux replaces every LF in the buffer with a
// carriage return before writing it to the pane (its documented default; -r
// would suppress the replacement). A pane running a shell never notices,
// because the tty line discipline maps CR back to NL — but an agent TUI runs
// in raw mode, where a bare CR is not a line separator, it is ENTER. So a
// multi-line prompt pasted without -p is not delivered as one message at all:
// the agent is handed each line followed by a submit, and everything before
// the final fragment is entered and cleared one line at a time.
//
// That is incident wf-a816d7fe (2026-08-27), and it is worth stating exactly
// because every layer above it reported success. AO built a correct 4600-byte
// fix prompt (its recorded promptReceipt digest matches a byte-exact
// reconstruction), the transport exited 0, and the composer-empty probe
// returned `submitted` — the composer WAS empty, because all ~90 fragments had
// been submitted. What reached the agent was the last 510 bytes: the closing
// guardrails paragraph. The reviewer's findings sit in the middle of the
// prompt and never arrived at all. The worker replied "your message arrived
// truncated (it starts mid-word), and I read it as a restatement of the
// guardrails rather than a new task", changed nothing, and AO correctly but
// unhelpfully stopped the run on ambiguous_worker_state.
//
// -p wraps the payload in bracketed-paste control codes (ESC[200~ … ESC[201~)
// when the application has requested bracketed paste mode, which every agent
// TUI AO drives does. Inside those markers the CRs are literal pasted text
// rather than submits, so the whole prompt lands in the composer as one
// message and SendMessage's own trailing Enter submits it exactly once.
//
// Verified against tmux 3.7b by reading the bytes the pane actually receives:
//
//	paste-buffer -d      -> "AAA\rBBB\rCCC\r"
//	paste-buffer -d -p   -> "\x1b[200~AAA\rBBB\rCCC\r\x1b[201~"
//
// -r is deliberately NOT added alongside it: a real terminal paste carries CR
// for its line breaks too, so keeping the replacement is what makes this
// byte-identical to a human pasting the same text into the same pane.
func pasteBufferArgs(buffer, id string) []string {
	return []string{"paste-buffer", "-d", "-p", "-b", buffer, "-t", id}
}

// deleteBufferArgs builds args for `tmux delete-buffer -b <buffer>`, used to
// clean up a loaded buffer whose paste never happened.
func deleteBufferArgs(buffer string) []string {
	return []string{"delete-buffer", "-b", buffer}
}

// sendEnterArgs builds args for `tmux send-keys -t <id> Enter` to submit the
// queued input.
func sendEnterArgs(id string) []string {
	return []string{"send-keys", "-t", id, "Enter"}
}

// paneInModeArgs builds args for
// `tmux display-message -p -t =<id> #{pane_in_mode}`, which answers "is this
// pane currently in one of tmux's own modes (copy-mode, view-mode, a chooser)".
//
// It is load-bearing rather than diagnostic. While a pane is in a mode, tmux
// handles keys itself against that mode's key table, so `send-keys ... Enter`
// is consumed by the MODE and never reaches the application — while
// `paste-buffer` still queues its bytes on the pane's input. Both commands exit
// 0, so a delivery made in that state looks entirely successful and submits
// nothing. See ensurePaneAcceptsKeys.
//
// The target is the bare id, matching send-keys/paste-buffer rather than the
// `=`-prefixed exact form used by the session-scoped commands: display-message
// resolves `=<name>` to nothing and prints an empty line, so the guard would
// answer "unreadable" for every healthy pane. Verified against tmux 3.7b.
func paneInModeArgs(id string) []string {
	return []string{"display-message", "-p", "-t", id, "#{pane_in_mode}"}
}

// cancelPaneModeArgs builds args for `tmux send-keys -X -t <id> cancel`, the
// mode-aware way to leave whatever mode a pane is in. -X dispatches a command
// to the mode itself, which is why this works for copy-mode, view-mode and the
// choosers alike, and why it is used instead of guessing at a key (`q`, Escape)
// whose meaning depends on the mode and the configured key table.
func cancelPaneModeArgs(id string) []string {
	return []string{"send-keys", "-X", "-t", id, "cancel"}
}

// sendInterruptArgs builds args for `tmux send-keys -t <id> C-c` to interrupt
// the foreground process without killing the terminal session.
func sendInterruptArgs(id string) []string {
	return []string{"send-keys", "-t", id, "C-c"}
}

// capturePaneArgs builds args for `tmux capture-pane -t <id> -p -S -<lines>`.
// -p prints to stdout; -S -<n> starts n lines back in history.
func capturePaneArgs(id string, lines int) []string {
	return []string{"capture-pane", "-t", id, "-p", "-S", fmt.Sprintf("-%d", lines)}
}

// capturePaneStyledArgs preserves SGR sequences so callers can distinguish a
// dim TUI placeholder from normal human-authored composer text.
func capturePaneStyledArgs(id string, lines int) []string {
	return []string{"capture-pane", "-e", "-t", id, "-p", "-S", fmt.Sprintf("-%d", lines)}
}

// ownerEnvKey is the session-environment variable carrying AO's ownership
// token. Session environment is used rather than a `@`-prefixed user option
// because `new-session -e` sets it AS PART OF the creation command, which a
// `set-option` call never can: the marker and the session become visible in the
// same tmux operation, so no state exists in which one is present without the
// other.
const ownerEnvKey = "AO_SESSION_OWNER"

// sessionOwnerArgs reads the ownership token back.
//
// tmux exits non-zero ("unknown variable") when the session carries no such
// variable, which the caller reads as "unmarked", not as an error.
func sessionOwnerArgs(id string) []string {
	return []string{"show-environment", "-t", exactSessionTarget(id), ownerEnvKey}
}

// paneDeadArgs asks whether each pane's process has exited.
//
// `has-session` answers a different and much weaker question — whether a NAME
// is registered — and a session whose reviewer has exited still answers yes to
// it whenever `remain-on-exit` keeps the dead pane around. Adopting on that
// answer is adopting a corpse, so liveness is proven per-pane instead.

// setRemainOnExitOffArgs pins the dead-pane behaviour AO relies on, whatever the
// operator's own tmux configuration says. With `remain-on-exit on` inherited
// from a user's config, a finished reviewer leaves its session standing, which
// is the precise shape of a phantom running review.
func setRemainOnExitOffArgs(id string) []string {
	return []string{"set-option", "-t", exactSessionTarget(id), "remain-on-exit", "off"}
}

// sessionInstanceArgs re-reads just the instance id, for the revalidation that
// closes a read-then-act window.
func sessionInstanceArgs(id string) []string {
	return []string{"display-message", "-p", "-t", id, "#{session_id}"}
}

// killSessionInstanceArgs destroys ONE EXACT session incarnation.
//
// tmux accepts a `$N` session id as a target, and that is the whole point: a
// kill aimed at a NAME lands on whatever holds the name at the moment it runs,
// which after a replacement is somebody else's session. Aimed at `$N` it either
// kills the instance AO proved it owned or it fails, and both outcomes are safe.
func killSessionInstanceArgs(instanceID string) []string {
	return []string{"kill-session", "-t", instanceID}
}
