package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// This file is the durable half of the skill registry (migration 0164).
//
// Two kinds of row, answering two different questions:
//
//   skill_registries       which sources this installation may install from
//   skill_install_origins  where each installed version actually came from,
//                          and what AO verified about it
//
// The rules live in internal/skillregistry; this layer only reads and writes
// rows. In particular it does NOT decide visibility: Registry.VisibleTo is one
// rule applied in Go, because a tenant filter written into three SQL statements
// is how one of them ends up wrong.

// SkillInstallOrigin is the recorded provenance of one installed version.
//
// It is a separate record from SkillInstallRecord because it answers a
// different question and has a different lifetime rule: the install says what
// is here, and this says where it came from and what AO checked. A
// local-directory install has no origin row at all, which is the honest shape --
// an absent row rather than a table half full of nulls.
type SkillInstallOrigin struct {
	SkillID string
	Version string

	// The registry as it was AT INSTALL TIME, copied rather than joined so the
	// provenance survives the registry being disabled or removed.
	RegistryID       string
	RegistryName     string
	RegistryType     string
	RegistryLocation string

	Publisher string
	SourceURL string

	ManifestDigest string
	ArtifactDigest string

	// TrustState is what AO can honestly say about this install.
	TrustState skillregistry.TrustState
	// TrustPolicy is what the registry required beyond integrity.
	TrustPolicy skillregistry.TrustPolicy
	// Provenance is what the release CLAIMED, recorded verbatim. AO
	// interpreted none of it.
	Provenance skillregistry.Provenance

	// CompatibilityVerdict records whether the check ran, not only what it
	// said: "unknown" must never read as a pass.
	CompatibilityVerdict skillregistry.CompatibilityVerdict

	PublishedAt *time.Time
	InstalledAt time.Time
	InstalledBy string

	// Revocation OBSERVED after the install. AO never uninstalls on its own.
	RevokedAt        *time.Time
	RevocationReason string
	RevocationSeenAt *time.Time
}

// Revoked reports whether AO has observed this installed release as withdrawn.
func (o SkillInstallOrigin) Revoked() bool { return o.RevokedAt != nil }

// UpsertSkillRegistry writes one registry configuration.
func (s *Store) UpsertSkillRegistry(
	ctx context.Context, reg skillregistry.Registry, at time.Time, actor string,
) (skillregistry.Registry, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillRegistry(ctx, gen.UpsertSkillRegistryParams{
		ID:                   reg.ID,
		DisplayName:          reg.DisplayName,
		Type:                 string(reg.Type),
		Location:             reg.Location,
		Enabled:              boolToInt64(reg.Enabled),
		TrustPolicy:          string(reg.TrustPolicy),
		PinnedPublisher:      reg.PinnedPublisher,
		Priority:             int64(reg.Priority),
		TenantID:             tenantPtr(reg),
		CredentialSecretName: reg.CredentialSecretName,
		AuthType:             string(reg.EffectiveAuthType()),
		ApiKeyHeader:         reg.APIKeyHeader,
		// The network policy is stored as JSON because it is a policy with
		// room to grow, not a single value: today it is a CIDR list, and the
		// next thing an on-premises registry needs (a client certificate ref,
		// a proxy exception) belongs beside it rather than in a new column
		// each time.
		NetworkPolicy: encodeNetworkPolicy(reg.NetworkPolicy),
		CreatedAt:     at,
		CreatedBy:     actor,
		UpdatedAt:     at,
		UpdatedBy:     actor,
	})
	if err != nil {
		return skillregistry.Registry{}, fmt.Errorf("upsert skill registry: %w", err)
	}
	return registryFromRow(row), nil
}

// GetSkillRegistry returns one configured registry, or (zero, false, nil).
func (s *Store) GetSkillRegistry(ctx context.Context, id string) (skillregistry.Registry, bool, error) {
	row, err := s.qr.GetSkillRegistry(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillregistry.Registry{}, false, nil
		}
		return skillregistry.Registry{}, false, fmt.Errorf("get skill registry: %w", err)
	}
	return registryFromRow(row), true, nil
}

