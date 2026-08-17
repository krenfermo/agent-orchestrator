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
}

func userView(u domain.User) UserView {
	return UserView{
		ID:          string(u.ID),
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Username:    u.Username,
		Status:      string(u.Status),
	}
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
	OK bool `json:"ok"`
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
}

// Register mounts the auth routes on the supplied router.
func (c *AuthController) Register(r chi.Router) {
	r.Post("/auth/login", c.login)
	r.Post("/auth/logout", c.logout)
	r.Get("/auth/me", c.me)
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
		envelope.WriteError(w, r, err)
		return
	}
	raw, sess, err := c.Mgr.CreateSession(r.Context(), u.ID)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	setSessionCookie(w, r, raw, sess.ExpiresAt)
	envelope.WriteJSON(w, http.StatusOK, LoginResponse{User: userView(u)})
}

func (c *AuthController) logout(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/auth/logout")
		return
	}
	if cookie, err := r.Cookie(identity.SessionCookieName); err == nil && cookie.Value != "" {
		_ = c.Mgr.RevokeSession(r.Context(), cookie.Value)
	}
	clearSessionCookie(w, r)
	envelope.WriteJSON(w, http.StatusOK, LogoutResponse{OK: true})
}

func (c *AuthController) me(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/auth/me")
		return
	}
	if cookie, err := r.Cookie(identity.SessionCookieName); err == nil && cookie.Value != "" {
		if u, err := c.Mgr.ResolveSession(r.Context(), cookie.Value); err == nil {
			view := userView(u)
			envelope.WriteJSON(w, http.StatusOK, MeResponse{Status: "authenticated", User: &view})
			return
		}
	}
	if c.TrustedLocal {
		if u, ok, err := c.Mgr.BootstrapAdmin(r.Context()); err == nil && ok {
			view := userView(u)
			envelope.WriteJSON(w, http.StatusOK, MeResponse{Status: "trusted-local", User: &view})
			return
		}
		envelope.WriteJSON(w, http.StatusOK, MeResponse{Status: "no_user"})
		return
	}
	envelope.WriteAPIError(w, r, http.StatusUnauthorized, "unauthorized", "NOT_AUTHENTICATED", "authentication required", nil)
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

// setSessionCookie writes the session cookie. Secure is set whenever the
// request itself arrived over TLS or a trusted reverse proxy declares it did
// (X-Forwarded-Proto: https) — the loopback desktop daemon serves plain
// http, so Secure stays unset there, matching how the existing LAN
// preview-file cookie (auth.go's maybeSetPreviewAuthCookie) handles the same
// loopback-vs-network distinction.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     identity.SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     identity.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	})
}

func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
