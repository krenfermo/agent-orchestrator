package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// marketplace.go -- the door through which a package from OUTSIDE this
// repository becomes an installed one.
//
// # What it is allowed to do
//
// Copy verified bytes into the catalog and record where they came from. That
// is the whole list.
//
// # What it deliberately cannot do
//
// Enable a skill on a project, grant a capability, approve an image, run a
// publisher hook or script, or execute anything at all. Installing and running
// stay four states apart -- AVAILABLE, INSTALLED, ENABLED, EXECUTABLE -- and
// nothing here moves an artifact more than one step. There is no Run on this
// surface and no method that would give a caller one.
//
// # Where the authority comes from
//
// settings.manage for every write, checked here as well as at the route, for
// the same reason ImageAuthority checks it: a mis-wired route must not be the
// only thing between a member and the installation's package set. Reads are
// gated by the route's settings.read; a tenant-scoped registry is additionally
// invisible to a caller outside its tenant, and invisible means NOT FOUND
// rather than forbidden, so a private registry's existence does not leak.
//
// # Why verification happens over bytes and not metadata
//
// Every check below that matters runs against the quarantine directory, after
// the fetch: the artifact digest, the manifest digest, the manifest itself, and
// the manifest's agreement with the release. A check against what the registry
// SAID would pass for a registry that lied, which is the only kind worth
// defending against.

// MarketplaceStore is the persistence the marketplace needs.
type MarketplaceStore interface {
	UpsertSkillRegistry(ctx context.Context, reg skillregistry.Registry, at time.Time, actor string) (skillregistry.Registry, error)
	GetSkillRegistry(ctx context.Context, id string) (skillregistry.Registry, bool, error)
	ListSkillRegistries(ctx context.Context) ([]skillregistry.Registry, error)
	DeleteSkillRegistry(ctx context.Context, id string) (bool, error)

	UpsertSkillInstallOrigin(ctx context.Context, o store.SkillInstallOrigin) (store.SkillInstallOrigin, error)
	GetSkillInstallOrigin(ctx context.Context, skillID, version string) (store.SkillInstallOrigin, bool, error)
	ListSkillInstallOrigins(ctx context.Context) ([]store.SkillInstallOrigin, error)
	ListSkillInstallOriginsForSkill(ctx context.Context, skillID string) ([]store.SkillInstallOrigin, error)
	MarkSkillInstallOriginRevoked(ctx context.Context, skillID, version, reason string, at time.Time) (bool, error)

	ListSkillInstalls(ctx context.Context) ([]store.SkillInstallRecord, error)
	AppendSkillAudit(ctx context.Context, entry store.SkillAuditEntry) error
}

// RegistryStatusStore is the phase-11 half: what the network said, and what a
// registry has withdrawn.
//
// It is a SEPARATE interface from MarketplaceStore rather than more methods on
// it, because it is optional. An installation with no status store still
// searches and installs from a local registry; it simply cannot record a
// connection test or a revocation sync, and the marketplace degrades to
// exactly that rather than refusing to start.
type RegistryStatusStore interface {
	UpsertSkillRegistryStatus(ctx context.Context, in store.SkillRegistryStatus) (store.SkillRegistryStatus, error)
	GetSkillRegistryStatus(ctx context.Context, registryID string) (store.SkillRegistryStatus, bool, error)
	ListSkillRegistryStatuses(ctx context.Context) ([]store.SkillRegistryStatus, error)

	UpsertSkillRegistryRevocation(ctx context.Context, in store.SkillRegistryRevocation) (store.SkillRegistryRevocation, error)
	GetSkillRegistryRevocation(ctx context.Context, registryID, skillID, version string) (store.SkillRegistryRevocation, bool, error)
	ListSkillRegistryRevocations(ctx context.Context, registryID string) ([]store.SkillRegistryRevocation, error)
}

// Marketplace searches configured registries and installs exact releases from
// them.
type Marketplace struct {
	store MarketplaceStore
	// catalog performs the actual install. Reusing it rather than writing a
	// second install path is what keeps the "same version, different bytes"
	// conflict, the symlink refusal and the re-verification from the installed
	// copy true for a registry install as well as a local one.
	catalog   *Service
	providers skillregistry.ProviderFactory
	// aoVersion is the running build's version, injected rather than imported:
	// internal/cli owns the build metadata and a service must not depend on the
	// CLI. An unparseable value makes the compatibility check report that it
	// did not run.
	aoVersion string
	// quarantineRoot is where fetched artifacts land before they are verified.
	// It is under AO's data dir, beside the catalog, and every directory in it
	// is removed whether the install succeeded or failed.
	quarantineRoot string
	// status records what the network said and what a registry withdrew. Nil
	// on an installation without it; every use is guarded, and the operations
	// that REQUIRE it say so rather than pretending to work.
	status RegistryStatusStore
	// secrets resolves a private registry's credential. Nil means a registry
	// with an authType other than none cannot be opened, which is the
	// fail-closed direction.
	secrets skillregistry.SecretResolver
	// cache is the metadata/artifact cache. Nil is a working "no cache".
	cache *skillregistry.Cache
	now   func() time.Time
	newID func() string
}

// NewMarketplace builds the marketplace over a store and a catalog service.
//
// A nil store or nil factory makes every operation fail closed rather than
// produce a half-working install path -- the same shape NewImageAuthority has,
// and for the same reason: an unconfigured installation must install nothing,
// not everything.
func NewMarketplace(
	st MarketplaceStore, catalog *Service, providers skillregistry.ProviderFactory,
	aoVersion, dataDir string,
) *Marketplace {
	return &Marketplace{
		store: st, catalog: catalog, providers: providers,
		aoVersion:      strings.TrimSpace(aoVersion),
		quarantineRoot: filepath.Join(dataDir, "skills", "quarantine"),
		// The cache lives under AO's data dir like everything else AO stores.
		// It is created here rather than injected because its location is not
		// a decision anybody should be making per installation.
		cache: skillregistry.NewCache(
			filepath.Join(dataDir, "skills", "registry-cache"), skillregistry.CacheLimits{}),
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { return "skreg-" + randomHex() },
	}
}

// WithConnectivity wires the phase-11 half: the status/revocation store and the
// credential resolver.
//
// It is a separate call rather than four more parameters on NewMarketplace so
// that the twelve existing call sites -- every one of them a test with a local
// fixture registry, which needs neither -- keep saying what they mean.
func (m *Marketplace) WithConnectivity(
	status RegistryStatusStore, secrets skillregistry.SecretResolver,
) *Marketplace {
	if m == nil {
		return nil
	}
	m.status = status
	m.secrets = secrets
	return m
}

// httpOptions are the client options every provider this marketplace opens
// gets: the shared cache, and nothing else.
//
// There is deliberately no path from here to RootCAs or Resolver. Those two
// exist for this package's HTTPS fixture and have no configuration reaching
// them, which is what keeps "AO verifies certificates" a property rather than a
// default.
func (m *Marketplace) httpOptions() skillregistry.HTTPSOptions {
	return skillregistry.HTTPSOptions{Cache: m.cache, Now: m.now}
}

// Available reports whether the marketplace can answer at all.
func (m *Marketplace) Available() bool {
	return m != nil && m.store != nil && m.catalog != nil && m.providers != nil
}

