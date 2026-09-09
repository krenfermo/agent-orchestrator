package skillregistry

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrInvalidConfig wraps every registry-configuration failure.
var ErrInvalidConfig = errors.New("skillregistry: invalid registry configuration")

func badConfigf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

// RegistryType is where a registry's releases physically come from.
type RegistryType string

const (
	// RegistryLocal is a directory on this host holding an index file and the
	// package trees it names. It is the only type this build implements, and
	// it is what a fixture, an offline mirror and an air-gapped install all
	// use.
	RegistryLocal RegistryType = "local"
	// RegistryHTTPS is an AO official or company-private registry served over
	// HTTPS. Declared so the configuration shape does not have to change when
	// one exists; refused at construction today, because a type AO can store
	// but not read would be a registry that silently returns nothing.
	RegistryHTTPS RegistryType = "https"
	// RegistryGit is a git remote holding the same index layout. Declared and
	// refused for the same reason.
	RegistryGit RegistryType = "git"
)

// Valid reports whether t is a declared type, implemented or not.
func (t RegistryType) Valid() bool {
	switch t {
	case RegistryLocal, RegistryHTTPS, RegistryGit:
		return true
	}
	return false
}

// Implemented reports whether this build can actually read this type.
func (t RegistryType) Implemented() bool { return t == RegistryLocal }

// TrustPolicy is what a registry must satisfy BEYOND integrity before an
// install from it proceeds.
//
// Integrity itself is not a policy option. Every install verifies both digests
// over the bytes AO fetched, and a mismatch is refused whatever the policy
// says; a "skip the hash check" setting would be a setting to install
// something other than what was resolved.
type TrustPolicy string

const (
	// TrustPolicyDigest requires only that AO's own hashes match the pinned
	// release. It is the default and it produces TrustVerified.
	TrustPolicyDigest TrustPolicy = "digest"
	// TrustPolicyPinnedPublisher additionally requires the release's publisher
	// to equal the registry's configured PinnedPublisher. It is the control
	// for "this registry only ever serves OUR packages", and it is what
	// catches a registry that starts publishing somebody else's name.
	TrustPolicyPinnedPublisher TrustPolicy = "pinned_publisher"
	// TrustPolicySigned additionally requires a verified signature.
	//
	// AO verifies none, so a registry on this policy installs NOTHING, and the
	// refusal names the missing verification. That is deliberate and it is the
	// honest shape: the alternative is a policy that reads as the strictest
	// setting and silently behaves as the weakest.
	TrustPolicySigned TrustPolicy = "signed"
)

// Valid reports whether p is a supported policy.
func (p TrustPolicy) Valid() bool {
	switch p {
	case TrustPolicyDigest, TrustPolicyPinnedPublisher, TrustPolicySigned:
		return true
	}
	return false
}

// Enforceable reports whether this build can actually satisfy the policy. A
// policy that cannot be satisfied is not an error to CONFIGURE -- an
// administrator may legitimately want their registry inert until AO can verify
// signatures -- but every install under it is refused.
func (p TrustPolicy) Enforceable() bool { return p != TrustPolicySigned }

