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

// InsertProviderProfile creates a new provider profile row, owned by
// p.UserID. Callers (service/providerprofile) are responsible for having
// resolved UserID from server-side identity, never from client input.
func (s *Store) InsertProviderProfile(ctx context.Context, p domain.ProviderProfile) (domain.ProviderProfile, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	caps, err := marshalCapabilities(p.Capabilities)
	if err != nil {
		return domain.ProviderProfile{}, err
	}
	row, err := s.qw.InsertProviderProfile(ctx, gen.InsertProviderProfileParams{
		ID:               string(p.ID),
		UserID:           string(p.UserID),
		Provider:         p.Provider,
		Harness:          string(p.Harness),
		DisplayName:      p.DisplayName,
		Enabled:          boolToInt64(p.Enabled),
		AuthState:        string(p.AuthState),
		AuthMethod:       string(p.AuthMethod),
		DefaultModel:     stringToNullString(p.DefaultModel),
		Capabilities:     caps,
		SecretCiphertext: p.SecretCiphertext,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	})
	if err != nil {
		return domain.ProviderProfile{}, fmt.Errorf("insert provider profile: %w", err)
	}
	return providerProfileFromRow(row)
}

// ListProviderProfilesByUser returns every profile owned by userID. Never
// returns another user's rows -- filtering happens in SQL (WHERE user_id
// = ?), not just in Go.
func (s *Store) ListProviderProfilesByUser(ctx context.Context, userID domain.UserID) ([]domain.ProviderProfile, error) {
	rows, err := s.qr.ListProviderProfilesByUser(ctx, string(userID))
	if err != nil {
		return nil, fmt.Errorf("list provider profiles: %w", err)
	}
	out := make([]domain.ProviderProfile, 0, len(rows))
	for _, r := range rows {
		p, err := providerProfileFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// GetProviderProfileByIDForUser returns the profile only if it exists AND is
// owned by userID; (zero, false, nil) otherwise -- a foreign profile id is
// indistinguishable from a missing one at this layer, which is what lets the
// controller return 404 without leaking existence.
func (s *Store) GetProviderProfileByIDForUser(ctx context.Context, id domain.ProviderProfileID, userID domain.UserID) (domain.ProviderProfile, bool, error) {
	row, err := s.qr.GetProviderProfileByIDForUser(ctx, gen.GetProviderProfileByIDForUserParams{
		ID:     string(id),
		UserID: string(userID),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ProviderProfile{}, false, nil
		}
		return domain.ProviderProfile{}, false, fmt.Errorf("get provider profile %s: %w", id, err)
	}
	p, err := providerProfileFromRow(row)
	if err != nil {
		return domain.ProviderProfile{}, false, err
	}
	return p, true, nil
}

// GetProviderProfileOwner returns the owning user id of a profile, or
// (\"\", false, nil) if it doesn't exist.
func (s *Store) GetProviderProfileOwner(ctx context.Context, id domain.ProviderProfileID) (domain.UserID, bool, error) {
	owner, err := s.qr.GetProviderProfileOwner(ctx, string(id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get provider profile owner %s: %w", id, err)
	}
	return domain.UserID(owner), true, nil
}

// UpdateProviderProfileForUser updates display name, enabled, and default
// model, scoped to (id, userID). Returns false if no row matched -- either
// the id doesn't exist or it belongs to a different user.
func (s *Store) UpdateProviderProfileForUser(ctx context.Context, id domain.ProviderProfileID, userID domain.UserID, displayName string, enabled bool, defaultModel string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateProviderProfileForUser(ctx, gen.UpdateProviderProfileForUserParams{
		DisplayName:  displayName,
		Enabled:      boolToInt64(enabled),
		DefaultModel: stringToNullString(defaultModel),
		UpdatedAt:    updatedAt,
		ID:           string(id),
		UserID:       string(userID),
	})
	if err != nil {
		return false, fmt.Errorf("update provider profile %s: %w", id, err)
	}
	return n > 0, nil
}

// UpdateProviderProfileAuthStateForUser updates the cached auth state,
// scoped to (id, userID). Returns false if no row matched.
func (s *Store) UpdateProviderProfileAuthStateForUser(ctx context.Context, id domain.ProviderProfileID, userID domain.UserID, state domain.ProviderAuthState, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateProviderProfileAuthStateForUser(ctx, gen.UpdateProviderProfileAuthStateForUserParams{
		AuthState: string(state),
		UpdatedAt: updatedAt,
		ID:        string(id),
		UserID:    string(userID),
	})
	if err != nil {
		return false, fmt.Errorf("update provider profile auth state %s: %w", id, err)
	}
	return n > 0, nil
}

func marshalCapabilities(caps []domain.ProviderCapability) (string, error) {
	if caps == nil {
		caps = []domain.ProviderCapability{}
	}
	b, err := json.Marshal(caps)
	if err != nil {
		return "", fmt.Errorf("marshal provider capabilities: %w", err)
	}
	return string(b), nil
}

func unmarshalCapabilities(raw string) ([]domain.ProviderCapability, error) {
	if raw == "" {
		return []domain.ProviderCapability{}, nil
	}
	var caps []domain.ProviderCapability
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return nil, fmt.Errorf("unmarshal provider capabilities: %w", err)
	}
	return caps, nil
}

func stringToNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func providerProfileFromRow(row gen.ProviderProfile) (domain.ProviderProfile, error) {
	caps, err := unmarshalCapabilities(row.Capabilities)
	if err != nil {
		return domain.ProviderProfile{}, err
	}
	return domain.ProviderProfile{
		ID:               domain.ProviderProfileID(row.ID),
		UserID:           domain.UserID(row.UserID),
		Provider:         row.Provider,
		Harness:          domain.AgentHarness(row.Harness),
		DisplayName:      row.DisplayName,
		Enabled:          row.Enabled != 0,
		AuthState:        domain.ProviderAuthState(row.AuthState),
		AuthMethod:       domain.ProviderAuthMethod(row.AuthMethod),
		DefaultModel:     row.DefaultModel.String,
		Capabilities:     caps,
		SecretCiphertext: row.SecretCiphertext,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}, nil
}
