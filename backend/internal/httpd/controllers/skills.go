package controllers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// skills.go — the administrative surface for AO's skill catalog.
//
// Two route families, because they answer to two different authorities:
//
//   /skills                 installation-wide. Gated by the global rule table
//                           (settings.read / settings.manage), like every other
//                           installation administration surface.
//   /projects/{id}/skills   project-scoped. Gated here, per project, the same
//                           way /projects/{id}/access is -- a project
//                           administrator activates a skill on their own
//                           project without holding installation authority.
//
// No route here executes a skill, and none is reachable by a worker: an agent
// credential carries a role capped below settings.manage and has no project
// grant that includes project.manage, so the same gates that stop a member
// stop an agent.

// SkillCatalog is the service surface this controller needs. It is an
// interface so the controller depends on the operations rather than on the
// service's construction.
type SkillCatalog interface {
	ListInstalled(ctx context.Context) ([]SkillInstallView, error)
	GetInstalled(ctx context.Context, skillID, version string) (SkillInstallView, error)
	InstallSkill(ctx context.Context, sourceDir, actor string) (SkillInstallView, error)
	UninstallSkill(ctx context.Context, skillID, version, actor string) error
	SkillAudit(ctx context.Context, skillID string) ([]SkillAuditView, error)

	ProjectSkills(ctx context.Context, projectID domain.ProjectID) ([]SkillActivationView, error)
	EnableSkill(ctx context.Context, in EnableSkillInput) (SkillActivationView, error)
	DisableSkill(ctx context.Context, projectID domain.ProjectID, skillID, actor string) error
	DrySkillRun(ctx context.Context, in SkillDryRunInput) (SkillDryRunView, error)
}

// EnableSkillInput carries an activation from the controller to the service,
// including the caller's resolved permissions. The service checks the grant
// against them; it does not re-derive authority.
type EnableSkillInput struct {
	ProjectID        domain.ProjectID
	SkillID          string
	Version          string
	Capabilities     []string
	Actor            string
	ActorPermissions []domain.Permission
}

// SkillDryRunInput carries a dry run to the service.
type SkillDryRunInput struct {
	ProjectID         domain.ProjectID
	SkillID           string
	ModeID            string
	Inputs            map[string]string
	AuthorizedTargets []string
	ActorPermissions  []domain.Permission
}

// SkillModeView is one selectable operating mode of a skill.
type SkillModeView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	RiskLevel    string   `json:"riskLevel" enum:"low,medium,high,critical"`
	Capabilities []string `json:"capabilities"`
	Approval     string   `json:"approval" enum:"none,per_activation,per_run,per_target"`
}

// SkillInstallView is one installed package version.
type SkillInstallView struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RiskLevel   string `json:"riskLevel" enum:"low,medium,high,critical"`
	OriginType  string `json:"originType" enum:"builtin,local,git"`
	OriginRef   string `json:"originRef,omitempty"`
	Publisher   string `json:"publisher"`
	SourceURL   string `json:"sourceUrl,omitempty"`
	// Digest is the content digest verified at install time. It is shown so an
	// operator can compare what is installed against what was published.
	Digest string `json:"digest"`
	// Capabilities is everything the package may ask for, across all modes.
	// It is not what any project granted.
	Capabilities []string        `json:"capabilities"`
	Modes        []SkillModeView `json:"modes"`
	// RequiresIsolatedRunner is the package's own declaration. AO's capability
	// table raises this independently; a manifest can never lower it.
	RequiresIsolatedRunner bool      `json:"requiresIsolatedRunner"`
	Approval               string    `json:"approval" enum:"none,per_activation,per_run,per_target"`
	InstalledAt            time.Time `json:"installedAt"`
	InstalledBy            string    `json:"installedBy,omitempty"`
}

// SkillListResponse is the body of GET /api/v1/skills.
type SkillListResponse struct {
	Skills []SkillInstallView `json:"skills"`
	// Capabilities is AO's capability vocabulary with its fixed policy, so a
	// client renders an activation dialog from the server's answer rather than
	// hard-coding a risk table that would drift.
	Capabilities []SkillCapabilityView `json:"capabilities"`
}

