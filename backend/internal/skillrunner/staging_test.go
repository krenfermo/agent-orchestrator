package skillrunner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagingProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"api/handler.go":      "package api\n",
		"web/app.js":          "const x = 1;\n",
		".git/config":         "[core]\n",
		"node_modules/a/i.js": "module.exports = 1;\n",
		"vendor/v/v.go":       "package v\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir
}

func stageInto(t *testing.T, project string, scope []string) Staging {
	t.Helper()
	root := filepath.Join(t.TempDir(), stagingDirName)
	staging, err := Stage(StageRequest{
		SourceDir: project, ScopePaths: scope, Root: root, RunID: "run-test",
		MaxFiles: 100, MaxFileBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	t.Cleanup(func() { _ = staging.Cleanup() })
	return staging
}

// Scope is enforced by what gets copied, not by an instruction the skill may
// ignore. The excluded directories are the important half: .git holds every
// version of every file, so staging it would defeat a scope restriction and
// hand over deleted secrets besides.
func TestStage_CopiesOnlyWhatIsInScope(t *testing.T) {
	project := stagingProject(t)

	all := stageInto(t, project, nil)
	paths := map[string]bool{}
	for _, in := range all.Inputs {
		paths[in.RelPath] = true
		if in.SHA256 == "" || in.Bytes == 0 {
			t.Fatalf("staged input has no digest: %+v", in)
		}
	}
	if !paths["api/handler.go"] || !paths["web/app.js"] {
		t.Fatalf("in-scope files missing: %v", paths)
	}
	for _, forbidden := range []string{".git/config", "node_modules/a/i.js", "vendor/v/v.go"} {
		if paths[forbidden] {
			t.Fatalf("%s was staged", forbidden)
		}
	}

	scoped := stageInto(t, project, []string{"api"})
	if len(scoped.Inputs) != 1 || scoped.Inputs[0].RelPath != "api/handler.go" {
		t.Fatalf("scoped staging = %+v", scoped.Inputs)
	}
	// Not merely unreported — the out-of-scope file is not on the mount at all.
	if _, err := os.Stat(filepath.Join(scoped.Dir, "web", "app.js")); !os.IsNotExist(err) {
		t.Fatalf("an out-of-scope file exists on the mount: %v", err)
	}
}

// A symlink is never followed: one inside the checkout could point at a
// credential outside it, and a copy that followed it would carry the
// credential across the boundary that exists to stop exactly that.
func TestStage_RefusesToFollowASymlinkOutOfTheProject(t *testing.T) {
	project := stagingProject(t)
	outside := filepath.Join(t.TempDir(), "creds.txt")
	if err := os.WriteFile(outside, []byte("SYNTHETIC-SECRET-NEVER-REAL\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "api", "linked.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	staging := stageInto(t, project, nil)
	for _, in := range staging.Inputs {
		if strings.Contains(in.RelPath, "linked") {
			t.Fatalf("a symlink was staged: %+v", in)
		}
	}
	body, err := os.ReadFile(filepath.Join(staging.Dir, "api", "linked.go"))
	if err == nil && strings.Contains(string(body), "SYNTHETIC-SECRET-NEVER-REAL") {
		t.Fatal("the symlink target was copied across the boundary")
	}
}

func TestStage_Refusals(t *testing.T) {
	project := stagingProject(t)
	root := filepath.Join(t.TempDir(), stagingDirName)
	base := StageRequest{
		SourceDir: project, Root: root, RunID: "run-test",
		MaxFiles: 100, MaxFileBytes: 1 << 20,
	}
	cases := []struct {
		name    string
		mutate  func(*StageRequest)
		wantSub string
	}{
		{"relative source", func(r *StageRequest) { r.SourceDir = "rel/path" }, "must be absolute"},
		{"root outside AO's namespace", func(r *StageRequest) { r.Root = t.TempDir() }, "not an AO staging root"},
		{"no limits", func(r *StageRequest) { r.MaxFiles = 0 }, "staging limits are required"},
		{"absolute scope path", func(r *StageRequest) { r.ScopePaths = []string{"/etc"} }, "must be repo-relative"},
		{"escaping scope path", func(r *StageRequest) { r.ScopePaths = []string{"../.."} }, "escapes the project"},
		{"missing scope path", func(r *StageRequest) { r.ScopePaths = []string{"nope"} }, "does not exist"},
		{
			"empty result",
			func(r *StageRequest) { r.SourceDir = t.TempDir() },
			"would report a clean result for a project it never read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			_, err := Stage(req)
			if !errors.Is(err, ErrStagingUnusable) {
				t.Fatalf("err = %v, want ErrStagingUnusable", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
		})
	}
}

// The staging root is never ~/.ao and never the home directory itself.
func TestStagingRootFor_NeverUsesHomeOrAODataDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	// A project directly in $HOME would put staging in $HOME. Refused.
	if _, err := StagingRootFor(filepath.Join(home, "proj"), ""); err == nil {
		t.Fatal("staging in the home directory was accepted")
	}

	root, err := StagingRootFor("/Users/someone/code/proj", "")
	if err != nil {
		t.Fatalf("StagingRootFor: %v", err)
	}
	if root != filepath.Join("/Users/someone/code", stagingDirName) {
		t.Fatalf("root = %q", root)
	}
	if strings.Contains(root, "/.ao/") || strings.HasSuffix(root, "/.ao") {
		t.Fatalf("staging resolved into AO's data dir: %q", root)
	}
	if _, err := StagingRootFor("relative/path", ""); err == nil {
		t.Fatal("a relative project path was accepted")
	}
	if _, err := StagingRootFor("/x", "relative"); err == nil {
		t.Fatal("a relative override was accepted")
	}
}

// Cleanup refuses any path outside AO's own namespace, so a bug in root
// selection cannot turn teardown into a delete of somebody's project.
func TestStagingCleanup_RefusesAPathItDoesNotOwn(t *testing.T) {
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "important.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := Staging{Dir: victim, Root: victim}.Cleanup()
	if err == nil || !strings.Contains(err.Error(), "not an AO staging directory") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(victim, "important.txt")); statErr != nil {
		t.Fatalf("Cleanup deleted a directory it does not own: %v", statErr)
	}
}

// The visibility check is what stands between a VM that shares no path and a
// clean audit of a project nobody read.
func TestVerifyVisible_FailsWhenTheMountDidNotDeliver(t *testing.T) {
	staging := Staging{Inputs: make([]StagedInput, 7)}

	err := staging.VerifyVisible(0)
	if !errors.Is(err, ErrStagingUnusable) || !strings.Contains(err.Error(), "shared mounts") {
		t.Fatalf("empty mount: err = %v", err)
	}
	err = staging.VerifyVisible(3)
	if !errors.Is(err, ErrStagingUnusable) || !strings.Contains(err.Error(), "3 of 7") {
		t.Fatalf("partial mount: err = %v", err)
	}
	if err := staging.VerifyVisible(7); err != nil {
		t.Fatalf("a complete mount was rejected: %v", err)
	}
}

// A filename the scan tool cannot address is skipped and RECORDED, never
// staged-and-silently-unscanned. A file counted as staged but never scanned is
// a coverage lie, which is the one failure this whole path exists to prevent.
func TestStage_SkipsUnaddressableFilenamesAndSaysSo(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "api"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	good := filepath.Join(project, "api", "ok.go")
	odd := filepath.Join(project, "api", "we ird$name.go")
	for _, p := range []string{good, odd} {
		if err := os.WriteFile(p, []byte("package api\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	staging := stageInto(t, project, nil)
	for _, in := range staging.Inputs {
		if strings.Contains(in.RelPath, "we ird") {
			t.Fatalf("an unaddressable filename was staged: %+v", in)
		}
	}
	if len(staging.Inputs) != 1 {
		t.Fatalf("staged = %+v", staging.Inputs)
	}
	found := false
	for _, s := range staging.Skipped {
		if strings.Contains(s.Path, "we ird") && s.Reason == "unaddressable_filename" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the skip was not recorded: %+v", staging.Skipped)
	}
}

func TestHasShellHostileName(t *testing.T) {
	for _, safe := range []string{"api/handler.go", "a-b_c.2.ts", "dir/sub/file.py"} {
		if hasShellHostileName(safe) {
			t.Fatalf("%q was rejected", safe)
		}
	}
	for _, hostile := range []string{"a b.go", "x'y.go", "a$b.go", "a;rm -rf.go", "a|b.go", "a\tb.go"} {
		if !hasShellHostileName(hostile) {
			t.Fatalf("%q was accepted", hostile)
		}
	}
}
