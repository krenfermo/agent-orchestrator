package skills

import (
	"context"

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
		view := registryView(reg)
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
	return registryView(saved), nil
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
		ActorTenants: in.ActorTenants,
	})
	if err != nil {
		return controllers.SkillMarketplaceSearchResponse{}, err
	}
	out := controllers.SkillMarketplaceSearchResponse{
		Releases:      make([]controllers.SkillReleaseView, 0, len(res.Releases)),
		Notes:         make([]controllers.SkillRegistryNoteView, 0, len(res.Notes)),
		InstallNotice: controllers.RegistryInstallNotice,
	}
	for _, rel := range res.Releases {
		out.Releases = append(out.Releases, releaseView(rel))
	}
	for _, note := range res.Notes {
		out.Notes = append(out.Notes, controllers.SkillRegistryNoteView{
			RegistryID: note.RegistryID, Reason: note.Reason,
		})
	}
	return out, nil
}

// GetMarketplaceRelease implements the controller's release detail.
func (m *Marketplace) GetMarketplaceRelease(
	ctx context.Context, in controllers.SkillReleaseLookupInput,
) (controllers.SkillReleaseDetailResponse, error) {
	current, versions, err := m.GetRelease(ctx, in.RegistryID, in.SkillID, in.Version, in.ActorTenants)
	if err != nil {
		return controllers.SkillReleaseDetailResponse{}, err
	}
	out := controllers.SkillReleaseDetailResponse{
		Release:       releaseView(current),
		Versions:      make([]controllers.SkillReleaseView, 0, len(versions)),
		InstallNotice: controllers.RegistryInstallNotice,
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

func registryView(reg skillregistry.Registry) controllers.SkillRegistryView {
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
		TrustPolicyEnforceable: reg.TrustPolicy.Enforceable(),
		PinnedPublisher:        reg.PinnedPublisher,
		Priority:               reg.Priority,
		TenantID:               string(reg.TenantID),
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
		RegistryID:            rel.RegistryID,
		RegistryName:          f.RegistryName,
		SkillID:               rel.SkillID,
		Name:                  rel.Name,
		Version:               rel.Version,
		Publisher:             rel.Publisher,
		Description:           rel.Description,
		RiskLevel:             rel.RiskLevel,
		SourceURL:             rel.SourceURL,
		ChangelogURL:          rel.ChangelogURL,
		Changelog:             rel.Changelog,
		ManifestDigest:        rel.ManifestDigest,
		ArtifactDigest:        rel.ArtifactDigest,
		SignatureFormat:       rel.Provenance.SignatureFormat,
		KeyID:                 rel.Provenance.KeyID,
		AttestationURL:        rel.Provenance.AttestationURL,
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
		Trust:                 string(f.Trust),
		TrustExplanation:      TrustExplanation(f.Trust),
		Installed:             f.Installed,
		InstalledVersion:      f.InstalledVersion,
		UpdateAvailable:       f.UpdateAvailable,
	}
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
		InstalledAt:      o.InstalledAt,
		InstalledBy:      o.InstalledBy,
		Revoked:          o.Revoked(),
		RevocationReason: o.RevocationReason,
	}
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
		// Unreachable in this build; see skillregistry's package doc and ADR
		// 0006. The sentence exists so that if it ever IS reachable, somebody
		// had to write what it would mean.
		return "A signature chained to a trust anchor this installation configured was verified."
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
