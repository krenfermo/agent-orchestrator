package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// skill_audit_report.go renders a full security audit (Frente 2 / 2E): the
// consolidated ao.security-audit/v1 report. Completeness first, then what was
// not covered, then the findings -- the same order as every other renderer
// here, for the same reason. A PARTIAL audit prints its report and still exits
// non-zero: a script must not read it as a completed audit.

// skillAuditTool mirrors skills.AuditTool.
const skillAuditTool = "ao.security-audit/v1"

type auditReportDTO struct {
	Completeness string `json:"completeness"`
	Summary      struct {
		ModesPlanned  int      `json:"modesPlanned"`
		ModesVerified int      `json:"modesVerified"`
		Statements    []string `json:"statements"`
	} `json:"summary"`
	Modes []struct {
		Mode         string `json:"mode"`
		Status       string `json:"status"`
		Verified     bool   `json:"verified"`
		RunID        string `json:"runId"`
		ErrorCode    string `json:"errorCode"`
		ReportSHA256 string `json:"reportSha256"`
		Coverage     *struct {
			Statement string `json:"statement"`
		} `json:"coverage"`
	} `json:"modes"`
	Findings []struct {
		ID             string `json:"id"`
		Severity       string `json:"severity"`
		Confidence     string `json:"confidence"`
		Title          string `json:"title"`
		Path           string `json:"path"`
		Line           int    `json:"line"`
		Recommendation string `json:"recommendation"`
		Sources        []struct {
			Mode   string `json:"mode"`
			RuleID string `json:"ruleId"`
		} `json:"sources"`
	} `json:"findings"`
	Limitations []string `json:"limitations"`
}

func renderAuditOutcome(cmd *cobra.Command, detail skillRunDetailDTO) error {
	r := detail.Run
	if detail.Integrity != "verified" || len(detail.Report) == 0 {
		return fmt.Errorf("audit %s ended %s but its stored report did not verify (integrity %q); it is not shown",
			r.ID, r.State, detail.Integrity)
	}
	var rep auditReportDTO
	if err := json.Unmarshal(detail.Report, &rep); err != nil {
		return fmt.Errorf("decode audit %s report: %w", r.ID, err)
	}
	out := cmd.OutOrStdout()
	p := func(format string, a ...any) error {
		_, err := fmt.Fprintf(out, format, a...)
		return err
	}
	if err := p("security audit %s  %s@%s  report sha256 %s (verified)\n", strings.ToUpper(rep.Completeness),
		r.SkillID, r.Version, r.ReportSHA256); err != nil {
		return err
	}
	for _, st := range rep.Summary.Statements {
		if err := p("  %s\n", st); err != nil {
			return err
		}
	}
	if err := p("\nmodes %d of %d verified\n", rep.Summary.ModesVerified, rep.Summary.ModesPlanned); err != nil {
		return err
	}
	for _, m := range rep.Modes {
		line := fmt.Sprintf("  %-14s %-20s", m.Mode, m.Status)
		if m.RunID != "" {
			line += " run " + m.RunID
		}
		if m.ErrorCode != "" {
			line += " " + m.ErrorCode
		}
		if m.Coverage != nil {
			line += "\n    " + m.Coverage.Statement
		}
		if err := p("%s\n", line); err != nil {
			return err
		}
	}
	if err := p("\nfindings %d\n", len(rep.Findings)); err != nil {
		return err
	}
	for _, f := range rep.Findings {
		loc := f.Path
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		sources := make([]string, 0, len(f.Sources))
		for _, s := range f.Sources {
			sources = append(sources, s.Mode+"/"+s.RuleID)
		}
		if err := p("  %s [%s] %s\n    %s  (%s, confidence %s)\n    fix: %s\n", f.ID, strings.ToUpper(f.Severity),
			f.Title, loc, strings.Join(sources, ", "), f.Confidence, f.Recommendation); err != nil {
			return err
		}
	}
	if err := p("\nwhat this cannot tell you\n"); err != nil {
		return err
	}
	for _, l := range rep.Limitations {
		if err := p("  - %s\n", l); err != nil {
			return err
		}
	}
	if r.State == "partial" {
		return fmt.Errorf("audit %s is PARTIAL (%s): %s", r.ID, r.ErrorCode, r.ErrorMessage)
	}
	return nil
}

// exportSkillRunReport writes the stored report bytes, and only when they hash
// to the digest the daemon recorded -- so the file on disk is verifiable with
// nothing but sha256sum and the run's reportSha256.
func exportSkillRunReport(out io.Writer, detail skillRunDetailDTO, path string) error {
	r := detail.Run
	if detail.Integrity != "verified" || len(detail.Report) == 0 {
		return fmt.Errorf("run %s has no verified report to export (integrity %q)", r.ID, detail.Integrity)
	}
	sum := sha256.Sum256(detail.Report)
	if got := hex.EncodeToString(sum[:]); got != r.ReportSHA256 {
		return fmt.Errorf("run %s: the served report hashes to %s, not the recorded %s; not exported", r.ID, got, r.ReportSHA256)
	}
	if err := os.WriteFile(path, detail.Report, 0o600); err != nil {
		return fmt.Errorf("export run %s report: %w", r.ID, err)
	}
	_, err := fmt.Fprintf(out, "exported %s (sha256 %s, verified)\n", path, r.ReportSHA256)
	return err
}
