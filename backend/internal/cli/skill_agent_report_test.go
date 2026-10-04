package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// An agent run's report is findings.v1, not a static-scan report. Decoding it
// as the latter would print "boundary evidence NOT DEMONSTRATED" for a run
// that never claimed a container, which is exactly the misleading output the
// CLI's renderers exist to avoid.
func TestSkillsRun_RendersAnAgentReportAsFindings(t *testing.T) {
	report := `{"schemaVersion":"security-audit/findings/v1",
	 "run":{"projectId":"p","mode":"authz-review","skillVersion":"0.2.0","startedAt":"2026-09-23T10:00:00Z","endedAt":"2026-09-23T10:03:00Z"},
	 "coverage":{"examined":["api/orders.go"],"skipped":[{"path":".env","reason":"excluded by AO before staging: denied-by-manifest"}]},
	 "findings":[{"id":"AUTHZ-1","title":"GetOrder skips the tenant predicate","severity":"high","confidence":"probable",
	   "category":"idor","evidence":{"summary":"s","locations":[{"path":"api/orders.go","line":12}]},
	   "reproduction":{"reproducible":false,"steps":[]},"recommendation":"Add the tenant predicate."}],
	 "notes":["AO: executed by host-agent/claude-code (model sonnet) over a read-only staged copy of 3 file(s)"]}`
	detail := map[string]any{
		"run": map[string]any{"id": "skr-1", "skillId": "security-audit", "version": "0.2.0",
			"modeId": "authz-review", "tool": skillAgentTool, "state": "succeeded", "reportSha256": "abc123",
			"summary": "host-agent/claude-code (sonnet) examined 1 of 3 staged files, 1 findings"},
		"integrity": "verified",
		"report":    json.RawMessage(report),
	}
	b, _ := json.Marshal(detail)
	start := strings.Replace(startAccepted, `"modeId":"static-code","tool":"ao.static-scan/v1"`,
		`"modeId":"authz-review","tool":"`+skillAgentTool+`"`, 1)
	_, deps := skillRunCLI(t, http.StatusAccepted, start, string(b))
	out, errOut, err := executeCLI(t, deps, "skills", "run", "security-audit",
		"--project", "ao-pilot-2", "--mode", "authz-review")
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, errOut)
	}
	for _, want := range []string{
		"tool=" + skillAgentTool, "AO: executed by host-agent/claude-code",
		"coverage: 1 path(s) examined, 1 skipped", "skipped .env", "findings 1",
		"high", "AUTHZ-1", "api/orders.go:12", "Add the tenant predicate.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	for _, never := range []string{"NOT DEMONSTRATED", "image"} {
		if strings.Contains(out, never) {
			t.Fatalf("agent report printed container evidence %q:\n%s", never, out)
		}
	}
}
