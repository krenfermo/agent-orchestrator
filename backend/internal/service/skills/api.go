package skills

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
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
	out := make([]controllers.SkillInstallView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, installView(rec))
	}
	return out, nil
}

// GetInstalled implements the controller's detail read.
func (s *Service) GetInstalled(ctx context.Context, skillID, version string) (controllers.SkillInstallView, error) {
	rec, err := s.InstalledRecord(ctx, skillID, version)
	if err != nil {
		return controllers.SkillInstallView{}, err
	}
	return installView(rec), nil
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