func (m *Marketplace) requireAvailable() error {
	if !m.Available() {
		return apierr.Invalid("SKILL_REGISTRY_UNAVAILABLE",
			"no skill registry backend is configured on this installation", nil)
	}
	return nil
}

// openProvider opens one registry with this marketplace's client options.
//
// Every read and every install goes through here, so the cache and the clock
// reach a provider from exactly one place. A factory that does not take options
// -- every test fixture -- falls back to Open and simply has no cache, which is
// the correct behaviour for a fixture.
func (m *Marketplace) openProvider(
	ctx context.Context, reg skillregistry.Registry,
) (skillregistry.Provider, error) {
	if withOpts, ok := m.providers.(skillregistry.ProviderFactoryWithOptions); ok {
		return withOpts.OpenWith(ctx, reg, m.httpOptions())
	}
	return m.providers.Open(ctx, reg)
}

// ---------------------------------------------------------------- registries

// RegistryRequest is one administrator's registry configuration.
type RegistryRequest struct {
	Registry         skillregistry.Registry
	Actor            string
	ActorPermissions []domain.Permission
	ActorTenants     []domain.TenantID
}

// SaveRegistry adds or updates one registry configuration.
//
// It opens the registry before recording it. A configuration AO cannot read is
// a configuration that would answer every search with silence, and silence
// reads as "this registry has nothing" -- which is a materially more
// comfortable fact than the truth.
func (m *Marketplace) SaveRegistry(ctx context.Context, req RegistryRequest) (skillregistry.Registry, error) {
	if err := m.requireAvailable(); err != nil {
		return skillregistry.Registry{}, err
	}
	if err := requireSettingsManage(req.ActorPermissions,
		"configuring a skill registry"); err != nil {
		return skillregistry.Registry{}, err
	}
	if strings.TrimSpace(req.Actor) == "" {
		return skillregistry.Registry{}, apierr.Invalid("SKILL_REGISTRY_ANONYMOUS",
			"a registry configuration records who made it; one nobody signed is one nobody can be asked about", nil)
	}
	reg := req.Registry
	reg.ID = strings.TrimSpace(reg.ID)
	reg.DisplayName = strings.TrimSpace(reg.DisplayName)
	reg.Location = strings.TrimSpace(reg.Location)
	reg.PinnedPublisher = strings.TrimSpace(reg.PinnedPublisher)
	reg.CredentialSecretName = strings.TrimSpace(reg.CredentialSecretName)
	if err := reg.Validate(); err != nil {
		return skillregistry.Registry{}, apierr.Invalid("SKILL_REGISTRY_INVALID", err.Error(), nil)
	}
	// A tenant-scoped registry may only be created by somebody who is in that
	// tenant. Otherwise settings.manage would be a way to plant a registry
	// inside an organization the actor cannot otherwise see.
	if reg.TenantID != "" && !reg.VisibleTo(req.ActorTenants) {
		return skillregistry.Registry{}, apierr.Forbidden("SKILL_REGISTRY_TENANT_REFUSED",
			fmt.Sprintf("you are not a member of %s, so you cannot configure a registry inside it", reg.TenantID))
	}
	// Opening it proves the configuration is one AO can act on: the base URL
	// reduces to one origin, the network policy parses, and the credential
	// resolves. It does NOT prove the registry answers -- that is what the
	// connection test is for, and conflating the two would make saving a
	// registry depend on it being up.
	if _, err := m.openProvider(ctx, reg); err != nil {
		return skillregistry.Registry{}, apierr.Invalid("SKILL_REGISTRY_UNREADABLE",
			fmt.Sprintf("this registry cannot be read: %v", err), nil)
	}

	previous, existed, err := m.store.GetSkillRegistry(ctx, reg.ID)
	if err != nil {
		return skillregistry.Registry{}, err
	}
	saved, err := m.store.UpsertSkillRegistry(ctx, reg, m.now(), req.Actor)
	if err != nil {
		return skillregistry.Registry{}, err
	}
	action := store.SkillAuditRegistryAdded
	if existed {
		action = store.SkillAuditRegistryUpdated
	}
	m.audit(ctx, store.SkillAuditEntry{
		Actor:  req.Actor,
		Action: action,
		Detail: registryDetail(saved),
	})
	// Enabling and disabling get their own rows. They are buried inside an
	// update otherwise, and "when did this registry start being allowed to
	// serve this installation" is a question with a date, not a diff.
	if existed && previous.Enabled != saved.Enabled {
		toggled := store.SkillAuditRegistryDisabled
		note := "it keeps every install it produced and their provenance, and stops answering " +
			"searches and serving installs"
		if saved.Enabled {
			toggled = store.SkillAuditRegistryEnabled
			note = "it may now answer searches and serve installs"
		}
		m.audit(ctx, store.SkillAuditEntry{
			Actor:  req.Actor,
			Action: toggled,
			Detail: fmt.Sprintf("registry %s: %s", saved.ID, note),
		})
	}
	return saved, nil
}

// RegistryStatus returns what AO last observed about one registry, or a zero
// status when nothing has been observed.
//
// A never-tested registry is a real state and reads as such: it is not a
// failure, and rendering it as one would teach people that red means nothing.
func (m *Marketplace) RegistryStatus(
	ctx context.Context, id string,
) (store.SkillRegistryStatus, error) {
	if m.status == nil {
		return store.SkillRegistryStatus{RegistryID: id}, nil
	}
	row, ok, err := m.status.GetSkillRegistryStatus(ctx, id)
	if err != nil || !ok {
		return store.SkillRegistryStatus{RegistryID: id}, nil //nolint:nilerr // an unrecorded status is not an error.
	}
	return row, nil
}

// RegistryStatuses returns the observed status of every registry, by id.
func (m *Marketplace) RegistryStatuses(
	ctx context.Context,
) (map[string]store.SkillRegistryStatus, error) {
	out := map[string]store.SkillRegistryStatus{}
	if m.status == nil {
		return out, nil
	}
	rows, err := m.status.ListSkillRegistryStatuses(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.RegistryID] = row
	}
	return out, nil
}

// RegistryRevocations returns what AO has recorded as withdrawn by one
// registry, newest observation first.
func (m *Marketplace) RegistryRevocations(
	ctx context.Context, id string, tenants []domain.TenantID,
) ([]store.SkillRegistryRevocation, error) {
	if err := m.requireAvailable(); err != nil {
		return nil, err
	}
	reg, err := m.visibleRegistry(ctx, id, tenants)
	if err != nil {
		return nil, err
	}
	if m.status == nil {
		return []store.SkillRegistryRevocation{}, nil
	}
	rows, err := m.status.ListSkillRegistryRevocations(ctx, reg.ID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].ObservedAt.Equal(rows[j].ObservedAt) {
			return rows[i].ObservedAt.After(rows[j].ObservedAt)
		}
		return rows[i].Ref() < rows[j].Ref()
	})
	return rows, nil
}

