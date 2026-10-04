// Package ssosvc orchestrates P4-A's OIDC login: it starts an Authorization
// Code request, validates the provider's callback, maps the verified claims
// onto an AO account, and issues an AO session.
//
// The division of labour is deliberate and load-bearing:
//
//   - internal/oidc speaks the protocol and returns verified claims. It knows
//     nothing about AO users.
//   - this package decides which AO account those claims are, and enforces the
//     operator's constraints.
//   - authsvc issues the AO session. The provider's access/refresh tokens are
//     never stored and never used as a browser session.
//
// That is what keeps OIDC claim parsing out of every handler: a controller
// calls Start/Complete and receives a domain.Principal.
package ssosvc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc"
)

// FlowTTL bounds how long a started login may sit unfinished. Long enough for
// a real person to type a password and satisfy an MFA prompt, short enough
// that an abandoned authorization request is not a standing replay target.
const FlowTTL = 15 * time.Minute

// Store is the durable surface this service needs. Backed by
// storage/sqlite/store.Store in production.
type Store interface {
	InsertOIDCLoginFlow(ctx context.Context, flow domain.OIDCLoginFlow) (domain.OIDCLoginFlow, error)
	GetOIDCLoginFlow(ctx context.Context, id string) (domain.OIDCLoginFlow, bool, error)
	MarkOIDCLoginFlowAuthenticated(ctx context.Context, id string, userID domain.UserID, at time.Time) (bool, error)
	ConsumeOIDCLoginFlow(ctx context.Context, id string, at time.Time) (bool, error)
	DeleteExpiredOIDCLoginFlows(ctx context.Context, before time.Time) (int64, error)

	GetExternalIdentityByIssuerSubject(ctx context.Context, issuer, subject string) (domain.ExternalIdentity, bool, error)
	ListExternalIdentitiesForUser(ctx context.Context, userID domain.UserID) ([]domain.ExternalIdentity, error)
	InsertExternalIdentity(ctx context.Context, ident domain.ExternalIdentity) (domain.ExternalIdentity, error)
	UpdateExternalIdentityClaims(ctx context.Context, id, email string, emailVerified bool, displayName string, at time.Time) (bool, error)

	GetUserByID(ctx context.Context, id domain.UserID) (domain.User, bool, error)
	GetUserByEmail(ctx context.Context, email string) (domain.User, bool, error)
	CountUsers(ctx context.Context) (int64, error)
}

// SessionIssuer is the subset of authsvc.Manager this service needs. Narrow on
// purpose: SSO may create an account and mint a session, and nothing else.
type SessionIssuer interface {
	CreateFederatedUser(ctx context.Context, in FederatedUserInput) (domain.User, error)
	CreateSessionAs(ctx context.Context, userID domain.UserID, method domain.AuthMethod, issuer, subject string) (string, domain.AuthSession, error)
}

// FederatedUserInput mirrors authsvc.FederatedUserInput. It is redeclared here
// rather than imported so this package depends on the narrow SessionIssuer
// interface instead of the whole identity service; daemon wiring adapts the
// two (see internal/daemon).
type FederatedUserInput struct {
	DisplayName string
	Email       string
	Username    string
	PreferOwner bool
}

// Manager is the service-facing SSO surface.
type Manager interface {
	// Enabled reports whether a provider is configured at all.
	Enabled() bool
	// DisplayName is the operator-chosen label for the sign-in button.
	DisplayName() string
	// Start begins an Authorization Code request and returns where to send
	// the user agent.
	Start(ctx context.Context, in StartInput) (StartResult, error)
	// Complete validates a provider callback and, for a browser flow, issues
	// the AO session. For a desktop flow it stamps the resolved identity and
	// leaves the session to be minted at Claim.
	Complete(ctx context.Context, in CallbackInput) (CompleteResult, error)
	// Claim redeems a desktop flow with the handoff secret the supervisor
	// kept on loopback, minting the AO session at that moment.
	Claim(ctx context.Context, flowID, handoffSecret string) (CompleteResult, error)
	// EndSessionURL returns the provider's RP-initiated logout URL, or "".
	EndSessionURL(ctx context.Context, postLogoutRedirect string) (string, error)
	// PurgeExpiredFlows prunes abandoned login flows.
	PurgeExpiredFlows(ctx context.Context) (int64, error)
}

