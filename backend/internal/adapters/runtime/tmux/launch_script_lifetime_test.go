package tmux

// Root-cause regression coverage for incident wf-57f90ff2 / session
// agent-orchestrator-29:
//
//	work dispatch failed (agent_start_failed): spawn agent-orchestrator-29:
//	runtime: tmux runtime: set status agent-orchestrator-29:
//	exit status 1: no such session: agent-orchestrator-29
//
// The session was created successfully and then disappeared BETWEEN
// `new-session` and the very next command. What removed it was AO itself:
// Create unlinked the staged launch script as soon as
// verifyPaneWorkingDirectory answered, believing a pane whose cwd is the
// workspace must already hold the script open. It does not — tmux chdirs the
// pane to `new-session -c <cwd>` before exec'ing the shell, so the probe is
// true from the instant the pane exists and says nothing about the script. The
// unlink therefore raced the shell's open(), `. <path>` failed, the shell
// exited, and tmux tore the session down under the next command.
//
// These tests pin the ordering rather than the symptom.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// oversizedLaunchConfig is a RuntimeConfig whose launch command is guaranteed
// to exceed maxInlineLaunchCommandBytes, i.e. the staged-script path every
// real worker/reviewer prompt takes.
func oversizedLaunchConfig() ports.RuntimeConfig {
	return ports.RuntimeConfig{
		SessionID:     "agent-orchestrator-29",
		WorkspacePath: "/work",
		Argv:          []string{"claude", strings.Repeat("A very long work prompt. ", 2000)},
	}
}

// errFakeExit stands in for a tmux CLI command that exited non-zero.
var errFakeExit = errors.New("exit status 1: no such session: agent-orchestrator-29")

func stagedLaunchScripts(t *testing.T, scratch string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(scratch, "launch", "ao-launch-*"))
	if err != nil {
		t.Fatalf("glob staged launch scripts: %v", err)
	}
	return files
}

// A. The exact incident ordering: the launch script must still be on disk at
// the moment tmux is asked to configure the session. Before the fix it was
// already unlinked by then, which is what killed the pane's shell.
func TestCreateNeverUnlinksTheLaunchScriptItAskedTheShellToSource(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	// display-message (pane cwd probe) then has-session must both answer.
	fr.outputs = [][]byte{nil, []byte("/work"), nil, nil, nil, nil}

	// Record, per tmux subcommand, whether the staged script existed when that
	// command was issued. The pane's shell is the only thing allowed to remove
	// it, and the fake shell here never runs, so it must exist throughout.
	presentAt := map[string]bool{}
	fr.hook = func(_ context.Context, call int) error {
		sub := subcommandOf(fr.calls[call-1].args)
		presentAt[sub] = len(stagedLaunchScripts(t, scratch)) == 1
		return nil
	}

	if _, err := r.Create(context.Background(), oversizedLaunchConfig()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, sub := range []string{"display-message", "set-option", "has-session"} {
		if !presentAt[sub] {
			t.Fatalf("the staged launch script was already gone when %q was issued: "+
				"AO unlinked a script the pane's shell may not have opened yet, which is what "+
				"turned a healthy spawn into \"no such session\"", sub)
		}
	}
	if got := stagedLaunchScripts(t, scratch); len(got) != 1 {
		t.Fatalf("staged scripts after Create = %v, want the one script left for the pane's shell to remove", got)
	}
}

// A (the other half): the cwd probe cannot be used as evidence that the shell
// opened the script, because tmux answers it from the pane's start directory —
// the very value Create passed as `new-session -c`. Pinning this keeps anyone
// from reinstating the old "verified, therefore safe to unlink" reasoning.
func TestPaneCwdProbeAnswersFromTheStartDirectoryNotTheScript(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	// The probe answers with the -c directory on the FIRST attempt, exactly as
	// real tmux does, with no shell having run anything at all.
	fr.outputs = [][]byte{nil, []byte("/work"), nil, nil, nil, nil}
	fr.hook = func(_ context.Context, call int) error {
		if subcommandOf(fr.calls[call-1].args) != "display-message" {
			return nil
		}
		if n := len(stagedLaunchScripts(t, scratch)); n != 1 {
			t.Fatalf("staged scripts at cwd-probe time = %d, want 1", n)
		}
		return nil
	}
	if _, err := r.Create(context.Background(), oversizedLaunchConfig()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n := countCalls(fr, "display-message"); n != 1 {
		t.Fatalf("cwd probe attempts = %d, want 1 — a probe that passes immediately proves nothing about the script", n)
	}
}

// The staged script's own first statement is the removal, so the unlink is
// ordered after the shell's open() by construction rather than by timing.
func TestStagedLaunchScriptRemovesItselfBeforeRunningTheAgent(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	fr.outputs = [][]byte{nil, []byte("/work"), nil, nil, nil, nil}
	if _, err := r.Create(context.Background(), oversizedLaunchConfig()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	files := stagedLaunchScripts(t, scratch)
	if len(files) != 1 {
		t.Fatalf("staged scripts = %v, want exactly 1", files)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read staged script: %v", err)
	}
	lines := strings.SplitN(string(body), "\n", 2)
	if !strings.Contains(lines[0], "rm -f -- ") || !strings.Contains(lines[0], files[0]) {
		t.Fatalf("first statement = %q, want a self-removal of %s", lines[0], files[0])
	}
	if len(lines) < 2 || !strings.Contains(lines[1], "cd ") {
		t.Fatalf("the launch body must follow the self-removal, got %q", body)
	}
}

// A post-create failure still cleans the script up: the shell demonstrably
// never got to run it, and the session is being destroyed anyway. This is the
// path that keeps the fix from trading a race for a disk leak.
func TestCreateRemovesTheStagedScriptWhenConfigurationFails(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	fr.outputs = [][]byte{nil, []byte("/work"), nil, nil, nil, nil}
	fr.hook = func(_ context.Context, call int) error {
		if subcommandOf(fr.calls[call-1].args) == "set-option" {
			return errFakeExit
		}
		return nil
	}
	if _, err := r.Create(context.Background(), oversizedLaunchConfig()); err == nil {
		t.Fatal("Create must fail when the session cannot be configured")
	}
	if got := stagedLaunchScripts(t, scratch); len(got) != 0 {
		t.Fatalf("staged scripts after a failed Create = %v, want none", got)
	}
}

// And a `new-session` that never succeeded must not leave one behind either.
func TestCreateRemovesTheStagedScriptWhenNewSessionFails(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	fr.err = errFakeExit
	if _, err := r.Create(context.Background(), oversizedLaunchConfig()); err == nil {
		t.Fatal("Create must fail when new-session fails")
	}
	if got := stagedLaunchScripts(t, scratch); len(got) != 0 {
		t.Fatalf("staged scripts after a failed new-session = %v, want none", got)
	}
}
