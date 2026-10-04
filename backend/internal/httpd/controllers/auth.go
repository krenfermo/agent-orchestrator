package controllers

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	ssosvc "github.com/aoagents/agent-orchestrator/backend/internal/service/ssosvc"
)

// UserView is the wire shape of a resolved user — deliberately excludes
// PasswordHash and any session-token material; it is the only shape a
// domain.User is ever rendered into on the wire.
type UserView struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Username    string `json:"username"`
	Status      string `json:"status" enum:"active,disabled"`
	Role        string `json:"role" enum:"owner,admin,member,viewer"`
}

func userView(u domain.User) UserView {
	return UserView{
		ID:          string(u.ID),
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Username:    u.Username,
		Status:      string(u.Status),
		Role:        string(u.Role),
	}
}

// SetupStatusResponse is the body of GET /api/v1/auth/setup-status.
type SetupStatusResponse struct {
	SetupRequired bool `json:"setupRequired"`
}

// RegisterRequest is the body of POST /api/v1/auth/register. Only usable
// while SetupStatusResponse.SetupRequired is true — see AuthController.register.
type RegisterRequest struct {
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

// AdminResetPasswordRequest is the body of the loopback-only
// POST /api/v1/auth/admin/reset-password.
type AdminResetPasswordRequest struct {
	Email       string `json:"email"`
	NewPassword string `json:"newPassword"`
}

// AdminResetPasswordResponse is the body of a successful admin reset-password call.
type AdminResetPasswordResponse struct {
	OK bool `json:"ok"`
}

// LoginRequest is the body of POST /api/v1/auth/login.
type LoginRequest struct {
	UsernameOrEmail string `json:"usernameOrEmail"`
	Password        string `json:"password"`
}

// LoginResponse is the body of a successful POST /api/v1/auth/login.
type LoginResponse struct {
	User UserView `json:"user"`
}

// LogoutResponse is the body of a successful POST /api/v1/auth/logout.
type LogoutResponse struct {
	// OK reports that the AO session was invalidated. That is the ONLY thing
	// this endpoint ever promises.
	OK bool `json:"ok"`
	// ProviderEndSessionURL is the identity provider's RP-initiated logout
	// URL, present only when the session was federated AND the provider
	// advertises an end_session_endpoint. Its presence is an OFFER, not a
	// claim: AO ended its own session and nothing more. A client that wants
	// the provider session ended too must navigate here; a client that does
	// not is still fully signed out of AO.
	ProviderEndSessionURL string `json:"providerEndSessionUrl,omitempty"`
}

// MeResponse is the body of GET /api/v1/auth/me. Status distinguishes a
// real, cookie-resolved session ("authenticated") from trusted-local mode's
// synthesized identity ("trusted-local"), from trusted-local mode with no
// bootstrap admin created yet ("no_user") — a state the frontend renders
// sensibly rather than crashing on a null user — from multi-user mode with
// no session at all, which never reaches this shape because it 401s instead.
type MeResponse struct {
	Status string    `json:"status" enum:"authenticated,trusted-local,no_user"`
	User   *UserView `json:"user,omitempty"`
	// AuthMethod is HOW this identity was established: "trusted_local" for a
	// synthesized desktop identity, "password" for a local login, "oidc" for
	// a federated one. P4-A: the frontend renders sign-out differently for a
	// federated session, and a client should never have to guess this from
	// the presence of a cookie.
	AuthMethod string `json:"authMethod,omitempty" enum:"trusted_local,password,oidc"`
	// Issuer is the identity provider behind a federated session, empty
	// otherwise. It names the provider; it is not a secret and carries no
	// token material.
	Issuer string `json:"issuer,omitempty"`
	// Permissions is P4-B's capability list: the INSTALLATION-WIDE permissions
	// this identity holds, evaluated by the backend. The frontend renders
	// administration navigation and controls from this and never from the role
	// name -- a role is an input to authorization, not a substitute for it, and
	// a renderer that maps roles to buttons has quietly become a second
	// authorization implementation that can disagree with the real one.
	//
	// Hiding a control is convenience only. Every route these permissions
	// describe is enforced again on the way in.
	Permissions []string `json:"permissions"`
}

// AuthController owns the /auth routes (Checkpoint 8P-A). A nil Mgr keeps
// routes registered but returns OpenAPI-backed 501s, matching every other
// controller's convention.
type AuthController struct {
	Mgr authsvc.Manager
	// TrustedLocal mirrors config.Config.TrustedLocalMode. When true, GET
	// /me synthesizes the bootstrap admin identity in the absence of a
	// session cookie, matching the identity middleware's own behavior.
	TrustedLocal bool
	// SSO is P4-A's OIDC surface, consulted on logout so the response can
	// offer the provider's own end-session URL when it advertises one.
	// Optional: nil simply means no provider logout is offered.
	SSO ssosvc.Manager
	// Audit records login/logout outcomes.
	Audit AuthAudit
	// CookiePolicy decides the session cookie's SameSite/Secure attributes.
	CookiePolicy SessionCookiePolicy
	// Authz is P4-B's evaluator, consulted by GET /me to report the caller's
	// effective capabilities. Nil simply reports none, which renders an app
	// with no administration surfaces -- the safe direction.
	Authz Authorizer
}

// Register mounts the auth routes on the supplied router.
func (c *AuthController) Register(r chi.Router) {
	r.Post("/auth/login", c.login)
	r.Post("/auth/logout", c.logout)
	r.Get("/auth/me", c.me)
	r.Get("/auth/setup-status", c.setupStatus)
	r.Post("/auth/register", c.register)
	r.Post("/auth/admin/reset-password", c.adminResetPassword)
}

func (c *AuthController) login(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/auth/login")
		return
	}
	var in LoginRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	u, err := c.Mgr.Authenticate(r.Context(), in.UsernameOrEmail, in.Password, loginSourceKey(r))
	if err != nil {
		c.Audit.LoginFailed(r, AuthAuditFields{
			Method:  domain.AuthMethodPassword,
			Outcome: apierrCode(err),
			Source:  loginSourceKey(r),
		})
		envelope.WriteError(w, r, err)
		return
	}
	raw, sess, err := c.Mgr.CreateSession(r.Context(), u.ID)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	setSessionCookie(w, r, c.CookiePolicy, raw, sess.ExpiresAt)
	c.Audit.LoginSucceeded(r, AuthAuditFields{
		Method:      domain.AuthMethodPassword,
		UserID:      u.ID,
		EmailDomain: emailDomainOf(u.Email),
		Source:      loginSourceKey(r),
	})
	envelope.WriteJSON(w, http.StatusOK, LoginResponse{User: userView(u)})
}

