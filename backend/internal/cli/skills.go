package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// skills.go — `ao skills`, a thin client over the catalog routes.
//
// It runs nothing. `ao skills dry-run` is the closest thing to an execution
// verb here, and it reports what a run WOULD need without starting one; the
// daemon side starts no process either. The DTOs below are hand-mirrored from
// httpd/controllers, which is the deliberate manual boundary AGENTS.md keeps
// between the CLI and the HTTP controller package.

// skillModeDTO mirrors controllers.SkillModeView.
type skillModeDTO struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	RiskLevel    string   `json:"riskLevel"`
	Capabilities []string `json:"capabilities"`
	Approval     string   `json:"approval"`
}

// skillInstallDTO mirrors controllers.SkillInstallView.
type skillInstallDTO struct {
	ID                     string         `json:"id"`
	Version                string         `json:"version"`
	Name                   string         `json:"name"`
	Description            string         `json:"description"`
	RiskLevel              string         `json:"riskLevel"`
	OriginType             string         `json:"originType"`
	OriginRef              string         `json:"originRef"`
	Publisher              string         `json:"publisher"`
	SourceURL              string         `json:"sourceUrl"`
	Digest                 string         `json:"digest"`
	Capabilities           []string       `json:"capabilities"`
	Modes                  []skillModeDTO `json:"modes"`
	RequiresIsolatedRunner bool           `json:"requiresIsolatedRunner"`
	Approval               string         `json:"approval"`
	InstalledAt            time.Time      `json:"installedAt"`
	InstalledBy            string         `json:"installedBy"`
}

// skillCapabilityDTO mirrors controllers.SkillCapabilityView.
type skillCapabilityDTO struct {
	Name                  string `json:"name"`
	Description           string `json:"description"`
	Risk                  string `json:"risk"`
	MinApproval           string `json:"minApproval"`
	RequiresIsolation     bool   `json:"requiresIsolation"`
	RequiresEgressControl bool   `json:"requiresEgressControl"`
	RequiredPermission    string `json:"requiredPermission"`
}

// skillListDTO mirrors controllers.SkillListResponse.
type skillListDTO struct {
	Skills       []skillInstallDTO    `json:"skills"`
	Capabilities []skillCapabilityDTO `json:"capabilities"`
}

// skillActivationDTO mirrors controllers.SkillActivationView.
type skillActivationDTO struct {
	SkillID               string   `json:"skillId"`
	SkillName             string   `json:"skillName"`
	Version               string   `json:"version"`
	Enabled               bool     `json:"enabled"`
	GrantedCapabilities   []string `json:"grantedCapabilities"`
	RequestedCapabilities []string `json:"requestedCapabilities"`
	ApprovedBy            string   `json:"approvedBy"`
	Available             bool     `json:"available"`
	Unavailable           string   `json:"unavailable"`
}

// projectSkillsDTO mirrors controllers.ProjectSkillsResponse.
type projectSkillsDTO struct {
	ProjectID   string               `json:"projectId"`
	Installed   []skillInstallDTO    `json:"installed"`
	Activations []skillActivationDTO `json:"activations"`
	Permissions []string             `json:"permissions"`
}

// installSkillRequest mirrors controllers.InstallSkillRequest.
type installSkillRequest struct {
	SourceDir string `json:"sourceDir"`
}