// SkillCapabilityView is one capability and the policy AO applies to it. The
// policy lives in AO, not in the package: a manifest may not describe its own
// capability as cheaper than it is.
type SkillCapabilityView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Risk        string `json:"risk" enum:"low,medium,high,critical"`
	MinApproval string `json:"minApproval" enum:"none,per_activation,per_run,per_target"`
	// RequiresControls names the execution-environment guarantees that must
	// ALL be attested before this capability can be carried. Empty means the
	// capability needs nothing from a runner. A client shows this so a person
	// approving a grant can see what has to exist, not merely that something
	// is "not isolated".
	RequiresControls []string `json:"requiresControls"`
	// RequiredPermission is the AO permission a person must hold to grant it.
	RequiredPermission string `json:"requiredPermission"`
}

// InstallSkillRequest is the body of POST /api/v1/skills.
type InstallSkillRequest struct {
	// SourceDir is an absolute directory on the daemon host holding skill.yaml.
	// A local path is the only install source this phase supports: fetching a
	// remote package means deciding a trust root, and AO has not made that
	// decision -- the manifest contract rejects the signature field for the
	// same reason.
	SourceDir string `json:"sourceDir"`
}

// SkillVersionParams identify one installed version.
type SkillVersionParams struct {
	SkillID string `path:"skillId" description:"Skill identifier (kebab-case)."`
	Version string `path:"version" description:"Installed version (MAJOR.MINOR.PATCH)."`
}

// SkillIDParams identify one skill by id.
type SkillIDParams struct {
	SkillID string `path:"skillId" description:"Skill identifier (kebab-case)."`
}

// ProjectSkillParams identify one skill on one project.
type ProjectSkillParams struct {
	ID      string `path:"id" description:"Project identifier (registry key)."`
	SkillID string `path:"skillId" description:"Skill identifier (kebab-case)."`
}

// SkillAuditView is one row of the catalog's audit trail.
type SkillAuditView struct {
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurredAt"`
	Actor      string    `json:"actor,omitempty"`
	Action     string    `json:"action" enum:"install,uninstall,enable,disable,grant_changed,install_rejected"`
	SkillID    string    `json:"skillId,omitempty"`
	Version    string    `json:"version,omitempty"`
	ProjectID  string    `json:"projectId,omitempty"`
	Digest     string    `json:"digest,omitempty"`
	// Capabilities is the grant in effect after this event.
	Capabilities []string `json:"capabilities"`
	Detail       string   `json:"detail,omitempty"`
}

// SkillAuditResponse is the body of the audit reads.
type SkillAuditResponse struct {
	Entries []SkillAuditView `json:"entries"`
}

// SkillActivationView is one project's activation of one skill.
type SkillActivationView struct {
	SkillID   string `json:"skillId"`
	SkillName string `json:"skillName"`
	Version   string `json:"version"`
	Enabled   bool   `json:"enabled"`
	// GrantedCapabilities is what this project authorized. Empty for a
	// disabled activation: disabling revokes the grant rather than parking it.
	GrantedCapabilities []string `json:"grantedCapabilities"`
	// RequestedCapabilities is what the pinned package asks for, so a client
	// can show the difference without a second request.
	RequestedCapabilities []string  `json:"requestedCapabilities"`
	ApprovedBy            string    `json:"approvedBy,omitempty"`
	ApprovedAt            time.Time `json:"approvedAt,omitzero"`
	UpdatedAt             time.Time `json:"updatedAt"`
	// Available reports whether the pinned package still resolves. False means
	// the version was uninstalled or the files no longer match their digest;
	// Unavailable carries the reason.
	Available   bool   `json:"available"`
	Unavailable string `json:"unavailable,omitempty"`
}

// ProjectSkillsResponse is the body of GET /api/v1/projects/{id}/skills.
type ProjectSkillsResponse struct {
	ProjectID string `json:"projectId"`
	// Installed is every package on this installation, so the activation UI
	// can offer what is available without a second request.
	Installed   []SkillInstallView    `json:"installed"`
	Activations []SkillActivationView `json:"activations"`
	// Permissions is what the CALLER may do in this project. The screen renders
	// its own controls from this rather than re-deriving authority from a role.
	Permissions []string `json:"permissions"`
}

