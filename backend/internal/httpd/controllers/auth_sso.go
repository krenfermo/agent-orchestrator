package controllers

import (
	"errors"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/ssosvc"
)

// OIDCProviderView is the PUBLIC description of the configured provider. It is
// the whole of what the frontend ever learns: a label and where to start.
// Issuer, client id, client secret, scopes and every constraint stay in the
// backend — a secret that is never in a response shape cannot leak through
// one.
type OIDCProviderView struct {
	// DisplayName labels the sign-in button.
	DisplayName string `json:"displayName"`
	// StartPath is the endpoint that begins a login.
	StartPath string `json:"startPath"`
}

// AuthProvidersResponse is the body of GET /api/v1/auth/providers: what
// sign-in methods this installation offers. It is deliberately answerable
// before any authentication, because the login screen has to render from it.
type AuthProvidersResponse struct {
	// Mode is the installation's identity posture.
	Mode string `json:"mode" enum:"trusted_local,oidc"`
	// PasswordEnabled reports whether local username/password sign-in is
	// offered. Always true today: P4-A adds SSO alongside local accounts, it
	// does not remove them.
	PasswordEnabled bool `json:"passwordEnabled"`
	// OIDC is present only when a provider is configured.
	OIDC *OIDCProviderView `json:"oidc,omitempty"`
}

// OIDCStartRequest begins a login. Every field is optional; an empty body
// starts a browser login to the default destination.
type OIDCStartRequest struct {
	// ReturnTo is where to land after sign-in. It is validated to a
	// same-origin absolute path; anything else silently becomes the default.
	ReturnTo string `json:"returnTo,omitempty"`
	// ClientKind is "browser" (default) or "desktop".
	ClientKind string `json:"clientKind,omitempty" enum:"browser,desktop"`
	// HandoffSecret is the desktop supervisor's pickup secret, required for
	// and only for a desktop login. It never travels to the provider.
	HandoffSecret string `json:"handoffSecret,omitempty"`
}

// OIDCStartResponse tells the caller where to send the user agent.
type OIDCStartResponse struct {
	// AuthorizationURL is the provider's authorization endpoint, fully
	// parameterized. The caller navigates or opens it.
	AuthorizationURL string `json:"authorizationUrl"`
	// FlowID is this login's `state`. Safe to hold: on its own it claims
	// nothing, and a desktop pickup additionally needs the handoff secret.
	FlowID string `json:"flowId"`
	// ExpiresAt is when an unfinished login stops being valid.
	ExpiresAt time.Time `json:"expiresAt"`
}

// OIDCClaimRequest redeems a finished desktop login.
type OIDCClaimRequest struct {
	FlowID        string `json:"flowId"`
	HandoffSecret string `json:"handoffSecret"`
}

// OIDCClaimResponse is the desktop pickup's answer. "pending" means the person
// has not finished at the provider yet and the caller should poll again; it is
// a normal state, not an error.
type OIDCClaimResponse struct {
	Status string    `json:"status" enum:"pending,complete"`
	User   *UserView `json:"user,omitempty"`
}

// SSOController owns the /auth/providers and /auth/oidc routes. A nil Mgr
// keeps the routes registered and answers the OpenAPI-backed 501, matching
// every other controller's convention; a wired-but-disabled Mgr answers
// SSO_NOT_CONFIGURED instead, which is a different and more useful fact.
type SSOController struct {
	// Mgr is nil when SSO was never wired.
	Mgr ssosvc.Manager
	// Mode is the installation's resolved identity posture.
	Mode domain.AuthMode
	// Audit records login success/failure.
	Audit AuthAudit
	// CookiePolicy decides the session cookie's SameSite/Secure attributes.
	CookiePolicy SessionCookiePolicy
}

// Register mounts the SSO routes on the supplied router.
func (c *SSOController) Register(r chi.Router) {
	r.Get("/auth/providers", c.providers)
	r.Post("/auth/oidc/start", c.start)
	r.Get("/auth/oidc/callback", c.callback)
	r.Post("/auth/oidc/claim", c.claim)
}

// ssoStartPath is where a login begins. Constant, so the frontend never has to
// build a provider URL itself.
const ssoStartPath = "/api/v1/auth/oidc/start"

func (c *SSOController) providers(w http.ResponseWriter, r *http.Request) {
	mode := c.Mode
	if mode == "" {
		mode = domain.AuthModeTrustedLocal
	}
	out := AuthProvidersResponse{Mode: string(mode), PasswordEnabled: true}
	if c.Mgr != nil && c.Mgr.Enabled() {
		out.OIDC = &OIDCProviderView{DisplayName: c.Mgr.DisplayName(), StartPath: ssoStartPath}
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *SSOController) start(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, ssoStartPath)
		return
	}
	var in OIDCStartRequest
	// An empty body is the common case (the login button posts nothing), so a
	// decode failure on an EMPTY body is not an error.
	if r.ContentLength != 0 {
		if err := decodeJSONStrict(r, &in); err != nil {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
			return
		}
	}
	kind := domain.OIDCClientKind(strings.TrimSpace(in.ClientKind))
	if kind == "" {
		kind = domain.OIDCClientBrowser
	}
	res, err := c.Mgr.Start(r.Context(), ssosvc.StartInput{
		ClientKind:    kind,
		ReturnTo:      in.ReturnTo,
		HandoffSecret: in.HandoffSecret,
	})
	if err != nil {
		c.Audit.LoginFailed(r, AuthAuditFields{
			Method:  domain.AuthMethodOIDC,
			Outcome: apierrCode(err),
			Source:  loginSourceKey(r),
		})
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OIDCStartResponse{
		AuthorizationURL: res.AuthorizationURL,
		FlowID:           res.FlowID,
		ExpiresAt:        res.ExpiresAt,
	})
}