// Service is the default Manager implementation.
type Service struct {
	cfg      config.OIDCConfig
	client   *oidc.Client
	store    Store
	sessions SessionIssuer
	now      func() time.Time
}

// New builds a Service. A zero/disabled cfg yields a Service whose Enabled()
// is false and whose every flow call returns SSO_NOT_CONFIGURED — the same
// optional-surface convention the rest of the daemon uses, so wiring never has
// to be conditional.
func New(cfg config.OIDCConfig, client *oidc.Client, store Store, sessions SessionIssuer, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: cfg, client: client, store: store, sessions: sessions, now: now}
}

var _ Manager = (*Service)(nil)

// Enabled reports whether a usable provider is configured AND every
// collaborator this service needs is wired. Both halves matter: a configured
// issuer with no store is not a working SSO installation.
func (s *Service) Enabled() bool {
	return s != nil && s.cfg.Enabled && s.client != nil && s.store != nil && s.sessions != nil
}

// DisplayName is the operator-chosen sign-in button label, falling back to a
// generic one so the button is never blank.
func (s *Service) DisplayName() string {
	if s == nil || s.cfg.DisplayName == "" {
		return config.DefaultOIDCDisplayName
	}
	return s.cfg.DisplayName
}

func (s *Service) notConfigured() error {
	return apierr.New(apierr.KindConflict, "SSO_NOT_CONFIGURED",
		"single sign-on is not configured on this installation", nil)
}

// StartInput describes a login being started.
type StartInput struct {
	// ClientKind distinguishes a browser login from the desktop
	// supervisor's system-browser login. See domain.OIDCClientKind.
	ClientKind domain.OIDCClientKind
	// ReturnTo is the raw, caller-supplied in-app destination. It is
	// validated here and stored only in its validated form.
	ReturnTo string
	// HandoffSecret is the desktop supervisor's pickup secret. Required for
	// a desktop flow, rejected for a browser one.
	HandoffSecret string
}

// StartResult is where to send the user agent, plus the flow's identifiers.
type StartResult struct {
	// AuthorizationURL is the provider's authorization endpoint with this
	// request's parameters.
	AuthorizationURL string
	// FlowID is the `state` value. It is safe to hand to the client that
	// started the flow: on its own it claims nothing.
	FlowID    string
	ExpiresAt time.Time
}

// Start mints state/nonce/PKCE, records the flow durably, and builds the
// authorization URL.
func (s *Service) Start(ctx context.Context, in StartInput) (StartResult, error) {
	if !s.Enabled() {
		return StartResult{}, s.notConfigured()
	}
	kind := in.ClientKind
	if kind == "" {
		kind = domain.OIDCClientBrowser
	}
	switch kind {
	case domain.OIDCClientBrowser:
		if in.HandoffSecret != "" {
			return StartResult{}, apierr.Invalid("SSO_INVALID_START", "a browser login carries no handoff secret", nil)
		}
	case domain.OIDCClientDesktop:
		if len(in.HandoffSecret) < 32 {
			return StartResult{}, apierr.Invalid("SSO_INVALID_START", "a desktop login requires a handoff secret of at least 32 characters", nil)
		}
	default:
		return StartResult{}, apierr.Invalid("SSO_INVALID_START", "unknown client kind", nil)
	}

	state, err := oidc.NewState()
	if err != nil {
		return StartResult{}, err
	}
	nonce, err := oidc.NewNonce()
	if err != nil {
		return StartResult{}, err
	}
	verifier, err := oidc.NewCodeVerifier()
	if err != nil {
		return StartResult{}, err
	}

	authURL, err := s.client.AuthorizationURL(ctx, state, nonce, oidc.CodeChallengeS256(verifier))
	if err != nil {
		return StartResult{}, mapProviderError(err)
	}

	now := s.now().UTC()
	flow := domain.OIDCLoginFlow{
		ID:           state,
		Nonce:        nonce,
		CodeVerifier: verifier,
		RedirectURI:  s.cfg.RedirectURL,
		ReturnTo:     SafeReturnTo(in.ReturnTo),
		ClientKind:   kind,
		CreatedAt:    now,
		ExpiresAt:    now.Add(FlowTTL),
	}
	if kind == domain.OIDCClientDesktop {
		flow.HandoffSecretHash = hashSecret(in.HandoffSecret)
	}
	if _, err := s.store.InsertOIDCLoginFlow(ctx, flow); err != nil {
		return StartResult{}, fmt.Errorf("record login flow: %w", err)
	}
	return StartResult{AuthorizationURL: authURL, FlowID: state, ExpiresAt: flow.ExpiresAt}, nil
}

