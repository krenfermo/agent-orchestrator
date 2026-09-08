package process

import (
	"context"
	"errors"
	"os/exec"
)

// verdict.go — "did the process actually answer, or did it merely stop?"
//
// This exists because of one incident and one class of bug behind it. AO ran
//
//	git check-ref-format --branch ao/sige-12/root
//
// and got back `signal: killed`. The caller read "the command failed" as "git
// rejected the name" and answered INVALID_BRANCH — a permanent, terminal,
// user-facing verdict about a branch name git accepts without complaint.
//
// The two are not the same fact and must never collapse into one:
//
//   - a process that RAN and EXITED with a status of its own has rendered a
//     verdict. `git check-ref-format` exiting non-zero means git looked at the
//     name and said no. That is a real answer and callers may act on it;
//   - a process that was KILLED — by our own context cancellation or deadline,
//     by an operator, by the OOM killer — or that never started at all (binary
//     missing, permission denied) has rendered NOTHING. Reading a verdict out
//     of it is inventing one.
//
// The distinction is decidable rather than heuristic: a signalled process has
// no exit status, and os.ProcessState.Exited() reports exactly that.
//
// Nothing here weakens any check. A probe that could not run stays a failure;
// it is simply a failure whose cause is unknown, and callers are expected to
// treat it as retryable/inconclusive rather than as the probe's answer.

// ExitStatus returns the status a finished process exited with, and whether it
// actually exited with one.
//
// ok is false for every outcome that is not a completed run: a process
// terminated by a signal, a command that could not be started, and any error
// that is not an *exec.ExitError at all.
func ExitStatus(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ProcessState == nil {
		return 0, false
	}
	if !exit.ProcessState.Exited() {
		// Terminated by a signal: there is no exit status to report.
		return 0, false
	}
	return exit.ProcessState.ExitCode(), true
}

// RenderedVerdict reports whether err came from a process that ran to
// completion and exited with a status of its own — i.e. whether the command's
// non-zero result is the command's ANSWER.
//
// ctx is consulted as well as the error: a cancelled or expired context means
// AO stopped waiting for this process, and an answer collected in that window
// is not one AO asked for. Pass a nil context when there is none to consult.
func RenderedVerdict(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	_, ok := ExitStatus(err)
	return ok
}
