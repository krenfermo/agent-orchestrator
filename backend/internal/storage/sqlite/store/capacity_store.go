package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// capacity_store.go — P1-C's durable scheduler storage.
//
// The one thing worth reading twice is AcquireCapacity: granting a slot has to
// count held slots and grant the next one in a SINGLE statement, or two
// concurrent dispatches both read "5 of 6 held" and both launch.

func capacityClaimFromRow(r gen.CapacityClaim) domain.CapacityClaim {
	return domain.CapacityClaim{
		ID: r.ID, Kind: domain.ExecutionKind(r.ExecutionKind),
		State: domain.CapacityClaimState(r.State), WorkflowRunID: r.WorkflowRunID,
		WorkflowStepID: r.WorkflowStepID, TaskID: r.TaskID,
		LifecycleGeneration: r.LifecycleGeneration, DispatchKey: r.DispatchKey,
		OwnerID: domain.UserID(r.OwnerID), ProjectID: r.ProjectID,
		RuntimeHandle: r.RuntimeHandle, RuntimeInstanceID: r.RuntimeInstanceID,
		Priority: r.Priority, EnqueuedAt: r.EnqueuedAt,
		HeldAt: nullTimeToTimePtr(r.HeldAt), ReleasedAt: nullTimeToTimePtr(r.ReleasedAt),
		ReleaseReason: r.ReleaseReason, UpdatedAt: r.UpdatedAt,
	}
}

// EnqueueCapacityClaim admits a launch intent to the durable queue.
//
// Idempotent by dispatch_key: reconciliation, a wake, a restart and a double
// click all re-derive the same key, and only the first one inserts a row. The
// bool reports whether this call created the claim, which callers use for
// logging only -- an existing claim is a success, not a conflict.
func (s *Store) EnqueueCapacityClaim(ctx context.Context, claim domain.CapacityClaim) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.EnqueueCapacityClaim(ctx, gen.EnqueueCapacityClaimParams{
		ID: claim.ID, ExecutionKind: string(claim.Kind), WorkflowRunID: claim.WorkflowRunID,
		WorkflowStepID: claim.WorkflowStepID, TaskID: claim.TaskID,
		LifecycleGeneration: claim.LifecycleGeneration, DispatchKey: claim.DispatchKey,
		OwnerID: string(claim.OwnerID), ProjectID: claim.ProjectID, Priority: claim.Priority,
		EnqueuedAt: claim.EnqueuedAt, UpdatedAt: claim.UpdatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("enqueue capacity claim: %w", err)
	}
	return n > 0, nil
}

// AcquireCapacity promotes a queued claim to held if — and only if — every
// configured bound still permits it.
//
// The three bounds are evaluated inside the UPDATE's own WHERE clause, against
// the same snapshot the write commits under. That is the whole correctness
// argument: a read-then-write scheduler lets two dispatches observe "one slot
// left" and both take it, and no amount of application-level locking fixes
// that across daemon restarts. Here the database refuses the second one.
//
// It is also fenced on lifecycle_generation, so a stale generation cannot
// promote a claim the lifecycle has moved past.
//
// A false result is not an error: it means "no slot right now", and the caller
// parks the run on the durable capacity wait rather than failing it.
func (s *Store) AcquireCapacity(ctx context.Context, dispatchKey string, generation int64, limits domain.CapacityLimits, kind domain.ExecutionKind, now time.Time) (bool, error) {
	limits = limits.Normalize()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.PromoteCapacityClaim(ctx, gen.PromoteCapacityClaimParams{
		HeldAt:              sql.NullTime{Time: now, Valid: true},
		UpdatedAt:           now,
		DispatchKey:         dispatchKey,
		LifecycleGeneration: generation,
		// Column5/6/7 are the global, per-kind and per-workflow bounds, in the
		// order the statement's three subqueries appear. sqlc names positional
		// parameters inside CAST() this way; the statement itself is the
		// documentation.
		Column5: int64(limits.Global),
		Column6: int64(limits.LimitFor(kind)),
		Column7: int64(limits.PerWorkflow),
	})
	if err != nil {
		return false, fmt.Errorf("acquire capacity: %w", err)
	}
	return n > 0, nil
}

// GetCapacityClaim reads one claim by its launch-intent identity.
func (s *Store) GetCapacityClaim(ctx context.Context, dispatchKey string) (domain.CapacityClaim, bool, error) {
	r, err := s.qr.GetCapacityClaimByDispatchKey(ctx, dispatchKey)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CapacityClaim{}, false, nil
	}
	if err != nil {
		return domain.CapacityClaim{}, false, fmt.Errorf("get capacity claim: %w", err)
	}
	return capacityClaimFromRow(r), true, nil
}

