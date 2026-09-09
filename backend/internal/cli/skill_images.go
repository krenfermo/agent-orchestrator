package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// skill_images.go — the administrative surface for AO's image trust root.
//
// The routes have existed since phase 8; until now the only way to reach them
// was to compose the HTTP request by hand, which meant the one decision that
// authorizes AO to execute container bytes was the least approachable thing in
// the product. That is the wrong way round.
//
// What these commands do NOT do is as load-bearing as what they do. Approving
// runs nothing, pulls nothing and enables no skill: it records that a named
// administrator looked at exact bytes and said this scope may execute them.
// The daemon additionally refuses to record an approval for a digest this host
// cannot show it, so `approve` can fail on a digest that looks perfectly well
// formed — that refusal is the check working.

// skillImageApprovalDTO mirrors controllers.SkillImageApprovalView. The CLI
// hand-mirrors DTOs rather than importing the controller package; that boundary
// is deliberate and predates this file.
type skillImageApprovalDTO struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenantId"`
	ProjectID      string `json:"projectId"`
	SkillID        string `json:"skillId"`
	Version        string `json:"version"`
	ModeID         string `json:"modeId"`
	Tool           string `json:"tool"`
	Reference      string `json:"reference"`
	Digest         string `json:"digest"`
	ApprovedBy     string `json:"approvedBy"`
	ApprovedAt     string `json:"approvedAt"`
	ExpiresAt      string `json:"expiresAt,omitempty"`
	RevokedAt      string `json:"revokedAt,omitempty"`
	Note           string `json:"note"`
	Active         bool   `json:"active"`
	InactiveReason string `json:"inactiveReason,omitempty"`
}

type skillImageListDTO struct {
	Approvals  []skillImageApprovalDTO `json:"approvals"`
	TrustModel string                  `json:"trustModel"`
	Revocation string                  `json:"revocationPolicy"`
}

type approveSkillImageDTO struct {
	TenantID         string `json:"tenantId"`
	ProjectID        string `json:"projectId"`
	SkillID          string `json:"skillId"`
	Version          string `json:"version"`
	ModeID           string `json:"modeId"`
	Tool             string `json:"tool"`
	Reference        string `json:"reference"`
	Digest           string `json:"digest"`
	ExpiresInSeconds int64  `json:"expiresInSeconds,omitempty"`
	Note             string `json:"note"`
	Confirm          bool   `json:"confirm"`
}

type approveSkillImageResultDTO struct {
	Approval skillImageApprovalDTO `json:"approval"`
}

func newSkillImagesCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Administer which container images this installation may execute",
		Long: "AO's trust root for container images.\n\n" +
			"An approval says a named administrator looked at exact bytes and decided this " +
			"scope may execute them to back one tool. It is NOT a publisher signature: AO " +
			"verifies none.\n\n" +
			"Approving runs nothing and pulls nothing. The daemon refuses to record an " +
			"approval for a digest this host cannot show it, so a well-formed digest that is " +
			"not present here is refused.",
	}
	cmd.AddCommand(newSkillImagesListCommand(ctx))
	cmd.AddCommand(newSkillImagesApproveCommand(ctx))
	cmd.AddCommand(newSkillImagesRevokeCommand(ctx))
	return cmd
}

func newSkillImagesListCommand(ctx *commandContext) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List image approvals, including revoked and expired ones",
		Long: "Revoked and expired approvals are INCLUDED. They are the history of what this " +
			"installation once allowed, and a list that hid them would answer \"what did we " +
			"approve\" with \"what is approved right now\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := "skills/images"
			if p := strings.TrimSpace(project); p != "" {
				path += "?projectId=" + p
			}
			var res skillImageListDTO
			if err := ctx.getJSON(cmd.Context(), path, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.Approvals) == 0 {
				if _, err := fmt.Fprintln(out, "no image approvals; nothing may execute"); err != nil {
					return err
				}
				return nil
			}
			for _, a := range res.Approvals {
				state := "active"
				if !a.Active {
					state = "INACTIVE"
					if a.InactiveReason != "" {
						state += " (" + a.InactiveReason + ")"
					}
				}
				if _, err := fmt.Fprintf(out, "%s  %s\n", a.ID, state); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(out,
					"  scope   %s/%s %s@%s mode=%s tool=%s\n",
					a.TenantID, a.ProjectID, a.SkillID, a.Version, a.ModeID, a.Tool); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(out, "  image   %s (%s)\n", a.Digest, a.Reference); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(out, "  by      %s at %s\n", a.ApprovedBy, a.ApprovedAt); err != nil {
					return err
				}
				if a.ExpiresAt != "" {
					if _, err := fmt.Fprintf(out, "  expires %s\n", a.ExpiresAt); err != nil {
						return err
					}
				}
				if a.RevokedAt != "" {
					if _, err := fmt.Fprintf(out, "  revoked %s\n", a.RevokedAt); err != nil {
						return err
					}
				}
				if a.Note != "" {
					if _, err := fmt.Fprintf(out, "  note    %s\n", a.Note); err != nil {
						return err
					}
				}
			}
			// The non-promises travel with the listing, not only with the docs:
			// this is where somebody decides whether to trust what they see.
			if res.TrustModel != "" {
				if _, err := fmt.Fprintf(out, "\n%s\n", res.TrustModel); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Only approvals scoped to this project")
	return cmd
}

