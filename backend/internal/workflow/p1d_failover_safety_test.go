package workflow_test

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1d_failover_safety_test.go — P1-D §R/§S, matrix 40/41/42/44.
//
// The rule these lock down is the one that must never be relaxed: AO does not
// start a second provider on a worktree the first may have written. It was
// already true structurally; these make it assertable.

// Matrix 40/41/42: only PROVEN states permit failover, and the ambiguous one
// never does — not even when it looks like a clean success.
func TestFailoverSafetyPermitsOnlyProvenStates(t *testing.T) {
	launchFailed := errors.New("provider is rate limited")
	tests := []struct {
		name            string
		err             error
		namedSession    bool
		proven          bool
		want            workflowcore.FailoverSafety
		permitsFailover bool
	}{
		{
			// The launcher's contract: a record, or an error having created
			// nothing. Nothing ran, so nothing was mutated.
			name: "classified launch failure with no session",
			err:  launchFailed,
			want: workflowcore.FailoverSafeBeforeExecution, permitsFailover: true,
		},
		{
			// AO holds positive evidence the workspace is untouched.
			name: "proven no mutation", err: launchFailed, proven: true,
			want: workflowcore.FailoverSafeAfterProvenNoMutation, permitsFailover: true,
		},
		{
			// errLaunchWithoutEvidence's shape: the launcher said "fine" and
			// named nothing. This is the one that must never fail over.
			name: "launcher reported success and named no session",
			want: workflowcore.FailoverAmbiguousExecution, permitsFailover: false,
		},
		{
			name: "launch succeeded", namedSession: true,
			want: workflowcore.FailoverCompletedExecution, permitsFailover: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workflowcore.ClassifyFailoverSafety(tt.err, tt.namedSession, tt.proven)
			if got != tt.want {
				t.Fatalf("classification = %q, want %q", got, tt.want)
			}
			if got.PermitsFailover() != tt.permitsFailover {
				t.Fatalf("%q permits failover = %v, want %v", got, got.PermitsFailover(), tt.permitsFailover)
			}
		})
	}

	// The ordering is the safety model: an ambiguous launch stays ambiguous
	// even when a workspace probe would have called it clean. Evidence about
	// the worktree cannot answer a question about whether the provider ran.
	if got := workflowcore.ClassifyFailoverSafety(nil, false, true); got != workflowcore.FailoverAmbiguousExecution {
		t.Fatalf("an ambiguous launch with a clean workspace classified as %q; ambiguity must not be resolved by a git probe", got)
	}
}

// Matrix 43/44/45: a provider attempt is not a task generation. Failover
// changes who is trying; it never changes what is being tried.
func TestProviderAttemptIdentitySeparatesAttemptFromObligation(t *testing.T) {
	obligation := workflowcore.ProviderAttemptIdentity{
		WorkflowRunID: "wf-1", WorkflowStepID: "wfs-1", LifecycleGeneration: 3,
		AttemptNumber: 1, From: domain.HarnessClaudeCode,
	}
	hop := obligation
	hop.AttemptNumber = 2
	hop.FailoverOrdinal = 1
	hop.To = domain.HarnessCodex
	hop.Reason = domain.WorkflowErrorRateLimited
	hop.Safety = workflowcore.FailoverSafeBeforeExecution

	// 44: the obligation is byte-stable across the hop.
	if hop.WorkflowRunID != obligation.WorkflowRunID ||
		hop.WorkflowStepID != obligation.WorkflowStepID ||
		hop.LifecycleGeneration != obligation.LifecycleGeneration {
		t.Fatalf("failover changed the obligation: %+v -> %+v", obligation, hop)
	}
	if !hop.Authorized() {
		t.Fatalf("a proven-safe hop to a different provider was not authorized: %+v", hop)
	}

	// 45: a hop AO could not prove safe is not authorized, however it was
	// recorded — the record's own consistency check.
	ambiguous := hop
	ambiguous.Safety = workflowcore.FailoverAmbiguousExecution
	if ambiguous.Authorized() {
		t.Fatal("a hop recorded with ambiguous safety read as authorized")
	}
	// 49: a hop to the provider it came from is not a hop, so it cannot be a
	// way around the failover budget.
	loop := hop
	loop.To = loop.From
	if loop.Authorized() {
		t.Fatal("a hop back to the same provider read as authorized; that is the Claude<->Codex loop")
	}
	// A hop naming no destination is not a hop either.
	nowhere := hop
	nowhere.To = ""
	if nowhere.Authorized() {
		t.Fatal("a hop with no destination read as authorized")
	}
}
