package skills

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// api.go — the projection onto the controller's DTOs.
//
// The one thing this layer genuinely DECIDES is what not to send. Two things
// are deliberately absent from every view here: the package's absolute path on
// the daemon host, and the manifest's scope.secrets.requested names. The first
// is filesystem layout a client has no use for; the second is a list of secret
// NAMES, which is not a secret but is a map of what to go looking for.

// ListInstalled implements the controller's catalog list.
func (s *Service) ListInstalled(ctx context.Context) ([]controllers.SkillInstallView, error) {
	recs, err := s.ListInstalledRecords(ctx)
	if err != nil {
		return nil, err
	}
	// One read for every origin rather than one per install: a settings screen
	// listing twenty skills should not become twenty queries, and this is a
	// local table.
	origins := s.originIndex(ctx)
	out := make([]controllers.SkillInstallView, 0, len(recs))
	for _, rec := range recs {
		view := installView(rec)
		if o, ok := origins[originKey(rec.Manifest.ID, rec.Manifest.Version)]; ok {
			view.Origin = originViewPtr(o)
		}
		out = append(out, view)
	}
	return out, nil
}

// originKey identifies one installed version.
func originKey(skillID, version string) string { return skillID + "@" + version }

// originIndex reads every recorded provenance row.
//
// A failure to read provenance does NOT fail the listing: what is installed is
// a fact this service owns, and losing the provenance table must not make the
// catalog unreadable. The rows simply come back absent, which the view already
// distinguishes from "nothing was verified".
func (s *Service) originIndex(ctx context.Context) map[string]store.SkillInstallOrigin {
	if s.origins == nil {
		return nil
	}
	rows, err := s.origins.ListSkillInstallOrigins(ctx)
	if err != nil {
		return nil
	}
	idx := make(map[string]store.SkillInstallOrigin, len(rows))
	for _, row := range rows {
		idx[originKey(row.SkillID, row.Version)] = row
	}
	return idx
}

// GetInstalled implements the controller's detail read.
func (s *Service) GetInstalled(ctx context.Context, skillID, version string) (controllers.SkillInstallView, error) {
	rec, err := s.InstalledRecord(ctx, skillID, version)
	if err != nil {
		return controllers.SkillInstallView{}, err
	}
	view := installView(rec)
	if s.origins != nil {
		if o, ok, err := s.origins.GetSkillInstallOrigin(ctx, skillID, version); err == nil && ok {
			view.Origin = originViewPtr(o)
		}
	}
	return view, nil
}

// InstallSkill implements the controller's install.
func (s *Service) InstallSkill(ctx context.Context, sourceDir, actor string) (controllers.SkillInstallView, error) {
	rec, err := s.Install(ctx, InstallRequest{SourceDir: sourceDir, Actor: actor})
	if err != nil {
		return controllers.SkillInstallView{}, err
	}
	return installView(rec), nil
}

// UninstallSkill implements the controller's uninstall.
func (s *Service) UninstallSkill(ctx context.Context, skillID, version, actor string) error {
	return s.Uninstall(ctx, skillID, version, actor)
}

// SkillAudit implements the controller's audit read.
func (s *Service) SkillAudit(ctx context.Context, skillID string) ([]controllers.SkillAuditView, error) {
	entries, err := s.AuditForSkill(ctx, skillID)
	if err != nil {
		return nil, err
	}
	out := make([]controllers.SkillAuditView, 0, len(entries))
	for _, e := range entries {
		view := controllers.SkillAuditView{
			ID:           e.ID,
			OccurredAt:   e.OccurredAt,
			Actor:        e.Actor,
			Action:       string(e.Action),
			SkillID:      e.SkillID,
			Version:      e.Version,
			Digest:       e.Digest,
			Capabilities: capabilityNamesOf(e.Capabilities),
			Detail:       e.Detail,
		}
		if e.ProjectID != nil {
			view.ProjectID = string(*e.ProjectID)
		}
		out = append(out, view)
	}
	return out, nil
}

