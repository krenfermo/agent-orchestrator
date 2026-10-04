package skillrunner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staging_coverage_test.go — nothing may leave the walk without leaving a trace.
//
// The defect these cover: an oversized file was dropped at staging with a bare
// `return nil`, so it appeared in NO count — not staged, not scanned, not
// skipped. A 4-file project reported as a 3-file project, and a reader had no
// way to tell. Symlinks and files past the file budget had the same shape.
//
// The invariant every test here asserts is the one that makes that class of
// defect impossible to reintroduce:
//
//	Discovered == len(Inputs) + SkippedCount

// assertNothingLost is the invariant, checked the same way everywhere.
func assertNothingLost(t *testing.T, s Staging) {
	t.Helper()
	if want := len(s.Inputs) + s.SkippedCount; s.Discovered != want {
		t.Fatalf("a file was lost: discovered %d != staged %d + skipped %d\n"+
			"inputs=%v\nskipped=%+v", s.Discovered, len(s.Inputs), s.SkippedCount, paths(s), s.Skipped)
	}
}

func paths(s Staging) []string {
	out := make([]string, 0, len(s.Inputs))
	for _, in := range s.Inputs {
		out = append(out, in.RelPath)
	}
	return out
}

// skipReasonFor returns the recorded reason for one path, or "" if the path was
// not recorded at all — which is the failure these tests exist to catch.
func skipReasonFor(s Staging, rel string) string {
	for _, sk := range s.Skipped {
		if sk.Path == rel {
			return sk.Reason
		}
	}
	return ""
}

func writeFile(t *testing.T, dir, rel string, size int) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("a", size)), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// stageWithLimits stages without the fixture project, so each test controls
// exactly what is in the tree.
func stageWithLimits(t *testing.T, project string, maxFiles int, maxBytes int64) (Staging, error) {
	t.Helper()
	root := filepath.Join(t.TempDir(), stagingDirName)
	return Stage(StageRequest{
		SourceDir: project, Root: root, RunID: "run-test",
		MaxFiles: maxFiles, MaxFileBytes: maxBytes,
	})
}

// The pilot-2 fixture, exactly: three small files and one over the bound.
func TestStage_AnOversizedFileIsSkippedNotDropped(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, "src/creds.go", 52)
	writeFile(t, project, "src/main.go", 29)
	writeFile(t, project, "README.md", 13)
	writeFile(t, project, "src/big.go", 600000)

	staging, err := stageWithLimits(t, project, 100, 524288)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	if staging.Discovered != 4 {
		t.Fatalf("discovered = %d, want the 4 files that are on disk", staging.Discovered)
	}
	if len(staging.Inputs) != 3 {
		t.Fatalf("staged = %v, want the three under the bound", paths(staging))
	}
	if got := skipReasonFor(staging, "src/big.go"); got != SkipReasonTooLarge {
		t.Fatalf("src/big.go reason = %q, want %q; a file the report never mentions "+
			"is the coverage lie this exists to prevent", got, SkipReasonTooLarge)
	}
	// The numbers, not just the verdict: "too_large" alone does not tell a
	// reader whether the bound is wrong or the file is.
	for _, sk := range staging.Skipped {
		if sk.Path != "src/big.go" {
			continue
		}
		if sk.Bytes != 600000 || sk.Limit != 524288 {
			t.Fatalf("size detail = %d/%d, want 600000/524288", sk.Bytes, sk.Limit)
		}
		if sk.Stage != SkipAtStaging {
			t.Fatalf("stage = %q, want %q", sk.Stage, SkipAtStaging)
		}
	}
	assertNothingLost(t, staging)
}

func TestStage_ASymlinkIsSkippedNotDropped(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, "src/main.go", 29)
	outside := filepath.Join(t.TempDir(), "creds.txt")
	if err := os.WriteFile(outside, []byte("SYNTHETIC-SECRET-NEVER-REAL\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "src", "linked.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	staging, err := stageWithLimits(t, project, 100, 524288)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	if got := skipReasonFor(staging, "src/linked.go"); got != SkipReasonSymlink {
		t.Fatalf("src/linked.go reason = %q, want %q", got, SkipReasonSymlink)
	}
	// Still refused, still not followed: recording it must not have turned a
	// refusal into a copy.
	for _, in := range staging.Inputs {
		if strings.Contains(in.RelPath, "linked") {
			t.Fatalf("a symlink was staged: %+v", in)
		}
	}
	body, err := os.ReadFile(filepath.Join(staging.Dir, "src", "linked.go"))
	if err == nil && strings.Contains(string(body), "SYNTHETIC-SECRET-NEVER-REAL") {
		t.Fatal("the symlink target was copied across the boundary")
	}
	assertNothingLost(t, staging)
}

// Both defects in one tree, because a fix that records one and forgets the
// other still loses a file.
func TestStage_OversizedAndSymlinkTogether(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, "src/main.go", 29)
	writeFile(t, project, "src/big.go", 600000)
	outside := filepath.Join(t.TempDir(), "creds.txt")
	if err := os.WriteFile(outside, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "src", "linked.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	staging, err := stageWithLimits(t, project, 100, 524288)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if staging.Discovered != 3 || len(staging.Inputs) != 1 || staging.SkippedCount != 2 {
		t.Fatalf("discovered %d, staged %d, skipped %d; want 3/1/2",
			staging.Discovered, len(staging.Inputs), staging.SkippedCount)
	}
	if got := skipReasonFor(staging, "src/big.go"); got != SkipReasonTooLarge {
		t.Fatalf("big.go = %q", got)
	}
	if got := skipReasonFor(staging, "src/linked.go"); got != SkipReasonSymlink {
		t.Fatalf("linked.go = %q", got)
	}
	assertNothingLost(t, staging)
}

