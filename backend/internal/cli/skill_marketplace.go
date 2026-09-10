package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// skill_marketplace.go — `ao skills marketplace` and `ao skills registry`.
//
// It installs and it configures. It runs nothing, and there is deliberately no
// verb here that would: installing moves a package exactly one step, from
// AVAILABLE to INSTALLED, and reaching a project still means `ao skills enable`
// under a different permission.
//
// The DTOs below are hand-mirrored from httpd/controllers, which is the
// deliberate manual boundary AGENTS.md keeps between the CLI and the HTTP
// controller package.

// skillRegistryDTO mirrors controllers.SkillRegistryView.
type skillRegistryDTO struct {
	ID                     string `json:"id"`
	DisplayName            string `json:"displayName"`
	Type                   string `json:"type"`
	Location               string `json:"location"`
	Enabled                bool   `json:"enabled"`
	TrustPolicy            string `json:"trustPolicy"`
	TrustPolicyEnforceable bool   `json:"trustPolicyEnforceable"`
	PinnedPublisher        string `json:"pinnedPublisher"`
	Priority               int    `json:"priority"`
	TenantID               string `json:"tenantId"`
	CredentialSecretName   string `json:"credentialSecretName"`
}

// skillRegistryListDTO mirrors controllers.SkillRegistryListResponse.
type skillRegistryListDTO struct {
	Registries []skillRegistryDTO `json:"registries"`
	TrustModel string             `json:"trustModel"`
}

// saveSkillRegistryRequest mirrors controllers.SaveSkillRegistryRequest.
type saveSkillRegistryRequest struct {
	DisplayName          string `json:"displayName"`
	Type                 string `json:"type"`
	Location             string `json:"location"`
	Enabled              bool   `json:"enabled"`
	TrustPolicy          string `json:"trustPolicy"`
	PinnedPublisher      string `json:"pinnedPublisher,omitempty"`
	Priority             int    `json:"priority,omitempty"`
	TenantID             string `json:"tenantId,omitempty"`
	CredentialSecretName string `json:"credentialSecretName,omitempty"`
}

// skillReleaseDTO mirrors controllers.SkillReleaseView.
type skillReleaseDTO struct {
	RegistryID   string `json:"registryId"`
	RegistryName string `json:"registryName"`
	SkillID      string `json:"skillId"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Publisher    string `json:"publisher"`
	Description  string `json:"description"`
	RiskLevel    string `json:"riskLevel"`
	SourceURL    string `json:"sourceUrl"`

	ManifestDigest  string `json:"manifestDigest"`
	ArtifactDigest  string `json:"artifactDigest"`
	SignatureFormat string `json:"signatureFormat"`
	KeyID           string `json:"keyId"`
	AttestationURL  string `json:"attestationUrl"`

	RequestedCapabilities []string `json:"requestedCapabilities"`
	ExecutionModes        []struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		RiskLevel    string   `json:"riskLevel"`
		Capabilities []string `json:"capabilities"`
	} `json:"executionModes"`

	AOMinVersion  string `json:"aoMinVersion"`
	AOMaxVersion  string `json:"aoMaxVersion"`
	Compatibility string `json:"compatibility"`

	PublishedAt      time.Time `json:"publishedAt"`
	Deprecated       bool      `json:"deprecated"`
	DeprecationNote  string    `json:"deprecationNote"`
	Revoked          bool      `json:"revoked"`
	RevocationReason string    `json:"revocationReason"`

	Trust            string `json:"trust"`
	TrustExplanation string `json:"trustExplanation"`

	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installedVersion"`
	UpdateAvailable  bool   `json:"updateAvailable"`
}

// skillMarketplaceSearchDTO mirrors controllers.SkillMarketplaceSearchResponse.
type skillMarketplaceSearchDTO struct {
	Releases []skillReleaseDTO `json:"releases"`
	Notes    []struct {
		RegistryID string `json:"registryId"`
		Reason     string `json:"reason"`
	} `json:"notes"`
	InstallNotice string `json:"installNotice"`
}

