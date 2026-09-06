package config

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerauth"
)

// provider_auth_test.go covers the two settings the keychain incident added.
//
// Both default to the behavior AO already had, because an install that never
// sets them must not change posture on upgrade -- and because the defaults are
// the only thing standing between this fix and a second incident in the
// opposite direction.

func TestLoad_ProviderAuthDefaultsPreserveExistingBehavior(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ProviderAuthMode != "" {
		t.Fatalf("ProviderAuthMode = %q, want the auto/unset default", cfg.ProviderAuthMode)
	}
	if isolate := cfg.ProviderRuntimeIsolation.IsolateFor(cfg.TrustedLocalMode); isolate {
		t.Fatal("the default configuration isolates provider runtimes; it must reproduce the trusted-local desktop default")
	}
}

func TestLoad_ProviderAuthMode(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv("AO_PROVIDER_AUTH_MODE", "environment")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ProviderAuthMode != providerauth.ModeEnvironment {
		t.Fatalf("ProviderAuthMode = %q, want environment", cfg.ProviderAuthMode)
	}
}

func TestLoad_RejectsAnInvalidProviderAuthMode(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv("AO_PROVIDER_AUTH_MODE", "oauth")
	if _, err := Load(); err == nil {
		t.Fatal("an invalid AO_PROVIDER_AUTH_MODE was accepted")
	}
}

// The setting that actually resolves the incident on the affected machine: an
// OIDC install (which forces TrustedLocalMode=false) can keep the desktop's
// own provider credentials.
func TestLoad_ProviderRuntimeIsolationHostSurvivesOIDC(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv("AO_AUTH_MODE", "oidc")
	t.Setenv("AO_OIDC_ISSUER", "https://accounts.google.com")
	t.Setenv("AO_OIDC_CLIENT_ID", "test-client-id")
	t.Setenv("AO_PROVIDER_RUNTIME_ISOLATION", "host")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TrustedLocalMode {
		t.Fatal("OIDC no longer implies TrustedLocalMode=false; this test no longer covers the incident")
	}
	if cfg.ProviderRuntimeIsolation != domain.ProviderRuntimeIsolationHost {
		t.Fatalf("ProviderRuntimeIsolation = %q, want host", cfg.ProviderRuntimeIsolation)
	}
	if cfg.ProviderRuntimeIsolation.IsolateFor(cfg.TrustedLocalMode) {
		t.Fatal("host isolation did not survive the OIDC-derived TrustedLocalMode=false, which is the whole point of the setting")
	}
}

func TestLoad_RejectsAnInvalidProviderRuntimeIsolation(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv("AO_PROVIDER_RUNTIME_ISOLATION", "isolated")
	if _, err := Load(); err == nil {
		t.Fatal("an invalid AO_PROVIDER_RUNTIME_ISOLATION was accepted")
	}
}
