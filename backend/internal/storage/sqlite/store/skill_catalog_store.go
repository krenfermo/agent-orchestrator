package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// This file is the durable half of the skill catalog (migration 0161). The
// rules live in internal/skillcatalog; this layer only reads and writes rows,
// so the file-backed Registry and the database decide identically.

// SkillInstallRecord is one installed package as persisted. It carries the
// full validated manifest so a list or detail view needs no disk read -- and
// so a package whose files vanished can still be reported and uninstalled
// rather than becoming unmanageable.
type SkillInstallRecord struct {
	Manifest    skillcatalog.Manifest
	Digest      string
	PackageDir  string
	InstalledAt time.Time
	InstalledBy string
}

// SkillActivationRecord is one project's activation of one skill.
type SkillActivationRecord struct {
	ProjectID domain.ProjectID
	SkillID   string
	Version   string
	Enabled   bool
	Grant     skillcatalog.Grant
	UpdatedAt time.Time
}

// SkillAuditAction names one recorded change to the catalog.
type SkillAuditAction string

// The recorded catalog actions. install_rejected is here because a package
// that FAILED verification is the entry an operator most wants to find later.
const (
	SkillAuditInstall         SkillAuditAction = "install"
	SkillAuditUninstall       SkillAuditAction = "uninstall"
	SkillAuditEnable          SkillAuditAction = "enable"
	SkillAuditDisable         SkillAuditAction = "disable"
	SkillAuditGrantChanged    SkillAuditAction = "grant_changed"
	SkillAuditInstallRejected SkillAuditAction = "install_rejected"
	// Phase 8, the image trust root (migration 0163). These live in the same
	// trail as the install/enable actions above because it is the same reviewer
	// asking the same kind of question, and two audit tables would be two
	// places to forget to look.
	SkillAuditImageApproved SkillAuditAction = "image_approved"
	SkillAuditImageRevoked  SkillAuditAction = "image_revoked"
	SkillAuditRunExecuted   SkillAuditAction = "run_executed"
	SkillAuditRunRefused    SkillAuditAction = "run_refused"
	// Phase 10, the registry / marketplace foundation (migration 0164). Same
	// trail again: "which registry did this package come from, and who decided
	// this installation may install from it" is the same reviewer's question.
	//
	// There is deliberately no skill_searched and no skill_release_viewed. A
	// search query is text a person typed, it can name an internal package or
	// a vulnerability they are hunting, and a row per search buys an auditor
	// nothing the install trail does not already carry.
	SkillAuditRegistryAdded      SkillAuditAction = "registry_added"
	SkillAuditRegistryUpdated    SkillAuditAction = "registry_updated"
	SkillAuditRegistryRemoved    SkillAuditAction = "registry_removed"
	SkillAuditInstallRefused     SkillAuditAction = "install_refused"
	SkillAuditUpdateAvailable    SkillAuditAction = "update_available"
	SkillAuditUpdateInstalled    SkillAuditAction = "update_installed"
	SkillAuditReleaseRevokedSeen SkillAuditAction = "release_revoked_seen"
	// Phase 11, private-registry connectivity (migration 0165). These record
	// the decisions that only exist once a registry is on the other end of a
	// socket: whether it was reachable, whether its credential worked, when AO
	// started moving bytes and when it refused to.
	//
	// registry_auth_failed is separate from install_refused because it is the
	// one an operator can FIX, and it is the one that means somebody's
	// credential expired rather than somebody's package failed a check. Its
	// detail names a secretRef and never a value.
	//
	// cached_artifact_used exists so an install served from cache is
	// distinguishable in the trail from one that went to the network. They
	// verify identically -- both recompute both digests over the bytes -- but
	// "where did these bytes come from" is exactly the question an incident
	// review asks, and an entry that could not answer it would be an entry
	// nobody trusts.
	//
	// Still deliberately absent, for the reason recorded above and in 0164: a
	// row per search. Reaching a private registry does not make the query worth
	// keeping; it makes it more sensitive.
	SkillAuditRegistryEnabled          SkillAuditAction = "registry_enabled"
	SkillAuditRegistryDisabled         SkillAuditAction = "registry_disabled"
	SkillAuditRegistryConnectionTested SkillAuditAction = "registry_connection_tested"
	SkillAuditRegistryAuthFailed       SkillAuditAction = "registry_auth_failed"
	SkillAuditSkillFetchStarted        SkillAuditAction = "skill_fetch_started"
	SkillAuditSkillFetchRefused        SkillAuditAction = "skill_fetch_refused"
	SkillAuditCachedArtifactUsed       SkillAuditAction = "cached_artifact_used"
	SkillAuditRevocationsSynced        SkillAuditAction = "registry_revocations_synced"
)

