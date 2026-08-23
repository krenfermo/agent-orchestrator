package tmux

// Checkpoint 8P-E.13C: large prompts must not travel inside a tmux command.
//
// The incident: a verify-driven fix prompt was typed into the pane with
// `send-keys -l <chunk>` at a 16 KB chunk size. tmux carries a whole command in
// one imsg frame capped at 16 KB, so the very first chunk was rejected —
//
//	send agent-orchestrator-15: ... exit status 1: command too long
//
// — and the fix step of wf-2261767d died there. These tests pin the transport,
// not the symptom: what tmux is asked to do, what the payload bytes look like
// when they get there, and what is left on disk afterwards.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// newBufferTestRuntime is newTestRuntime with an AO-owned scratch dir, so the
// staged prompt/launch files are observable (and never land in the real temp
// dir during tests).
func newBufferTestRuntime(t *testing.T) (*Runtime, *fakeRunner, string) {
	t.Helper()
	fr := &fakeRunner{}
	scratch := t.TempDir()
	r := New(Options{Binary: "tmux-test", Timeout: time.Second, Shell: "/bin/sh", ScratchDir: scratch})
	r.runner = fr
	r.enterDelay = 0
	r.reapSessions = (&recordingReaper{}).reap
	return r, fr, scratch
}

// argsOf returns the recorded args for the first call to subcommand.
func argsOf(fr *fakeRunner, subcommand string) ([]string, bool) {
	for _, c := range fr.calls {
		if subcommandOf(c.args) == subcommand {
			return c.args, true
		}
	}
	return nil, false
}

// A + B: a prompt far past the old 16 KB ceiling is delivered, and no command
// AO issues comes anywhere near a tmux command frame.
func TestSendMessageLargePromptUsesPasteBufferNotSendKeys(t *testing.T) {
	r, fr, _ := newBufferTestRuntime(t)
	message := strings.Repeat("verify findings line with some length\n", 4000) // ~150 KB

	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, message); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if n := countCalls(fr, "send-keys"); n != 1 {
		t.Fatalf("send-keys calls = %d, want exactly 1 (the submitting Enter)", n)
	}
	load, ok := argsOf(fr, "load-buffer")
	if !ok {
		t.Fatal("expected the payload to be loaded from a file via load-buffer")
	}
	if _, ok := argsOf(fr, "paste-buffer"); !ok {
		t.Fatal("expected the loaded buffer to be pasted into the pane")
	}
	for _, c := range fr.calls {
		for _, a := range c.args {
			if len(a) > ports.MaxInlinePromptBytes {
				t.Fatalf("a %d-byte argument reached the tmux command (subcommand %q); the payload must travel by file", len(a), subcommandOf(c.args))
			}
			if strings.Contains(a, "verify findings line") {
				t.Fatalf("prompt text appeared in argv of %q — it must never be visible in the process table", subcommandOf(c.args))
			}
		}
	}
	// The Enter that submits the prompt is still sent, last.
	if got, want := fr.calls[len(fr.calls)-1].args, srv(sendEnterArgs("sess-1")); !reflect.DeepEqual(got, want) {
		t.Fatalf("last call = %#v, want the submitting Enter %#v", got, want)
	}
	_ = load
}

// C: exact bytes. Quotes, backticks, $(...), backslashes, newlines, emoji and
// CJK all reach the file verbatim — the file is what tmux reads, so this is the
// fidelity guarantee.
func TestSendMessageLargePromptPreservesExactBytes(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	payload := "quotes: \"double\" 'single' `backtick`\n" +
		"shell: $(rm -rf /) ${HOME} $PATH | && ; > <\n" +
		"backslash: C:\\Users\\dev\\path \\n not-a-newline\n" +
		"unicode: café 世界 🚀 ✅ — em dash\n" +
		"tabs:\tand\ttabs\n"
	message := payload + strings.Repeat("filler to force the file transport\n", 500)

	var staged string
	fr.hook = func(context.Context, int) error {
		// Capture the staged file while it still exists: it is removed as soon
		// as the send returns.
		if staged == "" {
			if files, err := filepath.Glob(filepath.Join(scratch, "prompts", "ao-prompt-*")); err == nil && len(files) == 1 {
				b, rerr := os.ReadFile(files[0])
				if rerr == nil {
					staged = string(b)
				}
			}
		}
		return nil
	}
	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, message); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if staged == "" {
		t.Fatal("no staged prompt file was observed")
	}
	if staged != message {
		t.Fatalf("staged prompt differs from the message: %d bytes staged, %d bytes sent", len(staged), len(message))
	}
}

