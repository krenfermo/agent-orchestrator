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
}

// IsFederated reports whether this principal was established by an external
// identity provider.
func (p Principal) IsFederated() bool { return p.AuthMethod == AuthMethodOIDC }