// RemoveRegistry deletes one configuration.
//
// It leaves every package installed from it exactly where it is, with its
// provenance intact. Removing a compromised registry must not erase the
// evidence of what it served, and it must not silently uninstall packages a
// project may have pinned -- both of those are decisions for a person, taken
// one package at a time.
func (m *Marketplace) RemoveRegistry(
	ctx context.Context, id string, actor string, perms []domain.Permission, tenants []domain.TenantID,
) error {
	if err := m.requireAvailable(); err != nil {
		return err
	}
	if err := requireSettingsManage(perms, "removing a skill registry"); err != nil {
		return err
	}
	reg, err := m.visibleRegistry(ctx, id, tenants)
	if err != nil {
		return err
	}
	removed, err := m.store.DeleteSkillRegistry(ctx, reg.ID)
	if err != nil {
		return err
	}
	if !removed {
		return apierr.NotFound("SKILL_REGISTRY_NOT_FOUND", fmt.Sprintf("registry %s is not configured", id))
	}
	m.audit(ctx, store.SkillAuditEntry{
		Actor:  actor,
		Action: store.SkillAuditRegistryRemoved,
		Detail: registryDetail(reg) + "; installed packages and their provenance were kept",
	})
	return nil
}

// ListRegistries returns the registries this caller may see.
func (m *Marketplace) ListRegistries(
	ctx context.Context, tenants []domain.TenantID,
) ([]skillregistry.Registry, error) {
	if err := m.requireAvailable(); err != nil {
		return nil, err
	}
	all, err := m.store.ListSkillRegistries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]skillregistry.Registry, 0, len(all))
	for _, reg := range all {
		if reg.VisibleTo(tenants) {
			out = append(out, reg)
		}
	}
	return out, nil
}

// visibleRegistry resolves one registry for a caller, or reports NOT FOUND.
//
// Not found rather than forbidden, deliberately: a tenant-scoped registry's
// existence is itself information, and 403 on a private registry tells a
// stranger which organizations have one.
func (m *Marketplace) visibleRegistry(
	ctx context.Context, id string, tenants []domain.TenantID,
) (skillregistry.Registry, error) {
	reg, ok, err := m.store.GetSkillRegistry(ctx, strings.TrimSpace(id))
	if err != nil {
		return skillregistry.Registry{}, err
	}
	if !ok || !reg.VisibleTo(tenants) {
		return skillregistry.Registry{}, apierr.NotFound("SKILL_REGISTRY_NOT_FOUND",
			fmt.Sprintf("registry %s is not configured", id))
	}
	return reg, nil
}

// ------------------------------------------------------------------- reading

// SearchRequest is one marketplace search.
type SearchRequest struct {
	Query skillregistry.Query
	// RegistryID narrows the search to one registry. Empty searches every
	// enabled registry the caller can see.
	RegistryID   string
	ActorTenants []domain.TenantID
}

// FoundRelease is one search hit, plus what this installation already knows
// about it.
type FoundRelease struct {
	Release      skillregistry.Release
	RegistryName string
	// Trust is what AO can honestly say WITHOUT having fetched anything, which
	// is unverified for every installable release. A listing that showed
	// "verified" would be describing a check nobody ran.
	Trust skillregistry.TrustState
	// Installed reports whether this exact version is in the catalog.
	Installed bool
	// InstalledVersion is the newest version of this skill in the catalog,
	// whichever registry it came from. Empty when nothing is installed.
	InstalledVersion string
	// UpdateAvailable is true when this release is strictly newer than
	// InstalledVersion.
	UpdateAvailable bool
	// Compatibility is the verdict against the running AO version.
	Compatibility skillregistry.CompatibilityVerdict
	// Metadata is how AO came by this row: live from the registry, or from
	// cache because the registry could not be reached.
	//
	// It is carried per release rather than only per registry because a search
	// spans registries, and one of them being offline must not colour the rows
	// that came from the ones that answered. It touches NOTHING else on this
	// struct: a stale row keeps the trust state and the compatibility verdict
	// it would have had live, because freshness is a fact about the metadata
	// and neither of those is.
	Metadata skillregistry.MetadataState
}

// SearchNote is one thing the search could not do, named rather than swallowed.
type SearchNote struct {
	RegistryID string
	Reason     string
	// Metadata is how far AO got. A registry that was unreachable with nothing
	// cached is offline, and saying so is what stops a client rendering a
	// network outage as a configuration error.
	Metadata skillregistry.MetadataState
}

// RegistrySource is one registry the search consulted, and how it answered.
//
// It exists so a client can render "showing cached results, this registry was
// unreachable" WITHOUT inferring it from the shape of the result: an empty
// note list plus some rows is not evidence that everything is current, which
// is precisely the reading that let a stale answer pass for a live one.
type RegistrySource struct {
	RegistryID   string
	RegistryName string
	// Metadata is the explicit state, straight from the provider.
	Metadata skillregistry.MetadataState
}

// SearchResult is a search across registries.
type SearchResult struct {
	Releases []FoundRelease
	// Notes names every registry that could not be searched. An unreadable
	// registry must not disappear into an empty result: "this registry has
	// nothing" and "AO could not read this registry" are different answers,
	// and only one of them means somebody should go and look.
	Notes []SearchNote
	// Sources is every registry the search consulted, answered or not, with
	// the freshness of what it gave. It is always populated, so "is any of
	// this cached" is a field a client reads rather than a thing it deduces.
	Sources []RegistrySource
}

// Offline reports whether any consulted registry could not be reached. It is
// computed here, once, rather than in each client.
func (r SearchResult) Offline() bool {
	for _, src := range r.Sources {
		if src.Metadata.Offline {
			return true
		}
	}
	return false
}

// Search queries every enabled, visible registry.
//
// It moves no package bytes. The Provider interface is shaped so that it
// cannot: fetching is a separate method, and nothing on this path calls it.
//
// It writes no audit row either. A search query is text a person typed, it can
// name an internal package or a vulnerability they are hunting, and a row per
// search buys an auditor nothing the install trail does not already carry. The
// phase brief asked for search auditing "only if it genuinely adds value"; it
// does not, and the audit CHECK has no action for it, so the decision is
// enforced rather than merely intended.
func (m *Marketplace) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	if err := m.requireAvailable(); err != nil {
		return SearchResult{}, err
	}
	registries, err := m.searchTargets(ctx, req)
	if err != nil {
		return SearchResult{}, err
	}
	installed, err := m.installedIndex(ctx)
	if err != nil {
		return SearchResult{}, err
	}

	result := SearchResult{Releases: []FoundRelease{}, Notes: []SearchNote{}, Sources: []RegistrySource{}}
	for _, reg := range registries {
		provider, err := m.openProvider(ctx, reg)
		if err != nil {
			// The registry could not even be OPENED -- a bad configuration, an
			// unresolvable credential. Nothing was asked of the network, so
			// this is not offline; it is unreadable, and the two want
			// different fixes.
			state := skillregistry.MetadataState{Freshness: skillregistry.FreshnessLive}
			result.Notes = append(result.Notes,
				SearchNote{RegistryID: reg.ID, Reason: err.Error(), Metadata: state})
			result.Sources = append(result.Sources, sourceOf(reg, state))
			continue
		}
		rels, searchErr := provider.Search(ctx, req.Query)
		// The state is read whether or not the search succeeded: a registry
		// that failed BECAUSE it was unreachable is the row that has to say
		// offline.
		state := skillregistry.StateOf(provider)
		result.Sources = append(result.Sources, sourceOf(reg, state))
		if searchErr != nil {
			result.Notes = append(result.Notes,
				SearchNote{RegistryID: reg.ID, Reason: searchErr.Error(), Metadata: state})
			continue
		}
		for _, rel := range rels {
			result.Releases = append(result.Releases, m.decorate(rel, reg, installed, state))
		}
	}
	sortFound(result.Releases)
	sortSources(result.Sources)
	return result, nil
}

