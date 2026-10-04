package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
)

// The 2D reports render through the same renderer as static-code, built from
// the daemon's own types: a file-level finding prints no ":0", and the
// dependency inventory is summarised.
func TestSkillsRun_RendersADependencyReport(t *testing.T) {
	report := confinedReport()
	report.SchemaVersion = "ao.dependency-scan/v1"
	report.Findings = []skillrunner.ScanFinding{
		{RuleID: "DEP-003", Severity: "medium", Category: "dependency", Confidence: "confirmed",
			Title: "Manifest declares dependencies but no lockfile is present", Path: "tools/package.json", Line: 2,
			Recommendation: "commit a lockfile"},
		{RuleID: "SEC-100", Severity: "medium", Category: "secret", Confidence: "possible",
			Title: "Credential-bearing file present in the checkout (not read)", Path: ".env",
			Recommendation: "confirm it is not committed"},
	}
	report.Inventory = &skillrunner.DependencyInventory{
		Total: 7, ByEcosystem: map[string]int{"npm": 4, "pypi": 2, "go": 1}, Unparsed: []string{"Gemfile"},
	}
	_, deps := skillRunCLI(t, http.StatusAccepted, startAccepted,
		runDetail(t, runEnvelope(t, report), "succeeded", "verified"))
	out, errOut, err := executeCLI(t, deps, "skills", "run", "security-audit",
		"--project", "p", "--mode", "static-code")
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, errOut)
	}
	for _, want := range []string{
		"tools/package.json:2", "inventory 7 declared dependencies (go 1, npm 4, pypi 2)",
		"unparsed manifests: Gemfile",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".env:0") {
		t.Fatalf("a file-level finding printed a line that does not exist:\n%s", out)
	}
}
