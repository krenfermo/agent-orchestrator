package tmux

// P0-C SECTION D — >4 KB multiline prompt transport, proven against a REAL
// tmux server and a REAL raw-mode, bracketed-paste-aware terminal consumer.
//
// THE PRODUCTION DEFECT THIS PINS
//
// tmux replaces every LF in a paste buffer with CR. A TUI reading its input in
// raw mode sees CR as Enter, so `paste-buffer` WITHOUT -p delivered a 90-line
// fix prompt as ~90 separate submissions and only the last fragment survived as
// the message the agent answered. The reviewer's findings live in the MIDDLE of
// a fix prompt, so the failure presented as "the fix worker ignored the review".
// Everything upstream reported success.
//
// WHAT IS ACTUALLY EXERCISED HERE
//
// A real tmux server, a real pane, and a real consumer process whose tty is put
// into raw mode with term.MakeRaw and which requests bracketed paste with
// DECSET 2004 — the exact two things an agent TUI does and a cooked-mode shell
// does not. The consumer records the RAW BYTE STREAM it received, and the
// assertions are made on those bytes.
//
// LIMITATION, STATED PLAINLY: the consumer is AO's own minimal raw-mode
// harness, NOT the Claude TUI. Claude Code is not a deterministic, offline,
// scriptable process, so launching it in an automated test would make this
// suite non-hermetic and non-reproducible. What is proven is the TERMINAL
// SEMANTICS every such TUI depends on — raw mode, DECSET 2004, bracketed
// markers, CR-inside-brackets — not the behaviour of any particular agent
// binary. Nothing in this file may be reported as "real Claude E2E".
//
// The negative control below is what makes that proof meaningful: the SAME
// harness, pasted without -p, reproduces the shredding. A test that cannot fail
// on the bug proves nothing about the fix.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// The bracketed-paste markers tmux emits around a `paste-buffer -p` payload
	// when — and only when — the pane's application has requested DECSET 2004.
	pasteStartMarker = "\x1b[200~"
	pasteEndMarker   = "\x1b[201~"

	rawHelperEnv    = "AO_RAW_PASTE_HELPER"
	rawHelperOutEnv = "AO_RAW_PASTE_OUT"
	// rawHelperUntilEnv makes the consumer record until an explicit sentinel
	// arrives instead of stopping at the first completed paste. It is what lets
	// a test assert that a SECOND delivery never happened: tmux delivers to one
	// pane in order, so a sentinel sent afterwards is a happens-before edge, and
	// "nothing between the paste and the sentinel" is a proof rather than a wait.
	rawHelperUntilEnv = "AO_RAW_PASTE_UNTIL"
)