// skillReleaseDetailDTO mirrors controllers.SkillReleaseDetailResponse.
type skillReleaseDetailDTO struct {
	Release       skillReleaseDTO   `json:"release"`
	Versions      []skillReleaseDTO `json:"versions"`
	InstallNotice string            `json:"installNotice"`
}

// installSkillReleaseRequest mirrors controllers.InstallSkillReleaseRequest.
type installSkillReleaseRequest struct {
	RegistryID string `json:"registryId"`
	SkillID    string `json:"skillId"`
	Version    string `json:"version"`
	AsUpdate   bool   `json:"asUpdate,omitempty"`
}

// skillInstallOriginDTO mirrors controllers.SkillInstallOriginView.
type skillInstallOriginDTO struct {
	SkillID          string `json:"skillId"`
	Version          string `json:"version"`
	RegistryID       string `json:"registryId"`
	RegistryName     string `json:"registryName"`
	Publisher        string `json:"publisher"`
	ArtifactDigest   string `json:"artifactDigest"`
	Trust            string `json:"trust"`
	TrustExplanation string `json:"trustExplanation"`
	Compatibility    string `json:"compatibility"`
	Revoked          bool   `json:"revoked"`
	RevocationReason string `json:"revocationReason"`
}

// skillInstallOutcomeDTO mirrors controllers.SkillInstallOutcomeView.
type skillInstallOutcomeDTO struct {
	Install  skillInstallDTO       `json:"install"`
	Origin   skillInstallOriginDTO `json:"origin"`
	Updated  bool                  `json:"updated"`
	NextStep string                `json:"nextStep"`
}

// skillUpdateCheckDTO mirrors controllers.SkillUpdateCheckResponse.
type skillUpdateCheckDTO struct {
	Statuses []struct {
		SkillID          string                `json:"skillId"`
		Version          string                `json:"version"`
		Origin           skillInstallOriginDTO `json:"origin"`
		LatestVersion    string                `json:"latestVersion"`
		UpdateAvailable  bool                  `json:"updateAvailable"`
		RevokedNow       bool                  `json:"revokedNow"`
		RevocationReason string                `json:"revocationReason"`
		Unreachable      string                `json:"unreachable"`
	} `json:"statuses"`
	RevocationPolicy string `json:"revocationPolicy"`
}

// ------------------------------------------------------------------ commands

func newSkillRegistryCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Configure which registries this installation may install skills from",
		Long: "AO ships with no registry configured and no default endpoint, so an installation " +
			"that configures nothing can install nothing from one.\n\n" +
			"Configuring a registry requires settings.manage. It does not install anything.",
	}
	cmd.AddCommand(newSkillRegistryListCommand(ctx))
	cmd.AddCommand(newSkillRegistryAddCommand(ctx))
	cmd.AddCommand(newSkillRegistryRemoveCommand(ctx))
	return cmd
}

func newSkillRegistryListCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured skill registries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.listSkillRegistries(cmd)
		},
	}
}

