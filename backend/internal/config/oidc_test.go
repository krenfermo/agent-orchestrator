package config

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// clearOIDCEnv blanks every variable this file reads, so a test observes only
// what it sets regardless of the surrounding environment.
func clearOIDCEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AO_AUTH_MODE", "AO_TRUSTED_LOCAL_MODE", "AO_SESSION_COOKIE_SAMESITE",
		"AO_OIDC_ISSUER", "AO_OIDC_CLIENT_ID", "AO_OIDC_CLIENT_SECRET",
		"AO_OIDC_REDIRECT_URL", "AO_OIDC_SCOPES", "AO_OIDC_DISPLAY_NAME",
		"AO_OIDC_ALLOWED_DOMAINS", "AO_OIDC_REQUIRED_CLAIM", "AO_OIDC_LINK_VERIFIED_EMAIL",
	} {
		t.Setenv(k, "")
	}
}

// The default install — every install that exists today — is unchanged: no
// provider, trusted-local on, Lax cookies.
func TestAuthDefaultsAreTrustedLocalWithNoProvider(t *testing.T) {
	clearOIDCEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OIDC.Enabled {
		t.Error("OIDC reported enabled with no configuration")
	}
	if cfg.AuthMode != domain.AuthModeTrustedLocal {
		t.Errorf("AuthMode = %q, want trusted_local", cfg.AuthMode)
	}
	if !cfg.TrustedLocalMode {
		t.Error("TrustedLocalMode default flipped off")
	}
	if cfg.SessionCookieCrossSite {
		t.Error("session cookie defaulted to cross-site")
	}
}

// Configuring a provider moves the WHOLE installation to OIDC mode and turns
// trusted-local synthesis off. The two are one decision, not two switches.
func TestConfiguringAProviderDisablesTrustedLocalSynthesis(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("AO_OIDC_ISSUER", "https://idp.example.com/")
	t.Setenv("AO_OIDC_CLIENT_ID", "ao")
	t.Setenv("AO_TRUSTED_LOCAL_MODE", "on") // deliberately contradicted below

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OIDC.Enabled {
		t.Fatal("OIDC not enabled with issuer + client id set")
	}
	if cfg.AuthMode != domain.AuthModeOIDC {
		t.Errorf("AuthMode = %q, want oidc", cfg.AuthMode)
	}
	if cfg.TrustedLocalMode {
		t.Error("an OIDC installation still synthesizes an identity for cookie-less requests")
	}
	if cfg.OIDC.Issuer != "https://idp.example.com" {
		t.Errorf("issuer = %q, want the trailing slash trimmed", cfg.OIDC.Issuer)
	}
	if cfg.OIDC.RedirectURL == "" || cfg.OIDC.RedirectURL[len(cfg.OIDC.RedirectURL)-len(OIDCCallbackPath):] != OIDCCallbackPath {
		t.Errorf("redirect URL = %q, want a default ending in %q", cfg.OIDC.RedirectURL, OIDCCallbackPath)
	}
	if cfg.OIDC.DisplayName != DefaultOIDCDisplayName {
		t.Errorf("display name = %q", cfg.OIDC.DisplayName)
	}
	if len(cfg.OIDC.Scopes) != 3 || cfg.OIDC.Scopes[0] != "openid" {
		t.Errorf("scopes = %v", cfg.OIDC.Scopes)
	}
}