// TestRawPasteConsumerHelper is the raw-mode consumer. It is not a test: it runs
// only when the tmux pane re-executes this binary with AO_RAW_PASTE_HELPER=1,
// which is how it gets a real controlling tty to put into raw mode.
//
// It does exactly what an agent TUI does at startup and nothing else:
//
//  1. puts its tty into raw mode (no line discipline, no echo, CR is a byte);
//  2. requests bracketed paste with DECSET 2004;
//  3. announces readiness by creating <out>.ready — a fact the test polls for,
//     so nothing here is synchronised by sleeping;
//  4. records every byte it receives, verbatim, until the paste has closed AND
//     the submitting CR has arrived;
//  5. writes the recording to <out> and exits.
func TestRawPasteConsumerHelper(t *testing.T) {
	if os.Getenv(rawHelperEnv) != "1" {
		return
	}
	out := os.Getenv(rawHelperOutEnv)
	if out == "" {
		return
	}
	fd := int(os.Stdin.Fd())
	restore, err := term.MakeRaw(fd)
	if err != nil {
		// Recorded rather than swallowed: a harness that never reached raw mode
		// must fail the test, not silently pass through a cooked-mode path.
		_ = os.WriteFile(out, []byte("RAWMODE-FAILED: "+err.Error()), 0o600)
		_ = os.WriteFile(out+".ready", nil, 0o600)
		return
	}
	defer func() { _ = term.Restore(fd, restore) }()

	// DECSET 2004. Without this, tmux -p adds no markers at all: bracketed paste
	// is a capability the APPLICATION opts into.
	_, _ = os.Stdout.WriteString("\x1b[?2004h")

	if err := os.WriteFile(out+".ready", []byte("raw\n"), 0o600); err != nil {
		return
	}

	until := os.Getenv(rawHelperUntilEnv)

	// The recording is written ONLY when a terminating condition was actually
	// met. A transient read error on the pty must never be published as a
	// complete-but-empty recording: that turns a harness hiccup into a false
	// "the markers are missing" failure, which is the opposite of what this
	// suite is for. An abort writes a diagnostic beside it instead.
	var got bytes.Buffer
	buf := make([]byte, 4096)
	done := ""
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		n, rerr := os.Stdin.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
		b := got.Bytes()
		switch {
		case until != "":
			if bytes.Contains(b, []byte(until)) {
				done = "sentinel"
			}
		case bytes.Contains(b, []byte("NEGATIVE-CONTROL-DONE")):
			// The unbracketed control sends no markers; it ends on its own
			// explicit sentinel.
			done = "negative-control"
		default:
			// Done when the paste has closed AND the submitting Enter has
			// landed after it. Both are definite events, so this never waits
			// on time.
			if i := bytes.Index(b, []byte(pasteEndMarker)); i >= 0 {
				if bytes.ContainsAny(b[i+len(pasteEndMarker):], "\r\n") {
					done = "paste-complete"
				}
			}
		}
		if done != "" {
			break
		}
		if rerr != nil && n == 0 {
			// A pty read can fail transiently before the writer side is
			// settled. Back off briefly rather than declaring the stream over;
			// the deadline is what ends this loop, and it ends it as an abort.
			time.Sleep(10 * time.Millisecond)
		}
	}
	if done == "" {
		_ = os.WriteFile(out+".aborted",
			[]byte(fmt.Sprintf("no terminating condition after %d bytes: %q", got.Len(), got.String())), 0o600)
		return
	}
	_ = os.WriteFile(out, got.Bytes(), 0o600)
}

// startRawConsumer launches the raw-mode consumer inside a real tmux pane and
// returns the path its recording will be written to, once it is ready.
func startRawConsumer(t *testing.T, r *Runtime, id string) (ports.RuntimeHandle, string) {
	t.Helper()
	return startRawConsumerUntil(t, r, id, "")
}

func startRawConsumerUntil(t *testing.T, r *Runtime, id, until string) (ports.RuntimeHandle, string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "received.bin")

	env := map[string]string{rawHelperEnv: "1", rawHelperOutEnv: out}
	if until != "" {
		env[rawHelperUntilEnv] = until
	}
	h, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     domain.SessionID(id),
		WorkspacePath: dir,
		Argv:          []string{os.Args[0], "-test.run=TestRawPasteConsumerHelper", "--"},
		Env:           env,
		Owner:         "ao-worker:" + id,
	})
	if err != nil {
		t.Fatalf("Create raw-mode consumer pane: %v", err)
	}
	waitForFile(t, out+".ready", 30*time.Second, "the raw-mode consumer never reached raw mode")
	return h, out
}

