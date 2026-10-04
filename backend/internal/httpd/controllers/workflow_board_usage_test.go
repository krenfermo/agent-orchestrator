package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// workflow_board_usage_test.go — the Board's cost line must not turn a
// two-second poll into a two-second full-ledger fold.

func TestBoardUsageCache_FoldsOncePerTTL(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	cache := &boardUsageCache{now: func() time.Time { return now }}
	calls := 0
	fold := func(context.Context, string) (map[string]domain.CompactRunUsage, error) {
		calls++
		return map[string]domain.CompactRunUsage{"wf-1": {TotalTokens: 34_000, Recorded: true}}, nil
	}

	for i := 0; i < 5; i++ {
		got := cache.get(context.Background(), "p", fold)
		if got["wf-1"].TotalTokens != 34_000 {
			t.Fatalf("poll %d returned %+v", i, got)
		}
	}
	if calls != 1 {
		t.Fatalf("folds = %d, want 1 — five polls inside the TTL must reuse one aggregate", calls)
	}

	now = now.Add(boardUsageTTL + time.Second)
	cache.get(context.Background(), "p", fold)
	if calls != 2 {
		t.Fatalf("folds = %d after the TTL expired, want 2", calls)
	}
}

func TestBoardUsageCache_DoesNotCacheAFailure(t *testing.T) {
	// An eight-second window with no cost shown, because one query lost a race
	// with a checkpoint write, is a worse outcome than retrying on the next
	// poll.
	now := time.Unix(1700000000, 0).UTC()
	cache := &boardUsageCache{now: func() time.Time { return now }}
	calls := 0
	fold := func(context.Context, string) (map[string]domain.CompactRunUsage, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("db busy")
		}
		return map[string]domain.CompactRunUsage{"wf-1": {TotalTokens: 7, Recorded: true}}, nil
	}

	if got := cache.get(context.Background(), "p", fold); got != nil {
		t.Fatalf("a failed fold must yield nothing, got %+v", got)
	}
	if got := cache.get(context.Background(), "p", fold); got["wf-1"].TotalTokens != 7 {
		t.Fatalf("the next poll must retry, got %+v", got)
	}
	if calls != 2 {
		t.Fatalf("folds = %d, want 2", calls)
	}
}

func TestBoardUsageCache_KeepsProjectsApart(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	cache := &boardUsageCache{now: func() time.Time { return now }}
	fold := func(_ context.Context, projectID string) (map[string]domain.CompactRunUsage, error) {
		return map[string]domain.CompactRunUsage{projectID: {TotalTokens: 1, Recorded: true}}, nil
	}
	if _, ok := cache.get(context.Background(), "a", fold)["a"]; !ok {
		t.Fatal("project a")
	}
	if _, ok := cache.get(context.Background(), "b", fold)["b"]; !ok {
		t.Fatal("project b must not read project a's cached fold")
	}
}

func TestBoardUsageCache_NilAndEmptyAreSafe(t *testing.T) {
	var cache *boardUsageCache
	if got := cache.get(context.Background(), "p", nil); got != nil {
		t.Fatal("a nil cache must return nothing rather than panic")
	}
	live := &boardUsageCache{}
	if got := live.get(context.Background(), "", func(context.Context, string) (map[string]domain.CompactRunUsage, error) {
		t.Fatal("an empty project id must not reach the fold")
		return nil, nil
	}); got != nil {
		t.Fatalf("got %+v", got)
	}
}
