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

	fixedNow := time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC)

	cases := []struct {
		name               string
		sessionFound       bool
		session            domain.SessionRecord
		workspaceAvailable bool
		obs                ports.WorkspaceObservation
		baseSHA            string
		now                time.Time
		dispatchedAt       time.Time
		humanInputProven   bool
		turnCompleted      bool
		evidence           workerEvidence
		readOnly           readOnlyExpectation
		wantNoChange       bool
		wantStep           domain.WorkflowStepState
		wantRun            domain.WorkflowRunState
		wantErrorClass     domain.WorkflowErrorClass
		wantAttention      string
		// wantAmbiguous is the ambiguous_worker_state conclusion WITHOUT the
		// error class: the pure evaluator states it, and observeWorkStep is
		// obliged to collect the evidence snapshot before it can become one.
		// See ambiguous_worker_state.go.
		wantAmbiguous bool
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
			// Corroborated: AO holds a question it actually reconstructed from
			// this step's pane, so a person really is being asked something.
			name:             "waiting_input with an observed question -> waiting, needs_attention",
			sessionFound:     true,
			session:          activeSession(domain.ActivityWaitingInput, false),
			humanInputProven: true,
			wantStep:         domain.WorkflowStepWaiting,
			wantRun:          domain.WorkflowRunNeedsAttention,
		},
		{
			// Uncorroborated: the reading alone is not evidence. This is the
			// wf-57f90ff2 shape — a Codex PermissionRequest latching
			// waiting_input while the agent runs tests — and it must not stop
			// the run.
			name:         "waiting_input with nothing observed -> no change",
			sessionFound: true,
			session:      activeSession(domain.ActivityWaitingInput, false),
			wantNoChange: true,
		},
		{
			// `blocked` is proof on its own: it is only entered from a
			// correlated permission dialog and is cleared by that tool's own
			// post-tool-use or the turn boundary, so it cannot go stale while
			// the agent works.
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
			wantAmbiguous:      true,
		},
		{
			name:               "idle, workspace throttled/unavailable -> no change",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: false,
			wantNoChange:       true,
		},
		// Checkpoint 8P-E.3: a real autonomous run reproduced a worker whose
		// Spawn() succeeded but the process never got past its own startup
		// (found stuck at Claude Code's interactive trust prompt), so
		// FirstSignalAt never populated and the work step polled forever.
		{
			name:               "idle, no first signal, still within startup grace -> no change",
			sessionFound:       true,
			session:            domain.SessionRecord{ID: "sess-1", Activity: domain.Activity{State: domain.ActivityIdle}},
			workspaceAvailable: false,
			now:                fixedNow,
			dispatchedAt:       fixedNow.Add(-1 * time.Minute),
			wantNoChange:       true,
		},
		// Checkpoint 8P-E.24 (incident wf-00283521 / medusa-4) sharpened this.
		// Past the startup grace with no first signal, a session AO can still
		// see is NOT proof the worker died — that inference killed a worker
		// which was sixteen minutes into a complete implementation. It now
		// reconciles instead, and only fails on evidence of absence.
		{
			name:               "idle, no first signal, startup grace exceeded, worker provably gone -> failed, agent_start_failed",
			sessionFound:       true,
			session:            domain.SessionRecord{ID: "sess-1", Activity: domain.Activity{State: domain.ActivityIdle}},
			workspaceAvailable: false,
			now:                fixedNow,
			dispatchedAt:       fixedNow.Add(-(workStepFirstSignalTimeout + time.Minute)),
			evidence:           workerEvidence{ProbeKnown: true, ProbeAlive: false},
			wantStep:           domain.WorkflowStepFailed,
			wantRun:            domain.WorkflowRunNeedsAttention,
			wantErrorClass:     domain.WorkflowErrorAgentStartFailed,
		},
		{
			name:               "idle, no first signal, startup grace exceeded, worker still alive -> no change",
			sessionFound:       true,
			session:            domain.SessionRecord{ID: "sess-1", Activity: domain.Activity{State: domain.ActivityIdle}},
			workspaceAvailable: false,
			now:                fixedNow,
			dispatchedAt:       fixedNow.Add(-(workStepFirstSignalTimeout + time.Minute)),
			evidence:           workerEvidence{SessionAlive: true, ActivitySinceDispatch: true},
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
		// ---------------------------------------------------------------
		// Declared read-only tasks. The production incident: a plan whose
		// task was "Verify current repository state (build, tests, vet, git
		// status)", whose criteria forbade every edit, and whose worker was
		// classified ambiguous_worker_state for leaving the tree alone.
		//
		// The declaration is what changes the verdict, and ONLY the
		// declaration: every case above passes readOnlyExpectation{}, which
		// is what every legacy plan and every standalone objective resolves
		// to, and none of their expectations move.
		// ---------------------------------------------------------------
		{
			name:               "read-only task, idle, worktree git-verified unchanged -> completed",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha"},
			baseSHA:            "base-sha",
			readOnly:           readOnlyExpectation{Declared: true, Verdict: readOnlyWorkspaceUnchanged, Detail: "unchanged"},
			wantStep:           domain.WorkflowStepCompleted,
			wantRun:            domain.WorkflowRunWaiting,
		},
		{
			// The known dirty baseline the plan explicitly told the worker to
			// preserve. hasWorkEvidence() reads it as "work"; the fingerprint
			// comparison knows better, and it is the comparison that decides.
			name:               "read-only task, idle, pre-existing dirty baseline preserved -> completed",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha", Dirty: true, Untracked: true},
			baseSHA:            "base-sha",
			readOnly:           readOnlyExpectation{Declared: true, Verdict: readOnlyWorkspaceUnchanged, Detail: "unchanged"},
			wantStep:           domain.WorkflowStepCompleted,
			wantRun:            domain.WorkflowRunWaiting,
		},
		{
			name:               "read-only task, idle, worktree mutated -> waiting, needs_attention, not ambiguous",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha", Dirty: true},
			baseSHA:            "base-sha",
			readOnly:           readOnlyExpectation{Declared: true, Verdict: readOnlyWorkspaceMutated, Detail: "the task is declared read-only but the worktree changed since dispatch"},
			wantStep:           domain.WorkflowStepWaiting,
			wantRun:            domain.WorkflowRunNeedsAttention,
			wantAttention:      ReasonReadOnlyWorkspaceMutated,
		},
		{
			name:               "read-only task, session terminated, worktree unchanged -> completed",
			sessionFound:       true,
			session:            activeSession(domain.ActivityExited, true),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha"},
			baseSHA:            "base-sha",
			readOnly:           readOnlyExpectation{Declared: true, Verdict: readOnlyWorkspaceUnchanged, Detail: "unchanged"},
			wantStep:           domain.WorkflowStepCompleted,
			wantRun:            domain.WorkflowRunWaiting,
		},
		{
			// No session row at all is not evidence a worker ran to
			// completion, whatever the plan declared.
			name:           "read-only task, no session row, worktree unchanged -> still failed",
			sessionFound:   false,
			readOnly:       readOnlyExpectation{Declared: true, Verdict: readOnlyWorkspaceUnchanged, Detail: "unchanged"},
			wantStep:       domain.WorkflowStepFailed,
			wantRun:        domain.WorkflowRunNeedsAttention,
			wantErrorClass: domain.WorkflowErrorWorkerTerminatedUnexpectedly,
		},
		{
			// A worker that never produced a first signal has not been shown
			// to have run, and "the tree is unchanged" is exactly what a
			// worker that never started leaves behind. The startup
			// reconciliation owns this, not the read-only rule.
			name:               "read-only task, idle, no first signal -> startup path, not a read-only completion",
			sessionFound:       true,
			session:            domain.SessionRecord{ID: "sess-1", Activity: domain.Activity{State: domain.ActivityIdle}},
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha"},
			baseSHA:            "base-sha",
			now:                fixedNow,
			dispatchedAt:       fixedNow.Add(-(workStepFirstSignalTimeout + time.Minute)),
			evidence:           workerEvidence{ProbeKnown: true, ProbeAlive: false},
			readOnly:           readOnlyExpectation{Declared: true, Verdict: readOnlyWorkspaceUnchanged, Detail: "unchanged"},
			wantStep:           domain.WorkflowStepFailed,
			wantRun:            domain.WorkflowRunNeedsAttention,
			wantErrorClass:     domain.WorkflowErrorAgentStartFailed,
		},
		{
			// Declared read-only, but AO could not obtain the comparison. The
			// declaration alone proves nothing, so the pre-existing verdict
			// stands untouched.
			name:               "read-only task with an unknown workspace verdict -> unchanged behaviour (ambiguous)",
			sessionFound:       true,
			session:            activeSession(domain.ActivityIdle, false),
			workspaceAvailable: true,
			obs:                ports.WorkspaceObservation{HeadSHA: "base-sha"},
			baseSHA:            "base-sha",
			readOnly:           readOnlyExpectation{Declared: true, Verdict: readOnlyWorkspaceUnknown},
			wantStep:           domain.WorkflowStepWaiting,
			wantRun:            domain.WorkflowRunNeedsAttention,
			wantAmbiguous:      true,
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
			wantAmbiguous:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.now
			if now.IsZero() {
				now = fixedNow
			}
			got := evaluateWorkStepProgress(tc.sessionFound, tc.session, tc.workspaceAvailable, tc.obs, tc.baseSHA, now, tc.dispatchedAt, tc.humanInputProven, tc.turnCompleted, tc.evidence, tc.readOnly)
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
			if got.Ambiguous != tc.wantAmbiguous {
				t.Errorf("Ambiguous = %v, want %v", got.Ambiguous, tc.wantAmbiguous)
			}
			if tc.wantAttention != "" && got.AttentionReason != tc.wantAttention {
				t.Errorf("AttentionReason = %q, want %q", got.AttentionReason, tc.wantAttention)
			}
			// The evaluator may never hand back the class itself: it has no
			// evidence snapshot, so it has no right to it.
			if got.ErrorClass == domain.WorkflowErrorAmbiguousWorkerState {
				t.Error("the pure evaluator assigned ambiguous_worker_state directly, bypassing the evidence gate")
			}
			if !domain.ValidWorkflowStepTransition(domain.WorkflowStepRunning, got.NextStep) {
				t.Errorf("NextStep %q is not a valid transition from running", got.NextStep)
			}
		})
	}
}
