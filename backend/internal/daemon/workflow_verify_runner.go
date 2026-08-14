package daemon

import (
	"bytes"
	"context"
	"errors"
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