// F: nothing is left behind. The prompt file is removed even though tmux
// "read" it, and the buffer is deleted by paste-buffer -d.
func TestSendMessageLargePromptCleansUpStagedFile(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	message := strings.Repeat("x", 64*1024)

	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, message); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(scratch, "prompts", "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("staged prompt files left behind: %v", files)
	}
	paste, ok := argsOf(fr, "paste-buffer")
	if !ok {
		t.Fatal("expected a paste-buffer call")
	}
	if !containsArg(paste, "-d") {
		t.Fatalf("paste-buffer args = %v, want -d so the buffer is dropped after pasting", paste)
	}
}

// F (failure half): a paste that never happens still leaves no file, and drops
// the buffer it loaded.
func TestSendMessageCleansUpWhenPasteFails(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	fr.hook = func(_ context.Context, call int) error {
		if call == 2 { // 1 = load-buffer, 2 = paste-buffer
			return errors.New("no such pane")
		}
		return nil
	}
	err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, strings.Repeat("y", 32*1024))
	if err == nil {
		t.Fatal("expected the paste failure to be reported")
	}
	if errors.Is(err, ports.ErrPromptUndelivered) {
		t.Fatal("a failed paste is ambiguous, not provably undelivered: it must NOT invite an automatic re-send")
	}
	if _, ok := argsOf(fr, "delete-buffer"); !ok {
		t.Fatal("expected the loaded buffer to be deleted after a failed paste")
	}
	files, _ := filepath.Glob(filepath.Join(scratch, "prompts", "*"))
	if len(files) != 0 {
		t.Fatalf("staged prompt files left behind after a failed paste: %v", files)
	}
}

// E: a transport that refuses the payload before anything reaches the pane is
// reported as provably undelivered, which is what makes an automatic re-send
// safe (workflow's bounded retry depends on exactly this distinction).
func TestSendMessageRefusedBeforeDeliveryIsRetryable(t *testing.T) {
	t.Run("buffer transport", func(t *testing.T) {
		r, fr, _ := newBufferTestRuntime(t)
		fr.err = errors.New("exit status 1: command too long")
		err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, strings.Repeat("z", 32*1024))
		if !errors.Is(err, ports.ErrPromptUndelivered) {
			t.Fatalf("err = %v, want it to wrap ports.ErrPromptUndelivered", err)
		}
	})
	t.Run("inline transport", func(t *testing.T) {
		r, fr, _ := newBufferTestRuntime(t)
		fr.err = errors.New("exit status 1: command too long")
		err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "short prompt")
		if !errors.Is(err, ports.ErrPromptUndelivered) {
			t.Fatalf("err = %v, want it to wrap ports.ErrPromptUndelivered", err)
		}
	})
}

// G: small prompts keep the old, simpler inline path verbatim.
func TestSendMessageSmallPromptStaysInline(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "please fix the failing test"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, ok := argsOf(fr, "load-buffer"); ok {
		t.Fatal("a small prompt must not pay for the file transport")
	}
	if got, want := fr.calls[0].args, srv(sendKeysLiteralArgs("sess-1", "please fix the failing test")); !reflect.DeepEqual(got, want) {
		t.Fatalf("first call = %#v, want an inline literal send %#v", got, want)
	}
	files, _ := filepath.Glob(filepath.Join(scratch, "prompts", "*"))
	if len(files) != 0 {
		t.Fatalf("small prompt staged a file: %v", files)
	}
}