func (c *commandContext) listSkillRegistries(cmd *cobra.Command) error {
	var res skillRegistryListDTO
	if err := c.getJSON(cmd.Context(), "skills/registries", &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(res.Registries) == 0 {
		// An empty list says what that MEANS. "No registries" alone reads like
		// a setup step nobody got to.
		_, err := fmt.Fprintln(out, "no registries configured; nothing can be installed from one")
		return err
	}
	for _, reg := range res.Registries {
		state := "enabled"
		if !reg.Enabled {
			state = "disabled"
		}
		if _, err := fmt.Fprintf(out, "%-20s %-10s %-8s priority=%-4d %s\n",
			reg.ID, reg.Type, state, reg.Priority, reg.Location); err != nil {
			return err
		}
		policy := "  trust policy: " + reg.TrustPolicy
		if reg.PinnedPublisher != "" {
			policy += " (publisher " + reg.PinnedPublisher + ")"
		}
		if !reg.TrustPolicyEnforceable {
			// The strictest-looking setting must not read as if it were
			// working. It installs nothing.
			policy += " — NOT ENFORCEABLE in this build; nothing can be installed from this registry"
		}
		if _, err := fmt.Fprintln(out, policy); err != nil {
			return err
		}
		if reg.TenantID != "" {
			if _, err := fmt.Fprintf(out, "  tenant: %s\n", reg.TenantID); err != nil {
				return err
			}
		}
	}
	if res.TrustModel != "" {
		if _, err := fmt.Fprintf(out, "\n%s\n", res.TrustModel); err != nil {
			return err
		}
	}
	return nil
}

func newSkillRegistryAddCommand(ctx *commandContext) *cobra.Command {
	var (
		name        string
		regType     string
		location    string
		trustPolicy string
		publisher   string
		priority    int
		tenant      string
		credential  string
		disabled    bool
	)
	cmd := &cobra.Command{
		Use:   "add <registry-id>",
		Short: "Add or update one registry configuration",
		Long: "Add or update a registry. AO opens it before recording it: a registry it cannot " +
			"read is refused, because one that answers every search with silence reads as " +
			"\"this registry has nothing\".\n\n" +
			"--credential-secret names a SEALED SECRET, never a value.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.saveSkillRegistry(cmd, args[0], saveSkillRegistryRequest{
				DisplayName:          strings.TrimSpace(name),
				Type:                 strings.TrimSpace(regType),
				Location:             strings.TrimSpace(location),
				Enabled:              !disabled,
				TrustPolicy:          strings.TrimSpace(trustPolicy),
				PinnedPublisher:      strings.TrimSpace(publisher),
				Priority:             priority,
				TenantID:             strings.TrimSpace(tenant),
				CredentialSecretName: strings.TrimSpace(credential),
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name (required)")
	cmd.Flags().StringVar(&regType, "type", "local", "Registry type: local (https and git are declared but not implemented)")
	cmd.Flags().StringVar(&location, "location", "", "Absolute directory holding registry.json (required)")
	cmd.Flags().StringVar(&trustPolicy, "trust-policy", "digest",
		"What this registry must satisfy beyond integrity: digest, pinned_publisher, or signed (signed installs nothing; AO verifies no signature)")
	cmd.Flags().StringVar(&publisher, "pinned-publisher", "", "Required by --trust-policy pinned_publisher")
	cmd.Flags().IntVar(&priority, "priority", 100, "Display ordering when two registries offer the same skill")
	cmd.Flags().StringVar(&tenant, "tenant", "", "Limit this registry to one organization")
	cmd.Flags().StringVar(&credential, "credential-secret", "", "NAME of a sealed secret holding this registry's credential, never a value")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "Record the registry without letting it answer searches or serve installs")
	return cmd
}

func (c *commandContext) saveSkillRegistry(cmd *cobra.Command, id string, req saveSkillRegistryRequest) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return usageError{errors.New("usage: a registry id is required")}
	}
	if req.DisplayName == "" {
		return usageError{errors.New("usage: --name is required")}
	}
	if req.Location == "" {
		return usageError{errors.New("usage: --location is required")}
	}
	var res skillRegistryDTO
	if err := c.putJSON(cmd.Context(), "skills/registries/"+url.PathEscape(id), req, &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "registry %s (%s) at %s\n", res.ID, res.Type, res.Location); err != nil {
		return err
	}
	if !res.TrustPolicyEnforceable {
		if _, err := fmt.Fprintf(out,
			"WARNING: trust policy %q is not enforceable in this build; nothing can be installed from this registry\n",
			res.TrustPolicy); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out, "nothing was installed; use `ao skills marketplace search` to look")
	return err
}

func newSkillRegistryRemoveCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <registry-id>",
		Short: "Remove one registry configuration, keeping every package installed from it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			if id == "" {
				return usageError{errors.New("usage: a registry id is required")}
			}
			return ctx.deleteRegistry(cmd, id)
		},
	}
}

func (c *commandContext) deleteRegistry(cmd *cobra.Command, id string) error {
	if err := c.deleteJSON(cmd.Context(), "skills/registries/"+url.PathEscape(id), nil); err != nil {
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"removed registry %s\ninstalled packages and their recorded provenance were kept\n", id)
	return err
}

func newSkillsMarketplaceCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "marketplace",
		Short: "Search configured registries and install one exact release",
		Long: "Search, review and install skills from a configured registry.\n\n" +
			"Searching downloads no package code. Installing verifies INTEGRITY — AO fetches the " +
			"package, computes both digests itself and refuses anything that is not the release " +
			"it resolved — which is not the same as knowing who wrote it.\n\n" +
			"Installing enables the skill on no project. Use `ao skills enable` for that, and " +
			"`ao skills dry-run` to see what a run would need.",
	}
	cmd.AddCommand(newSkillsSearchCommand(ctx))
	cmd.AddCommand(newSkillsReleaseCommand(ctx))
	cmd.AddCommand(newSkillsInstallReleaseCommand(ctx))
	cmd.AddCommand(newSkillsUpdatesCommand(ctx))
	return cmd
}

func newSkillsSearchCommand(ctx *commandContext) *cobra.Command {
	var (
		registry          string
		publisher         string
		capability        string
		includeDeprecated bool
		includeRevoked    bool
		limit             int
	)
	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search every enabled registry you can see",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) == 1 {
				query = strings.TrimSpace(args[0])
			}
			params := url.Values{}
			addIf(params, "q", query)
			addIf(params, "registryId", strings.TrimSpace(registry))
			addIf(params, "publisher", strings.TrimSpace(publisher))
			addIf(params, "capability", strings.TrimSpace(capability))
			if includeDeprecated {
				params.Set("includeDeprecated", "true")
			}
			if includeRevoked {
				params.Set("includeRevoked", "true")
			}
			if limit > 0 {
				params.Set("limit", strconv.Itoa(limit))
			}
			return ctx.searchMarketplace(cmd, params)
		},
	}
	cmd.Flags().StringVar(&registry, "registry", "", "Search only this registry")
	cmd.Flags().StringVar(&publisher, "publisher", "", "Exact publisher match")
	cmd.Flags().StringVar(&capability, "capability", "", "Keep only releases requesting this AO capability")
	cmd.Flags().BoolVar(&includeDeprecated, "include-deprecated", false, "Include superseded releases")
	cmd.Flags().BoolVar(&includeRevoked, "include-revoked", false, "Include withdrawn releases; they can never be installed")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum releases to return")
	return cmd
}

func addIf(params url.Values, key, value string) {
	if value != "" {
		params.Set(key, value)
	}
}

func (c *commandContext) searchMarketplace(cmd *cobra.Command, params url.Values) error {
	path := "skills/marketplace"
	if encoded := params.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var res skillMarketplaceSearchDTO
	if err := c.getJSON(cmd.Context(), path, &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, rel := range res.Releases {
		state := "not installed"
		switch {
		case rel.Installed:
			state = "installed"
		case rel.UpdateAvailable:
			state = "update from " + rel.InstalledVersion
		}
		if _, err := fmt.Fprintf(out, "%-24s %-10s %-16s %-16s %s\n",
			rel.SkillID, rel.Version, rel.Publisher, state, rel.RegistryID); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "  %s\n", rel.Description); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "  risk=%s capabilities=[%s] compatibility=%s trust=%s\n",
			rel.RiskLevel, strings.Join(rel.RequestedCapabilities, " "),
			rel.Compatibility, rel.Trust); err != nil {
			return err
		}
		if rel.Revoked {
			if _, err := fmt.Fprintf(out, "  REVOKED: %s\n", rel.RevocationReason); err != nil {
				return err
			}
		}
		if rel.Deprecated {
			if _, err := fmt.Fprintf(out, "  deprecated: %s\n", orNone(rel.DeprecationNote)); err != nil {
				return err
			}
		}
	}
	if len(res.Releases) == 0 {
		if _, err := fmt.Fprintln(out, "no matching releases"); err != nil {
			return err
		}
	}
	// A registry AO could not read is NAMED. An empty result and an unreadable
	// registry are different answers, and only one means somebody should go
	// and look.
	for _, note := range res.Notes {
		if _, err := fmt.Fprintf(out, "\nregistry %s could not be read: %s\n",
			note.RegistryID, note.Reason); err != nil {
			return err
		}
	}
	if res.InstallNotice != "" {
		if _, err := fmt.Fprintf(out, "\n%s\n", res.InstallNotice); err != nil {
			return err
		}
	}
	return nil
}

