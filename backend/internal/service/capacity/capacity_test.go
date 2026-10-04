package capacity_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/capacity"
)

type fakeHealthStore struct {
	events map[domain.AgentHarness]domain.AgentHealthEvent
}

func (f *fakeHealthStore) GetAgentHealth(_ context.Context, harness domain.AgentHarness) (domain.AgentHealthEvent, bool, error) {
	ev, ok := f.events[harness]
	return ev, ok, nil
}

// TestReaderList_NoEventRecordedIsUnknown covers test item #7's negative
// side: with no agent_health_events row at all, capacity is unknown/unknown
// certainty, never a fabricated "available".
func TestReaderList_NoEventRecordedIsUnknown(t *testing.T) {
	r := capacity.NewReader(&fakeHealthStore{events: map[domain.AgentHarness]domain.AgentHealthEvent{}})
	snapshots, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snapshots) != len(capacity.KnownHarnesses) {
		t.Fatalf("len(snapshots) = %d, want %d", len(snapshots), len(capacity.KnownHarnesses))
	}
	for _, s := range snapshots {
		if s.State != domain.CapacityUnknown || s.Certainty != domain.MetricUnknown {
			t.Fatalf("harness %s = %+v, want unknown/unknown", s.Harness, s)
		}
		if s.DetectedAt != nil || s.ResetAt != nil {
			t.Fatalf("harness %s has a timestamp with no recorded event: %+v", s.Harness, s)
		}
	}
}

// TestReaderList_CooldownLinksToRealHealthEvent covers test item #7: a
// recorded rate-limit-shaped health event surfaces as CapacityCooldown with
// actual certainty, directly derived from that event (not invented).
func TestReaderList_CooldownLinksToRealHealthEvent(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeHealthStore{events: map[domain.AgentHarness]domain.AgentHealthEvent{
		"codex": {Harness: "codex", State: domain.AgentHealthCooldown, Reason: "rate_limited (inferred)", CreatedAt: now},
	}}
	r := capacity.NewReader(store)
	snapshots, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var codex domain.CapacitySnapshot
	found := false
	for _, s := range snapshots {
		if s.Harness == "codex" {
			codex, found = s, true
		}
	}
	if !found {
		t.Fatal("expected a codex snapshot")
	}
	if codex.State != domain.CapacityCooldown {
		t.Fatalf("State = %q, want cooldown", codex.State)
	}
	if codex.Certainty != domain.MetricActual {
		t.Fatalf("Certainty = %q, want actual (derived from a real recorded event)", codex.Certainty)
	}
	if codex.Provider != "openai" {
		t.Fatalf("Provider = %q, want openai", codex.Provider)
	}
	if codex.DetectedAt == nil || !codex.DetectedAt.Equal(now) {
		t.Fatalf("DetectedAt = %v, want %v", codex.DetectedAt, now)
	}
}

// TestReaderList_ResetOnlyWhenReal covers test item #8: ResetAt stays nil
// when the underlying agent_health_events row never recorded a
// CooldownUntil (which 8H itself never invents), and is only populated when
// that field is actually set.
func TestReaderList_ResetOnlyWhenReal(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(10 * time.Minute)
	store := &fakeHealthStore{events: map[domain.AgentHarness]domain.AgentHealthEvent{
		"codex":       {Harness: "codex", State: domain.AgentHealthCooldown, CreatedAt: now, CooldownUntil: nil},
		"claude-code": {Harness: "claude-code", State: domain.AgentHealthCooldown, CreatedAt: now, CooldownUntil: &reset},
	}}
	r := capacity.NewReader(store)
	snapshots, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range snapshots {
		switch s.Harness {
		case "codex":
			if s.ResetAt != nil {
				t.Fatalf("codex ResetAt = %v, want nil (no reset was ever recorded)", s.ResetAt)
			}
		case "claude-code":
			if s.ResetAt == nil || !s.ResetAt.Equal(reset) {
				t.Fatalf("claude-code ResetAt = %v, want %v", s.ResetAt, reset)
			}
		}
	}
}
