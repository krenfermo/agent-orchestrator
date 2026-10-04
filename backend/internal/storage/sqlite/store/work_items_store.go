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

// work_items_store.go — durable state for the external work-management
// integration (P4-E).
//
// The store never sees a plaintext credential. WorkItemConfig carries
// APITokenEncrypted, which is ciphertext internal/secretbox produced, exactly
// as app_settings does for the SMTP password — so a copied database file does
// not yield a token that can write to somebody's planning board.

// WorkItemConfig is one project's provider configuration.
type WorkItemConfig struct {
	ProjectID domain.ProjectID
	Provider  domain.WorkItemProvider
	BaseURL   string
	Workspace string

	ExternalProjectID   string
	ExternalProjectName string
	ExternalProjectKey  string

	// APITokenEncrypted is ciphertext. The store neither produces nor reads
	// plaintext; sealing and opening happen at the service boundary that owns
	// the secret box.
	APITokenEncrypted string

	Enabled      bool
	SyncStates   bool
	SyncComments bool

	LastCheckAt    time.Time
	LastCheckOK    bool
	LastCheckError string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Configured reports whether the row names enough to reach a provider. It is
// separate from Enabled because a half-filled form must still be savable —
// the same rule the email settings follow.
func (c WorkItemConfig) Configured() bool {
	return c.Workspace != "" && c.ExternalProjectID != "" && c.APITokenEncrypted != ""
}

// GetWorkItemConfig reads one project's configuration.
func (s *Store) GetWorkItemConfig(ctx context.Context, projectID domain.ProjectID) (WorkItemConfig, bool, error) {
	row, err := s.qr.GetWorkItemConfig(ctx, string(projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItemConfig{}, false, nil
	}
	if err != nil {
		return WorkItemConfig{}, false, fmt.Errorf("get work item config: %w", err)
	}
	return workItemConfigFromRow(row), true, nil
}

// ListEnabledWorkItemConfigs returns every project whose integration is on.
// It is the sync worker's working set.
func (s *Store) ListEnabledWorkItemConfigs(ctx context.Context) ([]WorkItemConfig, error) {
	rows, err := s.qr.ListWorkItemConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list work item configs: %w", err)
	}
	out := make([]WorkItemConfig, 0, len(rows))
	for _, row := range rows {
		out = append(out, workItemConfigFromRow(row))
	}
	return out, nil
}

// PutWorkItemConfig writes one project's configuration.
func (s *Store) PutWorkItemConfig(ctx context.Context, cfg WorkItemConfig, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	created := cfg.CreatedAt
	if created.IsZero() {
		created = now
	}
	err := s.qw.UpsertWorkItemConfig(ctx, gen.UpsertWorkItemConfigParams{
		ProjectID:           string(cfg.ProjectID),
		Provider:            string(cfg.Provider),
		BaseURL:             cfg.BaseURL,
		Workspace:           cfg.Workspace,
		ExternalProjectID:   cfg.ExternalProjectID,
		ExternalProjectName: cfg.ExternalProjectName,
		ExternalProjectKey:  cfg.ExternalProjectKey,
		ApiTokenEncrypted:   cfg.APITokenEncrypted,
		Enabled:             boolToInt(cfg.Enabled),
		SyncStates:          boolToInt(cfg.SyncStates),
		SyncComments:        boolToInt(cfg.SyncComments),
		CreatedAt:           created,
		UpdatedAt:           now,
	})
	if err != nil {
		return fmt.Errorf("upsert work item config: %w", err)
	}
	return nil
}

// SetWorkItemConfigCheck records the outcome of a preflight.
func (s *Store) SetWorkItemConfigCheck(
	ctx context.Context, projectID domain.ProjectID, ok bool, detail string, now time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.qw.SetWorkItemConfigCheck(ctx, gen.SetWorkItemConfigCheckParams{
		LastCheckAt:    nullTimeOrZero(now),
		LastCheckOk:    boolToInt(ok),
		LastCheckError: detail,
		UpdatedAt:      now,
		ProjectID:      string(projectID),
	})
	if err != nil {
		return fmt.Errorf("set work item config check: %w", err)
	}
	return nil
}

// DeleteWorkItemConfig removes a project's configuration, and with it the
// stored credential.
func (s *Store) DeleteWorkItemConfig(ctx context.Context, projectID domain.ProjectID) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.DeleteWorkItemConfig(ctx, string(projectID))
	if err != nil {
		return 0, fmt.Errorf("delete work item config: %w", err)
	}
	return n, nil
}