// CallbackInput is what the provider's redirect carried.
type CallbackInput struct {
	State string
	Code  string
	// ProviderError/ProviderErrorDescription are the provider's own OAuth
	// error, when it declined rather than issuing a code.
	ProviderError            string
	ProviderErrorDescription string
}

// CompleteResult is a finished (or, for a desktop flow, half-finished) login.
type CompleteResult struct {
	// Principal is who signed in.
	Principal domain.Principal
	// SessionToken is the RAW AO session token, returned exactly once for the
	// Set-Cookie response (browser) or the loopback handoff (desktop). Empty
	// for the desktop callback, which mints no session.
	SessionToken string
	// SessionExpiresAt is the issued session's expiry; zero when none was
	// issued.
	SessionExpiresAt time.Time
	// ReturnTo is the validated in-app destination for this flow.
	ReturnTo string
	// ClientKind is the flow's kind, so the caller knows whether to set a
	// cookie or render the desktop hand-back page.
	ClientKind domain.OIDCClientKind
	// Provisioned is true when this login created the AO account.
	Provisioned bool
}

// Complete validates the callback and resolves the identity behind it.
//
// The order of checks is the security contract: state (is this a login AO
// started?), expiry/replay, the provider's own error, then the code exchange —
// which itself verifies signature, issuer, audience, expiry and nonce — then
// the operator's constraints, and only then any account is touched.
func (s *Service) Complete(ctx context.Context, in CallbackInput) (CompleteResult, error) {
	if !s.Enabled() {
		return CompleteResult{}, s.notConfigured()
	}
	if strings.TrimSpace(in.State) == "" {
		return CompleteResult{}, apierr.Invalid("SSO_MISSING_STATE", "the sign-in response carried no state", nil)
	}
	flow, ok, err := s.store.GetOIDCLoginFlow(ctx, in.State)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("lookup login flow: %w", err)
	}
	now := s.now().UTC()
	if !ok {
		// An unknown state and a replayed one are the same answer on the
		// wire: "start again". Distinguishing them tells a prober which
		// states have existed.
		return CompleteResult{}, apierr.Invalid("SSO_INVALID_STATE", "this sign-in request is no longer valid; start again", nil)
	}
	if flow.ConsumedAt != nil {
		return CompleteResult{}, apierr.Invalid("SSO_INVALID_STATE", "this sign-in request is no longer valid; start again", nil)
	}
	if !flow.ExpiresAt.After(now) {
		return CompleteResult{}, apierr.Invalid("SSO_STATE_EXPIRED", "this sign-in request expired; start again", nil)
	}

	// The provider declined. Retire the flow so the state cannot be reused,
	// then report the provider's own error code — never its raw description,
	// which is attacker-influenceable text rendered in AO's UI.
	if in.ProviderError != "" {
		_, _ = s.store.ConsumeOIDCLoginFlow(ctx, flow.ID, now)
		return CompleteResult{}, apierr.Unauthorized("SSO_PROVIDER_DECLINED",
			"the identity provider declined the sign-in ("+sanitizeErrorCode(in.ProviderError)+")")
	}
	if strings.TrimSpace(in.Code) == "" {
		_, _ = s.store.ConsumeOIDCLoginFlow(ctx, flow.ID, now)
		return CompleteResult{}, apierr.Invalid("SSO_MISSING_CODE", "the sign-in response carried no authorization code", nil)
	}

	claims, err := s.client.Exchange(ctx, in.Code, flow.CodeVerifier, flow.Nonce)
	if err != nil {
		// A failed exchange burns the flow: the code is spent either way, and
		// leaving the state live would let it be retried against a stolen one.
		_, _ = s.store.ConsumeOIDCLoginFlow(ctx, flow.ID, now)
		return CompleteResult{}, mapProviderError(err)
	}

	if err := s.enforceConstraints(claims); err != nil {
		_, _ = s.store.ConsumeOIDCLoginFlow(ctx, flow.ID, now)
		return CompleteResult{}, err
	}

	user, provisioned, err := s.resolveAccount(ctx, claims, now)
	if err != nil {
		_, _ = s.store.ConsumeOIDCLoginFlow(ctx, flow.ID, now)
		return CompleteResult{}, err
	}

	result := CompleteResult{
		Principal: domain.Principal{
			User:       user,
			AuthMethod: domain.AuthMethodOIDC,
			Issuer:     claims.Issuer,
			Subject:    claims.Subject,
		},
		ReturnTo:    flow.ReturnTo,
		ClientKind:  flow.ClientKind,
		Provisioned: provisioned,
	}

	if flow.ClientKind == domain.OIDCClientDesktop {
		// The desktop supervisor, not the system browser, gets the session.
		// Stamping the user here and minting at Claim means the raw token
		// never travels through a browser URL, a deep link, or a renderer.
		stamped, err := s.store.MarkOIDCLoginFlowAuthenticated(ctx, flow.ID, user.ID, now)
		if err != nil {
			return CompleteResult{}, fmt.Errorf("stamp login flow: %w", err)
		}
		if !stamped {
			return CompleteResult{}, apierr.Invalid("SSO_INVALID_STATE", "this sign-in request is no longer valid; start again", nil)
		}
		return result, nil
	}

	// Browser flow: consume first. A losing racer on a replayed callback gets
	// "start again" instead of a second session.
	consumed, err := s.store.ConsumeOIDCLoginFlow(ctx, flow.ID, now)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("consume login flow: %w", err)
	}
	if !consumed {
		return CompleteResult{}, apierr.Invalid("SSO_INVALID_STATE", "this sign-in request is no longer valid; start again", nil)
	}
	raw, sess, err := s.sessions.CreateSessionAs(ctx, user.ID, domain.AuthMethodOIDC, claims.Issuer, claims.Subject)
	if err != nil {
		return CompleteResult{}, err
	}
	result.Principal.SessionID = sess.ID
	result.SessionToken = raw
	result.SessionExpiresAt = sess.ExpiresAt
	return result, nil
}