// enableSkillRequest mirrors controllers.EnableSkillRequest.
type enableSkillRequest struct {
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

// skillDryRunRequest mirrors controllers.SkillDryRunRequest.
type skillDryRunRequest struct {
	ModeID            string            `json:"modeId,omitempty"`
	Inputs            map[string]string `json:"inputs,omitempty"`
	AuthorizedTargets []string          `json:"authorizedTargets,omitempty"`
}

// skillDryRunDTO mirrors controllers.SkillDryRunResponse.
type skillDryRunDTO struct {
	SkillID   string `json:"skillId"`
	Version   string `json:"version"`
	SkillName string `json:"skillName"`
	ModeID    string `json:"modeId"`
	ModeName  string `json:"modeName"`
	ModeRisk  string `json:"modeRisk"`
	Verdict   string `json:"verdict"`
	Decisions []struct {
		Capability         string `json:"capability"`
		Satisfied          bool   `json:"satisfied"`
		Risk               string `json:"risk"`
		Description        string `json:"description"`
		RequiredPermission string `json:"requiredPermission"`
		DenialReason       string `json:"denialReason"`
		Detail             string `json:"detail"`
	} `json:"decisions"`
	MissingPermissions []string `json:"missingPermissions"`
	RequiredApproval   string   `json:"requiredApproval"`
	EffectiveRisk      string   `json:"effectiveRisk"`
	Runner             struct {
		RunnerID           string `json:"runnerId"`
		Available          bool   `json:"available"`
		Isolated           bool   `json:"isolated"`
		EgressControlled   bool   `json:"egressControlled"`
		NeedsIsolation     bool   `json:"needsIsolation"`
		NeedsEgressControl bool   `json:"needsEgressControl"`
	} `json:"runner"`
	Reasons []string `json:"reasons"`
}

// skillAuditDTO mirrors controllers.SkillAuditResponse.
type skillAuditDTO struct {
	Entries []struct {
		OccurredAt   time.Time `json:"occurredAt"`
		Actor        string    `json:"actor"`
		Action       string    `json:"action"`
		SkillID      string    `json:"skillId"`
		Version      string    `json:"version"`
		ProjectID    string    `json:"projectId"`
		Capabilities []string  `json:"capabilities"`
		Detail       string    `json:"detail"`
	} `json:"entries"`
}

func newSkillsCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Install skills, activate them per project, and check what a run would need",
		Long: "Manage AO's skill catalog.\n\n" +
			"Installing a skill never enables it, and enabling it never runs it. " +
			"Use `ao skills dry-run` to see what a run would need; it starts nothing.",
	}
	cmd.AddCommand(newSkillsListCommand(ctx))
	cmd.AddCommand(newSkillsShowCommand(ctx))
	cmd.AddCommand(newSkillsInstallCommand(ctx))
	cmd.AddCommand(newSkillsUninstallCommand(ctx))
	cmd.AddCommand(newSkillsEnableCommand(ctx))
	cmd.AddCommand(newSkillsDisableCommand(ctx))
	cmd.AddCommand(newSkillsDryRunCommand(ctx))
	cmd.AddCommand(newSkillsRunCommand(ctx))
	cmd.AddCommand(newSkillsAuditCommand(ctx))
	cmd.AddCommand(newSkillImagesCommand(ctx))
	return cmd
}

func newSkillsListCommand(ctx *commandContext) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed skills, or what one project has activated",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.listSkills(cmd, strings.TrimSpace(project))
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Show this project's activations instead of the installation catalog")
	return cmd
}

func (c *commandContext) listSkills(cmd *cobra.Command, project string) error {
	out := cmd.OutOrStdout()
	if project == "" {
		var res skillListDTO
		if err := c.getJSON(cmd.Context(), "skills", &res); err != nil {
			return err
		}
		if len(res.Skills) == 0 {
			_, err := fmt.Fprintln(out, "no skills installed")
			return err
		}
		for _, s := range res.Skills {
			if _, err := fmt.Fprintf(out, "%s@%s  %s  risk=%s  modes=%d\n",
				s.ID, s.Version, s.Name, s.RiskLevel, len(s.Modes)); err != nil {
				return err
			}
		}
		return nil
	}

	var res projectSkillsDTO
	if err := c.getJSON(cmd.Context(), "projects/"+url.PathEscape(project)+"/skills", &res); err != nil {
		return err
	}
	if len(res.Activations) == 0 {
		_, err := fmt.Fprintf(out, "no skills activated for %s (%d installed)\n", project, len(res.Installed))
		return err
	}
	for _, a := range res.Activations {
		state := "disabled"
		if a.Enabled {
			state = "enabled"
		}
		if a.Enabled && !a.Available {
			state = "enabled (unavailable)"
		}
		if _, err := fmt.Fprintf(out, "%s@%s  %s  granted=[%s]\n",
			a.SkillID, a.Version, state, strings.Join(a.GrantedCapabilities, " ")); err != nil {
			return err
		}
		if a.Unavailable != "" {
			if _, err := fmt.Fprintf(out, "  ! %s\n", a.Unavailable); err != nil {
				return err
			}
		}
	}
	return nil
}

