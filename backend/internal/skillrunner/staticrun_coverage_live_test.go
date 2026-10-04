package skillrunner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// staticrun_coverage_live_test.go — the pilot-2 fixture, against a real
// container, asserting the things pilot 2 could not.
//
// Everything here runs through busybox grep inside alpine, which is the engine
// that actually evaluates the rules. A Go-regexp test can say the pattern is
// right; only this can say the pattern SURVIVES the shell, the heredoc, the
// field splitting and the container.

// pilotFixture is the tree from the halted pilot, unchanged: a camelCase
// credential literal the scanner must find, a 600 KB file over the staging
// bound, a symlink, and an unsupported extension. Nothing here is real.
func pilotFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(repoScratchRoot(t), "cov-"+randomToken())
	files := map[string]string{
		// The exact line pilot 2 planted and the scanner missed.
		"src/creds.go": "package main\n\nconst apiKey = \"AKIAIOSFODNN7EXAMPLE\"\n",
		"src/main.go":  "package main\n\nfunc main() {}\n",
		"README.md":    "# pilot 2\n",
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
	// 600 KB, over the 512 KiB default bound.
	big := filepath.Join(dir, "src", "big.go")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 600000)), 0o600); err != nil {
		t.Fatalf("write big.go: %v", err)
	}
	// A symlink pointing outside the checkout: never followed, never copied,
	// and — the part that was broken — never silently dropped either.
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("SYNTHETIC-NEVER-REAL\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "src", "linked.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestLiveScan_NoFileDisappearsAndTheCamelCaseSecretIsFound(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	project := pilotFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	auth, scope := liveApproval(t, r)
	report, err := r.RunStaticScan(ctx, auth, StaticScanRequest{
		Scope:     scope,
		ProjectID: "ao-pilot-2", ProjectPath: project,
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
	c := report.Coverage

	// 1. The camelCase literal is found. This is the check pilot 2 failed.
	var secret *ScanFinding
	for i, f := range report.Findings {
		if f.RuleID == "AOSS-006" && f.Path == "src/creds.go" {
			secret = &report.Findings[i]
		}
	}
	if secret == nil {
		t.Fatalf("AOSS-006 did not fire on `const apiKey = \"...\"`; findings: %+v", report.Findings)
	}
	if secret.Severity != "critical" {
		t.Fatalf("severity = %q", secret.Severity)
	}
	// The value never leaves the container, then or now.
	if strings.Contains(reportText(t, report), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("the report carries the matched credential value")
	}

	// 2 and 3. The oversized file and the symlink are both PRESENT as skipped.
	// Under the defect they appeared nowhere at all.
	skipped := map[string]SkippedFile{}
	for _, s := range c.Skipped {
		skipped[s.Path] = s
	}
	big, ok := skipped["src/big.go"]
	if !ok {
		t.Fatalf("src/big.go is in neither the scanned nor the skipped set; "+
			"coverage = %+v", c)
	}
	if big.Reason != SkipReasonTooLarge || big.Stage != SkipAtStaging {
		t.Fatalf("src/big.go = %+v, want %s at %s", big, SkipReasonTooLarge, SkipAtStaging)
	}
	if big.Bytes != 600000 || big.Limit != int64(DefaultToolParams().MaxFileBytes) {
		t.Fatalf("src/big.go size detail = %d/%d", big.Bytes, big.Limit)
	}
	link, ok := skipped["src/linked.go"]
	if !ok {
		t.Fatalf("the symlink is not reported as skipped; coverage = %+v", c)
	}
	if link.Reason != SkipReasonSymlink || link.Stage != SkipAtStaging {
		t.Fatalf("src/linked.go = %+v", link)
	}
	// The unsupported extension is still the scanner's own call, not staging's.
	readme, ok := skipped["README.md"]
	if !ok || readme.Stage != SkipAtScan {
		t.Fatalf("README.md = %+v, want a scanner-stage skip", readme)
	}
	// And the oversized file was never copied into the container: the bound
	// exists to avoid the copy, not only to avoid the scan.
	if link.Bytes != 0 {
		t.Fatalf("a symlink reported a size: %+v", link)
	}

	// 4. Fingerprints. The runner refuses on a mismatch, so reaching here
	// already proves it; assert it explicitly so a future change that drops
	// the check fails a test rather than passing quietly.
	if report.Evidence.InputDigest == "" {
		t.Fatal("the container reported no input digest")
	}
	if report.Evidence.InputFilesVisible != c.FilesStaged {
		t.Fatalf("visible %d != staged %d", report.Evidence.InputFilesVisible, c.FilesStaged)
	}

	// 5. The report never declares coverage it cannot reconcile.
	if !c.Reconciled {
		t.Fatalf("coverage did not reconcile: %s (%+v)", c.ReconciliationNote, c)
	}
	if c.FilesDiscovered != c.FilesStaged+c.FilesSkippedPreStage {
		t.Fatalf("discovered %d != staged %d + pre-stage skips %d",
			c.FilesDiscovered, c.FilesStaged, c.FilesSkippedPreStage)
	}
	if c.FilesStaged != c.FilesScanned+c.FilesSkippedByScanner {
		t.Fatalf("staged %d != scanned %d + scanner skips %d",
			c.FilesStaged, c.FilesScanned, c.FilesSkippedByScanner)
	}
	// The concrete numbers for this fixture: 5 files on disk, 2 dropped before
	// staging, 3 staged, 1 unsupported, 2 scanned.
	if c.FilesDiscovered != 5 || c.FilesStaged != 3 || c.FilesScanned != 2 {
		t.Fatalf("discovered/staged/scanned = %d/%d/%d, want 5/3/2 (%+v)",
			c.FilesDiscovered, c.FilesStaged, c.FilesScanned, c)
	}
}

// A project whose every file is over the bound must be REFUSED with the reason
// named, not scanned into a clean-looking report over nothing.
func TestLiveScan_AProjectOfOnlyOversizedFilesIsRefused(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)

	dir := filepath.Join(repoScratchRoot(t), "onlybig-"+randomToken())
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "a.go"),
		[]byte(strings.Repeat("a", 600000)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	auth, scope := liveApproval(t, r)
	_, err := r.RunStaticScan(ctx, auth, StaticScanRequest{
		Scope:     scope,
		ProjectID: "ao-pilot-2", ProjectPath: dir,
		StagingRootOverride: stagingOverride(t),
		Params:              DefaultToolParams(),
		Limits: Limits{
			Wall: 90 * time.Second, MemoryBytes: 256 << 20, CPUs: 1,
			MaxPIDs: 64, MaxOutputBytes: 256 << 10,
		},
	})
	if err == nil {
		t.Fatal("a project with nothing stageable produced a report")
	}
	for _, want := range []string{"1 candidate file(s)", SkipReasonTooLarge} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
}
