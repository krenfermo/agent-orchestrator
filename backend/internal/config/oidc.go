package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// OIDCConfig is P4-A's provider configuration. It is BACKEND-OWNED and read
// once at boot from the environment: no API route sets it, and no API route
// renders ClientSecret — the public description of the provider that the
// frontend receives is built in httpd/controllers and carries a display name
// and a start URL, nothing more.
type OIDCConfig struct {
	// Enabled is true once a usable issuer + client id are configured. It is
	// derived, never set directly: a half-configured provider must not read
	// as "SSO is on".
	Enabled bool
	// Issuer is the provider's issuer identifier — the exact string its
	// discovery document and every ID token must carry in `iss`.
	Issuer string
	// ClientID identifies AO to the provider.
	ClientID string
	// ClientSecret authenticates AO at the token endpoint. Empty means a
	// public client, which is legitimate when the provider allows PKCE-only
	// exchange. It must never leave the backend.
	ClientSecret string
	// RedirectURL is the callback AO registered with the provider. Defaults
	// to the daemon's own loopback callback.
	RedirectURL string
	// Scopes requested at authorization time. Defaults to openid/profile/email.
	Scopes []string
	// DisplayName is what the sign-in button says ("Sign in with Okta").
	DisplayName string
	// AllowedEmailDomains, when non-empty, restricts which email domains may
	// sign in. It is a coarse constraint on top of the provider's own policy,
	// not a substitute for it.
	AllowedEmailDomains []string
	// LinkVerifiedEmail decides what a FIRST login for an unknown
	// (issuer, subject) does when the provider's VERIFIED email matches an
	// account that already exists locally: link to it (true, the default) or
	// refuse (false).
	//
	// The default is true because the alternative strands every account an
	// installation already has the moment SSO is turned on — P4-A ships no
	// account-linking UI, so a refused link has no remedy. It is a toggle
	// rather than a constant because docs/auth-sso-design.md is right that
	// automatic email linking is a takeover vector in a MULTI-provider
	// deployment; an operator running more than one issuer, or one they do
	// not fully trust to verify addresses, turns it off.
	// AO_OIDC_LINK_VERIFIED_EMAIL=off.
	LinkVerifiedEmail bool
	// RequiredClaimName/RequiredClaimValue, when set, additionally require
	// that claim to carry that value (matching a string claim or an element
	// of a string-array claim, e.g. groups=ao-users).
	RequiredClaimName  string
	RequiredClaimValue string
}

// DefaultOIDCDisplayName is the sign-in button label when the operator
// configures none.
const DefaultOIDCDisplayName = "single sign-on"

// OIDCCallbackPath is the daemon route the provider redirects back to. It is
// a constant because it is registered with the provider out of band: it can
// only change alongside the operator's provider configuration.
const OIDCCallbackPath = "/api/v1/auth/oidc/callback"