func newSkillsReleaseCommand(ctx *commandContext) *cobra.Command {
	var registry, version string
	cmd := &cobra.Command{
		Use:   "show <skill-id>",
		Short: "Show one release: publisher, capabilities, modes, digests and every version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(registry) == "" {
				return usageError{errors.New("usage: --registry is required")}
			}
			return ctx.showRelease(cmd, strings.TrimSpace(registry), args[0], strings.TrimSpace(version))
		},
	}
	cmd.Flags().StringVar(&registry, "registry", "", "Registry to read (required)")
	cmd.Flags().StringVar(&version, "version", "", "Version to feature (default: the newest offered)")
	return cmd
}

func (c *commandContext) showRelease(cmd *cobra.Command, registry, skillID, version string) error {
	path := "skills/marketplace/" + url.PathEscape(registry) + "/" + url.PathEscape(skillID)
	if version != "" {
		path += "?version=" + url.QueryEscape(version)
	}
	var res skillReleaseDetailDTO
	if err := c.getJSON(cmd.Context(), path, &res); err != nil {
		return err
	}
	return writeSkillRelease(cmd.OutOrStdout(), res)
}

func writeSkillRelease(out io.Writer, res skillReleaseDetailDTO) error {
	rel := res.Release
	if _, err := fmt.Fprintf(out, "%s@%s  %s\n", rel.SkillID, rel.Version, rel.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  %s\n", rel.Description); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  registry:      %s (%s)\n",
		rel.RegistryID, orNone(rel.RegistryName)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  publisher:     %s\n", rel.Publisher); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  risk:          %s\n", rel.RiskLevel); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  capabilities:  %s\n",
		orNone(strings.Join(rel.RequestedCapabilities, " "))); err != nil {
		return err
	}
	for _, mode := range rel.ExecutionModes {
		if _, err := fmt.Fprintf(out, "    mode %-16s risk=%-8s [%s]\n",
			mode.ID, mode.RiskLevel, strings.Join(mode.Capabilities, " ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "  compatibility: %s (needs AO >= %s%s)\n",
		rel.Compatibility, rel.AOMinVersion, maxVersionSuffix(rel.AOMaxVersion)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  manifest:      %s\n", rel.ManifestDigest); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  artifact:      %s\n", rel.ArtifactDigest); err != nil {
		return err
	}
	// The declared provenance, marked as a claim. AO validates none of it, and
	// a line that just printed "signature: present" would read as a check.
	if rel.SignatureFormat != "" || rel.KeyID != "" {
		if _, err := fmt.Fprintf(out, "  signature:     %s key=%s (CLAIMED; AO verifies no signature)\n",
			orNone(rel.SignatureFormat), orNone(rel.KeyID)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "  trust:         %s\n", rel.Trust); err != nil {
		return err
	}
	if rel.TrustExplanation != "" {
		if _, err := fmt.Fprintf(out, "                 %s\n", rel.TrustExplanation); err != nil {
			return err
		}
	}
	if rel.Revoked {
		if _, err := fmt.Fprintf(out, "  REVOKED:       %s\n", rel.RevocationReason); err != nil {
			return err
		}
	}
	if len(res.Versions) > 0 {
		if _, err := fmt.Fprintln(out, "\nversions:"); err != nil {
			return err
		}
		for _, v := range res.Versions {
			marker := " "
			if v.Installed {
				marker = "*"
			}
			state := ""
			switch {
			case v.Revoked:
				state = "  REVOKED: " + v.RevocationReason
			case v.Deprecated:
				state = "  deprecated"
			}
			if _, err := fmt.Fprintf(out, " %s %-12s %s%s\n",
				marker, v.Version, v.PublishedAt.Format("2006-01-02"), state); err != nil {
				return err
			}
		}
	}
	if res.InstallNotice != "" {
		if _, err := fmt.Fprintf(out, "\n%s\n", res.InstallNotice); err != nil {
			return err
		}
	}
	return nil
}

func maxVersionSuffix(maxVersion string) string {
	if maxVersion == "" {
		return ""
	}
	return " <= " + maxVersion
}

func newSkillsInstallReleaseCommand(ctx *commandContext) *cobra.Command {
	var registry, version string
	var asUpdate bool
	cmd := &cobra.Command{
		Use:   "install <skill-id>",
		Short: "Install one exact release from a configured registry",
		Long: "Install one exact release. A version is required and exact — there is no \"latest\".\n\n" +
			"AO re-resolves the release, refuses it if the registry has revoked it, checks the " +
			"registry's trust policy and the AO version range, fetches the package into a " +
			"quarantine directory and verifies both digests plus the manifest's agreement with " +
			"the listing over the bytes that landed, before anything reaches the catalog.\n\n" +
			"It enables the skill on no project, grants no capability and runs nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(registry) == "" {
				return usageError{errors.New("usage: --registry is required")}
			}
			if strings.TrimSpace(version) == "" {
				return usageError{errors.New("usage: --version is required; there is no latest to install")}
			}
			return ctx.installRelease(cmd, installSkillReleaseRequest{
				RegistryID: strings.TrimSpace(registry),
				SkillID:    strings.TrimSpace(args[0]),
				Version:    strings.TrimSpace(version),
				AsUpdate:   asUpdate,
			})
		},
	}
	cmd.Flags().StringVar(&registry, "registry", "", "Registry to install from (required)")
	cmd.Flags().StringVar(&version, "version", "", "Exact version to install (required)")
	cmd.Flags().BoolVar(&asUpdate, "update", false,
		"Refuse anything that is not strictly newer than the installed version")
	return cmd
}