// Skills phase 12: the trust decisions (migration 0166).
//
// They are in this trail rather than a new one because it is the same reviewer
// asking a continuous question -- "how did this package get here and what did
// AO check" -- and two audit tables are two places to forget to look.
//
// None of these carries key material beyond what is public. signing_key_seen
// and signature_verified record a key id and a fingerprint, both derived from
// a public key and both meant to be compared out of band. Nothing here has a
// place for a private key, a credential, or the full signed payload.
const (
	SkillAuditTrustRootAdded   SkillAuditAction = "trust_root_added"
	SkillAuditTrustRootUpdated SkillAuditAction = "trust_root_updated"
	SkillAuditTrustRootRevoked SkillAuditAction = "trust_root_revoked"

	// SkillAuditSigningKeySeen records the first time AO encountered a key id
	// in a signature it checked. It is deliberately separate from
	// signature_verified: a key showing up that nobody expected is worth
	// noticing whether or not its signature checked out.
	SkillAuditSigningKeySeen    SkillAuditAction = "signing_key_seen"
	SkillAuditSigningKeyAdded   SkillAuditAction = "signing_key_added"
	SkillAuditSigningKeyRotated SkillAuditAction = "signing_key_rotated"
	SkillAuditSigningKeyRevoked SkillAuditAction = "signing_key_revoked"
	SkillAuditKeyRevokedSeen    SkillAuditAction = "key_revoked_seen"

	SkillAuditSignatureVerified SkillAuditAction = "signature_verified"
	SkillAuditSignatureRefused  SkillAuditAction = "signature_refused"
	SkillAuditPublisherMismatch SkillAuditAction = "publisher_mismatch"
	SkillAuditPublisherRevoked  SkillAuditAction = "publisher_revoked"

	// SkillAuditTrustedInstall and its refusal are the two lines an auditor
	// looks for first: what reached trusted on this host, and what tried and
	// was turned away.
	SkillAuditTrustedInstall        SkillAuditAction = "trusted_install"
	SkillAuditTrustedInstallRefused SkillAuditAction = "trusted_install_refused"
)

// SkillAuditEntry is one row of the catalog's audit trail.
type SkillAuditEntry struct {
	ID           string
	OccurredAt   time.Time
	Actor        string
	Action       SkillAuditAction
	SkillID      string
	Version      string
	ProjectID    *domain.ProjectID
	Digest       string
	Capabilities []skillcatalog.Capability
	Detail       string
}

// UpsertSkillInstall records an installed package version. The caller has
// already validated and digest-verified the package; this writes what it
// verified.
func (s *Store) UpsertSkillInstall(ctx context.Context, rec SkillInstallRecord) (SkillInstallRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	manifestJSON, err := json.Marshal(rec.Manifest)
	if err != nil {
		return SkillInstallRecord{}, fmt.Errorf("marshal skill manifest: %w", err)
	}
	row, err := s.qw.UpsertSkillInstall(ctx, gen.UpsertSkillInstallParams{
		SkillID:      rec.Manifest.ID,
		Version:      rec.Manifest.Version,
		Name:         rec.Manifest.Name,
		Description:  rec.Manifest.Description,
		RiskLevel:    string(rec.Manifest.RiskLevel),
		OriginType:   string(rec.Manifest.Origin.Type),
		OriginRef:    rec.Manifest.Origin.Ref,
		Digest:       rec.Digest,
		ManifestJson: string(manifestJSON),
		PackageDir:   rec.PackageDir,
		InstalledAt:  rec.InstalledAt,
		InstalledBy:  rec.InstalledBy,
	})
	if err != nil {
		return SkillInstallRecord{}, fmt.Errorf("upsert skill install: %w", err)
	}
	return skillInstallFromRow(row)
}

// GetSkillInstall returns one installed version, or (zero, false, nil).
func (s *Store) GetSkillInstall(ctx context.Context, skillID, version string) (SkillInstallRecord, bool, error) {
	row, err := s.qr.GetSkillInstall(ctx, gen.GetSkillInstallParams{SkillID: skillID, Version: version})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillInstallRecord{}, false, nil
		}
		return SkillInstallRecord{}, false, fmt.Errorf("get skill install: %w", err)
	}
	rec, err := skillInstallFromRow(row)
	return rec, err == nil, err
}

