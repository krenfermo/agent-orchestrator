package ssosvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc"
)

// enforceConstraints applies the operator's optional gates BEFORE any account
// is created or touched. An identity that fails them must leave no trace: a
// rejected domain that still provisioned a user would be a bypass with extra
// steps.
func (s *Service) enforceConstraints(claims *oidc.IDTokenClaims) error {
	if len(s.cfg.AllowedEmailDomains) > 0 {
		domainPart := claims.EmailDomain()
		if domainPart == "" {
			return apierr.Forbidden("SSO_DOMAIN_NOT_ALLOWED",
				"this installation restricts sign-in by email domain, and the identity provider returned no email")
		}
		// An unverified email cannot satisfy a domain restriction: the whole
		// point of the restriction is that the domain is proof of something,
		// and an unverified address proves nothing.
		if !claims.EmailVerified {
			return apierr.Forbidden("SSO_EMAIL_NOT_VERIFIED",
				"this installation restricts sign-in by email domain and the identity provider did not verify this address")
		}
		if !containsFold(s.cfg.AllowedEmailDomains, domainPart) {
			return apierr.Forbidden("SSO_DOMAIN_NOT_ALLOWED", "this email domain is not permitted to sign in")
		}
	}
	if s.cfg.RequiredClaimName != "" {
		if !claims.HasClaimValue(s.cfg.RequiredClaimName, s.cfg.RequiredClaimValue) {
			return apierr.Forbidden("SSO_CLAIM_NOT_SATISFIED", "this account is not permitted to sign in to this installation")
		}
	}
	return nil
}

// resolveAccount maps verified claims onto an AO account.
//
// The canonical key is (issuer, sub) and nothing else. Email is used for
// exactly one thing — deciding whether a FIRST login for a new (issuer, sub)
// should attach to an existing local account instead of creating a second one
// — and only when the provider verified it. Linking on an unverified address
// is account takeover: anyone who can set an unverified email at the provider
// could otherwise walk into an existing AO account.
func (s *Service) resolveAccount(ctx context.Context, claims *oidc.IDTokenClaims, now time.Time) (domain.User, bool, error) {
	existing, found, err := s.store.GetExternalIdentityByIssuerSubject(ctx, claims.Issuer, claims.Subject)
	if err != nil {
		return domain.User{}, false, fmt.Errorf("lookup external identity: %w", err)
	}
	if found {
		user, ok, err := s.store.GetUserByID(ctx, existing.UserID)
		if err != nil {
			return domain.User{}, false, fmt.Errorf("lookup linked user: %w", err)
		}
		if !ok || user.Status != domain.UserStatusActive {
			return domain.User{}, false, apierr.Unauthorized("SSO_ACCOUNT_DISABLED", "this account cannot sign in")
		}
		// Refresh the display-only claim snapshot. A failure here must not
		// fail a login that has already been fully validated.
		_, _ = s.store.UpdateExternalIdentityClaims(ctx, existing.ID, claims.Email, claims.EmailVerified, claims.DisplayName(), now)
		return user, false, nil
	}

	// First login for this (issuer, sub).
	if claims.Email != "" && claims.EmailVerified && s.cfg.LinkVerifiedEmail {
		local, ok, err := s.store.GetUserByEmail(ctx, claims.Email)
		if err != nil {
			return domain.User{}, false, fmt.Errorf("lookup user by email: %w", err)
		}
		if ok {
			if local.Status != domain.UserStatusActive {
				return domain.User{}, false, apierr.Unauthorized("SSO_ACCOUNT_DISABLED", "this account cannot sign in")
			}
			if err := s.link(ctx, local.ID, claims, now); err != nil {
				return domain.User{}, false, err
			}
			return local, false, nil
		}
	} else if claims.Email != "" {
		// An address that already belongs to a local account, but that this
		// installation will not link on — either the provider did not verify
		// it, or linking is switched off — is a refusal, not a silent second
		// account: creating one would collide on the users.email unique index
		// anyway, and the honest error is far more useful than a constraint
		// violation.
		if _, ok, err := s.store.GetUserByEmail(ctx, claims.Email); err != nil {
			return domain.User{}, false, fmt.Errorf("lookup user by email: %w", err)
		} else if ok {
			if claims.EmailVerified {
				return domain.User{}, false, apierr.Forbidden("SSO_LINKING_DISABLED",
					"an account already uses this email address and automatic account linking is disabled on this installation")
			}
			return domain.User{}, false, apierr.Forbidden("SSO_EMAIL_NOT_VERIFIED",
				"an account already uses this email address and the identity provider did not verify it")
		}
	}

	// Provision. The first identity to sign in to an installation that has no
	// users at all becomes its owner — the same rule Bootstrap follows, so an
	// SSO-only deployment is never left ownerless.
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		return domain.User{}, false, fmt.Errorf("count users: %w", err)
	}
	user, err := s.sessions.CreateFederatedUser(ctx, FederatedUserInput{
		DisplayName: claims.DisplayName(),
		Email:       claims.Email,
		Username:    claims.PreferredUsername,
		PreferOwner: count == 0,
	})
	if err != nil {
		return domain.User{}, false, err
	}
	if err := s.link(ctx, user.ID, claims, now); err != nil {
		return domain.User{}, false, err
	}
	return user, true, nil
}

// link records the (issuer, sub) → user edge. A unique-index collision means a
// concurrent first login already created it; that is a success for the caller's
// purpose, so the winner's row is returned instead of an error.
func (s *Service) link(ctx context.Context, userID domain.UserID, claims *oidc.IDTokenClaims, now time.Time) error {
	_, err := s.store.InsertExternalIdentity(ctx, domain.ExternalIdentity{
		ID:            uuid.NewString(),
		UserID:        userID,
		Issuer:        claims.Issuer,
		Subject:       claims.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		DisplayName:   claims.DisplayName(),
		CreatedAt:     now,
		UpdatedAt:     now,
		LastLoginAt:   &now,
	})
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return nil
	}
	return fmt.Errorf("link external identity: %w", err)
}

// mapProviderError turns internal/oidc's sentinels into the API's error
// vocabulary. Every message here is safe to render: none carries a token, a
// code, a secret, or a provider response body.
func mapProviderError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, oidc.ErrProviderUnreachable):
		return apierr.New(apierr.KindInternal, "SSO_PROVIDER_UNREACHABLE",
			"could not reach the identity provider; try again shortly", nil)
	case errors.Is(err, oidc.ErrIssuerMismatch):
		return apierr.Unauthorized("SSO_ISSUER_MISMATCH",
			"the identity provider's response came from an unexpected issuer")
	case errors.Is(err, oidc.ErrAudienceMismatch):
		return apierr.Unauthorized("SSO_AUDIENCE_MISMATCH",
			"the identity provider's response was not issued for this application")
	case errors.Is(err, oidc.ErrTokenExpired):
		return apierr.Unauthorized("SSO_TOKEN_EXPIRED", "the sign-in response expired; start again")
	case errors.Is(err, oidc.ErrNonceMismatch):
		return apierr.Unauthorized("SSO_NONCE_MISMATCH", "the sign-in response did not match this sign-in request; start again")
	case errors.Is(err, oidc.ErrInvalidToken):
		return apierr.Unauthorized("SSO_INVALID_TOKEN", "the identity provider's response could not be verified")
	case errors.Is(err, oidc.ErrTokenExchange):
		return apierr.Unauthorized("SSO_TOKEN_EXCHANGE_FAILED", "the identity provider rejected the sign-in; start again")
	default:
		return err
	}
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
