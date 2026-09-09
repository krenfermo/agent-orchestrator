package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// skill_run.go — `ao skills run`, the first surface that actually executes one.
//
// The rendering rule here is the one the report itself is built around:
// COVERAGE COMES FIRST, before findings. "0 findings" is not a result until you
// know what was read, and a scanner that staged nothing and found nothing must
// not print like a clean bill of health. The tool's own stated limitations are
// printed for the same reason — they are not boilerplate, they are the tool
// saying what it cannot know.

type skillRunDTO struct {
	SkillID string `json:"skillId"`
	Version string `json:"version"`
	ModeID  string `json:"modeId"`
	Tool    string `json:"tool"`
	Report  struct {
		SchemaVersion            string `json:"schemaVersion"`
		ImageDigest              string `json:"imageDigest"`
		ApprovalID               string `json:"approvalId"`
		ApprovedBy               string `json:"approvedBy"`
		ApprovalRevokedDuringRun bool   `json:"approvalRevokedDuringRun"`
		StartedAt                string `json:"startedAt"`
		EndedAt                  string `json:"endedAt"`
		Truncated                bool   `json:"truncated"`
		Coverage                 struct {
			FilesStaged  int      `json:"filesStaged"`
			FilesVisible int      `json:"filesVisible"`
			FilesScanned int      `json:"filesScanned"`
			RulesRun     []string `json:"rulesRun"`
			Extensions   []string `json:"extensions"`
			Limitations  []string `json:"limitations"`
			Skipped      []struct {
				Path   string `json:"path"`
				Reason string `json:"reason"`
			} `json:"skipped"`
		} `json:"coverage"`
		Findings []struct {
			RuleID         string `json:"ruleId"`
			Severity       string `json:"severity"`
			Category       string `json:"category"`
			Title          string `json:"title"`
			Path           string `json:"path"`
			Line           int    `json:"line"`
			Recommendation string `json:"recommendation"`
			Confidence     string `json:"confidence"`
		} `json:"findings"`
	} `json:"report"`
}

type runSkillBodyDTO struct {
	ModeID string            `json:"modeId,omitempty"`
	Inputs map[string]string `json:"inputs,omitempty"`
}

func newSkillsRunCommand(ctx *commandContext) *cobra.Command {
	var (
		project string
		mode    string
		inputs  map[string]string
	)
	cmd := &cobra.Command{
		Use:   "run <skill-id>",
		Short: "Execute one authorized mode of a skill activated on a project",
		Long: "Runs a skill. Everything has to already be true: installed, enabled on this " +
			"project, pinned to a version, an administrator has approved an image digest for " +
			"this exact scope, every capability the mode declares is granted, and the runtime " +
			"attests every control the mode needs.\n\n" +
			"You contribute no image, no command and no argv. AO authors what runs.\n\n" +
			"Only the static, read-only scan is implemented. Use `ao skills dry-run` first to " +
			"see what a run would need; it starts nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			skill := strings.TrimSpace(args[0])
			if skill == "" {
				return usageError{fmt.Errorf("a skill id is required")}
			}
			if strings.TrimSpace(project) == "" {
				return usageError{fmt.Errorf("--project is required")}
			}
			var res skillRunDTO
			if err := ctx.postJSON(cmd.Context(),
				"projects/"+project+"/skills/"+skill+"/run",
				runSkillBodyDTO{ModeID: strings.TrimSpace(mode), Inputs: inputs},
				&res); err != nil {
				return err
			}
			return renderSkillRun(cmd, res)
		},
	}
	f := cmd.Flags()
	f.StringVar(&project, "project", "", "Project to run in (required)")
	f.StringVar(&mode, "mode", "", "Mode id; required unless the skill declares exactly one")
	f.StringToStringVar(&inputs, "input", nil, "Declared input, repeatable: --input key=value")
	return cmd
}

func renderSkillRun(cmd *cobra.Command, res skillRunDTO) error {
	out := cmd.OutOrStdout()
	p := func(format string, a ...any) error {
		_, err := fmt.Fprintf(out, format, a...)
		return err
	}

	if err := p("%s@%s mode=%s tool=%s\n", res.SkillID, res.Version, res.ModeID, res.Tool); err != nil {
		return err
	}
	// Which bytes ran, and who allowed them. A report that says one without the
	// other leaves the more important half unanswered.
	if err := p("image   %s\napproval %s by %s\n",
		res.Report.ImageDigest, res.Report.ApprovalID, res.Report.ApprovedBy); err != nil {
		return err
	}
	if res.Report.ApprovalRevokedDuringRun {
		if err := p("WARNING the approval was revoked while this was running; " +
			"AO does not kill a running container, so these results were produced under a " +
			"withdrawn authorization\n"); err != nil {
			return err
		}
	}

	// Coverage BEFORE findings, always.
	c := res.Report.Coverage
	if err := p("\ncoverage\n  staged %d, visible %d, scanned %d\n",
		c.FilesStaged, c.FilesVisible, c.FilesScanned); err != nil {
		return err
	}
	if len(c.RulesRun) > 0 {
		if err := p("  rules  %s\n", strings.Join(c.RulesRun, ", ")); err != nil {
			return err
		}
	}
	if len(c.Skipped) > 0 {
		if err := p("  skipped %d file(s):\n", len(c.Skipped)); err != nil {
			return err
		}
		for _, s := range c.Skipped {
			if err := p("    %s (%s)\n", s.Path, s.Reason); err != nil {
				return err
			}
		}
	}
	if res.Report.Truncated {
		if err := p("  TRUNCATED the tool's output hit AO's cap; the finding list may be incomplete\n"); err != nil {
			return err
		}
	}

	if err := p("\nfindings %d\n", len(res.Report.Findings)); err != nil {
		return err
	}
	for _, f := range res.Report.Findings {
		if err := p("  [%s] %s\n    %s:%d  %s (%s, confidence %s)\n    %s\n",
			strings.ToUpper(f.Severity), f.Title, f.Path, f.Line,
			f.RuleID, f.Category, f.Confidence, f.Recommendation); err != nil {
			return err
		}
	}
	if len(res.Report.Findings) == 0 && c.FilesScanned == 0 {
		// The one sentence that stops an empty report reading as a clean one.
		if err := p("  nothing was scanned, so nothing was found; this is not a clean result\n"); err != nil {
			return err
		}
	}

	// The tool saying what it cannot know. Last, so it is the thing left on
	// screen next to the findings.
	if len(c.Limitations) > 0 {
		if err := p("\nwhat this cannot tell you\n"); err != nil {
			return err
		}
		for _, l := range c.Limitations {
			if err := p("  - %s\n", l); err != nil {
				return err
			}
		}
	}
	return nil
}
