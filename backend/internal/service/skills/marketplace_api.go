package skills

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// marketplace_api.go — the projection onto the controller's DTOs.
//
// The things this layer decides not to send are the point:
//
//   - a registry's credential VALUE, which is not here to be omitted because it
//     is never loaded: the configuration carries the NAME of a sealed secret;
//   - the quarantine path an artifact passed through, which exists for
//     milliseconds and is filesystem layout a client has no use for;
//   - the manifest's scope.secrets.requested names, for the reason api.go
//     already records -- a list of secret names is a map of what to look for.
//
// The trust state and the compatibility verdict are the daemon's answers, sent
// with the sentence that explains them, so a client renders what AO actually
// checked rather than a table in React that would drift.

// ListRegistryViews implements the controller's registry list.
func (m *Marketplace) ListRegistryViews(
	ctx context.Context, tenants []domain.TenantID,
) ([]controllers.SkillRegistryView, error) {
	regs, err := m.ListRegistries(ctx, tenants)
	if err != nil {
		return nil, err
	}
	// One read for every registry's observed status. A per-row lookup would
	// be N+1 queries to render a settings panel.
	statuses, err := m.RegistryStatuses(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]controllers.SkillRegistryView, 0, len(regs))
	for _, reg := range regs {
		view := m.registryView(reg)
		view.Status = statusView(statuses[reg.ID])
		out = append(out, view)
	}
	return out, nil
}

// TestSkillRegistryConnection implements the controller's connection test.
func (m *Marketplace) TestSkillRegistryConnection(
	ctx context.Context, in controllers.SkillRegistryProbeInput,
) (controllers.SkillRegistryProbeView, error) {
	result, err := m.TestConnection(ctx, in.ID, in.Actor, in.ActorPermissions, in.ActorTenants)
	if err != nil {
		return controllers.SkillRegistryProbeView{}, err
	}
	return controllers.SkillRegistryProbeView{
		RegistryID:         result.RegistryID,
		State:              string(result.State),
		Detail:             result.Detail,
		Origin:             result.Origin,
		ReportedRegistryID: result.ReportedRegistryID,
		ProtocolVersion:    result.ProtocolVersion,
		AuthType:           result.AuthType,
		// The NAME. There is no code path on this side that holds the value.
		SecretRef: result.SecretRef,
		TestedAt:  result.TestedAt,
		LatencyMs: result.Latency.Milliseconds(),
		// Said by the daemon, so the promise on screen and the behaviour in
		// the service cannot drift apart.
		Assurance: controllers.RegistryProbeAssurance,
	}, nil
}

// SyncSkillRegistryRevocations implements the controller's revocation sync.
func (m *Marketplace) SyncSkillRegistryRevocations(
	ctx context.Context, in controllers.SkillRegistryProbeInput,
) (controllers.SkillRegistryRevocationSyncView, error) {
	result, err := m.SyncRevocations(ctx, in.ID, in.Actor, in.ActorPermissions, in.ActorTenants)
	if err != nil {
		return controllers.SkillRegistryRevocationSyncView{}, err
	}
	rows, err := m.RegistryRevocations(ctx, in.ID, in.ActorTenants)
	if err != nil {
		return controllers.SkillRegistryRevocationSyncView{}, err
	}
	installed, err := m.installedIndex(ctx)
	if err != nil {
		return controllers.SkillRegistryRevocationSyncView{}, err
	}
	out := controllers.SkillRegistryRevocationSyncView{
		RegistryID:       result.RegistryID,
		Fetched:          result.Fetched,
		NewlyRecorded:    result.NewlyRecorded,
		AffectedInstalls: nonNil(result.AffectedInstalls),
		Unreachable:      result.Unreachable,
		SyncedAt:         result.SyncedAt,
		Revocations:      make([]controllers.SkillRegistryRevocationView, 0, len(rows)),
		Policy:           controllers.RegistryRevocationPolicy,
	}
	for _, row := range rows {
		out.Revocations = append(out.Revocations, controllers.SkillRegistryRevocationView{
			RegistryID: row.RegistryID,
			SkillID:    row.SkillID,
			Version:    row.Version,
			Reason:     row.Reason,
			RevokedAt:  row.RevokedAt,
			ObservedAt: row.ObservedAt,
			Installed:  installed.present[row.Ref()],
		})
	}
	return out, nil
}

