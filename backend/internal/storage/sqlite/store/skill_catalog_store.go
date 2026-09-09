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