// The chunk size can no longer be set at (or above) tmux's own command ceiling
// by default — that default was the bug.
func TestDefaultChunkSizeStaysUnderTmuxCommandCeiling(t *testing.T) {
	r := New(Options{Binary: "tmux-test"})
	if r.chunkSize > ports.MaxInlinePromptBytes {
		t.Fatalf("default chunk size = %d, want <= %d", r.chunkSize, ports.MaxInlinePromptBytes)
	}
}

// B (launch half): claude-code, codex and every other adapter that delivers its
// prompt in the launch argv put that prompt inside `tmux new-session`. Past the
// inline budget the command body moves into a sourced script, so what tmux gets
// is a path.
func TestCreateStagesOversizedLaunchCommand(t *testing.T) {
	// D: the same transport covers every agent whose prompt travels in its
	// launch argv — the Codex reviewer AO dispatches for review, the Claude
	// worker it dispatches for work, and anything else built on the same
	// default (ports.PromptDeliveryInCommand).
	for _, harness := range []string{"codex", "claude"} {
		t.Run(harness, func(t *testing.T) { assertOversizedLaunchIsStaged(t, harness) })
	}
}

func assertOversizedLaunchIsStaged(t *testing.T, binary string) {
	t.Helper()
	r, fr, scratch := newBufferTestRuntime(t)
	prompt := strings.Repeat("A very long work prompt. ", 2000) // ~50 KB
	var scriptBody, scriptPath string
	fr.hook = func(_ context.Context, call int) error {
		if call == 1 {
			files, err := filepath.Glob(filepath.Join(scratch, "launch", "ao-launch-*"))
			if err == nil && len(files) == 1 {
				scriptPath = files[0]
				if b, rerr := os.ReadFile(files[0]); rerr == nil {
					scriptBody = string(b)
				}
			}
		}
		return nil
	}
	// display-message (pane cwd verification) then has-session must both answer.
	fr.outputs = [][]byte{nil, []byte("/work"), nil, nil, nil, nil}

	_, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "sess-1", WorkspacePath: "/work", Argv: []string{binary, prompt},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	newSession, ok := argsOf(fr, "new-session")
	if !ok {
		t.Fatal("expected a new-session call")
	}
	for _, a := range newSession {
		if len(a) > maxInlineLaunchCommandBytes {
			t.Fatalf("new-session carried a %d-byte argument; the launch body must travel by file", len(a))
		}
		if strings.Contains(a, "A very long work prompt") {
			t.Fatal("the prompt reached the tmux command line")
		}
	}
	if scriptBody == "" {
		t.Fatal("no launch script was staged")
	}
	if !strings.Contains(scriptBody, prompt) {
		t.Fatal("the staged launch script does not carry the prompt verbatim")
	}
	// The script removes itself; AO must NOT unlink it from the outside. See
	// TestCreateNeverUnlinksTheLaunchScriptItAskedTheShellToSource for why the
	// external unlink was the wf-57f90ff2 root cause.
	firstLine, _, _ := strings.Cut(scriptBody, "\n")
	if !strings.Contains(firstLine, "rm -f --") || !strings.Contains(firstLine, scriptPath) {
		t.Fatalf("the staged script's first statement must remove the script itself, got %q", firstLine)
	}
	if _, statErr := os.Stat(scriptPath); statErr != nil {
		t.Fatalf("launch script %s must survive Create — only the pane's own shell may remove it (%v)", scriptPath, statErr)
	}
}

// G (launch half): an ordinary launch command is still inlined exactly as
// before — no file, no behavior change.
func TestCreateKeepsSmallLaunchCommandInline(t *testing.T) {
	r, fr, scratch := newBufferTestRuntime(t)
	fr.outputs = [][]byte{nil, []byte("/work"), nil, nil, nil, nil}
	if _, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "sess-1", WorkspacePath: "/work", Argv: []string{"codex", "do the thing"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	newSession, _ := argsOf(fr, "new-session")
	if !containsArgContaining(newSession, "do the thing") {
		t.Fatalf("small launch command should stay inline, got %v", newSession)
	}
	files, _ := filepath.Glob(filepath.Join(scratch, "launch", "*"))
	if len(files) != 0 {
		t.Fatalf("small launch command staged a file: %v", files)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsArgContaining(args []string, want string) bool {
	for _, a := range args {
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}