func statusView(st store.SkillRegistryStatus) controllers.SkillRegistryStatusView {
	return controllers.SkillRegistryStatusView{
		LastProbeState:       string(st.LastProbeState),
		LastProbeDetail:      st.LastProbeDetail,
		LastProbeAt:          st.LastProbeAt,
		LastProbeLatency:     st.LastProbeLatency.Milliseconds(),
		LastSyncAt:           st.LastSyncAt,
		LastRevocationSyncAt: st.LastRevocationSyncAt,
	}
}

// SaveRegistryView implements the controller's registry write.
func (m *Marketplace) SaveRegistryView(
	ctx context.Context, in controllers.SaveSkillRegistryInput,
) (controllers.SkillRegistryView, error) {
	saved, err := m.SaveRegistry(ctx, RegistryRequest{
		Registry: skillregistry.Registry{
			ID:                   in.ID,
			DisplayName:          in.DisplayName,
			Type:                 skillregistry.RegistryType(in.Type),
			Location:             in.Location,
			Enabled:              in.Enabled,
			TrustPolicy:          skillregistry.TrustPolicy(in.TrustPolicy),
			PinnedPublisher:      in.PinnedPublisher,
			Priority:             in.Priority,
			TenantID:             domain.TenantID(in.TenantID),
			CredentialSecretName: in.CredentialSecretName,
			AuthType:             skillregistry.AuthType(in.AuthType),
			APIKeyHeader:         in.APIKeyHeader,
			NetworkPolicy: skillregistry.NetworkPolicy{
				PermittedPrivateCIDRs: in.PermittedPrivateCIDRs,
			},
		},
		Actor:            in.Actor,
		ActorPermissions: in.ActorPermissions,
		ActorTenants:     in.ActorTenants,
	})
	if err != nil {
		return controllers.SkillRegistryView{}, err
	}
	return m.registryView(saved), nil
}

// RemoveRegistryView implements the controller's registry removal.
func (m *Marketplace) RemoveRegistryView(
	ctx context.Context, in controllers.RemoveSkillRegistryInput,
) error {
	return m.RemoveRegistry(ctx, in.ID, in.Actor, in.ActorPermissions, in.ActorTenants)
}

// SearchMarketplace implements the controller's search.
func (m *Marketplace) SearchMarketplace(
	ctx context.Context, in controllers.SkillMarketplaceSearchInput,
) (controllers.SkillMarketplaceSearchResponse, error) {
	res, err := m.Search(ctx, SearchRequest{
		RegistryID: in.RegistryID,
		Query: skillregistry.Query{
			Text:              in.Text,
			Publisher:         in.Publisher,
			Capability:        in.Capability,
			IncludeDeprecated: in.IncludeDeprecated,
			IncludeRevoked:    in.IncludeRevoked,
			Limit:             in.Limit,
		},
		Actor:        in.Actor,
		ActorTenants: in.ActorTenants,
	})
	if err != nil {
		return controllers.SkillMarketplaceSearchResponse{}, err
	}
	offline := res.Offline()
	out := controllers.SkillMarketplaceSearchResponse{
		Releases:       make([]controllers.SkillReleaseView, 0, len(res.Releases)),
		Notes:          make([]controllers.SkillRegistryNoteView, 0, len(res.Notes)),
		Sources:        make([]controllers.SkillRegistryFreshnessView, 0, len(res.Sources)),
		Offline:        offline,
		InstallNotice:  controllers.RegistryInstallNotice,
		ExternalNotice: controllers.ExternalHostingNotice,
	}
	if offline {
		// Said by the daemon, so the sentence on screen and the behaviour in
		// the service cannot drift apart.
		out.FreshnessNotice = controllers.RegistryMetadataFreshnessNotice
	}
	for _, rel := range res.Releases {
		out.Releases = append(out.Releases, releaseView(rel))
	}
	for _, note := range res.Notes {
		state := note.Metadata.Normalized()
		out.Notes = append(out.Notes, controllers.SkillRegistryNoteView{
			RegistryID:        note.RegistryID,
			Reason:            note.Reason,
			MetadataFreshness: string(state.Freshness),
			MetadataOffline:   state.Offline,
		})
	}
	for _, src := range res.Sources {
		out.Sources = append(out.Sources, freshnessView(src))
	}
	return out, nil
}