func newSkillsShowCommand(ctx *commandContext) *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "show <skill-id>",
		Short: "Show one installed version: origin, capabilities, modes and digest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.showSkill(cmd, args[0], strings.TrimSpace(version))
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Version to show (required)")
	return cmd
}

func (c *commandContext) showSkill(cmd *cobra.Command, id, version string) error {
	if version == "" {
		return usageError{errors.New("usage: --version is required")}
	}
	var s skillInstallDTO
	path := "skills/" + url.PathEscape(id) + "/versions/" + url.PathEscape(version)
	if err := c.getJSON(cmd.Context(), path, &s); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	origin := s.OriginType
	if s.OriginRef != "" {
		origin += " " + s.OriginRef
	}
	lines := []string{
		fmt.Sprintf("%s@%s  %s", s.ID, s.Version, s.Name),
		s.Description,
		fmt.Sprintf("risk:       %s", s.RiskLevel),
		fmt.Sprintf("origin:     %s", origin),
		fmt.Sprintf("publisher:  %s", s.Publisher),
		fmt.Sprintf("digest:     %s", s.Digest),
		fmt.Sprintf("approval:   %s", s.Approval),
		fmt.Sprintf("installed:  %s by %s", s.InstalledAt.Format(time.RFC3339), orNone(s.InstalledBy)),
		fmt.Sprintf("capabilities: %s", strings.Join(s.Capabilities, " ")),
	}
	if s.RequiresIsolatedRunner {
		lines = append(lines, "requires an isolated runner: AO has none, so its containment-needing modes are blocked")
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, "modes:"); err != nil {
		return err
	}
	for _, m := range s.Modes {
		if _, err := fmt.Fprintf(out, "  %-18s risk=%-8s approval=%-14s [%s]\n",
			m.ID, m.RiskLevel, m.Approval, strings.Join(m.Capabilities, " ")); err != nil {
			return err
		}
	}
	return nil
}

func newSkillsInstallCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install <source-dir>",
		Short: "Install a skill package from a local directory",
		Long: "Install a skill package from an absolute directory containing skill.yaml.\n\n" +
			"The manifest is validated and the content digest verified before anything is " +
			"recorded. Installing enables the skill on no project.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.installSkill(cmd, args[0])
		},
	}
	return cmd
}

func (c *commandContext) installSkill(cmd *cobra.Command, source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return usageError{errors.New("usage: a source directory is required")}
	}
	// Resolve locally so a relative path works from the shell, while the
	// daemon keeps requiring an absolute one (it may not share this cwd).
	abs, err := filepath.Abs(source)
	if err != nil {
		return usageError{fmt.Errorf("usage: resolve %q: %w", source, err)}
	}
	var res skillInstallDTO
	if err := c.postJSON(cmd.Context(), "skills", installSkillRequest{SourceDir: abs}, &res); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"installed %s@%s (%s)\nenabled on no project; use `ao skills enable` to activate it\n",
		res.ID, res.Version, res.Digest)
	return err
}

func newSkillsUninstallCommand(ctx *commandContext) *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "uninstall <skill-id>",
		Short: "Remove one installed version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.uninstallSkill(cmd, args[0], strings.TrimSpace(version))
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Version to remove (required)")
	return cmd
}

func (c *commandContext) uninstallSkill(cmd *cobra.Command, id, version string) error {
	if version == "" {
		return usageError{errors.New("usage: --version is required")}
	}
	path := "skills/" + url.PathEscape(id) + "/versions/" + url.PathEscape(version)
	if err := c.deleteJSON(cmd.Context(), path, nil); err != nil {
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "uninstalled %s@%s\n", id, version)
	return err
}

