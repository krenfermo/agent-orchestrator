package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
)

// skill_run_evidence_test.go — the CLI prints the boundary the run demonstrated.
//
// These tests marshal the DAEMON'S OWN types rather than a hand-written JSON
// string. This package mirrors the daemon's DTOs by hand on purpose, and a
// hand mirror is only safe while something checks it still reflects the thing
// it mirrors: a field renamed in skillrunner would leave the mirror decoding
// to a zero value, the CLI would print a confident "NOT DEMONSTRATED" for a
// control the run actually proved, and no string-fixture test would notice.

// runEnvelope wraps a real report the way the run endpoint does.
func runEnvelope(t *testing.T, report skillrunner.StaticScanReport) string {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"skillId": "security-audit", "version": "0.1.0",
		"modeId": "static-code", "tool": "ao.static-scan/v1",
		"report": json.RawMessage(raw),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(body)
}

// confinedReport is what a healthy static scan produces on this host.
func confinedReport() skillrunner.StaticScanReport {
	return skillrunner.StaticScanReport{
		SchemaVersion: "ao.static-scan/v1",
		ImageDigest:   "sha256:6baf4358",
		ApprovalID:    "skimg-90ea1d61",
		ApprovedBy:    "36d87de2",
		Evidence: skillrunner.BoundaryEvidence{
			Runtime: "docker", EffectiveUID: 65534,
			MemoryMaxBytes: 512 << 20, PIDsMax: 128, CPUMax: "100000/100000",
			NetworkReachable: false, InputFilesVisible: 3,
			InputDigest: "9f8e7d6c5b4a", ReadOnlyRootFS: true, InheritedDaemonEnv: 0,
			Controls: []skillcatalog.Control{
				skillcatalog.ControlFilesystemIsolation,
				skillcatalog.ControlProcessIsolation,
				skillcatalog.ControlNoCredentialInheritance,
				skillcatalog.ControlResourceLimits,
				skillcatalog.ControlEgressDenyAll,
			},
		},
		Coverage: skillrunner.ScanCoverage{
			FilesDiscovered: 4, FilesStaged: 3, FilesVisible: 3, FilesScanned: 2,
			FilesSkippedPreStage: 1, FilesSkippedByScanner: 1, Reconciled: true,
			Skipped: []skillrunner.SkippedFile{
				{Path: "src/big.go", Reason: skillrunner.SkipReasonTooLarge,
					Stage: skillrunner.SkipAtStaging, Bytes: 600000, Limit: 524288},
				{Path: "README.md", Reason: skillrunner.SkipReasonUnsupportedExt,
					Stage: skillrunner.SkipAtScan},
			},
			RulesRun:    []string{"AOSS-006"},
			Limitations: []string{"This is a pattern scanner, not a static analyzer."},
		},
		Findings: []skillrunner.ScanFinding{},
	}
}

func TestSkillsRun_RendersTheBoundaryTheRunDemonstrated(t *testing.T) {
	_, deps := skillImagesCLI(t, http.StatusOK, runEnvelope(t, confinedReport()))

	out, errOut, err := executeCLI(t, deps, "skills", "run", "security-audit",
		"--project", "ao-pilot-2", "--mode", "static-code")
	if err != nil {
		t.Fatalf("run: %v (%s)", err, errOut)
	}
	for _, want := range []string{
		"boundary evidence",
		"demonstrated     filesystem isolation",
		"demonstrated     process isolation",
		"demonstrated     no credential inheritance",
		"demonstrated     resource limits",
		"demonstrated     network egress denied",
		// The raw observations behind the verdicts, so they can be checked.
		"uid 65534, rootfs read-only true, network reachable false, daemon env inherited 0",
		"cgroup memory.max 536870912, pids.max 128",
		// Input delivery is part of the contract and has to be visible.
		"inputs delivered 3 file(s), fingerprint 9f8e7d6c5b4a",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("boundary evidence is missing %q:\n%s", want, out)
		}
	}
}

// A control the run did NOT demonstrate has to be visible as not demonstrated.
// Omitting it is worse than printing it: an absent line reads as an absent
// risk, and this block exists to make confinement checkable rather than
// assumed.
func TestSkillsRun_AnUndemonstratedControlIsPrintedNotOmitted(t *testing.T) {
	report := confinedReport()
	// A run that came back root, writable and on the network: the shape of an
	// unconfined execution.
	report.Evidence.EffectiveUID = 0
	report.Evidence.ReadOnlyRootFS = false
	report.Evidence.NetworkReachable = true
	report.Evidence.InputDigest = ""
	report.Evidence.Controls = []skillcatalog.Control{
		skillcatalog.ControlNoCredentialInheritance,
	}

	_, deps := skillImagesCLI(t, http.StatusOK, runEnvelope(t, report))
	out, errOut, err := executeCLI(t, deps, "skills", "run", "security-audit",
		"--project", "ao-pilot-2", "--mode", "static-code")
	if err != nil {
		t.Fatalf("run: %v (%s)", err, errOut)
	}
	for _, want := range []string{
		"NOT DEMONSTRATED filesystem isolation",
		"NOT DEMONSTRATED process isolation",
		"NOT DEMONSTRATED resource limits",
		"NOT DEMONSTRATED network egress denied",
		// The one it did prove still reads as proven.
		"demonstrated     no credential inheritance",
		// Root is called root, not printed as a bare number.
		"uid 0 (ROOT)",
		"fingerprint NOT DEMONSTRATED",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output is missing %q:\n%s", want, out)
		}
	}
}

// Coverage that does not add up must say so where a reader looks for coverage.
func TestSkillsRun_UnreconciledCoverageIsAnnounced(t *testing.T) {
	report := confinedReport()
	// One file lost between discovery and staging: the pilot-2 defect exactly.
	report.Coverage.FilesSkippedPreStage = 0
	report.Coverage.Reconciled = false
	report.Coverage.ReconciliationNote = "discovered 4 != staged 3 + skipped-before-staging 0"

	_, deps := skillImagesCLI(t, http.StatusOK, runEnvelope(t, report))
	out, _, err := executeCLI(t, deps, "skills", "run", "security-audit",
		"--project", "ao-pilot-2", "--mode", "static-code")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "NOT RECONCILED") {
		t.Fatalf("an unreconciled coverage must say so:\n%s", out)
	}
	if !strings.Contains(out, "discovered 4 != staged 3 + skipped-before-staging 0") {
		t.Fatalf("the arithmetic that failed must be named:\n%s", out)
	}
}
