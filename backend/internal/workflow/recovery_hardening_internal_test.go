package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// recovery_hardening_internal_test.go — the predicates behind the two overnight
// blockages, tested directly.
//
// Everything here is a pure function of facts, which is the point: the
// decisions that killed wf-00283521 and refused wf-cd5bad10 were single
// inferences, and an inference nobody can put a table around is an inference
// nobody can check.

// Requirements 1, 2 and 3 of the incident brief, as one decision table:
//
//  1. a missing first signal on a LIVE/working agent is never terminal
//  2. a missing first signal with later valid task changes is reconciled
//  3. a truly dead worker with no work still fails
func TestReconcileMissingFirstSignal(t *testing.T) {
	now := time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC)
	justPastGrace := now.Add(-(workStepFirstSignalTimeout + time.Minute))
	pastReconcileBound := now.Add(-(workStepSignalReconcileTimeout + time.Minute))

	cases := []struct {
		name          string
		ev            workerEvidence
		dispatchedAt  time.Time
		wantLifecycle WorkerLifecycleState
		wantStep      domain.WorkflowStepState
		wantClass     domain.WorkflowErrorClass
		wantNoChange  bool
	}{
		{
			// The medusa-4 incident itself. The worker had been running for
			// sixteen minutes and AO declared it dead because a hook had never
			// fired. A live session and observed activity are exactly the
			// evidence that inference lacked.
			name:          "live agent, activity since dispatch -> signal_delayed, never terminal",
			ev:            workerEvidence{SessionAlive: true, ActivitySinceDispatch: true, WorkspaceObserved: true},
			dispatchedAt:  justPastGrace,
			wantLifecycle: WorkerLifecycleSignalDelayed,
			wantNoChange:  true,
		},
		{
			name:          "live agent proven by the runtime probe alone -> signal_delayed",
			ev:            workerEvidence{SessionAlive: true, ProbeKnown: true, ProbeAlive: true},
			dispatchedAt:  justPastGrace,
			wantLifecycle: WorkerLifecycleSignalDelayed,
			wantNoChange:  true,
		},
		{
			// Requirement 2: the work is there, so it is adopted rather than
			// thrown away — and this holds even past the reconcile bound,
			// because real work outranks every timer in this file.
			name:          "no first signal but git-verified work -> completed",
			ev:            workerEvidence{SessionAlive: true, WorkspaceObserved: true, WorkEvidence: true, CommitsSinceDispatch: 1},
			dispatchedAt:  pastReconcileBound,
			wantLifecycle: WorkerLifecycleCompleted,
			wantStep:      domain.WorkflowStepCompleted,
		},
		{
			// Requirement 2 again, for the shape AO actually had: the session
			// is gone, the hook never fired, and the worktree carries the work.
			// Death plus work is a completed task, not a startup failure.
			name:          "session gone but the worktree carries the work -> completed",
			ev:            workerEvidence{WorkspaceObserved: true, WorkEvidence: true},
			dispatchedAt:  justPastGrace,
			wantLifecycle: WorkerLifecycleCompleted,
			wantStep:      domain.WorkflowStepCompleted,
		},
		{
			// Requirement 3: nothing is weakened. A worker AO can prove is not
			// there, with nothing to show for it, still fails immediately.
			name:          "session gone, nothing to show -> failed",
			ev:            workerEvidence{WorkspaceObserved: true},
			dispatchedAt:  justPastGrace,
			wantLifecycle: WorkerLifecycleFailed,
			wantStep:      domain.WorkflowStepFailed,
			wantClass:     domain.WorkflowErrorAgentStartFailed,
		},
		{
			name:          "runtime probe says the process is gone -> failed",
			ev:            workerEvidence{SessionAlive: true, WorkspaceObserved: true, ProbeKnown: true, ProbeAlive: false},
			dispatchedAt:  justPastGrace,
			wantLifecycle: WorkerLifecycleFailed,
			wantStep:      domain.WorkflowStepFailed,
			wantClass:     domain.WorkflowErrorAgentStartFailed,
		},
		{
			// AO could obtain nothing either way. That is not death, and it is
			// not health: it is reconciling, bounded.
			name:          "no evidence of any kind, inside the bound -> reconciling",
			ev:            workerEvidence{SessionAlive: true},
			dispatchedAt:  justPastGrace,
			wantLifecycle: WorkerLifecycleReconciling,
			wantNoChange:  true,
		},
		{
			// …and the bound is real, so a run can never poll forever.
			name:          "no evidence of any kind, past the bound -> failed",
			ev:            workerEvidence{SessionAlive: true},
			dispatchedAt:  pastReconcileBound,
			wantLifecycle: WorkerLifecycleFailed,
			wantStep:      domain.WorkflowStepFailed,
			wantClass:     domain.WorkflowErrorAgentStartFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileMissingFirstSignal(tc.ev, now, tc.dispatchedAt)
			if got.Lifecycle != tc.wantLifecycle {
				t.Fatalf("lifecycle = %q, want %q (detail: %s)", got.Lifecycle, tc.wantLifecycle, got.Detail)
			}
			if got.Decision.NoChange != tc.wantNoChange {
				t.Fatalf("NoChange = %v, want %v", got.Decision.NoChange, tc.wantNoChange)
			}
			if tc.wantNoChange {
				if got.Decision.NextStep == domain.WorkflowStepFailed {
					t.Fatal("a no-change verdict must never carry a terminal step")
				}
				return
			}
			if got.Decision.NextStep != tc.wantStep {
				t.Fatalf("NextStep = %q, want %q", got.Decision.NextStep, tc.wantStep)
			}
			if got.Decision.ErrorClass != tc.wantClass {
				t.Fatalf("ErrorClass = %q, want %q", got.Decision.ErrorClass, tc.wantClass)
			}
			if got.Detail == "" {
				t.Fatal("every verdict must record the evidence it stood on")
			}
		})
	}
}