// Claim redeems a desktop flow. Possession of the state value alone claims
// nothing: the handoff secret never travelled to the provider and never left
// loopback, so only the supervisor that started the login can finish it.
func (s *Service) Claim(ctx context.Context, flowID, handoffSecret string) (CompleteResult, error) {
	if !s.Enabled() {
		return CompleteResult{}, s.notConfigured()
	}
	invalid := apierr.Unauthorized("SSO_INVALID_HANDOFF", "this sign-in could not be claimed; start again")

	flow, ok, err := s.store.GetOIDCLoginFlow(ctx, strings.TrimSpace(flowID))
	if err != nil {
		return CompleteResult{}, fmt.Errorf("lookup login flow: %w", err)
	}
	now := s.now().UTC()
	if !ok || flow.ClientKind != domain.OIDCClientDesktop || flow.ConsumedAt != nil {
		return CompleteResult{}, invalid
	}
	if !flow.ExpiresAt.After(now) {
		return CompleteResult{}, apierr.Unauthorized("SSO_STATE_EXPIRED", "this sign-in expired; start again")
	}
	if subtle.ConstantTimeCompare([]byte(flow.HandoffSecretHash), []byte(hashSecret(handoffSecret))) != 1 {
		return CompleteResult{}, invalid
	}
	if flow.AuthenticatedUserID == nil {
		// Not an error: the person has not finished at the provider yet. The
		// caller polls, so this is the "keep waiting" answer.
		return CompleteResult{ClientKind: domain.OIDCClientDesktop}, ErrHandoffPending
	}

	consumed, err := s.store.ConsumeOIDCLoginFlow(ctx, flow.ID, now)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("consume login flow: %w", err)
	}
	if !consumed {
		return CompleteResult{}, invalid
	}

	user, found, err := s.store.GetUserByID(ctx, *flow.AuthenticatedUserID)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("lookup authenticated user: %w", err)
	}
	if !found || user.Status != domain.UserStatusActive {
		return CompleteResult{}, apierr.Unauthorized("SSO_ACCOUNT_DISABLED", "this account cannot sign in")
	}

	raw, sess, err := s.sessions.CreateSessionAs(ctx, user.ID, domain.AuthMethodOIDC, s.cfg.Issuer, s.subjectFor(ctx, user.ID))
	if err != nil {
		return CompleteResult{}, err
	}
	return CompleteResult{
		Principal: domain.Principal{
			User:       user,
			AuthMethod: domain.AuthMethodOIDC,
			SessionID:  sess.ID,
			Issuer:     sess.Issuer,
			Subject:    sess.Subject,
		},
		SessionToken:     raw,
		SessionExpiresAt: sess.ExpiresAt,
		ReturnTo:         flow.ReturnTo,
		ClientKind:       domain.OIDCClientDesktop,
	}, nil
}