type approveImageOptions struct {
	tenant    string
	project   string
	skill     string
	version   string
	mode      string
	tool      string
	reference string
	digest    string
	note      string
	expiresIn time.Duration
	confirm   bool
}

func newSkillImagesApproveCommand(ctx *commandContext) *cobra.Command {
	var opts approveImageOptions
	cmd := &cobra.Command{
		Use:   "approve",
		Short: "Approve one exact digest for one exact scope",
		Long: "Records that these bytes may be executed by this scope to back this tool.\n\n" +
			"Every scope field is required: an approval missing one of them authorizes " +
			"somebody, somewhere, to run something. A newer package version is a different " +
			"manifest asking for different capabilities, so it has to be approved again.\n\n" +
			"--digest must be sha256:<64 hex>. A tag is refused because it is a mutable " +
			"pointer, and \"the image we approved\" has to mean one thing forever.\n\n" +
			"This does not pull, does not run, and does not enable a skill.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.validate(); err != nil {
				return err
			}
			body := approveSkillImageDTO{
				TenantID: opts.tenant, ProjectID: opts.project, SkillID: opts.skill,
				Version: opts.version, ModeID: opts.mode, Tool: opts.tool,
				Reference: opts.reference, Digest: opts.digest,
				Note: opts.note, Confirm: opts.confirm,
			}
			if opts.expiresIn > 0 {
				body.ExpiresInSeconds = int64(opts.expiresIn / time.Second)
			}
			var res approveSkillImageResultDTO
			if err := ctx.postJSON(cmd.Context(), "skills/images", body, &res); err != nil {
				return err
			}
			a := res.Approval
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"approved %s\n  %s to back %s for %s/%s %s@%s mode=%s\n  by %s at %s\n",
				a.ID, a.Digest, a.Tool, a.TenantID, a.ProjectID, a.SkillID, a.Version, a.ModeID,
				a.ApprovedBy, a.ApprovedAt)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.tenant, "tenant", "", "Tenant the approval is scoped to (required)")
	f.StringVar(&opts.project, "project", "", "Project the approval is scoped to (required)")
	f.StringVar(&opts.skill, "skill", "", "Skill id (required)")
	f.StringVar(&opts.version, "version", "", "Exact skill version (required)")
	f.StringVar(&opts.mode, "mode", "", "Mode id, e.g. static-code (required)")
	f.StringVar(&opts.tool, "tool", "", "AO tool contract the image backs, e.g. ao.static-scan/v1 (required)")
	f.StringVar(&opts.reference, "reference", "", "Repository the digest came from, recorded for a human (required)")
	f.StringVar(&opts.digest, "digest", "", "sha256:<64 hex> (required)")
	f.StringVar(&opts.note, "note", "", "What you checked (required)")
	f.DurationVar(&opts.expiresIn, "expires-in", 0, "Optional expiry, e.g. 720h. Omit for none")
	f.BoolVar(&opts.confirm, "confirm", false, "Required. Approving authorizes execution of these bytes")
	return cmd
}

// validate refuses locally what the daemon would refuse anyway, so a missing
// flag reads as a usage error (exit 2) instead of a round trip.
//
// It deliberately does NOT re-check the digest format: that rule is the
// daemon's, and a second copy here would drift from the one enforced.
func (o approveImageOptions) validate() error {
	required := []struct {
		flag, value string
	}{
		{"--tenant", o.tenant}, {"--project", o.project}, {"--skill", o.skill},
		{"--version", o.version}, {"--mode", o.mode}, {"--tool", o.tool},
		{"--reference", o.reference}, {"--digest", o.digest}, {"--note", o.note},
	}
	var missing []string
	for _, r := range required {
		if strings.TrimSpace(r.value) == "" {
			missing = append(missing, r.flag)
		}
	}
	if len(missing) > 0 {
		return usageError{fmt.Errorf("%s are required", strings.Join(missing, ", "))}
	}
	if !o.confirm {
		return usageError{fmt.Errorf(
			"--confirm is required: approving an image authorizes this installation to execute those bytes")}
	}
	return nil
}

func newSkillImagesRevokeCommand(ctx *commandContext) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "revoke <approval-id>",
		Short: "Withdraw an image approval",
		Long: "Revoking stops NEW executions immediately: the approval is re-checked at the " +
			"last point before a container starts.\n\n" +
			"It does NOT kill a container already running, and it does NOT recall a secret " +
			"already delivered. Both are stated here because a runbook needs the " +
			"non-promises as much as the promise.\n\n" +
			"The approval stays visible in `ao skills images list`, marked revoked.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			if id == "" {
				return usageError{fmt.Errorf("an approval id is required")}
			}
			if !confirm {
				return usageError{fmt.Errorf(
					"--confirm is required: revoking stops new executions under this approval")}
			}
			if err := ctx.deleteJSON(cmd.Context(), "skills/images/"+id, nil); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"revoked %s\nnew executions under this approval are refused from now on; "+
					"a container already running is not killed, and a secret already delivered is not recalled\n", id)
			return err
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Required. Revoking stops new executions under this approval")
	return cmd
}
