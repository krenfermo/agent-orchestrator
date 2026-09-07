// Package agentauth mints and resolves the credential an AO-LAUNCHED AGENT uses
// to talk to the daemon's own API.
//
// Why it exists. Under AO_AUTH_MODE=oidc a request resolves its identity from
// the ao_session cookie, and an agent AO started itself -- a reviewer pane, a
// worker pane -- holds no cookie and cannot obtain one: the OIDC flow needs a
// browser and a person. Every permission-gated route therefore answered the
// agent with 401 NOT_AUTHENTICATED, including the one route a reviewer exists to
// call. A reviewer would finish, try to record its verdict, be refused, and idle
// forever; thirty minutes later the review-staleness threshold parked the run on
// review_state_ambiguous, and AO reported that it could not prove what the
// review had concluded -- about a review it had refused to listen to.
//
// What this is NOT:
//
//   - not an exemption. reviews/submit stays behind the same authorization gate
//     as every other session-scoped route; the agent simply arrives at it with
//     an identity instead of without one.
//   - not the operator's credential. An agent presenting a person's session
//     would inherit that person's authority over every project in every
//     organization, which is a much larger grant than "record this verdict".
//   - not a new authorization model. The credential names the account it acts
//     FOR, and authorization grants the INTERSECTION of that account's rights
//     and the agent's own bounded grant -- so RBAC, project grants and tenancy
//     all still decide, and the agent can only ever do less than its owner.
//
// The credential is bound to one project, one session, one workflow run, one
// step and one runtime incarnation, capped by the agent's role, expiring, and
// revoked when the work it was minted for is closed out.
package agentauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// DefaultTTL is how long a freshly minted agent credential stays valid.
//
// It is generous because the cost of getting it wrong is asymmetric: a
// credential that expires under a long-running reviewer recreates exactly the
// failure this package exists to remove, while one that outlives its work is
// revoked the moment that work is closed out anyway (RevokeForReviewRun) and is
// bounded to one session and one run in the meantime.
const DefaultTTL = 72 * time.Hour

// Store is the durable surface this service needs.
type Store interface {
	InsertAgentCredential(ctx context.Context, cred domain.AgentCredential) (domain.AgentCredential, error)
	GetAgentCredentialByTokenHash(ctx context.Context, tokenHash string) (domain.AgentCredential, bool, error)
	ListAgentCredentialsForReviewRun(ctx context.Context, reviewRunID string) ([]domain.AgentCredential, error)
	TouchAgentCredentialLastSeen(ctx context.Context, id string, at time.Time) (bool, error)
	RevokeAgentCredentialsForReviewRun(ctx context.Context, reviewRunID string, at time.Time) (int64, error)
	RevokeAgentCredentialsForSession(ctx context.Context, sessionID domain.SessionID, at time.Time) (int64, error)
	GetUserByID(ctx context.Context, id domain.UserID) (domain.User, bool, error)
}

// Service is the default implementation.
type Service struct {
	store Store
	now   func() time.Time
	ttl   time.Duration
}

// New builds a Service. now defaults to time.Now and ttl to DefaultTTL.
func New(store Store, now func() time.Time, ttl time.Duration) *Service {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Service{store: store, now: now, ttl: ttl}
}

// IssueInput describes the launch a credential is being minted for. Every field
// but Permissions is a binding the resolved credential will be confined to.
type IssueInput struct {
	Role domain.AgentRole
	// UserID is the account the agent acts for -- the workflow run's owner.
	// Required: an agent with no account behind it has nothing to intersect
	// against, and "no account" must not mean "unbounded".
	UserID    domain.UserID
	ProjectID domain.ProjectID
	// SessionID is the one session the agent may reach. For a reviewer this is
	// the WORKER's session, because that is the session its review_run belongs
	// to and therefore the one `ao review submit` addresses.
	SessionID         domain.SessionID
	WorkflowRunID     string
	WorkflowStepID    string
	ReviewRunID       string
	RuntimeHandle     string
	RuntimeInstanceID string
	Generation        int64
	// Permissions asks for LESS than the role's ceiling. Empty means the whole
	// ceiling, which is what every AO launch asks for.
	Permissions []domain.Permission
}

// Issued is a minted credential plus the raw token, which exists only in this
// return value: it is never stored and never logged.
type Issued struct {
	Token      string
	Credential domain.AgentCredential
}