// A missing first signal must never, on its own, be enough to fail a step. This
// asserts the invariant across the whole evidence space rather than one row of
// it: if AO holds ANY positive liveness, the answer is never terminal.
func TestMissingFirstSignalNeverFailsAgainstPositiveLiveness(t *testing.T) {
	now := time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC)
	// Deliberately past the outer bound: even there, positive liveness wins.
	dispatchedAt := now.Add(-(workStepSignalReconcileTimeout + time.Hour))

	for _, ev := range []workerEvidence{
		{SessionAlive: true, ActivitySinceDispatch: true},
		{SessionAlive: true, ProbeKnown: true, ProbeAlive: true},
		{SessionAlive: true, WorkspaceObserved: true, WorkEvidence: true},
		// Note what is NOT in this list: a session AO can no longer see. Past
		// activity on a session that is gone, with nothing in the worktree, is
		// the "ran and produced nothing" case, and that one is supposed to fail.
		// Liveness means the worker is alive NOW.
	} {
		got := reconcileMissingFirstSignal(ev, now, dispatchedAt)
		if got.Decision.NextStep == domain.WorkflowStepFailed {
			t.Fatalf("evidence %+v produced a terminal failure: %s", ev, got.Detail)
		}
		if got.Decision.ErrorClass == domain.WorkflowErrorAgentStartFailed {
			t.Fatalf("evidence %+v was classified agent_start_failed: %s", ev, got.Detail)
		}
	}
}