// An operator can keep trusted-local even with a provider configured (a
// desktop that offers SSO as an option). The explicit variable wins.
func TestExplicitTrustedLocalAuthModeWinsOverAConfiguredProvider(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("AO_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("AO_OIDC_CLIENT_ID", "ao")
	t.Setenv("AO_AUTH_MODE", "trusted_local")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthMode != domain.AuthModeTrustedLocal {
		t.Errorf("AuthMode = %q, want trusted_local", cfg.AuthMode)
	}
	if !cfg.OIDC.Enabled {
		t.Error("the provider should still be usable as an option in trusted-local mode")
	}
	if !cfg.TrustedLocalMode {
		t.Error("explicit trusted_local did not keep synthesis on")
	}
}

func TestOIDCConfigurationErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"issuer without a client id", map[string]string{
			"AO_OIDC_ISSUER": "https://idp.example.com",
		}},
		{"non-loopback issuer over plain http", map[string]string{
			"AO_OIDC_ISSUER": "http://idp.example.com", "AO_OIDC_CLIENT_ID": "ao",
		}},
		{"issuer carrying a query", map[string]string{
			"AO_OIDC_ISSUER": "https://idp.example.com/?x=1", "AO_OIDC_CLIENT_ID": "ao",
		}},
		{"issuer that is not a URL", map[string]string{
			"AO_OIDC_ISSUER": "idp.example.com", "AO_OIDC_CLIENT_ID": "ao",
		}},
		{"oidc mode demanded with no provider", map[string]string{
			"AO_AUTH_MODE": "oidc",
		}},
		{"unknown auth mode", map[string]string{
			"AO_AUTH_MODE": "kerberos",
		}},
		{"malformed required claim", map[string]string{
			"AO_OIDC_ISSUER": "https://idp.example.com", "AO_OIDC_CLIENT_ID": "ao",
			"AO_OIDC_REQUIRED_CLAIM": "groups",
		}},
		{"unknown samesite value", map[string]string{
			"AO_SESSION_COOKIE_SAMESITE": "strictish",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearOIDCEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("expected Load to reject this configuration")
			}
		})
	}
}

// A loopback issuer over plain http is the one http case that is allowed: it
// is what a local Keycloak/Dex and the deterministic test harness both are.
func TestLoopbackIssuerMayUsePlainHTTP(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("AO_OIDC_ISSUER", "http://127.0.0.1:9999")
	t.Setenv("AO_OIDC_CLIENT_ID", "ao")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OIDC.Enabled {
		t.Error("loopback issuer rejected")
	}
}

func TestOIDCOptionalFieldsParse(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("AO_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("AO_OIDC_CLIENT_ID", "ao")
	t.Setenv("AO_OIDC_CLIENT_SECRET", "shh")
	t.Setenv("AO_OIDC_REDIRECT_URL", "https://ao.example.com/api/v1/auth/oidc/callback")
	t.Setenv("AO_OIDC_SCOPES", "profile, email, groups")
	t.Setenv("AO_OIDC_DISPLAY_NAME", "Okta")
	t.Setenv("AO_OIDC_ALLOWED_DOMAINS", "example.com, @Corp.example ")
	t.Setenv("AO_OIDC_REQUIRED_CLAIM", "groups=ao-users")
	t.Setenv("AO_SESSION_COOKIE_SAMESITE", "none")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// `openid` is prepended when the operator omits it: without it the
	// provider runs a plain OAuth flow and never issues an ID token at all.
	if len(cfg.OIDC.Scopes) != 4 || cfg.OIDC.Scopes[0] != "openid" {
		t.Errorf("scopes = %v, want openid prepended", cfg.OIDC.Scopes)
	}
	want := []string{"example.com", "corp.example"}
	if len(cfg.OIDC.AllowedEmailDomains) != len(want) {
		t.Fatalf("allowed domains = %v", cfg.OIDC.AllowedEmailDomains)
	}
	for i, d := range want {
		if cfg.OIDC.AllowedEmailDomains[i] != d {
			t.Errorf("allowed domain %d = %q, want %q", i, cfg.OIDC.AllowedEmailDomains[i], d)
		}
	}
	if cfg.OIDC.RequiredClaimName != "groups" || cfg.OIDC.RequiredClaimValue != "ao-users" {
		t.Errorf("required claim = %q=%q", cfg.OIDC.RequiredClaimName, cfg.OIDC.RequiredClaimValue)
	}
	if cfg.OIDC.DisplayName != "Okta" {
		t.Errorf("display name = %q", cfg.OIDC.DisplayName)
	}
	if !cfg.SessionCookieCrossSite {
		t.Error("AO_SESSION_COOKIE_SAMESITE=none did not select the cross-site policy")
	}
}