func (c *commandContext) installRelease(cmd *cobra.Command, req installSkillReleaseRequest) error {
	var res skillInstallOutcomeDTO
	if err := c.postJSON(cmd.Context(), "skills/marketplace/install", req, &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	verb := "installed"
	if res.Updated {
		verb = "updated to"
	}
	if _, err := fmt.Fprintf(out, "%s %s@%s from registry %s\n",
		verb, res.Install.ID, res.Install.Version, res.Origin.RegistryID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  publisher: %s\n", res.Origin.Publisher); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  artifact:  %s\n", res.Origin.ArtifactDigest); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  trust:     %s\n", res.Origin.Trust); err != nil {
		return err
	}
	if res.Origin.TrustExplanation != "" {
		if _, err := fmt.Fprintf(out, "             %s\n", res.Origin.TrustExplanation); err != nil {
			return err
		}
	}
	// The compatibility check not RUNNING is a different fact from it passing,
	// and a source build is the ordinary case where it did not run.
	if res.Origin.Compatibility == "unknown" {
		if _, err := fmt.Fprintln(out,
			"  note:      this build reports no version, so the AO compatibility check did not run"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "\n%s\n", orNone(res.NextStep))
	return err
}

func newSkillsUpdatesCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "updates",
		Short: "Ask each installed release's registry what it says now",
		Long: "AO polls nothing on its own; this is a request you made.\n\n" +
			"It reports a newer installable version where one exists, names a registry it could " +
			"not reach, and marks any installed release the registry has since REVOKED. A " +
			"revocation blocks new installs and nothing else.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.checkSkillUpdates(cmd)
		},
	}
}

func (c *commandContext) checkSkillUpdates(cmd *cobra.Command) error {
	var res skillUpdateCheckDTO
	if err := c.getJSON(cmd.Context(), "skills/updates", &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(res.Statuses) == 0 {
		_, err := fmt.Fprintln(out, "no skills were installed from a registry")
		return err
	}
	anyRevoked := false
	for _, s := range res.Statuses {
		line := fmt.Sprintf("%-24s %-10s registry=%s", s.SkillID, s.Version, s.Origin.RegistryID)
		switch {
		case s.UpdateAvailable:
			line += "  update available: " + s.LatestVersion
		case s.Unreachable != "":
			line += "  " + s.Unreachable
		default:
			line += "  up to date"
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
		if s.RevokedNow {
			anyRevoked = true
			if _, err := fmt.Fprintf(out, "  REVOKED by the registry: %s\n",
				orNone(s.RevocationReason)); err != nil {
				return err
			}
		}
	}
	if anyRevoked && res.RevocationPolicy != "" {
		if _, err := fmt.Fprintf(out, "\n%s\n", res.RevocationPolicy); err != nil {
			return err
		}
	}
	return nil
}
