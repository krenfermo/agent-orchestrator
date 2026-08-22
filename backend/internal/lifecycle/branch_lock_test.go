package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Checkpoint 8P-E.14A. Direct-branch execution ownership is turn-scoped for an
// ordinary task, and these tests pin the boundaries it moves on.
//
// The incident being fixed: a task session's lock was released only by
// MarkTerminated, and an ordinary task that finishes successfully is never
// terminated — it goes idle and stays alive so the user can keep talking to it.
// The branch stayed occupied forever and every later task on it failed with
// BRANCH_IN_USE.

type recordedAcquire struct {
	projectID domain.ProjectID
	sessionID domain.SessionID
}

type recordedRelease struct {
	sessionID string
	reason    string
}

type fakeBranchLocks struct {
	mu         sync.Mutex
	acquires   []recordedAcquire
	releases   []recordedRelease
	acquireErr error
}

func (f *fakeBranchLocks) AcquireForSession(_ context.Context, projectID domain.ProjectID, sessionID domain.SessionID) ([]domain.BranchLock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires = append(f.acquires, recordedAcquire{projectID: projectID, sessionID: sessionID})
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	return []domain.BranchLock{{ID: "blk-1", SessionID: string(sessionID)}}, nil
}

func (f *fakeBranchLocks) ReleaseSession(_ context.Context, sessionID, reason string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, recordedRelease{sessionID: sessionID, reason: reason})
	return 1, nil
}

func (f *fakeBranchLocks) snapshot() ([]recordedAcquire, []recordedRelease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedAcquire(nil), f.acquires...), append([]recordedRelease(nil), f.releases...)
}

func newManagerWithBranchLocks(t *testing.T) (*Manager, *fakeStore, *fakeBranchLocks) {
	t.Helper()
	m, st, _ := newManager()
	locks := &fakeBranchLocks{}
	m.SetBranchLocks(locks)
	return m, st, locks
}

// The regression for the incident, at the reducer that owns the fact: the Stop
// hook of a finished turn hands the branch back, without the session being
// terminated and without anyone having to kill it.
func TestTurnEndReleasesTheSessionBranchLock(t *testing.T) {
	m, st, locks := newManagerWithBranchLocks(t)
	rec := working("mer-1")
	rec.FirstSignalAt = time.Now().Add(-time.Minute)
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}

	_, releases := locks.snapshot()
	if len(releases) != 1 || releases[0].sessionID != "mer-1" {
		t.Fatalf("releases = %+v, want the finished task's own lock released", releases)
	}
	if got := st.sessions["mer-1"]; got.IsTerminated {
		t.Fatal("the session was terminated; a finished task stays alive and only gives the branch back")
	}
}

// Every way a turn can end frees the branch, not only the happy one: an agent
// that exits, a killed process, and a stopped chat controller all leave no turn
// in flight.
func TestEveryTurnEndingEventReleasesTheLock(t *testing.T) {
	for _, tc := range []struct {
		event string
		state domain.ActivityState
	}{
		{"stop", domain.ActivityIdle},
		{"session-end", domain.ActivityExited},
		{"process-exited", domain.ActivityExited},
		{"chat.controller.stopped", domain.ActivityIdle},
	} {
		t.Run(tc.event, func(t *testing.T) {
			m, st, locks := newManagerWithBranchLocks(t)
			rec := working("mer-1")
			rec.FirstSignalAt = time.Now().Add(-time.Minute)
			st.sessions["mer-1"] = rec

			if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
				Valid: true, State: tc.state, Event: tc.event,
			}); err != nil {
				t.Fatalf("ApplyActivitySignal: %v", err)
			}
			if _, releases := locks.snapshot(); len(releases) != 1 {
				t.Fatalf("releases = %+v, want 1 for %s", releases, tc.event)
			}
		})
	}
}

// A repeated Stop on an already-idle row is dropped by the state reducer (there
// is nothing to write), but the branch must still come back: the turn ended.
// Without this, a task whose "active" hook was lost holds its branch forever.
func TestRepeatedTurnEndOnAnAlreadyIdleSessionStillReleases(t *testing.T) {
	m, st, locks := newManagerWithBranchLocks(t)
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()}
	rec.FirstSignalAt = time.Now().Add(-time.Minute)
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if _, releases := locks.snapshot(); len(releases) != 1 {
		t.Fatalf("releases = %+v, want the turn end honored even with no state change", releases)
	}
}

