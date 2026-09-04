package controllers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Audit event names. Stable strings: an operator greps for these.
const (
	AuditEventLoginSucceeded = "ao.auth.login.succeeded"
	AuditEventLoginFailed    = "ao.auth.login.failed"
	AuditEventLogout         = "ao.auth.logout"
	AuditEventSessionExpired = "ao.auth.session.expired"
)

// AuthAudit records identity-lifecycle events to the daemon log and, when a
// telemetry sink is wired, to it as well.
//
// What is recorded is deliberately bounded: the AO user id, the auth method,
// the issuer, the email DOMAIN, an outcome code, and the caller's source key.
// What is never recorded — here or anywhere on this path — is an access token,
// a refresh token, a client secret, an authorization code, or a raw ID token.
// The federated subject is not logged either: it is a stable per-user
// identifier at the provider, and the AO user id already identifies the actor
// for anyone with database access.
type AuthAudit struct {
	Log  *slog.Logger
	Sink ports.EventSink
}

// AuthAuditFields is one audit record's payload.
type AuthAuditFields struct {
	// Outcome is a stable machine code: "ok", or the apierr code of a failure
	// ("SSO_NONCE_MISMATCH", "INVALID_CREDENTIALS", …).
	Outcome string
	// Method is how the caller tried to authenticate.
	Method domain.AuthMethod
	// UserID is set once an account has been resolved.
	UserID domain.UserID
	// Issuer is the OIDC issuer, for a federated attempt.
	Issuer string
	// EmailDomain is the domain part only. The local part is the personal
	// identifier and is not audit material.
	EmailDomain string
	// Source identifies the caller for rate-limit/forensic purposes (remote
	// address host).
	Source string
	// Provisioned reports whether this login created the AO account.
	Provisioned bool
}

func (a AuthAudit) record(ctx context.Context, r *http.Request, name string, level ports.TelemetryLevel, f AuthAuditFields) {
	attrs := []any{"event", name, "outcome", f.Outcome}
	if f.Method != "" {
		attrs = append(attrs, "authMethod", string(f.Method))
	}
	if f.UserID != "" {
		attrs = append(attrs, "userId", string(f.UserID))
	}
	if f.Issuer != "" {
		attrs = append(attrs, "issuer", f.Issuer)
	}
	if f.EmailDomain != "" {
		attrs = append(attrs, "emailDomain", f.EmailDomain)
	}
	if f.Source != "" {
		attrs = append(attrs, "source", f.Source)
	}
	if f.Provisioned {
		attrs = append(attrs, "provisionedAccount", true)
	}
	if a.Log != nil {
		if level == ports.TelemetryLevelWarn {
			a.Log.Warn("auth audit", attrs...)
		} else {
			a.Log.Info("auth audit", attrs...)
		}
	}
	if a.Sink == nil {
		return
	}
	payload := map[string]any{"outcome": f.Outcome}
	if f.Method != "" {
		payload["authMethod"] = string(f.Method)
	}
	if f.UserID != "" {
		payload["userId"] = string(f.UserID)
	}
	if f.Issuer != "" {
		payload["issuer"] = f.Issuer
	}
	if f.EmailDomain != "" {
		payload["emailDomain"] = f.EmailDomain
	}
	if f.Provisioned {
		payload["provisionedAccount"] = true
	}
	var requestID string
	if r != nil {
		requestID = middleware.GetReqID(r.Context())
	}
	a.Sink.Emit(ctx, ports.TelemetryEvent{
		Name:       name,
		Source:     "auth",
		OccurredAt: time.Now().UTC(),
		Level:      level,
		RequestID:  requestID,
		Payload:    payload,
	})
}

// LoginSucceeded records a completed authentication.
func (a AuthAudit) LoginSucceeded(r *http.Request, f AuthAuditFields) {
	f.Outcome = "ok"
	a.record(requestContext(r), r, AuditEventLoginSucceeded, ports.TelemetryLevelInfo, f)
}

// LoginFailed records a rejected authentication attempt, tagged with the
// stable failure code the caller was told.
func (a AuthAudit) LoginFailed(r *http.Request, f AuthAuditFields) {
	if f.Outcome == "" {
		f.Outcome = "unknown_error"
	}
	a.record(requestContext(r), r, AuditEventLoginFailed, ports.TelemetryLevelWarn, f)
}

// Logout records an AO session being invalidated. It says nothing about the
// identity provider's own session: AO ended its own, and claiming more would
// be a lie the audit trail would carry forever.
func (a AuthAudit) Logout(r *http.Request, f AuthAuditFields) {
	f.Outcome = "ok"
	a.record(requestContext(r), r, AuditEventLogout, ports.TelemetryLevelInfo, f)
}

// SessionExpired records a presented session that had already lapsed.
func (a AuthAudit) SessionExpired(r *http.Request, f AuthAuditFields) {
	f.Outcome = "SESSION_EXPIRED"
	a.record(requestContext(r), r, AuditEventSessionExpired, ports.TelemetryLevelInfo, f)
}

func requestContext(r *http.Request) context.Context {
	if r == nil {
		return context.Background()
	}
	return r.Context()
}

// emailDomainOf returns the domain part of an email, or "".
func emailDomainOf(email string) string {
	for i := len(email) - 1; i >= 0; i-- {
		if email[i] == '@' {
			if i == len(email)-1 {
				return ""
			}
			return email[i+1:]
		}
	}
	return ""
}
