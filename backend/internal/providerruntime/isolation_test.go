package providerruntime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// isolation_test.go pins the setting that separates "who is signed in to AO"
// from "where AO's provider CLIs keep their credentials".
//
// Those were the same switch, and the coupling is how the keychain incident
// reached a machine that had been working for weeks: enabling Google sign-in
// set TrustedLocalMode=false, which moved every provider launch onto an
// AO-owned runtime home, which on macOS moved it onto an AO-owned keychain
// that could not be opened. The person had changed how they log in to AO and
// lost the credential their planner ran on.

func resolverFor(t *testing.T, trustedLocal bool, isolation domain.ProviderRuntimeIsolation) *Resolver {
	t.Helper()
	return &Resolver{
		Owners: fakeOwners{owner: userPtr("u1")},
		Profiles: fakeProfiles{profiles: []domain.ProviderProfile{{
			ID: "p1", UserID: "u1", Harness: domain.HarnessClaudeCode, Enabled: true,
		}}},
		DataDir:      t.TempDir(),
		TrustedLocal: trustedLocal,
		Isolation:    isolation,
	}
}

// The regression: OIDC (TrustedLocal=false) on a single-user desktop no longer
// forces an isolated runtime home when the operator has said otherwise.
func TestResolveForOwner_HostIsolationKeepsTheDesktopCredentials(t *testing.T) {
	r := resolverFor(t, false, domain.ProviderRuntimeIsolationHost)
	env, profileID, err := r.ResolveForOwner(context.Background(), "u1", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if env != nil {
		t.Fatalf("host isolation still substituted the launch environment: %v", env)
	}
	// The profile is still reported: routing, capacity and health scoping all
	// depend on it, and turning off the runtime substitution must not turn off
	// the identity of the provider connection being used.
	if profileID != "p1" {
		t.Fatalf("profile id = %q, want it still reported under host isolation", profileID)
	}
}

// Strict isolates even in trusted-local mode, which is the posture a shared
// install needs and which was previously unreachable.
func TestResolveForOwner_StrictIsolatesEvenInTrustedLocal(t *testing.T) {
	r := resolverFor(t, true, domain.ProviderRuntimeIsolationStrict)
	env, _, err := r.ResolveForOwner(context.Background(), "u1", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if env["HOME"] == "" {
		t.Fatal("strict isolation did not substitute HOME")
	}
	if filepath.Dir(filepath.Dir(env["HOME"])) != filepath.Join(r.DataDir, "users") {
		t.Fatalf("HOME %q is not inside the AO data directory's per-user root", env["HOME"])
	}
}

// Auto is the default and must reproduce the pre-existing derivation exactly,
// in both directions, so an install that never sets this behaves as it always
// has.
func TestResolveForOwner_AutoPreservesTheOldDerivation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		trustedLocal bool
		wantIsolated bool
	}{
		{name: "trusted-local keeps the host runtime", trustedLocal: true, wantIsolated: false},
		{name: "multi-user isolates", trustedLocal: false, wantIsolated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := resolverFor(t, tc.trustedLocal, domain.ProviderRuntimeIsolationAuto)
			env, _, err := r.ResolveForOwner(context.Background(), "u1", domain.HarnessClaudeCode)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if isolated := env != nil; isolated != tc.wantIsolated {
				t.Fatalf("isolated = %v, want %v", isolated, tc.wantIsolated)
			}
		})
	}
}

// The zero value must mean auto. A Resolver constructed without the new field
// (there are several, in different wirings) must not silently change posture.
func TestResolveForOwner_ZeroIsolationMeansAuto(t *testing.T) {
	r := resolverFor(t, true, "")
	env, _, err := r.ResolveForOwner(context.Background(), "u1", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if env != nil {
		t.Fatal("the zero isolation value changed behavior; it must mean auto")
	}
}

// Host isolation must not re-open the multi-user hole it looks like: a user
// with no matching profile is still not blocked, exactly as trusted-local
// already behaved, because host isolation IS the single-desktop posture.
func TestResolveForOwner_HostIsolationDoesNotBlockAnUnconfiguredProfile(t *testing.T) {
	r := &Resolver{
		Owners:       fakeOwners{owner: userPtr("u1")},
		Profiles:     fakeProfiles{},
		DataDir:      t.TempDir(),
		TrustedLocal: false,
		Isolation:    domain.ProviderRuntimeIsolationHost,
	}
	if _, _, err := r.ResolveForOwner(context.Background(), "u1", domain.HarnessClaudeCode); err != nil {
		t.Fatalf("host isolation blocked a launch with no configured profile: %v", err)
	}
}

func TestParseProviderRuntimeIsolation(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    domain.ProviderRuntimeIsolation
		wantErr bool
	}{
		{raw: "", want: domain.ProviderRuntimeIsolationAuto},
		{raw: "auto", want: domain.ProviderRuntimeIsolationAuto},
		{raw: " HOST ", want: domain.ProviderRuntimeIsolationHost},
		{raw: "strict", want: domain.ProviderRuntimeIsolationStrict},
		{raw: "isolated", wantErr: true},
	} {
		got, err := domain.ParseProviderRuntimeIsolation(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseProviderRuntimeIsolation(%q) accepted an invalid value", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseProviderRuntimeIsolation(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("ParseProviderRuntimeIsolation(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