// ProjectSkills implements the controller's per-project activation list.
//
// It reports whether each activation still RESOLVES, rather than only whether
// it is flagged enabled. An activation whose package was uninstalled or edited
// on disk is enabled and unusable at the same time, and a list that showed
// only the flag would be reporting the more flattering of the two facts.
func (s *Service) ProjectSkills(ctx context.Context, projectID domain.ProjectID) ([]controllers.SkillActivationView, error) {
	acts, err := s.ListForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]controllers.SkillActivationView, 0, len(acts))
	for _, a := range acts {
		out = append(out, s.activationView(ctx, a))
	}
	return out, nil
}

// EnableSkill implements the controller's activation.
func (s *Service) EnableSkill(ctx context.Context, in controllers.EnableSkillInput) (controllers.SkillActivationView, error) {
	act, err := s.Enable(ctx, EnableRequest{
		ProjectID:        in.ProjectID,
		SkillID:          in.SkillID,
		Version:          in.Version,
		Capabilities:     parseCapabilities(in.Capabilities),
		Actor:            in.Actor,
		ActorPermissions: in.ActorPermissions,
	})
	if err != nil {
		return controllers.SkillActivationView{}, err
	}
	return s.activationView(ctx, act), nil
}

// DisableSkill implements the controller's deactivation.
func (s *Service) DisableSkill(ctx context.Context, projectID domain.ProjectID, skillID, actor string) error {
	return s.Disable(ctx, projectID, skillID, actor)
}

// DrySkillRun implements the controller's dry run.
func (s *Service) DrySkillRun(ctx context.Context, in controllers.SkillDryRunInput) (controllers.SkillDryRunView, error) {
	dr, err := s.DryRun(ctx, DryRunRequest{
		ProjectID:         in.ProjectID,
		SkillID:           in.SkillID,
		ModeID:            in.ModeID,
		Inputs:            in.Inputs,
		AuthorizedTargets: in.AuthorizedTargets,
		ActorPermissions:  in.ActorPermissions,
	})
	if err != nil {
		return controllers.SkillDryRunView{}, err
	}
	decisions := make([]controllers.SkillCapabilityDecisionView, 0, len(dr.Decisions))
	for _, d := range dr.Decisions {
		decisions = append(decisions, controllers.SkillCapabilityDecisionView{
			Capability:         string(d.Capability),
			Satisfied:          d.Satisfied,
			Risk:               string(d.Risk),
			Description:        d.Description,
			RequiredPermission: string(d.RequiredPermission),
			DenialReason:       string(d.DenialReason),
			Detail:             d.Detail,
			MissingControl:     string(d.MissingControl),
			RequiresControls:   controlNames(d.RequiresControls),
		})
	}
	missing := make([]string, 0, len(dr.MissingPermissions))
	for _, p := range dr.MissingPermissions {
		missing = append(missing, string(p))
	}
	reasons := dr.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	return controllers.SkillDryRunView{
		SkillID:            dr.SkillID,
		Version:            dr.Version,
		SkillName:          dr.SkillName,
		ModeID:             dr.ModeID,
		ModeName:           dr.ModeName,
		ModeRisk:           string(dr.ModeRisk),
		Verdict:            string(dr.Verdict),
		Decisions:          decisions,
		MissingPermissions: missing,
		RequiredApproval:   string(dr.RequiredApproval),
		EffectiveRisk:      string(dr.EffectiveRisk),
		Runner: controllers.SkillRunnerStatusView{
			RunnerID:           dr.Runner.RunnerID,
			Available:          dr.Runner.Available,
			Isolated:           dr.Runner.Isolated,
			EgressControlled:   dr.Runner.EgressControlled,
			Controls:           controlNames(dr.Runner.Controls),
			MissingControls:    controlNames(dr.Runner.MissingControls),
			NeedsIsolation:     dr.Runner.NeedsIsolation,
			NeedsEgressControl: dr.Runner.NeedsEgressControl,
			Unavailable:        dr.Runner.Unavailable,
		},
		Reasons: reasons,
	}, nil
}