var (
	registryIDRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	// A registry credential is stored as the NAME of a sealed secret, in the
	// same UPPER_SNAKE_CASE vocabulary skillcatalog uses for scope.secrets.
	// The shape is enforced so a value pasted into this field is rejected
	// rather than persisted.
	secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// Registry is one configured source of skill releases.
type Registry struct {
	// ID is stable and referenced by every install this registry produced.
	ID          string       `json:"id"`
	DisplayName string       `json:"displayName"`
	Type        RegistryType `json:"type"`
	// Location is the type's address: an absolute directory for local, a URL
	// for https, a remote for git.
	Location string `json:"location"`
	// Enabled false keeps the row, its installs and their provenance, and
	// stops it answering searches or serving installs. Removing a registry is
	// a different act with a different consequence.
	Enabled bool `json:"enabled"`
	// TrustPolicy is what this registry must satisfy beyond integrity.
	TrustPolicy TrustPolicy `json:"trustPolicy"`
	// PinnedPublisher is required by, and only meaningful for, the
	// pinned_publisher policy.
	PinnedPublisher string `json:"pinnedPublisher,omitempty"`
	// Priority orders registries when more than one offers the same skill id.
	// Lower is earlier. It breaks display ties only -- it never decides which
	// registry an install comes from, because an install names its registry.
	Priority int `json:"priority"`
	// TenantID limits the registry to one organization. Empty means every
	// tenant on this installation can see and install from it.
	//
	// This is the tenant boundary the phase brief asks for, and it is checked
	// against the CALLER's tenant memberships rather than against anything in
	// a request: a caller who could name their own tenant could name one with
	// a private registry attached.
	TenantID domain.TenantID `json:"tenantId,omitempty"`
	// CredentialSecretName names a sealed secret holding this registry's
	// credential. It is a NAME, never a value: internal/secretbox and the
	// skill_secrets table already seal values under an administrator-managed
	// key, and a second, plaintext home for a credential would be a second way
	// to leak one.
	CredentialSecretName string `json:"credentialSecretName,omitempty"`
}

// Validate enforces the configuration contract.
func (r Registry) Validate() error {
	if !registryIDRe.MatchString(r.ID) {
		return badConfigf("id %q must be lowercase kebab-case", r.ID)
	}
	if len(r.ID) > 64 {
		return badConfigf("id %q is longer than 64 characters", r.ID)
	}
	if strings.TrimSpace(r.DisplayName) == "" {
		return badConfigf("displayName is required")
	}
	if !r.Type.Valid() {
		return badConfigf("type %q is not one of local, https, git", r.Type)
	}
	if !r.Type.Implemented() {
		return badConfigf("type %q is declared but this build cannot read it; "+
			"a registry AO can store and not read would answer every search with silence", r.Type)
	}
	location := strings.TrimSpace(r.Location)
	if location == "" {
		return badConfigf("location is required")
	}
	if r.Type == RegistryLocal {
		if !filepath.IsAbs(location) {
			return badConfigf("a local registry location must be an absolute path")
		}
		if strings.Contains(location, "..") {
			return badConfigf("a local registry location must not traverse upward")
		}
	}
	if !r.TrustPolicy.Valid() {
		return badConfigf("trustPolicy %q is not one of digest, pinned_publisher, signed", r.TrustPolicy)
	}
	pinned := strings.TrimSpace(r.PinnedPublisher)
	if r.TrustPolicy == TrustPolicyPinnedPublisher && pinned == "" {
		return badConfigf("trustPolicy pinned_publisher requires pinnedPublisher")
	}
	if r.TrustPolicy != TrustPolicyPinnedPublisher && pinned != "" {
		return badConfigf("pinnedPublisher is only meaningful with trustPolicy pinned_publisher")
	}
	if pinned != "" && !publisherRe.MatchString(pinned) {
		return badConfigf("pinnedPublisher %q must be a plain identifier", pinned)
	}
	if r.Priority < 0 || r.Priority > 9999 {
		return badConfigf("priority must be between 0 and 9999")
	}
	if name := strings.TrimSpace(r.CredentialSecretName); name != "" {
		if !secretNameRe.MatchString(name) {
			return badConfigf("credentialSecretName %q must be an UPPER_SNAKE_CASE secret NAME, never a value", name)
		}
		if r.Type == RegistryLocal {
			return badConfigf("a local registry reads a directory and needs no credential; " +
				"naming one would record a secret this registry never uses")
		}
	}
	return nil
}

// VisibleTo reports whether a caller who belongs to these tenants may see this
// registry. An installation-wide registry (no tenant) is visible to everyone;
// a tenant-scoped one is visible only to members of that tenant.
//
// It is deliberately a method on the CONFIG rather than a query filter: the
// same rule has to hold for search, for detail, and for install, and three
// copies of it in three SQL statements is how one of them ends up wrong.
func (r Registry) VisibleTo(tenants []domain.TenantID) bool {
	if strings.TrimSpace(string(r.TenantID)) == "" {
		return true
	}
	for _, t := range tenants {
		if t == r.TenantID {
			return true
		}
	}
	return false
}

// SortRegistries orders registries by priority, then by id. Every listing uses
// it so "the first registry" means one thing.
func SortRegistries(rs []Registry) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Priority != rs[j].Priority {
			return rs[i].Priority < rs[j].Priority
		}
		return rs[i].ID < rs[j].ID
	})
}