// ListSkillInstalls returns every installed version.
func (s *Store) ListSkillInstalls(ctx context.Context) ([]SkillInstallRecord, error) {
	rows, err := s.qr.ListSkillInstalls(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill installs: %w", err)
	}
	out := make([]SkillInstallRecord, 0, len(rows))
	for _, row := range rows {
		rec, err := skillInstallFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// DeleteSkillInstall removes one installed version. It reports whether a row
// was removed so the caller can distinguish "uninstalled" from "was not there".
func (s *Store) DeleteSkillInstall(ctx context.Context, skillID, version string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteSkillInstall(ctx, gen.DeleteSkillInstallParams{SkillID: skillID, Version: version})
	if err != nil {
		return false, fmt.Errorf("delete skill install: %w", err)
	}
	return n > 0, nil
}

// UpsertSkillActivation writes one project's activation. There is one row per
// (project, skill), so enabling a different version replaces the previous
// activation rather than adding a second grant beside it.
func (s *Store) UpsertSkillActivation(ctx context.Context, rec SkillActivationRecord) (SkillActivationRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	caps, err := json.Marshal(nonNilCapabilities(rec.Grant.Capabilities))
	if err != nil {
		return SkillActivationRecord{}, fmt.Errorf("marshal granted capabilities: %w", err)
	}
	var approvedAt sql.NullTime
	if !rec.Grant.ApprovedAt.IsZero() {
		approvedAt = sql.NullTime{Time: rec.Grant.ApprovedAt, Valid: true}
	}
	row, err := s.qw.UpsertSkillActivation(ctx, gen.UpsertSkillActivationParams{
		ProjectID:    rec.ProjectID,
		SkillID:      rec.SkillID,
		Version:      rec.Version,
		Enabled:      boolToInt64(rec.Enabled),
		Capabilities: string(caps),
		ApprovedBy:   rec.Grant.ApprovedBy,
		ApprovedAt:   approvedAt,
		UpdatedAt:    rec.UpdatedAt,
	})
	if err != nil {
		return SkillActivationRecord{}, fmt.Errorf("upsert skill activation: %w", err)
	}
	return skillActivationFromRow(row)
}

// GetSkillActivation returns one project's activation of one skill.
func (s *Store) GetSkillActivation(ctx context.Context, projectID domain.ProjectID, skillID string) (SkillActivationRecord, bool, error) {
	row, err := s.qr.GetSkillActivation(ctx, gen.GetSkillActivationParams{ProjectID: projectID, SkillID: skillID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillActivationRecord{}, false, nil
		}
		return SkillActivationRecord{}, false, fmt.Errorf("get skill activation: %w", err)
	}
	rec, err := skillActivationFromRow(row)
	return rec, err == nil, err
}

// ListSkillActivationsForProject returns every activation row for a project,
// enabled or not.
func (s *Store) ListSkillActivationsForProject(ctx context.Context, projectID domain.ProjectID) ([]SkillActivationRecord, error) {
	rows, err := s.qr.ListSkillActivationsForProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list skill activations: %w", err)
	}
	return skillActivationsFromRows(rows)
}

// ListSkillActivationsForSkillVersion returns every project that has an
// activation row pinned to one version. Uninstall consults this so it can
// refuse while a project still depends on the files it would remove.
func (s *Store) ListSkillActivationsForSkillVersion(ctx context.Context, skillID, version string) ([]SkillActivationRecord, error) {
	rows, err := s.qr.ListSkillActivationsForSkillVersion(ctx,
		gen.ListSkillActivationsForSkillVersionParams{SkillID: skillID, Version: version})
	if err != nil {
		return nil, fmt.Errorf("list skill activations for version: %w", err)
	}
	return skillActivationsFromRows(rows)
}

// DeleteSkillActivation removes one activation row entirely. Disabling keeps
// the row (with the grant cleared) so the history survives; this is used when
// the pinned version is uninstalled and the row would otherwise dangle.
func (s *Store) DeleteSkillActivation(ctx context.Context, projectID domain.ProjectID, skillID string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteSkillActivation(ctx, gen.DeleteSkillActivationParams{ProjectID: projectID, SkillID: skillID})
	if err != nil {
		return false, fmt.Errorf("delete skill activation: %w", err)
	}
	return n > 0, nil
}

// AppendSkillAudit records one change to the catalog. The trail is append-only
// and outlives the package it describes: the most interesting rows are about a
// package that has since been uninstalled.
func (s *Store) AppendSkillAudit(ctx context.Context, entry SkillAuditEntry) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	caps, err := json.Marshal(nonNilCapabilities(entry.Capabilities))
	if err != nil {
		return fmt.Errorf("marshal audit capabilities: %w", err)
	}
	return s.qw.InsertSkillAuditEntry(ctx, gen.InsertSkillAuditEntryParams{
		ID:           entry.ID,
		OccurredAt:   entry.OccurredAt,
		Actor:        entry.Actor,
		Action:       string(entry.Action),
		SkillID:      entry.SkillID,
		Version:      entry.Version,
		ProjectID:    entry.ProjectID,
		Digest:       entry.Digest,
		Capabilities: string(caps),
		Detail:       entry.Detail,
	})
}