// A project of nothing but oversized files stages nothing. It must be REFUSED,
// and the refusal must name the reason: "there is nothing here" and "everything
// here was excluded, here is why" send an operator to different places, and
// only the second one is true.
func TestStage_AProjectOfOnlyOversizedFilesIsRefusedWithTheReason(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, "src/a.go", 600000)
	writeFile(t, project, "src/b.go", 700000)

	_, err := stageWithLimits(t, project, 100, 524288)
	if err == nil {
		t.Fatal("staging nothing must be refused, not reported as an empty project")
	}
	if !errors.Is(err, ErrStagingUnusable) {
		t.Fatalf("error = %v, want ErrStagingUnusable", err)
	}
	for _, want := range []string{"2 candidate file(s)", SkipReasonTooLarge} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
}

// The same file COUNT with different bytes must produce different fingerprints.
// A count check alone passes here, which is why the fingerprint is over
// path+size+sha256 rather than over the number of files.
func TestStage_SameFileCountDifferentBytesFingerprintsDifferently(t *testing.T) {
	build := func(body string) Staging {
		project := t.TempDir()
		writeFile(t, project, "src/main.go", 29)
		if err := os.WriteFile(filepath.Join(project, "src", "creds.go"),
			[]byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		staging, err := stageWithLimits(t, project, 100, 524288)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		assertNothingLost(t, staging)
		return staging
	}

	a := build("package main // one\n")
	b := build("package main // two\n")
	if len(a.Inputs) != len(b.Inputs) {
		t.Fatalf("the trees must have the same file count: %d vs %d", len(a.Inputs), len(b.Inputs))
	}
	if stagedInputsDigest(a.Inputs) == stagedInputsDigest(b.Inputs) {
		t.Fatal("two trees with the same file count and different bytes fingerprinted the same; " +
			"a mount carrying a stale tree would pass")
	}
}

// Past the file budget the walk records and CONTINUES. It used to abandon the
// walk, which left every remaining file uncounted — on exactly the large
// repositories where knowing how much was not read matters most.
func TestStage_FilesPastTheBudgetAreRecordedNotAbandoned(t *testing.T) {
	project := t.TempDir()
	const total = 12
	for i := range total {
		writeFile(t, project, fmt.Sprintf("src/f%02d.go", i), 20)
	}

	staging, err := stageWithLimits(t, project, 5, 524288)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(staging.Inputs) != 5 {
		t.Fatalf("staged = %d, want the budget of 5", len(staging.Inputs))
	}
	if staging.Discovered != total {
		t.Fatalf("discovered = %d, want all %d; the walk stopped counting", staging.Discovered, total)
	}
	if staging.SkippedCount != total-5 {
		t.Fatalf("skipped = %d, want %d", staging.SkippedCount, total-5)
	}
	for _, sk := range staging.Skipped {
		if sk.Reason != SkipReasonBudget {
			t.Fatalf("reason = %q, want %q", sk.Reason, SkipReasonBudget)
		}
	}
	assertNothingLost(t, staging)
}

// The enumeration is capped; the COUNT never is. A report that listed a
// hundred thousand skips would be unusable, but one that undercounted them
// would be untrue, and only one of those is acceptable.
func TestStage_TheSkipListIsCappedAndSaysSoWhileTheCountStaysExact(t *testing.T) {
	project := t.TempDir()
	total := maxRecordedSkips + 25
	writeFile(t, project, "src/keep.go", 20)
	for i := range total {
		writeFile(t, project, fmt.Sprintf("big/f%04d.go", i), 600000)
	}

	staging, err := stageWithLimits(t, project, 100, 524288)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if staging.SkippedCount != total {
		t.Fatalf("SkippedCount = %d, want the exact %d", staging.SkippedCount, total)
	}
	if len(staging.Skipped) != maxRecordedSkips {
		t.Fatalf("listed %d, want the cap of %d", len(staging.Skipped), maxRecordedSkips)
	}
	if !staging.SkippedTruncated {
		t.Fatal("a capped list must say it was capped")
	}
	assertNothingLost(t, staging)
}

// Existing behaviour that must survive the change: unreadable and
// unaddressable keep their reasons, and both still reconcile.
func TestStage_UnreadableAndUnaddressableKeepTheirReasons(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, "src/ok.go", 20)
	writeFile(t, project, "src/we ird$name.go", 20)
	unreadable := filepath.Join(project, "src", "locked.go")
	writeFile(t, project, "src/locked.go", 20)
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Skipf("chmod unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	if _, err := os.ReadFile(unreadable); err == nil {
		t.Skip("running as a user that can read 0o000 files")
	}

	staging, err := stageWithLimits(t, project, 100, 524288)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if got := skipReasonFor(staging, "src/we ird$name.go"); got != SkipReasonUnaddressable {
		t.Fatalf("unaddressable reason = %q", got)
	}
	if got := skipReasonFor(staging, "src/locked.go"); got != SkipReasonUnreadable {
		t.Fatalf("unreadable reason = %q", got)
	}
	assertNothingLost(t, staging)
}
