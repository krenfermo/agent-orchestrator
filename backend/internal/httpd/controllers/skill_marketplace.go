package controllers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// skill_marketplace.go — the registry / marketplace surface.
//
// It sits under /skills, so the global rule table already gates it: settings.
// read to search and look, settings.manage to configure a registry or install
// from one. That is the right gate and it needed no change — installing a
// package puts code on this host, which is installation administration, and it
// is deliberately NOT something a project administrator can do for their own
// project. Enabling a skill on a project stays where it was, under
// project.manage, and running it stays where it was.
//
// # What this surface deliberately does not have
//
// A Run route, or anything that would give a client one. Installing moves an
// artifact exactly one step: from AVAILABLE to INSTALLED. Reaching a project
// takes a separate call under a separate permission, and reaching a container
// takes an image approval on top of that. A marketplace with a Run button would
// collapse three decisions that exist to be made separately.
//
// # What a search response carries that a client must not compute
//
// The trust state and the compatibility verdict come from the daemon. A client
// that derived "verified" from the presence of a digest would be inventing an
// assurance nobody checked, and a client that hard-coded a compatibility rule
// would drift from the one the install path enforces.

// SkillMarketplace is the service surface this controller needs.
type SkillMarketplace interface {
	ListRegistryViews(ctx context.Context, tenants []domain.TenantID) ([]SkillRegistryView, error)
	SaveRegistryView(ctx context.Context, in SaveSkillRegistryInput) (SkillRegistryView, error)
	RemoveRegistryView(ctx context.Context, in RemoveSkillRegistryInput) error

	SearchMarketplace(ctx context.Context, in SkillMarketplaceSearchInput) (SkillMarketplaceSearchResponse, error)
	GetMarketplaceRelease(ctx context.Context, in SkillReleaseLookupInput) (SkillReleaseDetailResponse, error)
	InstallMarketplaceRelease(ctx context.Context, in SkillInstallReleaseInput) (SkillInstallOutcomeView, error)
	CheckMarketplaceUpdates(ctx context.Context, in SkillUpdateCheckInput) (SkillUpdateCheckResponse, error)

	// Phase 11: the two operations that only exist once a registry is on the
	// other end of a socket. Both are settings.manage, because both make AO
	// act on the network with a credential somebody granted it.
	TestSkillRegistryConnection(ctx context.Context, in SkillRegistryProbeInput) (SkillRegistryProbeView, error)
	SyncSkillRegistryRevocations(ctx context.Context, in SkillRegistryProbeInput) (SkillRegistryRevocationSyncView, error)
}

// SkillTenancy resolves which organizations a caller belongs to.
//
// It is a separate, deliberately tiny port rather than a field on the
// marketplace service, because the answer is a property of the REQUEST and the
// service must never take a tenant from a request body: a caller who could name
// their own tenant could name one with a private registry attached to it.
type SkillTenancy interface {
	ListActiveTenantMembershipsForUser(ctx context.Context, user domain.UserID) ([]domain.TenantMembership, error)
	ListTenants(ctx context.Context) ([]domain.Tenant, error)
}

// ---------------------------------------------------------------- input types

// SaveSkillRegistryInput carries a registry configuration to the service.
type SaveSkillRegistryInput struct {
	ID                    string
	DisplayName           string
	Type                  string
	Location              string
	Enabled               bool
	TrustPolicy           string
	PinnedPublisher       string
	Priority              int
	TenantID              string
	CredentialSecretName  string
	AuthType              string
	APIKeyHeader          string
	PermittedPrivateCIDRs []string
	// Owner, Repository and AllowedOwners are the external scope. They are on
	// the INPUT and not only on the wire body because the two are separate
	// types on purpose, and a field that exists in one and not the other is a
	// field a client can send and nothing will ever read -- which is how a
	// whole registry type ends up impossible to configure while every test
	// that talks to the service directly still passes.
	Owner         string
	Repository    string
	AllowedOwners []string

	Actor            string
	ActorPermissions []domain.Permission
	ActorTenants     []domain.TenantID
}

// RemoveSkillRegistryInput carries a removal and the caller's authority.
type RemoveSkillRegistryInput struct {
	ID               string
	Actor            string
	ActorPermissions []domain.Permission
	ActorTenants     []domain.TenantID
}

// SkillMarketplaceSearchInput carries a search.
type SkillMarketplaceSearchInput struct {
	Text              string
	RegistryID        string
	Publisher         string
	Capability        string
	IncludeDeprecated bool
	IncludeRevoked    bool
	Limit             int
	// Actor is who is looking. The QUERY is never recorded -- a search string
	// can name an internal package or a vulnerability somebody is hunting --
	// and this is used for one thing only: attributing the audit line an
	// external registry writes when it first pins a tag to a commit, or
	// notices one has moved.
	Actor        string
	ActorTenants []domain.TenantID
}

// SkillReleaseLookupInput identifies one release for display.
type SkillReleaseLookupInput struct {
	RegistryID string
	SkillID    string
	Version    string
	// Actor is who is looking, for the tag-ledger audit line only.
	Actor        string
	ActorTenants []domain.TenantID
}

// SkillInstallReleaseInput carries an install.
type SkillInstallReleaseInput struct {
	RegistryID            string
	SkillID               string
	Version               string
	AsUpdate              bool
	AllowOfflineFromCache bool
	// AcknowledgeMovedTag is a person saying explicitly that they mean to take
	// the commit a tag points at now rather than the one AO recorded. It
	// acknowledges the MOVE and skips no check.
	AcknowledgeMovedTag bool

	Actor            string
	ActorPermissions []domain.Permission
	ActorTenants     []domain.TenantID
}

// SkillRegistryProbeInput identifies one registry to act on, and the caller's
// authority. It serves both the connection test and the revocation sync: they
// take the same three things and are gated identically.
type SkillRegistryProbeInput struct {
	ID               string
	Actor            string
	ActorPermissions []domain.Permission
	ActorTenants     []domain.TenantID
}

// SkillUpdateCheckInput carries an update check.
type SkillUpdateCheckInput struct {
	Actor        string
	ActorTenants []domain.TenantID
}