// loadOIDC reads the OIDC configuration and resolves the installation's
// AuthMode.
//
// The two are resolved together on purpose. "Is trusted-local on" and "is
// OIDC configured" are not independent questions: an install that requires
// SSO must not simultaneously hand a bootstrap-admin identity to any request
// that omits a cookie. So configuring a provider moves the whole installation
// to AuthModeOIDC and switches trusted-local synthesis off — one decision,
// not two switches an operator can get out of step.
//
//	AO_AUTH_MODE            trusted_local|oidc (default: derived)
//	AO_OIDC_ISSUER          provider issuer identifier
//	AO_OIDC_CLIENT_ID       client id
//	AO_OIDC_CLIENT_SECRET   client secret (omit for a public/PKCE-only client)
//	AO_OIDC_REDIRECT_URL    callback URL (default http://127.0.0.1:<port>/api/v1/auth/oidc/callback)
//	AO_OIDC_SCOPES          space- or comma-separated (default "openid profile email")
//	AO_OIDC_DISPLAY_NAME    sign-in button label
//	AO_OIDC_ALLOWED_DOMAINS comma-separated email domains permitted to sign in
//	AO_OIDC_REQUIRED_CLAIM  claim=value that a signing-in identity must carry
//	AO_OIDC_LINK_VERIFIED_EMAIL  link a first federated login to an existing
//	                        local account on a verified email match (default on)
func loadOIDC(cfg *Config) error {
	oidc := OIDCConfig{
		LinkVerifiedEmail: true,
		Issuer:            strings.TrimSpace(os.Getenv("AO_OIDC_ISSUER")),
		ClientID:          strings.TrimSpace(os.Getenv("AO_OIDC_CLIENT_ID")),
		ClientSecret:      os.Getenv("AO_OIDC_CLIENT_SECRET"),
		RedirectURL:       strings.TrimSpace(os.Getenv("AO_OIDC_REDIRECT_URL")),
		DisplayName:       strings.TrimSpace(os.Getenv("AO_OIDC_DISPLAY_NAME")),
	}

	if oidc.Issuer != "" {
		u, err := url.Parse(oidc.Issuer)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("invalid AO_OIDC_ISSUER %q: must be an absolute URL", oidc.Issuer)
		}
		// An https issuer is what the spec requires; http is tolerated only
		// for a loopback provider, which is what the deterministic test
		// harness and a local Keycloak/Dex both are.
		if u.Scheme != "https" && !isLoopbackHostname(u.Hostname()) {
			return fmt.Errorf("invalid AO_OIDC_ISSUER %q: a non-loopback issuer must use https", oidc.Issuer)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("invalid AO_OIDC_ISSUER %q: an issuer identifier carries no query or fragment", oidc.Issuer)
		}
		oidc.Issuer = strings.TrimSuffix(oidc.Issuer, "/")
	}

	if raw := strings.TrimSpace(os.Getenv("AO_OIDC_SCOPES")); raw != "" {
		oidc.Scopes = parseScopes(raw)
	}
	if raw := strings.TrimSpace(os.Getenv("AO_OIDC_ALLOWED_DOMAINS")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(part), "@"))
			if d != "" {
				oidc.AllowedEmailDomains = append(oidc.AllowedEmailDomains, d)
			}
		}
	}
	if raw := strings.TrimSpace(os.Getenv("AO_OIDC_REQUIRED_CLAIM")); raw != "" {
		name, value, ok := strings.Cut(raw, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			return fmt.Errorf("invalid AO_OIDC_REQUIRED_CLAIM %q: expected claim=value", raw)
		}
		oidc.RequiredClaimName, oidc.RequiredClaimValue = name, value
	}

	if raw := strings.TrimSpace(os.Getenv("AO_OIDC_LINK_VERIFIED_EMAIL")); raw != "" {
		v, err := parseToggleEnv("AO_OIDC_LINK_VERIFIED_EMAIL", raw)
		if err != nil {
			return err
		}
		oidc.LinkVerifiedEmail = v
	}

	oidc.Enabled = oidc.Issuer != "" && oidc.ClientID != ""
	if oidc.Issuer != "" && oidc.ClientID == "" {
		return fmt.Errorf("AO_OIDC_ISSUER is set but AO_OIDC_CLIENT_ID is not: an incomplete provider configuration would silently disable SSO")
	}
	if oidc.Enabled {
		if oidc.RedirectURL == "" {
			oidc.RedirectURL = fmt.Sprintf("http://%s:%d%s", LoopbackHost, cfg.Port, OIDCCallbackPath)
		}
		if u, err := url.Parse(oidc.RedirectURL); err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("invalid AO_OIDC_REDIRECT_URL %q: must be an absolute URL", oidc.RedirectURL)
		}
		if oidc.DisplayName == "" {
			oidc.DisplayName = DefaultOIDCDisplayName
		}
		if len(oidc.Scopes) == 0 {
			oidc.Scopes = []string{"openid", "profile", "email"}
		} else if !containsScope(oidc.Scopes, "openid") {
			// Without `openid` the provider runs a plain OAuth flow and never
			// issues an ID token, so AO would have no verifiable identity at
			// all. Adding it is strictly what the operator meant.
			oidc.Scopes = append([]string{"openid"}, oidc.Scopes...)
		}
	}
	cfg.OIDC = oidc

	mode := domain.AuthModeTrustedLocal
	if oidc.Enabled {
		mode = domain.AuthModeOIDC
	}
	if raw := strings.TrimSpace(os.Getenv("AO_AUTH_MODE")); raw != "" {
		switch domain.AuthMode(strings.ToLower(raw)) {
		case domain.AuthModeTrustedLocal:
			mode = domain.AuthModeTrustedLocal
		case domain.AuthModeOIDC:
			if !oidc.Enabled {
				return fmt.Errorf("AO_AUTH_MODE=oidc requires AO_OIDC_ISSUER and AO_OIDC_CLIENT_ID")
			}
			mode = domain.AuthModeOIDC
		default:
			return fmt.Errorf("invalid AO_AUTH_MODE %q: must be trusted_local or oidc", raw)
		}
	}
	cfg.AuthMode = mode
	if mode == domain.AuthModeOIDC {
		// Deriving this rather than trusting a second env var is the point:
		// there is no configuration in which AO both demands SSO and hands
		// out an identity to a request that presents no credential.
		cfg.TrustedLocalMode = false
	}
	return nil
}

func parseScopes(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]struct{}{}
	for _, f := range fields {
		s := strings.TrimSpace(f)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func containsScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

func isLoopbackHostname(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}