// ListSkillAuditForSkill returns the trail for one skill id.
func (s *Store) ListSkillAuditForSkill(ctx context.Context, skillID string) ([]SkillAuditEntry, error) {
	rows, err := s.qr.ListSkillAuditForSkill(ctx, skillID)
	if err != nil {
		return nil, fmt.Errorf("list skill audit: %w", err)
	}
	out := make([]SkillAuditEntry, 0, len(rows))
	for _, row := range rows {
		entry, err := skillAuditFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// ListSkillAuditForProject returns the trail for one project.
func (s *Store) ListSkillAuditForProject(ctx context.Context, projectID domain.ProjectID) ([]SkillAuditEntry, error) {
	rows, err := s.qr.ListSkillAuditForProject(ctx, &projectID)
	if err != nil {
		return nil, fmt.Errorf("list project skill audit: %w", err)
	}
	out := make([]SkillAuditEntry, 0, len(rows))
	for _, row := range rows {
		entry, err := skillAuditFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

func skillInstallFromRow(row gen.SkillInstall) (SkillInstallRecord, error) {
	var manifest skillcatalog.Manifest
	if err := json.Unmarshal([]byte(row.ManifestJson), &manifest); err != nil {
		return SkillInstallRecord{}, fmt.Errorf("decode stored manifest for %s@%s: %w",
			row.SkillID, row.Version, err)
	}
	return SkillInstallRecord{
		Manifest:    manifest,
		Digest:      row.Digest,
		PackageDir:  row.PackageDir,
		InstalledAt: row.InstalledAt,
		InstalledBy: row.InstalledBy,
	}, nil
}

func skillActivationsFromRows(rows []gen.SkillActivation) ([]SkillActivationRecord, error) {
	out := make([]SkillActivationRecord, 0, len(rows))
	for _, row := range rows {
		rec, err := skillActivationFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func skillActivationFromRow(row gen.SkillActivation) (SkillActivationRecord, error) {
	caps, err := decodeCapabilities(row.Capabilities)
	if err != nil {
		return SkillActivationRecord{}, fmt.Errorf("decode grant for %s on %s: %w",
			row.SkillID, row.ProjectID, err)
	}
	grant := skillcatalog.Grant{Capabilities: caps, ApprovedBy: row.ApprovedBy}
	if row.ApprovedAt.Valid {
		grant.ApprovedAt = row.ApprovedAt.Time
	}
	return SkillActivationRecord{
		ProjectID: row.ProjectID,
		SkillID:   row.SkillID,
		Version:   row.Version,
		Enabled:   row.Enabled != 0,
		Grant:     grant,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

func skillAuditFromRow(row gen.SkillAudit) (SkillAuditEntry, error) {
	caps, err := decodeCapabilities(row.Capabilities)
	if err != nil {
		return SkillAuditEntry{}, fmt.Errorf("decode audit capabilities for %s: %w", row.ID, err)
	}
	return SkillAuditEntry{
		ID:           row.ID,
		OccurredAt:   row.OccurredAt,
		Actor:        row.Actor,
		Action:       SkillAuditAction(row.Action),
		SkillID:      row.SkillID,
		Version:      row.Version,
		ProjectID:    row.ProjectID,
		Digest:       row.Digest,
		Capabilities: caps,
		Detail:       row.Detail,
	}, nil
}

func decodeCapabilities(raw string) ([]skillcatalog.Capability, error) {
	if raw == "" {
		return nil, nil
	}
	var caps []skillcatalog.Capability
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return nil, err
	}
	return caps, nil
}

// nonNilCapabilities keeps an empty grant marshalling as [] rather than null,
// so a disabled row reads as "granted nothing" instead of "unknown".
func nonNilCapabilities(caps []skillcatalog.Capability) []skillcatalog.Capability {
	if caps == nil {
		return []skillcatalog.Capability{}
	}
	return caps
}