// --- links -----------------------------------------------------------------

// GetWorkItemLink reads one link by id.
func (s *Store) GetWorkItemLink(ctx context.Context, id string) (domain.WorkItemLink, bool, error) {
	row, err := s.qr.GetWorkItemLink(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkItemLink{}, false, nil
	}
	if err != nil {
		return domain.WorkItemLink{}, false, fmt.Errorf("get work item link: %w", err)
	}
	return workItemLinkFromRow(row), true, nil
}

// GetWorkItemLinkByScope reads the link attached to one AO thing.
func (s *Store) GetWorkItemLinkByScope(
	ctx context.Context, projectID domain.ProjectID, scope domain.WorkItemLinkScope, scopeID string,
) (domain.WorkItemLink, bool, error) {
	row, err := s.qr.GetWorkItemLinkByScope(ctx, gen.GetWorkItemLinkByScopeParams{
		ProjectID: string(projectID), Scope: string(scope), ScopeID: scopeID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkItemLink{}, false, nil
	}
	if err != nil {
		return domain.WorkItemLink{}, false, fmt.Errorf("get work item link by scope: %w", err)
	}
	return workItemLinkFromRow(row), true, nil
}

// ListWorkItemLinks returns every link in a project.
func (s *Store) ListWorkItemLinks(ctx context.Context, projectID domain.ProjectID) ([]domain.WorkItemLink, error) {
	rows, err := s.qr.ListWorkItemLinks(ctx, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("list work item links: %w", err)
	}
	out := make([]domain.WorkItemLink, 0, len(rows))
	for _, row := range rows {
		out = append(out, workItemLinkFromRow(row))
	}
	return out, nil
}

// ListWorkItemLinksForExternalItem answers "which AO work is this external
// item about", from indexed identifiers rather than from a title.
func (s *Store) ListWorkItemLinksForExternalItem(
	ctx context.Context, provider domain.WorkItemProvider, workspace, itemID string,
) ([]domain.WorkItemLink, error) {
	rows, err := s.qr.ListWorkItemLinksByExternalItem(ctx, gen.ListWorkItemLinksByExternalItemParams{
		Provider: string(provider), Workspace: workspace, ExternalItemID: itemID,
	})
	if err != nil {
		return nil, fmt.Errorf("list work item links by external item: %w", err)
	}
	out := make([]domain.WorkItemLink, 0, len(rows))
	for _, row := range rows {
		out = append(out, workItemLinkFromRow(row))
	}
	return out, nil
}

// PutWorkItemLink writes a link, replacing any link already attached to the
// same AO thing.
func (s *Store) PutWorkItemLink(ctx context.Context, link domain.WorkItemLink, now time.Time) error {
	if err := link.Validate(); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	created := link.CreatedAt
	if created.IsZero() {
		created = now
	}
	err := s.qw.UpsertWorkItemLink(ctx, gen.UpsertWorkItemLinkParams{
		ID:                link.ID,
		ProjectID:         string(link.ProjectID),
		Scope:             string(link.Scope),
		ScopeID:           link.ScopeID,
		Provider:          string(link.Ref.Provider),
		Workspace:         link.Ref.Workspace,
		ExternalProjectID: link.Ref.Project,
		ExternalItemID:    link.Ref.ID,
		ExternalItemKey:   link.Ref.Key,
		Origin:            string(link.Origin),
		SyncEnabled:       boolToInt(link.SyncEnabled),
		LastSeenTitle:     link.LastSeenTitle,
		LastSeenState:     string(link.LastSeenState),
		LastSeenAt:        nullTimeOrZero(link.LastSeenAt),
		CreatedBy:         link.CreatedBy,
		CreatedAt:         created,
		UpdatedAt:         now,
	})
	if err != nil {
		return fmt.Errorf("upsert work item link: %w", err)
	}
	return nil
}

// TouchWorkItemLinkSnapshot refreshes a link's display cache. It cannot
// re-target the link — a provider read must never change what a link points at.
func (s *Store) TouchWorkItemLinkSnapshot(
	ctx context.Context, id, title string, state domain.WorkItemStateGroup, now time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.qw.TouchWorkItemLinkSnapshot(ctx, gen.TouchWorkItemLinkSnapshotParams{
		LastSeenTitle: title,
		LastSeenState: string(state),
		LastSeenAt:    nullTimeOrZero(now),
		UpdatedAt:     now,
		ID:            id,
	})
	if err != nil {
		return fmt.Errorf("touch work item link snapshot: %w", err)
	}
	return nil
}

// SetWorkItemLinkSync turns state/comment pushing on or off for one link.
func (s *Store) SetWorkItemLinkSync(ctx context.Context, id string, enabled bool, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SetWorkItemLinkSync(ctx, gen.SetWorkItemLinkSyncParams{
		SyncEnabled: boolToInt(enabled), UpdatedAt: now, ID: id,
	})
	if err != nil {
		return 0, fmt.Errorf("set work item link sync: %w", err)
	}
	return n, nil
}

// DeleteWorkItemLink unlinks. The project id is part of the predicate rather
// than merely checked in Go: a delete that could address any link by id alone
// would be a cross-project write waiting for a caller to forget a check.
func (s *Store) DeleteWorkItemLink(ctx context.Context, projectID domain.ProjectID, id string) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.DeleteWorkItemLink(ctx, gen.DeleteWorkItemLinkParams{
		ID: id, ProjectID: string(projectID),
	})
	if err != nil {
		return 0, fmt.Errorf("delete work item link: %w", err)
	}
	return n, nil
}

