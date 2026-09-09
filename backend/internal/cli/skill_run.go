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
			FilesDiscovered       int      `json:"filesDiscovered"`
			FilesStaged           int      `json:"filesStaged"`
			FilesVisible          int      `json:"filesVisible"`
			FilesScanned          int      `json:"filesScanned"`
			FilesSkippedPreStage  int      `json:"filesSkippedPreStage"`
			FilesSkippedByScanner int      `json:"filesSkippedByScanner"`
			SkippedListTruncated  bool     `json:"skippedListTruncated"`
			Reconciled            bool     `json:"reconciled"`
			ReconciliationNote    string   `json:"reconciliationNote"`
			RulesRun              []string `json:"rulesRun"`
			Extensions            []string `json:"extensions"`
			Limitations           []string `json:"limitations"`
			Skipped               []struct {
				Path   string `json:"path"`
				Reason string `json:"reason"`
				Stage  string `json:"stage"`
				Bytes  int64  `json:"bytes"`
				Limit  int64  `json:"limit"`
			} `json:"skipped"`
		} `json:"coverage"`
		// Evidence is the boundary the run demonstrated from INSIDE the
		// container. The daemon has always sent it and this CLI never printed
		// it, so a reader could not tell a confined run from an unconfined
		// one without reading the HTTP response by hand.
		//
		// The keys are Go field names because skillrunner.BoundaryEvidence
		// carries no json tags. Renaming them would be a wire change for a
		// field no client reads yet, so the mirror matches what is on the
		// wire today rather than what would be prettier.
		Evidence struct {
			Runtime             string   `json:"Runtime"`
			EffectiveUID        int      `json:"EffectiveUID"`
			MemoryMaxBytes      int64    `json:"MemoryMaxBytes"`
			PIDsMax             int      `json:"PIDsMax"`
			CPUMax              string   `json:"CPUMax"`
			NetworkReachable    bool     `json:"NetworkReachable"`
			InputFilesVisible   int      `json:"InputFilesVisible"`
			InputDigest         string   `json:"InputDigest"`
			ReadOnlyRootFS      bool     `json:"ReadOnlyRootFS"`
			InheritedDaemonEnv  int      `json:"InheritedDaemonEnv"`
			SecretRefsDelivered []string `json:"SecretRefsDelivered"`
			Controls            []string `json:"Controls"`
		} `json:"evidence"`
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

	// Coverage BEFORE findings, always, and with its DENOMINATOR first: a
	// reader who sees "scanned 2" and nothing to read it against cannot tell a
	// two-file project from a two-of-four one.
	c := res.Report.Coverage
	if err := p("\ncoverage\n  discovered %d, staged %d, visible %d, scanned %d\n",
		c.FilesDiscovered, c.FilesStaged, c.FilesVisible, c.FilesScanned); err != nil {
		return err
	}
	// Whether the counts add up, said out loud. An unreconciled coverage block
	// must not be read as a complete one, so it is stated here rather than
	// left for the reader to work out with arithmetic.
	if c.Reconciled {
		if err := p("  reconciled  discovered = staged + skipped-before-staging (%d = %d + %d); "+
			"staged = scanned + skipped-by-scanner (%d = %d + %d)\n",
			c.FilesDiscovered, c.FilesStaged, c.FilesSkippedPreStage,
			c.FilesStaged, c.FilesScanned, c.FilesSkippedByScanner); err != nil {
			return err
		}
	} else {
		note := c.ReconciliationNote
		if note == "" {
			note = "the counts above do not add up"
		}
		if err := p("  NOT RECONCILED  %s\n", note); err != nil {
			return err
		}
	}
	if len(c.RulesRun) > 0 {
		if err := p("  rules  %s\n", strings.Join(c.RulesRun, ", ")); err != nil {
			return err
		}
	}
	// The counts are authoritative, but never at the cost of printing nothing:
	// a response whose counts are missing or wrong must still show the entries
	// it does carry, or a defect in the accounting would hide the very list
	// the accounting exists to guarantee.
	total := c.FilesSkippedPreStage + c.FilesSkippedByScanner
	if total < len(c.Skipped) {
		total = len(c.Skipped)
	}
	if total > 0 {
		if err := p("  skipped %d file(s) (%d before staging, %d by the scanner):\n",
			total, c.FilesSkippedPreStage, c.FilesSkippedByScanner); err != nil {
			return err
		}
		for _, s := range c.Skipped {
			detail := ""
			// A size bound is the one reason where the numbers are the
			// actionable part: "too_large" alone does not say by how much.
			if s.Bytes > 0 && s.Limit > 0 {
				detail = fmt.Sprintf(", %d bytes over a %d-byte limit", s.Bytes, s.Limit)
			}
			where := s.Stage
			if where == "" {
				where = "unknown"
			}
			if err := p("    %s (%s, at %s%s)\n", s.Path, s.Reason, where, detail); err != nil {
				return err
			}
		}
		if c.SkippedListTruncated {
			if err := p("    ... only the first %d are listed; the counts above are complete\n",
				len(c.Skipped)); err != nil {
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

	if err := renderBoundaryEvidence(p, res); err != nil {
		return err
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

// staticRunControls are the confinement controls a static scan can demonstrate,
// in the order a reader should see them.
//
// The list mirrors skillrunner.demonstratedControls, which is what decides
// whether each one is present in the evidence. It is spelled out here rather
// than imported because this package hand-mirrors the daemon's DTOs by
// design -- and because the whole point of the block is to name a control that
// is ABSENT, which a list derived from the evidence itself could never do.
// A control the daemon demonstrates but this list does not know about is
// printed too, under "also demonstrated", so nothing is lost to drift.
var staticRunControls = []struct{ id, label string }{
	{"filesystem_isolation", "filesystem isolation"},
	{"process_isolation", "process isolation (non-root, pid-capped)"},
	{"no_credential_inheritance", "no credential inheritance"},
	{"resource_limits", "resource limits"},
	{"egress_deny_all", "network egress denied"},
}

// renderBoundaryEvidence prints what the run proved about its own confinement.
//
// It exists because the daemon has always returned this and the CLI has always
// dropped it: a report that says what a scan found, with nothing about whether
// the scan was contained while it looked, is half a report. Anything the run
// did not demonstrate is printed as NOT DEMONSTRATED rather than left out --
// an omitted control reads as an absent risk, which is the opposite of true.
func renderBoundaryEvidence(p func(string, ...any) error, res skillRunDTO) error {
	ev := res.Report.Evidence
	if err := p("\nboundary evidence (observed from inside the container)\n"); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range ev.Controls {
		have[c] = true
	}
	known := map[string]bool{}
	for _, c := range staticRunControls {
		known[c.id] = true
		state := "NOT DEMONSTRATED"
		if have[c.id] {
			state = "demonstrated"
		}
		if err := p("  %-16s %s\n", state, c.label); err != nil {
			return err
		}
	}
	for _, c := range ev.Controls {
		if !known[c] {
			if err := p("  %-16s %s (also demonstrated)\n", "demonstrated", c); err != nil {
				return err
			}
		}
	}

	// The raw observations behind the verdicts above, so a reader can check
	// them rather than take them.
	uid := fmt.Sprintf("%d", ev.EffectiveUID)
	if ev.EffectiveUID == 0 {
		uid = "0 (ROOT)"
	}
	if err := p("  uid %s, rootfs read-only %t, network reachable %t, daemon env inherited %d\n",
		uid, ev.ReadOnlyRootFS, ev.NetworkReachable, ev.InheritedDaemonEnv); err != nil {
		return err
	}
	if ev.MemoryMaxBytes > 0 || ev.PIDsMax > 0 {
		if err := p("  cgroup memory.max %d, pids.max %d, cpu.max %s\n",
			ev.MemoryMaxBytes, ev.PIDsMax, orUnknown(ev.CPUMax)); err != nil {
			return err
		}
	}
	// Input delivery is part of the contract: AO fingerprints what it staged
	// and the container fingerprints what it read. The run is refused unless
	// they match, so printing the digest is what lets a reader confirm the
	// scan read THIS tree and not some other one.
	digest := ev.InputDigest
	if digest == "" {
		digest = "NOT DEMONSTRATED (the container reported no input digest)"
	}
	if err := p("  inputs delivered %d file(s), fingerprint %s\n",
		ev.InputFilesVisible, digest); err != nil {
		return err
	}
	if len(ev.SecretRefsDelivered) > 0 {
		if err := p("  secrets delivered (names only) %s\n",
			strings.Join(ev.SecretRefsDelivered, ", ")); err != nil {
			return err
		}
	}
	return nil
}