// ListSkillRegistries returns every configured registry, unfiltered.
//
// Unfiltered is deliberate: tenant visibility is decided by
// skillregistry.Registry.VisibleTo, in one place, over the full set. A store
// method that quietly applied the filter would leave the caller unable to tell
// "no registries are configured" from "none of them are yours", and those are
// different things to say on screen.
func (s *Store) ListSkillRegistries(ctx context.Context) ([]skillregistry.Registry, error) {
	rows, err := s.qr.ListSkillRegistries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill registries: %w", err)
	}
	out := make([]skillregistry.Registry, 0, len(rows))
	for _, row := range rows {
		out = append(out, registryFromRow(row))
	}
	skillregistry.SortRegistries(out)
	return out, nil
}

// DeleteSkillRegistry removes one configuration. It leaves every install that
// came from it, and every provenance row describing them, exactly where they
// are: removing a compromised registry must not erase the evidence of what it
// served.
func (s *Store) DeleteSkillRegistry(ctx context.Context, id string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteSkillRegistry(ctx, id)
	if err != nil {
		return false, fmt.Errorf("delete skill registry: %w", err)
	}
	return n > 0, nil
}

// UpsertSkillInstallOrigin records where one installed version came from.
func (s *Store) UpsertSkillInstallOrigin(
	ctx context.Context, o SkillInstallOrigin,
) (SkillInstallOrigin, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillInstallOrigin(ctx, gen.UpsertSkillInstallOriginParams{
		SkillID:              o.SkillID,
		Version:              o.Version,
		RegistryID:           o.RegistryID,
		RegistryName:         o.RegistryName,
		RegistryType:         o.RegistryType,
		RegistryLocation:     o.RegistryLocation,
		Publisher:            o.Publisher,
		SourceURL:            o.SourceURL,
		ManifestDigest:       o.ManifestDigest,
		ArtifactDigest:       o.ArtifactDigest,
		TrustState:           string(o.TrustState),
		TrustPolicy:          string(o.TrustPolicy),
		SignatureFormat:      o.Provenance.SignatureFormat,
		Signature:            o.Provenance.Signature,
		KeyID:                o.Provenance.KeyID,
		AttestationURL:       o.Provenance.AttestationURL,
		CompatibilityVerdict: string(o.CompatibilityVerdict),
		PublishedAt:          timePtrToNullTime(o.PublishedAt),
		InstalledAt:          o.InstalledAt,
		InstalledBy:          o.InstalledBy,
		RevokedAt:            timePtrToNullTime(o.RevokedAt),
		RevocationReason:     o.RevocationReason,
		RevocationSeenAt:     timePtrToNullTime(o.RevocationSeenAt),
	})
	if err != nil {
		return SkillInstallOrigin{}, fmt.Errorf("upsert skill install origin: %w", err)
	}
	return originFromRow(row), nil
}

// GetSkillInstallOrigin returns one install's provenance, or (zero, false, nil).
func (s *Store) GetSkillInstallOrigin(
	ctx context.Context, skillID, version string,
) (SkillInstallOrigin, bool, error) {
	row, err := s.qr.GetSkillInstallOrigin(ctx, gen.GetSkillInstallOriginParams{
		SkillID: skillID, Version: version,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillInstallOrigin{}, false, nil
		}
		return SkillInstallOrigin{}, false, fmt.Errorf("get skill install origin: %w", err)
	}
	return originFromRow(row), true, nil
}

// ListSkillInstallOrigins returns every recorded provenance row.
func (s *Store) ListSkillInstallOrigins(ctx context.Context) ([]SkillInstallOrigin, error) {
	rows, err := s.qr.ListSkillInstallOrigins(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill install origins: %w", err)
	}
	return originsFromRows(rows), nil
}

// ListSkillInstallOriginsForSkill returns every version's provenance for one
// skill id. It is what answers "which registry does this skill belong to",
// which is the question the cross-registry collision rule asks.
func (s *Store) ListSkillInstallOriginsForSkill(
	ctx context.Context, skillID string,
) ([]SkillInstallOrigin, error) {
	rows, err := s.qr.ListSkillInstallOriginsForSkill(ctx, skillID)
	if err != nil {
		return nil, fmt.Errorf("list skill install origins for skill: %w", err)
	}
	return originsFromRows(rows), nil
}

