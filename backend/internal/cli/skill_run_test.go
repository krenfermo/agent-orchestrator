package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// runRequestLog records every request the fake daemon saw, in order.
type runRequestLog struct {
	mu   sync.Mutex
	reqs []skillsCapture
}

func (l *runRequestLog) all() []skillsCapture {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]skillsCapture(nil), l.reqs...)
}

// skillRunCLI is a fake daemon for the durable run flow: POST .../run answers
// startStatus/startBody, and each GET of the run answers the next of details
// (the last one repeats), which is how a test walks a run through its states.
func skillRunCLI(t *testing.T, startStatus int, startBody string, details ...string) (*runRequestLog, Deps) {
	t.Helper()
	cfg := setConfigEnv(t)
	log := &runRequestLog{}
	var mu sync.Mutex
	next := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			// The CLI's own invocation telemetry is not part of the run flow.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		log.mu.Lock()
		log.reqs = append(log.reqs, skillsCapture{method: r.Method, path: r.URL.RequestURI(), body: string(raw)})
		log.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(startStatus)
			_, _ = io.WriteString(w, startBody)
			return
		}
		mu.Lock()
		body := `{}`
		if len(details) > 0 {
			i := next
			if i >= len(details) {
				i = len(details) - 1
			}
			body = details[i]
			next++
		}
		mu.Unlock()
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	return log, Deps{ProcessAlive: func(int) bool { return true }}
}

const startAccepted = `{"run":{"id":"skr-1","skillId":"security-audit","version":"1.2.0",
 "modeId":"static-code","tool":"ao.static-scan/v1","state":"queued"},"created":true}`

// runDetail wraps a report (taken from a legacy SkillRunView body) as the detail
// of a run in the given state.
func runDetail(t *testing.T, legacyViewBody, state, integrity string) string {
	t.Helper()
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal([]byte(legacyViewBody), &legacy); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	detail := map[string]any{
		"run": map[string]any{"id": "skr-1", "skillId": "security-audit", "version": "1.2.0",
			"modeId": "static-code", "tool": "ao.static-scan/v1", "state": state,
			"reportSha256": "abc123"},
		"integrity": integrity,
	}
	if state == "succeeded" && integrity == "verified" {
		detail["report"] = legacy["report"]
	}
	b, _ := json.Marshal(detail)
	return string(b)
}

// `ao skills run`. The assertions are about two things: the request AO sends
// (the caller contributes no command), and the ORDER the report is rendered in.
//
// Coverage before findings is not a layout preference. "0 findings" is not a
// result until you know what was read, and a scan that staged nothing must not
// print like a clean bill of health.

const staticRunBody = `{"skillId":"security-audit","version":"1.2.0","modeId":"static-code",
 "tool":"ao.static-scan/v1","report":{"schemaVersion":"1","imageDigest":"sha256:dddd",
 "approvalId":"skimg-9","approvedBy":"admin","approvalRevokedDuringRun":false,
 "coverage":{"filesDiscovered":13,"filesStaged":12,"filesVisible":12,"filesScanned":10,
   "filesSkippedPreStage":1,"filesSkippedByScanner":2,"reconciled":true,
   "rulesRun":["hardcoded-secret","weak-hash"],"extensions":[".go",".ts"],
   "skipped":[{"path":"src/huge.go","reason":"too_large","stage":"staging",
     "bytes":600000,"limit":524288},
    {"path":"vendor/big.bin","reason":"binary","stage":"scan"},
    {"path":"README.md","reason":"unsupported_extension","stage":"scan"}],
   "limitations":["This is a pattern scanner, not a static analyzer."]},
 "evidence":{"Runtime":"docker","EffectiveUID":65534,"MemoryMaxBytes":536870912,
   "PIDsMax":128,"CPUMax":"100000/100000","NetworkReachable":false,
   "InputFilesVisible":12,"InputDigest":"ab12cd34ef56","ReadOnlyRootFS":true,
   "InheritedDaemonEnv":0,
   "Controls":["filesystem_isolation","process_isolation","no_credential_inheritance",
     "resource_limits","egress_deny_all"]},
 "findings":[{"ruleId":"hardcoded-secret","severity":"high","category":"secrets",
   "title":"Possible hardcoded credential","path":"src/db.go","line":42,
   "recommendation":"Move it to a secret store.","confidence":"possible"}]}}`