// waitForFile polls for a file the helper creates to announce a state change.
// It is a condition wait, not a sleep: the deadline exists only to fail the
// test rather than hang it.
func waitForFile(t *testing.T, path string, within time.Duration, msg string) []byte {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			return b
		}
		// The consumer publishes a diagnostic rather than a truncated recording
		// when it never reached a terminating condition. Surface it: an aborted
		// harness must read as a harness failure, never as a transport defect.
		if b, err := os.ReadFile(path + ".aborted"); err == nil {
			t.Fatalf("%s: the raw-mode consumer aborted: %s", msg, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s (%s did not appear within %s)", msg, path, within)
	return nil
}

// largeMultilinePrompt builds a >4 KB multiline prompt shaped like the real
// thing: a beginning marker, filler, the REVIEWER FINDINGS in the middle, more
// filler, and an end marker. The middle is what the incident destroyed.
func largeMultilinePrompt(kind string) (prompt, begin, middle, end string) {
	begin = "<<" + kind + "-BEGIN-3f9a1c>>"
	middle = "<<" + kind + "-FINDINGS-7b2e>>"
	end = "<<" + kind + "-END-c41d0e>>"

	findings := strings.Join([]string{
		middle,
		"1. internal/workflow/verify.go:412 — verify authorised by a review generation that did not review this HEAD.",
		"2. internal/workflow/fix_dispatch.go:88 — the fix prompt is built before the generation is claimed.",
		"3. quoting torture: \"double\" 'single' `backtick` $(rm -rf /) ${HOME} | && ; > < \\n café 世界 🚀 — em dash\ttab",
		"4. a line ending in a backslash \\",
		"5. a line with trailing spaces   ",
		middle + "-END",
	}, "\n")

	var b strings.Builder
	b.WriteString(begin + "\n")
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&b, "context line %03d: the worker must apply every finding below, in order.\n", i)
	}
	b.WriteString(findings + "\n")
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&b, "trailing line %03d: do not stop until all findings are addressed.\n", i)
	}
	b.WriteString(end)
	return b.String(), begin, middle, end
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// assertOnePastePreservedTheWholePrompt is the whole of section D's matrix,
// applied to one recorded byte stream.
func assertOnePastePreservedTheWholePrompt(t *testing.T, received, prompt, begin, middle, end string) {
	t.Helper()

	if strings.HasPrefix(received, "RAWMODE-FAILED") {
		t.Fatalf("the consumer never entered raw mode, so this test could only have passed through a cooked-mode path: %s", received)
	}
	// D8 — this cannot have passed via a shell cooked-mode path. A cooked tty
	// never delivers DECSET-gated bracketed markers, and tmux only emits them
	// because the consumer requested them from raw mode.
	if !strings.Contains(received, pasteStartMarker) || !strings.Contains(received, pasteEndMarker) {
		t.Fatalf("D7: bracketed-paste markers missing from the received stream (%d bytes); "+
			"without them a raw-mode TUI reads every CR as Enter", len(received))
	}

	// D1/D2 — ONE logical paste: exactly one open marker and one close marker,
	// in that order.
	if n := strings.Count(received, pasteStartMarker); n != 1 {
		t.Fatalf("D1: %d paste-start markers, want exactly 1 — the prompt did not arrive as one logical paste", n)
	}
	if n := strings.Count(received, pasteEndMarker); n != 1 {
		t.Fatalf("D1: %d paste-end markers, want exactly 1", n)
	}
	start := strings.Index(received, pasteStartMarker)
	stop := strings.Index(received, pasteEndMarker)
	if stop < start {
		t.Fatal("D1: the paste closed before it opened")
	}
	body := received[start+len(pasteStartMarker) : stop]

	// tmux carries a paste's line breaks as CR, exactly as a real terminal paste
	// does. Inside the brackets those CRs are literal text, which is the entire
	// point of the markers. Normalising them back is the documented, correct
	// reading of the stream — not a fudge.
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	// D6 — receipt digest matches the intended payload, byte for byte.
	if normalized != prompt {
		t.Fatalf("D6: payload mismatch.\n  intended %d bytes sha256=%s\n  received %d bytes sha256=%s\n  first divergence at %d",
			len(prompt), digest(prompt), len(normalized), digest(normalized), firstDivergence(prompt, normalized))
	}

	// D4 — beginning, middle and end markers all survived, in order.
	bi, mi, ei := strings.Index(normalized, begin), strings.Index(normalized, middle), strings.Index(normalized, end)
	if bi < 0 || mi < 0 || ei < 0 {
		t.Fatalf("D4: markers lost (begin=%d middle=%d end=%d)", bi, mi, ei)
	}
	if bi >= mi || mi >= ei {
		t.Fatalf("D4: markers out of order (begin=%d middle=%d end=%d)", bi, mi, ei)
	}

	// D3 — the reviewer findings in the MIDDLE survived byte for byte. This is
	// the exact text the incident dropped.
	fstart := strings.Index(normalized, middle)
	fend := strings.Index(normalized, middle+"-END")
	wantStart := strings.Index(prompt, middle)
	wantEnd := strings.Index(prompt, middle+"-END")
	if fstart < 0 || fend < 0 || normalized[fstart:fend] != prompt[wantStart:wantEnd] {
		t.Fatal("D3: the findings block in the middle of the prompt did not survive")
	}

	// D5 — no line became a separate submission. Every CR the consumer saw is
	// INSIDE the bracketed region; a raw-mode TUI submits on the ones outside.
	outside := received[:start] + received[stop+len(pasteEndMarker):]
	// The single trailing Enter that submits the whole prompt is expected and
	// is the only submit in the stream.
	submits := strings.Count(outside, "\r") + strings.Count(outside, "\n")
	if submits != 1 {
		t.Fatalf("D5: %d Enter presses outside the bracketed paste, want exactly 1 (the submit). "+
			"More than one means the prompt was submitted line by line", submits)
	}
	// And the payload really did contain many line breaks, so the check above
	// is not vacuous.
	if inner := strings.Count(body, "\r") + strings.Count(body, "\n"); inner < 100 {
		t.Fatalf("D5: only %d line breaks inside the paste; this prompt is not multiline enough to prove anything", inner)
	}

	// The >4 KB bar, restated on the bytes that actually travelled.
	if len(prompt) <= 4096 {
		t.Fatalf("this prompt is %d bytes; the matrix requires >4 KB", len(prompt))
	}
	t.Logf("delivered %d bytes as one bracketed paste; sha256=%s; %d line breaks inside the brackets",
		len(prompt), digest(prompt), strings.Count(body, "\r")+strings.Count(body, "\n"))
}