func sourceOf(reg skillregistry.Registry, state skillregistry.MetadataState) RegistrySource {
	return RegistrySource{
		RegistryID:   reg.ID,
		RegistryName: reg.DisplayName,
		Metadata:     state.Normalized(),
	}
}

func sortSources(sources []RegistrySource) {
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].RegistryID < sources[j].RegistryID
	})
}

func (m *Marketplace) searchTargets(
	ctx context.Context, req SearchRequest,
) ([]skillregistry.Registry, error) {
	visible, err := m.ListRegistries(ctx, req.ActorTenants)
	if err != nil {
		return nil, err
	}
	if id := strings.TrimSpace(req.RegistryID); id != "" {
		reg, err := m.visibleRegistry(ctx, id, req.ActorTenants)
		if err != nil {
			return nil, err
		}
		if !reg.Enabled {
			return nil, nil
		}
		return []skillregistry.Registry{reg}, nil
	}
	enabled := make([]skillregistry.Registry, 0, len(visible))
	for _, reg := range visible {
		if reg.Enabled {
			enabled = append(enabled, reg)
		}
	}
	return enabled, nil
}

// installedIndex maps skill id to its newest installed version, and (id,
// version) to presence.
type installedIndex struct {
	newest  map[string]string
	present map[string]bool
}

func (m *Marketplace) installedIndex(ctx context.Context) (installedIndex, error) {
	recs, err := m.store.ListSkillInstalls(ctx)
	if err != nil {
		return installedIndex{}, err
	}
	idx := installedIndex{newest: map[string]string{}, present: map[string]bool{}}
	for _, rec := range recs {
		id, version := rec.Manifest.ID, rec.Manifest.Version
		idx.present[id+"@"+version] = true
		if cur, ok := idx.newest[id]; !ok || skillregistry.NewerThan(version, cur) {
			idx.newest[id] = version
		}
	}
	return idx, nil
}

func (m *Marketplace) decorate(
	rel skillregistry.Release, reg skillregistry.Registry, idx installedIndex,
	state skillregistry.MetadataState,
) FoundRelease {
	installedVersion := idx.newest[rel.SkillID]
	return FoundRelease{
		Release:      rel,
		RegistryName: reg.DisplayName,
		// AssessAvailable and CheckCompatibility are called with exactly what
		// they were called with before. Freshness is passed alongside them and
		// into neither: a cached row is not less trusted and not less
		// compatible, it is less CURRENT, and merging those would be the same
		// mistake in the other direction.
		Trust:            skillregistry.AssessAvailable(rel),
		Installed:        idx.present[rel.Ref()],
		InstalledVersion: installedVersion,
		UpdateAvailable:  installedVersion != "" && skillregistry.NewerThan(rel.Version, installedVersion),
		Compatibility:    skillregistry.CheckCompatibility(rel, m.aoVersion),
		Metadata:         state.Normalized(),
	}
}

func sortFound(found []FoundRelease) {
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Release.SkillID != found[j].Release.SkillID {
			return found[i].Release.SkillID < found[j].Release.SkillID
		}
		if found[i].Release.Version != found[j].Release.Version {
			return skillregistry.NewerThan(found[i].Release.Version, found[j].Release.Version)
		}
		return found[i].Release.RegistryID < found[j].Release.RegistryID
	})
}

// GetRelease returns one release for display, with every version of the same
// skill the registry offers.
func (m *Marketplace) GetRelease(
	ctx context.Context, registryID, skillID, version string, tenants []domain.TenantID,
) (FoundRelease, []FoundRelease, error) {
	if err := m.requireAvailable(); err != nil {
		return FoundRelease{}, nil, err
	}
	reg, err := m.visibleRegistry(ctx, registryID, tenants)
	if err != nil {
		return FoundRelease{}, nil, err
	}
	provider, err := m.openProvider(ctx, reg)
	if err != nil {
		return FoundRelease{}, nil, registryUnreadable(reg.ID, err)
	}
	idx, err := m.installedIndex(ctx)
	if err != nil {
		return FoundRelease{}, nil, err
	}
	versions, err := provider.ListVersions(ctx, skillID)
	if err != nil {
		return FoundRelease{}, nil, releaseLookupError(reg.ID, skillID, version, err)
	}
	state := skillregistry.StateOf(provider)
	all := make([]FoundRelease, 0, len(versions))
	var current FoundRelease
	var found bool
	for _, rel := range versions {
		decorated := m.decorate(rel, reg, idx, state)
		all = append(all, decorated)
		if rel.Version == version || (version == "" && !found) {
			current, found = decorated, true
		}
	}
	if !found {
		return FoundRelease{}, nil, apierr.NotFound("SKILL_RELEASE_NOT_FOUND",
			fmt.Sprintf("%s@%s is not offered by registry %s", skillID, version, reg.ID))
	}
	return current, all, nil
}

// ----------------------------------------------------------------- installing

// InstallReleaseRequest installs one exact release.
type InstallReleaseRequest struct {
	RegistryID string
	SkillID    string
	// Version is required and exact. There is no "latest" install target, for
	// the same reason there is no "latest" activation target: a person
	// approves a specific thing.
	Version string
	// AsUpdate refuses anything that is not strictly newer than every
	// installed version of this skill, and records update_installed rather
	// than install.
	//
	// It exists so a person clicking "Update" cannot be handed an older
	// release. Installing an older version deliberately, by naming it, stays
	// allowed: versions live side by side and an activation pins one, so an
	// install can never change what a project already resolves to.
	AsUpdate bool
	// AllowOfflineFromCache permits an install to proceed when the registry
	// cannot be reached, using bytes AO already fetched and verified.
	//
	// It is an explicit act rather than a stored setting on purpose. Every
	// other "install anyway" in AO is a person deciding, and a checkbox in
	// settings that quietly allowed offline installs would be a setting nobody
	// remembers is on. What it CANNOT do is any of the checks: the cached tree
	// is re-hashed, the manifest is re-read, the recorded revocation is
	// re-consulted, and a release AO never downloaded cannot be installed
	// offline at all.
	AllowOfflineFromCache bool

	Actor            string
	ActorPermissions []domain.Permission
	ActorTenants     []domain.TenantID
}

// InstallOutcome is one completed registry install.
type InstallOutcome struct {
	Install store.SkillInstallRecord
	Origin  store.SkillInstallOrigin
	Release skillregistry.Release
	// Updated reports whether this replaced an older installed version as the
	// newest. It never means a project's pinned version changed -- nothing
	// here touches an activation.
	Updated bool
	// FromCache reports that the bytes came from AO's artifact cache rather
	// than from the network. They were verified identically -- both digests,
	// recomputed over the tree -- and the distinction is recorded because
	// "where did these bytes come from" is what an incident review asks.
	FromCache bool
	// Offline reports that the registry could not be reached and the install
	// proceeded on evidence AO already held. It is never true unless the
	// caller asked for it.
	Offline bool
}

