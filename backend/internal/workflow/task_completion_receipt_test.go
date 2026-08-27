package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The Completed status for ordinary tasks is derived, at read time, from a
// session fact (SessionRecord.TurnCompletedAt). Workflow decides its own
// lifecycle from git-verified evidence plus activity state and never from a
// display status, and this pins that the new fact leaves every one of those
// decisions byte-for-byte identical.
//
// This is the boundary that matters: a workflow worker session reports its
// turns exactly like a task does, so it now carries the receipt too. What it
// must not do is let "the agent said it finished" stand in for the workspace
// evidence a workflow step is required to see before it completes.
func TestWorkflowDecisionsIgnoreTheTaskCompletionReceipt(t *testing.T) {
	now := time.Date(2026, 8, 22, 20, 0, 0, 0, time.UTC)
	dispatchedAt := now.Add(-10 * time.Minute)

	session := func(state domain.ActivityState, terminated bool, completed bool) domain.SessionRecord {
		rec := domain.SessionRecord{
			ID:            "sess-1",
			IsTerminated:  terminated,
			Activity:      domain.Activity{State: state, LastActivityAt: dispatchedAt},
			FirstSignalAt: dispatchedAt,
		}
		if completed {
			rec.TurnCompletedAt = now.Add(-time.Minute)
		}
		return rec
	}

	cases := []struct {
		name      string
		state     domain.ActivityState
		term      bool
		available bool
		obs       ports.WorkspaceObservation
	}{
		{"idle worker with no work evidence", domain.ActivityIdle, false, true, ports.WorkspaceObservation{HeadSHA: "base"}},
		{"idle worker with dirty worktree", domain.ActivityIdle, false, true, ports.WorkspaceObservation{HeadSHA: "base", Dirty: true}},
		{"idle worker, workspace unobservable", domain.ActivityIdle, false, false, ports.WorkspaceObservation{}},
		{"worker still active", domain.ActivityActive, false, true, ports.WorkspaceObservation{HeadSHA: "base"}},
		{"worker awaiting input", domain.ActivityWaitingInput, false, true, ports.WorkspaceObservation{HeadSHA: "base"}},
		{"worker process exited", domain.ActivityExited, false, true, ports.WorkspaceObservation{HeadSHA: "base"}},
		{"worker session terminated", domain.ActivityIdle, true, true, ports.WorkspaceObservation{HeadSHA: "base"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			without := evaluateWorkStepProgress(
				true, session(tc.state, tc.term, false), tc.available, tc.obs, "base", now, dispatchedAt, false, workerEvidence{SessionAlive: true}, readOnlyExpectation{},
			)
			with := evaluateWorkStepProgress(
				true, session(tc.state, tc.term, true), tc.available, tc.obs, "base", now, dispatchedAt, false, workerEvidence{SessionAlive: true}, readOnlyExpectation{},
			)
			if with != without {
				t.Fatalf("work-step decision changed with a completion receipt:\n with = %+v\n want = %+v", with, without)
			}

			healthWithout := sessionHealthFromFacts(session(tc.state, tc.term, false), true)
			healthWith := sessionHealthFromFacts(session(tc.state, tc.term, true), true)
			if healthWith != healthWithout {
				t.Fatalf("session health changed with a completion receipt: %q, want %q", healthWith, healthWithout)
			}
		})
	}
}
