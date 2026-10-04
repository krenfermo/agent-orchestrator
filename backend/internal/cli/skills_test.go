package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// skillsCapture records what the CLI sent, so each test can assert the wire
// call rather than only the rendered output.
type skillsCapture struct {
	method string
	path   string
	body   string
}

// skillsCLIServer is a fake daemon that answers the catalog routes.
func skillsCLIServer(t *testing.T, capture *skillsCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capture.method = r.Method
		capture.path = r.URL.RequestURI()
		capture.body = string(raw)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/skills":
			_, _ = io.WriteString(w, `{"skills":[{"id":"security-audit","version":"0.1.0",`+
				`"name":"Security Audit","riskLevel":"critical","modes":[{"id":"static-code"}]}],`+
				`"capabilities":[{"name":"repo.read","risk":"low","requiredPermission":"project.read"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/skills":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"security-audit","version":"0.1.0","digest":"abc123"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/skills/security-audit/versions/0.1.0":
			_, _ = io.WriteString(w, `{"id":"security-audit","version":"0.1.0","name":"Security Audit",`+
				`"description":"On-demand audit","riskLevel":"critical","originType":"builtin",`+
				`"publisher":"agent-orchestrator","digest":"abc123","approval":"per_activation",`+
				`"requiresIsolatedRunner":true,"capabilities":["repo.read","net.egress"],`+
				`"modes":[{"id":"static-code","riskLevel":"low","approval":"per_activation",`+
				`"capabilities":["repo.read"]}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/skills/security-audit/versions/0.1.0":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/skills/security-audit/audit":
			_, _ = io.WriteString(w, `{"entries":[{"occurredAt":"2026-09-08T10:00:00Z","actor":"",`+
				`"action":"install","skillId":"security-audit","version":"0.1.0",`+
				`"capabilities":[],"detail":"installed from /tmp/pkg"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/medusa/skills":
			_, _ = io.WriteString(w, `{"projectId":"medusa","installed":[],"activations":[`+
				`{"skillId":"security-audit","version":"0.1.0","enabled":true,`+
				`"grantedCapabilities":["repo.read"],"available":true}],"permissions":["project.read"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/projects/medusa/skills/security-audit":
			_, _ = io.WriteString(w, `{"skillId":"security-audit","version":"0.1.0","enabled":true,`+
				`"grantedCapabilities":["repo.read","report.write"],"available":true}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/medusa/skills/security-audit":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/medusa/skills/security-audit/dry-run":
			_, _ = io.WriteString(w, `{"skillId":"security-audit","version":"0.1.0","modeId":"dependencies",`+
				`"modeRisk":"medium","verdict":"blocked","requiredApproval":"per_run","decisions":[`+
				`{"capability":"repo.read","satisfied":true,"risk":"low","description":"Read the source."},`+
				`{"capability":"net.egress","satisfied":false,"risk":"high",`+
				`"denialReason":"needs_isolated_runner","detail":"no runner attests an isolated execution environment"}],`+
				`"missingPermissions":["project.manage"],"runner":{"runnerId":"none","available":false,`+
				`"isolated":false,"egressControlled":false,"needsIsolation":true,"needsEgressControl":true},`+
				`"reasons":["net.egress: no runner attests an isolated execution environment (needs_isolated_runner)"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func skillsCLI(t *testing.T) (*skillsCapture, Deps) {
	t.Helper()
	cfg := setConfigEnv(t)
	capture := &skillsCapture{}
	srv := skillsCLIServer(t, capture)
	writeRunFileFor(t, cfg, srv)
	return capture, Deps{ProcessAlive: func(int) bool { return true }}
}

func TestSkillsList_RendersTheCatalogAndOneProject(t *testing.T) {
	capture, deps := skillsCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "list")
	if err != nil {
		t.Fatalf("list: %v (%s)", err, errOut)
	}
	if !strings.Contains(out, "security-audit@0.1.0") || !strings.Contains(out, "risk=critical") {
		t.Fatalf("list output = %q", out)
	}
	if capture.path != "/api/v1/skills" {
		t.Fatalf("path = %q", capture.path)
	}

	out, errOut, err = executeCLI(t, deps, "skills", "list", "--project", "medusa")
	if err != nil {
		t.Fatalf("project list: %v (%s)", err, errOut)
	}
	if !strings.Contains(out, "enabled") || !strings.Contains(out, "granted=[repo.read]") {
		t.Fatalf("project list output = %q", out)
	}
	if capture.path != "/api/v1/projects/medusa/skills" {
		t.Fatalf("path = %q", capture.path)
	}
}

// Install resolves a relative path locally, because the daemon requires an
// absolute one and may not share this shell's working directory.
func TestSkillsInstall_SendsAnAbsolutePathAndSaysNothingIsEnabled(t *testing.T) {
	capture, deps := skillsCLI(t)
	dir := t.TempDir()

	out, errOut, err := executeCLI(t, deps, "skills", "install", dir)
	if err != nil {
		t.Fatalf("install: %v (%s)", err, errOut)
	}
	if !strings.Contains(out, "installed security-audit@0.1.0") {
		t.Fatalf("install output = %q", out)
	}
	if !strings.Contains(out, "enabled on no project") {
		t.Fatalf("install output must say installing enables nothing: %q", out)
	}
	var sent installSkillRequest
	if err := json.Unmarshal([]byte(capture.body), &sent); err != nil {
		t.Fatalf("decode request: %v (%s)", err, capture.body)
	}
	if !filepath.IsAbs(sent.SourceDir) {
		t.Fatalf("sourceDir %q is not absolute", sent.SourceDir)
	}
}

func TestSkillsEnable_RequiresAPinnedVersionAndAnExplicitGrant(t *testing.T) {
	capture, deps := skillsCLI(t)

	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"no project", []string{"skills", "enable", "security-audit", "--version", "0.1.0", "--capability", "repo.read"}, "--project is required"},
		{"no version", []string{"skills", "enable", "security-audit", "--project", "medusa", "--capability", "repo.read"}, "always pinned"},
		{"no capability", []string{"skills", "enable", "security-audit", "--project", "medusa", "--version", "0.1.0"}, "at least one --capability"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := executeCLI(t, deps, tc.args...)
			if err == nil {
				t.Fatal("command was accepted")
			}
			// A usage error must exit 2, not 1 -- the convention AGENTS.md
			// sets for CLI misuse.
			if got := ExitCode(err); got != 2 {
				t.Fatalf("exit code = %d, want 2 (err %v)", got, err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
		})
	}

	out, errOut, err := executeCLI(t, deps, "skills", "enable", "security-audit",
		"--project", "medusa", "--version", "0.1.0",
		"--capability", "repo.read", "--capability", "report.write")
	if err != nil {
		t.Fatalf("enable: %v (%s)", err, errOut)
	}
	if !strings.Contains(out, "enabled security-audit@0.1.0 on medusa") {
		t.Fatalf("enable output = %q", out)
	}
	if !strings.Contains(out, "nothing runs until you ask it to") {
		t.Fatalf("enable output must say enabling runs nothing: %q", out)
	}
	var sent enableSkillRequest
	if err := json.Unmarshal([]byte(capture.body), &sent); err != nil {
		t.Fatalf("decode request: %v (%s)", err, capture.body)
	}
	if sent.Version != "0.1.0" || len(sent.Capabilities) != 2 {
		t.Fatalf("request = %#v", sent)
	}
}

func TestSkillsDisable_SaysTheGrantIsRevoked(t *testing.T) {
	_, deps := skillsCLI(t)
	out, errOut, err := executeCLI(t, deps, "skills", "disable", "security-audit", "--project", "medusa")
	if err != nil {
		t.Fatalf("disable: %v (%s)", err, errOut)
	}
	if !strings.Contains(out, "its grant is revoked") {
		t.Fatalf("disable output = %q", out)
	}
}

// The dry run has to render the one thing that matters: this cannot run, and
// here is exactly why.
func TestSkillsDryRun_ReportsTheBlockerAndTheRunner(t *testing.T) {
	capture, deps := skillsCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "dry-run", "security-audit",
		"--project", "medusa", "--mode", "dependencies")
	if err != nil {
		t.Fatalf("dry-run: %v (%s)", err, errOut)
	}
	for _, want := range []string{
		"BLOCKED",
		"security-audit@0.1.0",
		"mode=dependencies",
		"approval required: per_run",
		"ok  repo.read",
		"NO  net.egress",
		"missing permissions: project.manage",
		"runner: none (isolated=false egress-controlled=false); this mode needs isolation and egress control",
		"blocked: net.egress",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output is missing %q:\n%s", want, out)
		}
	}
	// The mode is filled into the inputs so a caller does not pass it twice.
	var sent skillDryRunRequest
	if err := json.Unmarshal([]byte(capture.body), &sent); err != nil {
		t.Fatalf("decode request: %v (%s)", err, capture.body)
	}
	if sent.ModeID != "dependencies" || sent.Inputs["mode"] != "dependencies" {
		t.Fatalf("request = %#v", sent)
	}

	_, _, err = executeCLI(t, deps, "skills", "dry-run", "security-audit", "--mode", "static-code")
	if err == nil || !strings.Contains(err.Error(), "--project is required") {
		t.Fatalf("err = %v", err)
	}
	_, _, err = executeCLI(t, deps, "skills", "dry-run", "security-audit",
		"--project", "medusa", "--input", "bogus")
	if err == nil || !strings.Contains(err.Error(), "must be key=value") {
		t.Fatalf("err = %v", err)
	}
}

func TestSkillsShow_RendersModesAndTheRunnerCaveat(t *testing.T) {
	_, deps := skillsCLI(t)

	_, _, err := executeCLI(t, deps, "skills", "show", "security-audit")
	if err == nil || !strings.Contains(err.Error(), "--version is required") {
		t.Fatalf("err = %v", err)
	}

	out, errOut, err := executeCLI(t, deps, "skills", "show", "security-audit", "--version", "0.1.0")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, errOut)
	}
	for _, want := range []string{
		"security-audit@0.1.0",
		"digest:     abc123",
		"requires an isolated runner: AO has none",
		"static-code",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output is missing %q:\n%s", want, out)
		}
	}
}

func TestSkillsAudit_RendersTheInstallationScope(t *testing.T) {
	_, deps := skillsCLI(t)
	out, errOut, err := executeCLI(t, deps, "skills", "audit", "security-audit")
	if err != nil {
		t.Fatalf("audit: %v (%s)", err, errOut)
	}
	// An empty actor is the unauthenticated loopback caller, and must read as
	// something other than a blank column.
	if !strings.Contains(out, "install") || !strings.Contains(out, "(installation)") ||
		!strings.Contains(out, "(local)") {
		t.Fatalf("audit output = %q", out)
	}
}

func TestSkillsUninstall_RequiresAVersion(t *testing.T) {
	_, deps := skillsCLI(t)
	_, _, err := executeCLI(t, deps, "skills", "uninstall", "security-audit")
	if err == nil || !strings.Contains(err.Error(), "--version is required") {
		t.Fatalf("err = %v", err)
	}
	out, errOut, err := executeCLI(t, deps, "skills", "uninstall", "security-audit", "--version", "0.1.0")
	if err != nil {
		t.Fatalf("uninstall: %v (%s)", err, errOut)
	}
	if !strings.Contains(out, "uninstalled security-audit@0.1.0") {
		t.Fatalf("uninstall output = %q", out)
	}
}