// InstallRelease resolves, verifies and installs one exact release.
//
// The order below is the design. Every refusal happens before any byte reaches
// the catalog, and every check that matters runs against the quarantine
// directory rather than against what the registry said about it.
func (m *Marketplace) InstallRelease(
	ctx context.Context, req InstallReleaseRequest,
) (InstallOutcome, error) {
	if err := m.requireAvailable(); err != nil {
		return InstallOutcome{}, err
	}
	if err := requireSettingsManage(req.ActorPermissions, "installing a skill from a registry"); err != nil {
		return InstallOutcome{}, err
	}
	if strings.TrimSpace(req.Actor) == "" {
		return InstallOutcome{}, apierr.Invalid("SKILL_INSTALL_ANONYMOUS",
			"an install records who performed it", nil)
	}

	reg, err := m.visibleRegistry(ctx, req.RegistryID, req.ActorTenants)
	if err != nil {
		return InstallOutcome{}, err
	}
	if !reg.Enabled {
		return InstallOutcome{}, m.refuse(ctx, req, reg, "SKILL_REGISTRY_DISABLED",
			fmt.Sprintf("registry %s is disabled", reg.ID))
	}
	provider, err := m.openProvider(ctx, reg)
	if err != nil {
		return InstallOutcome{}, registryUnreadable(reg.ID, err)
	}

	// 1. Resolve ONE exact release, re-read from the authoritative source.
	//    ResolveExactRelease never answers from cache, which is what makes
	//    step 2 mean something.
	rel, offline, err := m.resolveForInstall(ctx, provider, reg, req)
	if err != nil {
		return InstallOutcome{}, err
	}
	rel.RegistryID = reg.ID

	// 2. Revocation blocks a NEW install immediately. It is checked twice, and
	//    the second check is the one that matters when the network is down:
	//    the registry's live answer, AND what AO has already recorded as
	//    withdrawn. A registry AO cannot reach is not a registry with nothing
	//    revoked, and silence is not consent.
	if rel.Revoked {
		return InstallOutcome{}, m.refuse(ctx, req, reg, "SKILL_RELEASE_REVOKED",
			fmt.Sprintf("%s was revoked by %s: %s", rel.Ref(), reg.ID, rel.RevocationReason))
	}
	if known, ok := m.knownRevocation(ctx, reg.ID, rel.SkillID, rel.Version); ok {
		return InstallOutcome{}, m.refuse(ctx, req, reg, "SKILL_RELEASE_REVOKED",
			fmt.Sprintf("%s was revoked by %s on %s: %s. AO recorded this and it stands whether or "+
				"not the registry can be reached right now",
				rel.Ref(), reg.ID, known.ObservedAt.Format(time.RFC3339), known.Reason))
	}

	// 3. The registry's trust policy, beyond integrity.
	if !reg.TrustPolicy.Enforceable() {
		return InstallOutcome{}, m.refuse(ctx, req, reg, "SKILL_TRUST_POLICY_UNSATISFIABLE",
			fmt.Sprintf("registry %s requires a verified signature and AO verifies none; "+
				"nothing can be installed from it until it can", reg.ID))
	}
	if reg.TrustPolicy == skillregistry.TrustPolicyPinnedPublisher &&
		rel.Publisher != reg.PinnedPublisher {
		return InstallOutcome{}, m.refuse(ctx, req, reg, "SKILL_PUBLISHER_MISMATCH",
			fmt.Sprintf("%s is published by %q and registry %s is pinned to %q",
				rel.Ref(), rel.Publisher, reg.ID, reg.PinnedPublisher))
	}

	// 4. Compatibility. An unknown verdict does NOT refuse -- see the note on
	//    skillregistry.CheckCompatibility -- but it is recorded as unknown so
	//    nobody later reads it as a pass.
	compat := skillregistry.CheckCompatibility(rel, m.aoVersion)
	switch compat {
	case skillregistry.CompatibilityTooOld:
		return InstallOutcome{}, m.refuse(ctx, req, reg, "SKILL_AO_TOO_OLD",
			fmt.Sprintf("%s needs AO %s or newer; this build is %s",
				rel.Ref(), rel.Compatibility.AOMinVersion, m.describeAOVersion()))
	case skillregistry.CompatibilityTooNew:
		return InstallOutcome{}, m.refuse(ctx, req, reg, "SKILL_AO_TOO_NEW",
			fmt.Sprintf("%s supports AO up to %s; this build is %s",
				rel.Ref(), rel.Compatibility.AOMaxVersion, m.describeAOVersion()))
	}

	// 5. A skill id belongs to the registry that first installed it. Two
	//    registries taking turns publishing one id is the collision this rule
	//    exists to stop.
	if err := m.checkRegistryBinding(ctx, rel.SkillID, reg.ID); err != nil {
		return InstallOutcome{}, m.refuseErr(ctx, req, reg, err)
	}

	// 6. No-downgrade, when this was asked for as an update.
	if req.AsUpdate {
		if err := m.checkIsAnUpdate(ctx, rel); err != nil {
			return InstallOutcome{}, m.refuseErr(ctx, req, reg, err)
		}
	}

	// 7. Fetch into quarantine, and verify EVERYTHING against the bytes that
	//    landed. The quarantine is removed whichever way this goes.
	quarantine, cleanup, err := m.quarantine(rel)
	if err != nil {
		return InstallOutcome{}, err
	}
	defer cleanup()

	fromCache, err := m.materialize(ctx, provider, reg, rel, req, quarantine, offline)
	if err != nil {
		return InstallOutcome{}, err
	}
	// EVERY path lands here. Cached bytes and freshly downloaded bytes are
	// verified identically, because a cache entry is a file on this host and
	// "we checked it when we wrote it" is not a check that holds now.
	if err := m.verifyBytes(rel, quarantine); err != nil {
		return InstallOutcome{}, m.refuseErr(ctx, req, reg, err)
	}
	pkg, err := skillcatalog.LoadPackage(quarantine)
	if err != nil {
		return InstallOutcome{}, m.refuseErr(ctx, req, reg,
			apierr.Invalid("SKILL_MANIFEST_INVALID", err.Error(), nil))
	}
	if err := verifyManifestAgreesWithRelease(pkg.Manifest, rel); err != nil {
		return InstallOutcome{}, m.refuseErr(ctx, req, reg, err)
	}

	// 8. Install through the ordinary catalog path, which re-verifies from the
	//    INSTALLED copy and refuses a version whose bytes changed.
	installed, err := m.catalog.Install(ctx, InstallRequest{
		SourceDir:   quarantine,
		Actor:       req.Actor,
		SourceLabel: fmt.Sprintf("registry %s, release %s", reg.ID, rel.Ref()),
	})
	if err != nil {
		return InstallOutcome{}, err
	}

	// 9. Record the provenance. It is AO's row, not the registry's, and it
	//    survives the release disappearing from the registry entirely.
	published := rel.PublishedAt
	origin, err := m.store.UpsertSkillInstallOrigin(ctx, store.SkillInstallOrigin{
		SkillID:              rel.SkillID,
		Version:              rel.Version,
		RegistryID:           reg.ID,
		RegistryName:         reg.DisplayName,
		RegistryType:         string(reg.Type),
		RegistryLocation:     reg.Location,
		Publisher:            rel.Publisher,
		SourceURL:            rel.SourceURL,
		ManifestDigest:       rel.ManifestDigest,
		ArtifactDigest:       rel.ArtifactDigest,
		TrustState:           skillregistry.AssessInstalled(rel, true),
		TrustPolicy:          reg.TrustPolicy,
		Provenance:           rel.Provenance,
		CompatibilityVerdict: compat,
		PublishedAt:          &published,
		InstalledAt:          m.now(),
		InstalledBy:          req.Actor,
	})
	if err != nil {
		return InstallOutcome{}, err
	}

	// 10. Keep the verified bytes, addressed by the digest AO computed. This
	//     is what makes a later offline install possible, and it is written
	//     AFTER the install succeeded so the cache only ever holds trees that
	//     passed every check.
	m.cacheArtifact(reg, rel, quarantine)

	if req.AsUpdate {
		m.audit(ctx, store.SkillAuditEntry{
			Actor:   req.Actor,
			Action:  store.SkillAuditUpdateInstalled,
			SkillID: rel.SkillID,
			Version: rel.Version,
			Digest:  installed.Digest,
			Detail:  fmt.Sprintf("updated from registry %s", reg.ID),
		})
	}
	return InstallOutcome{
		Install: installed, Origin: origin, Release: rel, Updated: req.AsUpdate,
		FromCache: fromCache, Offline: offline,
	}, nil
}

