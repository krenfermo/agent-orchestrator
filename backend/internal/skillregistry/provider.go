package skillregistry

import (
	"context"
	"errors"
	"strings"
)

// Errors a provider returns. They are sentinels so the service can map a
// missing release onto a 404 without matching on message text.
var (
	// ErrNoSuchSkill means the registry offers no releases at all under that id.
	ErrNoSuchSkill = errors.New("skillregistry: no such skill in this registry")
	// ErrNoSuchRelease means the id exists but not at that version.
	ErrNoSuchRelease = errors.New("skillregistry: no such release in this registry")
	// ErrRegistryUnreadable means the registry itself could not be read -- a
	// missing directory, a malformed index. It is distinct from "nothing
	// matched", because an unreadable registry answering a search with an
	// empty list would read as "this registry has nothing", which is a
	// materially different and much more comfortable fact than the truth.
	ErrRegistryUnreadable = errors.New("skillregistry: registry is unreadable")

	// The four below split "unreadable" for a registry AO reaches over the
	// network, because an operator staring at a red row needs to know which
	// thing to go and fix, and because three of them mean something different
	// about whether the registry can be TRUSTED at all.

	// ErrRegistryUnreachable means the network never carried a response --
	// DNS did not answer, the connection was refused, the request timed out.
	// It says nothing about the registry itself.
	ErrRegistryUnreachable = errors.New("skillregistry: registry is unreachable")
	// ErrRegistryTLS means the TLS handshake failed verification. It is
	// deliberately NOT merged into "unreachable": an unreachable registry is
	// an operations problem, and a registry whose certificate does not verify
	// is either a misconfiguration or somebody in the middle.
	ErrRegistryTLS = errors.New("skillregistry: registry TLS verification failed")
	// ErrRegistryAuth means the registry rejected the credential (401/403).
	// The error names the secretRef and never the value.
	ErrRegistryAuth = errors.New("skillregistry: registry refused the credential")
	// ErrRegistryResponse means the registry answered with something this
	// build cannot parse or will not accept -- a wrong content type, an
	// unsupported apiVersion, a body over the size limit. It is separate from
	// unreadable because the registry IS answering; what it says is the
	// problem.
	ErrRegistryResponse = errors.New("skillregistry: registry returned an invalid response")
)

// Query is one search. Every field narrows; an empty Query lists everything
// the registry offers that is neither deprecated nor revoked.
type Query struct {
	// Text matches the skill id, name and description, case-insensitively.
	Text string
	// Publisher, when set, must match exactly.
	Publisher string
	// Capability, when set, keeps only releases that request it. It is the
	// filter that answers "what wants my repository" and "what wants the
	// network", which is the question worth filtering on.
	Capability string
	// IncludeDeprecated and IncludeRevoked widen the result. Both default to
	// false so the ordinary listing is the installable one, and both exist so
	// that "why can I not find the version I had" has an answer on screen.
	IncludeDeprecated bool
	IncludeRevoked    bool
	// Limit caps the result. Zero means the provider's own default.
	Limit int
}

// Normalized returns the query with its text fields trimmed and lowercased
// where matching is case-insensitive. Providers call it so two providers do
// not disagree about whether " Security " matches "security".
func (q Query) Normalized() Query {
	q.Text = strings.ToLower(strings.TrimSpace(q.Text))
	q.Publisher = strings.TrimSpace(q.Publisher)
	q.Capability = strings.TrimSpace(q.Capability)
	return q
}

// DefaultSearchLimit caps a search that asked for no limit.
const DefaultSearchLimit = 100

// MaxSearchLimit caps a search that asked for too many.
const MaxSearchLimit = 500

// EffectiveLimit is the limit a provider should apply.
func (q Query) EffectiveLimit() int {
	switch {
	case q.Limit <= 0:
		return DefaultSearchLimit
	case q.Limit > MaxSearchLimit:
		return MaxSearchLimit
	}
	return q.Limit
}

