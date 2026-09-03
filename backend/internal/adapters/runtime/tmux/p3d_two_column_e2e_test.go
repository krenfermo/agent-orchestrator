package tmux

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	claudecodeq "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// P3-D §18 — the two-column select, and the half-drawn one, against a real
// terminal.
//
// The unit fixtures prove the parser reads a captured pane correctly. These
// prove the whole loop works on a pane a real terminal actually painted:
// tmux's own rendering, its own line wrapping, its own escape handling. The
// incident this closes was invisible to every string-based test in the tree
// because it lived in the columns, and columns only exist once something has
// drawn them.

// AO selects option 2 in a two-column layout, and the real program records
// option 2. The labels it navigates by are the clean ones — a plan built from
// "pathhelpers.go   │ func Join(a, b string) {}" matches nothing.
func TestRealTmuxTwoColumnSelectConfirmsTheIntendedOption(t *testing.T) {
	r, h, outPath := selectTUISessionLayout(t, "two-column", "pathhelpers.go")

	dialog, ok := observeDialog(t, r, h)
	if !ok {
		t.Fatal("the real two-column pane produced no dialog")
	}
	if got := dialog.Options[1].Label; got != "pathhelpers.go" {
		t.Fatalf("option 2 label = %q, want a label with none of the preview column in it", got)
	}
	if sel, has := dialog.SelectedOption(); !has || sel.ID != "1" {
		t.Fatalf("cursor = %+v, want option 1", sel)
	}

	plan, refusal := (claudecodeq.DialogResponder{}).PlanDialogResponse(dialog,
		domain.StructuredProviderResponse{OptionID: "2", OptionLabel: "pathhelpers.go"})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q", refusal)
	}
	runPlan(t, r, h, plan)

	if got := readSelected(t, outPath); got != "2" {
		t.Fatalf("the program recorded option %q, want 2", got)
	}
}

// The last option, because an off-by-one that survives a move of one row does
// not survive a move of three.
func TestRealTmuxTwoColumnSelectReachesTheLastOption(t *testing.T) {
	r, h, outPath := selectTUISessionLayout(t, "two-column", "pathhelpers.go")

	dialog, ok := observeDialog(t, r, h)
	if !ok {
		t.Fatal("the real two-column pane produced no dialog")
	}
	plan, refusal := (claudecodeq.DialogResponder{}).PlanDialogResponse(dialog,
		domain.StructuredProviderResponse{OptionID: "4", OptionLabel: "Chat about this"})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q", refusal)
	}
	runPlan(t, r, h, plan)

	if got := readSelected(t, outPath); got != "4" {
		t.Fatalf("the program recorded option %q, want 4", got)
	}
}

// A pane caught half-drawn is INCONCLUSIVE, and nothing is pressed.
//
// This is the property the whole three-state model exists for: the old parser
// reported such a screen as "no dialog", the caller read that as "the prompt is
// gone", and an answer was recorded as delivered to an agent still sitting on
// its question. Here the fixture never finishes rendering, so the observation
// can never resolve — and AO must simply not act.
func TestRealTmuxPartialRedrawIsInconclusiveAndPressesNothing(t *testing.T) {
	r, h, outPath := selectTUISessionLayout(t, "partial", "pathutil.go")

	out, err := r.GetVisibleOutput(context.Background(), h, 60)
	if err != nil {
		t.Fatalf("GetVisibleOutput: %v", err)
	}
	obs := (claudecodeq.DialogParser{}).ObserveDialog(out)
	if !obs.Inconclusive() {
		t.Fatalf("state = %q (%s), want inconclusive for a half-drawn prompt", obs.State, obs.Reason)
	}
	// And nothing was confirmed, because nothing was ever pressed: the fixture
	// only writes the file when it receives Enter.
	if got := readSelectedIfAny(t, outPath); got != "" {
		t.Fatalf("the program recorded option %q from a pane AO could not read", got)
	}
}

// runPlan executes a response plan against the real pane, including the
// re-observation the plan requires before its confirm.
func runPlan(t *testing.T, r *Runtime, h ports.RuntimeHandle, plan ports.DialogResponsePlan) {
	t.Helper()
	ctx := context.Background()
	for _, action := range plan.Actions {
		switch action.Kind {
		case ports.DialogActionKey:
			if err := r.SendKeys(ctx, h, []ports.DialogKey{action.Key}); err != nil {
				t.Fatalf("SendKeys(%q): %v", action.Key, err)
			}
		case ports.DialogActionObserve:
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
}

// readSelectedIfAny is readSelected for a file that is expected NOT to exist:
// the fixture writes it only when it receives Enter, so its absence is the
// assertion that nothing was pressed.
func readSelectedIfAny(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
