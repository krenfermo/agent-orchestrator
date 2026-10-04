package controllers

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func TestBuildSessionLifecycleUsageView_CountsByAction(t *testing.T) {
	pack := domain.SessionContextPack{Version: "v1", Facts: domain.TaskCheckpointSummary{Objective: "x"}}
	entries := []workflowcore.SessionLifecycleAuditEntry{
		{Decision: domain.SessionLifecycleDecision{Action: domain.LifecycleReuse, Reasons: []domain.SessionLifecycleReason{domain.LifecycleReasonSameTaskHealthy}}},
		{Decision: domain.SessionLifecycleDecision{Action: domain.LifecycleCompact, Reasons: []domain.SessionLifecycleReason{domain.LifecycleReasonManyFixCycles}}, ContextPack: &pack},
		{Decision: domain.SessionLifecycleDecision{Action: domain.LifecycleNewSession, Reasons: []domain.SessionLifecycleReason{domain.LifecycleReasonTaskBoundary}}, ContextPack: &pack},
		{Decision: domain.SessionLifecycleDecision{Action: domain.LifecycleNewSession, Reasons: []domain.SessionLifecycleReason{domain.LifecycleReasonProviderSwitch}}, ContextPack: &pack},
	}

	v := BuildSessionLifecycleUsageView(entries)
	if v.SessionsReused != 1 {
		t.Fatalf("SessionsReused = %d, want 1", v.SessionsReused)
	}
	if v.SessionsCompacted != 1 {
		t.Fatalf("SessionsCompacted = %d, want 1", v.SessionsCompacted)
	}
	if v.SessionsCreated != 2 {
		t.Fatalf("SessionsCreated = %d, want 2", v.SessionsCreated)
	}
	if v.ContextPacksCreated != 3 {
		t.Fatalf("ContextPacksCreated = %d, want 3", v.ContextPacksCreated)
	}
	if v.SessionSwitches != 1 {
		t.Fatalf("SessionSwitches = %d, want 1", v.SessionSwitches)
	}
	if len(v.Decisions) != 4 {
		t.Fatalf("Decisions = %d, want 4", len(v.Decisions))
	}
}

func TestSessionLifecycleUsageResponse_NeverLeaksContextPackContent(t *testing.T) {
	pack := domain.SessionContextPack{Version: "v1", Facts: domain.TaskCheckpointSummary{Objective: "secret-objective-text"}}
	view := WorkflowUsageView{SessionLifecycle: SessionLifecycleUsageView{
		Decisions: []workflowcore.SessionLifecycleAuditEntry{
			{Decision: domain.SessionLifecycleDecision{Action: domain.LifecycleNewSession, Reasons: []domain.SessionLifecycleReason{domain.LifecycleReasonTaskBoundary}, ContextPackHash: pack.ContentHash()}, ContextPack: &pack},
		},
	}}
	resp := workflowUsageResponse(view)
	if len(resp.SessionLifecycle.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(resp.SessionLifecycle.Decisions))
	}
	d := resp.SessionLifecycle.Decisions[0]
	if d.ContextPackHash != pack.ContentHash() {
		t.Fatalf("hash = %q, want %q", d.ContextPackHash, pack.ContentHash())
	}
	// The wire response only ever carries the hash, never the pack's facts.
}
