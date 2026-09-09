package skillregistry

import (
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func validRegistry() Registry {
	return Registry{
		ID:          "company-private",
		DisplayName: "Company Private",
		Type:        RegistryLocal,
		Location:    "/srv/ao/registry",
		Enabled:     true,
		TrustPolicy: TrustPolicyDigest,
		Priority:    10,
	}
}

func TestRegistry_ValidateAcceptsAWellFormedRegistry(t *testing.T) {
	if err := validRegistry().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestRegistry_ValidateRejectsMalformedConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Registry)
		want   string
	}{
		{"id not kebab", func(r *Registry) { r.ID = "Company_Private" }, "kebab-case"},
		{"no display name", func(r *Registry) { r.DisplayName = " " }, "displayName"},
		{"unknown type", func(r *Registry) { r.Type = "ftp" }, "not one of"},
		// A type AO can store but not read would answer every search with
		// silence, which reads as "this registry has nothing".
		{"declared but unimplemented type", func(r *Registry) { r.Type = RegistryHTTPS; r.Location = "https://x" }, "cannot read it"},
		{"relative location", func(r *Registry) { r.Location = "registry" }, "absolute path"},
		{"traversing location", func(r *Registry) { r.Location = "/srv/../etc" }, "traverse upward"},
		{"unknown policy", func(r *Registry) { r.TrustPolicy = "vibes" }, "trustPolicy"},
		{"pinned policy with no publisher", func(r *Registry) { r.TrustPolicy = TrustPolicyPinnedPublisher }, "requires pinnedPublisher"},
		{"publisher pinned under the wrong policy", func(r *Registry) { r.PinnedPublisher = "acme" }, "only meaningful"},
		{"priority out of range", func(r *Registry) { r.Priority = -1 }, "priority"},
		// A credential is a NAME here, never a value.
		{"credential looks like a value", func(r *Registry) {
			r.Type, r.Location = RegistryLocal, "/srv/ao/registry"
			r.CredentialSecretName = "ghp_realtokenmaterial"
		}, "never a value"},
		{"local registry naming a credential", func(r *Registry) { r.CredentialSecretName = "REGISTRY_TOKEN" }, "needs no credential"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRegistry()
			tc.mutate(&r)
			err := r.Validate()
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate = %v, want ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRegistry_PinnedPublisherPolicyIsValid(t *testing.T) {
	r := validRegistry()
	r.TrustPolicy = TrustPolicyPinnedPublisher
	r.PinnedPublisher = "acme"
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// The signed policy is CONFIGURABLE and not ENFORCEABLE. An administrator may
// legitimately want a registry inert until AO can verify signatures; what must
// not happen is the strictest-looking setting behaving as the weakest.
func TestSignedPolicyIsConfigurableAndNotEnforceable(t *testing.T) {
	r := validRegistry()
	r.TrustPolicy = TrustPolicySigned
	if err := r.Validate(); err != nil {
		t.Fatalf("the signed policy must be configurable: %v", err)
	}
	if r.TrustPolicy.Enforceable() {
		t.Fatal("Enforceable() = true, but AO verifies no signature")
	}
	for _, p := range []TrustPolicy{TrustPolicyDigest, TrustPolicyPinnedPublisher} {
		if !p.Enforceable() {
			t.Fatalf("%q should be enforceable", p)
		}
	}
}

func TestRegistry_VisibleToIsolatesTenants(t *testing.T) {
	installationWide := validRegistry()
	private := validRegistry()
	private.TenantID = "tnt_acme"

	other := []domain.TenantID{"tnt_other"}
	member := []domain.TenantID{"tnt_other", "tnt_acme"}

	if !installationWide.VisibleTo(other) {
		t.Fatal("an installation-wide registry must be visible to every tenant")
	}
	if !installationWide.VisibleTo(nil) {
		t.Fatal("an installation-wide registry must be visible with no tenant memberships")
	}
	if private.VisibleTo(other) {
		t.Fatal("a tenant-scoped registry leaked to a non-member")
	}
	if private.VisibleTo(nil) {
		t.Fatal("a tenant-scoped registry leaked to a caller with no memberships")
	}
	if !private.VisibleTo(member) {
		t.Fatal("a tenant-scoped registry was hidden from its own member")
	}
}

func TestSortRegistries_IsPriorityThenID(t *testing.T) {
	mk := func(id string, priority int) Registry {
		r := validRegistry()
		r.ID, r.Priority = id, priority
		return r
	}
	rs := []Registry{mk("zeta", 5), mk("alpha", 10), mk("beta", 5)}
	SortRegistries(rs)
	got := []string{rs[0].ID, rs[1].ID, rs[2].ID}
	want := []string{"beta", "zeta", "alpha"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
