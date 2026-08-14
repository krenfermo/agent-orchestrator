package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func TestWorkflowVerifyRunnerUsesRequestedDirectory(t *testing.T) {
	dir := t.TempDir()
	got, err := (workflowVerifyRunner{}).Run(context.Background(), workflowcore.VerifyCommandRequest{Command: "pwd", Directory: dir, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got.StdoutTail) != dir {
		t.Fatalf("cwd=%q want %q", got.StdoutTail, dir)
	}
}

func TestWorkflowVerifyRunnerRejectsUnsafeCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"git", []string{"push", "origin", "HEAD"}},
		{"git", []string{"merge", "main"}},
		{"sh", []string{"-c", "echo unsafe"}},
		{"rm", []string{"-rf", "fixture"}},
	} {
		t.Run(strings.Join(append([]string{tc.name}, tc.args...), "_"), func(t *testing.T) {
			if _, err := (workflowVerifyRunner{}).Run(context.Background(), workflowcore.VerifyCommandRequest{Command: tc.name, Args: tc.args, Directory: t.TempDir(), Timeout: time.Second}); err == nil {
				t.Fatal("unsafe command was accepted")
			}
		})
	}
}

func TestWorkflowVerifyRunnerTimeoutAndBoundedOutput(t *testing.T) {
	got, err := (workflowVerifyRunner{}).Run(context.Background(), workflowcore.VerifyCommandRequest{Command: "sleep", Args: []string{"1"}, Directory: t.TempDir(), Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !got.TimedOut {
		t.Fatal("expected timeout")
	}
	b := &tailBuffer{limit: 8}
	_, _ = b.Write([]byte("0123456789"))
	if b.String() != "23456789" {
		t.Fatalf("tail=%q", b.String())
	}
}

func TestWorkflowVerifyRunnerPreservesDaemonCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err := (workflowVerifyRunner{}).Run(ctx, workflowcore.VerifyCommandRequest{Command: "sleep", Args: []string{"1"}, Directory: t.TempDir(), Timeout: time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
