package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// skill_agent_report.go renders the report of a skill run whose executor was
// an agent (Frente 2 / 2C). It is a findings.v1 document -- the package's own
// contract, validated and redacted by the daemon before it was stored -- and
// not the static scan's report, so it has its own renderer rather than being
// decoded into a shape that would print container evidence it never had.

// skillAgentTool mirrors skillagent.Tool.
const skillAgentTool = "ao.skill-agent/v1"

// agentReportDTO mirrors the parts of findings.v1 the CLI prints.
type agentReportDTO struct {
	SchemaVersion string `json:"schemaVersion"`
	Coverage      struct {
		Examined []string `json:"examined"`
		Skipped  []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	} `json:"coverage"`
	Findings []struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		Severity   string `json:"severity"`
		Confidence string `json:"confidence"`
		Category   string `json:"category"`
		Evidence   struct {
			Summary   string `json:"summary"`
			Locations []struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			} `json:"locations"`
		} `json:"evidence"`
		Recommendation string `json:"recommendation"`
	} `json:"findings"`
	Notes []string `json:"notes"`
}

func renderAgentReport(cmd *cobra.Command, r skillRunSummaryDTO, raw json.RawMessage) error {
	var rep agentReportDTO
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("decode run %s report: %w", r.ID, err)
	}
	out := cmd.OutOrStdout()
	p := func(format string, a ...any) error {
		_, err := fmt.Fprintf(out, format, a...)
		return err
	}
	if err := p("report sha256 %s (verified)\n%s@%s mode=%s tool=%s\n%s\n",
		r.ReportSHA256, r.SkillID, r.Version, r.ModeID, r.Tool, r.Summary); err != nil {
		return err
	}
	for _, n := range rep.Notes {
		if strings.HasPrefix(n, "AO:") {
			if err := p("%s\n", n); err != nil {
				return err
			}
		}
	}
	if err := p("\ncoverage: %d path(s) examined, %d skipped\n",
		len(rep.Coverage.Examined), len(rep.Coverage.Skipped)); err != nil {
		return err
	}
	for _, sk := range rep.Coverage.Skipped {
		if err := p("  skipped %s (%s)\n", sk.Path, sk.Reason); err != nil {
			return err
		}
	}
	if err := p("\nfindings %d\n", len(rep.Findings)); err != nil {
		return err
	}
	for _, f := range rep.Findings {
		loc := ""
		if len(f.Evidence.Locations) > 0 {
			loc = f.Evidence.Locations[0].Path
			if f.Evidence.Locations[0].Line > 0 {
				loc = fmt.Sprintf("%s:%d", loc, f.Evidence.Locations[0].Line)
			}
		}
		if err := p("  %-8s %-9s %s  %s  %s\n    fix: %s\n",
			f.Severity, f.Confidence, f.ID, loc, f.Title, f.Recommendation); err != nil {
			return err
		}
	}
	if len(rep.Findings) == 0 && len(rep.Coverage.Examined) == 0 {
		// The same sentence the static scan prints, for the same reason.
		return p("\nno findings, and the agent examined nothing: this is NOT a clean audit\n")
	}
	return nil
}