func (c *AuthController) logout(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/auth/logout")
		return
	}
	out := LogoutResponse{OK: true}
	var actor AuthAuditFields
	actor.Method = domain.AuthMethodTrustedLocal
	if cookie, err := r.Cookie(identity.SessionCookieName); err == nil && cookie.Value != "" {
		// Resolve BEFORE revoking: after revocation there is nothing left to
		// attribute the audit line to, and the provider-logout offer depends
		// on whether this session was federated at all.
		if p, err := c.Mgr.ResolvePrincipal(r.Context(), cookie.Value); err == nil {
			actor = AuthAuditFields{
				Method:      p.AuthMethod,
				UserID:      p.User.ID,
				Issuer:      p.Issuer,
				EmailDomain: emailDomainOf(p.User.Email),
			}
			if p.IsFederated() && c.SSO != nil {
				// A provider that advertises no end-session endpoint yields
				// "", and the field is then absent: AO never implies it ended
				// a session it cannot end.
				if endURL, err := c.SSO.EndSessionURL(r.Context(), ""); err == nil {
					out.ProviderEndSessionURL = endURL
				}
			}
		}
		_ = c.Mgr.RevokeSession(r.Context(), cookie.Value)
	}
	clearSessionCookie(w, r, c.CookiePolicy)
	actor.Source = loginSourceKey(r)
	c.Audit.Logout(r, actor)
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *AuthController) me(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/auth/me")
		return
	}
	if cookie, err := r.Cookie(identity.SessionCookieName); err == nil && cookie.Value != "" {
		p, err := c.Mgr.ResolvePrincipal(r.Context(), cookie.Value)
		if err == nil {
			view := userView(p.User)
			envelope.WriteJSON(w, http.StatusOK, MeResponse{
				Status:      "authenticated",
				User:        &view,
				AuthMethod:  string(p.AuthMethod),
				Issuer:      p.Issuer,
				Permissions: c.permissionsFor(r, p),
			})
			return
		}
		if apierrCode(err) == "SESSION_EXPIRED" {
			// An expired session is a distinct, auditable event: it is the
			// difference between "this person's access lapsed" and "someone
			// presented a token that never existed".
			c.Audit.SessionExpired(r, AuthAuditFields{Source: loginSourceKey(r)})
		}
	}
	if c.TrustedLocal {
		if u, ok, err := c.Mgr.BootstrapAdmin(r.Context()); err == nil && ok {
			view := userView(u)
			envelope.WriteJSON(w, http.StatusOK, MeResponse{
				Status:      "trusted-local",
				User:        &view,
				AuthMethod:  string(domain.AuthMethodTrustedLocal),
				Permissions: c.permissionsFor(r, authsvc.TrustedLocalPrincipal(u)),
			})
			return
		}
		envelope.WriteJSON(w, http.StatusOK, MeResponse{Status: "no_user", Permissions: []string{}})
		return
	}
	envelope.WriteAPIError(w, r, http.StatusUnauthorized, "unauthorized", "NOT_AUTHENTICATED", "authentication required", nil)
}

func (c *AuthController) setupStatus(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/auth/setup-status")
		return
	}
	required, err := c.Mgr.SetupRequired(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, SetupStatusResponse{SetupRequired: required})
}