// EnableSkillRequest is the body of PUT /api/v1/projects/{id}/skills/{skillId}.
type EnableSkillRequest struct {
	// Version is required. There is no "latest": an install must never
	// silently change what a project already approved.
	Version string `json:"version"`
	// Capabilities is the grant. Anything the manifest requests but this omits
	// is refused when a run is planned.
	Capabilities []string `json:"capabilities"`
}

// SkillDryRunRequest is the body of the dry run.
type SkillDryRunRequest struct {
	ModeID string            `json:"modeId,omitempty"`
	Inputs map[string]string `json:"inputs,omitempty"`
	// AuthorizedTargets are targets a human has explicitly approved for this
	// run. A per-target capability with none is refused.
	AuthorizedTargets []string `json:"authorizedTargets,omitempty"`
}

// SkillCapabilityDecisionView is one capability's outcome in a dry run.
type SkillCapabilityDecisionView struct {
	Capability string `json:"capability"`
	// Satisfied is whether THIS capability passed every check on its own. It
	// is diagnostics, not the verdict: authorization is fail-closed, so a run
	// with one unsatisfied capability is refused entirely even though the rest
	// report satisfied. Read `verdict` for whether the run can happen.
	Satisfied          bool   `json:"satisfied"`
	Risk               string `json:"risk" enum:"low,medium,high,critical"`
	Description        string `json:"description"`
	RequiredPermission string `json:"requiredPermission"`
	DenialReason       string `json:"denialReason,omitempty"`
	Detail             string `json:"detail,omitempty"`
	// MissingControl is the specific execution-environment guarantee this
	// capability needed and did not get.
	MissingControl string `json:"missingControl,omitempty"`
	// RequiresControls is everything this capability depends on, so a client
	// can show the whole requirement rather than only the first blocker.
	RequiresControls []string `json:"requiresControls"`
}

// SkillRunnerStatusView is what the execution environment provides, and what
// the run would need from it.
type SkillRunnerStatusView struct {
	RunnerID string `json:"runnerId"`
	// Available is whether any runner could carry this run at all.
	Available bool `json:"available"`
	// Isolated and EgressControlled are the two coarse summaries a compact UI
	// renders. Controls is the real answer.
	Isolated         bool `json:"isolated"`
	EgressControlled bool `json:"egressControlled"`
	// Controls are the guarantees this environment has DEMONSTRATED. It is
	// produced by AO from the runner's own probes; a caller cannot supply it,
	// and no request field can influence it.
	Controls []string `json:"controls"`
	// MissingControls are the guarantees this run needs and does not have,
	// which is the list that says what has to be built.
	MissingControls    []string `json:"missingControls"`
	NeedsIsolation     bool     `json:"needsIsolation"`
	NeedsEgressControl bool     `json:"needsEgressControl"`
	// Unavailable explains why no runner is usable, when Available is false
	// for an environmental reason rather than because none is configured.
	Unavailable string `json:"unavailable,omitempty"`
}

// SkillDryRunResponse is the body of the dry run. It describes a run that has
// NOT happened and that this build cannot perform.
type SkillDryRunResponse struct {
	SkillID   string `json:"skillId"`
	Version   string `json:"version"`
	SkillName string `json:"skillName"`
	ModeID    string `json:"modeId"`
	ModeName  string `json:"modeName"`
	ModeRisk  string `json:"modeRisk" enum:"low,medium,high,critical"`
	Verdict   string `json:"verdict" enum:"executable,requires_approval,blocked"`
	// Decisions is one entry per capability the mode requests, in order.
	Decisions []SkillCapabilityDecisionView `json:"decisions"`
	// MissingPermissions names the AO permissions the caller lacks.
	MissingPermissions []string              `json:"missingPermissions"`
	RequiredApproval   string                `json:"requiredApproval" enum:"none,per_activation,per_run,per_target"`
	EffectiveRisk      string                `json:"effectiveRisk,omitempty" enum:"low,medium,high,critical"`
	Runner             SkillRunnerStatusView `json:"runner"`
	// Reasons is the structured blocker list, one line each.
	Reasons []string `json:"reasons"`
}