func firstDivergence(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// D1 + D3..D8 for the WORKER prompt.
func TestRealTmux_LargeWorkerPromptArrivesAsOneBracketedPaste(t *testing.T) {
	requireRealTmux(t)
	r, _ := realTmuxServer(t)

	h, out := startRawConsumer(t, r, "p0c-rawworker")
	prompt, begin, middle, end := largeMultilinePrompt("WORKER")

	if err := r.SendMessage(context.Background(), h, prompt); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	received := waitForFile(t, out, 60*time.Second, "the raw-mode consumer never recorded a completed paste")
	assertOnePastePreservedTheWholePrompt(t, string(received), prompt, begin, middle, end)
}

// D2 + D3..D8 for the FIX prompt — the exact shape of the incident: reviewer
// findings carried in the middle of a >4 KB multiline fix prompt.
func TestRealTmux_LargeFixPromptArrivesAsOneBracketedPaste(t *testing.T) {
	requireRealTmux(t)
	r, _ := realTmuxServer(t)

	h, out := startRawConsumer(t, r, "p0c-rawfix")
	prompt, begin, middle, end := largeMultilinePrompt("FIX")

	if err := r.SendMessage(context.Background(), h, prompt); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	received := waitForFile(t, out, 60*time.Second, "the raw-mode consumer never recorded a completed paste")
	assertOnePastePreservedTheWholePrompt(t, string(received), prompt, begin, middle, end)
}

// THE NEGATIVE CONTROL, and the reason the two tests above mean anything.
//
// The same real tmux, the same real raw-mode consumer, the same >4 KB multiline
// prompt — pasted WITHOUT -p, exactly as the code did before the fix. The
// stream must arrive unbracketed and full of bare CRs: in a raw-mode TUI each
// one of those is Enter, which is the shredding the incident suffered.
//
// If this ever stops reproducing, the harness has stopped modelling the
// terminal semantics and the positive tests above are worthless.
func TestRealTmux_PasteWithoutBracketsShredsThePromptIntoSubmissions(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)

	h, out := startRawConsumer(t, r, "p0c-rawnoctl")
	prompt, _, _, _ := largeMultilinePrompt("CONTROL")

	// Stage the payload and paste it the OLD way: no -p, so tmux inserts no
	// markers and translates every LF to CR.
	file := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(file, []byte(prompt+"\nNEGATIVE-CONTROL-DONE\n"), 0o600); err != nil {
		t.Fatalf("stage payload: %v", err)
	}
	// Address the exact incarnation, so the control can never reach anything else.
	target := h.InstanceID
	if o, err := exec.Command("tmux", "-L", socket, "load-buffer", "-b", "p0c-ctl", file).CombinedOutput(); err != nil {
		t.Fatalf("load-buffer: %v: %s", err, o)
	}
	if o, err := exec.Command("tmux", "-L", socket, "paste-buffer", "-d", "-b", "p0c-ctl", "-t", target).CombinedOutput(); err != nil {
		t.Fatalf("paste-buffer (no -p): %v: %s", err, o)
	}
	received := string(waitForFile(t, out, 60*time.Second, "the negative control never completed"))
	if strings.HasPrefix(received, "RAWMODE-FAILED") {
		t.Fatalf("the consumer never entered raw mode: %s", received)
	}
	if strings.Contains(received, pasteStartMarker) || strings.Contains(received, pasteEndMarker) {
		t.Fatal("a paste without -p produced bracketed markers; the negative control is not modelling the defect")
	}
	// This is the defect, made visible: hundreds of bare Enters, every one of
	// which a raw-mode TUI submits.
	submits := strings.Count(received, "\r") + strings.Count(received, "\n")
	if submits < 100 {
		t.Fatalf("the unbracketed paste produced only %d bare Enters; the defect did not reproduce, "+
			"so the bracketed-paste tests above prove nothing", submits)
	}
	t.Logf("negative control reproduced the defect: %d bare Enter presses, 0 bracketed-paste markers", submits)
}

