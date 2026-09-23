package tmux

// Inventory under a non-UTF-8 locale, against a REAL tmux server.
//
// Found by the Frente 1 workflow-cycle E2E (backend/e2e/workflowcycle): a
// daemon started with no locale variables -- the ordinary environment of one
// started by launchd or by the desktop app -- listed ZERO sessions while a
// finished reviewer's pane was alive on its server. tmux sanitizes control
// characters in format output unless the client runs under a UTF-8 locale, so
// the tab ListSessions split on came back as `_`, every line was skipped as
// unparseable, and runtime GC's inventory pass was blind. Every existing
// real-tmux test inherited the developer's UTF-8 locale and could not see it.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// forceCLocale runs every tmux client this test starts -- the adapter's and
// the raw ones below -- under the C locale. LC_ALL outranks LANG and LC_CTYPE,
// so whatever the developer's environment holds cannot mask the defect.
func forceCLocale(t *testing.T) {
	t.Helper()
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C")
	t.Setenv("LC_CTYPE", "C")
}

func TestRealTmuxInventoryListsSessionsUnderCLocale(t *testing.T) {
	requireRealTmux(t)
	forceCLocale(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	instance := createOwnedSession(t, r, "ao-reviewer-locale-1", "ao-reviewer:rv-locale-1")

	// Negative control: under this same locale, a tab-separated format really
	// does lose its tab. If this ever stops reproducing, the environment no
	// longer models the defect and the assertion below proves nothing.
	raw, err := exec.Command("tmux", "-L", socket, "list-sessions", "-F", "#{session_id}\t#{session_name}").CombinedOutput()
	if err != nil {
		t.Fatalf("raw list-sessions: %v: %s", err, raw)
	}
	if strings.Contains(string(raw), "\t") {
		t.Fatalf("negative control did not reproduce: tmux kept the tab under LC_ALL=C (%q); this test no longer models the defect", raw)
	}

	sessions, err := r.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("inventory under the C locale found %d sessions, want 1: %+v (raw tmux output %q)", len(sessions), sessions, raw)
	}
	got := sessions[0]
	if got.ID != "ao-reviewer-locale-1" || got.InstanceID != instance {
		t.Fatalf("inventory = %+v, want name ao-reviewer-locale-1 at incarnation %s", got, instance)
	}
	if !got.OwnerKnown || got.Owner != "ao-reviewer:rv-locale-1" {
		t.Fatalf("inventory owner = (%q, known=%v), want the ownership token", got.Owner, got.OwnerKnown)
	}
}

// A name tmux accepts may itself contain a space. Splitting at the FIRST space
// keeps it whole, because an incarnation id never contains one.
func TestRealTmuxInventoryKeepsANameWithSpacesWhole(t *testing.T) {
	requireRealTmux(t)
	forceCLocale(t)
	r, socket := realTmuxServer(t)

	createStrangerSession(t, socket, "a name with spaces")
	sessions, err := r.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "a name with spaces" || !strings.HasPrefix(sessions[0].InstanceID, "$") {
		t.Fatalf("inventory = %+v, want one session named %q with a `$N` incarnation", sessions, "a name with spaces")
	}
}
