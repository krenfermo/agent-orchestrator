package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/pkg/contract"
)

// The completion receipt is the durable half of the Completed status: without
// persistence, a finished task would go back to reading Inactive the moment the
// daemon restarted. This exercises the real restart — the process-level store
// is closed and the same database file is reopened.
func TestSessionCompletionReceiptSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	first, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedProject(t, first, "mer")
	rec, err := first.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A fresh session has no receipt: NULL round-trips as the zero time, which
	// is what every row written before this column existed reads as.
	if !rec.TurnCompletedAt.IsZero() {
		t.Fatalf("fresh session already looks finished: %v", rec.TurnCompletedAt)
	}

	finishedAt := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	rec.TurnCompletedAt = finishedAt
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: finishedAt}
	if err := first.UpdateSession(ctx, rec); err != nil {
		t.Fatalf("update session: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	second, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	got, found, err := second.GetSession(ctx, rec.ID)
	if err != nil || !found {
		t.Fatalf("get session after restart: found=%v err=%v", found, err)
	}
	if !got.TurnCompletedAt.Equal(finishedAt) {
		t.Fatalf("TurnCompletedAt after restart = %v, want %v", got.TurnCompletedAt, finishedAt)
	}

	status := contract.DeriveStatus(contract.SessionFacts{
		Activity:       contract.ActivityState(got.Activity.State),
		LastActivityAt: got.Activity.LastActivityAt,
		// A restored session has no hook receipt for its new launch, which on
		// its own would read no_signal.
		SignalExpected: true,
		TurnCompleted:  !got.TurnCompletedAt.IsZero(),
		IsTerminated:   got.IsTerminated,
	}, nil, time.Now().UTC(), 90*time.Second)
	if status != contract.StatusCompleted {
		t.Fatalf("status after restart = %q, want %q", status, contract.StatusCompleted)
	}

	// Clearing it round-trips too: the next turn takes the task out of
	// Completed and back to whatever it is actually doing.
	got.TurnCompletedAt = time.Time{}
	if err := second.UpdateSession(ctx, got); err != nil {
		t.Fatalf("clear receipt: %v", err)
	}
	cleared, _, err := second.GetSession(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !cleared.TurnCompletedAt.IsZero() {
		t.Fatalf("receipt not cleared: %v", cleared.TurnCompletedAt)
	}
}