// resolveForInstall re-reads one exact release, and decides what to do when the
// registry cannot be reached.
//
// The unreachable case is the only one with a fallback, and it is narrow: the
// caller must have asked for it, AO must hold a release it previously resolved
// AND verified, and everything downstream still runs. Anything else -- a 404, a
// bad certificate, a rejected credential, a malformed answer -- is a refusal,
// because none of those means "the network is down"; they mean something is
// wrong that an install must not paper over.
func (m *Marketplace) resolveForInstall(
	ctx context.Context, provider skillregistry.Provider,
	reg skillregistry.Registry, req InstallReleaseRequest,
) (skillregistry.Release, bool, error) {
	version := strings.TrimSpace(req.Version)
	rel, err := provider.ResolveExactRelease(ctx, req.SkillID, version)
	if err == nil {
		return rel, false, nil
	}
	if !errors.Is(err, skillregistry.ErrRegistryUnreachable) {
		return skillregistry.Release{}, false, releaseLookupError(reg.ID, req.SkillID, version, err)
	}
	if !req.AllowOfflineFromCache {
		return skillregistry.Release{}, false, m.refuse(ctx, req, reg, "SKILL_REGISTRY_UNREACHABLE",
			fmt.Sprintf("registry %s could not be reached to confirm %s@%s: %v. "+
				"Installing without confirming is possible only from bytes AO already fetched and "+
				"verified, and only if you ask for it",
				reg.ID, req.SkillID, version, err))
	}
	cached, ok := m.cachedRelease(reg.ID, req.SkillID, version)
	if !ok {
		// The rule that matters most in offline mode: a release AO never
		// downloaded cannot be installed, whatever anybody asks for.
		return skillregistry.Release{}, false, m.refuse(ctx, req, reg, "SKILL_NOT_IN_CACHE",
			fmt.Sprintf("registry %s cannot be reached and AO has never fetched %s@%s, so there is "+
				"nothing verified to install from", reg.ID, req.SkillID, version))
	}
	return cached, true, nil
}

// materialize puts the package tree into the quarantine, from the cache when
// AO already holds it and from the network otherwise.
//
// It returns whether the bytes came from cache. It does NOT verify: everything
// it produces goes through verifyBytes next, on one path, so there is no way
// for a source to arrive unchecked.
func (m *Marketplace) materialize(
	ctx context.Context, provider skillregistry.Provider, reg skillregistry.Registry,
	rel skillregistry.Release, req InstallReleaseRequest, quarantine string, offline bool,
) (bool, error) {
	if entry, err := m.cache.GetArtifact(reg.ID, rel.ArtifactDigest, rel.ManifestDigest); err == nil {
		// GetArtifact recomputed both digests over the bytes on disk before
		// returning, so a poisoned entry is already gone rather than copied.
		if copyErr := skillcatalog.CopyPackage(entry.Dir(), quarantine); copyErr == nil {
			m.cache.TouchArtifact(reg.ID, rel.ArtifactDigest, rel.ManifestDigest)
			m.audit(ctx, store.SkillAuditEntry{
				Actor:   req.Actor,
				Action:  store.SkillAuditCachedArtifactUsed,
				SkillID: rel.SkillID,
				Version: rel.Version,
				Digest:  rel.ArtifactDigest,
				Detail: fmt.Sprintf("registry %s: the bytes came from AO's verified artifact cache, "+
					"not from the network; both digests were recomputed over them first", reg.ID),
			})
			return true, nil
		}
		// A cache copy that failed is not a reason to give up -- the registry
		// is still the authority -- so fall through to the network.
	}
	if offline {
		return false, m.refuse(ctx, req, reg, "SKILL_NOT_IN_CACHE",
			fmt.Sprintf("registry %s cannot be reached and AO holds no verified copy of %s",
				reg.ID, rel.Ref()))
	}

	m.audit(ctx, store.SkillAuditEntry{
		Actor:   req.Actor,
		Action:  store.SkillAuditSkillFetchStarted,
		SkillID: rel.SkillID,
		Version: rel.Version,
		Digest:  rel.ArtifactDigest,
		Detail: fmt.Sprintf("fetching %s from registry %s into quarantine; nothing is executed and "+
			"nothing reaches the catalog until both digests match", rel.Ref(), reg.ID),
	})
	if err := provider.FetchArtifact(ctx, rel, quarantine); err != nil {
		m.audit(ctx, store.SkillAuditEntry{
			Actor:   req.Actor,
			Action:  store.SkillAuditSkillFetchRefused,
			SkillID: rel.SkillID,
			Version: rel.Version,
			Detail:  fmt.Sprintf("registry %s: %v", reg.ID, err),
		})
		if errors.Is(err, skillregistry.ErrRegistryAuth) {
			m.audit(ctx, store.SkillAuditEntry{
				Actor:  req.Actor,
				Action: store.SkillAuditRegistryAuthFailed,
				Detail: fmt.Sprintf("registry %s refused the credential %s while fetching %s",
					reg.ID, reg.CredentialSecretName, rel.Ref()),
			})
		}
		return false, m.refuse(ctx, req, reg, "SKILL_ARTIFACT_UNFETCHABLE",
			fmt.Sprintf("%s could not be fetched from %s: %v", rel.Ref(), reg.ID, err))
	}
	return false, nil
}

// cacheArtifact stores a tree that has passed every check.
//
// Best-effort: a cache that could not be written costs the next install a
// download, and failing an install that already succeeded to report it would be
// the wrong trade.
func (m *Marketplace) cacheArtifact(
	reg skillregistry.Registry, rel skillregistry.Release, dir string,
) {
	if m.cache == nil {
		return
	}
	_ = m.cache.PutArtifact(skillregistry.ArtifactEntry{
		RegistryID:     reg.ID,
		SkillID:        rel.SkillID,
		Version:        rel.Version,
		ArtifactDigest: rel.ArtifactDigest,
		ManifestDigest: rel.ManifestDigest,
		Release:        rel,
	}, dir)
}

// cachedRelease returns the release AO recorded alongside a cached artifact.
//
// It comes from the ARTIFACT cache and not the metadata cache, deliberately:
// the metadata cache holds whatever a registry last said, and this holds what
// AO verified bytes against. An offline install acts on the second.
func (m *Marketplace) cachedRelease(
	registryID, skillID, version string,
) (skillregistry.Release, bool) {
	if m.cache == nil {
		return skillregistry.Release{}, false
	}
	entry, ok := m.cache.FindArtifactRelease(registryID, skillID, version)
	if !ok {
		return skillregistry.Release{}, false
	}
	return entry, true
}

