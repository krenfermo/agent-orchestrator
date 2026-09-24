package skillrunner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// exec.go — every container CLI call AO makes ends when its limit does.
//
// THE INCIDENT. During the 2D/2E end-to-end runs the Colima VM's virtiofs share
// stalled under host load. A staging-visibility probe container blocked in
// `cat` on the mount; the `docker run` that started it was killed at its
// deadline, but the container was not, and two of them were still running 13
// and 32 minutes later. A dependencies scan in the same window ran out its
// 2-minute wall clock.
//
// What this file guarantees, and what it cannot:
//
//   - The CLI runs in its own process group, and a deadline or a cancel kills
//     the GROUP, so a helper the CLI started cannot outlive it holding AO's pipes.
//   - After the kill AO waits commandGrace for the process to be reaped, and then
//     STOPS WAITING. The caller gets ErrCommandAbandoned and moves on; one
//     goroutine stays behind to reap the process whenever the kernel lets it.
//     That is the only honest option: SIGKILL cannot interrupt a process in
//     uninterruptible sleep (a read on a wedged network or VM filesystem, on some
//     kernels), and no user-space code can make it. What AO can promise is that
//     its WORKER is released, not that the process is gone.
//   - Killing the CLI never stops the CONTAINER: that lives in the runtime's VM.
//     Removing it is a separate, bounded, CONFIRMED step (removeContainer), and a
//     removal that cannot be confirmed is recorded as pending, never as done.

// ErrRuntimeTimeout is the container runtime not answering within the limit AO
// set for one operation. It is distinct from ErrRuntimeUnavailable (probed at
// boot, the runtime is not usable at all) and from ErrStagingNotVisible (the
// runtime answered, and the mount was empty): a timeout says nothing about the
// share or the image, only that the runtime was stuck, and it is worth retrying
// once the runtime recovers.
var ErrRuntimeTimeout = errors.New("skillrunner: the container runtime did not answer in time")

// ErrCommandAbandoned is a container CLI process that did not exit after it was
// killed. AO stopped waiting for it; the process may still exist.
var ErrCommandAbandoned = errors.New("skillrunner: a container CLI process did not exit after it was killed")

// commandGrace is how long AO waits, after killing a CLI process, for the kernel
// to reap it before abandoning the wait. It is also the WaitDelay after which
// output pipes held open by a descendant are closed.
var commandGrace = 5 * time.Second

// abandonedCommands counts CLI processes AO stopped waiting for. It is process-
// wide because the processes are: an operator looking at a host with a wedged
// runtime needs one number, not one per Runner.
var abandonedCommands atomic.Int64

// killGroup is killProcessGroup; a var only so a test can stand in for a
// process the kernel will not let die, which no test can create for real.
var killGroup = killProcessGroup

// AbandonedCommands reports how many container CLI processes AO killed and then
// stopped waiting for since the daemon started.
func AbandonedCommands() int64 { return abandonedCommands.Load() }

// runBounded runs cmd until it exits or ctx ends, whichever is first, and never
// longer than ctx plus commandGrace.
//
// cmd must not have been started, and must not carry a Context (the kill is
// done here, to the process group). stdout and stderr, when cmd writes to
// in-memory buffers, are only safe to read when the error is not
// ErrCommandAbandoned: an abandoned process may still be writing to them.
func runBounded(ctx context.Context, cmd *exec.Cmd) error {
	setProcessGroup(cmd)
	// Wait returns this long after the process exits even if a descendant still
	// holds the output pipes open, so a surviving grandchild cannot pin Wait.
	cmd.WaitDelay = commandGrace
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	killGroup(cmd)
	timer := time.NewTimer(commandGrace)
	defer timer.Stop()
	select {
	case <-done:
		return describeContextEnd(ctx, cmd, nil)
	case <-timer.C:
		abandonedCommands.Add(1)
		return describeContextEnd(ctx, cmd, fmt.Errorf("%w: pid %d was killed and had not exited after %s",
			ErrCommandAbandoned, cmd.Process.Pid, commandGrace))
	}
}

// describeContextEnd names why a command was stopped: a deadline is the
// runtime timing out (ErrRuntimeTimeout), a cancel is the caller's decision
// (context.Canceled), and either may carry the abandonment.
func describeContextEnd(ctx context.Context, cmd *exec.Cmd, abandoned error) error {
	what := commandLine(cmd)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		if abandoned != nil {
			return fmt.Errorf("%w: %s did not finish within its limit: %w: %w",
				ErrRuntimeTimeout, what, ctx.Err(), abandoned)
		}
		return fmt.Errorf("%w: %s did not finish within its limit: %w", ErrRuntimeTimeout, what, ctx.Err())
	}
	if abandoned != nil {
		return fmt.Errorf("%s was stopped: %w: %w", what, ctx.Err(), abandoned)
	}
	return fmt.Errorf("%s was stopped: %w", what, ctx.Err())
}

// commandLine renders a command for an error: the binary and the verb, never
// the full argv (which can carry paths and, for a run, the environment).
func commandLine(cmd *exec.Cmd) string {
	if len(cmd.Args) > 1 {
		return fmt.Sprintf("`%s %s`", cmd.Args[0], cmd.Args[1])
	}
	return fmt.Sprintf("`%s`", strings.Join(cmd.Args, " "))
}

// ErrWallClockExceeded is a run that used up its own wall clock (Limits.Wall).
// It produced no complete output, so there is no report.
var ErrWallClockExceeded = errors.New("skillrunner: the run exceeded its wall clock")

// cleanupNote tells the reader of an error whether the run's container is
// known to be gone.
func cleanupNote(res Result) string {
	note := ""
	if res.Abandoned {
		note += fmt.Sprintf("; the container CLI did not exit after it was killed and AO stopped waiting "+
			"for it (%d such process(es) since the daemon started)", AbandonedCommands())
	}
	if res.Cleanup == CleanupPending {
		note += "; the runtime did not confirm the container removed, so it is recorded for cleanup and will be retried"
	}
	return note
}
