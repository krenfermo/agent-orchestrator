package cli

import (
	"strings"
	"testing"
	"time"
)

// `ao session list` prints an age next to every session. It has to be the age of
// the newest SIGNAL, not of the newest state CHANGE.
//
// The two diverge exactly when it matters most. A worker that has been running
// flat out for twenty minutes entered `active` once, at the start, so the
// transition clock reads twenty minutes old for the whole run — and printing
// "(20m)" next to a session that answered four seconds ago tells the operator
// the opposite of the truth. This is the shape run wf-1c2cb9bd had while it was
// making 111 model calls.
func TestSessionLineReportsTheAgeOfTheNewestSignal(t *testing.T) {
	entered := time.Now().Add(-20 * time.Minute)
	sess := sessionDTO{
		ID:     "medusa-12",
		Status: "working",
		Activity: sessionActivity{
			State:          "active",
			LastActivityAt: entered,
			LastSignalAt:   time.Now().Add(-4 * time.Second),
		},
	}
	line := strings.Join(sessionLineParts(sess), " ")
	if strings.Contains(line, "20m") {
		t.Fatalf("line reports the transition age for a session heard from 4s ago: %q", line)
	}
	if !strings.Contains(line, "(4s)") {
		t.Fatalf("line = %q, want the signal age (4s)", line)
	}
}

// A session that really has gone quiet still reports its real age, and a
// response from a daemon that predates the liveness clock keeps falling back to
// the transition one rather than printing nothing.
func TestSessionLineFallsBackAndStillAgesQuietSessions(t *testing.T) {
	quiet := sessionDTO{Activity: sessionActivity{
		State:          "idle",
		LastActivityAt: time.Now().Add(-9 * time.Minute),
		LastSignalAt:   time.Now().Add(-9 * time.Minute),
	}}
	if line := strings.Join(sessionLineParts(quiet), " "); !strings.Contains(line, "9m") {
		t.Fatalf("quiet session line = %q, want a 9m age", line)
	}

	legacy := sessionDTO{Activity: sessionActivity{
		State:          "active",
		LastActivityAt: time.Now().Add(-7 * time.Minute),
	}}
	if line := strings.Join(sessionLineParts(legacy), " "); !strings.Contains(line, "7m") {
		t.Fatalf("legacy session line = %q, want the transition age 7m", line)
	}

	none := sessionDTO{Activity: sessionActivity{State: "idle"}}
	for _, p := range sessionLineParts(none) {
		if strings.HasPrefix(p, "(") {
			t.Fatalf("session with no clocks printed an age: %q", p)
		}
	}
}

// The orchestrator listing shares the rule.
func TestOrchestratorLineReportsTheAgeOfTheNewestSignal(t *testing.T) {
	sess := sessionDTO{Activity: sessionActivity{
		State:          "active",
		LastActivityAt: time.Now().Add(-20 * time.Minute),
		LastSignalAt:   time.Now().Add(-4 * time.Second),
	}}
	line := strings.Join(orchestratorLineParts(sess), " ")
	if !strings.Contains(line, "(4s)") || strings.Contains(line, "20m") {
		t.Fatalf("orchestrator line = %q, want the signal age (4s)", line)
	}
}