// freshnessView projects one consulted registry's metadata state, with the
// sentence that explains it.
func freshnessView(src RegistrySource) controllers.SkillRegistryFreshnessView {
	state := src.Metadata.Normalized()
	return controllers.SkillRegistryFreshnessView{
		RegistryID:   src.RegistryID,
		RegistryName: src.RegistryName,
		Freshness:    string(state.Freshness),
		Offline:      state.Offline,
		FetchedAt:    state.FetchedAt,
		Explanation:  FreshnessExplanation(state),
	}
}

// GetMarketplaceRelease implements the controller's release detail.
func (m *Marketplace) GetMarketplaceRelease(
	ctx context.Context, in controllers.SkillReleaseLookupInput,
) (controllers.SkillReleaseDetailResponse, error) {
	current, versions, err := m.GetRelease(ctx, in.RegistryID, in.SkillID, in.Version, in.Actor, in.ActorTenants)
	if err != nil {
		return controllers.SkillReleaseDetailResponse{}, err
	}
	state := current.Metadata.Normalized()
	out := controllers.SkillReleaseDetailResponse{
		Release:  releaseView(current),
		Versions: make([]controllers.SkillReleaseView, 0, len(versions)),
		Source: freshnessView(RegistrySource{
			RegistryID:   current.Release.RegistryID,
			RegistryName: current.RegistryName,
			Metadata:     state,
		}),
		Offline:       state.Offline,
		InstallNotice: controllers.RegistryInstallNotice,
	}
	if state.Offline {
		out.FreshnessNotice = controllers.RegistryMetadataFreshnessNotice
	}
	for _, rel := range versions {
		out.Versions = append(out.Versions, releaseView(rel))
	}
	return out, nil
}

// InstallMarketplaceRelease implements the controller's install.
func (m *Marketplace) InstallMarketplaceRelease(
	ctx context.Context, in controllers.SkillInstallReleaseInput,
) (controllers.SkillInstallOutcomeView, error) {
	outcome, err := m.InstallRelease(ctx, InstallReleaseRequest{
		RegistryID:            in.RegistryID,
		SkillID:               in.SkillID,
		Version:               in.Version,
		AsUpdate:              in.AsUpdate,
		AllowOfflineFromCache: in.AllowOfflineFromCache,
		AcknowledgeMovedTag:   in.AcknowledgeMovedTag,
		Actor:                 in.Actor,
		ActorPermissions:      in.ActorPermissions,
		ActorTenants:          in.ActorTenants,
	})
	if err != nil {
		return controllers.SkillInstallOutcomeView{}, err
	}
	return controllers.SkillInstallOutcomeView{
		Install:   installView(outcome.Install),
		Origin:    originView(outcome.Origin),
		Updated:   outcome.Updated,
		FromCache: outcome.FromCache,
		Offline:   outcome.Offline,
		// Said by the daemon, so the promise on screen and the behaviour in
		// the service cannot drift apart.
		NextStep: controllers.RegistryInstalledNotice,
	}, nil
}

// CheckMarketplaceUpdates implements the controller's update check.
func (m *Marketplace) CheckMarketplaceUpdates(
	ctx context.Context, in controllers.SkillUpdateCheckInput,
) (controllers.SkillUpdateCheckResponse, error) {
	statuses, err := m.CheckUpdates(ctx, in.Actor, in.ActorTenants)
	if err != nil {
		return controllers.SkillUpdateCheckResponse{}, err
	}
	out := controllers.SkillUpdateCheckResponse{
		Statuses:         make([]controllers.SkillUpdateStatusView, 0, len(statuses)),
		RevocationPolicy: controllers.RegistryRevocationPolicy,
	}
	for _, s := range statuses {
		out.Statuses = append(out.Statuses, controllers.SkillUpdateStatusView{
			SkillID:          s.SkillID,
			Version:          s.Version,
			Origin:           originView(s.Origin),
			LatestVersion:    s.LatestVersion,
			UpdateAvailable:  s.UpdateAvailable,
			RevokedNow:       s.RevokedNow,
			RevocationReason: s.RevocationReason,
			Unreachable:      s.Unreachable,
		})
	}
	return out, nil
}