// ----------------------------------------------------------------- wire types

// SkillRegistryView is one configured registry.
//
// It deliberately carries no credential, only the NAME of the sealed secret a
// registry would use — and never its value, which lives in the sealed store and
// has no business on this wire.
type SkillRegistryView struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Type        string `json:"type" enum:"local,https,git,github"`
	Location    string `json:"location"`
	Enabled     bool   `json:"enabled"`
	TrustPolicy string `json:"trustPolicy" enum:"digest,pinned_publisher,signed,official,external_integrity,external_signed,external_org_allowlist,external_deny"`
	// TrustPolicyEnforceable is false for a policy this build cannot satisfy.
	// A registry requiring a verified signature installs nothing, and a client
	// has to be able to say so rather than showing the strictest-looking
	// setting as if it were working.
	TrustPolicyEnforceable bool   `json:"trustPolicyEnforceable"`
	PinnedPublisher        string `json:"pinnedPublisher,omitempty"`
	// Owner and Repository are the external scope. Owner is the account or
	// organization; an empty Repository means every repository under it that
	// publishes AO releases, discovered under a hard request budget. Both are
	// empty for every registry that is not external.
	Owner      string `json:"owner,omitempty"`
	Repository string `json:"repository,omitempty"`
	// AllowedOwners is the external_org_allowlist control. Plain owner names,
	// never patterns. It is sent so a settings screen can SHOW it: an
	// allowlist nobody can read is one nobody reviews, and what it grants is
	// PERMISSION TO INSTALL rather than trust.
	AllowedOwners []string `json:"allowedOwners,omitempty"`
	// External reports whether this registry serves somebody else's packages.
	// It is the daemon's answer rather than a client comparing type strings,
	// because it is what decides whether the hosting notice must be shown.
	External bool `json:"external"`
	Priority int  `json:"priority"`
	// TenantID is empty for an installation-wide registry.
	TenantID string `json:"tenantId,omitempty"`
	// CredentialSecretName is the NAME of a sealed secret. The value is not
	// omitted here -- it is never loaded on this path at all.
	CredentialSecretName string `json:"credentialSecretName,omitempty"`
	// AuthType is how the credential is presented. A NAME, safe to render.
	AuthType string `json:"authType" enum:"none,bearer,api_key_header"`
	// APIKeyHeader is the header name an api_key_header registry uses.
	APIKeyHeader string `json:"apiKeyHeader,omitempty"`
	// PermittedPrivateCIDRs are the private ranges this registry -- and only
	// this registry -- may resolve into. They are sent so a settings screen can
	// SHOW them: an exception nobody can see is one nobody reviews. No
	// exception can ever re-open link-local, and therefore none can reach the
	// cloud metadata address.
	PermittedPrivateCIDRs []string `json:"permittedPrivateCidrs,omitempty"`
	// NetworkPolicySummary says the same thing in a sentence, from the daemon.
	NetworkPolicySummary string `json:"networkPolicySummary"`

	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`

	// Status is what AO last observed about this registry. It is zero-valued
	// and "never tested" for a registry nobody has tested, which is a real
	// state and not a failure.
	Status SkillRegistryStatusView `json:"status"`
}

// SkillRegistryStatusView is what the network last said about one registry.
type SkillRegistryStatusView struct {
	// LastProbeState is empty when no connection test has ever run. A client
	// must render that as "never tested" rather than as a failure.
	LastProbeState string `json:"lastProbeState" enum:",CONNECTED,AUTH_FAILED,TLS_FAILED,UNREACHABLE,INVALID_RESPONSE,POLICY_BLOCKED"`
	// LastProbeDetail names a secretRef on an auth failure and never a value.
	LastProbeDetail  string     `json:"lastProbeDetail,omitempty"`
	LastProbeAt      *time.Time `json:"lastProbeAt,omitempty"`
	LastProbeLatency int64      `json:"lastProbeLatencyMs,omitempty"`
	// LastSyncAt is the last successful metadata read. It is what an "as of"
	// on a stale listing is measured against.
	LastSyncAt *time.Time `json:"lastSyncAt,omitempty"`
	// LastRevocationSyncAt is separate: a registry can be answering metadata
	// and still not have had its withdrawal list read today.
	LastRevocationSyncAt *time.Time `json:"lastRevocationSyncAt,omitempty"`
}

// SkillRegistryProbeView is one connection test.
type SkillRegistryProbeView struct {
	RegistryID string `json:"registryId"`
	// State is one of six. CONNECTED is deliberately the hardest to reach: a
	// socket opening, TLS verifying and a 200 arriving are each necessary and
	// none is sufficient, because a load balancer, a captive portal and an
	// unrelated service on the right port all produce one.
	State string `json:"state" enum:"CONNECTED,AUTH_FAILED,TLS_FAILED,UNREACHABLE,INVALID_RESPONSE,POLICY_BLOCKED"`
	// Detail is the sentence to show. It names a secretRef on an auth failure
	// and never a credential value.
	Detail string `json:"detail"`
	// Origin is what AO actually tried to reach, so "it works in my browser"
	// and "AO cannot reach it" can be compared.
	Origin string `json:"origin,omitempty"`
	// ReportedRegistryID is what the far end called itself. Shown on a
	// mismatch, because "you have pointed this at a different registry" is the
	// most useful thing to say when it happens.
	ReportedRegistryID string    `json:"reportedRegistryId,omitempty"`
	ProtocolVersion    string    `json:"protocolVersion,omitempty"`
	AuthType           string    `json:"authType,omitempty"`
	SecretRef          string    `json:"secretRef,omitempty"`
	TestedAt           time.Time `json:"testedAt"`
	LatencyMs          int64     `json:"latencyMs"`
	// Assurance states what a connection test does and does not do. It comes
	// from the daemon so a client cannot describe it more optimistically than
	// the thing performing it.
	Assurance string `json:"assurance"`
}

// SkillRegistryRevocationView is one withdrawal AO has recorded.
type SkillRegistryRevocationView struct {
	RegistryID string `json:"registryId"`
	SkillID    string `json:"skillId"`
	Version    string `json:"version"`
	Reason     string `json:"reason"`
	// RevokedAt is when the registry says it withdrew the release; ObservedAt
	// is when AO first saw it. Two facts, and only the second is AO's.
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	ObservedAt time.Time  `json:"observedAt"`
	// Installed reports that this exact release is on this host. AO marked it
	// and blocks new installs of it; it uninstalled nothing.
	Installed bool `json:"installed"`
}

// SkillRegistryRevocationSyncView is one revocation sync.
type SkillRegistryRevocationSyncView struct {
	RegistryID    string `json:"registryId"`
	Fetched       int    `json:"fetched"`
	NewlyRecorded int    `json:"newlyRecorded"`
	// AffectedInstalls names the installed releases this sync marked revoked.
	AffectedInstalls []string `json:"affectedInstalls"`
	// Unreachable names why AO could not ask. What it already knows is
	// unchanged and stays in force.
	Unreachable string    `json:"unreachable,omitempty"`
	SyncedAt    time.Time `json:"syncedAt"`
	// Revocations is everything AO now holds for this registry.
	Revocations []SkillRegistryRevocationView `json:"revocations"`
	// Policy is the exact promise and non-promise of a revocation.
	Policy string `json:"policy"`
}

// SkillRegistryListResponse is the body of GET /api/v1/skills/registries.
type SkillRegistryListResponse struct {
	Registries []SkillRegistryView `json:"registries"`
	// TrustModel states, in words, what an install here does and does not
	// prove. It comes from the daemon so a client cannot describe the trust
	// model more optimistically than the thing enforcing it.
	TrustModel string `json:"trustModel"`
}

// SaveSkillRegistryRequest is the wire body for PUT /skills/registries/{id}.
type SaveSkillRegistryRequest struct {
	DisplayName string `json:"displayName"`
	Type        string `json:"type" enum:"local,https,git,github"`
	// Location is a directory for local, a baseURL for https, and the API BASE
	// URL for github -- https://api.github.com, or a GitHub-compatible
	// endpoint this installation was pointed at. Deliberately not a repository
	// page: a client pointed at an HTML page follows whatever it redirects to.
	Location    string `json:"location"`
	Enabled     bool   `json:"enabled"`
	TrustPolicy string `json:"trustPolicy" enum:"digest,pinned_publisher,signed,official,external_integrity,external_signed,external_org_allowlist,external_deny"`
	// PinnedPublisher is required by pinned_publisher, optional on an external
	// registry -- where it is the "this repository must keep publishing as
	// acme" control -- and refused everywhere else.
	PinnedPublisher string `json:"pinnedPublisher,omitempty"`
	// Owner is required for a github registry and refused for every other
	// type. Repository narrows it to one repository; empty means every
	// repository the owner has that publishes AO releases, under a bounded
	// scan.
	Owner      string `json:"owner,omitempty"`
	Repository string `json:"repository,omitempty"`
	// AllowedOwners is required by external_org_allowlist and refused
	// otherwise. Plain owner names: a wildcard here is how an organization
	// allowlist becomes an everything allowlist, and one is refused outright.
	AllowedOwners []string `json:"allowedOwners,omitempty"`
	Priority      int      `json:"priority,omitempty"`
	TenantID      string   `json:"tenantId,omitempty"`
	// CredentialSecretName is the NAME of a sealed secret, never a value.
	CredentialSecretName string `json:"credentialSecretName,omitempty"`
	// AuthType is how that credential is presented. Explicit rather than
	// inferred from the presence of a secret: "AO guessed bearer and the
	// registry wanted a header" is a 401 nobody can debug from a settings
	// screen.
	AuthType string `json:"authType,omitempty" enum:"none,bearer,api_key_header"`
	// APIKeyHeader is the header name for api_key_header. A name, never a
	// value; Authorization, Cookie, Proxy-Authorization and Host are refused.
	APIKeyHeader string `json:"apiKeyHeader,omitempty"`
	// PermittedPrivateCIDRs re-opens private ranges for THIS registry only. It
	// exists because refusing every private address is right by default and
	// wrong for an installation whose registry genuinely lives at 10.x. No
	// entry may overlap link-local, so none can reach cloud metadata.
	PermittedPrivateCIDRs []string `json:"permittedPrivateCidrs,omitempty"`
}

// SkillReleaseModeView is one execution mode a release advertises.
type SkillReleaseModeView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	RiskLevel    string   `json:"riskLevel" enum:"low,medium,high,critical"`
	Capabilities []string `json:"capabilities"`
}

// SkillReleaseView is one release a registry offers, with what this
// installation already knows about it.
type SkillReleaseView struct {
	RegistryID   string `json:"registryId"`
	RegistryName string `json:"registryName"`
	SkillID      string `json:"skillId"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Publisher    string `json:"publisher"`
	Description  string `json:"description"`
	RiskLevel    string `json:"riskLevel" enum:"low,medium,high,critical"`
	SourceURL    string `json:"sourceUrl,omitempty"`
	ChangelogURL string `json:"changelogUrl,omitempty"`
	Changelog    string `json:"changelog,omitempty"`

	ManifestDigest string `json:"manifestDigest"`
	ArtifactDigest string `json:"artifactDigest"`

	// SignatureFormat, KeyID and AttestationURL are what the release CLAIMS in
	// the pre-signature vocabulary: free-form strings from some other
	// ecosystem that AO records and never checks. They are NOT the AO
	// signature -- that is Signed/SignatureKeyID below, and it is the one AO
	// verifies.
	SignatureFormat string `json:"signatureFormat,omitempty"`
	KeyID           string `json:"keyId,omitempty"`
	AttestationURL  string `json:"attestationUrl,omitempty"`

	// Signed reports that the release carries AO signature material. It says
	// NOTHING about whether that material verifies: a listing has checked no
	// signature, exactly as it has hashed no bytes, and a client that rendered
	// "signed" as an assurance would be describing a check nobody ran.
	Signed bool `json:"signed"`
	// SignatureKeyID and SignatureScheme are what the release names, for a
	// person who wants to see which key claims it before installing.
	SignatureKeyID  string `json:"signatureKeyId,omitempty"`
	SignatureScheme string `json:"signatureScheme,omitempty"`

	RequestedCapabilities []string               `json:"requestedCapabilities"`
	ExecutionModes        []SkillReleaseModeView `json:"executionModes"`

	AOMinVersion string `json:"aoMinVersion"`
	AOMaxVersion string `json:"aoMaxVersion,omitempty"`
	// Compatibility is the daemon's verdict against the RUNNING build.
	// "unknown" means the check could not run (a source build reporting an
	// unparseable version) and must not be rendered as a pass.
	Compatibility string `json:"compatibility" enum:"compatible,ao-too-old,ao-too-new,unknown"`

	PublishedAt      time.Time `json:"publishedAt"`
	Deprecated       bool      `json:"deprecated,omitempty"`
	DeprecationNote  string    `json:"deprecationNote,omitempty"`
	Revoked          bool      `json:"revoked,omitempty"`
	RevocationReason string    `json:"revocationReason,omitempty"`

	// The git source, when this release came from a forge. Every field is what
	// AO RESOLVED, never what a descriptor claimed: the provider re-stamps the
	// owner, the repository and the commit from its own lookup, so a
	// repository cannot publish a descriptor claiming to be somebody else's
	// code at somebody else's commit.
	SourceProvider   string `json:"sourceProvider,omitempty" enum:",github"`
	SourceOwner      string `json:"sourceOwner,omitempty"`
	SourceRepository string `json:"sourceRepository,omitempty"`
	// SourceTag is a LABEL and never the identity. SourceCommit is the
	// identity: a full 40-character SHA. ShortCommit is the first twelve, for
	// a place the full one does not fit -- twelve and not seven, because seven
	// collides in a large repository and a display abbreviation that can
	// collide is one somebody eventually compares two of.
	SourceTag         string `json:"sourceTag,omitempty"`
	SourceCommit      string `json:"sourceCommit,omitempty"`
	SourceShortCommit string `json:"sourceShortCommit,omitempty"`
	SourcePath        string `json:"sourcePath,omitempty"`
	// SourceVisibility is what the forge said about the repository. Display
	// metadata and a warning surface, never a trust input.
	SourceVisibility string `json:"sourceVisibility,omitempty" enum:",public,private"`
	// TagMoved reports that the tag this release is published under points
	// somewhere else than AO recorded. It travels BESIDE trust and into none
	// of it: a moved tag does not make a release less verified, it makes the
	// name it was found under mean something else.
	TagMoved            bool       `json:"tagMoved,omitempty"`
	TagMovedFromCommit  string     `json:"tagMovedFromCommit,omitempty"`
	TagMovedAt          *time.Time `json:"tagMovedAt,omitempty"`
	TagMovedExplanation string     `json:"tagMovedExplanation,omitempty"`
	// ExternalRevoked is an administrative withdrawal made HERE, of this
	// release's account, repository or commit. It is separate from Revoked,
	// which is what a registry said, because only one of the two is this
	// installation's own decision.
	ExternalRevoked       bool   `json:"externalRevoked,omitempty"`
	ExternalRevokedReason string `json:"externalRevokedReason,omitempty"`

	// Trust is what AO can honestly say about this release right now. In a
	// listing it is "unverified" for everything installable, because a search
	// fetches nothing and therefore verified nothing. It only becomes
	// "verified" once AO has hashed the bytes at install.
	Trust string `json:"trust" enum:"revoked,unverified,verified,trusted"`
	// TrustExplanation says what Trust means in words, from the daemon.
	TrustExplanation string `json:"trustExplanation"`

	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	UpdateAvailable  bool   `json:"updateAvailable"`

	// MetadataFreshness is how AO came by this row: "live" means the registry
	// answered during this request, and the other three all mean it did not.
	// It is sent per release because a search spans registries and one being
	// offline must not colour the rows that came from the ones that answered.
	//
	// It says nothing about Trust and nothing about Compatibility. A cached
	// row is not less trusted and not less compatible; it is less CURRENT.
	MetadataFreshness string `json:"metadataFreshness" enum:"live,cached,stale,offline"`
	// MetadataOffline reports that the registry could not be reached for this
	// row. It is a separate field from the freshness, and never inferred from
	// it, so a client renders the outage rather than deducing it.
	MetadataOffline bool `json:"metadataOffline"`
	// MetadataAsOf is when the registry actually answered. For anything other
	// than "live" it is in the past, and it is what a client shows instead of
	// presenting the row as current.
	MetadataAsOf time.Time `json:"metadataAsOf,omitempty"`
}

