package ports

import "errors"

// Checkpoint 8P-E.13C: how a prompt physically reaches an agent's terminal.
//
// The incident this exists for: a verify-driven fix prompt (objective +
// acceptance criteria + the reviewer's verbatim findings + the verify output)
// was handed to the tmux runtime, which typed it into the pane with
// `tmux send-keys -l <chunk>` in 16 KB chunks. tmux's client/server protocol
// carries a whole command in a single imsg whose payload ceiling is 16 KB, so
// the first chunk of a large prompt was rejected outright:
//
//	send agent-orchestrator-15: ... exit status 1: command too long
//
// The fix is not a bigger chunk or a shorter prompt — both only move the
// ceiling. Above a conservative inline budget the payload stops travelling
// through the command at all: AO writes the exact bytes to a private file and
// the runtime references that file (tmux load-buffer + paste-buffer), so the
// command AO issues stays a few hundred bytes no matter how large the prompt
// is, and the prompt itself never passes through argv, a shell, or any
// quoting/escaping layer that could corrupt it.
//
// The policy lives here, in ports, rather than inside the tmux adapter,
// because two layers need the same answer: the adapter, which performs the
// delivery, and the workflow, which records what it did (prompt bytes,
// transport, context-pack use) so a future delivery problem is diagnosable
// from the durable ledger instead of a stack trace.

// PromptTransport names how a message was (or will be) delivered to a
// terminal-attached agent.
type PromptTransport string

const (
	// PromptTransportInline types the payload into the pane as literal keys.
	// Bounded by the runtime's own command-size ceiling, which is exactly why
	// it is only used below MaxInlinePromptBytes.
	PromptTransportInline PromptTransport = "inline"
	// PromptTransportBufferFile writes the payload to a private file and has
	// the runtime load it into a paste buffer by path. The command carries a
	// path, never the payload, so size is bounded by the filesystem rather
	// than by any protocol frame.
	PromptTransportBufferFile PromptTransport = "buffer_file"
)

// MaxInlinePromptBytes is the largest payload AO will type into a pane
// inline.
//
// tmux's ceiling is its imsg payload limit (16 KB) for the ENTIRE command —
// binary, flags, target, and the text. 4 KB leaves a factor-of-four margin
// for everything else in the frame, which is deliberate: the failure mode of
// being wrong here is a dispatch that fails in production, and the cost of
// being conservative is a few extra paste-buffer round trips.
const MaxInlinePromptBytes = 4 * 1024

// PromptTransportFor is the single decision function both the runtime and the
// workflow's observability call, so the recorded transport can never disagree
// with the one actually used.
func PromptTransportFor(payloadBytes int) PromptTransport {
	if payloadBytes > MaxInlinePromptBytes {
		return PromptTransportBufferFile
	}
	return PromptTransportInline
}

// PromptSubmission is what AO could PROVE about a prompt it wrote into an
// interactive agent's terminal — Checkpoint 8P-E.17.
//
// The distinction exists because "the transport commands exited 0" and "the
// agent was given a turn" are different facts, and AO used to record the first
// as if it were the second. In incident wf-57f90ff2 a 15 KB fix prompt was
// pasted into a pane sitting in tmux copy-mode: the paste queued, the
// submitting Enter was consumed by the mode, both commands succeeded, and the
// prompt sat in Codex's composer as an unsubmitted draft while AO believed it
// had been delivered.
//
// Note what is deliberately NOT in this vocabulary: "the turn actually
// started". That is not the transport's to claim — it is established by the
// agent's own submit hook reaching AO as session activity, and it is judged by
// the workflow (see fixCycleStarted). This type only ever answers "did AO's
// submit land", which is the part the transport can observe.
type PromptSubmission string

const (
	// PromptSubmissionUnset is the zero value: no submission check ran at all
	// (an empty nudge, a chat-mode send, a harness with no composer probe).
	// It is not a verdict and must never be read as one.
	PromptSubmissionUnset PromptSubmission = ""
	// PromptSubmitted means the composer was observed empty after the submit, so
	// the payload left the composer. Positive evidence.
	PromptSubmitted PromptSubmission = "submitted"
	// PromptLoadedNotSubmitted means the payload reached the composer and is still
	// sitting there. Positive evidence too — of the failure. A caller must
	// never re-paste on this verdict; the bytes are already in the draft, and
	// pasting again is exactly what appends a second copy.
	PromptLoadedNotSubmitted PromptSubmission = "loaded_not_submitted"
	// PromptSubmissionAmbiguous means the check could not be made (no styled pane
	// read, no detector for this harness, a read error). Missing evidence,
	// never evidence of either outcome.
	PromptSubmissionAmbiguous PromptSubmission = "ambiguous"
)

// ErrPromptUndelivered reports that a prompt delivery failed BEFORE any part
// of the payload reached the agent — the transport refused the message
// outright (tmux's "command too long", a paste buffer that could not be
// loaded, a temp file that could not be written).
//
// It is the difference between a retry that is safe and one that is not. AO's
// messaging path has no delivery acknowledgement, so a failure mid-delivery is
// permanently ambiguous and must never be retried automatically. This sentinel
// is only ever wrapped by failures that are provably atomic: nothing was typed,
// nothing was pasted, so re-sending delivers the instructions exactly once.
var ErrPromptUndelivered = errors.New("runtime: prompt was refused before delivery")