// Requirement 10: the preflight detects a provider that would need an operator
// at startup, and names WHICH kind of interaction it would be — because "sign
// in", "trust this folder" and "fix the permission mode" are three different
// things a person does.
func TestEvaluateWorkerPreflight(t *testing.T) {
	ready := WorkerPreflightResult{BinaryOK: true, AuthOK: true, TrustOK: true, PermissionModeOK: true}

	t.Run("a ready provider dispatches", func(t *testing.T) {
		if v := evaluateWorkerPreflight("claude-code", "/repo", ready); !v.Ready {
			t.Fatalf("a ready provider was refused: %+v", v)
		}
	})

	t.Run("an untrusted workspace is named as a trust problem", func(t *testing.T) {
		res := ready
		res.TrustOK = false
		v := evaluateWorkerPreflight("claude-code", "/repo/incident-42", res)
		if v.Ready {
			t.Fatal("a launch that would open the trust prompt was allowed")
		}
		if v.Class != WorkflowErrorProviderWorkspaceTrustRequired || v.Reason != ReasonProviderWorkspaceTrustRequired {
			t.Fatalf("class/reason = %q/%q, want the workspace-trust pair", v.Class, v.Reason)
		}
	})

	t.Run("rejected credentials are named as an auth problem", func(t *testing.T) {
		res := ready
		res.AuthOK = false
		v := evaluateWorkerPreflight("claude-code", "/repo", res)
		if v.Ready || v.Class != WorkflowErrorProviderAuthRequired {
			t.Fatalf("verdict = %+v, want a provider_auth_required refusal", v)
		}
	})

	t.Run("an unattendable permission mode is named as a preflight failure", func(t *testing.T) {
		res := ready
		res.PermissionModeOK = false
		v := evaluateWorkerPreflight("claude-code", "/repo", res)
		if v.Ready || v.Class != WorkflowErrorProviderPreflightFailed {
			t.Fatalf("verdict = %+v, want a provider_preflight_failed refusal", v)
		}
	})

	t.Run("what AO could not check is never a refusal", func(t *testing.T) {
		// The alternative — grounding a dispatch because a probe was
		// inconclusive — would be strictly worse than the incident this
		// preflight exists for.
		res := WorkerPreflightResult{
			BinaryOK: true, AuthUnknown: true, TrustUnknown: true, PermissionModeUnknown: true,
		}
		if v := evaluateWorkerPreflight("codex", "/repo", res); !v.Ready {
			t.Fatalf("an unknown readiness answer refused the dispatch: %+v", v)
		}
	})

	t.Run("a refusal classifies as permanent, never as a retryable hiccup", func(t *testing.T) {
		res := ready
		res.TrustOK = false
		v := evaluateWorkerPreflight("claude-code", "/repo", res)
		cls := classifyWorkerLaunchFailure(&ErrProviderPreflight{Class: v.Class, Reason: v.Reason, Detail: v.Detail})
		if cls.Retryable {
			t.Fatal("a workspace-trust refusal was classified retryable; no amount of waiting trusts a folder")
		}
		if cls.Reason != ReasonProviderWorkspaceTrustRequired {
			t.Fatalf("reason = %q, want %q", cls.Reason, ReasonProviderWorkspaceTrustRequired)
		}
		if disp, ok := attentionDispositions[cls.Reason]; !ok || disp.HumanAction == "" {
			t.Fatal("the refusal reason has no human action registered, so it would reach the Board unactionable")
		}
	})
}

// Requirement 14, for this file's share of it: the provenance vocabulary must
// authorize exactly two classes and nothing else. A future value that quietly
// joined Authorized() would be a route around review.
func TestOnlyAOsOwnAgentsProduceAuthorizedProvenance(t *testing.T) {
	authorized := map[WorkspaceProvenanceClass]bool{
		ProvenanceAuthorizedWork: true,
		ProvenanceAuthorizedFix:  true,
	}
	for _, class := range []WorkspaceProvenanceClass{
		ProvenanceAuthorizedWork, ProvenanceAuthorizedFix, ProvenancePreexisting,
		ProvenanceOtherAOTask, ProvenanceExternal, ProvenanceConflicting, ProvenanceUnknown,
	} {
		if class.Authorized() != authorized[class] {
			t.Fatalf("%s.Authorized() = %v, want %v", class, class.Authorized(), authorized[class])
		}
	}
}
