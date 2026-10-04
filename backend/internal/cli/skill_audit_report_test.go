package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const auditReportFixture = `{"schemaVersion":"ao.security-audit/v1","completeness":"partial",` +
	`"summary":{"modesPlanned":4,"modesVerified":3,"statements":["PARTIAL: 3 of 4 audit modes produced a verified report. Not covered: authz-review refused_before_start SKILL_AGENT_UNAVAILABLE."]},` +
	`"modes":[{"mode":"secret-scan","status":"succeeded","verified":true,"runId":"skr-000000000000000000000001","coverage":{"statement":"4 of 4 staged file(s) scanned"}},` +
	`{"mode":"authz-review","status":"refused_before_start","verified":false,"errorCode":"SKILL_AGENT_UNAVAILABLE"}],` +
	`"findings":[{"id":"AUD-001","severity":"critical","confidence":"possible","title":"Credential-shaped literal","path":"app/db.py","line":3,` +
	`"recommendation":"rotate first","sources":[{"mode":"secret-scan","ruleId":"SEC-011"},{"mode":"static-code","ruleId":"AOSS-006"}]}],` +
	`"limitations":["authz-review did not produce a verified report; its checks were NOT performed in this audit."]}`

func auditDetail(t *testing.T, state, sha string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"run": map[string]any{"id": "skr-1", "skillId": "security-audit", "version": "0.4.0", "modeId": "full-audit",
			"tool": skillAuditTool, "state": state, "reportSha256": sha,
			"errorCode": "SKILL_AUDIT_PARTIAL", "errorMessage": "not every mode produced a verified report"},
		"integrity": "verified",
		"report":    json.RawMessage(auditReportFixture),
	})
	return string(b)
}

func fixtureSHA() string {
	sum := sha256.Sum256([]byte(auditReportFixture))
	return hex.EncodeToString(sum[:])
}

// A partial audit prints its report -- completeness and what was not covered
// first -- and still exits non-zero.
func TestSkillsRun_APartialAuditPrintsItsReportAndFails(t *testing.T) {
	start := strings.Replace(startAccepted, `"modeId":"static-code","tool":"ao.static-scan/v1"`,
		`"modeId":"full-audit","tool":"`+skillAuditTool+`"`, 1)
	_, deps := skillRunCLI(t, http.StatusAccepted, start, auditDetail(t, "partial", fixtureSHA()))
	out, _, err := executeCLI(t, deps, "skills", "run", "security-audit", "--project", "p", "--mode", "full-audit")
	if err == nil || !strings.Contains(err.Error(), "PARTIAL") {
		t.Fatalf("a partial audit exited %v", err)
	}
	for _, want := range []string{"security audit PARTIAL", "Not covered: authz-review", "modes 3 of 4 verified",
		"AUD-001 [CRITICAL]", "app/db.py:3", "secret-scan/SEC-011, static-code/AOSS-006", "were NOT performed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Not covered") > strings.Index(out, "AUD-001") {
		t.Fatal("findings were printed before what was not covered")
	}
}

func TestSkillsRuns_ExportWritesTheVerifiedBytes(t *testing.T) {
	_, deps := skillRunCLI(t, http.StatusAccepted, startAccepted, auditDetail(t, "succeeded", fixtureSHA()))
	path := filepath.Join(t.TempDir(), "audit.json")
	out, _, err := executeCLI(t, deps, "skills", "runs", "--project", "p", "skr-1", "--export", path)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	b, err := os.ReadFile(path) //nolint:gosec // test.
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != fixtureSHA() || !strings.Contains(out, "verified") {
		t.Fatalf("exported bytes do not hash to the recorded digest:\n%s", out)
	}
}

func TestSkillsRuns_ExportRefusesBytesThatDoNotMatchTheDigest(t *testing.T) {
	_, deps := skillRunCLI(t, http.StatusAccepted, startAccepted, auditDetail(t, "succeeded", strings.Repeat("0", 64)))
	path := filepath.Join(t.TempDir(), "audit.json")
	if _, _, err := executeCLI(t, deps, "skills", "runs", "--project", "p", "skr-1", "--export", path); err == nil ||
		!strings.Contains(err.Error(), "not exported") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a mismatched report was written")
	}
}
