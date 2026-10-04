package tmux

// Incident wf-57f90ff2 (2026-08-23), transport half.
//
// A 15 KB fix prompt was delivered into worker session agent-orchestrator-29
// while its pane sat in tmux copy-mode. tmux hands keys to the MODE while a
// pane is in one, so:
//
//	paste-buffer   -> the payload was queued on the pane's input, exit 0
//	send-keys Enter -> consumed by copy-mode, never reached Codex, exit 0
//
// AO saw two successful commands and recorded a delivered fix cycle. The prompt
// sat in Codex's composer as an unsubmitted draft, and the run was later stopped
// for a change the worker had never been asked to make. A later re-delivery
// appended a SECOND copy of the same prompt to the same draft.
//
// These tests pin the guard that now runs before anything is written, including
// the property that makes the whole thing safe: a refusal happens before the
// payload is staged, so it is provably a non-delivery and a caller may re-send.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A pane that is not in a mode is left completely alone: no cancel, no extra
// keys, exactly the delivery AO always made.
func TestSendMessageDoesNotDisturbAPaneThatIsNotInAMode(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.paneInMode = []string{"0"}

	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if n := countCalls(fr, "display-message"); n != 1 {
		t.Fatalf("pane-mode probes = %d, want exactly 1", n)
	}
	for _, c := range fr.calls {
		if containsArg(c.args, "-X") {
			t.Fatalf("a healthy pane was sent a mode command: %v", c.args)
		}
	}
	sent := sendCalls(fr)
	if len(sent) != 2 {
		t.Fatalf("delivery calls = %d, want 2 (chunk + Enter)", len(sent))
	}
}

// The incident's own state: the pane is in copy-mode. AO must leave the mode
// BEFORE writing anything, then deliver normally.
func TestSendMessageLeavesCopyModeBeforeDelivering(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.paneInMode = []string{"1", "0"} // in copy-mode, then out after the cancel

	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	cancelAt, firstWriteAt := -1, -1
	for i, c := range fr.calls {
		switch {
		case containsArg(c.args, "-X") && containsArg(c.args, "cancel"):
			if cancelAt < 0 {
				cancelAt = i
			}
		case subcommandOf(c.args) == "send-keys" || subcommandOf(c.args) == "paste-buffer" || subcommandOf(c.args) == "load-buffer":
			if firstWriteAt < 0 {
				firstWriteAt = i
			}
		}
	}
	if cancelAt < 0 {
		t.Fatal("pane was in copy-mode and AO never left it: the submitting Enter would be swallowed")
	}
	if firstWriteAt < cancelAt {
		t.Fatalf("AO wrote to the pane (call %d) before leaving copy-mode (call %d)", firstWriteAt, cancelAt)
	}
	if n := countCalls(fr, "display-message"); n != 2 {
		t.Fatalf("pane-mode probes = %d, want 2 (before and after the cancel)", n)
	}
}

// The cancel's exit status is not trusted: tmux reports success for a command a
// mode declined. A pane still in a mode must refuse the delivery outright, and
// refuse it as provably undelivered so the caller may safely re-send later.
func TestSendMessageRefusesWhenTheModeWillNotClear(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.paneInMode = []string{"1"} // stays in the mode however often it is read

	err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "hello")
	if err == nil {
		t.Fatal("expected a refusal: keys sent into a stuck mode never reach the agent")
	}
	if !errors.Is(err, ports.ErrPromptUndelivered) {
		t.Fatalf("err = %v, want it to wrap ports.ErrPromptUndelivered (nothing was written)", err)
	}
	for _, c := range fr.calls {
		if subcommandOf(c.args) == "paste-buffer" || subcommandOf(c.args) == "load-buffer" {
			t.Fatalf("payload was staged despite the refusal: %v", c.args)
		}
		if subcommandOf(c.args) == "send-keys" && containsArg(c.args, "-l") {
			t.Fatalf("payload was typed despite the refusal: %v", c.args)
		}
	}
}

// A pane whose mode cannot be read at all is missing evidence, never evidence
// of a healthy pane. Refuse, and refuse retryably.
func TestSendMessageRefusesWhenPaneModeIsUnreadable(t *testing.T) {
	for name, fr := range map[string]*fakeRunner{
		"probe errors":     {paneInModeErr: errors.New("no such pane")},
		"unexpected value": {paneInMode: []string{""}},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newTestRuntime(0)
			r.runner = fr
			err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "hello")
			if !errors.Is(err, ports.ErrPromptUndelivered) {
				t.Fatalf("err = %v, want ports.ErrPromptUndelivered", err)
			}
		})
	}
}

// The large-prompt path is guarded too, and — critically — the guard runs
// before the prompt file is ever staged, so a refusal leaves nothing on disk
// and nothing in a tmux buffer.
func TestBufferTransportIsGuardedBeforeStaging(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	fr.paneInMode = []string{"1"}

	err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, strings.Repeat("x", 32*1024))
	if !errors.Is(err, ports.ErrPromptUndelivered) {
		t.Fatalf("err = %v, want ports.ErrPromptUndelivered", err)
	}
	if _, ok := argsOf(fr, "load-buffer"); ok {
		t.Fatal("the payload was loaded into a tmux buffer despite the pane refusing keys")
	}
	files, _ := filepath.Glob(filepath.Join(scratch, "prompts", "*"))
	if len(files) != 0 {
		t.Fatalf("prompt staged on disk despite the refusal: %v", files)
	}
}

// An empty-message nudge is exactly an Enter, which is exactly the key
// copy-mode swallows. It must be guarded like any other delivery, or a "catch-up
// Enter" silently does nothing.
func TestNudgeIsGuardedAgainstCopyMode(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.paneInMode = []string{"1", "0"}

	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, ""); err != nil {
		t.Fatalf("SendMessage (nudge): %v", err)
	}
	cancelled := false
	for _, c := range fr.calls {
		if containsArg(c.args, "-X") && containsArg(c.args, "cancel") {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatal("a nudge into a pane in copy-mode did not leave the mode first")
	}
}
