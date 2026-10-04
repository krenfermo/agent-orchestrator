package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The regression this file exists for, in the shape it actually occurred.
//
// Run wf-1c2cb9bd (project medusa, 2026-09-09) dispatched a worker at 17:10:18Z.
// The worker ran continuously for the next twenty minutes: 111 model calls,
// edits across two repositories, two test suites. Its session row held
// activity_last_at = 17:10:20Z for the whole of it, because every one of those
// signals reported the state the row already had and the lifecycle reducer
// folds a same-state repeat without touching the transition clock.
//
// The uncorroborated waiting_input branch measured its fifteen-minute silence
// window against that frozen field. So a single latching waiting_input hint --
// the ordinary Codex PermissionRequest shape this branch exists to tolerate --
// arriving after minute fifteen would have been read as fifteen minutes of
// silence and stopped a demonstrably healthy run as ambiguous.
func TestWorkStep_WaitingInputIsNotAmbiguousWhileSignalsArrive(t *testing.T) {
	dispatchedAt := time.Date(2026, 9, 9, 17, 10, 18, 0, time.UTC)
	now := dispatchedAt.Add(20 * time.Minute)

	sess := domain.SessionRecord{
		ID: "medusa-12",
		Activity: domain.Activity{
			State: domain.ActivityWaitingInput,
			// Entered the state two seconds after dispatch and never left it.
			LastActivityAt: dispatchedAt.Add(2 * time.Second),
			// ...but was heard from four seconds ago.
			LastSignalAt: now.Add(-4 * time.Second),
		},
		FirstSignalAt: dispatchedAt.Add(2 * time.Second),
	}

	got := evaluateWorkStepProgress(
		true, sess, false, ports.WorkspaceObservation{}, "base-sha",
		now, dispatchedAt, false, false, workerEvidence{}, readOnlyExpectation{},
	)
	if !got.NoChange || got.Progress != WorkerActive {
		t.Fatalf("a worker heard from 4s ago must be left alone: %+v", got)
	}
	if got.Ambiguous {
		t.Fatalf("a worker heard from 4s ago must not be ambiguous: %+v", got)
	}
}

// The other half of the same rule: the window still has to close on a session
// that has genuinely stopped saying anything. Nothing about the recovery path
// changes -- it is reached by a session whose LIVENESS, not merely whose
// transition, has aged past the window.
func TestWorkStep_WaitingInputStillAgesIntoAmbiguityWhenTrulySilent(t *testing.T) {
	dispatchedAt := time.Date(2026, 9, 9, 17, 10, 18, 0, time.UTC)
	now := dispatchedAt.Add(20 * time.Minute)
	silent := now.Add(-(workerNeedsInputCorroborationWindow + time.Minute))

	sess := domain.SessionRecord{
		ID:            "medusa-12",
		Activity:      domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: silent, LastSignalAt: silent},
		FirstSignalAt: dispatchedAt.Add(2 * time.Second),
	}

	got := evaluateWorkStepProgress(
		true, sess, false, ports.WorkspaceObservation{}, "base-sha",
		now, dispatchedAt, false, false, workerEvidence{}, readOnlyExpectation{},
	)
	if !got.Ambiguous {
		t.Fatalf("a genuinely silent worker must still reach the bounded stop: %+v", got)
	}
	if got.NextRun != domain.WorkflowRunNeedsAttention || got.AttentionReason != ReasonWorkerDispatchAmbiguous {
		t.Fatalf("recovery disposition changed: %+v", got)
	}
}

// A row that predates the liveness column carries a zero LastSignalAt, and must
// keep answering from the transition clock rather than reading as "never heard
// from" (which would age every legacy session straight into the stop).
func TestWorkStep_WaitingInputFallsBackToTransitionClockWithoutLiveness(t *testing.T) {
	dispatchedAt := time.Date(2026, 9, 9, 17, 10, 18, 0, time.UTC)
	now := dispatchedAt.Add(20 * time.Minute)

	fresh := domain.SessionRecord{
		Activity:      domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now.Add(-time.Minute)},
		FirstSignalAt: dispatchedAt,
	}
	got := evaluateWorkStepProgress(
		true, fresh, false, ports.WorkspaceObservation{}, "base-sha",
		now, dispatchedAt, false, false, workerEvidence{}, readOnlyExpectation{},
	)
	if !got.NoChange {
		t.Fatalf("legacy row with a recent transition must be left alone: %+v", got)
	}

	stale := fresh
	stale.Activity.LastActivityAt = now.Add(-(workerNeedsInputCorroborationWindow + time.Minute))
	got = evaluateWorkStepProgress(
		true, stale, false, ports.WorkspaceObservation{}, "base-sha",
		now, dispatchedAt, false, false, workerEvidence{}, readOnlyExpectation{},
	)
	if !got.Ambiguous {
		t.Fatalf("legacy row with an old transition must still age in: %+v", got)
	}
}

// Corroboration outranks liveness in both directions: a question AO actually
// observed stops the run whether or not the agent is still emitting signals.
// This is the invariant the liveness change must not weaken.
func TestWorkStep_CorroboratedQuestionStopsRunEvenWhileSignalsArrive(t *testing.T) {
	dispatchedAt := time.Date(2026, 9, 9, 17, 10, 18, 0, time.UTC)
	now := dispatchedAt.Add(20 * time.Minute)

	sess := domain.SessionRecord{
		Activity: domain.Activity{
			State:          domain.ActivityWaitingInput,
			LastActivityAt: dispatchedAt.Add(2 * time.Second),
			LastSignalAt:   now.Add(-time.Second),
		},
		FirstSignalAt: dispatchedAt.Add(2 * time.Second),
	}
	got := evaluateWorkStepProgress(
		true, sess, false, ports.WorkspaceObservation{}, "base-sha",
		now, dispatchedAt, true, false, workerEvidence{}, readOnlyExpectation{},
	)
	if got.NoChange || got.NextRun != domain.WorkflowRunNeedsAttention {
		t.Fatalf("a corroborated question must still stop the run: %+v", got)
	}
}

// Liveness reads the newest of the two clocks and never regresses below the
// transition one, which is what lets every caller migrate to it safely.
func TestActivityLivenessPrefersTheNewestClock(t *testing.T) {
	base := time.Date(2026, 9, 9, 17, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   domain.Activity
		want time.Time
	}{
		{"liveness ahead", domain.Activity{LastActivityAt: base, LastSignalAt: base.Add(time.Minute)}, base.Add(time.Minute)},
		{"no liveness yet", domain.Activity{LastActivityAt: base}, base},
		{"liveness behind (never regress)", domain.Activity{LastActivityAt: base, LastSignalAt: base.Add(-time.Hour)}, base},
		{"nothing at all", domain.Activity{}, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Liveness(); !got.Equal(tc.want) {
				t.Fatalf("Liveness() = %v, want %v", got, tc.want)
			}
		})
	}
}