// MarkSkillInstallOriginRevoked records a revocation AO observed after the
// install. It returns false when the row was already marked, so the caller
// audits release_revoked_seen exactly once rather than on every check.
func (s *Store) MarkSkillInstallOriginRevoked(
	ctx context.Context, skillID, version, reason string, at time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.MarkSkillInstallOriginRevoked(ctx, gen.MarkSkillInstallOriginRevokedParams{
		RevokedAt:        sql.NullTime{Time: at, Valid: true},
		RevocationReason: reason,
		RevocationSeenAt: sql.NullTime{Time: at, Valid: true},
		SkillID:          skillID,
		Version:          version,
	})
	if err != nil {
		return false, fmt.Errorf("mark skill install origin revoked: %w", err)
	}
	return n > 0, nil
}

// tenantPtr maps an empty tenant onto NULL rather than onto the default
// tenant. "Installation-wide" and "belongs to the default organization" are
// different facts, and a registry that silently acquired a tenant would become
// invisible to every other one.
func tenantPtr(reg skillregistry.Registry) *domain.TenantID {
	if reg.TenantID == "" {
		return nil
	}
	t := reg.TenantID
	return &t
}

func registryFromRow(row gen.SkillRegistry) skillregistry.Registry {
	reg := skillregistry.Registry{
		ID:                   row.ID,
		DisplayName:          row.DisplayName,
		Type:                 skillregistry.RegistryType(row.Type),
		Location:             row.Location,
		Enabled:              row.Enabled != 0,
		TrustPolicy:          skillregistry.TrustPolicy(row.TrustPolicy),
		PinnedPublisher:      row.PinnedPublisher,
		Priority:             int(row.Priority),
		CredentialSecretName: row.CredentialSecretName,
		AuthType:             skillregistry.AuthType(row.AuthType),
		APIKeyHeader:         row.ApiKeyHeader,
		NetworkPolicy:        decodeNetworkPolicy(row.NetworkPolicy),
		CreatedAt:            row.CreatedAt,
		UpdatedAt:            row.UpdatedAt,
	}
	if row.TenantID != nil {
		reg.TenantID = *row.TenantID
	}
	return reg
}

// encodeNetworkPolicy renders the policy for the column. A policy that will not
// marshal is stored as the empty one, which is the FULL denylist -- the safe
// direction, and the only one available when the alternative is storing
// something the reader cannot enforce.
func encodeNetworkPolicy(p skillregistry.NetworkPolicy) string {
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// decodeNetworkPolicy reads it back. A malformed column yields the empty
// policy, for the same reason: an unreadable exception must widen nothing.
func decodeNetworkPolicy(raw string) skillregistry.NetworkPolicy {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return skillregistry.NetworkPolicy{}
	}
	var p skillregistry.NetworkPolicy
	if err := json.Unmarshal([]byte(trimmed), &p); err != nil {
		return skillregistry.NetworkPolicy{}
	}
	return p
}

func originsFromRows(rows []gen.SkillInstallOrigin) []SkillInstallOrigin {
	out := make([]SkillInstallOrigin, 0, len(rows))
	for _, row := range rows {
		out = append(out, originFromRow(row))
	}
	return out
}

func originFromRow(row gen.SkillInstallOrigin) SkillInstallOrigin {
	return SkillInstallOrigin{
		SkillID:          row.SkillID,
		Version:          row.Version,
		RegistryID:       row.RegistryID,
		RegistryName:     row.RegistryName,
		RegistryType:     row.RegistryType,
		RegistryLocation: row.RegistryLocation,
		Publisher:        row.Publisher,
		SourceURL:        row.SourceURL,
		ManifestDigest:   row.ManifestDigest,
		ArtifactDigest:   row.ArtifactDigest,
		TrustState:       skillregistry.TrustState(row.TrustState),
		TrustPolicy:      skillregistry.TrustPolicy(row.TrustPolicy),
		Provenance: skillregistry.Provenance{
			SignatureFormat: row.SignatureFormat,
			Signature:       row.Signature,
			KeyID:           row.KeyID,
			AttestationURL:  row.AttestationURL,
		},
		CompatibilityVerdict: skillregistry.CompatibilityVerdict(row.CompatibilityVerdict),
		PublishedAt:          nullTimeToPtr(row.PublishedAt),
		InstalledAt:          row.InstalledAt,
		InstalledBy:          row.InstalledBy,
		RevokedAt:            nullTimeToPtr(row.RevokedAt),
		RevocationReason:     row.RevocationReason,
		RevocationSeenAt:     nullTimeToPtr(row.RevocationSeenAt),
	}
}