// BindCapacityClaimRuntime records which runtime incarnation a held claim paid
// for. Conditional on the claim still being held under the same generation, so
// a stale writer cannot attach a runtime to somebody else's slot.
func (s *Store) BindCapacityClaimRuntime(ctx context.Context, dispatchKey, handle, instanceID string, generation int64, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.BindCapacityClaimRuntime(ctx, gen.BindCapacityClaimRuntimeParams{
		RuntimeHandle: handle, RuntimeInstanceID: instanceID, UpdatedAt: now,
		DispatchKey: dispatchKey, LifecycleGeneration: generation,
	})
	return n > 0, err
}

// ReleaseCapacityClaim returns one slot.
//
// Generation-fenced and therefore idempotent: a claim already released matches
// zero rows, which the caller reads as "already free" rather than as a
// failure, and a stale generation matches zero rows too — it can never release
// a newer claim that reused the same dispatch key.
func (s *Store) ReleaseCapacityClaim(ctx context.Context, dispatchKey string, generation int64, reason string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ReleaseCapacityClaim(ctx, gen.ReleaseCapacityClaimParams{
		ReleasedAt: sql.NullTime{Time: now, Valid: true}, ReleaseReason: reason,
		UpdatedAt: now, DispatchKey: dispatchKey, LifecycleGeneration: generation,
	})
	return n > 0, err
}

// ReleaseCapacityClaimsForRun frees everything one run still holds.
//
// Deliberately NOT generation-fenced, and deliberately reserved for a run that
// has reached a terminal state: at that point no generation of that run may
// launch anything ever again, so there is no newer claim to protect — and a
// terminal run holding a slot forever is the leak this exists to prevent.
func (s *Store) ReleaseCapacityClaimsForRun(ctx context.Context, runID, reason string, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.ReleaseCapacityClaimsForRun(ctx, gen.ReleaseCapacityClaimsForRunParams{
		ReleasedAt: sql.NullTime{Time: now, Valid: true}, ReleaseReason: reason,
		UpdatedAt: now, WorkflowRunID: runID,
	})
}

// CapacityUsageByKind returns held and queued counts per execution kind, plus
// the global held count.
func (s *Store) CapacityUsageByKind(ctx context.Context) (held, queued map[domain.ExecutionKind]int, total int, err error) {
	heldRows, err := s.qr.CountHeldCapacityClaimsByKind(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("count held capacity claims: %w", err)
	}
	queuedRows, err := s.qr.CountQueuedCapacityClaimsByKind(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("count queued capacity claims: %w", err)
	}
	held, queued = map[domain.ExecutionKind]int{}, map[domain.ExecutionKind]int{}
	for _, r := range heldRows {
		held[domain.ExecutionKind(r.ExecutionKind)] = int(r.Held)
		total += int(r.Held)
	}
	for _, r := range queuedRows {
		queued[domain.ExecutionKind(r.ExecutionKind)] = int(r.Queued)
	}
	return held, queued, total, nil
}

// ListHeldCapacityClaims returns every claim currently occupying a slot.
func (s *Store) ListHeldCapacityClaims(ctx context.Context) ([]domain.CapacityClaim, error) {
	rows, err := s.qr.ListHeldCapacityClaims(ctx)
	if err != nil {
		return nil, fmt.Errorf("list held capacity claims: %w", err)
	}
	return capacityClaimsFromRows(rows), nil
}

// ListQueuedCapacityClaims returns the next queued claims in scheduling order:
// priority, then age, then id as a total tiebreak.
func (s *Store) ListQueuedCapacityClaims(ctx context.Context, limit int) ([]domain.CapacityClaim, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.qr.ListQueuedCapacityClaims(ctx, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("list queued capacity claims: %w", err)
	}
	return capacityClaimsFromRows(rows), nil
}

// ListCapacityClaimsForRun returns one run's whole claim history.
func (s *Store) ListCapacityClaimsForRun(ctx context.Context, runID string) ([]domain.CapacityClaim, error) {
	rows, err := s.qr.ListCapacityClaimsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list capacity claims for run: %w", err)
	}
	return capacityClaimsFromRows(rows), nil
}

// ListOutstandingCapacityClaims returns every claim that is not released, for
// recovery sweeps.
func (s *Store) ListOutstandingCapacityClaims(ctx context.Context) ([]domain.CapacityClaim, error) {
	rows, err := s.qr.ListOutstandingCapacityClaims(ctx)
	if err != nil {
		return nil, fmt.Errorf("list outstanding capacity claims: %w", err)
	}
	return capacityClaimsFromRows(rows), nil
}

func capacityClaimsFromRows(rows []gen.CapacityClaim) []domain.CapacityClaim {
	out := make([]domain.CapacityClaim, 0, len(rows))
	for _, r := range rows {
		out = append(out, capacityClaimFromRow(r))
	}
	return out
}