// registryView renders one registry, including whether its trust policy can
// actually be satisfied HERE.
//
// It is a method rather than a function because "enforceable" stopped being a
// property of the policy in phase 12 and became a property of the
// INSTALLATION: signed works when a trust root exists, and official works when
// an AO Official root ships. A constant answer would have told a settings
// screen that the official policy works on a build that refuses every install
// under it.
func (m *Marketplace) registryView(reg skillregistry.Registry) controllers.SkillRegistryView {
	return controllers.SkillRegistryView{
		ID:          reg.ID,
		DisplayName: reg.DisplayName,
		Type:        string(reg.Type),
		Location:    reg.Location,
		Enabled:     reg.Enabled,
		TrustPolicy: string(reg.TrustPolicy),
		// A policy this build cannot satisfy installs nothing. A client has to
		// be able to say so rather than showing the strictest-looking setting
		// as if it were working.
		TrustPolicyEnforceable: m.policySatisfiable(reg.TrustPolicy),
		PinnedPublisher:        reg.PinnedPublisher,
		// The external scope, sent so a settings screen can show it. An
		// allowlist nobody can read is one nobody reviews, and an owner
		// nobody can see is an install source nobody notices.
		Owner:         reg.Owner,
		Repository:    reg.Repository,
		AllowedOwners: nonNil(reg.AllowedOwners),
		// The daemon's answer, not a client comparing type strings: it is what
		// decides whether the hosting notice has to be shown.
		External: reg.Type.External(),
		Priority: reg.Priority,
		TenantID: string(reg.TenantID),
		// The NAME only. The value lives sealed and is never loaded here.
		CredentialSecretName: reg.CredentialSecretName,
		AuthType:             string(reg.EffectiveAuthType()),
		APIKeyHeader:         reg.APIKeyHeader,
		// Sent so a settings screen can SHOW them. An exception nobody can see
		// is one nobody reviews, and this is the one setting on the row that
		// widens what AO may connect to.
		PermittedPrivateCIDRs: nonNil(reg.NetworkPolicy.PermittedPrivateCIDRs),
		NetworkPolicySummary:  reg.NetworkPolicy.Summary(),
		CreatedAt:             reg.CreatedAt,
		UpdatedAt:             reg.UpdatedAt,
	}
}