// Provider is one registry AO can read.
//
// # The split that matters
//
// Four of the five methods return METADATA and cannot move a byte of package
// content. FetchArtifact is the fifth, and it is separate precisely so that
// "do not download code during a search" is a property of the interface rather
// than a rule somebody has to remember. A provider implementation that fetched
// during Search would have nowhere to put the bytes.
//
// # Get versus ResolveExactRelease
//
// Get is the DISPLAY read: it may answer from an index the provider already
// holds, and it is what a marketplace listing calls.
//
// ResolveExactRelease is the PRE-INSTALL read: it re-reads from the
// authoritative source, refuses anything that is not one exact immutable
// version, and is the only result an install may act on. Two methods rather
// than a flag, because a single method with a "please be careful" parameter is
// a method somebody eventually calls without it.
//
// # What an implementation must not do
//
// Execute anything, follow a symlink out of its own root, or return a release
// whose RegistryID is anything but its own. The service sets RegistryID after
// every call for exactly that reason.
type Provider interface {
	// RegistryID is the configured id this provider serves.
	RegistryID() string

	// Search returns releases matching q, ordered by SortReleases and capped
	// by q.EffectiveLimit. It moves no package bytes.
	Search(ctx context.Context, q Query) ([]Release, error)

	// Get returns one release for display. It may be served from an index.
	Get(ctx context.Context, skillID, version string) (Release, error)

	// ListVersions returns every release of one skill, newest first,
	// INCLUDING deprecated and revoked ones: the version list is where a
	// person goes to find out that the version they wanted was withdrawn.
	ListVersions(ctx context.Context, skillID string) ([]Release, error)

	// ResolveExactRelease re-reads one exact release from the authoritative
	// source. version must be a complete MAJOR.MINOR.PATCH[-prerelease]; a
	// range, a tag, an alias or an empty string is an error, never a guess.
	ResolveExactRelease(ctx context.Context, skillID, version string) (Release, error)

	// FetchArtifact materializes rel's package tree into destDir, which the
	// caller owns and has already created empty.
	//
	// It is the only method that moves package content. An implementation must
	// write nothing outside destDir and must refuse any entry that is not a
	// regular file, so that a symlink in a registry never becomes a symlink in
	// AO's catalog. Whether the bytes are the RIGHT bytes is not its job: the
	// caller hashes what landed and compares against rel.
	FetchArtifact(ctx context.Context, rel Release, destDir string) error
}

// MetadataFresher is the OPTIONAL half of Provider: a provider that can say
// how it came by the metadata answer it just returned.
//
// It is optional rather than a fifth method on Provider because a provider
// that reads a directory has no network to be offline from and no cache to be
// stale against -- FileProvider is always live, and a method it would have to
// implement by returning a constant is a method somebody has to read. A caller
// type-asserts for it and treats a provider that does not implement it as
// live.
type MetadataFresher interface {
	// MetadataState describes the most recent metadata answer from this
	// provider. It says nothing about artifacts and nothing about trust.
	MetadataState() MetadataState
}

// StateOf reports how p came by its last metadata answer. A provider that does
// not track freshness reads live, because a provider with no network and no
// cache cannot be anything else.
func StateOf(p Provider) MetadataState {
	if fresher, ok := p.(MetadataFresher); ok {
		return fresher.MetadataState().Normalized()
	}
	return MetadataState{Freshness: FreshnessLive}
}

// ProviderFactory builds a provider for a configured registry. It is an
// interface so the service can be tested against a fixture registry with no
// filesystem, and so adding an AO official registry later is a new case here
// rather than a change to every caller.
type ProviderFactory interface {
	Open(ctx context.Context, reg Registry) (Provider, error)
}

// ProviderFactoryWithOptions is a factory that accepts the caller's client
// options -- the shared cache and clock -- alongside the registry.
//
// It is an OPTIONAL interface rather than a widening of ProviderFactory
// because most factories are test fixtures that have no HTTP client at all,
// and a second parameter they would ignore is a second parameter somebody has
// to read. A caller type-asserts for it and falls back to Open.
type ProviderFactoryWithOptions interface {
	ProviderFactory
	OpenWith(ctx context.Context, reg Registry, opts HTTPSOptions) (Provider, error)
}

// ProviderFactoryFunc adapts a function to ProviderFactory.
type ProviderFactoryFunc func(ctx context.Context, reg Registry) (Provider, error)

// Open implements ProviderFactory.
func (f ProviderFactoryFunc) Open(ctx context.Context, reg Registry) (Provider, error) {
	return f(ctx, reg)
}
