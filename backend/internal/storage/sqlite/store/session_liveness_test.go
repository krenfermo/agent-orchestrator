package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The liveness clock has to survive the process, or it answers nothing: the
// question it exists for ("has this worker gone silent?") is asked by a daemon
// that may have restarted since the signal arrived. This is the round trip
// through every write path a session takes.
func TestSessionLivenessSurvivesEveryWritePath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "liveness")

	entered := time.Now().UTC().Truncate(time.Second).Add(-20 * time.Minute)
	heard := entered.Add(19 * time.Minute)

	rec := sampleRecord("liveness")
	rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: entered, LastSignalAt: heard}
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. Insert.
	got, ok, err := s.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("get after insert: ok=%v err=%v", ok, err)
	}
	assertClocks(t, "after insert", got.Activity, entered, heard)

	// 2. The narrow activity-signal write, which is the hot path: a same-state
	//    repeat that advances liveness and must leave the transition alone.
	later := heard.Add(time.Minute)
	got.Activity.LastSignalAt = later
	got.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	applied, err := s.UpdateSessionFromActivitySignal(ctx, got)
	if err != nil || !applied {
		t.Fatalf("activity-signal write: applied=%v err=%v", applied, err)
	}
	reread, _, err := s.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after signal write: %v", err)
	}
	assertClocks(t, "after activity-signal write", reread.Activity, entered, later)

	// 3. The full update.
	latest := later.Add(time.Minute)
	reread.Activity.LastSignalAt = latest
	if err := s.UpdateSession(ctx, reread); err != nil {
		t.Fatalf("update: %v", err)
	}
	final, _, err := s.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	assertClocks(t, "after full update", final.Activity, entered, latest)

	// 4. The list paths the Board reads.
	listed, err := s.ListSessions(ctx, "liveness")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list by project: n=%d err=%v", len(listed), err)
	}
	assertClocks(t, "list by project", listed[0].Activity, entered, latest)

	all, err := s.ListAllSessions(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("list all: n=%d err=%v", len(all), err)
	}
	assertClocks(t, "list all", all[0].Activity, entered, latest)
}

// A caller that knows only about the transition clock — spawn, restore, the
// reaper — must not be able to leave behind a row that reads as never heard
// from, because that is the value the corroboration window ages into a stop.
func TestSessionLivenessIsSeededWhenOnlyTheTransitionIsWritten(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "seeded")

	at := time.Now().UTC().Truncate(time.Second)
	rec := sampleRecord("seeded")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: at}
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, ok, err := s.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Activity.LastSignalAt.IsZero() {
		t.Fatalf("liveness left unset by a transition-only write: %+v", got.Activity)
	}
	if !got.Activity.Liveness().Equal(at) {
		t.Fatalf("Liveness() = %v, want the transition clock %v", got.Activity.Liveness(), at)
	}

	// The respawn shape. MarkSpawned replaces the whole Activity struct with a
	// fresh {idle, now}, which zeroes the liveness clock — correct, because the
	// previous launch's liveness says nothing about this one. What must not
	// happen is the row being left at "never heard from": AO just started this
	// process, so the spawn instant is the honest answer.
	respawned := got
	respawnedAt := at.Add(time.Hour)
	respawned.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: respawnedAt}
	respawned.UpdatedAt = respawnedAt
	if err := s.UpdateSession(ctx, respawned); err != nil {
		t.Fatalf("respawn update: %v", err)
	}
	after, _, err := s.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after respawn: %v", err)
	}
	if !after.Activity.Liveness().Equal(respawnedAt) {
		t.Fatalf("after respawn Liveness() = %v, want the spawn instant %v", after.Activity.Liveness(), respawnedAt)
	}
}

func assertClocks(t *testing.T, when string, got domain.Activity, wantTransition, wantLiveness time.Time) {
	t.Helper()
	if !got.LastActivityAt.UTC().Equal(wantTransition) {
		t.Fatalf("%s: transition clock = %v, want %v", when, got.LastActivityAt.UTC(), wantTransition)
	}
	if !got.LastSignalAt.UTC().Equal(wantLiveness) {
		t.Fatalf("%s: liveness clock = %v, want %v", when, got.LastSignalAt.UTC(), wantLiveness)
	}
}