func TestSkillsRun_SendsNoCommandAndRendersCoverageFirst(t *testing.T) {
	log, deps := skillRunCLI(t, http.StatusAccepted, startAccepted,
		runDetail(t, staticRunBody, "succeeded", "verified"))

	out, errOut, err := executeCLI(t, deps, "skills", "run", "security-audit",
		"--project", "medusa", "--mode", "static-code", "--input", "depth=2")
	if err != nil {
		t.Fatalf("run: %v (%s)", err, errOut)
	}
	reqs := log.all()
	if len(reqs) < 2 || reqs[0].method != http.MethodPost ||
		reqs[0].path != "/api/v1/projects/medusa/skills/security-audit/run" ||
		reqs[1].path != "/api/v1/projects/medusa/skills/runs/skr-1" {
		t.Fatalf("requests: %+v", reqs)
	}
	capture := reqs[0]

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
		// The denominator comes first: "scanned 10" alone cannot be read.
		"discovered 13, staged 12, visible 12, scanned 10",
		"hardcoded-secret", "vendor/big.bin (binary, at scan)",
		"[HIGH] Possible hardcoded credential", "src/db.go:42",
		// The oversized file, its stage AND the numbers behind the verdict.
		"src/huge.go (too_large, at staging, 600000 bytes over a 524288-byte limit)",
		"skipped 3 file(s) (1 before staging, 2 by the scanner)",
		// The coverage says out loud that it adds up.
		"reconciled", "13 = 12 + 1", "12 = 10 + 2",
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
	_, deps := skillRunCLI(t, http.StatusAccepted, startAccepted,
		runDetail(t, `{"skillId":"security-audit","version":"1.2.0","modeId":"static-code",
		  "tool":"ao.static-scan/v1","report":{"imageDigest":"sha256:dddd",
		  "approvalId":"skimg-9","approvedBy":"admin",
		  "coverage":{"filesStaged":0,"filesVisible":0,"filesScanned":0},"findings":[]}}`, "succeeded", "verified"))

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
	_, deps := skillRunCLI(t, http.StatusAccepted, startAccepted,
		runDetail(t, `{"skillId":"security-audit","version":"1.2.0","modeId":"static-code",
		  "tool":"ao.static-scan/v1","report":{"imageDigest":"sha256:dddd",
		  "approvalId":"skimg-9","approvedBy":"admin","approvalRevokedDuringRun":true,
		  "coverage":{"filesStaged":3,"filesVisible":3,"filesScanned":3},"findings":[]}}`, "succeeded", "verified"))

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

// The CLI waits through the run's states and only then renders the result.
func TestSkillsRun_WaitsForTheDurableRunToEnd(t *testing.T) {
	log, deps := skillRunCLI(t, http.StatusAccepted, startAccepted,
		`{"run":{"id":"skr-1","state":"queued"}}`,
		`{"run":{"id":"skr-1","state":"running"}}`,
		runDetail(t, staticRunBody, "succeeded", "verified"))
	out, _, err := executeCLI(t, deps, "skills", "run", "security-audit", "--project", "medusa",
		"--poll-interval", "1ms")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len(log.all()); n != 4 {
		t.Fatalf("expected 1 start + 3 polls, got %d requests", n)
	}
	for _, want := range []string{"run skr-1 accepted", "report sha256 abc123 (verified)", "coverage"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output is missing %q:\n%s", want, out)
		}
	}
}