// --- the sync outbox -------------------------------------------------------

// WorkItemSyncRow is one queued sync intent.
type WorkItemSyncRow struct {
	ID            string
	ProjectID     domain.ProjectID
	LinkID        string
	Event         domain.WorkItemSyncEvent
	Body          string
	TargetState   domain.WorkItemStateGroup
	DedupeKey     string
	Status        string
	Attempts      int64
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// The outbox statuses.
const (
	WorkItemSyncPending = "pending"
	WorkItemSyncDone    = "done"
	WorkItemSyncFailed  = "failed"
)

// EnqueueWorkItemSync records one sync intent, exactly once per dedupe key.
//
// It reports whether a row was actually inserted. A caller that gets false has
// not failed: the intent was already recorded, which is the whole point of the
// key and is what makes a duplicate lifecycle callback harmless.
func (s *Store) EnqueueWorkItemSync(ctx context.Context, row WorkItemSyncRow, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.EnqueueWorkItemSync(ctx, gen.EnqueueWorkItemSyncParams{
		ID:            row.ID,
		ProjectID:     string(row.ProjectID),
		LinkID:        row.LinkID,
		Event:         string(row.Event),
		Body:          row.Body,
		TargetState:   string(row.TargetState),
		DedupeKey:     row.DedupeKey,
		NextAttemptAt: nullTimeOrZero(row.NextAttemptAt),
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		return false, fmt.Errorf("enqueue work item sync: %w", err)
	}
	return n > 0, nil
}

// ClaimDueWorkItemSyncs returns the pending rows whose backoff has elapsed.
func (s *Store) ClaimDueWorkItemSyncs(ctx context.Context, now time.Time, limit int) ([]WorkItemSyncRow, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.qw.ClaimDueWorkItemSyncs(ctx, gen.ClaimDueWorkItemSyncsParams{
		NextAttemptAt: nullTimeOrZero(now), Limit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("claim work item syncs: %w", err)
	}
	out := make([]WorkItemSyncRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, workItemSyncFromRow(row))
	}
	return out, nil
}

// MarkWorkItemSyncDone settles a row successfully.
func (s *Store) MarkWorkItemSyncDone(ctx context.Context, id string, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkWorkItemSyncDone(ctx, gen.MarkWorkItemSyncDoneParams{UpdatedAt: now, ID: id})
	if err != nil {
		return 0, fmt.Errorf("mark work item sync done: %w", err)
	}
	return n, nil
}

// DeferWorkItemSync records a transient failure and schedules a retry.
func (s *Store) DeferWorkItemSync(ctx context.Context, id, reason string, next, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.DeferWorkItemSync(ctx, gen.DeferWorkItemSyncParams{
		NextAttemptAt: nullTimeOrZero(next), LastError: reason, UpdatedAt: now, ID: id,
	})
	if err != nil {
		return 0, fmt.Errorf("defer work item sync: %w", err)
	}
	return n, nil
}

// MarkWorkItemSyncFailed settles a row terminally, keeping the reason.
func (s *Store) MarkWorkItemSyncFailed(ctx context.Context, id, reason string, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkWorkItemSyncFailed(ctx, gen.MarkWorkItemSyncFailedParams{
		LastError: reason, UpdatedAt: now, ID: id,
	})
	if err != nil {
		return 0, fmt.Errorf("mark work item sync failed: %w", err)
	}
	return n, nil
}

// WorkItemSyncCounts reports the queue census for one project, which is what
// the UI renders as sync health.
func (s *Store) WorkItemSyncCounts(ctx context.Context, projectID domain.ProjectID) (map[string]int64, error) {
	rows, err := s.qr.CountWorkItemSyncByStatus(ctx, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("count work item syncs: %w", err)
	}
	out := map[string]int64{}
	for _, row := range rows {
		out[row.Status] = row.Total
	}
	return out, nil
}

// ListWorkItemSyncs returns a project's recent queue rows, newest first.
func (s *Store) ListWorkItemSyncs(ctx context.Context, projectID domain.ProjectID, limit int) ([]WorkItemSyncRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.qr.ListWorkItemSyncForProject(ctx, gen.ListWorkItemSyncForProjectParams{
		ProjectID: string(projectID), Limit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list work item syncs: %w", err)
	}
	out := make([]WorkItemSyncRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, workItemSyncFromRow(row))
	}
	return out, nil
}

// PruneSettledWorkItemSyncs removes drained rows older than a cutoff. Failed
// rows survive: they are the ones somebody still has to look at.
func (s *Store) PruneSettledWorkItemSyncs(ctx context.Context, before time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.DeleteSettledWorkItemSyncsBefore(ctx, before)
	if err != nil {
		return 0, fmt.Errorf("prune work item syncs: %w", err)
	}
	return n, nil
}

// --- audit -----------------------------------------------------------------

// WorkItemAuditRow is one recorded provider operation.
type WorkItemAuditRow struct {
	ID              string
	ProjectID       domain.ProjectID
	LinkID          string
	Provider        domain.WorkItemProvider
	Operation       string
	ExternalItemID  string
	ExternalItemKey string
	Outcome         string
	ErrorKind       string
	Detail          string
	Attempts        int64
	DurationMS      int64
	CreatedAt       time.Time
}

// The audit outcomes.
const (
	WorkItemAuditOK        = "ok"
	WorkItemAuditRetryable = "retryable"
	WorkItemAuditFailed    = "failed"
	WorkItemAuditSkipped   = "skipped"
)

// RecordWorkItemAudit appends one operation to the trail.
//
// Detail is only ever an adapter-produced, truncated message. Nothing that
// reaches this function has seen a request header, and the credential is not
// in scope at any call site.
func (s *Store) RecordWorkItemAudit(ctx context.Context, row WorkItemAuditRow, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.qw.InsertWorkItemSyncAudit(ctx, gen.InsertWorkItemSyncAuditParams{
		ID:              row.ID,
		ProjectID:       string(row.ProjectID),
		LinkID:          row.LinkID,
		Provider:        string(row.Provider),
		Operation:       row.Operation,
		ExternalItemID:  row.ExternalItemID,
		ExternalItemKey: row.ExternalItemKey,
		Outcome:         row.Outcome,
		ErrorKind:       row.ErrorKind,
		Detail:          row.Detail,
		Attempts:        row.Attempts,
		DurationMs:      row.DurationMS,
		CreatedAt:       now,
	})
	if err != nil {
		return fmt.Errorf("record work item audit: %w", err)
	}
	return nil
}

// ListWorkItemAudit returns a project's recent operations, newest first.
func (s *Store) ListWorkItemAudit(ctx context.Context, projectID domain.ProjectID, limit int) ([]WorkItemAuditRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.qr.ListWorkItemSyncAudit(ctx, gen.ListWorkItemSyncAuditParams{
		ProjectID: string(projectID), Limit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list work item audit: %w", err)
	}
	out := make([]WorkItemAuditRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, WorkItemAuditRow{
			ID: row.ID, ProjectID: domain.ProjectID(row.ProjectID), LinkID: row.LinkID,
			Provider: domain.WorkItemProvider(row.Provider), Operation: row.Operation,
			ExternalItemID: row.ExternalItemID, ExternalItemKey: row.ExternalItemKey,
			Outcome: row.Outcome, ErrorKind: row.ErrorKind, Detail: row.Detail,
			Attempts: row.Attempts, DurationMS: row.DurationMs, CreatedAt: row.CreatedAt,
		})
	}
	return out, nil
}

// --- row mapping -----------------------------------------------------------

func workItemConfigFromRow(row gen.WorkItemConfig) WorkItemConfig {
	return WorkItemConfig{
		ProjectID:           domain.ProjectID(row.ProjectID),
		Provider:            domain.WorkItemProvider(row.Provider),
		BaseURL:             row.BaseURL,
		Workspace:           row.Workspace,
		ExternalProjectID:   row.ExternalProjectID,
		ExternalProjectName: row.ExternalProjectName,
		ExternalProjectKey:  row.ExternalProjectKey,
		APITokenEncrypted:   row.ApiTokenEncrypted,
		Enabled:             row.Enabled != 0,
		SyncStates:          row.SyncStates != 0,
		SyncComments:        row.SyncComments != 0,
		LastCheckAt:         row.LastCheckAt.Time,
		LastCheckOK:         row.LastCheckOk != 0,
		LastCheckError:      row.LastCheckError,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
	}
}

func workItemLinkFromRow(row gen.WorkItemLink) domain.WorkItemLink {
	return domain.WorkItemLink{
		ID:        row.ID,
		ProjectID: domain.ProjectID(row.ProjectID),
		Scope:     domain.WorkItemLinkScope(row.Scope),
		ScopeID:   row.ScopeID,
		Ref: domain.WorkItemRef{
			Provider:  domain.WorkItemProvider(row.Provider),
			Workspace: row.Workspace,
			Project:   row.ExternalProjectID,
			ID:        row.ExternalItemID,
			Key:       row.ExternalItemKey,
		},
		Origin:        domain.WorkItemLinkOrigin(row.Origin),
		SyncEnabled:   row.SyncEnabled != 0,
		LastSeenTitle: row.LastSeenTitle,
		LastSeenState: domain.WorkItemStateGroup(row.LastSeenState),
		LastSeenAt:    row.LastSeenAt.Time,
		CreatedBy:     row.CreatedBy,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
}

func workItemSyncFromRow(row gen.WorkItemSyncOutbox) WorkItemSyncRow {
	return WorkItemSyncRow{
		ID:            row.ID,
		ProjectID:     domain.ProjectID(row.ProjectID),
		LinkID:        row.LinkID,
		Event:         domain.WorkItemSyncEvent(row.Event),
		Body:          row.Body,
		TargetState:   domain.WorkItemStateGroup(row.TargetState),
		DedupeKey:     row.DedupeKey,
		Status:        row.Status,
		Attempts:      row.Attempts,
		NextAttemptAt: row.NextAttemptAt.Time,
		LastError:     row.LastError,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
}