func (s *Service) activationView(ctx context.Context, a store.SkillActivationRecord) controllers.SkillActivationView {
	view := controllers.SkillActivationView{
		SkillID:               a.SkillID,
		Version:               a.Version,
		Enabled:               a.Enabled,
		GrantedCapabilities:   capabilityNamesOf(a.Grant.Capabilities),
		RequestedCapabilities: []string{},
		ApprovedBy:            a.Grant.ApprovedBy,
		ApprovedAt:            a.Grant.ApprovedAt,
		UpdatedAt:             a.UpdatedAt,
	}
	rec, ok, err := s.store.GetSkillInstall(ctx, a.SkillID, a.Version)
	if err != nil || !ok {
		view.Unavailable = "the pinned version is not installed"
		return view
	}
	view.SkillName = rec.Manifest.Name
	view.RequestedCapabilities = capabilityNamesOf(rec.Manifest.Capabilities)

	// Prove the package still resolves rather than assuming the row is enough.
	if _, err := s.Resolve(ctx, a.ProjectID, a.SkillID); err != nil {
		if a.Enabled {
			view.Unavailable = err.Error()
		}
		return view
	}
	view.Available = true
	return view
}

func installView(rec store.SkillInstallRecord) controllers.SkillInstallView {
	m := rec.Manifest
	modes := make([]controllers.SkillModeView, 0, len(m.Modes))
	for _, mode := range m.Modes {
		modes = append(modes, controllers.SkillModeView{
			ID:           mode.ID,
			Name:         mode.Name,
			Description:  mode.Description,
			RiskLevel:    string(mode.RiskLevel),
			Capabilities: capabilityNamesOf(mode.Capabilities),
			Approval:     string(mode.Approval),
		})
	}
	return controllers.SkillInstallView{
		ID:                     m.ID,
		Version:                m.Version,
		Name:                   m.Name,
		Description:            m.Description,
		RiskLevel:              string(m.RiskLevel),
		OriginType:             string(m.Origin.Type),
		OriginRef:              m.Origin.Ref,
		Publisher:              m.Provenance.Publisher,
		SourceURL:              m.Provenance.SourceURL,
		Digest:                 rec.Digest,
		Capabilities:           capabilityNamesOf(m.Capabilities),
		Modes:                  modes,
		RequiresIsolatedRunner: m.Authorization.RequiresIsolatedRunner,
		Approval:               string(m.Authorization.Approval),
		InstalledAt:            rec.InstalledAt,
		InstalledBy:            rec.InstalledBy,
	}
}

func controlNames(controls []skillcatalog.Control) []string {
	out := make([]string, 0, len(controls))
	for _, c := range controls {
		out = append(out, string(c))
	}
	return out
}

func capabilityNamesOf(caps []skillcatalog.Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}

func parseCapabilities(names []string) []skillcatalog.Capability {
	out := make([]skillcatalog.Capability, 0, len(names))
	for _, n := range names {
		out = append(out, skillcatalog.Capability(n))
	}
	return out
}

// Compile-time proof that the service satisfies the controller's port. It is
// here rather than in the controller so the dependency points one way.
var _ controllers.SkillCatalog = (*Service)(nil)

// ListImageApprovals implements the controller's trust-root listing.
func (s *Service) ListImageApprovals(ctx context.Context) ([]controllers.SkillImageApprovalView, error) {
	approvals, err := s.images.ListApprovals(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]controllers.SkillImageApprovalView, 0, len(approvals))
	for _, a := range approvals {
		out = append(out, imageApprovalView(a, now))
	}
	return out, nil
}

