package skillregistry

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress"
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
	// RegistryHTTPS is a company-private registry served over HTTPS. It is
	// implemented (phase 11): AO reaches exactly one configured origin, over
	// verified TLS, for metadata only, and fetches bytes on a separate path
	// that runs only during an install.
	//
	// It is NOT an open marketplace and it is not AO Official. Both of those
	// are the same transport with a different trust root, and neither exists
	// yet -- see docs/adr/0007.
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
func (t RegistryType) Implemented() bool { return t == RegistryLocal || t == RegistryHTTPS }

// Remote reports whether reading this type means opening a socket. It is what
// decides whether a credential, a network policy and a cache are meaningful for
// a registry -- a local directory has none of the three, and accepting a
// credential for one would record a secret that registry never uses.
func (t RegistryType) Remote() bool { return t == RegistryHTTPS || t == RegistryGit }

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
	// TrustPolicySigned additionally requires a VERIFIED SIGNATURE chaining to
	// a trust root this installation configured. It produces TrustTrusted, and
	// as of phase 12 it is satisfiable: a release AO could only ever call
	// verified before now reaches trusted when a key AO holds signed it.
	//
	// A verified result NEVER satisfies this policy. That is the whole
	// distinction the two states exist to carry: integrity is "these are the
	// bytes that were resolved", and trust is "and this is who said so".
	TrustPolicySigned TrustPolicy = "signed"
	// TrustPolicyOfficial is signed, narrowed to the OFFICIAL tier: the key
	// must chain to a root compiled into this AO build, and the publisher must
	// be the official one.
	//
	// This build carries no official root (see official.go for why shipping a
	// placeholder would be a forgery handed out for free), so a registry on
	// this policy installs nothing and the refusal says so. The machinery is
	// real and tested; the key is a release-engineering act this phase does
	// not perform.
	TrustPolicyOfficial TrustPolicy = "official"
)

// Valid reports whether p is a supported policy.
func (p TrustPolicy) Valid() bool {
	switch p {
	case TrustPolicyDigest, TrustPolicyPinnedPublisher, TrustPolicySigned, TrustPolicyOfficial:
		return true
	}
	return false
}

// RequiresSignature reports whether this policy refuses anything short of
// TrustTrusted.
//
// It is the one place that answers "does verified count here", asked by the
// install path and by every screen that renders a policy. Two policies
// answering yes and a third answering no is a switch somebody widens without
// noticing; a method is a switch the compiler helps with.
func (p TrustPolicy) RequiresSignature() bool {
	return p == TrustPolicySigned || p == TrustPolicyOfficial
}

// AcceptedTiers is which trust-root tiers satisfy this policy.
//
// signed accepts official AND enterprise: an administrator who configured
// their own root meant it, and refusing it would make "signed" mean "signed by
// us", which is what official is for.
func (p TrustPolicy) AcceptedTiers() []TrustTier {
	switch p {
	case TrustPolicyOfficial:
		return []TrustTier{TierOfficial}
	case TrustPolicySigned:
		return []TrustTier{TierOfficial, TierEnterprise}
	}
	return nil
}

// Enforceable reports whether this build can satisfy the policy AT ALL, before
// any particular release is considered.
//
// Phase 12 made signed enforceable: AO verifies signatures now. official stays
// unenforceable only because no official root has been published, and that is
// answered by the caller consulting the trust store rather than by a constant
// here -- which is why this method no longer knows about official either.
func (p TrustPolicy) Enforceable() bool { return true }

var (
	registryIDRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	// A registry credential is stored as the NAME of a sealed secret, in the
	// same UPPER_SNAKE_CASE vocabulary skillcatalog uses for scope.secrets.
	// The shape is enforced so a value pasted into this field is rejected
	// rather than persisted.
	secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	// An API-key header name. RFC 7230 allows more, but a registry asking for
	// a header outside this shape is a registry asking for something worth
	// looking at by hand.
	headerNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
)

// NetworkPolicy is what one registry's traffic may reach BEYOND the default,
// which is "the configured origin, at a public address".
//
// It is a struct rather than a bool so the exception is written down entry by
// entry. An installation whose registry genuinely lives at 10.4.0.0/16 says so,
// and the entry appears in the settings screen and in the audit line -- an
// exception nobody can see is one nobody reviews.
type NetworkPolicy struct {
	// PermittedPrivateCIDRs re-opens private ranges for THIS registry only.
	//
	// It can never re-open link-local, and therefore never the cloud metadata
	// address: skillegress.ParsePermittedCIDRs refuses an overlapping entry,
	// and it is the same function the skill egress proxy validates its
	// exceptions with. One policy, two callers.
	PermittedPrivateCIDRs []string `json:"permittedPrivateCidrs,omitempty"`
}

