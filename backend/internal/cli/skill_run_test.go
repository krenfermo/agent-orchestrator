package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// `ao skills run`. The assertions are about two things: the request AO sends
// (the caller contributes no command), and the ORDER the report is rendered in.
//
// Coverage before findings is not a layout preference. "0 findings" is not a
// result until you know what was read, and a scan that staged nothing must not
// print like a clean bill of health.

const staticRunBody = `{"skillId":"security-audit","version":"1.2.0","modeId":"static-code",
 "tool":"ao.static-scan/v1","report":{"schemaVersion":"1","imageDigest":"sha256:dddd",
 "approvalId":"skimg-9","approvedBy":"admin","approvalRevokedDuringRun":false,
 "coverage":{"filesStaged":12,"filesVisible":12,"filesScanned":10,
   "rulesRun":["hardcoded-secret","weak-hash"],"extensions":[".go",".ts"],
   "skipped":[{"path":"vendor/big.bin","reason":"binary"}],
   "limitations":["This is a pattern scanner, not a static analyzer."]},
 "findings":[{"ruleId":"hardcoded-secret","severity":"high","category":"secrets",
   "title":"Possible hardcoded credential","path":"src/db.go","line":42,
   "recommendation":"Move it to a secret store.","confidence":"possible"}]}}`

func TestSkillsRun_SendsNoCommandAndRendersCoverageFirst(t *testing.T) {
	capture, deps := skillImagesCLI(t, http.StatusOK, staticRunBody)

	out, errOut, err := executeCLI(t, deps, "skills", "run", "security-audit",
		"--project", "medusa", "--mode", "static-code", "--input", "depth=2")
	if err != nil {
		t.Fatalf("run: %v (%s)", err, errOut)
	}
	if capture.method != http.MethodPost ||
		capture.path != "/api/v1/projects/medusa/skills/security-audit/run" {
		t.Fatalf("%s %s", capture.method, capture.path)
	}

	// The body carries the mode and the declared inputs, and NOTHING that could
	// contribute to a command line.
	var sent map[string]any
	if err := json.Unmarshal([]byte(capture.body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent["modeId"] != "static-code" {
		t.Fatalf("modeId = %v", sent["modeId"])
	}
	for _, forbidden := range []string{"image", "argv", "command", "digest", "attestation"} {
		if _, ok := sent[forbidden]; ok {
			t.Fatalf("the CLI sent %q; the caller contributes no command", forbidden)
		}
	}

	// Which bytes ran and who allowed them.
	for _, want := range []string{"sha256:dddd", "skimg-9", "admin"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output is missing %q:\n%s", want, out)
		}
	}
	// Coverage strictly before findings.
	cov, find := strings.Index(out, "coverage"), strings.Index(out, "findings")
	if cov < 0 || find < 0 || cov > find {
		t.Fatalf("coverage must be rendered before findings:\n%s", out)
	}
	for _, want := range []string{
		"staged 12, visible 12, scanned 10",
		"hardcoded-secret", "vendor/big.bin (binary)",
		"[HIGH] Possible hardcoded credential", "src/db.go:42",
		// The tool saying what it cannot know.
		"what this cannot tell you", "pattern scanner",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output is missing %q:\n%s", want, out)
		}
	}
}

// The failure this rendering exists to prevent: an empty report read as a clean
// one. Nothing scanned and nothing found must say so in words.
func TestSkillsRun_AnEmptyScanDoesNotReadAsClean(t *testing.T) {
	_, deps := skillImagesCLI(t, http.StatusOK,
		`{"skillId":"security-audit","version":"1.2.0","modeId":"static-code",
		  "tool":"ao.static-scan/v1","report":{"imageDigest":"sha256:dddd",
		  "approvalId":"skimg-9","approvedBy":"admin",
		  "coverage":{"filesStaged":0,"filesVisible":0,"filesScanned":0},"findings":[]}}`)

	out, _, err := executeCLI(t, deps, "skills", "run", "security-audit", "--project", "medusa")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "this is not a clean result") {
		t.Fatalf("an empty scan must not read as clean:\n%s", out)
	}
}

// An approval withdrawn mid-run is a fact about the results, and AO does not
// kill a running container. It must reach the reader.
func TestSkillsRun_SurfacesAnApprovalRevokedDuringTheRun(t *testing.T) {
	_, deps := skillImagesCLI(t, http.StatusOK,
		`{"skillId":"security-audit","version":"1.2.0","modeId":"static-code",
		  "tool":"ao.static-scan/v1","report":{"imageDigest":"sha256:dddd",
		  "approvalId":"skimg-9","approvedBy":"admin","approvalRevokedDuringRun":true,
		  "coverage":{"filesStaged":3,"filesVisible":3,"filesScanned":3},"findings":[]}}`)

	out, _, err := executeCLI(t, deps, "skills", "run", "security-audit", "--project", "medusa")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "revoked while this was running") {
		t.Fatalf("a revocation during the run must be reported:\n%s", out)
	}
}

func TestSkillsRun_RequiresAProject(t *testing.T) {
	capture, deps := skillImagesCLI(t, 0, `{}`)
	if _, _, err := executeCLI(t, deps, "skills", "run", "security-audit"); err == nil {
		t.Fatal("ran without --project")
	}
	if strings.Contains(capture.path, "/run") {
		t.Fatalf("a request reached the daemon anyway: %s", capture.path)
	}
}

// A refusal from the daemon — the ordinary case on a host with no runtime — has
// to reach the operator instead of being rendered as an empty report.
func TestSkillsRun_SurfacesTheDaemonsRefusal(t *testing.T) {
	_, deps := skillImagesCLI(t, http.StatusConflict,
		`{"code":"SKILL_RUNNER_UNAVAILABLE",
		  "message":"this installation has no skill runner configured, so nothing can execute"}`)

	_, errOut, err := executeCLI(t, deps, "skills", "run", "security-audit", "--project", "medusa")
	if err == nil {
		t.Fatal("a refused run reported success")
	}
	if !strings.Contains(err.Error()+errOut, "no skill runner") {
		t.Fatalf("the refusal must reach the operator: %v %s", err, errOut)
	}
}