func releaseView(f FoundRelease) controllers.SkillReleaseView {
	rel := f.Release
	modes := make([]controllers.SkillReleaseModeView, 0, len(rel.ExecutionModes))
	for _, mode := range rel.ExecutionModes {
		modes = append(modes, controllers.SkillReleaseModeView{
			ID:           mode.ID,
			Name:         mode.Name,
			Description:  mode.Description,
			RiskLevel:    mode.RiskLevel,
			Capabilities: nonNil(mode.Capabilities),
		})
	}
	return controllers.SkillReleaseView{
		RegistryID:      rel.RegistryID,
		RegistryName:    f.RegistryName,
		SkillID:         rel.SkillID,
		Name:            rel.Name,
		Version:         rel.Version,
		Publisher:       rel.Publisher,
		Description:     rel.Description,
		RiskLevel:       rel.RiskLevel,
		SourceURL:       rel.SourceURL,
		ChangelogURL:    rel.ChangelogURL,
		Changelog:       rel.Changelog,
		ManifestDigest:  rel.ManifestDigest,
		ArtifactDigest:  rel.ArtifactDigest,
		SignatureFormat: rel.Provenance.SignatureFormat,
		KeyID:           rel.Provenance.KeyID,
		AttestationURL:  rel.Provenance.AttestationURL,
		// Signed says the material is THERE. It never says it verifies: a
		// search checks no signature, exactly as it hashes no bytes.
		Signed:                rel.Signed(),
		SignatureKeyID:        rel.Signature.KeyID,
		SignatureScheme:       string(rel.Signature.Scheme),
		RequestedCapabilities: nonNil(rel.RequestedCapabilities),
		ExecutionModes:        modes,
		AOMinVersion:          rel.Compatibility.AOMinVersion,
		AOMaxVersion:          rel.Compatibility.AOMaxVersion,
		Compatibility:         string(f.Compatibility),
		PublishedAt:           rel.PublishedAt,
		Deprecated:            rel.Deprecated,
		DeprecationNote:       rel.DeprecationNote,
		Revoked:               rel.Revoked,
		RevocationReason:      rel.RevocationReason,
		// The git source, as AO resolved it.
		SourceProvider:    string(rel.Source.Provider),
		SourceOwner:       rel.Source.Owner,
		SourceRepository:  rel.Source.Repository,
		SourceTag:         rel.Source.Tag,
		SourceCommit:      rel.Source.Commit,
		SourceShortCommit: rel.Source.ShortCommit(),
		SourcePath:        rel.Source.Path,
		SourceVisibility:  rel.Source.Visibility,
		// A moved tag travels beside trust and into none of it.
		TagMoved:              f.TagMoved,
		TagMovedFromCommit:    f.TagMovedFrom,
		TagMovedAt:            f.TagMovedAt,
		TagMovedExplanation:   movedTagExplanation(f),
		ExternalRevoked:       f.ExternalRevoked,
		ExternalRevokedReason: f.ExternalRevokedReason,
		Trust:                 string(f.Trust),
		TrustExplanation:      TrustExplanation(f.Trust),
		Installed:             f.Installed,
		InstalledVersion:      f.InstalledVersion,
		UpdateAvailable:       f.UpdateAvailable,
		// Freshness travels beside trust and compatibility and into neither.
		MetadataFreshness: string(f.Metadata.Normalized().Freshness),
		MetadataOffline:   f.Metadata.Offline,
		MetadataAsOf:      f.Metadata.FetchedAt,
	}
}

// movedTagExplanation is the daemon's sentence for a row whose tag moved.
//
// It is composed here rather than in a UI for the reason every other
// explanatory sentence on this surface is: a screen that wrote its own would
// eventually write a shorter, kinder one, and the kind version of this
// particular sentence ("updated") is exactly the reading the attack wants.
func movedTagExplanation(f FoundRelease) string {
	if !f.TagMoved {
		return ""
	}
	return skillregistry.DescribeTagMove(store.SkillExternalTag{
		Owner:           f.Release.Source.Owner,
		Repository:      f.Release.Source.Repository,
		Tag:             f.Release.Source.Tag,
		Commit:          f.Release.Source.Commit,
		MovedFromCommit: f.TagMovedFrom,
		MovedAt:         f.TagMovedAt,
	})
}

// FreshnessExplanation says in words what a metadata state means.
//
// It is served rather than rendered in the UI for the same reason
// TrustExplanation is: a screen that wrote its own version of these sentences
// would eventually write a nicer one, and the nicest available lie about this
// particular subject is "up to date".
func FreshnessExplanation(state skillregistry.MetadataState) string {
	asOf := ""
	if !state.FetchedAt.IsZero() {
		asOf = " The registry last answered at " + state.FetchedAt.UTC().Format(time.RFC3339) + "."
	}
	switch {
	case state.Offline && state.Freshness == skillregistry.FreshnessStale:
		return "AO could not reach this registry, and what is shown was cached longer ago than the cache " +
			"considers current. It is not what the registry says now: a release withdrawn since then would " +
			"still appear." + asOf
	case state.Offline && state.Freshness == skillregistry.FreshnessOffline && !state.FetchedAt.IsZero():
		return "AO could not reach this registry. What is shown is metadata AO cached recently and could not " +
			"confirm just now -- recent is not the same as current." + asOf
	case state.Offline:
		return "AO could not reach this registry and has nothing cached for it, so it contributed nothing to " +
			"this result. That is not the same as the registry having nothing."
	case state.Freshness == skillregistry.FreshnessCached:
		return "This came from AO's cache without asking the registry, and the cache still considers it " +
			"current." + asOf
	case state.Freshness == skillregistry.FreshnessStale:
		return "This came from AO's cache and the cache no longer considers it current." + asOf
	}
	return "The registry answered during this request." + asOf
}