// SkillRegistryNoteView names a registry the search could not read. It exists
// so an unreadable registry cannot disappear into an empty result: "this
// registry has nothing" and "AO could not read this registry" are different
// answers, and only one means somebody should go and look.
type SkillRegistryNoteView struct {
	RegistryID string `json:"registryId"`
	Reason     string `json:"reason"`
	// MetadataFreshness and MetadataOffline say how far AO got. A registry
	// that was UNREACHABLE with nothing cached reads offline; one whose
	// configuration could not be opened does not, because no network was
	// involved and the two want different fixes.
	MetadataFreshness string `json:"metadataFreshness" enum:"live,cached,stale,offline"`
	MetadataOffline   bool   `json:"metadataOffline"`
}

// SkillRegistryFreshnessView is one registry the read consulted and how it
// answered.
//
// It is sent for EVERY consulted registry, answered or not, so that "is any of
// this cached" is a field a client reads rather than something it deduces from
// an empty note list. Deducing it is what let a stale answer pass for a live
// one.
type SkillRegistryFreshnessView struct {
	RegistryID   string `json:"registryId"`
	RegistryName string `json:"registryName,omitempty"`
	// Freshness is one of live, cached, stale, offline. Only "live" means the
	// registry confirmed this during the request.
	Freshness string `json:"freshness" enum:"live,cached,stale,offline"`
	// Offline reports that the registry could not be reached.
	Offline bool `json:"offline"`
	// FetchedAt is when the registry last actually answered. Zero means never.
	FetchedAt time.Time `json:"fetchedAt,omitempty"`
	// Explanation says in words what the state means, from the daemon, for the
	// same reason TrustExplanation is served: a screen that wrote its own
	// version of these sentences would eventually write a more comfortable
	// one, and the most comfortable available lie here is "up to date".
	Explanation string `json:"explanation"`
}