// quarantine creates the staging directory for one fetch and returns a cleanup
// that removes it whichever way the install goes.
func (m *Marketplace) quarantine(rel skillregistry.Release) (string, func(), error) {
	if err := os.MkdirAll(m.quarantineRoot, 0o700); err != nil {
		return "", nil, fmt.Errorf("create quarantine root: %w", err)
	}
	dir, err := os.MkdirTemp(m.quarantineRoot, "fetch-")
	if err != nil {
		return "", nil, fmt.Errorf("create quarantine for %s: %w", rel.Ref(), err)
	}
	// FetchArtifact copies into dir with CopyPackage, which clears the
	// destination first; an existing empty directory is exactly what it wants.
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// verifyBytes is the integrity check, and it runs over the quarantine.
//
// Both digests, because skillcatalog's package digest deliberately excludes the
// manifest that carries it: checking only one leaves either the manifest or
// everything else uncovered.
func (m *Marketplace) verifyBytes(rel skillregistry.Release, dir string) error {
	artifact, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		return apierr.Invalid("SKILL_ARTIFACT_UNREADABLE", err.Error(), nil)
	}
	if artifact != rel.ArtifactDigest {
		return apierr.Forbidden("SKILL_ARTIFACT_DIGEST_MISMATCH",
			fmt.Sprintf("%s hashed to %s and the release declares %s; "+
				"the bytes served are not the bytes that were resolved",
				rel.Ref(), artifact, rel.ArtifactDigest))
	}
	manifest, err := skillregistry.FileDigest(filepath.Join(dir, skillcatalog.ManifestFileName))
	if err != nil {
		return apierr.Invalid("SKILL_MANIFEST_UNREADABLE", err.Error(), nil)
	}
	if manifest != rel.ManifestDigest {
		return apierr.Forbidden("SKILL_MANIFEST_DIGEST_MISMATCH",
			fmt.Sprintf("%s's manifest hashed to %s and the release declares %s",
				rel.Ref(), manifest, rel.ManifestDigest))
	}
	return nil
}

// verifyManifestAgreesWithRelease refuses a package that is not the thing the
// listing described.
//
// The capability comparison is EXACT in both directions on purpose.
// Understating is the obvious attack -- a listing that asks for repo.read and a
// manifest that asks for the network. Overstating matters too: it means the
// listing a person approved described a different package, and the next person
// to read the marketplace entry would be reading a lie.
func verifyManifestAgreesWithRelease(m skillcatalog.Manifest, rel skillregistry.Release) error {
	if m.ID != rel.SkillID {
		return apierr.Forbidden("SKILL_ID_MISMATCH",
			fmt.Sprintf("the release names %q and the package declares %q", rel.SkillID, m.ID))
	}
	if m.Version != rel.Version {
		return apierr.Forbidden("SKILL_VERSION_MISMATCH",
			fmt.Sprintf("the release names %s and the package declares %s", rel.Version, m.Version))
	}
	if m.Provenance.Publisher != rel.Publisher {
		return apierr.Forbidden("SKILL_PUBLISHER_MISMATCH",
			fmt.Sprintf("the release is published by %q and the package declares %q",
				rel.Publisher, m.Provenance.Publisher))
	}
	declared := map[string]bool{}
	for _, c := range m.Capabilities {
		declared[string(c)] = true
	}
	listed := map[string]bool{}
	for _, c := range rel.RequestedCapabilities {
		listed[c] = true
	}
	var extra, missing []string
	for c := range declared {
		if !listed[c] {
			extra = append(extra, c)
		}
	}
	for c := range listed {
		if !declared[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		return apierr.Forbidden("SKILL_CAPABILITY_MISMATCH",
			fmt.Sprintf("the package asks for %s, which the release listing does not mention",
				strings.Join(extra, ", ")))
	}
	if len(missing) > 0 {
		return apierr.Forbidden("SKILL_CAPABILITY_MISMATCH",
			fmt.Sprintf("the release listing mentions %s, which the package does not ask for",
				strings.Join(missing, ", ")))
	}
	return nil
}

// checkRegistryBinding refuses a skill id already installed from a different
// registry.
func (m *Marketplace) checkRegistryBinding(ctx context.Context, skillID, registryID string) error {
	origins, err := m.store.ListSkillInstallOriginsForSkill(ctx, skillID)
	if err != nil {
		return err
	}
	for _, o := range origins {
		if o.RegistryID != registryID {
			return apierr.Conflict("SKILL_REGISTRY_COLLISION",
				fmt.Sprintf("%s is already installed from registry %s (version %s); "+
					"a skill id belongs to the registry it came from. Uninstall every version "+
					"from %s before installing this id from %s",
					skillID, o.RegistryID, o.Version, o.RegistryID, registryID),
				map[string]any{"boundRegistryId": o.RegistryID, "boundVersion": o.Version})
		}
	}
	return nil
}

// checkIsAnUpdate refuses an update that is not strictly newer.
func (m *Marketplace) checkIsAnUpdate(ctx context.Context, rel skillregistry.Release) error {
	idx, err := m.installedIndex(ctx)
	if err != nil {
		return err
	}
	current, ok := idx.newest[rel.SkillID]
	if !ok {
		return apierr.Invalid("SKILL_UPDATE_NOT_INSTALLED",
			fmt.Sprintf("%s is not installed, so there is nothing to update", rel.SkillID), nil)
	}
	if !skillregistry.NewerThan(rel.Version, current) {
		return apierr.Conflict("SKILL_UPDATE_NOT_NEWER",
			fmt.Sprintf("%s is not newer than the installed %s; "+
				"install it by name if you mean to go back to it", rel.Version, current), nil)
	}
	return nil
}

// ------------------------------------------------------------------- updates

// UpdateStatus is what a check found about one installed release.
type UpdateStatus struct {
	SkillID string
	// Version is the installed version this row is about.
	Version string
	Origin  store.SkillInstallOrigin
	// LatestVersion is the newest installable release the registry offers, or
	// empty when there is none.
	LatestVersion   string
	UpdateAvailable bool
	// RevokedNow reports that the installed release is revoked in the registry
	// TODAY. AO marks it and stops new installs; it removes nothing.
	RevokedNow       bool
	RevocationReason string
	// Unreachable names why AO could not ask -- the registry was removed,
	// disabled, or is unreadable. Provenance is unaffected either way.
	Unreachable string
}

// CheckUpdates asks each installed release's registry what it says now.
//
// It is a request a person made. AO polls nothing, mirrors nothing and
// synchronizes nothing on its own: a background job that fetched from
// configured registries would be a background job that reaches the network on
// a schedule nobody approved.
//
// It never uninstalls. A release revoked after it was installed is marked and
// surfaced; deciding what to do about it is a human's job, and deleting
// somebody's installed package because a remote registry changed its mind would
// be AO acting on an instruction from outside.
func (m *Marketplace) CheckUpdates(
	ctx context.Context, actor string, tenants []domain.TenantID,
) ([]UpdateStatus, error) {
	if err := m.requireAvailable(); err != nil {
		return nil, err
	}
	origins, err := m.store.ListSkillInstallOrigins(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UpdateStatus, 0, len(origins))
	for _, origin := range origins {
		out = append(out, m.checkOne(ctx, origin, actor, tenants))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SkillID != out[j].SkillID {
			return out[i].SkillID < out[j].SkillID
		}
		return skillregistry.NewerThan(out[i].Version, out[j].Version)
	})
	return out, nil
}