func originView(o store.SkillInstallOrigin) controllers.SkillInstallOriginView {
	return controllers.SkillInstallOriginView{
		SkillID:          o.SkillID,
		Version:          o.Version,
		RegistryID:       o.RegistryID,
		RegistryName:     o.RegistryName,
		RegistryType:     o.RegistryType,
		Publisher:        o.Publisher,
		SourceURL:        o.SourceURL,
		ManifestDigest:   o.ManifestDigest,
		ArtifactDigest:   o.ArtifactDigest,
		Trust:            string(o.TrustState),
		TrustExplanation: TrustExplanation(o.TrustState),
		TrustPolicy:      string(o.TrustPolicy),
		Compatibility:    string(o.CompatibilityVerdict),
		// The git provenance, exactly as it was written at install.
		SourceProvider:      string(o.Source.Provider),
		SourceOwner:         o.Source.Owner,
		SourceRepository:    o.Source.Repository,
		SourceTag:           o.Source.Tag,
		SourceCommit:        o.Source.Commit,
		SourceShortCommit:   o.Source.ShortCommit(),
		SourcePath:          o.Source.Path,
		SourceVisibility:    o.Source.Visibility,
		SourceFetchedAt:     o.SourceFetchedAt,
		IdentityExplanation: originIdentityExplanation(o),
		InstalledAt:         o.InstalledAt,
		InstalledBy:         o.InstalledBy,
		// The chain, as one object. A client that had to reassemble it from
		// loose fields would eventually render half a chain as a whole one.
		Provenance:              provenanceView(o.Verification),
		RevocationStateObserved: o.RevocationStateObserved,
		MetadataAsOf:            asOfOrZero(o.MetadataFetchedAt),
		Revoked:                 o.Revoked(),
		RevocationReason:        o.RevocationReason,
	}
}

// originIdentityExplanation says who an installed external release is BY, as
// the several separate facts it actually is.
//
// It is empty for every install that did not come from a forge, because there
// the publisher and the registry are the same administrator's decision and
// there is nothing to disentangle. On a forge there are four different answers
// to "who published this" and they are routinely different people -- see
// skillregistry/publisheridentity.go -- and a screen that merged them would
// say "published by acme" because a repository at github.com/acme said so.
func originIdentityExplanation(o store.SkillInstallOrigin) string {
	if !o.Source.Declared() {
		return ""
	}
	identity := skillregistry.ExternalIdentity{
		Provider:          o.Source.Provider,
		Owner:             o.Source.Owner,
		Repository:        o.Source.Repository,
		DeclaredPublisher: o.Publisher,
	}
	if o.Verification.Verified {
		identity.SigningPublisher = o.Verification.Publisher
	}
	return identity.Describe()
}

// TrustExplanation says in words what a trust state means.
//
// It is served rather than rendered in the UI for the same reason the image
// trust model is: a screen that wrote its own version of these sentences would
// eventually write a nicer one, and the nicest available lie about this
// particular subject is "trusted".
func TrustExplanation(state skillregistry.TrustState) string {
	switch state {
	case skillregistry.TrustRevoked:
		return "The registry withdrew this release. It cannot be installed. " +
			"An installed copy is left exactly where it is; removing it is a human's decision."
	case skillregistry.TrustVerified:
		return "AO fetched the bytes and computed both digests itself, and they match the release it " +
			"resolved. That is integrity, not provenance: AO verified no signature and does not know " +
			"who wrote this code."
	case skillregistry.TrustTrusted:
		return "AO verified the bytes AND a signature over exactly those bytes, made by a key " +
			"that chains to a trust root this installation configured and held by the publisher " +
			"this release names. That says WHO signed it. It does not say the code is safe, that " +
			"it has no vulnerabilities, that its capabilities are benign, or that anybody " +
			"reviewed it."
	}
	return "Nothing has been checked. AO has not fetched these bytes, so it has verified nothing " +
		"about them -- the digests below are what the registry claims, not what AO measured."
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