// SkillMarketplaceSearchResponse is the body of GET /api/v1/skills/marketplace.
type SkillMarketplaceSearchResponse struct {
	Releases []SkillReleaseView      `json:"releases"`
	Notes    []SkillRegistryNoteView `json:"notes"`
	// Sources is every registry consulted and how fresh what it gave is.
	Sources []SkillRegistryFreshnessView `json:"sources"`
	// Offline is true when ANY consulted registry could not be reached, so
	// results on screen include cached metadata. It is computed by the daemon
	// and sent, rather than each client folding over sources and reaching its
	// own conclusion.
	Offline bool `json:"offline"`
	// FreshnessNotice is the sentence a client must show when Offline is true.
	FreshnessNotice string `json:"freshnessNotice,omitempty"`
	// ExternalNotice is the sentence a client must show before installing from
	// a forge. It is served so a screen cannot describe hosting more warmly
	// than the daemon does, and the warm version of this one -- "from GitHub"
	// -- reads as an endorsement to almost everybody.
	ExternalNotice string `json:"externalNotice"`
	// InstallNotice is the sentence a client must show before installing. It
	// comes from the daemon so the promise on screen and the behaviour in the
	// service cannot drift apart.
	InstallNotice string `json:"installNotice"`
}

// SkillReleaseDetailResponse is one release and every version of it.
type SkillReleaseDetailResponse struct {
	Release  SkillReleaseView   `json:"release"`
	Versions []SkillReleaseView `json:"versions"`
	// Source is the registry this was read from and how fresh the read is.
	Source SkillRegistryFreshnessView `json:"source"`
	// Offline is true when the registry could not be reached and this detail
	// came from cache.
	Offline bool `json:"offline"`
	// FreshnessNotice is the sentence a client must show when Offline is true.
	FreshnessNotice string `json:"freshnessNotice,omitempty"`
	// InstalledNotice is what a client must show AFTER installing.
	InstallNotice string `json:"installNotice"`
}

