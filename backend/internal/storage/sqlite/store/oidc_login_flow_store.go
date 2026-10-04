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

// InsertOIDCLoginFlow records a new in-flight Authorization Code request.
func (s *Store) InsertOIDCLoginFlow(ctx context.Context, flow domain.OIDCLoginFlow) (domain.OIDCLoginFlow, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.InsertOIDCLoginFlow(ctx, gen.InsertOIDCLoginFlowParams{
		ID:                flow.ID,
		Nonce:             flow.Nonce,
		CodeVerifier:      flow.CodeVerifier,
		RedirectUri:       flow.RedirectURI,
		ReturnTo:          flow.ReturnTo,
		ClientKind:        string(flow.ClientKind),
		HandoffSecretHash: flow.HandoffSecretHash,
		CreatedAt:         flow.CreatedAt,
		ExpiresAt:         flow.ExpiresAt,
	})
	if err != nil {
		return domain.OIDCLoginFlow{}, fmt.Errorf("insert oidc login flow: %w", err)
	}
	return oidcLoginFlowFromRow(row), nil
}

// GetOIDCLoginFlow returns the flow for a `state` value, or (zero, false, nil)
// when no such flow exists. Expiry and single-use consumption are the caller's
// checks (see domain.OIDCLoginFlow.Pending), so a replayed state can be
// reported distinctly from an invented one.
func (s *Store) GetOIDCLoginFlow(ctx context.Context, id string) (domain.OIDCLoginFlow, bool, error) {
	row, err := s.qr.GetOIDCLoginFlow(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.OIDCLoginFlow{}, false, nil
		}
		return domain.OIDCLoginFlow{}, false, fmt.Errorf("get oidc login flow: %w", err)
	}
	return oidcLoginFlowFromRow(row), true, nil
}

// MarkOIDCLoginFlowAuthenticated stamps the resolved user on a flow. It
// succeeds at most once per flow (the UPDATE requires a still-unauthenticated,
// unconsumed row), which is what makes a replayed callback a no-op rather than
// a second login.
func (s *Store) MarkOIDCLoginFlowAuthenticated(ctx context.Context, id string, userID domain.UserID, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.MarkOIDCLoginFlowAuthenticated(ctx, gen.MarkOIDCLoginFlowAuthenticatedParams{
		AuthenticatedUserID: &userID,
		AuthenticatedAt:     timePtrToNullTime(&at),
		ID:                  id,
	})
	if err != nil {
		return false, fmt.Errorf("mark oidc login flow %s authenticated: %w", id, err)
	}
	return n > 0, nil
}

// ConsumeOIDCLoginFlow retires a flow. Returns false when it was already
// consumed, which is the atomic guard against a replayed state or a second
// desktop pickup.
func (s *Store) ConsumeOIDCLoginFlow(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.ConsumeOIDCLoginFlow(ctx, gen.ConsumeOIDCLoginFlowParams{
		ConsumedAt: timePtrToNullTime(&at),
		ID:         id,
	})
	if err != nil {
		return false, fmt.Errorf("consume oidc login flow %s: %w", id, err)
	}
	return n > 0, nil
}

// DeleteExpiredOIDCLoginFlows prunes flows past their expiry. Abandoned logins
// are the normal case (a user who closes the provider tab), so this table
// would otherwise only grow.
func (s *Store) DeleteExpiredOIDCLoginFlows(ctx context.Context, before time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteExpiredOIDCLoginFlows(ctx, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired oidc login flows: %w", err)
	}
	return n, nil
}

func oidcLoginFlowFromRow(row gen.OidcLoginFlow) domain.OIDCLoginFlow {
	return domain.OIDCLoginFlow{
		ID:                  row.ID,
		Nonce:               row.Nonce,
		CodeVerifier:        row.CodeVerifier,
		RedirectURI:         row.RedirectUri,
		ReturnTo:            row.ReturnTo,
		ClientKind:          domain.OIDCClientKind(row.ClientKind),
		HandoffSecretHash:   row.HandoffSecretHash,
		AuthenticatedUserID: row.AuthenticatedUserID,
		AuthenticatedAt:     nullTimeToTimePtr(row.AuthenticatedAt),
		CreatedAt:           row.CreatedAt,
		ExpiresAt:           row.ExpiresAt,
		ConsumedAt:          nullTimeToTimePtr(row.ConsumedAt),
	}
}
