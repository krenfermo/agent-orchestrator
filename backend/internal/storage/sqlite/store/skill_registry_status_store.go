package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// skill_registry_status_store.go -- the durable half of "what did the network
// actually say" (migration 0165).
//
// Two kinds of row, and they are separate from the configuration on purpose:
//
//	skill_registry_status       what the last connection test and the last sync
//	                            observed
//	skill_registry_revocations  what a registry says is withdrawn, kept so the
//	                            answer survives being offline
//
// The second one is the asymmetry that matters. 0164 deliberately caches no
// listing of what a registry OFFERS, because a cached offer would let an
// install act on a release withdrawn since somebody last looked. A withdrawal
// is the opposite: keeping it can only ever be wrong in the safe direction, so
// it is persisted, and an install that cannot reach its registry consults it
// rather than reading silence as consent.

// SkillRegistryStatus is what AO last observed about one registry.
type SkillRegistryStatus struct {
	RegistryID string
	// LastProbeState is one of the six ProbeStates, or empty when no
	// connection test has been run. Empty is a real state and is rendered as
	// "never tested" rather than as a failure.
	LastProbeState   skillregistry.ProbeState
	LastProbeDetail  string
	LastProbeAt      *time.Time
	LastProbeLatency time.Duration
	// LastSyncAt is the last SUCCESSFUL metadata read. It is what an "as of"
	// on a stale listing is measured against.
	LastSyncAt *time.Time
	// LastRevocationSyncAt is separate: a registry can be answering metadata
	// and still have had its withdrawal list not read today, and those are
	// different things to tell an operator.
	LastRevocationSyncAt *time.Time
	UpdatedAt            time.Time
}

// Tested reports whether a connection test has ever run.
func (s SkillRegistryStatus) Tested() bool { return s.LastProbeState != "" }

// SkillRegistryRevocation is one withdrawal AO has recorded.
type SkillRegistryRevocation struct {
	RegistryID string
	SkillID    string
	Version    string
	Reason     string
	// RevokedAt is when the REGISTRY says it withdrew the release; ObservedAt
	// is when AO first saw it. Two facts, and only the second is AO's.
	RevokedAt  *time.Time
	ObservedAt time.Time
}

// Ref is the "<skillId>@<version>" identity.
func (r SkillRegistryRevocation) Ref() string { return r.SkillID + "@" + r.Version }

// UpsertSkillRegistryStatus records what one observation saw.
//
// A probe writes the probe fields and leaves the sync timestamps nil; a sync
// does the reverse. The query COALESCEs, so neither erases the other's answer:
// "the last test passed" and "the last successful read was an hour ago" are
// both true at once and both worth showing.
func (s *Store) UpsertSkillRegistryStatus(
	ctx context.Context, in SkillRegistryStatus,
) (SkillRegistryStatus, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillRegistryStatus(ctx, gen.UpsertSkillRegistryStatusParams{
		RegistryID:           in.RegistryID,
		LastProbeState:       string(in.LastProbeState),
		LastProbeDetail:      in.LastProbeDetail,
		LastProbeAt:          timePtrToNullTime(in.LastProbeAt),
		LastProbeLatencyMs:   in.LastProbeLatency.Milliseconds(),
		LastSyncAt:           timePtrToNullTime(in.LastSyncAt),
		LastRevocationSyncAt: timePtrToNullTime(in.LastRevocationSyncAt),
		UpdatedAt:            in.UpdatedAt,
	})
	if err != nil {
		return SkillRegistryStatus{}, fmt.Errorf("upsert skill registry status: %w", err)
	}
	return statusFromRow(gen.SkillRegistryStatus(row)), nil
}

// GetSkillRegistryStatus returns one registry's status, or (zero, false, nil).
func (s *Store) GetSkillRegistryStatus(
	ctx context.Context, registryID string,
) (SkillRegistryStatus, bool, error) {
	row, err := s.qr.GetSkillRegistryStatus(ctx, registryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillRegistryStatus{}, false, nil
		}
		return SkillRegistryStatus{}, false, fmt.Errorf("get skill registry status: %w", err)
	}
	return statusFromRow(gen.SkillRegistryStatus(row)), true, nil
}