func newSkillsEnableCommand(ctx *commandContext) *cobra.Command {
	var (
		project string
		version string
		caps    []string
	)
	cmd := &cobra.Command{
		Use:   "enable <skill-id>",
		Short: "Enable an installed skill on one project with an explicit capability grant",
		Long: "Enable an installed skill on one project.\n\n" +
			"The activation is pinned to one version and carries exactly the capabilities " +
			"you grant. A grant can never exceed your own permissions on the project, and " +
			"enabling runs nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.enableSkill(cmd, args[0], strings.TrimSpace(project), strings.TrimSpace(version), caps)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project to activate the skill on (required)")
	cmd.Flags().StringVar(&version, "version", "", "Version to pin (required)")
	cmd.Flags().StringArrayVar(&caps, "capability", nil, "A capability to grant. Repeatable; required")
	return cmd
}

func (c *commandContext) enableSkill(cmd *cobra.Command, id, project, version string, caps []string) error {
	switch {
	case project == "":
		return usageError{errors.New("usage: --project is required")}
	case version == "":
		return usageError{errors.New("usage: --version is required; an activation is always pinned")}
	case len(caps) == 0:
		return usageError{errors.New("usage: at least one --capability is required; " +
			"a grant of nothing would activate a skill that can do nothing")}
	}
	granted := make([]string, 0, len(caps))
	for _, cap := range caps {
		for _, part := range strings.Split(cap, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				granted = append(granted, trimmed)
			}
		}
	}
	path := "projects/" + url.PathEscape(project) + "/skills/" + url.PathEscape(id)
	var res skillActivationDTO
	if err := c.putJSON(cmd.Context(), path,
		enableSkillRequest{Version: version, Capabilities: granted}, &res); err != nil {
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"enabled %s@%s on %s with [%s]\nnothing runs until you ask it to; `ao skills dry-run` reports what a run would need\n",
		res.SkillID, res.Version, project, strings.Join(res.GrantedCapabilities, " "))
	return err
}

func newSkillsDisableCommand(ctx *commandContext) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "disable <skill-id>",
		Short: "Disable a skill on one project and revoke its grant",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.disableSkill(cmd, args[0], strings.TrimSpace(project))
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project to deactivate the skill on (required)")
	return cmd
}

func (c *commandContext) disableSkill(cmd *cobra.Command, id, project string) error {
	if project == "" {
		return usageError{errors.New("usage: --project is required")}
	}
	path := "projects/" + url.PathEscape(project) + "/skills/" + url.PathEscape(id)
	if err := c.deleteJSON(cmd.Context(), path, nil); err != nil {
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"disabled %s on %s; its grant is revoked, so re-enabling means granting again\n", id, project)
	return err
}

func newSkillsDryRunCommand(ctx *commandContext) *cobra.Command {
	var (
		project string
		mode    string
		inputs  []string
		targets []string
	)
	cmd := &cobra.Command{
		Use:   "dry-run <skill-id>",
		Short: "Report what a run would need, without running anything",
		Long: "Report what running one mode of a skill would need on one project.\n\n" +
			"This starts no process, opens no socket and changes nothing. It reports the " +
			"capabilities the mode requests, which are satisfied, which permissions you are " +
			"missing, what approval is outstanding, and what the runner does and does not provide.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.drySkillRun(cmd, args[0], strings.TrimSpace(project), strings.TrimSpace(mode), inputs, targets)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project to plan against (required)")
	cmd.Flags().StringVar(&mode, "mode", "", "Mode to plan. Optional only when the skill declares exactly one")
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "An input as key=value. Repeatable")
	cmd.Flags().StringArrayVar(&targets, "target", nil, "A target explicitly authorized for this run. Repeatable")
	return cmd
}

