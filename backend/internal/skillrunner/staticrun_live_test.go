package skillrunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syntheticProject writes a small checkout with deliberate, obviously-fake
// problems. Nothing here is a real credential and nothing is a real host.
func syntheticProject(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(repoScratchRoot(t), "proj-"+randomToken())
	files := map[string]string{
		"api/handler.go": "package api\n\n" +
			"func find(db *DB, name string) {\n" +
			"\trows, _ := db.Query(\"SELECT * FROM users WHERE name = '\" + name + \"'\")\n" +
			"\t_ = rows\n}\n",
		"config/settings.py": "DEBUG = True\n" +
			"api_key = \"NOT-A-REAL-KEY-0123456789abcdef\"\n",
		"web/client.js": "const h = require('crypto').createHash('md5');\n" +
			"fetch(url, { rejectUnauthorized: false });\n",
		"README.md":          "# synthetic\nnot scanned: unsupported extension\n",
		"vendor/lib/huge.go": "package lib\n// excluded from staging\n",
		".git/config":        "[core]\n",
		"docs/notes.txt":     "also unsupported\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// stagingOverride keeps every test's staging inside the repository tree, which
// is the one path the container runtime is known to share here.
func stagingOverride(t *testing.T) string {
	t.Helper()
	return repoScratchRoot(t)
}

// The whole path, end to end, against a real container: stage a scope-limited
// copy, run AO's own tool inside the boundary, and get a report whose coverage
// says what was and was not read.
func TestLiveScan_ProducesAReportWithHonestCoverage(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	project := syntheticProject(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	auth, scope := liveApproval(t, r)
	report, err := r.RunStaticScan(ctx, auth, StaticScanRequest{
		Scope:     scope,
		ProjectID: "medusa", ProjectPath: project,
		StagingRootOverride: stagingOverride(t),
		Params:              DefaultToolParams(),
		Limits: Limits{
			Wall: 90 * time.Second, MemoryBytes: 256 << 20, CPUs: 1,
			MaxPIDs: 64, MaxOutputBytes: 256 << 10,
		},
	})
	if err != nil {
		t.Fatalf("RunStaticScan: %v", err)
	}

	// The scan actually read something, and AO can prove how much.
	if report.Coverage.FilesStaged == 0 {
		t.Fatal("nothing was staged")
	}
	if report.Coverage.FilesVisible != report.Coverage.FilesStaged {
		t.Fatalf("the container saw %d of %d staged files",
			report.Coverage.FilesVisible, report.Coverage.FilesStaged)
	}
	if report.Coverage.FilesScanned == 0 {
		t.Fatalf("no file was scanned: %+v", report.Coverage)
	}

	// It found the planted problems, by rule.
	found := map[string]bool{}
	for _, f := range report.Findings {
		found[f.RuleID] = true
		if f.Confidence != "possible" {
			t.Fatalf("a pattern match claimed confidence %q: %+v", f.Confidence, f)
		}
		if strings.HasPrefix(f.Path, "/work") || filepath.IsAbs(f.Path) {
			t.Fatalf("a finding cites a container path: %q", f.Path)
		}
		if f.Recommendation == "" {
			t.Fatalf("a finding has no recommendation: %+v", f)
		}
	}
	for _, want := range []string{"AOSS-001", "AOSS-006"} {
		if !found[want] {
			t.Fatalf("rule %s did not fire on the planted case; findings: %+v", want, report.Findings)
		}
	}

	// The report must never carry the matched text. The planted "key" is the
	// exact string a leaky implementation would echo.
	body := reportText(t, report)
	if strings.Contains(body, "NOT-A-REAL-KEY-0123456789abcdef") {
		t.Fatal("the report contains the matched credential value")
	}

	// Coverage says what was NOT read, so a short report cannot read as a
	// clean one.
	if len(report.Coverage.Skipped) == 0 {
		t.Fatalf("nothing was reported as skipped, but README.md and notes.txt are unsupported")
	}
	skipped := map[string]string{}
	for _, s := range report.Coverage.Skipped {
		skipped[s.Path] = s.Reason
	}
	if skipped["README.md"] != "unsupported_extension" {
		t.Fatalf("README.md skip reason = %q", skipped["README.md"])
	}
	if len(report.Coverage.RulesRun) != len(staticScanRules) {
		t.Fatalf("rulesRun = %v", report.Coverage.RulesRun)
	}
	if len(report.Coverage.Limitations) == 0 {
		t.Fatal("the report states no limitations, so an empty finding list would read as an assurance")
	}

	// The boundary held, and the report carries the proof rather than a claim.
	if report.Evidence.NetworkReachable {
		t.Fatal("the scan had network")
	}
	if report.Evidence.EffectiveUID == 0 || !report.Evidence.ReadOnlyRootFS {
		t.Fatalf("evidence = %+v", report.Evidence)
	}
	// The report names the bytes the RUNTIME resolved -- a bare digest, not a
	// name. A name would put back the mutable pointer the approval exists to
	// remove.
	if report.ImageDigest != hostAlpineDigest(t, r) {
		t.Fatalf("image = %q, want the approved digest %q", report.ImageDigest, hostAlpineDigest(t, r))
	}
	// And it names who allowed them. A report that says which bytes ran
	// without saying who approved them answers the less useful half.
	if report.ApprovalID != "img-1" || report.ApprovedBy != "ada" {
		t.Fatalf("report does not name the approval: %q by %q", report.ApprovalID, report.ApprovedBy)
	}
	if report.ApprovalRevokedDuringRun {
		t.Fatal("the approval was live throughout and the report says it was revoked")
	}
}

// Excluded directories never reach the container: .git holds every version of
// every file, so staging it would defeat a scope restriction outright.
func TestLiveScan_ExcludesGitAndVendorFromStaging(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	project := syntheticProject(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	auth, scope := liveApproval(t, r)
	report, err := r.RunStaticScan(ctx, auth, StaticScanRequest{
		Scope:     scope,
		ProjectID: "medusa", ProjectPath: project,
		StagingRootOverride: stagingOverride(t), Params: DefaultToolParams(),
		Limits: Limits{Wall: 60 * time.Second, MemoryBytes: 256 << 20, CPUs: 1, MaxPIDs: 64, MaxOutputBytes: 128 << 10},
	})
	if err != nil {
		t.Fatalf("RunStaticScan: %v", err)
	}
	all := reportText(t, report)
	for _, forbidden := range []string{".git", "vendor/"} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("%q reached the scan: %s", forbidden, all)
		}
	}
}

// Scope is enforced by what exists on the mount, not by an instruction.
func TestLiveScan_ScopeLimitsWhatIsStaged(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	project := syntheticProject(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	auth, scope := liveApproval(t, r)
	report, err := r.RunStaticScan(ctx, auth, StaticScanRequest{
		Scope:     scope,
		ProjectID: "medusa", ProjectPath: project, ScopePaths: []string{"api"},
		StagingRootOverride: stagingOverride(t), Params: DefaultToolParams(),
		Limits: Limits{Wall: 60 * time.Second, MemoryBytes: 256 << 20, CPUs: 1, MaxPIDs: 64, MaxOutputBytes: 128 << 10},
	})
	if err != nil {
		t.Fatalf("RunStaticScan: %v", err)
	}
	if report.Coverage.FilesStaged != 1 {
		t.Fatalf("scope api staged %d files", report.Coverage.FilesStaged)
	}
	body := reportText(t, report)
	if strings.Contains(body, "config/settings.py") || strings.Contains(body, "web/client.js") {
		t.Fatalf("out-of-scope files reached the scan: %s", body)
	}
	// The in-scope rule still fires. A one-file scope is the narrowest scan
	// somebody can ask for, and it must not silently find nothing.
	inScope := false
	for _, f := range report.Findings {
		if f.RuleID == "AOSS-006" {
			t.Fatalf("a rule fired on a file outside the scope: %+v", f)
		}
		if f.RuleID == "AOSS-001" && f.Path == "api/handler.go" {
			inScope = true
		}
	}
	if !inScope {
		t.Fatalf("the in-scope rule did not fire on a single-file scope: %+v", report.Findings)
	}
}

// Staging leaves nothing behind, on success or failure. A copy of somebody's
// source sitting beside their projects is exactly what gets discovered months
// later.
func TestLiveScan_CleansUpItsStaging(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	project := syntheticProject(t)
	root := stagingOverride(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	auth, scope := liveApproval(t, r)
	if _, err := r.RunStaticScan(ctx, auth, StaticScanRequest{
		Scope:     scope,
		ProjectID: "medusa", ProjectPath: project, StagingRootOverride: root,
		Params: DefaultToolParams(),
		Limits: Limits{Wall: 60 * time.Second, MemoryBytes: 256 << 20, CPUs: 1, MaxPIDs: 64, MaxOutputBytes: 128 << 10},
	}); err != nil {
		t.Fatalf("RunStaticScan: %v", err)
	}
	// The per-run directory must be gone. The root itself is kept on purpose
	// (see Staging.Cleanup) and must simply be empty.
	stagingRoot := filepath.Join(root, stagingDirName)
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		t.Fatalf("read staging root: %v", err)
	}
	if len(entries) > 0 {
		t.Fatalf("staged inputs survived the run: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(project, ".ao-skill-staging")); err == nil {
		t.Fatal("staging was written inside the project checkout")
	}
	if leftover := listSkillRunContainers(t, r); leftover != "" {
		t.Fatalf("containers survived: %s", leftover)
	}
}

// A tool AO does not ship a contract for is refused, and nothing runs.
func TestLiveScan_RefusesAnUnapprovedTool(t *testing.T) {
	r := liveRunner(t)
	_, err := r.resolveProbeContract(context.Background(), Tool("nmap"))
	if !errors.Is(err, ErrToolNotApproved) {
		t.Fatalf("err = %v, want ErrToolNotApproved", err)
	}
	if !strings.Contains(err.Error(), string(ToolStaticScan)) {
		t.Fatalf("the refusal should list what IS approved: %v", err)
	}
	if leftover := listSkillRunContainers(t, r); leftover != "" {
		t.Fatalf("a refused tool started a container: %s", leftover)
	}
}

// An empty scope refuses before a container starts, rather than producing a
// clean report of nothing.
func TestLiveScan_RefusesAnEmptyScope(t *testing.T) {
	r := liveRunner(t)
	empty := filepath.Join(repoScratchRoot(t), "empty-"+randomToken())
	if err := os.MkdirAll(empty, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(empty) })

	auth, scope := liveApproval(t, r)
	_, err := r.RunStaticScan(context.Background(), auth, StaticScanRequest{
		Scope:     scope,
		ProjectID: "medusa", ProjectPath: empty,
		StagingRootOverride: stagingOverride(t), Params: DefaultToolParams(),
	})
	if !errors.Is(err, ErrStagingUnusable) {
		t.Fatalf("err = %v, want ErrStagingUnusable", err)
	}
	if !strings.Contains(err.Error(), "never read") {
		t.Fatalf("the refusal should say why an empty scan is worse than none: %v", err)
	}
	if leftover := listSkillRunContainers(t, r); leftover != "" {
		t.Fatalf("an empty scope started a container: %s", leftover)
	}
}

func requireAlpine(t *testing.T, r *Runner) {
	t.Helper()
	if _, err := r.resolveProbeContract(context.Background(), ToolStaticScan); err != nil {
		t.Skipf("static-scan base image is unavailable: %v", err)
	}
}