// SkillDryRunView is the service-side shape of the dry run.
type SkillDryRunView = SkillDryRunResponse

// SkillsController owns both skill route families.
type SkillsController struct {
	Catalog SkillCatalog
	Guard   Guard
}

// Register mounts the skill routes.
func (c *SkillsController) Register(r chi.Router) {
	// Installation-wide. Gated by the global rule table's "skills" family.
	r.Get("/skills", c.list)
	r.Post("/skills", c.install)
	r.Get("/skills/{skillId}/audit", c.audit)
	r.Get("/skills/{skillId}/versions/{version}", c.detail)
	r.Delete("/skills/{skillId}/versions/{version}", c.uninstall)

	// Project-scoped. Gated per project inside each handler.
	r.Get("/projects/{id}/skills", c.projectSkills)
	r.Put("/projects/{id}/skills/{skillId}", c.enable)
	r.Delete("/projects/{id}/skills/{skillId}", c.disable)
	r.Post("/projects/{id}/skills/{skillId}/dry-run", c.dryRun)
}

func (c *SkillsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills")
		return
	}
	skills, err := c.Catalog.ListInstalled(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, SkillListResponse{
		Skills:       skills,
		Capabilities: SkillCapabilityCatalog(),
	})
}

func (c *SkillsController) detail(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/{skillId}/versions/{version}")
		return
	}
	view, err := c.Catalog.GetInstalled(r.Context(),
		chi.URLParam(r, "skillId"), chi.URLParam(r, "version"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, view)
}

func (c *SkillsController) install(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/skills")
		return
	}
	var in InstallSkillRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	view, err := c.Catalog.InstallSkill(r.Context(), in.SourceDir, c.actor(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, view)
}

func (c *SkillsController) uninstall(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/skills/{skillId}/versions/{version}")
		return
	}
	if err := c.Catalog.UninstallSkill(r.Context(),
		chi.URLParam(r, "skillId"), chi.URLParam(r, "version"), c.actor(r)); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (c *SkillsController) audit(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/{skillId}/audit")
		return
	}
	// The global rule table put this route behind settings.read, which a
	// member and a viewer both hold. That is the right floor for "what is
	// installed here" and the wrong one for the trail: it names who installed
	// and enabled what, and carries the host path each package came from.
	// audit.read narrows it to the owner and an administrator -- and gives
	// domain.PermAuditRead the enforced consumer its own doc comment has been
	// waiting for.
	if !c.Guard.AllowGlobal(w, r, domain.PermAuditRead) {
		return
	}
	entries, err := c.Catalog.SkillAudit(r.Context(), chi.URLParam(r, "skillId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if entries == nil {
		entries = []SkillAuditView{}
	}
	envelope.WriteJSON(w, http.StatusOK, SkillAuditResponse{Entries: entries})
}