func (c *commandContext) drySkillRun(cmd *cobra.Command, id, project, mode string, inputs, targets []string) error {
	if project == "" {
		return usageError{errors.New("usage: --project is required")}
	}
	parsed := map[string]string{}
	for _, in := range inputs {
		key, value, ok := strings.Cut(in, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return usageError{fmt.Errorf("usage: --input %q must be key=value", in)}
		}
		parsed[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if mode != "" && parsed["mode"] == "" {
		// The example package takes the mode as an input as well as a path
		// segment; filling it saves every caller from passing it twice.
		parsed["mode"] = mode
	}
	path := "projects/" + url.PathEscape(project) + "/skills/" + url.PathEscape(id) + "/dry-run"
	var res skillDryRunDTO
	if err := c.postJSON(cmd.Context(), path, skillDryRunRequest{
		ModeID: mode, Inputs: parsed, AuthorizedTargets: targets,
	}, &res); err != nil {
		return err
	}
	return writeSkillDryRun(cmd.OutOrStdout(), res)
}

func writeSkillDryRun(out io.Writer, res skillDryRunDTO) error {
	verdict := map[string]string{
		"executable":        "EXECUTABLE",
		"requires_approval": "REQUIRES APPROVAL",
		"blocked":           "BLOCKED",
	}[res.Verdict]
	if verdict == "" {
		verdict = strings.ToUpper(res.Verdict)
	}
	if _, err := fmt.Fprintf(out, "%s  %s@%s  mode=%s (%s)\n",
		verdict, res.SkillID, res.Version, res.ModeID, res.ModeRisk); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "approval required: %s\n", res.RequiredApproval); err != nil {
		return err
	}
	for _, d := range res.Decisions {
		mark := "ok "
		note := d.Description
		if !d.Satisfied {
			mark = "NO "
			note = d.Detail
		}
		if _, err := fmt.Fprintf(out, "  %s %-18s risk=%-8s %s\n", mark, d.Capability, d.Risk, note); err != nil {
			return err
		}
	}
	if len(res.MissingPermissions) > 0 {
		missing := append([]string(nil), res.MissingPermissions...)
		sort.Strings(missing)
		if _, err := fmt.Fprintf(out, "missing permissions: %s\n", strings.Join(missing, " ")); err != nil {
			return err
		}
	}
	// The runner line is the honest part: say what AO has, not what the mode
	// wishes it had.
	if res.Runner.NeedsIsolation || res.Runner.NeedsEgressControl {
		need := []string{}
		if res.Runner.NeedsIsolation {
			need = append(need, "isolation")
		}
		if res.Runner.NeedsEgressControl {
			need = append(need, "egress control")
		}
		if _, err := fmt.Fprintf(out, "runner: %s (isolated=%t egress-controlled=%t); this mode needs %s\n",
			res.Runner.RunnerID, res.Runner.Isolated, res.Runner.EgressControlled,
			strings.Join(need, " and ")); err != nil {
			return err
		}
	}
	for _, reason := range res.Reasons {
		if _, err := fmt.Fprintf(out, "blocked: %s\n", reason); err != nil {
			return err
		}
	}
	return nil
}

func newSkillsAuditCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit <skill-id>",
		Short: "Show who installed, enabled or re-granted a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.skillAudit(cmd, args[0])
		},
	}
	return cmd
}

func (c *commandContext) skillAudit(cmd *cobra.Command, id string) error {
	var res skillAuditDTO
	if err := c.getJSON(cmd.Context(), "skills/"+url.PathEscape(id)+"/audit", &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(res.Entries) == 0 {
		_, err := fmt.Fprintf(out, "no catalog history for %s\n", id)
		return err
	}
	for _, e := range res.Entries {
		scope := e.ProjectID
		if scope == "" {
			scope = "(installation)"
		}
		line := fmt.Sprintf("%s  %-16s %-12s %s by %s",
			e.OccurredAt.Format(time.RFC3339), e.Action, scope, e.Version, orNone(e.Actor))
		if len(e.Capabilities) > 0 {
			line += "  [" + strings.Join(e.Capabilities, " ") + "]"
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
		if e.Detail != "" {
			if _, err := fmt.Fprintf(out, "  %s\n", e.Detail); err != nil {
				return err
			}
		}
	}
	return nil
}

// orNone renders the unauthenticated loopback caller, which records an empty
// actor, as something a reader can tell apart from a missing field.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(local)"
	}
	return s
}