// Validate rejects an exception that could not be enforced or must not exist.
func (n NetworkPolicy) Validate() error {
	if len(n.PermittedPrivateCIDRs) > 16 {
		return badConfigf("networkPolicy names %d private ranges; a list nobody can read is not a policy",
			len(n.PermittedPrivateCIDRs))
	}
	if _, err := skillegress.ParsePermittedCIDRs(n.PermittedPrivateCIDRs); err != nil {
		return badConfigf("networkPolicy: %v", err)
	}
	return nil
}

// Summary renders the policy for a settings screen and an audit line.
func (n NetworkPolicy) Summary() string {
	if len(n.PermittedPrivateCIDRs) == 0 {
		return "public addresses only"
	}
	return "private ranges permitted: " + strings.Join(n.PermittedPrivateCIDRs, ", ")
}

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
	// AuthType is how the credential is presented. Empty means none, which is
	// the default and is what a local registry always is.
	//
	// It is explicit rather than inferred from the presence of a secret,
	// because "we guessed bearer and the registry wanted a header" is a
	// 401 nobody can debug from the settings screen.
	AuthType AuthType `json:"authType,omitempty"`
	// APIKeyHeader is the header name an api_key_header registry expects.
	// Empty means DefaultAPIKeyHeader. A NAME, printed freely; the value is
	// never here.
	APIKeyHeader string `json:"apiKeyHeader,omitempty"`
	// NetworkPolicy is what this registry's traffic may reach beyond the
	// default of "the configured origin, at a public address".
	NetworkPolicy NetworkPolicy `json:"networkPolicy,omitzero"`
	// CreatedAt and UpdatedAt are the store's, filled on read. They are zero
	// on a Registry a caller is submitting, and the store never takes them
	// from one: a client that could set its own updatedAt could hide a change.
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// EffectiveAuthType is the auth type this registry actually uses. An empty
// configuration means none.
func (r Registry) EffectiveAuthType() AuthType {
	if strings.TrimSpace(string(r.AuthType)) == "" {
		return AuthNone
	}
	return r.AuthType
}

// EffectiveAPIKeyHeader is the header an api_key_header registry uses.
func (r Registry) EffectiveAPIKeyHeader() string {
	if name := strings.TrimSpace(r.APIKeyHeader); name != "" {
		return name
	}
	return DefaultAPIKeyHeader
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
	if r.Type == RegistryHTTPS {
		// The location IS the baseURL, and parsing it is the network policy:
		// https only, one origin, no credentials in the URL, no query. A
		// registry whose address cannot be reduced to one origin is one AO
		// cannot bound, and an unbounded client is an SSRF gadget.
		if _, _, err := ParseBaseURL(location); err != nil {
			return badConfigf("location: %v", err)
		}
	}
	if !r.TrustPolicy.Valid() {
		return badConfigf("trustPolicy %q is not one of digest, pinned_publisher, signed, official",
			r.TrustPolicy)
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
		if !r.Type.Remote() {
			return badConfigf("a local registry reads a directory and needs no credential; " +
				"naming one would record a secret this registry never uses")
		}
	}
	if err := r.validateAuth(); err != nil {
		return err
	}
	if !r.Type.Remote() && len(r.NetworkPolicy.PermittedPrivateCIDRs) > 0 {
		return badConfigf("a %s registry opens no socket, so a network policy on it would be a "+
			"permission that grants nothing and reads as if it did", r.Type)
	}
	if err := r.NetworkPolicy.Validate(); err != nil {
		return err
	}
	return nil
}

// validateAuth keeps the auth type, the secret and the header consistent.
//
// Each refusal below is a state where the configuration would LOOK
// authenticated and behave otherwise, which is the failure worth catching in a
// settings form rather than in a 401 three weeks later.
func (r Registry) validateAuth() error {
	authType := r.EffectiveAuthType()
	if !authType.Valid() {
		return badConfigf("authType %q is not one of none, bearer, api_key_header", r.AuthType)
	}
	if authType != AuthNone && !r.Type.Remote() {
		return badConfigf("a %s registry reads a directory and authenticates to nothing", r.Type)
	}
	name := strings.TrimSpace(r.CredentialSecretName)
	if authType.NeedsSecret() && name == "" {
		return badConfigf("authType %s requires credentialSecretName; a registry set to authenticate "+
			"with no secret to send would fail every request and read as configured", authType)
	}
	if authType == AuthNone && name != "" {
		return badConfigf("credentialSecretName is set and authType is none, so the credential would " +
			"never be sent; set an authType or clear the secret")
	}
	header := strings.TrimSpace(r.APIKeyHeader)
	if header != "" {
		if authType != AuthAPIKeyHeader {
			return badConfigf("apiKeyHeader is only meaningful with authType api_key_header")
		}
		if !headerNameRe.MatchString(header) {
			return badConfigf("apiKeyHeader %q is not a plain header name", header)
		}
		// A credential sent under these names is a credential sent somewhere
		// it will be interpreted, cached or forwarded by something that is not
		// the registry.
		switch strings.ToLower(header) {
		case "authorization", "cookie", "proxy-authorization", "host":
			return badConfigf("apiKeyHeader %q is reserved; use authType bearer for Authorization", header)
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
