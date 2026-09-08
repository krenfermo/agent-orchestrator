package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// The classifier's own table. Everything above it in AO depends on exactly one
// property: a signalled or unstartable process is never mistaken for a process
// that answered.

func TestVerdictHelper(t *testing.T) {
	switch os.Getenv("GO_WANT_VERDICT_HELPER") {
	case "exit-3":
		os.Exit(3)
	case "sleep":
		select {}
	}
}

func helperCmd(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestVerdictHelper")
	cmd.Env = append(os.Environ(), "GO_WANT_VERDICT_HELPER="+mode)
	return cmd
}

func TestExitStatusReportsARealExitCode(t *testing.T) {
	err := helperCmd("exit-3").Run()
	code, ok := ExitStatus(err)
	if !ok {
		t.Fatalf("ExitStatus(%v) reported no verdict, want one", err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !RenderedVerdict(context.Background(), err) {
		t.Fatalf("RenderedVerdict = false, want true for a process that exited")
	}
}

func TestExitStatusRefusesAVerdictForASignalledProcess(t *testing.T) {
	cmd := helperCmd("sleep")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	err := cmd.Wait()
	if _, ok := ExitStatus(err); ok {
		t.Fatalf("ExitStatus(%v) reported a verdict, want none: the process was killed", err)
	}
	if RenderedVerdict(context.Background(), err) {
		t.Fatalf("RenderedVerdict = true, want false for a killed process")
	}
}

func TestExitStatusRefusesAVerdictForAProcessThatNeverStarted(t *testing.T) {
	err := exec.Command("ao-no-such-binary-anywhere").Run()
	if err == nil {
		t.Fatal("expected a start failure")
	}
	if _, ok := ExitStatus(err); ok {
		t.Fatalf("ExitStatus(%v) reported a verdict, want none: nothing ran", err)
	}
}

func TestRenderedVerdictRefusesAVerdictUnderACancelledContext(t *testing.T) {
	err := helperCmd("exit-3").Run()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if RenderedVerdict(ctx, err) {
		t.Fatal("RenderedVerdict = true under a cancelled context, want false")
	}
	if !RenderedVerdict(nil, err) { //nolint:staticcheck // a nil context is the documented "nothing to consult"
		t.Fatal("RenderedVerdict = false with no context to consult, want true")
	}
}

func TestExitStatusTreatsSuccessAsAVerdict(t *testing.T) {
	code, ok := ExitStatus(nil)
	if !ok || code != 0 {
		t.Fatalf("ExitStatus(nil) = (%d, %v), want (0, true)", code, ok)
	}
	if _, ok := ExitStatus(errors.New("some non-exec failure")); ok {
		t.Fatal("a non-exec error must not be read as a process verdict")
	}
}
