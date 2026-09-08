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

// ListRevocableAgentCredentials returns the credentials a reconciliation pass
// would revoke. It is the read half of the same predicate the write below
// applies, so the two can never disagree about what "finished" means.
func (s *Store) ListRevocableAgentCredentials(ctx context.Context) ([]domain.RevocableAgentCredential, error) {
	rows, err := s.qr.ListRevocableAgentCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("list revocable agent credentials: %w", err)
	}
	out := make([]domain.RevocableAgentCredential, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.RevocableAgentCredential{
			CredentialID:  row.ID,
			ReviewRunID:   row.ReviewRunID,
			RuntimeHandle: row.RuntimeHandle,
		})
	}
	return out, nil
}

// RevokeClosedReviewRunAgentCredentials revokes every live credential whose
// review run has stopped running, in one set-based statement.
//
// It is the recovery half of credential revocation: the obligation it
// discharges is re-derived from durable rows rather than remembered, so a pass
// that fails, a daemon that dies mid-pass, and a revocation that was never
// attempted at all are the same situation on the next pass.
func (s *Store) RevokeClosedReviewRunAgentCredentials(ctx context.Context, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeClosedReviewRunAgentCredentials(ctx, timePtrToNullTime(&at))
	if err != nil {
		return 0, fmt.Errorf("revoke agent credentials for closed review runs: %w", err)
	}
	return n, nil
}

// RevokeAgentCredentialsForClosedReviewRun revokes one review run's live
// credentials, but only once that run has durably stopped running.
//
// The guard is the difference between this and
// RevokeAgentCredentialsForReviewRun above: the unguarded form is for a launch
// that failed, where there is no reviewer to protect. This one runs beside a
// live reviewer, so it must never take away the identity a review still in
// progress needs in order to report what it found.
func (s *Store) RevokeAgentCredentialsForClosedReviewRun(ctx context.Context, reviewRunID string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeAgentCredentialsForClosedReviewRun(ctx, gen.RevokeAgentCredentialsForClosedReviewRunParams{
		RevokedAt:   timePtrToNullTime(&at),
		ReviewRunID: reviewRunID,
	})
	if err != nil {
		return 0, fmt.Errorf("revoke agent credentials for closed review run %s: %w", reviewRunID, err)
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

// BindAgentCredentialSession binds an unbound worker credential to the session
// its launch produced, exactly once.
//
// It reports whether it bound anything. False is not an error: it means the row
// was already bound, already revoked, or gone — three different histories that
// are the same answer here, because in all of them this call must not be the
// one that decides which session the credential speaks for.
//
// The guard lives in the SQL rather than in a read-then-write, so two passes
// racing over the same launch cannot both succeed.
func (s *Store) BindAgentCredentialSession(ctx context.Context, credentialID string, sessionID domain.SessionID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.BindAgentCredentialSession(ctx, gen.BindAgentCredentialSessionParams{
		SessionID: sessionID,
		ID:        credentialID,
	})
	if err != nil {
		return false, fmt.Errorf("bind agent credential %s to session %s: %w", credentialID, sessionID, err)
	}
	return n > 0, nil
}

// RevokeAgentCredential revokes one credential by id. Idempotent: revoking an
// already-revoked credential reports zero rows rather than failing, because the
// state the caller wanted is the state that already holds.
func (s *Store) RevokeAgentCredential(ctx context.Context, credentialID string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeAgentCredential(ctx, gen.RevokeAgentCredentialParams{
		RevokedAt: timePtrToNullTime(&at),
		ID:        credentialID,
	})
	if err != nil {
		return 0, fmt.Errorf("revoke agent credential %s: %w", credentialID, err)
	}
	return n, nil
}

// RevokeSupersededWorkerAgentCredentials ends every live worker credential for
// one step that belongs to an attempt other than the one given.
//
// This is the replacement case the derived sweep cannot reach: while a step is
// being re-dispatched it is still running, so a predecessor's credential still
// satisfies "may live" even though its launch is over. The attempt is the
// discriminator, which is why it is also what fences the credential file.
func (s *Store) RevokeSupersededWorkerAgentCredentials(ctx context.Context, stepID, keepAttemptID string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeSupersededWorkerAgentCredentials(ctx, gen.RevokeSupersededWorkerAgentCredentialsParams{
		RevokedAt:         timePtrToNullTime(&at),
		WorkflowStepID:    stepID,
		RuntimeInstanceID: keepAttemptID,
	})
	if err != nil {
		return 0, fmt.Errorf("revoke superseded worker agent credentials for step %s: %w", stepID, err)
	}
	return n, nil
}

// ListRevocableWorkerAgentCredentials returns the worker credentials a
// reconciliation pass would revoke: those whose work step is no longer running.
// The read half of the same predicate the write below applies.
func (s *Store) ListRevocableWorkerAgentCredentials(ctx context.Context) ([]domain.RevocableAgentCredential, error) {
	rows, err := s.qr.ListRevocableWorkerAgentCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("list revocable worker agent credentials: %w", err)
	}
	out := make([]domain.RevocableAgentCredential, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.RevocableAgentCredential{
			CredentialID:   row.ID,
			WorkflowStepID: row.WorkflowStepID,
			RuntimeHandle:  row.RuntimeHandle,
		})
	}
	return out, nil
}

// RevokeStaleWorkerAgentCredentials revokes every live worker credential whose
// work step has stopped running, in one set-based statement.
//
// Same recovery property as its review-run twin: the obligation is re-derived
// from durable rows on every pass, so a pass that failed, a daemon that died
// mid-pass, and a revocation nobody ever attempted are the same situation next
// time round.
func (s *Store) RevokeStaleWorkerAgentCredentials(ctx context.Context, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeStaleWorkerAgentCredentials(ctx, timePtrToNullTime(&at))
	if err != nil {
		return 0, fmt.Errorf("revoke stale worker agent credentials: %w", err)
	}
	return n, nil
}