func (c *SkillsController) projectSkills(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/projects/{id}/skills")
		return
	}
	id := projectID(r)
	if !c.Guard.AllowProject(w, r, domain.PermProjectRead, id, "PROJECT_NOT_FOUND", "project not found") {
		return
	}
	activations, err := c.Catalog.ProjectSkills(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	installed, err := c.Catalog.ListInstalled(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	out := ProjectSkillsResponse{
		ProjectID:   string(id),
		Installed:   installed,
		Activations: activations,
		Permissions: []string{},
	}
	for _, p := range c.callerProjectPermissions(r, id) {
		out.Permissions = append(out.Permissions, string(p))
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *SkillsController) enable(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodPut, "/api/v1/projects/{id}/skills/{skillId}")
		return
	}
	id := projectID(r)
	if !c.Guard.AllowProject(w, r, domain.PermProjectManage, id, "PROJECT_NOT_FOUND", "project not found") {
		return
	}
	var in EnableSkillRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	view, err := c.Catalog.EnableSkill(r.Context(), EnableSkillInput{
		ProjectID:        id,
		SkillID:          chi.URLParam(r, "skillId"),
		Version:          in.Version,
		Capabilities:     in.Capabilities,
		Actor:            c.actor(r),
		ActorPermissions: c.callerProjectPermissions(r, id),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, view)
}

func (c *SkillsController) disable(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/projects/{id}/skills/{skillId}")
		return
	}
	id := projectID(r)
	if !c.Guard.AllowProject(w, r, domain.PermProjectManage, id, "PROJECT_NOT_FOUND", "project not found") {
		return
	}
	if err := c.Catalog.DisableSkill(r.Context(), id, chi.URLParam(r, "skillId"), c.actor(r)); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (c *SkillsController) dryRun(w http.ResponseWriter, r *http.Request) {
	if c.Catalog == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/projects/{id}/skills/{skillId}/dry-run")
		return
	}
	id := projectID(r)
	// A dry run reads: it starts nothing and changes nothing, so project.read
	// is the right gate. It reports the caller's OWN missing permissions, so a
	// member can see exactly what they would need before asking for it.
	if !c.Guard.AllowProject(w, r, domain.PermProjectRead, id, "PROJECT_NOT_FOUND", "project not found") {
		return
	}
	var in SkillDryRunRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	view, err := c.Catalog.DrySkillRun(r.Context(), SkillDryRunInput{
		ProjectID:         id,
		SkillID:           chi.URLParam(r, "skillId"),
		ModeID:            in.ModeID,
		Inputs:            in.Inputs,
		AuthorizedTargets: in.AuthorizedTargets,
		ActorPermissions:  c.callerProjectPermissions(r, id),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, view)
}

// actor names the principal for the audit trail. An empty string is the
// unauthenticated loopback caller, which is a real and recordable case on a
// single-user desktop, not a missing value.
func (c *SkillsController) actor(r *http.Request) string {
	p, err := identity.RequirePrincipal(r)
	if err != nil {
		return ""
	}
	return string(p.User.ID)
}

// callerProjectPermissions resolves what the caller may do in this project.
//
// When no identity layer is wired -- the default single-user desktop, where
// AGENTS.md keeps the loopback listener unauthenticated and trusted -- there is
// no subject to resolve, and returning an empty set would make every grant
// refused and every dry run blocked on a missing permission. That would be a
// new, silent trust boundary on the one listener AO deliberately does not gate,
// and it would break the feature on the default install. So a disabled guard
// yields the full vocabulary, exactly as GlobalAuthzMiddleware passes a
// disabled guard through. When the guard IS enabled, an unresolvable subject
// yields nothing, which is the fail-closed answer for a multi-user install.
func (c *SkillsController) callerProjectPermissions(r *http.Request, id domain.ProjectID) []domain.Permission {
	if !c.Guard.Enabled() {
		return domain.AllPermissions
	}
	sub, ok := c.Guard.Subject(r)
	if !ok {
		return nil
	}
	return sub.ProjectPermissions(id)
}

// controlNames renders a control list for the wire, never nil so a client can
// treat "no controls needed" and "field absent" the same way.
func controlNames(controls []skillcatalog.Control) []string {
	out := make([]string, 0, len(controls))
	for _, c := range controls {
		out = append(out, string(c))
	}
	return out
}

// SkillCapabilityCatalog projects AO's capability table onto the wire. It is a
// function rather than a constant so the response can never disagree with the
// table the authorization decision actually uses.
func SkillCapabilityCatalog() []SkillCapabilityView {
	caps := skillcatalog.AllCapabilities()
	out := make([]SkillCapabilityView, 0, len(caps))
	for _, name := range caps {
		spec, ok := name.Spec()
		if !ok {
			continue
		}
		out = append(out, SkillCapabilityView{
			Name:               string(name),
			Description:        spec.Description,
			Risk:               string(spec.Risk),
			MinApproval:        string(spec.MinApproval),
			RequiresControls:   controlNames(spec.RequiresControls),
			RequiredPermission: string(spec.RequiredPermission),
		})
	}
	return out
}