// ErrHandoffPending is returned by Claim while the person has not finished at
// the provider. It is a normal polling answer, not a failure.
var ErrHandoffPending = errors.New("ssosvc: sign-in not finished yet")

// subjectFor recovers the federated subject for a user at the configured
// issuer, so a desktop-claimed session records the same issuer/subject a
// browser-claimed one would. An unexpectedly missing link leaves the subject
// empty rather than failing a login that has already been fully validated.
func (s *Service) subjectFor(ctx context.Context, userID domain.UserID) string {
	list, err := s.store.ListExternalIdentitiesForUser(ctx, userID)
	if err != nil {
		return ""
	}
	for _, id := range list {
		if id.Issuer == s.cfg.Issuer {
			return id.Subject
		}
	}
	return ""
}

// EndSessionURL returns the provider's RP-initiated logout URL, or "" when the
// provider advertises none.
func (s *Service) EndSessionURL(ctx context.Context, postLogoutRedirect string) (string, error) {
	if !s.Enabled() {
		return "", nil
	}
	u, err := s.client.EndSessionURL(ctx, postLogoutRedirect)
	if err != nil {
		return "", mapProviderError(err)
	}
	return u, nil
}

// PurgeExpiredFlows prunes abandoned login flows.
func (s *Service) PurgeExpiredFlows(ctx context.Context) (int64, error) {
	if s == nil || s.store == nil {
		return 0, nil
	}
	return s.store.DeleteExpiredOIDCLoginFlows(ctx, s.now().UTC())
}

func hashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// sanitizeErrorCode keeps a provider's OAuth error code renderable: OAuth
// error codes are a constrained ASCII vocabulary, so anything outside it is
// not a code and is not echoed.
func sanitizeErrorCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 64 {
		raw = raw[:64]
	}
	for _, r := range raw {
		lower := r >= 'a' && r <= 'z'
		upper := r >= 'A' && r <= 'Z'
		digit := r >= '0' && r <= '9'
		if !lower && !upper && !digit && r != '_' {
			return "unspecified_error"
		}
	}
	if raw == "" {
		return "unspecified_error"
	}
	return raw
}

// SafeReturnTo bounds the post-login redirect destination. Only a same-origin
// absolute PATH is ever honored: no scheme, no host, no protocol-relative
// "//evil.example", no backslash variant. Everything else becomes "/".
//
// This is the open-redirect boundary, and it is a whitelist by construction —
// the destination is rebuilt from a validated path rather than filtered.
func SafeReturnTo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "/" {
		return ""
	}
	// Reject anything a browser could read as an authority component before
	// url.Parse gets a chance to normalize it away.
	if strings.ContainsAny(raw, "\\\r\n\t") {
		return ""
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return ""
	}
	if !strings.HasPrefix(u.Path, "/") {
		return ""
	}
	out := u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

// ResolveReturnTo turns a stored ReturnTo into the concrete destination, using
// fallback when the flow carried none.
func ResolveReturnTo(stored, fallback string) string {
	if stored != "" {
		return stored
	}
	if fallback == "" {
		return "/"
	}
	return fallback
}