// callback is the provider's redirect target. It is a browser endpoint, not a
// JSON one: it answers with a redirect (browser flow) or a terminal HTML page
// (desktop flow), and never renders a token, a code or a provider message.
func (c *SSOController) callback(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/auth/oidc/callback")
		return
	}
	q := r.URL.Query()
	res, err := c.Mgr.Complete(r.Context(), ssosvc.CallbackInput{
		State:                    q.Get("state"),
		Code:                     q.Get("code"),
		ProviderError:            q.Get("error"),
		ProviderErrorDescription: q.Get("error_description"),
	})
	if err != nil {
		code := apierrCode(err)
		c.Audit.LoginFailed(r, AuthAuditFields{
			Method:  domain.AuthMethodOIDC,
			Outcome: code,
			Source:  loginSourceKey(r),
		})
		// A failed callback cannot know whether it was a browser or a desktop
		// flow when the state itself is what failed, so it answers the shape
		// that is safe either way: an HTML page carrying the stable code.
		// A flow that DID resolve reports through its own surface.
		writeSSOFailurePage(w, code, ssoErrorMessage(err))
		return
	}

	c.Audit.LoginSucceeded(r, AuthAuditFields{
		Method:      domain.AuthMethodOIDC,
		UserID:      res.Principal.User.ID,
		Issuer:      res.Principal.Issuer,
		EmailDomain: emailDomainOf(res.Principal.User.Email),
		Source:      loginSourceKey(r),
		Provisioned: res.Provisioned,
	})

	if res.ClientKind == domain.OIDCClientDesktop {
		// The session is minted at pickup, over loopback, by the supervisor
		// that started this login. Nothing sensitive is rendered here.
		writeSSODesktopDonePage(w)
		return
	}
	setSessionCookie(w, r, c.CookiePolicy, res.SessionToken, res.SessionExpiresAt)
	http.Redirect(w, r, ssosvc.ResolveReturnTo(res.ReturnTo, "/"), http.StatusFound)
}

func (c *SSOController) claim(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/auth/oidc/claim")
		return
	}
	var in OIDCClaimRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	res, err := c.Mgr.Claim(r.Context(), in.FlowID, in.HandoffSecret)
	if errors.Is(err, ssosvc.ErrHandoffPending) {
		envelope.WriteJSON(w, http.StatusOK, OIDCClaimResponse{Status: "pending"})
		return
	}
	if err != nil {
		c.Audit.LoginFailed(r, AuthAuditFields{
			Method:  domain.AuthMethodOIDC,
			Outcome: apierrCode(err),
			Source:  loginSourceKey(r),
		})
		envelope.WriteError(w, r, err)
		return
	}
	// The cookie is the handoff. The raw token is set as a Set-Cookie header
	// on this loopback response and is never placed in the JSON body, so the
	// supervisor can install it into its session cookie jar without any
	// JavaScript ever holding it.
	setSessionCookie(w, r, c.CookiePolicy, res.SessionToken, res.SessionExpiresAt)
	view := userView(res.Principal.User)
	envelope.WriteJSON(w, http.StatusOK, OIDCClaimResponse{Status: "complete", User: &view})
}

// writeSSOFailurePage renders a terminal page for a failed callback. The
// provider's own text is never echoed; only AO's stable code and AO's own
// message, both HTML-escaped.
func writeSSOFailurePage(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Always a 400-class answer: every reachable failure here is a rejected
	// or stale sign-in attempt, and a browser landing on this page needs the
	// explanation far more than it needs a precise status.
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>Sign-in failed</title>" +
		"<body style=\"font-family:system-ui,sans-serif;padding:2rem;max-width:34rem\">" +
		"<h1 style=\"font-size:1.1rem\">Sign-in failed</h1><p>" + html.EscapeString(message) +
		"</p><p style=\"color:#666;font-size:.85rem\">Code: " + html.EscapeString(code) +
		"</p><p style=\"color:#666;font-size:.85rem\">You can close this window and try again.</p></body>"))
}

// writeSSODesktopDonePage is the terminal page a desktop login lands on. It
// carries no token, no code and no identifier.
func writeSSODesktopDonePage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>Signed in</title>" +
		"<body style=\"font-family:system-ui,sans-serif;padding:2rem;max-width:34rem\">" +
		"<h1 style=\"font-size:1.1rem\">You are signed in</h1>" +
		"<p>Return to Agent Orchestrator; you can close this window.</p></body>"))
}

// apierrCode extracts the stable machine code from a structured error, or a
// generic marker for anything unstructured.
func apierrCode(err error) string {
	var e *apierr.Error
	if errors.As(err, &e) && e.Code != "" {
		return e.Code
	}
	return "INTERNAL_ERROR"
}

// errorMessage returns the safe, human-facing text of a structured error. An
// unstructured error is never rendered: it could carry an upstream response.
func ssoErrorMessage(err error) string {
	var e *apierr.Error
	if errors.As(err, &e) && e.Message != "" {
		return e.Message
	}
	return "Sign-in could not be completed."
}