func (m *Marketplace) checkOne(
	ctx context.Context, origin store.SkillInstallOrigin, actor string, tenants []domain.TenantID,
) UpdateStatus {
	status := UpdateStatus{
		SkillID: origin.SkillID, Version: origin.Version, Origin: origin,
		RevokedNow:       origin.Revoked(),
		RevocationReason: origin.RevocationReason,
	}
	reg, err := m.visibleRegistry(ctx, origin.RegistryID, tenants)
	if err != nil {
		// The provenance stays exactly as it is. A registry that is gone does
		// not make an installed package's history unknown.
		status.Unreachable = fmt.Sprintf("registry %s is no longer configured or is not visible to you",
			origin.RegistryID)
		return status
	}
	if !reg.Enabled {
		status.Unreachable = fmt.Sprintf("registry %s is disabled", reg.ID)
		return status
	}
	provider, err := m.openProvider(ctx, reg)
	if err != nil {
		status.Unreachable = fmt.Sprintf("registry %s is unreadable: %v", reg.ID, err)
		return status
	}
	versions, err := provider.ListVersions(ctx, origin.SkillID)
	if err != nil {
		status.Unreachable = fmt.Sprintf("registry %s no longer offers %s: %v",
			reg.ID, origin.SkillID, err)
		return status
	}
	for _, rel := range versions {
		if rel.Version == origin.Version && rel.Revoked && !origin.Revoked() {
			m.recordRevocation(ctx, origin, rel, actor)
			status.RevokedNow = true
			status.RevocationReason = rel.RevocationReason
		}
		if rel.Revoked || rel.Deprecated {
			continue
		}
		if skillregistry.NewerThan(rel.Version, origin.Version) &&
			skillregistry.NewerThan(rel.Version, status.LatestVersion) {
			status.LatestVersion = rel.Version
		}
	}
	if status.LatestVersion != "" {
		status.UpdateAvailable = true
		m.audit(ctx, store.SkillAuditEntry{
			Actor:   actor,
			Action:  store.SkillAuditUpdateAvailable,
			SkillID: origin.SkillID,
			Version: status.LatestVersion,
			Detail: fmt.Sprintf("registry %s offers %s; %s is installed",
				reg.ID, status.LatestVersion, origin.Version),
		})
	}
	return status
}

// recordRevocation marks an installed release the registry has since withdrawn.
// It audits once, because the store reports whether the row actually changed.
func (m *Marketplace) recordRevocation(
	ctx context.Context, origin store.SkillInstallOrigin, rel skillregistry.Release, actor string,
) {
	marked, err := m.store.MarkSkillInstallOriginRevoked(
		ctx, origin.SkillID, origin.Version, rel.RevocationReason, m.now())
	if err != nil || !marked {
		return
	}
	m.audit(ctx, store.SkillAuditEntry{
		Actor:   actor,
		Action:  store.SkillAuditReleaseRevokedSeen,
		SkillID: origin.SkillID,
		Version: origin.Version,
		Digest:  origin.ArtifactDigest,
		Detail: fmt.Sprintf("registry %s revoked this release: %s. "+
			"AO did not uninstall it and did not disable it on any project; that decision is yours",
			origin.RegistryID, rel.RevocationReason),
	})
}

// InstallOrigin returns the recorded provenance of one installed version.
func (m *Marketplace) InstallOrigin(
	ctx context.Context, skillID, version string,
) (store.SkillInstallOrigin, bool, error) {
	if err := m.requireAvailable(); err != nil {
		return store.SkillInstallOrigin{}, false, err
	}
	return m.store.GetSkillInstallOrigin(ctx, skillID, version)
}

// --------------------------------------------------------------------- shared

func requireSettingsManage(held []domain.Permission, what string) error {
	if holdsSettingsManage(held) {
		return nil
	}
	return apierr.Forbidden("SKILL_REGISTRY_REFUSED",
		what+" requires the settings.manage permission")
}

func (m *Marketplace) describeAOVersion() string {
	if m.aoVersion == "" {
		return "unversioned"
	}
	return m.aoVersion
}

func registryDetail(reg skillregistry.Registry) string {
	parts := []string{
		fmt.Sprintf("registry %s (%s) at %s", reg.ID, reg.Type, reg.Location),
		"trust policy " + string(reg.TrustPolicy),
		// The auth TYPE and the secret NAME, never a value -- and the network
		// exception, because an exception nobody can see is one nobody
		// reviews.
		"auth " + string(reg.EffectiveAuthType()),
		reg.NetworkPolicy.Summary(),
	}
	if reg.CredentialSecretName != "" {
		parts = append(parts, "credential secret "+reg.CredentialSecretName)
	}
	if !reg.Enabled {
		parts = append(parts, "disabled")
	}
	if reg.TenantID != "" {
		parts = append(parts, "tenant "+string(reg.TenantID))
	}
	return strings.Join(parts, "; ")
}

func registryUnreadable(id string, err error) error {
	return apierr.Invalid("SKILL_REGISTRY_UNREADABLE",
		fmt.Sprintf("registry %s could not be read: %v", id, err), nil)
}

func releaseLookupError(registryID, skillID, version string, err error) error {
	switch {
	case errors.Is(err, skillregistry.ErrNoSuchSkill):
		return apierr.NotFound("SKILL_NOT_OFFERED",
			fmt.Sprintf("registry %s offers no skill %s", registryID, skillID))
	case errors.Is(err, skillregistry.ErrNoSuchRelease):
		return apierr.NotFound("SKILL_RELEASE_NOT_FOUND",
			fmt.Sprintf("registry %s offers no release %s@%s: %v", registryID, skillID, version, err))
	case errors.Is(err, skillregistry.ErrRegistryUnreadable):
		return registryUnreadable(registryID, err)
	}
	return apierr.Invalid("SKILL_RELEASE_LOOKUP_FAILED", err.Error(), nil)
}

// refuse audits an install_refused and returns the error. Every refusal after
// the registry has been resolved goes through here: "somebody tried to install
// a release that failed a check" is the entry an operator most wants to find,
// and it is exactly the one a happy-path-only trail would be missing.
func (m *Marketplace) refuse(
	ctx context.Context, req InstallReleaseRequest, reg skillregistry.Registry, code, message string,
) error {
	return m.refuseErr(ctx, req, reg, apierr.Forbidden(code, message))
}

func (m *Marketplace) refuseErr(
	ctx context.Context, req InstallReleaseRequest, reg skillregistry.Registry, err error,
) error {
	m.audit(ctx, store.SkillAuditEntry{
		Actor:   req.Actor,
		Action:  store.SkillAuditInstallRefused,
		SkillID: req.SkillID,
		Version: req.Version,
		Detail:  fmt.Sprintf("refused from registry %s: %v", reg.ID, err),
	})
	return err
}

// audit appends one trail entry, best-effort for the same reason
// Service.audit is: the operation it describes already happened, and failing
// the caller now would leave the catalog and its history disagreeing in the
// other direction.
func (m *Marketplace) audit(ctx context.Context, entry store.SkillAuditEntry) {
	entry.ID = m.newID()
	entry.OccurredAt = m.now()
	_ = m.store.AppendSkillAudit(ctx, entry)
}
