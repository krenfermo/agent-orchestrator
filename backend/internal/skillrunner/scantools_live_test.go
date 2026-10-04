package skillrunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scantools_live_test.go runs the 2D tools in the REAL container, because the
// engine that matters is busybox grep and busybox awk inside alpine, not Go's
// regexp. Every credential below is fake and shaped only to match its rule.

const (
	fakeAWS    = "AKIAQ7FAKEKEY0000EXA"
	fakeGitHub = "ghp_FAKE0000000000000000000000000000abcd"
	fakePasswd = "Sup3r-Fake-Passw0rd-2D"
	fakeEnvVal = "FAKE-ENV-VALUE-NEVER-READ"
	fakeGitTok = "ghp_FAKEGITURL0000000000000000000000wxyz"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(repoScratchRoot(t), "proj-"+randomToken())
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func runTool(t *testing.T, tool Tool, mode, project string, deny []string) StaticScanReport {
	t.Helper()
	r := liveRunner(t)
	requireAlpine(t, r)
	scope := testScope()
	scope.ModeID = mode
	auth := newFakeAuthority()
	a := approvalFor(scope, hostAlpineDigest(t, r))
	a.Tool = string(tool)
	auth.put(a)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	report, err := r.RunStaticScan(ctx, auth, StaticScanRequest{
		Scope: scope, Tool: tool, DenyGlobs: deny,
		ProjectID: "p", ProjectPath: project, StagingRootOverride: stagingOverride(t),
		Limits: Limits{Wall: 90 * time.Second, MemoryBytes: 256 << 20, CPUs: 1, MaxPIDs: 64, MaxOutputBytes: 256 << 10},
	})
	if err != nil {
		t.Fatalf("RunStaticScan(%s): %v", tool, err)
	}
	return report
}

func reportJSON(t *testing.T, r StaticScanReport) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var manifestDeny = []string{".env", ".env.*", "**/*.pem", "**/*.key", "**/id_rsa*"}

func TestLiveSecretScan_FindsShapesNeverStoresValuesAndNeverReadsDeniedFiles(t *testing.T) {
	project := writeTree(t, map[string]string{
		"app/config.go":    "package app\n\nvar awsKey = \"" + fakeAWS + "\"\n",
		"scripts/ci.sh":    "export GH=" + fakeGitHub + "\n",
		"app/db.py":        "password = \"" + fakePasswd + "\"\n",
		"docs/setup.md":    "Connect to postgres://admin:" + fakePasswd + "@db.internal:5432/app\n",
		".npmrc":           "//registry.npmjs.org/:_authToken=" + fakeGitHub + "\n",
		"certs/server.pem": "-----BEGIN PRIVATE KEY-----\nFAKE\n-----END PRIVATE KEY-----\n",
		".env":             "API_TOKEN=" + fakeEnvVal + "\n",
		"app/clean.go":     "package app\n\n// reads the key from the environment\nvar key = os.Getenv(\"API_KEY\")\n",
	})
	report := runTool(t, ToolSecretScan, "secret-scan", project, manifestDeny)

	if report.SchemaVersion != "ao.secret-scan/v1" {
		t.Fatalf("schema = %q", report.SchemaVersion)
	}
	byRule := map[string][]string{}
	for _, f := range report.Findings {
		byRule[f.RuleID] = append(byRule[f.RuleID], f.Path)
	}
	for rule, path := range map[string]string{
		"SEC-001": "app/config.go", "SEC-003": "scripts/ci.sh", "SEC-010": "docs/setup.md",
		"SEC-011": "app/db.py", "SEC-012": ".npmrc", "SEC-100": ".env",
	} {
		if !strings.Contains(strings.Join(byRule[rule], ","), path) {
			t.Fatalf("%s did not fire on %s; findings: %+v", rule, path, report.Findings)
		}
	}
	if !strings.Contains(strings.Join(byRule["SEC-100"], ","), "certs/server.pem") {
		t.Fatalf("a denied private key file was not reported present: %v", byRule["SEC-100"])
	}
	// SEC-002 must NOT fire on server.pem: the file is denied and never read.
	if strings.Contains(strings.Join(byRule["SEC-002"], ","), "server.pem") {
		t.Fatal("a denied file was read")
	}
	for _, f := range report.Findings {
		if f.Path == "app/clean.go" {
			t.Fatalf("a clean file produced a finding: %+v", f)
		}
	}
	body := reportJSON(t, report)
	for _, v := range []string{fakeAWS, fakeGitHub, fakePasswd, fakeEnvVal, "FAKE\n"} {
		if strings.Contains(body, v) {
			t.Fatalf("the report carries a secret value %q", v)
		}
	}
}

func TestLiveDependencyScan_InventoriesAndReportsOnlyFacts(t *testing.T) {
	project := writeTree(t, map[string]string{
		// npm, locked, with a git dependency carrying a token in its URL.
		"web/package.json": `{
  "name": "web",
  "dependencies": {
    "express": "^4.19.2",
    "left-pad": "git+https://user:` + fakeGitTok + `@github.com/acme/left-pad.git",
    "chaos": "*"
  },
  "devDependencies": {
    "vitest": "1.6.0"
  }
}
`,
		"web/package-lock.json": `{"lockfileVersion": 3, "packages": {}}` + "\n",
		// npm, NOT locked.
		"tools/package.json": `{
  "name": "tools",
  "dependencies": {
    "chalk": "5.3.0"
  }
}
`,
		// go, locked.
		"go.mod": "module example.com/x\n\ngo 1.22\n\nrequire (\n\tgithub.com/google/uuid v1.6.0\n\tgolang.org/x/text v0.14.0 // indirect\n)\n\nrequire github.com/pkg/errors v0.9.1\n",
		"go.sum": "github.com/google/uuid v1.6.0 h1:x\n",
		// pip, one pinned, one floating, and a plain-HTTP index.
		"requirements.txt": "--index-url http://pypi.internal/simple\nrequests==2.32.3\nflask>=2.0\n",
		"Cargo.toml":       "[package]\nname = \"c\"\n\n[dependencies]\nserde = \"1.0\"\nrand = { git = \"https://github.com/rust-random/rand\" }\n",
		"Gemfile":          "gem 'rails'\n",
		"src/main.go":      "package main\n\nfunc main() {}\n",
	})
	report := runTool(t, ToolDependencyScan, "dependencies", project, manifestDeny)

	inv := report.Inventory
	if inv == nil {
		t.Fatal("no inventory")
	}
	if inv.ByEcosystem["npm"] != 5 || inv.ByEcosystem["go"] != 3 || inv.ByEcosystem["pypi"] != 2 || inv.ByEcosystem["cargo"] != 2 {
		t.Fatalf("inventory counts = %v (entries %+v)", inv.ByEcosystem, inv.Entries)
	}
	if len(inv.Unparsed) != 1 || inv.Unparsed[0] != "Gemfile" {
		t.Fatalf("unparsed = %v", inv.Unparsed)
	}
	// The dependency scan never receives source code.
	if report.Coverage.FilesStaged != 7 {
		t.Fatalf("staged %d files; only manifests and lockfiles may be staged", report.Coverage.FilesStaged)
	}
	got := map[string]int{}
	for _, f := range report.Findings {
		got[f.RuleID+" "+f.Path]++
		if f.Confidence != "confirmed" {
			t.Fatalf("a dependency fact claimed %q", f.Confidence)
		}
		if f.Category != "dependency" && f.Category != "supply-chain" {
			t.Fatalf("a dependency finding claims category %q", f.Category)
		}
	}
	want := map[string]int{
		"DEP-001 web/package.json": 1, "DEP-002 web/package.json": 1,
		"DEP-003 tools/package.json": 1,
		"DEP-004 requirements.txt":   1, "DEP-002 requirements.txt": 1,
		"DEP-001 Cargo.toml": 1,
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("finding %q x%d, want x%d; all: %v", k, got[k], n, got)
		}
	}
	if len(report.Findings) != 6 {
		t.Fatalf("expected exactly the six planted facts, got %d: %v", len(report.Findings), got)
	}
	body := reportJSON(t, report)
	if strings.Contains(body, fakeGitTok) {
		t.Fatal("a token in a git dependency URL reached the report")
	}
	for _, claim := range []string{"CVE-", "GHSA-", "vulnerab"} {
		if strings.Contains(strings.ToLower(strings.Join(findingTitles(report), " ")), strings.ToLower(claim)) {
			t.Fatalf("a finding claims %q without an advisory source", claim)
		}
	}
}

// A clean, pinned, locked project yields an inventory and NO findings. A scan
// that found something here would be inventing it.
func TestLiveDependencyScan_CleanProjectHasNoFindings(t *testing.T) {
	project := writeTree(t, map[string]string{
		"package.json":      "{\n  \"dependencies\": {\n    \"express\": \"4.19.2\"\n  }\n}\n",
		"package-lock.json": "{}\n",
		"go.mod":            "module x\n\nrequire github.com/google/uuid v1.6.0\n",
		"go.sum":            "github.com/google/uuid v1.6.0 h1:x\n",
		"requirements.txt":  "requests==2.32.3\n",
	})
	report := runTool(t, ToolDependencyScan, "dependencies", project, manifestDeny)
	if len(report.Findings) != 0 {
		t.Fatalf("a clean project produced findings: %+v", report.Findings)
	}
	if report.Inventory == nil || report.Inventory.Total != 3 {
		t.Fatalf("inventory = %+v", report.Inventory)
	}
}

func findingTitles(r StaticScanReport) []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Title)
	}
	return out
}
