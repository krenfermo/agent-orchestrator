package sessionmanager

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// P9 — independent review finding 1. "Resume agent" respawns the pane of a live
// session through Runtime.Restart with a handle built from the row, which names
// only the session; tmux hands that handle back without an incarnation. Writing
// it verbatim erased the recorded `$N`, and every later ownership proof then
// failed closed as provenance_missing on a worker AO had just relaunched itself.
// The recorded incarnation must survive, beside the NEW launch and token.
func TestP9ResumeThroughRestartKeepsTheRecordedIncarnation(t *testing.T) {
	runtime := &fakeRestartRuntime{fakeRuntime: &fakeRuntime{}}
	manager, store, _ := newSwitchTestManager(t, runtime)
	rec := store.sessions["proj-1"]
	rec.Metadata.RuntimeInstanceID = "$7"
	rec.Metadata.RuntimeOwnerToken = domain.SessionRuntimeOwnerToken(rec.ID, rec.Metadata.RuntimeLaunchID)
	store.sessions[rec.ID] = rec

	if _, err := manager.ResumeAgentWithMode(context.Background(), rec.ID); err != nil {
		t.Fatalf("ResumeAgentWithMode: %v", err)
	}
	if runtime.restarted != 1 {
		t.Fatalf("restarts = %d, want the live pane respawned once", runtime.restarted)
	}
	got := store.sessions[rec.ID].Metadata
	if got.RuntimeInstanceID != "$7" {
		t.Fatalf("runtime incarnation after resume = %q, want the recorded $7 kept", got.RuntimeInstanceID)
	}
	if got.RuntimeLaunchID == rec.Metadata.RuntimeLaunchID || got.RuntimeLaunchID == "" {
		t.Fatalf("launch id = %q, want a NEW launch", got.RuntimeLaunchID)
	}
	if got.RuntimeOwnerToken != domain.SessionRuntimeOwnerToken(rec.ID, got.RuntimeLaunchID) {
		t.Fatalf("owner token %q does not bind the new launch %q", got.RuntimeOwnerToken, got.RuntimeLaunchID)
	}
	if runtime.lastCfg.Owner != got.RuntimeOwnerToken {
		t.Fatalf("the runtime was restamped with %q, the row records %q", runtime.lastCfg.Owner, got.RuntimeOwnerToken)
	}
}