// ListSkillRegistryStatuses returns every recorded status.
func (s *Store) ListSkillRegistryStatuses(ctx context.Context) ([]SkillRegistryStatus, error) {
	rows, err := s.qr.ListSkillRegistryStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill registry statuses: %w", err)
	}
	out := make([]SkillRegistryStatus, 0, len(rows))
	for _, row := range rows {
		out = append(out, statusFromRow(gen.SkillRegistryStatus(row)))
	}
	return out, nil
}

// UpsertSkillRegistryRevocation records one withdrawal.
//
// ObservedAt is written once and never moved forward: it records when AO FIRST
// saw the withdrawal, which is what an incident review needs. A re-sync that
// refreshed it would erase how long the release had been known-bad.
func (s *Store) UpsertSkillRegistryRevocation(
	ctx context.Context, in SkillRegistryRevocation,
) (SkillRegistryRevocation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillRegistryRevocation(ctx, gen.UpsertSkillRegistryRevocationParams{
		RegistryID: in.RegistryID,
		SkillID:    in.SkillID,
		Version:    in.Version,
		Reason:     in.Reason,
		RevokedAt:  timePtrToNullTime(in.RevokedAt),
		ObservedAt: in.ObservedAt,
	})
	if err != nil {
		return SkillRegistryRevocation{}, fmt.Errorf("upsert skill registry revocation: %w", err)
	}
	return revocationFromRow(gen.SkillRegistryRevocation(row)), nil
}

// GetSkillRegistryRevocation answers "is this exact release withdrawn", which
// is the question an install asks even when the registry cannot be reached.
func (s *Store) GetSkillRegistryRevocation(
	ctx context.Context, registryID, skillID, version string,
) (SkillRegistryRevocation, bool, error) {
	row, err := s.qr.GetSkillRegistryRevocation(ctx, gen.GetSkillRegistryRevocationParams{
		RegistryID: registryID, SkillID: skillID, Version: version,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillRegistryRevocation{}, false, nil
		}
		return SkillRegistryRevocation{}, false, fmt.Errorf("get skill registry revocation: %w", err)
	}
	return revocationFromRow(gen.SkillRegistryRevocation(row)), true, nil
}

// ListSkillRegistryRevocations returns one registry's withdrawals.
func (s *Store) ListSkillRegistryRevocations(
	ctx context.Context, registryID string,
) ([]SkillRegistryRevocation, error) {
	rows, err := s.qr.ListSkillRegistryRevocations(ctx, registryID)
	if err != nil {
		return nil, fmt.Errorf("list skill registry revocations: %w", err)
	}
	out := make([]SkillRegistryRevocation, 0, len(rows))
	for _, row := range rows {
		out = append(out, revocationFromRow(gen.SkillRegistryRevocation(row)))
	}
	return out, nil
}

// ListAllSkillRegistryRevocations returns every recorded withdrawal.
func (s *Store) ListAllSkillRegistryRevocations(ctx context.Context) ([]SkillRegistryRevocation, error) {
	rows, err := s.qr.ListAllSkillRegistryRevocations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all skill registry revocations: %w", err)
	}
	out := make([]SkillRegistryRevocation, 0, len(rows))
	for _, row := range rows {
		out = append(out, revocationFromRow(gen.SkillRegistryRevocation(row)))
	}
	return out, nil
}

func statusFromRow(row gen.SkillRegistryStatus) SkillRegistryStatus {
	return SkillRegistryStatus{
		RegistryID:           row.RegistryID,
		LastProbeState:       skillregistry.ProbeState(row.LastProbeState),
		LastProbeDetail:      row.LastProbeDetail,
		LastProbeAt:          nullTimeToPtr(row.LastProbeAt),
		LastProbeLatency:     time.Duration(row.LastProbeLatencyMs) * time.Millisecond,
		LastSyncAt:           nullTimeToPtr(row.LastSyncAt),
		LastRevocationSyncAt: nullTimeToPtr(row.LastRevocationSyncAt),
		UpdatedAt:            row.UpdatedAt,
	}
}

func revocationFromRow(row gen.SkillRegistryRevocation) SkillRegistryRevocation {
	return SkillRegistryRevocation{
		RegistryID: row.RegistryID,
		SkillID:    row.SkillID,
		Version:    row.Version,
		Reason:     row.Reason,
		RevokedAt:  nullTimeToPtr(row.RevokedAt),
		ObservedAt: row.ObservedAt,
	}
}