// InstallSkillReleaseRequest is the wire body for a marketplace install.
type InstallSkillReleaseRequest struct {
	RegistryID string `json:"registryId"`
	SkillID    string `json:"skillId"`
	// Version is required and exact. There is no "latest" install target.
	Version string `json:"version"`
	// AsUpdate refuses anything that is not strictly newer than the installed
	// version, so a person clicking Update cannot be handed an older release.
	AsUpdate bool `json:"asUpdate,omitempty"`
	// AllowOfflineFromCache permits the install to proceed when the registry
	// cannot be reached, using bytes AO already fetched and verified. It
	// weakens no check: the cached tree is re-hashed, the manifest is re-read,
	// and a release AO has never downloaded cannot be installed this way at
	// all.
	AllowOfflineFromCache bool `json:"allowOfflineFromCache,omitempty"`
	// AcknowledgeMovedTag is a person saying, explicitly, that they mean to
	// take the commit a tag points at NOW rather than the one AO recorded for
	// it.
	//
	// It exists because a moved tag is not always an attack -- publishers
	// re-tag by mistake -- and a system that refused forever is one people
	// route around. What it must never be is the default: a silent reinstall
	// of "the same version" from different bytes is the substitution this
	// whole surface exists to catch. It acknowledges the MOVE and skips no
	// check: the digests, the manifest, the capabilities, the signature and
	// the revocations all still run, against the new commit.
	AcknowledgeMovedTag bool `json:"acknowledgeMovedTag,omitempty"`
}

// SkillInstallOriginView is the recorded provenance of one installed version.
type SkillInstallOriginView struct {
	SkillID        string `json:"skillId"`
	Version        string `json:"version"`
	RegistryID     string `json:"registryId"`
	RegistryName   string `json:"registryName,omitempty"`
	RegistryType   string `json:"registryType"`
	Publisher      string `json:"publisher"`
	SourceURL      string `json:"sourceUrl,omitempty"`
	ManifestDigest string `json:"manifestDigest"`
	ArtifactDigest string `json:"artifactDigest"`
	Trust          string `json:"trust" enum:"revoked,unverified,verified,trusted"`
	// The git provenance, written once at install and never rewritten. If the
	// tag later points elsewhere these fields still say what they said: the
	// bytes on this host came from this commit, and AO verified them against
	// this commit's digests. That is true forever, and rewriting it because
	// somebody moved a pointer would erase the only evidence of what was
	// actually installed.
	SourceProvider    string     `json:"sourceProvider,omitempty" enum:",github"`
	SourceOwner       string     `json:"sourceOwner,omitempty"`
	SourceRepository  string     `json:"sourceRepository,omitempty"`
	SourceTag         string     `json:"sourceTag,omitempty"`
	SourceCommit      string     `json:"sourceCommit,omitempty"`
	SourceShortCommit string     `json:"sourceShortCommit,omitempty"`
	SourcePath        string     `json:"sourcePath,omitempty"`
	SourceVisibility  string     `json:"sourceVisibility,omitempty" enum:",public,private"`
	SourceFetchedAt   *time.Time `json:"sourceFetchedAt,omitempty"`
	// IdentityExplanation is the daemon's sentence about WHO this release is
	// by, kept as the several separate facts it actually is: the host account,
	// the publisher the package declares, and whether anything cryptographic
	// ties that name to whoever wrote the code. Empty for a non-external
	// install.
	//
	// It is served rather than composed in a UI because the comfortable
	// version -- "published by acme" -- is exactly the impersonation this
	// phase exists to refuse.
	IdentityExplanation string    `json:"identityExplanation,omitempty"`
	TrustExplanation    string    `json:"trustExplanation"`
	TrustPolicy         string    `json:"trustPolicy"`
	Compatibility       string    `json:"compatibility" enum:"compatible,ao-too-old,ao-too-new,unknown"`
	InstalledAt         time.Time `json:"installedAt"`
	InstalledBy         string    `json:"installedBy,omitempty"`

	// Provenance is the verified chain, present when AO checked a signature.
	// It is the answer to "why does this say trusted", and it is served as one
	// object rather than as loose fields so a client cannot render half a
	// chain as a whole one.
	Provenance *SkillProvenanceView `json:"provenance,omitempty"`
	// RevocationStateObserved is what the revocation picture looked like at
	// install time. "not-checked" and "none-known" are deliberately different
	// sentences: a reader must be able to tell "AO asked and nothing was
	// withdrawn" from "AO could not ask".
	RevocationStateObserved string `json:"revocationStateObserved,omitempty"`
	// MetadataAsOf is when the metadata this install acted on was fetched.
	MetadataAsOf time.Time `json:"metadataAsOf,omitempty"`
	// Revoked reports a revocation AO OBSERVED after the install. AO did not
	// uninstall it and did not disable it on any project.
	Revoked          bool   `json:"revoked,omitempty"`
	RevocationReason string `json:"revocationReason,omitempty"`
}