// The idle-vs-terminal distinction, from the other side. A task that is merely
// quiet — an untagged idle signal from an old CLI, or a tool-use event mid-turn
// — has NOT finished, and its branch is not up for grabs.
func TestQuietButUnfinishedTaskKeepsItsLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal ports.ActivitySignal
	}{
		{
			// No event tag: an old CLI or an adapter with no turn model. AO
			// cannot tell a finished turn from a quiet one, so it keeps the
			// branch — the fail-safe direction.
			name:   "untagged idle signal",
			signal: ports.ActivitySignal{Valid: true, State: domain.ActivityIdle},
		},
		{
			name:   "waiting on the user inside the turn",
			signal: ports.ActivitySignal{Valid: true, State: domain.ActivityWaitingInput, Event: "notification"},
		},
		{
			name:   "blocked on a permission dialog",
			signal: ports.ActivitySignal{Valid: true, State: domain.ActivityBlocked, Event: "permission-request"},
		},
		{
			name:   "between tool calls",
			signal: ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Event: "post-tool-use"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, st, locks := newManagerWithBranchLocks(t)
			rec := working("mer-1")
			rec.FirstSignalAt = time.Now().Add(-time.Minute)
			st.sessions["mer-1"] = rec

			if err := m.ApplyActivitySignal(ctx, "mer-1", tc.signal); err != nil {
				t.Fatalf("ApplyActivitySignal: %v", err)
			}
			acquires, releases := locks.snapshot()
			if len(releases) != 0 {
				t.Fatalf("releases = %+v, want none: this task has not finished", releases)
			}
			if len(acquires) != 0 {
				t.Fatalf("acquires = %+v, want none: no new turn started", acquires)
			}
		})
	}
}

// The other half of turn-scoping: a new turn takes the branch back. Without
// this, releasing at the end of a turn would leave a live session writing a
// branch it does not own.
func TestTurnStartAcquiresTheSessionBranchLock(t *testing.T) {
	m, st, locks := newManagerWithBranchLocks(t)
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()}
	rec.FirstSignalAt = time.Now().Add(-time.Minute)
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityActive, Event: "user-prompt-submit",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	acquires, releases := locks.snapshot()
	if len(acquires) != 1 || acquires[0].sessionID != "mer-1" || acquires[0].projectID != "mer" {
		t.Fatalf("acquires = %+v, want the new turn to take the project's branch", acquires)
	}
	if len(releases) != 0 {
		t.Fatalf("releases = %+v, want none on a turn start", releases)
	}
}

// A turn start that cannot get the branch is reported, not fatal: the prompt
// was already submitted by the time the hook fires, so there is nothing left to
// refuse. The refusal that protects the branch happens at task start instead.
func TestTurnStartSurvivesALockConflict(t *testing.T) {
	m, st, locks := newManagerWithBranchLocks(t)
	locks.acquireErr = errors.New("branch in use")
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()}
	rec.FirstSignalAt = time.Now().Add(-time.Minute)
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityActive, Event: "user-prompt-submit",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if got := st.sessions["mer-1"].Activity.State; got != domain.ActivityActive {
		t.Fatalf("activity = %q, want the turn recorded regardless of the lock outcome", got)
	}
}

// Termination still releases, by whichever route a task ends: killed,
// failed-to-spawn, reaped, or terminated after its PR merged.
func TestMarkTerminatedStillReleasesTheSessionBranchLock(t *testing.T) {
	m, st, locks := newManagerWithBranchLocks(t)
	st.sessions["mer-1"] = working("mer-1")

	if err := m.MarkTerminated(ctx, "mer-1"); err != nil {
		t.Fatalf("MarkTerminated: %v", err)
	}
	_, releases := locks.snapshot()
	if len(releases) != 1 || releases[0].sessionID != "mer-1" {
		t.Fatalf("releases = %+v, want the terminated task's lock released", releases)
	}
}

// A signal from a stale launch belongs to a process AO already replaced. It
// must not move the current turn's branch ownership in either direction.
func TestStaleLaunchSignalDoesNotMoveTheLock(t *testing.T) {
	m, st, locks := newManagerWithBranchLocks(t)
	rec := working("mer-1")
	rec.Metadata.RuntimeLaunchID = "launch-2"
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop", LaunchID: "launch-1",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if acquires, releases := locks.snapshot(); len(releases) != 0 || len(acquires) != 0 {
		t.Fatalf("acquires = %+v releases = %+v, want a stale launch ignored", acquires, releases)
	}
}
