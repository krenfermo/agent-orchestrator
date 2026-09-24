//go:build !windows

package skillruns

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// scanners_e2e_test.go is Frente 2 / 2D's end-to-end test of the
// deterministic scanners through a REAL daemon, REAL SQLite and a REAL
// container runtime:
//
//	project -> security-audit -> secret-scan | dependencies -> Docker ->
//	findings -> SkillRun -> restart -> history
//
// with a positive project (planted, obviously fake credentials and dependency
// problems) and a clean one (where any finding would be invented), plus the
// static-code regression on the same daemon. Same gate and shared root as 2B:
//
//	AO_SKILL_RUN_E2E=1 AO_SKILL_RUN_E2E_SHARED_ROOT=<a path the runtime shares> \
//	  go test ./e2e/skillruns/ -run Scanners -v -count=1 -timeout 20m

const (
	// Planted values. Each is fake and shaped only to trip its rule; none may
	// appear in the database, its WAL, a stored report or the daemon log.
	e2eAWSKey   = "AKIAQ7E2EFAKE0000KEY"
	e2eGHToken  = "ghp_E2EFAKE000000000000000000000000000zz"
	e2ePassword = "E2E-2D-Fake-Passw0rd-77"
	e2eEnvValue = "E2E-2D-ENV-VALUE-NEVER-READ"
	e2eGitToken = "ghp_E2EGITURL0000000000000000000000000yy"
)