// SkillProvenanceView is the verified signature chain behind one install.
//
// Every value here is PUBLIC: a key id, a fingerprint derived from public key
// bytes, a trust root id and a verdict. There is no field a private key or a
// credential could occupy, and the signed payload itself is deliberately
// absent -- it is reconstructible from the release, and a second copy would be
// a second thing that can disagree with the first.
type SkillProvenanceView struct {
	// Verified is the verdict. When it is false the fields below record how
	// far AO got before it refused, which is more useful than an empty object.
	Verified bool `json:"verified"`

	Scheme    string `json:"scheme,omitempty"`
	Algorithm string `json:"algorithm,omitempty"`

	KeyID string `json:"keyId,omitempty"`
	// KeyFingerprint is sha256 over the raw public key, lowercase hex. It is
	// what a person compares against a fingerprint the publisher published
	// somewhere else -- which is the only way a first key is ever verified.
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
	// KeyFingerprintShort is the same value grouped for a screen. The full
	// value is always beside it: a truncated fingerprint is for RECOGNISING a
	// key you already know, never for deciding two keys are the same one.
	KeyFingerprintShort string `json:"keyFingerprintShort,omitempty"`
	// KeyOrigin is how this key reached AO's trust store: "certificate" means
	// a root signed a statement authorizing it, "administrative" means
	// somebody here vouched for it, "built-in" means it arrived with the AO
	// build. Three different assurances, and a screen that rendered them
	// identically would be overstating one.
	KeyOrigin string `json:"keyOrigin,omitempty" enum:"certificate,administrative,built-in"`

	TrustRootID   string `json:"trustRootId,omitempty"`
	TrustRootName string `json:"trustRootName,omitempty"`
	TrustRootTier string `json:"trustRootTier,omitempty" enum:"official,enterprise,external"`

	Publisher string `json:"publisher,omitempty"`

	// SignedAt is the publisher's claim, covered by the signature. VerifiedAt
	// is AO's own clock at the moment it checked. Two facts, and only the
	// second one is AO's.
	SignedAt   time.Time `json:"signedAt,omitzero"`
	VerifiedAt time.Time `json:"verifiedAt,omitzero"`

	// RefusalCode and Refusal are present when AO checked and said no. They
	// are recorded rather than dropped, because "AO checked and refused" and
	// "AO never checked" are different facts about an installed package.
	RefusalCode string `json:"refusalCode,omitempty"`
	Refusal     string `json:"refusal,omitempty"`
}

// SkillInstallOutcomeView is one completed marketplace install.
type SkillInstallOutcomeView struct {
	Install SkillInstallView       `json:"install"`
	Origin  SkillInstallOriginView `json:"origin"`
	Updated bool                   `json:"updated"`
	// FromCache reports that the bytes came from AO's verified artifact cache
	// rather than the network. They were verified identically; the distinction
	// is here because "where did these bytes come from" is what an incident
	// review asks.
	FromCache bool `json:"fromCache,omitempty"`
	// Offline reports that the registry could not be reached and the install
	// proceeded on evidence AO already held.
	Offline bool `json:"offline,omitempty"`
	// NextStep is the sentence a client shows after installing. Installing
	// reaches no project, and the response says so rather than leaving the UI
	// to imply otherwise.
	NextStep string `json:"nextStep"`
}

// SkillUpdateStatusView is what a check found about one installed release.
type SkillUpdateStatusView struct {
	SkillID         string                 `json:"skillId"`
	Version         string                 `json:"version"`
	Origin          SkillInstallOriginView `json:"origin"`
	LatestVersion   string                 `json:"latestVersion,omitempty"`
	UpdateAvailable bool                   `json:"updateAvailable"`
	// RevokedNow reports that the registry has withdrawn this exact installed
	// release. AO marked it and blocks new installs; it removed nothing.
	RevokedNow       bool   `json:"revokedNow"`
	RevocationReason string `json:"revocationReason,omitempty"`
	// Unreachable names why AO could not ask. Provenance is unaffected.
	Unreachable string `json:"unreachable,omitempty"`
}

// SkillUpdateCheckResponse is the body of GET /api/v1/skills/updates.
type SkillUpdateCheckResponse struct {
	Statuses []SkillUpdateStatusView `json:"statuses"`
	// RevocationPolicy states the exact promise and non-promise of a
	// revocation AO observed after an install.
	RevocationPolicy string `json:"revocationPolicy"`
}

// SkillRegistryParams identify one configured registry.
type SkillRegistryParams struct {
	RegistryID string `path:"registryId" description:"Registry identifier (kebab-case)."`
}

// SkillReleaseParams identify one release in one registry.
type SkillReleaseParams struct {
	RegistryID string `path:"registryId" description:"Registry identifier (kebab-case)."`
	SkillID    string `path:"skillId" description:"Skill identifier (kebab-case)."`
}

