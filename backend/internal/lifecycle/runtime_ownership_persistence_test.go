package lifecycle

// Regression suite for the Runtime GC production-wiring defect found alongside
// incident wf-170b16ce.
//
// The symptom: 25 tmux sessions on AO's own server, from workflows finished
// hours and days earlier — agent-orchestrator-40 through -50, medusa-3 through
// -9, and their reviewer panes. Runtime GC had been running the whole time
// (boot sweep plus a 15-minute periodic sweep) and had reclaimed none of them.
//
// The cause was here. Session Manager computes the ownership identity correctly
// at spawn — `domain.SessionRuntimeOwnerToken(id, launchID)` attached
// ATOMICALLY to the tmux session via `new-session -e`, and the immutable
// incarnation read back off the creation command itself — and hands both to
// MarkSpawned in the metadata. The SQL writes both columns. mergeMetadata,
// between the two, copied neither. Every one of the 67 session rows in the
// production database carried an empty runtime_instance_id and an empty
// runtime_owner_token, including sessions created minutes earlier by a daemon
// built from the P1-D commit.
//
// runtimegc.Sweeper.sessionCandidates reads exactly those two columns and
// skips any row where either is empty, calling it "pre-P1-D". So P1-D's worker
// and reviewer reclamation was unreachable in production from the day it
// shipped, while its own unit tests passed against SessionRecords built by
// hand — which is precisely the seam these tests cover instead.

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The identity Runtime GC needs must survive the write. This is the exact fact
// whose absence made every finished worker permanently unprovable.
func TestMarkSpawnedPersistsTheRuntimeOwnershipIdentity(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer", IsTerminated: true,
		Activity: domain.Activity{State: domain.ActivityExited},
	}
	token := domain.SessionRuntimeOwnerToken("mer-1", "launch-1")

	if err := m.MarkSpawned(ctx, "mer-1", domain.SessionMetadata{
		RuntimeHandleID:   "mer-1",
		RuntimeLaunchID:   "launch-1",
		RuntimeInstanceID: "$7",
		RuntimeOwnerToken: token,
	}); err != nil {
		t.Fatal(err)
	}

	got := st.sessions["mer-1"].Metadata
	if got.RuntimeInstanceID != "$7" {
		t.Fatalf("runtime instance id = %q, want $7: Runtime GC addresses destroys to the incarnation and skips a row without one", got.RuntimeInstanceID)
	}
	if got.RuntimeOwnerToken != token {
		t.Fatalf("runtime owner token = %q, want %q: without it AO cannot prove the runtime is its own", got.RuntimeOwnerToken, token)
	}
	// And the identity must be internally consistent, because that is the
	// predicate the sweeper actually applies.
	if !domain.RuntimeOwnedBySession(got.RuntimeOwnerToken, "mer-1", got.RuntimeLaunchID) {
		t.Fatal("the persisted token does not prove ownership for the persisted launch")
	}
}

// A relaunch REPLACES the identity. Carrying a previous launch's incarnation
// forward would be worse than dropping it: the sweeper would address a destroy
// to a runtime that no longer belongs to this launch, which is the stale-handle
// adoption P1-D §C exists to prevent.
func TestMarkSpawnedReplacesTheRuntimeIdentityOnRelaunch(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer", IsTerminated: true,
		Activity: domain.Activity{State: domain.ActivityExited},
		Metadata: domain.SessionMetadata{
			RuntimeHandleID:   "mer-1",
			RuntimeLaunchID:   "launch-1",
			RuntimeInstanceID: "$7",
			RuntimeOwnerToken: domain.SessionRuntimeOwnerToken("mer-1", "launch-1"),
		},
	}
	second := domain.SessionRuntimeOwnerToken("mer-1", "launch-2")

	if err := m.MarkSpawned(ctx, "mer-1", domain.SessionMetadata{
		RuntimeHandleID:   "mer-1",
		RuntimeLaunchID:   "launch-2",
		RuntimeInstanceID: "$9",
		RuntimeOwnerToken: second,
	}); err != nil {
		t.Fatal(err)
	}

	got := st.sessions["mer-1"].Metadata
	if got.RuntimeInstanceID != "$9" {
		t.Fatalf("runtime instance id = %q, want $9: the previous launch's incarnation must not survive a relaunch", got.RuntimeInstanceID)
	}
	if got.RuntimeOwnerToken != second {
		t.Fatalf("runtime owner token = %q, want the new launch's token", got.RuntimeOwnerToken)
	}
	if domain.RuntimeOwnedBySession(got.RuntimeOwnerToken, "mer-1", "launch-1") {
		t.Fatal("the persisted token still proves ownership for the superseded launch")
	}
}

// The three runtime identity fields describe ONE launch, so they must move
// together. A merge that kept an old incarnation beside a new launch id would
// produce a row that reads as owned and points at the wrong runtime — the one
// combination that is more dangerous than recording nothing at all.
func TestMarkSpawnedNeverMixesRuntimeIdentitiesAcrossLaunches(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer", IsTerminated: true,
		Activity: domain.Activity{State: domain.ActivityExited},
		Metadata: domain.SessionMetadata{
			RuntimeLaunchID:   "launch-1",
			RuntimeInstanceID: "$7",
			RuntimeOwnerToken: domain.SessionRuntimeOwnerToken("mer-1", "launch-1"),
		},
	}
	// A relaunch that genuinely produced no runtime identity (a spawn that
	// failed before Create returned) must not inherit the old one.
	if err := m.MarkSpawned(ctx, "mer-1", domain.SessionMetadata{RuntimeLaunchID: "launch-2"}); err != nil {
		t.Fatal(err)
	}

	got := st.sessions["mer-1"].Metadata
	if got.RuntimeInstanceID != "" || got.RuntimeOwnerToken != "" {
		t.Fatalf("stale identity survived a launch that recorded none: instance=%q token=%q",
			got.RuntimeInstanceID, got.RuntimeOwnerToken)
	}
	// Unprovable is the correct answer here, and the sweeper's fail-closed
	// reading of it is what keeps this safe rather than merely tidy.
	if domain.RuntimeOwnedBySession(got.RuntimeOwnerToken, "mer-1", got.RuntimeLaunchID) {
		t.Fatal("an empty token proved ownership")
	}
}