// P0-C SECTION E (transport half) — ONE FIX GENERATION, ONE REAL DELIVERY,
// ACROSS A RESTART.
//
// The generation half of section E is proven in internal/workflow (a fix
// generation is claimed durably, a stale one cannot send, a crash after Send
// before ack adopts the same delivery rather than issuing a second). Those
// tests drive a fake sender, so what they cannot show is what actually reached
// the pane. This is the other half, on the real wire: the bytes one generation
// put there, and the absence of the bytes a second delivery would have put
// there after a daemon restart.
//
// The sentinel is what makes the negative claim provable rather than a wait.
// tmux delivers to a single pane in order, so a small message sent AFTER the
// simulated restart has finished reconciling establishes a happens-before edge:
// anything a duplicate delivery would have written must already be in the
// recording by the time the sentinel appears.
func TestRealTmux_OneFixGenerationDeliversExactlyOnePasteAcrossARestart(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)

	const sentinel = "P0C-FIX-SENTINEL-9d2c"
	h, out := startRawConsumerUntil(t, r, "p0c-fixgen", sentinel)
	prompt, begin, middle, end := largeMultilinePrompt("FIXGEN")

	// The one delivery this generation is entitled to.
	if err := r.SendMessage(context.Background(), h, prompt); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// "Daemon restart": a brand-new Runtime built from nothing but the socket,
	// exactly as a restarted daemon does. It re-observes the session and adopts
	// it. Recovery must NOT re-send: the delivery is already proven.
	restarted := New(Options{Timeout: 10 * time.Second, Socket: socket, ScratchDir: t.TempDir()})
	facts, exists, err := restarted.SessionFacts(context.Background(), h)
	if err != nil || !exists {
		t.Fatalf("the restarted runtime did not recover the worker session: (%+v, %t, %v)", facts, exists, err)
	}
	if facts.InstanceID != h.InstanceID {
		t.Fatalf("recovered a different incarnation: %q != %q", facts.InstanceID, h.InstanceID)
	}

	// The happens-before edge.
	if err := restarted.SendMessage(context.Background(), h, sentinel); err != nil {
		t.Fatalf("SendMessage(sentinel): %v", err)
	}
	received := string(waitForFile(t, out, 60*time.Second, "the consumer never saw the post-restart sentinel"))

	if strings.HasPrefix(received, "RAWMODE-FAILED") {
		t.Fatalf("the consumer never entered raw mode: %s", received)
	}
	si := strings.Index(received, sentinel)
	if si < 0 {
		t.Fatal("the sentinel never arrived, so nothing here is ordered")
	}
	before := received[:si]

	// EXACTLY ONE delivery reached the pane. A duplicate would be a second
	// bracketed paste, and it would necessarily precede the sentinel.
	if n := strings.Count(before, pasteStartMarker); n != 1 {
		t.Fatalf("%d bracketed pastes reached the pane before the sentinel, want exactly 1: "+
			"the restart re-delivered a fix prompt the worker had already been given", n)
	}
	assertOnePastePreservedTheWholePrompt(t, before, prompt, begin, middle, end)
	t.Logf("one fix generation delivered %d bytes as one bracketed paste; the restart added none", len(prompt))
}