// Issue mints a credential for one agent launch.
func (s *Service) Issue(ctx context.Context, in IssueInput) (Issued, error) {
	if !domain.ValidAgentRole(in.Role) {
		return Issued{}, fmt.Errorf("agent credential: unknown role %q", in.Role)
	}
	if strings.TrimSpace(string(in.UserID)) == "" {
		return Issued{}, fmt.Errorf("agent credential: an owning account is required")
	}
	if strings.TrimSpace(string(in.ProjectID)) == "" {
		return Issued{}, fmt.Errorf("agent credential: a project binding is required")
	}
	if strings.TrimSpace(string(in.SessionID)) == "" {
		return Issued{}, fmt.Errorf("agent credential: a session binding is required")
	}
	// The account has to exist and be active AT ISSUE TIME. Minting a
	// credential for a disabled account would create an authority that outlives
	// the decision to disable it; the resolver re-checks this on every request
	// as well, so a later disable also takes effect immediately.
	u, ok, err := s.store.GetUserByID(ctx, in.UserID)
	if err != nil {
		return Issued{}, fmt.Errorf("agent credential: look up owning account: %w", err)
	}
	if !ok || u.Status != domain.UserStatusActive {
		return Issued{}, fmt.Errorf("agent credential: owning account %s is not active", in.UserID)
	}

	raw, err := randomToken()
	if err != nil {
		return Issued{}, fmt.Errorf("agent credential: generate token: %w", err)
	}
	now := s.now().UTC()
	cred := domain.AgentCredential{
		ID:                "agc-" + uuid.NewString(),
		TokenHash:         HashToken(raw),
		Role:              in.Role,
		UserID:            in.UserID,
		ProjectID:         in.ProjectID,
		SessionID:         in.SessionID,
		WorkflowRunID:     in.WorkflowRunID,
		WorkflowStepID:    in.WorkflowStepID,
		ReviewRunID:       in.ReviewRunID,
		RuntimeHandle:     in.RuntimeHandle,
		RuntimeInstanceID: in.RuntimeInstanceID,
		Generation:        in.Generation,
		Permissions:       domain.CapAgentPermissions(in.Role, in.Permissions),
		CreatedAt:         now,
		ExpiresAt:         now.Add(s.ttl),
		LastSeenAt:        now,
	}
	stored, err := s.store.InsertAgentCredential(ctx, cred)
	if err != nil {
		return Issued{}, err
	}
	return Issued{Token: raw, Credential: stored}, nil
}

// ResolveAgentPrincipal turns a raw agent token into a principal, or returns a
// structured error the transport renders as 401.
//
// Every failure answers the same way a browser session's does: the caller is
// told the credential is not usable, never which of the reasons applied to a
// token they do not hold.
func (s *Service) ResolveAgentPrincipal(ctx context.Context, rawToken string) (domain.Principal, error) {
	notFound := apierr.Unauthorized("NOT_AUTHENTICATED", "agent credential not found, revoked, or expired")
	if strings.TrimSpace(rawToken) == "" {
		return domain.Principal{}, notFound
	}
	cred, ok, err := s.store.GetAgentCredentialByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		return domain.Principal{}, fmt.Errorf("lookup agent credential: %w", err)
	}
	now := s.now().UTC()
	if !ok || !cred.Active(now) {
		return domain.Principal{}, notFound
	}
	// The account is re-read on every request, not cached into the credential:
	// disabling somebody must stop their agents too, and it must stop them now
	// rather than at the credential's expiry.
	u, ok, err := s.store.GetUserByID(ctx, cred.UserID)
	if err != nil {
		return domain.Principal{}, fmt.Errorf("lookup agent credential account: %w", err)
	}
	if !ok || u.Status != domain.UserStatusActive {
		return domain.Principal{}, notFound
	}
	// Best-effort; a failure here must never fail resolution.
	_, _ = s.store.TouchAgentCredentialLastSeen(ctx, cred.ID, now)

	authority := cred.Authority()
	return domain.Principal{
		User:       u,
		AuthMethod: domain.AuthMethodAgent,
		Agent:      &authority,
	}, nil
}

// RevokeForReviewRun revokes every credential minted for one review run. It is
// called wherever a review run is closed out, so that a reviewer AO has stopped
// listening to also stops being able to speak.
func (s *Service) RevokeForReviewRun(ctx context.Context, reviewRunID string) (int64, error) {
	if strings.TrimSpace(reviewRunID) == "" {
		return 0, nil
	}
	return s.store.RevokeAgentCredentialsForReviewRun(ctx, reviewRunID, s.now().UTC())
}

// RevokeForSession revokes every credential bound to one session.
func (s *Service) RevokeForSession(ctx context.Context, sessionID domain.SessionID) (int64, error) {
	if strings.TrimSpace(string(sessionID)) == "" {
		return 0, nil
	}
	return s.store.RevokeAgentCredentialsForSession(ctx, sessionID, s.now().UTC())
}

// EverIssuedForReviewRun reports whether AO ever minted a credential for this
// review run -- revoked and expired ones included.
//
// It answers a question about AO's OWN launch record, and that is what makes it
// usable as evidence: a reviewer for which no credential was ever minted was
// started by a build that could not give one, so on an installation that
// requires authentication it has no channel through which any verdict could
// reach AO. See workflow's ambiguous-review recovery.
func (s *Service) EverIssuedForReviewRun(ctx context.Context, reviewRunID string) (bool, error) {
	if strings.TrimSpace(reviewRunID) == "" {
		return false, nil
	}
	creds, err := s.store.ListAgentCredentialsForReviewRun(ctx, reviewRunID)
	if err != nil {
		return false, err
	}
	return len(creds) > 0, nil
}

// HashToken is the one-way form a credential is stored as.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
