package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// This file is the durable half of the image trust root (migration 0163).
//
// A row here is one administrator's recorded decision that this installation
// may execute one image digest, for one exact scope, to back one AO tool. The
// UNIQUE constraint on (scope, tool) is load-bearing: re-approving REPLACES, so
// "which image may this scope run" has exactly one answer and a second digest
// cannot accumulate quietly beside the first.
//
// Nothing here decides whether an approval is usable. Expiry and revocation are
// evaluated in Go, at the moment of use, so the same answer holds for any store
// and so revocation cannot be defeated by a stale read.

// UpsertSkillImageApproval writes one approval, replacing any existing one for
// the same scope and tool.
func (s *Store) UpsertSkillImageApproval(
	ctx context.Context, a skillimage.Approval,
) (skillimage.Approval, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillImageApproval(ctx, gen.UpsertSkillImageApprovalParams{
		ID:         a.ID,
		TenantID:   a.Scope.TenantID,
		ProjectID:  a.Scope.ProjectID,
		SkillID:    a.Scope.SkillID,
		Version:    a.Scope.Version,
		ModeID:     a.Scope.ModeID,
		Tool:       a.Tool,
		Reference:  a.Reference,
		Digest:     a.Digest,
		ApprovedBy: a.ApprovedBy,
		ApprovedAt: a.ApprovedAt,
		ExpiresAt:  timePtrToNullTime(a.ExpiresAt),
		RevokedAt:  timePtrToNullTime(a.RevokedAt),
		Note:       a.Note,
	})
	if err != nil {
		return skillimage.Approval{}, fmt.Errorf("upsert skill image approval: %w", err)
	}
	return imageApprovalFromRow(row), nil
}

// GetSkillImageApprovalForScope returns the approval written for one exact
// scope and tool, whether or not it is still active.
//
// It deliberately returns the row rather than filtering on expiry or revocation
// in SQL: a caller that asked "may I run this" needs to be told WHY not, and a
// query that returned nothing for a revoked approval would make "revoked" and
// "never approved" the same answer.
func (s *Store) GetSkillImageApprovalForScope(
	ctx context.Context, scope skillimage.Scope, tool string,
) (skillimage.Approval, bool, error) {
	row, err := s.qr.GetSkillImageApprovalForScope(ctx, gen.GetSkillImageApprovalForScopeParams{
		TenantID:  scope.TenantID,
		ProjectID: scope.ProjectID,
		SkillID:   scope.SkillID,
		Version:   scope.Version,
		ModeID:    scope.ModeID,
		Tool:      tool,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return skillimage.Approval{}, false, nil
	}
	if err != nil {
		return skillimage.Approval{}, false, fmt.Errorf("get skill image approval for scope: %w", err)
	}
	return imageApprovalFromRow(row), true, nil
}

// GetSkillImageApproval returns one approval by id.
func (s *Store) GetSkillImageApproval(ctx context.Context, id string) (skillimage.Approval, bool, error) {
	row, err := s.qr.GetSkillImageApprovalByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return skillimage.Approval{}, false, nil
	}
	if err != nil {
		return skillimage.Approval{}, false, fmt.Errorf("get skill image approval: %w", err)
	}
	return imageApprovalFromRow(row), true, nil
}

// ListSkillImageApprovals returns every approval, active or not. An
// administrative view needs the revoked and expired ones too: they are the
// history of what this installation once allowed.
func (s *Store) ListSkillImageApprovals(ctx context.Context) ([]skillimage.Approval, error) {
	rows, err := s.qr.ListSkillImageApprovals(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill image approvals: %w", err)
	}
	out := make([]skillimage.Approval, 0, len(rows))
	for _, row := range rows {
		out = append(out, imageApprovalFromRow(row))
	}
	return out, nil
}

// ListSkillImageApprovalsForProject narrows the listing to one project, which
// is how a project-scoped reader stays inside its own tenant's data.
func (s *Store) ListSkillImageApprovalsForProject(
	ctx context.Context, projectID domain.ProjectID,
) ([]skillimage.Approval, error) {
	rows, err := s.qr.ListSkillImageApprovalsForProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list skill image approvals for project: %w", err)
	}
	out := make([]skillimage.Approval, 0, len(rows))
	for _, row := range rows {
		out = append(out, imageApprovalFromRow(row))
	}
	return out, nil
}

// RevokeSkillImageApproval stamps a revocation. It reports whether a row
// changed, so a second revoke of the same approval is distinguishable from a
// revoke of something that was never approved.
func (s *Store) RevokeSkillImageApproval(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeSkillImageApproval(ctx, gen.RevokeSkillImageApprovalParams{
		RevokedAt: sql.NullTime{Time: at, Valid: true},
		ID:        id,
	})
	if err != nil {
		return false, fmt.Errorf("revoke skill image approval: %w", err)
	}
	return n > 0, nil
}

func imageApprovalFromRow(row gen.SkillImageApproval) skillimage.Approval {
	return skillimage.Approval{
		ID: row.ID,
		Scope: skillimage.Scope{
			TenantID:  row.TenantID,
			ProjectID: row.ProjectID,
			SkillID:   row.SkillID,
			Version:   row.Version,
			ModeID:    row.ModeID,
		},
		Tool:       row.Tool,
		Reference:  row.Reference,
		Digest:     row.Digest,
		ApprovedBy: row.ApprovedBy,
		ApprovedAt: row.ApprovedAt,
		ExpiresAt:  nullTimeToPtr(row.ExpiresAt),
		RevokedAt:  nullTimeToPtr(row.RevokedAt),
		Note:       row.Note,
	}
}
