package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

const verifyOutputTailLimit = 16 * 1024

type workflowVerifyRunner struct{}

func (workflowVerifyRunner) Run(ctx context.Context, req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
	if err := workflowcore.ValidateVerifyCommand(req.Command, req.Args); err != nil {
		return workflowcore.VerifyCommandExecution{ExitCode: -1}, err
	}
	commandCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cmd := aoprocess.CommandContext(commandCtx, req.Command, req.Args...)
	cmd.Dir = req.Directory
	// Checkpoint 8M.1: skip Python's .pyc bytecode cache for Verify commands.
	// A no-op for non-Python commands; for Python it stops __pycache__ from
	// ever being generated, closing the E2E-observed false
	// verify_workspace_changed failure mode at the source.
	env := append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if req.Directory != "" {
		// os/exec only auto-derives PWD from Dir when Env is left nil (see
		// exec.Cmd's own doc comment on Dir); setting Env explicitly here
		// opts out of that, so it must be replicated by hand or a command
		// like `pwd` would report the physical (symlink-resolved) directory
		// instead of the logical one callers passed in.
		env = append(env, "PWD="+req.Directory)
	}
	cmd.Env = env
	stdout, stderr := &tailBuffer{limit: verifyOutputTailLimit}, &tailBuffer{limit: verifyOutputTailLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	started := time.Now()
	err := cmd.Run()
	duration := time.Since(started)
	result := workflowcore.VerifyCommandExecution{ExitCode: 0, DurationMS: duration.Milliseconds(), StdoutTail: stdout.String(), StderrTail: stderr.String(), TimedOut: errors.Is(commandCtx.Err(), context.DeadlineExceeded)}
	if err == nil {
		return result, nil
	}
	if commandCtx.Err() != nil && !result.TimedOut {
		return result, commandCtx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	result.ExitCode = -1
	if result.TimedOut {
		return result, nil
	}
	return result, err
}

type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return n, nil
}
func (b *tailBuffer) String() string { return string(bytes.ToValidUTF8(b.data, []byte("?"))) }