// SkillMarketplaceQuery is the search's query string. It is exported so the
// spec generator and the handler read one declaration.
type SkillMarketplaceQuery struct {
	Q                 *string `query:"q,omitempty" description:"Free text matched against a release's id, name, description and publisher."`
	RegistryID        *string `query:"registryId,omitempty" description:"Search only this registry. Omit to search every enabled registry you can see."`
	Publisher         *string `query:"publisher,omitempty" description:"Exact publisher match."`
	Capability        *string `query:"capability,omitempty" description:"Keep only releases that request this AO capability."`
	IncludeDeprecated *bool   `query:"includeDeprecated,omitempty" description:"Include releases the publisher superseded. Off by default, so the ordinary listing is the installable one."`
	IncludeRevoked    *bool   `query:"includeRevoked,omitempty" description:"Include withdrawn releases. They can never be installed; showing them answers \"where did that version go\"."`
	Limit             *int64  `query:"limit,omitempty" minimum:"1" maximum:"500" description:"Maximum releases to return. Defaults to 100."`
}

// SkillReleaseQuery selects which version a release detail features.
type SkillReleaseQuery struct {
	Version *string `query:"version,omitempty" description:"Which version to feature. Omit for the newest the registry offers."`
}

// ------------------------------------------------------------------- handlers

// registerMarketplaceRoutes mounts the registry and marketplace surface. Same
// /skills family and therefore the same gate: settings.read to look,
// settings.manage to change.
func (c *SkillsController) registerMarketplaceRoutes(r chi.Router) {
	r.Get("/skills/registries", c.listRegistries)
	r.Put("/skills/registries/{registryId}", c.saveRegistry)
	r.Delete("/skills/registries/{registryId}", c.removeRegistry)
	// Both are POST rather than GET despite reading nothing of AO's: each
	// makes the daemon open a connection and present a stored credential, and
	// a GET that reaches the network is a GET something will eventually
	// prefetch.
	r.Post("/skills/registries/{registryId}/test", c.testRegistry)
	r.Post("/skills/registries/{registryId}/revocations/sync", c.syncRegistryRevocations)

	r.Get("/skills/marketplace", c.searchMarketplace)
	r.Get("/skills/marketplace/{registryId}/{skillId}", c.marketplaceRelease)
	r.Post("/skills/marketplace/install", c.installRelease)

	r.Get("/skills/updates", c.checkUpdates)
}

func (c *SkillsController) listRegistries(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/registries")
		return
	}
	views, err := c.Marketplace.ListRegistryViews(r.Context(), c.callerTenants(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, SkillRegistryListResponse{
		Registries: views,
		TrustModel: RegistryTrustModelStatement,
	})
}

