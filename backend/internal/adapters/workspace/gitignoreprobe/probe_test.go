package gitignoreprobe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// probe_test.go runs against a REAL git repository in a temp directory,
// because the whole point of the adapter is that AO's answer is git's answer.
// A stub would only prove that this file and probe.go agree with each other,
// and the cases that matter — a negated pattern, a tracked file a pattern
// would otherwise match — are exactly the ones a hand-written stub would get
// wrong in the same direction the adapter would.
//
// Nothing here touches a user repository, a daemon, or ~/.ao.

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		// A deterministic identity, and no chance of picking up the developer's
		// own hooks, templates or signing configuration.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.invalid",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q")
	write(".gitignore", "dist/\n*.log\n!keep.log\ntracked.log\nreports/*.pdf\n")
	write("src/main.go", "package main\n")
	write("tracked.log", "kept anyway\n")
	write("dist/app.js", "x\n")
	write("other.log", "x\n")
	write("keep.log", "x\n")
	// tracked.log matches its own rule and is committed anyway, which is the
	// case `--no-index` would get wrong.
	run("add", "-f", ".gitignore", "src/main.go", "tracked.log")
	run("commit", "-qm", "init")
	return dir
}

func TestProbeIgnoredPaths(t *testing.T) {
	repo := newRepo(t)
	probe := &Probe{}

	tests := []struct {
		name  string
		paths []string
		want  []workflow.IgnoredDeliverable
	}{
		{
			name:  "a tracked source file is not ignored",
			paths: []string{"src/main.go"},
			want:  nil,
		},
		{
			name:  "an untracked file under an ignored directory is ignored, with the rule",
			paths: []string{"dist/app.js"},
			want: []workflow.IgnoredDeliverable{
				{Path: "dist/app.js", RuleSource: ".gitignore", RuleLine: 1, Pattern: "dist/"},
			},
		},
		{
			name:  "a glob pattern is reported with its own line",
			paths: []string{"other.log"},
			want: []workflow.IgnoredDeliverable{
				{Path: "other.log", RuleSource: ".gitignore", RuleLine: 2, Pattern: "*.log"},
			},
		},
		{
			// git reports the negating rule as a MATCH under -v. A probe that
			// read a match as "ignored" would invert every exception in the
			// repository, which is why this case exists.
			name:  "a negated pattern re-includes the path, so it is not ignored",
			paths: []string{"keep.log"},
			want:  nil,
		},
		{
			// Without --no-index git omits tracked paths entirely, which is the
			// answer AO needs: git can see changes to this file.
			name:  "a TRACKED file matching an ignore rule is not ignored",
			paths: []string{"tracked.log"},
			want:  nil,
		},
		{
			// The deliverable has not been produced yet. That is the situation
			// the check always runs in, so a path that does not exist on disk
			// must be answered on its name.
			name:  "a path that does not exist yet is still answered",
			paths: []string{"reports/audit.pdf"},
			want: []workflow.IgnoredDeliverable{
				{Path: "reports/audit.pdf", RuleSource: ".gitignore", RuleLine: 5, Pattern: "reports/*.pdf"},
			},
		},
		{
			name:  "a non-existent path under no rule is not ignored",
			paths: []string{"src/not-written-yet.go"},
			want:  nil,
		},
		{
			name:  "a nested path under an ignored directory is ignored by the directory rule",
			paths: []string{"dist/deep/nested/out.js"},
			want: []workflow.IgnoredDeliverable{
				{Path: "dist/deep/nested/out.js", RuleSource: ".gitignore", RuleLine: 1, Pattern: "dist/"},
			},
		},
		{
			name: "a mixed batch reports only the ignored ones",
			paths: []string{
				"src/main.go", "dist/app.js", "keep.log", "tracked.log", "reports/audit.pdf",
			},
			want: []workflow.IgnoredDeliverable{
				{Path: "dist/app.js", RuleSource: ".gitignore", RuleLine: 1, Pattern: "dist/"},
				{Path: "reports/audit.pdf", RuleSource: ".gitignore", RuleLine: 5, Pattern: "reports/*.pdf"},
			},
		},
		{
			name:  "an empty request asks git nothing",
			paths: nil,
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := probe.IgnoredPaths(context.Background(), repo, tt.paths)
			if err != nil {
				t.Fatalf("IgnoredPaths: %v", err)
			}
			sort.Slice(got, func(i, j int) bool { return got[i].Path < got[j].Path })
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("IgnoredPaths:\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// A repository AO cannot ask is an unknown, and the caller turns an unknown
// into "dispatch anyway". It must arrive as an error rather than as an empty
// (and therefore reassuring) answer.
func TestProbeReportsFailureRatherThanSilence(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	probe := &Probe{}

	t.Run("no repository path", func(t *testing.T) {
		if _, err := probe.IgnoredPaths(context.Background(), "", []string{"x.go"}); err == nil {
			t.Fatal("an empty repository path returned no error")
		}
	})

	t.Run("not a repository", func(t *testing.T) {
		if _, err := probe.IgnoredPaths(context.Background(), t.TempDir(), []string{"x.go"}); err == nil {
			t.Fatal("a directory that is not a git repository returned no error")
		}
	})

	t.Run("git cannot be run", func(t *testing.T) {
		broken := &Probe{GitBinary: filepath.Join(t.TempDir(), "no-such-git")}
		if _, err := broken.IgnoredPaths(context.Background(), t.TempDir(), []string{"x.go"}); err == nil {
			t.Fatal("an unrunnable git returned no error")
		}
	})
}

// parseCheckIgnore is the one place a malformed answer could become a refusal,
// so its failure directions are pinned separately from the live repository.
func TestParseCheckIgnore(t *testing.T) {
	t.Run("a negation record is dropped", func(t *testing.T) {
		got, err := parseCheckIgnore([]byte(".gitignore\x003\x00!keep.log\x00keep.log\x00"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("a negated rule produced an ignore: %+v", got)
		}
	})
	t.Run("a truncated record is an error, not an invented ignore", func(t *testing.T) {
		if _, err := parseCheckIgnore([]byte(".gitignore\x003\x00dist/\x00")); err == nil {
			t.Fatal("a 3-field record parsed cleanly")
		}
	})
	t.Run("a record with no pattern is not an ignore", func(t *testing.T) {
		got, err := parseCheckIgnore([]byte("\x000\x00\x00some/path.go\x00"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("an unattributed record produced an ignore: %+v", got)
		}
	})
}