// ApproveImage implements the controller's approval.
func (s *Service) ApproveImage(
	ctx context.Context, in controllers.ApproveSkillImageInput,
) (controllers.SkillImageApprovalView, error) {
	approval, err := s.images.Approve(ctx, ApproveRequest{
		Scope: skillimage.Scope{
			TenantID: in.TenantID, ProjectID: in.ProjectID,
			SkillID: in.SkillID, Version: in.Version, ModeID: in.ModeID,
		},
		Tool: in.Tool, Reference: in.Reference, Digest: in.Digest,
		ExpiresIn: time.Duration(in.ExpiresInSeconds) * time.Second,
		Note:      in.Note, Confirm: in.Confirm,
		Actor: in.Actor, ActorPermissions: in.ActorPermissions,
	})
	if err != nil {
		return controllers.SkillImageApprovalView{}, err
	}
	return imageApprovalView(approval, s.now()), nil
}

// RevokeImageApproval implements the controller's revocation.
func (s *Service) RevokeImageApproval(
	ctx context.Context, id string, in controllers.RevokeSkillImageInput,
) error {
	return s.images.Revoke(ctx, id, in.Actor, in.ActorPermissions)
}

// RevocationPolicy is the exact promise and non-promise of revoking, served
// with the listing so a client rendering it shows the same words the runner
// enforces rather than a paraphrase.
func (s *Service) RevocationPolicy() string { return skillrunner.RevocationPolicy }

// imageApprovalView projects one approval onto the wire.
//
// Active and InactiveReason are COMPUTED here rather than stored, so a client
// cannot read a stale row as permission, and so "expired" and "revoked" stay
// distinguishable instead of collapsing into one false.
func imageApprovalView(a skillimage.Approval, now time.Time) controllers.SkillImageApprovalView {
	view := controllers.SkillImageApprovalView{
		ID:        a.ID,
		TenantID:  string(a.Scope.TenantID),
		ProjectID: string(a.Scope.ProjectID),
		SkillID:   a.Scope.SkillID,
		Version:   a.Scope.Version,
		ModeID:    a.Scope.ModeID,
		Tool:      a.Tool,
		Reference: a.Reference,
		Digest:    a.Digest,

		ApprovedBy:     a.ApprovedBy,
		ApprovedAt:     a.ApprovedAt.UTC().Format(time.RFC3339),
		Note:           a.Note,
		Active:         a.Active(now),
		InactiveReason: a.InactiveReason(now),
	}
	if a.ExpiresAt != nil {
		view.ExpiresAt = a.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if a.RevokedAt != nil {
		view.RevokedAt = a.RevokedAt.UTC().Format(time.RFC3339)
	}
	return view
}

// Compile-time proof that the service satisfies the trust-root port.
var _ controllers.SkillImageTrust = (*Service)(nil)

// ExecuteSkill implements the controller's execution route.
//
// It is a thin adapter and nothing more: every refusal — not installed, not
// enabled, a control the runtime does not attest, an image nobody approved —
// belongs to RunSkill and is not re-decided here. The one thing this does own
// is the wire shape: the report is marshalled as it stands, so what a reader
// sees is what the tool produced.
func (s *Service) ExecuteSkill(
	ctx context.Context, in controllers.SkillRunInput,
) (controllers.SkillRunView, error) {
	res, err := s.RunSkill(ctx, RunRequest{
		ProjectID:        in.ProjectID,
		SkillID:          in.SkillID,
		ModeID:           in.ModeID,
		Inputs:           in.Inputs,
		Actor:            in.Actor,
		ActorPermissions: in.ActorPermissions,
	})
	if err != nil {
		return controllers.SkillRunView{}, err
	}
	report, err := json.Marshal(res.Report)
	if err != nil {
		return controllers.SkillRunView{}, err
	}
	return controllers.SkillRunView{
		SkillID: res.SkillID, Version: res.Version, ModeID: res.ModeID,
		Tool: res.Tool, Report: report,
	}, nil
}
