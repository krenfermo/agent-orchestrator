package contextrouter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
)

// The production diff source runs git inside the checkout, so it needs an
// absolute root. A relative one is refused rather than resolved against the
// daemon's working directory, which would silently diff whatever tree the
// process happened to be started in.
func TestGitDiffSourceRequiresAnAbsoluteRoot(t *testing.T) {
	source := NewGitDiffSource()
	for name, project := range map[string]Project{
		"empty root":    {ID: "proj-1"},
		"blank root":    {ID: "proj-1", Root: "   "},
		"relative root": {ID: "proj-1", Root: "checkouts/proj-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := source.Changes(context.Background(), project); err == nil {
				t.Fatalf("root %q was accepted", project.Root)
			}
		})
	}
}

// The happy path against a real checkout, so the requirement above is a
// precondition of something that works rather than of nothing.
func TestGitDiffSourceReportsWorkingTreeChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.test",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	run("init", "--initial-branch=main")
	write("kept.go", "package kept\n")
	write("edited.go", "package edited\n")
	run("add", ".")
	run("commit", "-m", "base")

	write("edited.go", "package edited\n\nfunc F() {}\n")
	write("added.go", "package added\n")
	run("add", "added.go")

	diff, err := NewGitDiffSource().Changes(context.Background(), Project{ID: "proj-1", Root: root})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	got := map[string]codegraph.ChangeStatus{}
	for _, change := range diff.Changes {
		got[change.Path] = change.Status
	}
	if got["edited.go"] != codegraph.ChangeModified {
		t.Fatalf("edited.go reported as %q, want %q (all changes: %+v)", got["edited.go"], codegraph.ChangeModified, diff.Changes)
	}
	if got["added.go"] != codegraph.ChangeAdded {
		t.Fatalf("added.go reported as %q, want %q (all changes: %+v)", got["added.go"], codegraph.ChangeAdded, diff.Changes)
	}
	if _, unchanged := got["kept.go"]; unchanged {
		t.Fatalf("an unchanged file was reported as changed: %+v", diff.Changes)
	}
}
