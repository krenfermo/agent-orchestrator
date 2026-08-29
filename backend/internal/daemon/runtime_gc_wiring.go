package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
	taskworkspace "github.com/aoagents/agent-orchestrator/backend/internal/workspace"
)

// runtime_gc_wiring.go — P1-C §R: when Runtime GC runs.
//
// Three triggers, all bounded, and none of them aggressive:
//
//   - once at daemon startup, as part of boot reconciliation. A restart is
//     exactly when the runtimes of a crashed daemon are most likely to be
//     orphaned, and it is the one moment nothing is mid-launch;
//   - a low-frequency periodic sweep, so a daemon that stays up for weeks does
//     not accumulate;
//   - explicitly, by an operator (`ao runtime gc`, POST /api/v1/runtime/gc).
//
// There is deliberately no "after every terminal runtime" trigger. A sweep per
// finished step would be a sweep every few minutes on a busy machine, for a
// problem that is measured in days -- and the periodic pass reclaims exactly
// the same resources a little later, using exactly the same proofs.

// runtimeGCInterval is the periodic sweep's period.
//
// Fifteen minutes: long enough that the sweep is invisible on a busy machine,
// short enough that a finished reviewer's session does not sit around for an
// afternoon. It is not tunable, on purpose -- a GC interval is not a knob an
// operator should have to reason about, and the two things that actually
// matter (what may be destroyed, and the proofs required) are not affected by
// it at all.
const runtimeGCInterval = 15 * time.Minute

// newRuntimeGC builds the sweeper, or nil when the runtime cannot be
// enumerated or its instances cannot be addressed.
//
// A nil sweeper means no GC, which is untidy and never unsafe. It is the same
// stance every optional dependency in this daemon takes, and it is the right
// one here specifically: a sweeper that could not prove ownership would have
// to skip everything anyway.
func newRuntimeGC(runtime any, store *sqlitestore.Store, worktrees runtimegc.WorktreeReleaser, log *slog.Logger) *runtimegc.Sweeper {
	inventory, hasInventory := runtime.(ports.RuntimeInventory)
	facts, hasFacts := runtime.(ports.SessionFactsReader)
	if !hasInventory && !hasFacts {
		return nil
	}
	sweeper := &runtimegc.Sweeper{Claims: store, Runs: store, Sessions: store, Log: log}
	// P1-D §X: the placement sweep. Both halves or neither -- reading records
	// nothing can act on finds candidates it cannot reclaim, and a releaser
	// with no records has nothing to reclaim.
	if worktrees != nil {
		sweeper.Worktrees, sweeper.WorktreeGC = store, worktrees
	}
	if hasInventory {
		sweeper.Inventory = inventory
	}
	if hasFacts {
		sweeper.Facts = facts
	}
	return sweeper
}

// startRuntimeGC runs the boot sweep and then the periodic one.
//
// The boot sweep is synchronous but never fatal: a GC failure at startup must
// not stop the daemon from serving, because everything it would have cleaned
// is inert by definition. The periodic loop exits with the daemon's context.
func startRuntimeGC(ctx context.Context, sweeper *runtimegc.Sweeper, log *slog.Logger) (done <-chan struct{}) {
	finished := make(chan struct{})
	if sweeper == nil {
		close(finished)
		return finished
	}
	if _, err := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "daemon_startup"}); err != nil {
		log.Warn("runtime gc: boot sweep did not run", "err", err)
	}
	go func() {
		defer close(finished)
		ticker := time.NewTicker(runtimeGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "periodic"}); err != nil {
					// One failed sweep is one failed sweep. The next tick
					// re-derives everything from durable facts, so nothing
					// accumulates as a consequence of this.
					log.Warn("runtime gc: periodic sweep failed", "err", err)
				}
			}
		}
	}()
	return finished
}

// worktreeReleaser adapts the workspace manager's task-keyed Release to the
// (run, task) shape the sweeper uses.
//
// The run id is carried but not passed down: the manager resolves the record
// by task id, which is its primary key, and a worktree belongs to exactly one
// run. Keeping the run in the sweeper's own signature is what makes a finding
// attributable in a report; the manager does not need it to act.
type worktreeReleaser struct{ mgr *taskworkspace.Manager }

func (w worktreeReleaser) ReleaseTaskWorktree(ctx context.Context, _, taskID string) error {
	if w.mgr == nil {
		return nil
	}
	_, _, err := w.mgr.Release(ctx, taskID)
	return err
}
