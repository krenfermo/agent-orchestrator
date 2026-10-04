package skillrunner

import (
	"strings"
	"testing"
)

// staticrun_reconcile_test.go — the coverage block says whether it adds up.
//
// "Coverage complete" is a claim, and this is the arithmetic behind it. The
// pilot-2 defect produced coverage that was internally consistent (staged ==
// visible) and still described a smaller project than the one on disk; only a
// denominator catches that, and only checking the denominator makes the
// report say so.

func TestScanCoverage_ReconcilesWhenNothingWasLost(t *testing.T) {
	c := ScanCoverage{
		FilesDiscovered: 5, FilesStaged: 3, FilesScanned: 2,
		FilesSkippedPreStage: 2, FilesSkippedByScanner: 1,
	}
	c.reconcile()
	if !c.Reconciled {
		t.Fatalf("5 = 3 + 2 and 3 = 2 + 1 must reconcile: %s", c.ReconciliationNote)
	}
	if c.ReconciliationNote != "" {
		t.Fatalf("a reconciled coverage carries no note: %q", c.ReconciliationNote)
	}
}

// The exact shape of the defect: a file dropped between discovery and staging
// with nothing recorded. The counts are each individually plausible.
func TestScanCoverage_ADroppedFileIsCaughtByTheDenominator(t *testing.T) {
	c := ScanCoverage{
		FilesDiscovered: 4, FilesStaged: 3, FilesScanned: 2,
		FilesSkippedPreStage: 0, FilesSkippedByScanner: 1,
	}
	c.reconcile()
	if c.Reconciled {
		t.Fatal("a file vanished between discovery and staging and the coverage claimed to add up")
	}
	for _, want := range []string{"discovered 4", "staged 3", "skipped-before-staging 0", "incomplete"} {
		if !strings.Contains(c.ReconciliationNote, want) {
			t.Fatalf("the note does not name %q: %q", want, c.ReconciliationNote)
		}
	}
}

func TestScanCoverage_AFileLostInsideTheContainerIsCaughtToo(t *testing.T) {
	c := ScanCoverage{
		FilesDiscovered: 3, FilesStaged: 3, FilesScanned: 1,
		FilesSkippedPreStage: 0, FilesSkippedByScanner: 0,
	}
	c.reconcile()
	if c.Reconciled {
		t.Fatal("two staged files were neither scanned nor skipped and the coverage still reconciled")
	}
	if !strings.Contains(c.ReconciliationNote, "staged 3 != scanned 1 + skipped-by-scanner 0") {
		t.Fatalf("note = %q", c.ReconciliationNote)
	}
}

// Both equations broken at once names both, so one fix does not hide the other.
func TestScanCoverage_BothEquationsAreReported(t *testing.T) {
	c := ScanCoverage{FilesDiscovered: 9, FilesStaged: 3, FilesScanned: 1}
	c.reconcile()
	if c.Reconciled {
		t.Fatal("nothing here adds up")
	}
	if !strings.Contains(c.ReconciliationNote, "discovered 9") ||
		!strings.Contains(c.ReconciliationNote, "staged 3 != scanned 1") {
		t.Fatalf("both failures must be named: %q", c.ReconciliationNote)
	}
}
