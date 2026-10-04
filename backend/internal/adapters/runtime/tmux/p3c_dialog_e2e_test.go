package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	claudecodeq "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// p3c_dialog_e2e_test.go — P3-C §21: structured dialog response against a REAL
// terminal.
//
// A fake runtime cannot prove this capability. The failure it prevents is
// "Enter confirmed a different option than the one chosen", and that failure
// lives in the gap between what AO thinks the cursor is doing and what a real
// terminal actually does with an arrow key. testdata/select_tui.sh is a real
// program reading real bytes from a real pty, with the same semantics as the
// Claude prompt from the incident: Enter confirms the CURSOR, not an intent.
// If AO's navigation is wrong by one row, this test records the wrong number.

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
}

// selectTUISession starts the fake select prompt in a real tmux pane and
// returns the runtime, the handle, and the path the confirmed option lands in.
func selectTUISession(t *testing.T) (*Runtime, ports.RuntimeHandle, string) {
	t.Helper()
	return selectTUISessionLayout(t, "single", "pathhelpers.go")
}

// selectTUISessionLayout is the same fixture with the layout chosen, so the
// shapes P3-D had to survive can be exercised against a real terminal rather
// than against a string in a test (P3-D §18). `ready` is the text whose
// appearance means the pane has finished rendering that layout.
func selectTUISessionLayout(t *testing.T, layout, ready string) (*Runtime, ports.RuntimeHandle, string) {
	t.Helper()
	requireTmux(t)
	ctx := context.Background()
	// A per-run unique name. tmux derives its session name from this, and the
	// fixture blocks forever waiting for a key, so a previous run's leftover
	// would otherwise collide by name for the life of the tmux server.
	id := strings.ReplaceAll(t.Name(), "/", "_") + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	script, err := filepath.Abs("testdata/select_tui.sh")
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "selected")

	r := New(Options{Timeout: 10 * time.Second})
	var created ports.RuntimeHandle
	t.Cleanup(func() {
		if created.ID != "" {
			_ = r.Destroy(context.Background(), created)
		}
	})

	h, err := r.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(id),
		WorkspacePath: t.TempDir(),
		Argv:          []string{"sh", script, outPath, layout},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created = h
	// Wait for the prompt to render before touching it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out, oerr := r.GetOutput(ctx, h, 60); oerr == nil && strings.Contains(out, ready) {
			return r, h, outPath
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("the select prompt never rendered in the pane")
	return nil, ports.RuntimeHandle{}, ""
}

func readSelected(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(150 * time.Millisecond)
	}
	return ""
}

// observeDialog is the read half of delivery, run against the real pane with
// the real Claude parser.
func observeDialog(t *testing.T, r *Runtime, h ports.RuntimeHandle) (domain.ProviderDialog, bool) {
	t.Helper()
	out, err := r.GetVisibleOutput(context.Background(), h, 60)
	if err != nil {
		t.Fatalf("GetVisibleOutput: %v", err)
	}
	obs := (claudecodeq.DialogParser{}).ObserveDialog(out)
	return obs.Dialog, obs.Present()
}

// THE E2E. AO chooses option 2, and the real program records option 2.
func TestRealTmuxStructuredResponseSelectsTheIntendedOption(t *testing.T) {
	r, h, outPath := selectTUISession(t)
	ctx := context.Background()

	dialog, ok := observeDialog(t, r, h)
	if !ok {
		t.Fatal("the real pane produced no dialog")
	}
	if sel, has := dialog.SelectedOption(); !has || sel.ID != "1" {
		t.Fatalf("cursor = %+v, want option 1 — the plan is computed from it", sel)
	}

	plan, refusal := (claudecodeq.DialogResponder{}).PlanDialogResponse(dialog,
		domain.StructuredProviderResponse{OptionID: "2", OptionLabel: "pathhelpers.go"})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q", refusal)
	}

	for _, action := range plan.Actions {
		switch action.Kind {
		case ports.DialogActionKey:
			if err := r.SendKeys(ctx, h, []ports.DialogKey{action.Key}); err != nil {
				t.Fatalf("SendKeys(%q): %v", action.Key, err)
			}
		case ports.DialogActionObserve:
			// The step that makes the confirm safe: re-read the REAL pane and
			// require the cursor to be on the target before pressing Enter.
			landed := false
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if d, ok := observeDialog(t, r, h); ok {
					if sel, has := d.SelectedOption(); has && sel.ID == action.ExpectOptionID {
						landed = true
						break
					}
				}
				time.Sleep(150 * time.Millisecond)
			}
			if !landed {
				t.Fatalf("the cursor never reached option %s in the real pane", action.ExpectOptionID)
			}
		}
	}

	if got := readSelected(t, outPath); got != "2" {
		t.Fatalf("the program recorded option %q, want 2 — AO confirmed the wrong row", got)
	}
}

// The other direction, because an off-by-one that happens to work downward
// would still be wrong upward.
func TestRealTmuxStructuredResponseSelectsTheLastOption(t *testing.T) {
	r, h, outPath := selectTUISession(t)
	ctx := context.Background()

	dialog, ok := observeDialog(t, r, h)
	if !ok {
		t.Fatal("the real pane produced no dialog")
	}
	plan, refusal := (claudecodeq.DialogResponder{}).PlanDialogResponse(dialog,
		domain.StructuredProviderResponse{OptionID: "4", OptionLabel: "Chat about this"})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q", refusal)
	}
	for _, action := range plan.Actions {
		if action.Kind != ports.DialogActionKey {
			continue
		}
		if err := r.SendKeys(ctx, h, []ports.DialogKey{action.Key}); err != nil {
			t.Fatalf("SendKeys: %v", err)
		}
		time.Sleep(120 * time.Millisecond)
	}
	if got := readSelected(t, outPath); got != "4" {
		t.Fatalf("the program recorded option %q, want 4", got)
	}
}

// §25 at the runtime boundary: SendKeys is the ONLY door. A key outside the
// vocabulary is refused rather than forwarded to tmux's key parser, so no
// caller can smuggle a control sequence into a pane through it.
func TestRealTmuxSendKeysRefusesAKeyOutsideTheVocabulary(t *testing.T) {
	r, h, outPath := selectTUISession(t)
	err := r.SendKeys(context.Background(), h, []ports.DialogKey{ports.DialogKey("C-c")})
	if err == nil {
		t.Fatal("an unrecognised key was forwarded to the pane")
	}
	if got := readSelected(t, outPath); got != "" {
		t.Fatalf("the refused key still reached the program (recorded %q)", got)
	}
}

// A dialog answered once is gone, and the same keys pressed again cannot answer
// a NEXT prompt: there is nothing to observe, so delivery refuses.
func TestRealTmuxAnsweredDialogIsGone(t *testing.T) {
	r, h, outPath := selectTUISession(t)
	ctx := context.Background()
	if err := r.SendKeys(ctx, h, []ports.DialogKey{ports.KeyDown, ports.KeyEnter}); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if got := readSelected(t, outPath); got != "2" {
		t.Fatalf("recorded %q, want 2", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := observeDialog(t, r, h); !ok {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("the prompt is still on screen after being answered, so a redelivery could not prove it landed")
}
