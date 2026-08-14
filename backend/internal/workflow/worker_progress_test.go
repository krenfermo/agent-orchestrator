package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestEvaluateWorkStepProgress covers the full conservative completion-rule
// decision table from Checkpoint 8B §8: every fact combination the spec
// specifies, asserted against evaluateWorkStepProgress directly (this file
// is package workflow, not workflow_test, so it can reach the unexported
// function without going through the Coordinator).
func TestEvaluateWorkStepProgress(t *testing.T) {
	activeSession := func(state domain.ActivityState, terminated bool) domain.SessionRecord {
		return domain.SessionRecord{
			ID:            "sess-1",
			IsTerminated:  terminated,
			Activity:      domain.Activity{State: state},
			FirstSignalAt: time.Now(),
		}
	}

	cases := []struct {
		name               string
		sessionFound       bool
		session            domain.SessionRecord
		workspaceAvailable bool
		obs                ports.WorkspaceObservation
		baseSHA            string
		wantNoChange       bool
		wantStep           domain.WorkflowStepState
		wantRun            domain.WorkflowRunState
		wantErrorClass     domain.WorkflowErrorClass
	}{
		{
			name:           "session not found -> failed, needs_attention",
			sessionFound:   false,
			wantStep:       domain.WorkflowStepFailed,
			wantRun:        domain.WorkflowRunNeedsAttention,
			wantErrorClass: domain.WorkflowErrorWorkerTerminatedUnexpectedly,
		},
		{
			name:           "terminated, no commit evidence -> failed",
			sessionFound:   true,
			session:        activeSession(domain.ActivityExited, true),
			baseSHA:        "base",
			wantStep:       domain.WorkflowStepFailed,
			wantRun:        domain.WorkflowRunNeedsAttention,
			wantErrorClass: domain.WorkflowErrorWorkerTerminatedUnexpectedly,
		},
		{
			name:               "terminated, commit evidence -> completed",
			sessionFound:       true,
			session:            activeSession(domain.ActivityExited, true),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "new-sha"},
			baseSHA:            "base-sha",
			wantStep:           domain.WorkflowStepCompleted,
			wantRun:            domain.WorkflowRunWaiting,
		},
		{
			name:         "active session -> no change",
			sessionFound: true,
			session:      activeSession(domain.ActivityActive, false),
			wantNoChange: true,
		},
		{
			name:         "waiting_input -> waiting, needs_attention",
			sessionFound: true,
			session:      activeSession(domain.ActivityWaitingInput, false),
			wantStep:     domain.WorkflowStepWaiting,
			wantRun:      domain.WorkflowRunNeedsAttention,
		},
		{
			name:         "blocked -> waiting, needs_attention",
			sessionFound: true,
			session:      activeSession(domain.ActivityBlocked, false),
			wantStep:     domain.WorkflowStepWaiting,
			wantRun:      domain.WorkflowRunNeedsAttention,
		},
		{
			name:               "idle with commit evidence -> completed",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "new-sha"},
			baseSHA:            "base-sha",
			wantStep:           domain.WorkflowStepCompleted,
			wantRun:            domain.WorkflowRunWaiting,
		},
		{
			name:               "idle before first signal and without work evidence -> no change",
			sessionFound:       true,
			session:            domain.SessionRecord{ID: "sess-1", Activity: domain.Activity{State: domain.ActivityIdle}},
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha"},
			baseSHA:            "base-sha",
			wantNoChange:       true,
		},
		{
			name:               "idle with no verifiable change -> waiting, ambiguous",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha"},
			baseSHA:            "base-sha",
			wantStep:           domain.WorkflowStepWaiting,
			wantRun:            domain.WorkflowRunNeedsAttention,
			wantErrorClass:     domain.WorkflowErrorAmbiguousWorkerState,
		},
		{
			name:               "idle, workspace throttled/unavailable -> no change",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: false,
			wantNoChange:       true,
		},
		// Regression: discovered by the real Codex E2E run. The guardrail
		// prompt tells the worker not to commit/push/merge, so a genuinely
		// completed task commonly leaves an untracked/dirty file rather than a
		// new HEAD SHA. Uncommitted-but-real worktree evidence must count as
		// verifiable work, not be treated the same as "nothing happened".
		{
			name:               "idle, untracked file with unchanged HEAD -> completed",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha", Untracked: true},
			baseSHA:            "base-sha",
			wantStep:           domain.WorkflowStepCompleted,
			wantRun:            domain.WorkflowRunWaiting,
		},
		{
			name:               "idle, dirty worktree with unchanged HEAD -> completed",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha", Dirty: true},
			baseSHA:            "base-sha",
			wantStep:           domain.WorkflowStepCompleted,
			wantRun:            domain.WorkflowRunWaiting,
		},
		{
			name:               "idle, staged-but-uncommitted change -> completed",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha", Staged: true},
			baseSHA:            "base-sha",
			wantStep:           domain.WorkflowStepCompleted,
			wantRun:            domain.WorkflowRunWaiting,
		},
		{
			name:               "idle, truly no changes at all -> waiting, ambiguous",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha"},
			baseSHA:            "base-sha",
			wantStep:           domain.WorkflowStepWaiting,
			wantRun:            domain.WorkflowRunNeedsAttention,
			wantErrorClass:     domain.WorkflowErrorAmbiguousWorkerState,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateWorkStepProgress(tc.sessionFound, tc.session, tc.workspaceAvailable, tc.obs, tc.baseSHA)
			if got.NoChange != tc.wantNoChange {
				t.Fatalf("NoChange = %v, want %v", got.NoChange, tc.wantNoChange)
			}
			if tc.wantNoChange {
				return
			}
			if got.NextStep != tc.wantStep {
				t.Errorf("NextStep = %q, want %q", got.NextStep, tc.wantStep)
			}
			if got.NextRun != tc.wantRun {
				t.Errorf("NextRun = %q, want %q", got.NextRun, tc.wantRun)
			}
			if got.ErrorClass != tc.wantErrorClass {
				t.Errorf("ErrorClass = %q, want %q", got.ErrorClass, tc.wantErrorClass)
			}
			if !domain.ValidWorkflowStepTransition(domain.WorkflowStepRunning, got.NextStep) {
				t.Errorf("NextStep %q is not a valid transition from running", got.NextStep)
			}
		})
	}
}
