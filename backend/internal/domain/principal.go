package domain

import "time"

// AuthMethod names HOW a request's identity was established. P4-A introduces
// it as the third field (after "who" and "which session") that a future
// P4-B authorization decision may legitimately read: an installation can
// reasonably require, say, that a destructive action be taken only by a
// federated identity, and it cannot express that without knowing this.
type AuthMethod string

const (
	// AuthMethodTrustedLocal is the identity the daemon synthesizes for a
	// cookie-less request while AO_TRUSTED_LOCAL_MODE is on: today's
	// single-user desktop, where the loopback listener itself is the trust
	// boundary. No credential was presented, and that is recorded honestly
	// rather than being dressed up as a login.
	AuthMethodTrustedLocal AuthMethod = "trusted_local"
	// AuthMethodPassword is a session issued by Checkpoint 8P-A's
	// username/email + bcrypt password login.
	AuthMethodPassword AuthMethod = "password"
	// AuthMethodOIDC is a session issued by P4-A's OIDC Authorization Code
	// flow. Issuer/Subject on the Principal are populated only for this
	// method.
	AuthMethodOIDC AuthMethod = "oidc"
	// AuthMethodAgent is an AO-LAUNCHED AGENT presenting the scoped credential
	// the daemon minted for its own launch. It is not a login and never
	// belongs to a browser: no person authenticated, and the request may do
	// only what that credential's AgentAuthority permits, intersected with
	// what the account it acts for may do.
	//
	// It is a distinct method rather than a flavour of the others because the
	// distinction is load-bearing in both directions: a browser session must
	// never be resolvable from an agent token, and an agent token must never
	// be accepted where a person's session cookie is expected.
	AuthMethodAgent AuthMethod = "agent"
)

// AuthMode is the installation's identity posture. It is deliberately a
// closed, explicit set rather than a pile of booleans: "is trusted local on"
// and "is OIDC configured" are not independent questions, and treating them
// as such is how an install ends up simultaneously requiring SSO and handing
// out an admin identity to anyone who omits a cookie.
type AuthMode string

const (
	// AuthModeTrustedLocal is the default and preserves today's behavior
	// exactly: a request with no session cookie resolves to the bootstrap
	// admin, and no login screen ever appears.
	AuthModeTrustedLocal AuthMode = "trusted_local"
	// AuthModeOIDC requires a real, provider-backed session. Trusted-local
	// synthesis is off in this mode -- by construction, not by a second
	// switch an operator could forget.
	AuthModeOIDC AuthMode = "oidc"
)

// ExternalIdentity is one federated identity belonging to a user.
//
// (Issuer, Subject) is the canonical key and the ONLY thing AO matches on:
// per OIDC Core 2, `sub` is the only claim guaranteed stable and unique
// within an issuer. Email is a snapshot of the last login's claim, kept for
// display and for the operator-configured domain constraint -- never for
// identifying who this is.
type ExternalIdentity struct {
	ID            string
	UserID        UserID
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastLoginAt   *time.Time
}

// Principal is the canonical answer to "who is making this request", and the
// single shape every layer above the middleware reads. P4-A's whole API
// contribution is that OIDC claim parsing happens once, at the edge, and
// everything downstream sees this instead: a resolved user, the method that
// resolved it, and -- for a federated session only -- the issuer/subject it
// came from.
//
// P4-B ("what may they do?") consumes exactly these fields. P4-A answers only
// "who authenticated?" and deliberately carries no role/permission evaluation
// of its own beyond the User.Role that 8P-E.8 already stored.
type Principal struct {
	// User is the durable AO account this request acts as.
	User User
	// AuthMethod is how that account was established for this request.
	AuthMethod AuthMethod
	// SessionID is the auth_sessions row backing the request, empty for a
	// trusted-local synthesized identity (which has no session row).
	SessionID string
	// Issuer and Subject are the federated identity behind the session, set
	// only when AuthMethod is AuthMethodOIDC.
	Issuer  string
	Subject string
	// Agent is the scoped authority of an AO-launched agent, set only when
	// AuthMethod is AuthMethodAgent and nil for every human request.
	//
	// It NARROWS: authorization grants the intersection of this and what User
	// may do, so an agent acting for an owner is still confined to one
	// project, one session and one run. A nil Agent on a non-agent principal
	// is what keeps every existing decision byte-for-byte unchanged.
	Agent *AgentAuthority
}

// IsAgent reports whether this request is an AO-launched agent acting under a
// scoped credential rather than a person.
func (p Principal) IsAgent() bool {
	return p.AuthMethod == AuthMethodAgent && p.Agent != nil
}

// IsFederated reports whether this principal was established by an external
// identity provider.
func (p Principal) IsFederated() bool { return p.AuthMethod == AuthMethodOIDC }