func (c *SkillsController) saveRegistry(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodPut, "/api/v1/skills/registries/{registryId}")
		return
	}
	var in SaveSkillRegistryRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	view, err := c.Marketplace.SaveRegistryView(r.Context(), SaveSkillRegistryInput{
		ID:                    chi.URLParam(r, "registryId"),
		DisplayName:           in.DisplayName,
		Type:                  in.Type,
		Location:              in.Location,
		Enabled:               in.Enabled,
		TrustPolicy:           in.TrustPolicy,
		PinnedPublisher:       in.PinnedPublisher,
		Priority:              in.Priority,
		TenantID:              in.TenantID,
		CredentialSecretName:  in.CredentialSecretName,
		AuthType:              in.AuthType,
		APIKeyHeader:          in.APIKeyHeader,
		PermittedPrivateCIDRs: in.PermittedPrivateCIDRs,
		Owner:                 in.Owner,
		Repository:            in.Repository,
		AllowedOwners:         in.AllowedOwners,
		Actor:                 c.actor(r),
		ActorPermissions:      c.callerGlobalPermissions(r),
		ActorTenants:          c.callerTenants(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, view)
}

func (c *SkillsController) removeRegistry(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/skills/registries/{registryId}")
		return
	}
	if err := c.Marketplace.RemoveRegistryView(r.Context(), RemoveSkillRegistryInput{
		ID:               chi.URLParam(r, "registryId"),
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
		ActorTenants:     c.callerTenants(r),
	}); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (c *SkillsController) searchMarketplace(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/marketplace")
		return
	}
	q := r.URL.Query()
	res, err := c.Marketplace.SearchMarketplace(r.Context(), SkillMarketplaceSearchInput{
		Text:              strings.TrimSpace(q.Get("q")),
		RegistryID:        strings.TrimSpace(q.Get("registryId")),
		Publisher:         strings.TrimSpace(q.Get("publisher")),
		Capability:        strings.TrimSpace(q.Get("capability")),
		IncludeDeprecated: queryBool(q.Get("includeDeprecated")),
		IncludeRevoked:    queryBool(q.Get("includeRevoked")),
		Limit:             queryInt(q.Get("limit")),
		Actor:             c.actor(r),
		ActorTenants:      c.callerTenants(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, res)
}

func (c *SkillsController) marketplaceRelease(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/marketplace/{registryId}/{skillId}")
		return
	}
	res, err := c.Marketplace.GetMarketplaceRelease(r.Context(), SkillReleaseLookupInput{
		RegistryID:   chi.URLParam(r, "registryId"),
		SkillID:      chi.URLParam(r, "skillId"),
		Version:      strings.TrimSpace(r.URL.Query().Get("version")),
		Actor:        c.actor(r),
		ActorTenants: c.callerTenants(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, res)
}

func (c *SkillsController) installRelease(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/skills/marketplace/install")
		return
	}
	var in InstallSkillReleaseRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	view, err := c.Marketplace.InstallMarketplaceRelease(r.Context(), SkillInstallReleaseInput{
		RegistryID:            in.RegistryID,
		SkillID:               in.SkillID,
		Version:               in.Version,
		AsUpdate:              in.AsUpdate,
		AllowOfflineFromCache: in.AllowOfflineFromCache,
		AcknowledgeMovedTag:   in.AcknowledgeMovedTag,
		Actor:                 c.actor(r),
		ActorPermissions:      c.callerGlobalPermissions(r),
		ActorTenants:          c.callerTenants(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, view)
}

// testRegistry runs one connection test. It reads one small metadata endpoint
// and does nothing else -- no download, no catalog change, no install, no
// enable, no image approval -- and the response says so in a sentence from the
// daemon.
func (c *SkillsController) testRegistry(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/skills/registries/{registryId}/test")
		return
	}
	view, err := c.Marketplace.TestSkillRegistryConnection(r.Context(), SkillRegistryProbeInput{
		ID:               chi.URLParam(r, "registryId"),
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
		ActorTenants:     c.callerTenants(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	// 200 whatever the verdict. A failed connection test is a successful
	// answer to the question that was asked, and a 502 here would make a
	// client unable to tell "the registry is down" from "AO could not run the
	// test".
	envelope.WriteJSON(w, http.StatusOK, view)
}

// syncRegistryRevocations asks one registry what it has withdrawn.
//
// It marks, it blocks new installs, and it puts the sentence on screen. It
// never uninstalls, never deletes files and never disables a skill on a
// project: deciding what to do about an installed release that was withdrawn is
// a human's call, one package at a time.
func (c *SkillsController) syncRegistryRevocations(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodPost,
			"/api/v1/skills/registries/{registryId}/revocations/sync")
		return
	}
	view, err := c.Marketplace.SyncSkillRegistryRevocations(r.Context(), SkillRegistryProbeInput{
		ID:               chi.URLParam(r, "registryId"),
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
		ActorTenants:     c.callerTenants(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, view)
}

func (c *SkillsController) checkUpdates(w http.ResponseWriter, r *http.Request) {
	if c.Marketplace == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/updates")
		return
	}
	res, err := c.Marketplace.CheckMarketplaceUpdates(r.Context(), SkillUpdateCheckInput{
		Actor:        c.actor(r),
		ActorTenants: c.callerTenants(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, res)
}

// callerTenants resolves which organizations this caller belongs to.
//
// It follows callerGlobalPermissions exactly, for the reason recorded there. A
// disabled guard is the default single-user desktop on the unauthenticated
// loopback listener, where every tenant on the installation is the caller's;
// returning nothing there would hide a tenant-scoped registry from the only
// person who could have created it. When the guard IS enabled, an unresolvable
// subject yields nothing, which is the fail-closed answer for a multi-user
// install: no memberships means installation-wide registries only.
//
// A nil Tenancy port yields nothing, which has the same effect. That is
// deliberate: an installation with no tenancy wiring should see the
// installation-wide registries and no private ones, not every private one.
func (c *SkillsController) callerTenants(r *http.Request) []domain.TenantID {
	if c.Tenancy == nil {
		return nil
	}
	if !c.Guard.Enabled() {
		tenants, err := c.Tenancy.ListTenants(r.Context())
		if err != nil {
			return nil
		}
		out := make([]domain.TenantID, 0, len(tenants))
		for _, t := range tenants {
			out = append(out, t.ID)
		}
		return out
	}
	p, err := identity.RequirePrincipal(r)
	if err != nil {
		return nil
	}
	memberships, err := c.Tenancy.ListActiveTenantMembershipsForUser(r.Context(), p.User.ID)
	if err != nil {
		return nil
	}
	out := make([]domain.TenantID, 0, len(memberships))
	for _, m := range memberships {
		out = append(out, m.TenantID)
	}
	return out
}

func queryBool(raw string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	return err == nil && v
}

func queryInt(raw string) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return v
}

// RegistryTrustModelStatement is the trust model in words, served from the
// daemon so a client cannot describe it more optimistically than the thing
// enforcing it.
//
// It is a constant here and not in the UI for the same reason
// trustModelStatement is: a screen that wrote its own version of this sentence
// would eventually write a nicer one.
const RegistryTrustModelStatement = "Installing verifies INTEGRITY: AO fetches the package, computes both digests " +
	"itself, and refuses anything that is not byte-for-byte the release it resolved. That is \"verified\". " +
	"\"Trusted\" is verified PLUS a signature over those exact bytes, made by a key that chains to a trust root " +
	"this installation configured and held by the publisher the release names -- so trusted says who signed it, " +
	"never that the code is safe, that it has no vulnerabilities, that its capabilities are benign or that " +
	"anybody reviewed it. Installing enables the skill on no project, grants no capability and approves no " +
	"container image."

// RegistryProbeAssurance states exactly what a connection test does, served
// from the daemon so a client cannot describe it more generously than the thing
// performing it.
//
// The second sentence is the one that matters. A green badge that appeared
// whenever a socket opened would be worse than no badge, because people act on
// it.
const RegistryProbeAssurance = "A connection test reads one small metadata endpoint. It downloads no package, " +
	"changes no catalog, installs nothing, enables nothing on any project and approves no container image. " +
	"CONNECTED means this registry answered AS ITSELF over verified TLS, speaking a protocol version this " +
	"build knows -- not merely that something answered on that address."

// RegistryMetadataFreshnessNotice is what a client must show when a listing
// includes metadata AO could not confirm with the registry during the request.
//
// It is served for the same reason RegistryProbeAssurance is: the sentence and
// the behaviour must not drift, and the comfortable version of this sentence
// ("everything looks fine") is exactly the one a screen would write for itself.
//
// The second half is the part that matters. Installing does NOT run off this
// listing: it re-resolves the exact release from the registry, and an install
// that cannot reach the registry is refused unless a person explicitly asks
// for the cached-bytes path, which re-hashes everything and re-checks the
// recorded revocations.
const RegistryMetadataFreshnessNotice = "A registry could not be reached, so some of what is listed is metadata AO " +
	"cached earlier and could not confirm just now. It is shown with the time it was fetched and is NOT current: a " +
	"release withdrawn since then would still appear here. Installing is unaffected -- an install re-resolves the " +
	"exact release from the registry and is refused when it cannot, so nothing is ever installed on the strength " +
	"of this listing."

// RegistryInstallNotice is what a client must show BEFORE installing.
const RegistryInstallNotice = "This installs the Skill but does not enable it on any project."

// RegistryInstalledNotice is what a client must show AFTER installing.
const RegistryInstalledNotice = "Installed. Choose a project to enable it."

// RegistryRevocationPolicy is the exact promise and non-promise of a revocation
// AO observed after an install.
const RegistryRevocationPolicy = "A release the registry has since revoked is marked here and can no longer be " +
	"installed. AO does NOT uninstall it, does NOT disable it on any project, and does NOT stop a run already " +
	"under way. Deciding what to do about an installed release that was withdrawn is a human's call, one package " +
	"at a time."