// A run that did not succeed is a non-zero exit that names the code.
func TestSkillsRun_AnUnsuccessfulRunFailsWithItsCode(t *testing.T) {
	_, deps := skillRunCLI(t, http.StatusAccepted, startAccepted,
		`{"run":{"id":"skr-1","state":"refused","errorCode":"SKILL_IMAGE_NOT_APPROVED",
		  "errorMessage":"no image is approved for this scope"},"integrity":"none"}`)
	_, _, err := executeCLI(t, deps, "skills", "run", "security-audit", "--project", "medusa")
	if err == nil || !strings.Contains(err.Error(), "SKILL_IMAGE_NOT_APPROVED") {
		t.Fatalf("a refused run must fail with its code: %v", err)
	}
}

// A report whose stored bytes no longer verify is never rendered.
func TestSkillsRun_AnUnverifiedReportIsNotShown(t *testing.T) {
	_, deps := skillRunCLI(t, http.StatusAccepted, startAccepted,
		runDetail(t, staticRunBody, "succeeded", "mismatch"))
	out, _, err := executeCLI(t, deps, "skills", "run", "security-audit", "--project", "medusa")
	if err == nil || !strings.Contains(err.Error(), "did not verify") || strings.Contains(out, "coverage") {
		t.Fatalf("an unverified report was rendered: err=%v\n%s", err, out)
	}
}

// The project id is a path segment and is escaped like one.
func TestSkillsRun_EscapesTheProjectID(t *testing.T) {
	log, deps := skillRunCLI(t, http.StatusAccepted, startAccepted)
	if _, _, err := executeCLI(t, deps, "skills", "run", "security-audit",
		"--project", "team a/b?x", "--no-wait"); err != nil {
		t.Fatalf("run: %v", err)
	}
	reqs := log.all()
	if len(reqs) != 1 || reqs[0].path != "/api/v1/projects/team%20a%2Fb%3Fx/skills/security-audit/run" {
		t.Fatalf("requests: %+v", reqs)
	}
}

// --no-wait returns after the start and sends the idempotency key.
func TestSkillsRun_NoWaitSendsTheKeyAndReturns(t *testing.T) {
	log, deps := skillRunCLI(t, http.StatusAccepted, startAccepted)
	out, _, err := executeCLI(t, deps, "skills", "run", "security-audit", "--project", "medusa",
		"--no-wait", "--idempotency-key", "ci-42")
	if err != nil || !strings.Contains(out, "skr-1") {
		t.Fatalf("run: %v\n%s", err, out)
	}
	reqs := log.all()
	if len(reqs) != 1 || !strings.Contains(reqs[0].body, `"idempotencyKey":"ci-42"`) {
		t.Fatalf("requests: %+v", reqs)
	}
}

func TestSkillsRuns_ListsTheHistory(t *testing.T) {
	capture, deps := skillImagesCLI(t, http.StatusOK,
		`{"runs":[{"id":"skr-2","state":"succeeded","skillId":"security-audit","version":"1.2.0",
		  "modeId":"static-code","createdAt":"2026-09-23T10:00:00Z","durationMs":1500,
		  "summary":"ao.static-scan/v1 scanned 2 of 3 staged files, 1 findings"},
		 {"id":"skr-1","state":"failed","skillId":"security-audit","version":"1.2.0",
		  "modeId":"static-code","createdAt":"2026-09-23T09:00:00Z",
		  "errorCode":"SKILL_RUN_INTERRUPTED","errorMessage":"the daemon stopped"}]}`)
	out, _, err := executeCLI(t, deps, "skills", "runs", "--project", "medusa", "--limit", "5")
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if capture.path != "/api/v1/projects/medusa/skills/runs?limit=5" {
		t.Fatalf("path %s", capture.path)
	}
	for _, want := range []string{"skr-2", "succeeded", "1.5s", "skr-1", "SKILL_RUN_INTERRUPTED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("history is missing %q:\n%s", want, out)
		}
	}
}