func (s *scratch) initScannerRepo(dir string) {
	s.t.Helper()
	files := map[string]string{
		"app/config.go": "package app\n\nvar awsKey = \"" + e2eAWSKey + "\"\n",
		"scripts/ci.sh": "#!/bin/sh\nexport GH_TOKEN=" + e2eGHToken + "\n",
		"app/db.py":     "password = \"" + e2ePassword + "\"\n",
		"docs/setup.md": "Use postgres://svc:" + e2ePassword + "@db.internal:5432/app\n",
		".env":          "API_TOKEN=" + e2eEnvValue + "\n",
		"certs/tls.pem": "-----BEGIN PRIVATE KEY-----\nE2EFAKE\n-----END PRIVATE KEY-----\n",
		"app/main.go":   "package app\n\nfunc main() {}\n",
		"web/package.json": "{\n  \"name\": \"web\",\n  \"dependencies\": {\n" +
			"    \"express\": \"4.19.2\",\n" +
			"    \"left-pad\": \"git+https://bot:" + e2eGitToken + "@github.com/acme/left-pad.git\",\n" +
			"    \"chaos\": \"latest\"\n  }\n}\n",
		"web/package-lock.json": "{\"lockfileVersion\": 3}\n",
		"tools/package.json":    "{\n  \"dependencies\": {\n    \"chalk\": \"5.3.0\"\n  }\n}\n",
		"requirements.txt":      "--index-url http://pypi.internal/simple\nrequests==2.32.3\nflask\n",
		"go.mod":                "module example.com/x\n\nrequire github.com/google/uuid v1.6.0\n",
		"go.sum":                "github.com/google/uuid v1.6.0 h1:x\n",
	}
	for rel, body := range files {
		writeFile(s.t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}
	s.git(dir, "init", "-q", "-b", "main")
	s.git(dir, "add", "-A")
	s.git(dir, "commit", "-qm", "init")
}

func (s *scratch) initCleanRepo(dir string) {
	s.t.Helper()
	files := map[string]string{
		"app/main.go":       "package app\n\nimport \"os\"\n\nvar key = os.Getenv(\"API_KEY\")\n",
		"package.json":      "{\n  \"dependencies\": {\n    \"express\": \"4.19.2\"\n  }\n}\n",
		"package-lock.json": "{\"lockfileVersion\": 3}\n",
		"requirements.txt":  "requests==2.32.3\n",
		"go.mod":            "module x\n\nrequire github.com/google/uuid v1.6.0\n",
		"go.sum":            "github.com/google/uuid v1.6.0 h1:x\n",
		"README.md":         "# clean\n",
	}
	for rel, body := range files {
		writeFile(s.t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}
	s.git(dir, "init", "-q", "-b", "main")
	s.git(dir, "add", "-A")
	s.git(dir, "commit", "-qm", "init")
}

func (s *scratch) approveTool(project, version, mode, tool, digest string) {
	s.t.Helper()
	s.expect(http.MethodPost, "/api/v1/skills/images", map[string]any{
		"tenantId": "tnt_default", "projectId": project, "skillId": "security-audit",
		"version": version, "modeId": mode, "tool": tool,
		"reference": "alpine", "digest": digest,
		"note": "E2E 2D: the local alpine:3.19 the runner's live tests use", "confirm": true,
	}, http.StatusCreated, nil)
}

type scanDetail struct {
	Run struct {
		ID           string `json:"id"`
		State        string `json:"state"`
		Tool         string `json:"tool"`
		ReportSHA256 string `json:"reportSha256"`
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
		FindingCount int    `json:"findingCount"`
	} `json:"run"`
	Findings []struct {
		RuleID     string `json:"ruleId"`
		Path       string `json:"path"`
		Confidence string `json:"confidence"`
		Category   string `json:"category"`
		Title      string `json:"title"`
	} `json:"findings"`
	Report    json.RawMessage `json:"report"`
	Integrity string          `json:"integrity"`
}

func (s *scratch) runScan(project, mode string) scanDetail {
	s.t.Helper()
	sv, code, ecode := s.startRun(project, mode, "")
	if code != http.StatusAccepted {
		s.t.Fatalf("%s on %s: start = %d %s", mode, project, code, ecode)
	}
	s.waitRun(project, sv.Run.ID)
	return s.getScan(project, sv.Run.ID)
}

func (s *scratch) getScan(project, runID string) scanDetail {
	s.t.Helper()
	var d scanDetail
	s.expect(http.MethodGet, "/api/v1/projects/"+project+"/skills/runs/"+runID, nil, http.StatusOK, &d)
	return d
}

func ruleCounts(d scanDetail) map[string]int {
	out := map[string]int{}
	for _, f := range d.Findings {
		out[f.RuleID+" "+f.Path]++
	}
	return out
}

var plantedValues = []string{e2eAWSKey, e2eGHToken, e2ePassword, e2eEnvValue, e2eGitToken, "E2EFAKE\n"}

func TestScannersAreDurableThroughARealDaemonAndDocker(t *testing.T) {
	s := newScratch(t)
	scan := filepath.Join(s.root, "projects", "scanme")
	clean := filepath.Join(s.root, "projects", "clean")
	third := filepath.Join(s.root, "projects", "third")
	s.initScannerRepo(scan)
	s.initCleanRepo(clean)
	s.initCleanRepo(third)
	containersBefore := len(skillRunContainers(t))

	s.startDaemon()
	for id, p := range map[string]string{"scanme": scan, "clean": clean, "third": third} {
		s.expect(http.MethodPost, "/api/v1/projects", map[string]any{"path": p, "projectId": id, "name": id}, http.StatusCreated, nil)
	}
	var catalog struct {
		Skills []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"skills"`
	}
	s.expect(http.MethodGet, "/api/v1/skills", nil, http.StatusOK, &catalog)
	version := ""
	for _, sk := range catalog.Skills {
		if sk.ID == "security-audit" && sk.Version > version {
			version = sk.Version
		}
	}
	if version != shippedVersion(t) {
		t.Fatalf("security-audit available at %q, want %s", version, shippedVersion(t))
	}
	caps := []string{"repo.read", "deps.read", "report.write"}
	for _, p := range []string{"scanme", "clean"} {
		s.expect(http.MethodPut, "/api/v1/projects/"+p+"/skills/security-audit",
			map[string]any{"version": version, "capabilities": caps}, http.StatusOK, nil)
	}
	digest := imageDigest(t)
	for _, p := range []string{"scanme", "clean"} {
		s.approveTool(p, version, "secret-scan", "ao.secret-scan/v1", digest)
		s.approveTool(p, version, "dependencies", "ao.dependency-scan/v1", digest)
		s.approveTool(p, version, "static-code", "ao.static-scan/v1", digest)
	}

	// ---- secret-scan: every planted shape found, no value stored ----
	sec := s.runScan("scanme", "secret-scan")
	if sec.Run.State != "succeeded" || sec.Run.Tool != "ao.secret-scan/v1" || sec.Integrity != "verified" {
		t.Fatalf("secret-scan ended %s %s: %s", sec.Run.State, sec.Run.ErrorCode, sec.Run.ErrorMessage)
	}
	got := ruleCounts(sec)
	for _, want := range []string{
		"SEC-001 app/config.go", "SEC-003 scripts/ci.sh", "SEC-011 app/db.py", "SEC-010 docs/setup.md",
		"SEC-100 .env", "SEC-100 certs/tls.pem",
	} {
		if got[want] == 0 {
			t.Fatalf("secret-scan missed %q; findings %v", want, got)
		}
	}
	for k := range got {
		if strings.HasSuffix(k, "app/main.go") || strings.HasPrefix(k, "SEC-002 certs/") {
			t.Fatalf("secret-scan reported %q: a clean file, or the contents of a denied file", k)
		}
	}
	secReport := string(sec.Report)
	for _, v := range plantedValues {
		if strings.Contains(secReport, v) {
			t.Fatalf("the secret-scan report stores %q", v)
		}
	}

	// ---- dependencies: exactly the planted facts, an inventory, no token ----
	dep := s.runScan("scanme", "dependencies")
	if dep.Run.State != "succeeded" || dep.Run.Tool != "ao.dependency-scan/v1" || dep.Integrity != "verified" {
		t.Fatalf("dependencies ended %s %s: %s", dep.Run.State, dep.Run.ErrorCode, dep.Run.ErrorMessage)
	}
	want := map[string]int{
		"DEP-001 web/package.json": 1, "DEP-002 web/package.json": 1, "DEP-003 tools/package.json": 1,
		"DEP-004 requirements.txt": 1, "DEP-002 requirements.txt": 1,
	}
	gotDep := ruleCounts(dep)
	if len(dep.Findings) != len(want) {
		t.Fatalf("dependencies found %v, want exactly %v", gotDep, want)
	}
	for k, n := range want {
		if gotDep[k] != n {
			t.Fatalf("dependencies found %v, want exactly %v", gotDep, want)
		}
	}
	for _, f := range dep.Findings {
		if f.Confidence != "confirmed" || strings.Contains(strings.ToLower(f.Title), "vulnerab") {
			t.Fatalf("a dependency finding overclaims: %+v", f)
		}
	}
	var depReport struct {
		Inventory struct {
			Total       int            `json:"total"`
			ByEcosystem map[string]int `json:"byEcosystem"`
		} `json:"inventory"`
		Coverage struct {
			FilesStaged int `json:"filesStaged"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal(dep.Report, &depReport); err != nil {
		t.Fatal(err)
	}
	if depReport.Inventory.Total != 7 || depReport.Inventory.ByEcosystem["npm"] != 4 ||
		depReport.Inventory.ByEcosystem["pypi"] != 2 || depReport.Inventory.ByEcosystem["go"] != 1 {
		t.Fatalf("inventory = %+v", depReport.Inventory)
	}
	if depReport.Coverage.FilesStaged != 6 {
		t.Fatalf("the dependency scan staged %d files; only the 6 manifests/lockfiles may be staged",
			depReport.Coverage.FilesStaged)
	}
	if strings.Contains(string(dep.Report), e2eGitToken) {
		t.Fatal("the token in a git dependency URL was stored")
	}

	// ---- regression: static-code still runs, and now honours the deny list ----
	st := s.runScan("scanme", "static-code")
	if st.Run.State != "succeeded" || st.Run.Tool != "ao.static-scan/v1" {
		t.Fatalf("static-code ended %s %s: %s", st.Run.State, st.Run.ErrorCode, st.Run.ErrorMessage)
	}
	if !strings.Contains(string(st.Report), `"path":".env","reason":"denied-by-manifest"`) {
		t.Fatal("static-code staged a file the manifest denies")
	}

	// ---- the clean project: zero findings from both scanners ----
	for _, mode := range []string{"secret-scan", "dependencies"} {
		d := s.runScan("clean", mode)
		if d.Run.State != "succeeded" || len(d.Findings) != 0 {
			t.Fatalf("%s on a clean project: %s, findings %v", mode, d.Run.State, ruleCounts(d))
		}
	}

	// ---- negatives ----
	// No image approved for this project -> accepted, ends refused.
	s.expect(http.MethodPut, "/api/v1/projects/third/skills/security-audit",
		map[string]any{"version": version, "capabilities": []string{"repo.read", "report.write"}}, http.StatusOK, nil)
	if d := s.runScan("third", "secret-scan"); d.Run.State != "refused" || d.Run.ErrorCode != "SKILL_IMAGE_NOT_APPROVED" {
		t.Fatalf("secret-scan without an approval ended %s %s", d.Run.State, d.Run.ErrorCode)
	}
	// deps.read never granted -> refused before acceptance, no run.
	if _, code, ecode := s.startRun("third", "dependencies", ""); code != http.StatusForbidden || ecode != "SKILL_RUN_REFUSED" {
		t.Fatalf("dependencies without deps.read answered %d %s", code, ecode)
	}

	// ---- restart: every run and its verified report survive ----
	history := map[string]string{sec.Run.ID: sec.Run.ReportSHA256, dep.Run.ID: dep.Run.ReportSHA256, st.Run.ID: st.Run.ReportSHA256}
	s.stopDaemon(syscall.SIGTERM)
	s.startDaemon()
	for id, sha := range history {
		d := s.getScan("scanme", id)
		if d.Run.State != "succeeded" || d.Integrity != "verified" || d.Run.ReportSHA256 != sha {
			t.Fatalf("after restart run %s: %s %s sha %s", id, d.Run.State, d.Integrity, d.Run.ReportSHA256)
		}
	}
	if n := len(s.listRuns("scanme")); n != 3 {
		t.Fatalf("history holds %d runs, want 3", n)
	}
	s.stopDaemon(syscall.SIGTERM)

	// ---- nothing left behind, nothing stored in clear ----
	if n := len(skillRunContainers(t)); n != containersBefore {
		t.Fatalf("%d skill-run containers left behind", n-containersBefore)
	}
	entries, _ := os.ReadDir(filepath.Join(s.shared, ".ao-skill-staging"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "skr-") {
			t.Fatalf("a staged copy survived: %s", e.Name())
		}
	}
	checkScannerDatabase(t, s.dataDir, s.root, sec.Run.ID)
}

func checkScannerDatabase(t *testing.T, dataDir, root, runID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db")+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var report, sum string
	if err := db.QueryRow(`SELECT report_json, report_sha256 FROM skill_runs WHERE id = ?`, runID).Scan(&report, &sum); err != nil {
		t.Fatal(err)
	}
	got := sha256.Sum256([]byte(report))
	if hex.EncodeToString(got[:]) != sum {
		t.Fatal("stored report bytes do not hash to the stored digest")
	}
	// The database, its WAL and the daemon log, as raw bytes.
	for _, p := range []string{filepath.Join(dataDir, "ao.db"), filepath.Join(dataDir, "ao.db-wal"), filepath.Join(root, "daemon.log")} {
		b, err := os.ReadFile(p) //nolint:gosec // test.
		if err != nil {
			continue
		}
		for _, v := range plantedValues {
			if strings.Contains(string(b), v) {
				t.Fatalf("planted value %q found in %s", v, filepath.Base(p))
			}
		}
	}
}