// register creates the installation's first (owner) account. Only usable
// while zero users exist — authsvc.RegisterFirstUser rejects any call after
// the first succeeds, whether from a genuine second attempt or a concurrent
// racer, via the ux_users_single_owner unique index.
func (c *AuthController) register(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/auth/register")
		return
	}
	var in RegisterRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	u, err := c.Mgr.RegisterFirstUser(r.Context(), authsvc.CreateUserInput{
		DisplayName: in.DisplayName,
		Email:       in.Email,
		Username:    in.Email,
		Password:    in.Password,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	raw, sess, err := c.Mgr.CreateSession(r.Context(), u.ID)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	setSessionCookie(w, r, c.CookiePolicy, raw, sess.ExpiresAt)
	c.Audit.LoginSucceeded(r, AuthAuditFields{
		Method:      domain.AuthMethodPassword,
		UserID:      u.ID,
		EmailDomain: emailDomainOf(u.Email),
		Source:      loginSourceKey(r),
		Provisioned: true,
	})
	envelope.WriteJSON(w, http.StatusOK, LoginResponse{User: userView(u)})
}

// adminResetPassword lets the local machine operator reset a known account's
// password without a session — the recovery path for "I forgot my
// password and have no other account to sign in with." Mounted under
// /api/v1/auth/admin, which lan_listener.go's lanControlBlockedPrefixes
// makes unreachable over the LAN listener: this only ever answers on the
// loopback listener, the same trust boundary AO_BOOTSTRAP_ADMIN_* env vars
// already rely on.
func (c *AuthController) adminResetPassword(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/auth/admin/reset-password")
		return
	}
	var in AdminResetPasswordRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	if err := c.Mgr.ResetPassword(r.Context(), in.Email, in.NewPassword); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, AdminResetPasswordResponse{OK: true})
}

// loginSourceKey identifies the caller for login-lockout throttling — the
// remote address's host part, same shape as httpd/auth.go's sourceKey for
// the mobile-pairing lockout.
func loginSourceKey(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// SessionCookiePolicy decides the session cookie's SameSite/Secure attributes.
// It exists because AO has two genuinely different deployments and one
// hard-coded answer cannot serve both:
//
//   - A browser deployment renders the app from the daemon's OWN origin, so
//     the cookie is same-site and SameSiteLax is exactly right: it is sent on
//     every in-app request and withheld from cross-site ones.
//
//   - The DESKTOP renders from a custom `app://` scheme and calls the daemon
//     at http://127.0.0.1:<port>. That is a cross-site request, so a Lax
//     cookie is never sent and a session cookie is effectively inert there.
//     SameSiteNone (which browsers accept only alongside Secure, and which
//     Chromium honors on a loopback origin because loopback is a trustworthy
//     origin) is what makes a real session usable in the desktop app.
//
// The zero value is the Lax behavior 8P-A shipped, so nothing changes for any
// deployment that does not explicitly ask for the other one.
type SessionCookiePolicy struct {
	// CrossSite selects SameSite=None; Secure. Set only by a deployment whose
	// renderer is a different origin than the daemon (the Electron desktop),
	// via AO_SESSION_COOKIE_SAMESITE=none.
	CrossSite bool
}

func (p SessionCookiePolicy) sameSite() http.SameSite {
	if p.CrossSite {
		return http.SameSiteNoneMode
	}
	return http.SameSiteLaxMode
}

// secure reports the Secure attribute. SameSite=None is only honored on a
// Secure cookie, so the two move together; otherwise Secure tracks whether the
// request actually arrived over TLS (directly or through a proxy that says so).
func (p SessionCookiePolicy) secure(r *http.Request) bool {
	return p.CrossSite || requestIsSecure(r)
}

// setSessionCookie writes the session cookie under the given policy. HttpOnly
// is unconditional: the token is never readable from JavaScript, in any
// deployment, which is what keeps it out of localStorage and out of any XSS
// payload's reach.
func setSessionCookie(w http.ResponseWriter, r *http.Request, policy SessionCookiePolicy, token string, expiresAt time.Time) {
	//nolint:gosec // G124: HttpOnly+SameSite always set; Secure tracks the policy and the actual scheme.
	http.SetCookie(w, &http.Cookie{
		Name:     identity.SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: policy.sameSite(),
		Secure:   policy.secure(r),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request, policy SessionCookiePolicy) {
	// The clearing cookie must match the attributes the browser stored it
	// under, or the old cookie survives the "logout" that appeared to work.
	//nolint:gosec // G124: HttpOnly+SameSite always set; Secure tracks the policy and the actual scheme.
	http.SetCookie(w, &http.Cookie{
		Name:     identity.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: policy.sameSite(),
		Secure:   policy.secure(r),
	})
}

func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// permissionsFor resolves the caller's installation-wide capabilities. A nil
// evaluator, or a resolution failure, reports an EMPTY list rather than a
// permissive one: a UI that renders nothing it cannot prove is allowed is a
// degraded experience, while a UI that renders everything on a failed lookup
// is a lie about authority.
func (c *AuthController) permissionsFor(r *http.Request, p domain.Principal) []string {
	if c.Authz == nil {
		return []string{}
	}
	sub, err := c.Authz.Resolve(r.Context(), p)
	if err != nil {
		return []string{}
	}
	perms := sub.GlobalPermissions()
	out := make([]string, 0, len(perms))
	for _, perm := range perms {
		out = append(out, string(perm))
	}
	return out
}
