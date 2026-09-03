package controllers

import (
	"context"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// workflow_board_usage.go — keeping the Board's cost line cheap.
//
// The Board polls every two seconds while anything is moving, and the usage
// aggregate it needs folds the whole project's ledger. That fold is already one
// grouped query rather than one per card, but the ledger is append-only and
// never pruned: at 100,000 events it measures ~250ms, and 250ms every two
// seconds is a quarter of a core burned forever on a number nobody needs to the
// second.
//
// So the fold is memoized per project for a few seconds. A spend figure that
// lags a poll or two is fine — it is a running total, not a state transition —
// while the Board's own state stays as live as it was.
//
// The cache is deliberately tiny and dumb: a value per project, a timestamp, and
// a mutex. It stores only what the aggregate already computed, so a stale entry
// is a slightly old TRUE figure and never a wrong one, and dropping the whole
// cache costs one query.

// boardUsageTTL is how long a project's folded usage may be reused. Short
// enough that a finished run's cost appears while the user is still looking at
// the board; long enough that a two-second poll cannot drive the fold.
const boardUsageTTL = 8 * time.Second

type boardUsageEntry struct {
	byRun    map[string]domain.CompactRunUsage
	computed time.Time
}

type boardUsageCache struct {
	mu      sync.Mutex
	entries map[string]boardUsageEntry
	// now is injectable so the TTL is testable without sleeping.
	now func() time.Time
}

func (c *boardUsageCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// get returns the project's folded usage, computing it at most once per TTL.
//
// A failed fold is NOT cached: the Board would rather retry a transient error on
// the next poll than show no cost for eight seconds because one query lost a
// race with a checkpoint.
func (c *boardUsageCache) get(
	ctx context.Context,
	projectID string,
	fold func(context.Context, string) (map[string]domain.CompactRunUsage, error),
) map[string]domain.CompactRunUsage {
	if c == nil || fold == nil || projectID == "" {
		return nil
	}
	now := c.clock()

	c.mu.Lock()
	if entry, ok := c.entries[projectID]; ok && now.Sub(entry.computed) < boardUsageTTL {
		c.mu.Unlock()
		return entry.byRun
	}
	c.mu.Unlock()

	// The fold runs OUTSIDE the lock. Holding it across a query would serialize
	// every board poll in the process behind the slowest project's aggregate,
	// which is the opposite of what this cache exists to do. Two concurrent
	// misses may both compute; they produce the same answer and the second
	// simply overwrites the first.
	byRun, err := fold(ctx, projectID)
	if err != nil {
		return nil
	}

	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]boardUsageEntry{}
	}
	c.entries[projectID] = boardUsageEntry{byRun: byRun, computed: now}
	c.mu.Unlock()
	return byRun
}
