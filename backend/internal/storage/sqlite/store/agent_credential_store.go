package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// InsertAgentCredential creates one agent-credential row. cred.TokenHash must
// already be the SHA-256 of the raw token -- the raw token is returned to the
// launcher that will hand it to its agent and is never written here.
func (s *Store) InsertAgentCredential(ctx context.Context, cred domain.AgentCredential) (domain.AgentCredential, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	perms, err := marshalAgentPermissions(cred.Permissions)
	if err != nil {
		return domain.AgentCredential{}, err
	}
	row, err := s.qw.InsertAgentCredential(ctx, gen.InsertAgentCredentialParams{
		ID:                cred.ID,
		TokenHash:         cred.TokenHash,
		Role:              cred.Role,
		UserID:            cred.UserID,
		ProjectID:         cred.ProjectID,
		SessionID:         cred.SessionID,
		WorkflowRunID:     cred.WorkflowRunID,
		WorkflowStepID:    cred.WorkflowStepID,
		ReviewRunID:       cred.ReviewRunID,
		RuntimeHandle:     cred.RuntimeHandle,
		RuntimeInstanceID: cred.RuntimeInstanceID,
		Generation:        cred.Generation,
		Permissions:       perms,
		CreatedAt:         cred.CreatedAt,
		ExpiresAt:         cred.ExpiresAt,
		LastSeenAt:        cred.LastSeenAt,
		RevokedAt:         timePtrToNullTime(cred.RevokedAt),
	})
	if err != nil {
		return domain.AgentCredential{}, fmt.Errorf("insert agent credential: %w", err)
	}
	return agentCredentialFromRow(row)
}

// GetAgentCredentialByTokenHash returns the row for a raw token's SHA-256, or
// (zero, false, nil) when none exists. Like GetAuthSessionByTokenHash it does
// NOT filter on revoked/expired: the resolver distinguishes "no such credential"
// from "this credential is finished", and only the second is worth telling
// anybody about.
func (s *Store) GetAgentCredentialByTokenHash(ctx context.Context, tokenHash string) (domain.AgentCredential, bool, error) {
	row, err := s.qr.GetAgentCredentialByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AgentCredential{}, false, nil
		}
		return domain.AgentCredential{}, false, fmt.Errorf("get agent credential by token hash: %w", err)
	}
	cred, cerr := agentCredentialFromRow(row)
	if cerr != nil {
		return domain.AgentCredential{}, false, cerr
	}
	return cred, true, nil
}

// ListAgentCredentialsForReviewRun returns every credential ever minted for one
// review run, revoked and expired ones included. Recovery reads it to answer a
// question about AO's OWN launch record -- "was this reviewer ever given a way
// to speak to me?" -- which a filtered list could not answer.
func (s *Store) ListAgentCredentialsForReviewRun(ctx context.Context, reviewRunID string) ([]domain.AgentCredential, error) {
	rows, err := s.qr.ListAgentCredentialsForReviewRun(ctx, reviewRunID)
	if err != nil {
		return nil, fmt.Errorf("list agent credentials for review run %s: %w", reviewRunID, err)
	}
	out := make([]domain.AgentCredential, 0, len(rows))
	for _, row := range rows {
		cred, cerr := agentCredentialFromRow(row)
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, cred)
	}
	return out, nil
}

// TouchAgentCredentialLastSeen records that a still-active credential was used.
func (s *Store) TouchAgentCredentialLastSeen(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.TouchAgentCredentialLastSeen(ctx, gen.TouchAgentCredentialLastSeenParams{
		LastSeenAt: at,
		ID:         id,
	})
	if err != nil {
		return false, fmt.Errorf("touch agent credential %s: %w", id, err)
	}
	return n > 0, nil
}

// RevokeAgentCredentialsForReviewRun revokes every live credential minted for
// one review run. Called when that run is closed out, so a reviewer AO has
// finished with cannot keep speaking for it.
func (s *Store) RevokeAgentCredentialsForReviewRun(ctx context.Context, reviewRunID string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeAgentCredentialsForReviewRun(ctx, gen.RevokeAgentCredentialsForReviewRunParams{
		RevokedAt:   timePtrToNullTime(&at),
		ReviewRunID: reviewRunID,
	})
	if err != nil {
		return 0, fmt.Errorf("revoke agent credentials for review run %s: %w", reviewRunID, err)
	}
	return n, nil
}

// RevokeAgentCredentialsForSession revokes every live credential bound to one
// session, for the session's own termination path.
func (s *Store) RevokeAgentCredentialsForSession(ctx context.Context, sessionID domain.SessionID, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeAgentCredentialsForSession(ctx, gen.RevokeAgentCredentialsForSessionParams{
		RevokedAt: timePtrToNullTime(&at),
		SessionID: sessionID,
	})
	if err != nil {
		return 0, fmt.Errorf("revoke agent credentials for session %s: %w", sessionID, err)
	}
	return n, nil
}

func marshalAgentPermissions(perms []domain.Permission) (string, error) {
	if len(perms) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(perms)
	if err != nil {
		return "", fmt.Errorf("encode agent permissions: %w", err)
	}
	return string(raw), nil
}

func agentCredentialFromRow(row gen.AgentCredential) (domain.AgentCredential, error) {
	var perms []domain.Permission
	if row.Permissions != "" {
		if err := json.Unmarshal([]byte(row.Permissions), &perms); err != nil {
			// A grant AO cannot read is not a grant. Failing here rather than
			// defaulting to "no permissions" keeps a corrupted row from being
			// silently rewritten as a valid-but-inert credential.
			return domain.AgentCredential{}, fmt.Errorf("decode agent permissions for credential %s: %w", row.ID, err)
		}
	}
	return domain.AgentCredential{
		ID:                row.ID,
		TokenHash:         row.TokenHash,
		Role:              row.Role,
		UserID:            row.UserID,
		ProjectID:         row.ProjectID,
		SessionID:         row.SessionID,
		WorkflowRunID:     row.WorkflowRunID,
		WorkflowStepID:    row.WorkflowStepID,
		ReviewRunID:       row.ReviewRunID,
		RuntimeHandle:     row.RuntimeHandle,
		RuntimeInstanceID: row.RuntimeInstanceID,
		Generation:        row.Generation,
		Permissions:       perms,
		CreatedAt:         row.CreatedAt,
		ExpiresAt:         row.ExpiresAt,
		LastSeenAt:        row.LastSeenAt,
		RevokedAt:         nullTimeToTimePtr(row.RevokedAt),
	}, nil
}
