package directbranch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// staticRepos is a fixed project -> repo path map for tests.
type staticRepos map[domain.ProjectID]string

func (r staticRepos) RepoPath(id domain.ProjectID) (string, error) {
	if p := r[id]; p != "" {
		return p, nil
	}
	return "", errors.New("no repo for project")
}

func TestCreateUsesConfiguredBranchAndCreatesNoWorktreeOrAOBranch(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "feat/engineering-control-center")
	ws := newWorkspace(t, git, repo)

	info, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID:     "proj",
		SessionID:     "sess-1",
		SessionPrefix: "ao",
		Branch:        "feat/engineering-control-center",
		BaseBranch:    "feat/engineering-control-center",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The workspace IS the registered repository, not a managed copy.
	if info.Path != physical(t, repo) {
		t.Fatalf("workspace path = %q, want the registered repo %q", info.Path, repo)
	}
	if info.Branch != "feat/engineering-control-center" {
		t.Fatalf("branch = %q, want the configured branch", info.Branch)
	}
	if got := gitOutput(t, git, repo, "branch", "--show-current"); got != "feat/engineering-control-center" {
		t.Fatalf("checked-out branch = %q, want the configured branch", got)
	}
	// No worktree was registered: `worktree list` shows only the main one.
	if lines := strings.Split(gitOutput(t, git, repo, "worktree", "list"), "\n"); len(lines) != 1 {
		t.Fatalf("worktree list has %d entries, want exactly the main worktree:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// No ao/* branch was created anywhere.
	for _, branch := range strings.Split(gitOutput(t, git, repo, "branch", "--list", "--format=%(refname:short)"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(branch), "ao/") {
			t.Fatalf("direct-branch create produced an ao/* branch: %q", branch)
		}
	}
}

func TestCreateSwitchesToConfiguredBranchFromCleanRepo(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	runGit(t, git, repo, "branch", "feature/work")
	ws := newWorkspace(t, git, repo)

	if _, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "s", BaseBranch: "feature/work",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := gitOutput(t, git, repo, "branch", "--show-current"); got != "feature/work" {
		t.Fatalf("branch = %q, want feature/work", got)
	}
}

func TestCreateRefusesDirtyRepositoryOnBranchSwitch(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	runGit(t, git, repo, "branch", "feature/work")
	writeFile(t, filepath.Join(repo, "README.md"), "a human was editing this\n")
	ws := newWorkspace(t, git, repo)

	_, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "s", BaseBranch: "feature/work",
	})
	if !errors.Is(err, ports.ErrWorkspaceRepositoryDirty) {
		t.Fatalf("create over dirty repo err = %v, want ErrWorkspaceRepositoryDirty", err)
	}
	// The user's branch and their edit are both untouched.
	if got := gitOutput(t, git, repo, "branch", "--show-current"); got != "main" {
		t.Fatalf("branch = %q, want the repo left on main", got)
	}
	if body := readFile(t, filepath.Join(repo, "README.md")); body != "a human was editing this\n" {
		t.Fatalf("README.md = %q, want the human's edit preserved", body)
	}
}

// A repository already sitting on the configured branch is usable even when
// dirty: that is the normal re-entry case (the run's own in-progress work), and
// the dirty *gate* for a fresh run lives in the branch-lock preflight, not here.
func TestCreateAllowsDirtyRepositoryAlreadyOnConfiguredBranch(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	writeFile(t, filepath.Join(repo, "in-progress.txt"), "work\n")
	ws := newWorkspace(t, git, repo)

	if _, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "s", BaseBranch: "main",
	}); err != nil {
		t.Fatalf("create on the configured branch: %v", err)
	}
}

func TestCreateRefusesUnknownBranchInsteadOfFallingBack(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	ws := newWorkspace(t, git, repo)

	_, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "s", BaseBranch: "never/fetched",
	})
	if !errors.Is(err, ports.ErrWorkspaceBranchNotFetched) {
		t.Fatalf("err = %v, want ErrWorkspaceBranchNotFetched", err)
	}
	if got := gitOutput(t, git, repo, "branch", "--show-current"); got != "main" {
		t.Fatalf("branch = %q, want main (no fallback checkout)", got)
	}
}

func TestCreateRequiresAConfiguredBranch(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	ws := newWorkspace(t, git, repo)

	if _, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: "s"}); !errors.Is(err, ErrBranchUnconfigured) {
		t.Fatalf("err = %v, want ErrBranchUnconfigured", err)
	}
}

func TestDestroyAndForceDestroyNeverTouchTheRepository(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	ws := newWorkspace(t, git, repo)
	ctx := context.Background()

	info, err := ws.Create(ctx, ports.WorkspaceConfig{ProjectID: "proj", SessionID: "s", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ws.Destroy(ctx, info); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := ws.ForceDestroy(ctx, info); err != nil {
		t.Fatalf("force destroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Fatalf("repository was damaged by teardown: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatalf("repository .git was damaged by teardown: %v", err)
	}
}

func TestStashUncommittedAndApplyPreservedAreNoOps(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	writeFile(t, filepath.Join(repo, "dirty.txt"), "uncommitted\n")
	ws := newWorkspace(t, git, repo)
	ctx := context.Background()
	info := ports.WorkspaceInfo{Path: repo, Branch: "main", SessionID: "s", ProjectID: "proj"}

	ref, err := ws.StashUncommitted(ctx, info)
	if err != nil || ref != "" {
		t.Fatalf("StashUncommitted = (%q, %v), want (\"\", nil)", ref, err)
	}
	if err := ws.ApplyPreserved(ctx, info, "refs/ao/preserved/s"); err != nil {
		t.Fatalf("ApplyPreserved: %v", err)
	}
	if body := readFile(t, filepath.Join(repo, "dirty.txt")); body != "uncommitted\n" {
		t.Fatalf("dirty.txt = %q, want the working tree untouched", body)
	}
}

func TestCommitAllCommitsOnTheConfiguredBranchWithAOIdentity(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "feat/x")
	ws := newWorkspace(t, git, repo)
	ctx := context.Background()
	info := ports.WorkspaceInfo{Path: repo, Branch: "feat/x", ProjectID: "proj"}

	// Clean tree: nothing to commit is a normal, non-error outcome.
	if sha, committed, err := ws.CommitAll(ctx, info, "chore: nothing"); err != nil || committed || sha != "" {
		t.Fatalf("CommitAll on a clean tree = (%q, %v, %v), want (\"\", false, nil)", sha, committed, err)
	}

	writeFile(t, filepath.Join(repo, "new.txt"), "autonomous work\n")
	before := gitOutput(t, git, repo, "rev-parse", "HEAD")
	sha, committed, err := ws.CommitAll(ctx, info, "feat: autonomous change")
	if err != nil || !committed {
		t.Fatalf("CommitAll = (%q, %v, %v), want a commit", sha, committed, err)
	}
	if sha == before {
		t.Fatalf("HEAD did not move")
	}
	if got := gitOutput(t, git, repo, "branch", "--show-current"); got != "feat/x" {
		t.Fatalf("committed on branch %q, want feat/x", got)
	}
	if got := gitOutput(t, git, repo, "log", "-1", "--pretty=%an <%ae>"); got != ports.WorkspaceCommitAuthorName+" <"+ports.WorkspaceCommitAuthorEmail+">" {
		t.Fatalf("commit author = %q, want AO's own identity", got)
	}
	if got := gitOutput(t, git, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("tree still dirty after commit: %q", got)
	}
}

// A workspace project's repositories are independent: each stays on its own
// configured branch, and the parent never stages the child's work. This is the
// MEDUSA shape from the checkpoint brief.
func TestCreateWorkspaceProjectKeepsRepositoryBoundaries(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	root := setupRepo(t, git, filepath.Join(tmp, "root"), "main")
	child := setupRepo(t, git, filepath.Join(tmp, "childsrc"), "medusa_back_v2")
	// Physically nest the child inside the root, as a workspace project does.
	nested := filepath.Join(root, "backend_node")
	moveDir(t, child, nested)
	// The parent must not track the child's directory.
	writeFile(t, filepath.Join(root, ".gitignore"), "backend_node/\n")
	runGit(t, git, root, "add", ".gitignore")
	runGit(t, git, root, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-m", "ignore child")

	ws := newWorkspace(t, git, root)
	info, err := ws.CreateWorkspaceProject(context.Background(), ports.WorkspaceProjectConfig{
		ProjectID:    "medusa",
		SessionID:    "sess-1",
		Branch:       "ao/should-be-ignored",
		RootRepoPath: root,
		BaseBranch:   "main",
		Repos: []ports.WorkspaceProjectRepoConfig{
			{Name: "backend_node", RelativePath: "backend_node", RepoPath: nested, BaseBranch: "medusa_back_v2"},
		},
	})
	if err != nil {
		t.Fatalf("create workspace project: %v", err)
	}
	if len(info.Worktrees) != 2 {
		t.Fatalf("worktrees = %d, want root + one child", len(info.Worktrees))
	}
	byName := map[string]ports.WorkspaceRepoInfo{}
	for _, wt := range info.Worktrees {
		byName[wt.RepoName] = wt
	}
	if got := byName[domain.RootWorkspaceRepoName].Branch; got != "main" {
		t.Fatalf("root branch = %q, want main", got)
	}
	if got := byName["backend_node"].Branch; got != "medusa_back_v2" {
		t.Fatalf("child branch = %q, want medusa_back_v2", got)
	}
	if got := gitOutput(t, git, root, "branch", "--show-current"); got != "main" {
		t.Fatalf("root repo checked out %q, want main", got)
	}
	if got := gitOutput(t, git, nested, "branch", "--show-current"); got != "medusa_back_v2" {
		t.Fatalf("child repo checked out %q, want medusa_back_v2", got)
	}

	// A change in the child is committed by the child and is invisible to the parent.
	writeFile(t, filepath.Join(nested, "service.ts"), "export const x = 1;\n")
	childInfo := ports.WorkspaceInfo{Path: nested, Branch: "medusa_back_v2", ProjectID: "medusa"}
	if _, committed, err := ws.CommitAll(context.Background(), childInfo, "feat: child change"); err != nil || !committed {
		t.Fatalf("commit child: committed=%v err=%v", committed, err)
	}
	if got := gitOutput(t, git, root, "status", "--porcelain"); got != "" {
		t.Fatalf("parent repo saw the child's change: %q", got)
	}
	if got := gitOutput(t, git, nested, "log", "-1", "--pretty=%s"); got != "feat: child change" {
		t.Fatalf("child HEAD subject = %q", got)
	}
}

func TestPreflightRepositoryReportsDirtyWithoutMutating(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	ws := newWorkspace(t, git, repo)

	clean, err := ws.PreflightRepository(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("preflight clean: %v", err)
	}
	if clean.Dirty || clean.CurrentBranch != "main" || clean.ConfiguredBranch != "main" || clean.HeadSHA == "" {
		t.Fatalf("clean preflight = %#v", clean)
	}

	writeFile(t, filepath.Join(repo, "scratch.txt"), "x\n")
	dirty, err := ws.PreflightRepository(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("preflight dirty: %v", err)
	}
	if !dirty.Dirty || len(dirty.Changes) == 0 {
		t.Fatalf("dirty preflight = %#v", dirty)
	}
	if got := gitOutput(t, git, repo, "branch", "--show-current"); got != "main" {
		t.Fatalf("preflight mutated the repo: branch = %q", got)
	}
}

func TestMaterializeIntegrationCommitIsUnsupported(t *testing.T) {
	ws := &Workspace{binary: "git", repos: staticRepos{}}
	_, _, _, err := ws.MaterializeIntegrationCommit(context.Background(), ports.WorkspaceInfo{Path: "/tmp"}, "refs/ao/x", "", "m", nil)
	if !errors.Is(err, ports.ErrWorkspaceOperationUnsupported) {
		t.Fatalf("err = %v, want ErrWorkspaceOperationUnsupported", err)
	}
}

func TestAddExcludeIsIdempotent(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupRepo(t, git, tmp, "main")
	ws := newWorkspace(t, git, repo)
	info := ports.WorkspaceInfo{Path: repo}
	ctx := context.Background()

	for range 2 {
		if err := ws.AddExclude(ctx, info, "/.ao-attachments/"); err != nil {
			t.Fatalf("add exclude: %v", err)
		}
	}
	body := readFile(t, filepath.Join(repo, ".git", "info", "exclude"))
	if n := strings.Count(body, "/.ao-attachments/"); n != 1 {
		t.Fatalf("pattern written %d times, want 1:\n%s", n, body)
	}
}

// ---- helpers ----

func newWorkspace(t *testing.T, git, repo string) *Workspace {
	t.Helper()
	ws, err := New(Options{Binary: git, RepoResolver: staticRepos{"proj": repo, "medusa": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return ws
}

func requireGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	return git
}

// setupRepo creates a real repository at dir with one commit on branch.
func setupRepo(t *testing.T, git, dir, branch string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run(t, git, "init", dir)
	runGit(t, git, dir, "config", "core.autocrlf", "false")
	runGit(t, git, dir, "config", "user.email", "ao@example.com")
	runGit(t, git, dir, "config", "user.name", "Ao Agents")
	writeFile(t, filepath.Join(dir, "README.md"), "seed\n")
	runGit(t, git, dir, "add", "README.md")
	runGit(t, git, dir, "commit", "-m", "seed")
	runGit(t, git, dir, "branch", "-M", branch)
	return dir
}

func moveDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatalf("rename %s -> %s: %v", from, to, err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func physical(t *testing.T, path string) string {
	t.Helper()
	abs, err := absPath(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	return abs
}

func gitOutput(t *testing.T, git, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(git, append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s -C %s %s: %v\n%s", git, dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runGit(t *testing.T, git, dir string, args ...string) {
	t.Helper()
	run(t, git, append([]string{"-C", dir}, args...)...)
}

func run(t *testing.T, binary string, args ...string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", binary, strings.Join(args, " "), err, out)
	}
}

// AO never pushes in direct-branch mode. The adapter has no push capability at
// all, which is a stronger guarantee than a policy check: there is no code path
// that could publish a commit even if a policy were misread.
func TestCommitAllNeverPushesToTheRemote(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	run(t, git, "init", "--bare", "--initial-branch=main", origin)
	repo := setupRepo(t, git, filepath.Join(tmp, "repo"), "main")
	runGit(t, git, repo, "remote", "add", "origin", origin)
	runGit(t, git, repo, "push", "-u", "origin", "main")
	originHeadBefore := gitOutput(t, git, origin, "rev-parse", "main")

	ws := newWorkspace(t, git, repo)
	writeFile(t, filepath.Join(repo, "local-only.txt"), "not for the remote\n")
	sha, committed, err := ws.CommitAll(context.Background(), ports.WorkspaceInfo{Path: repo, Branch: "main", ProjectID: "proj"}, "feat: local only")
	if err != nil || !committed {
		t.Fatalf("CommitAll = (%q, %v, %v)", sha, committed, err)
	}
	if got := gitOutput(t, git, origin, "rev-parse", "main"); got != originHeadBefore {
		t.Fatalf("remote main moved from %q to %q: the commit was pushed", originHeadBefore, got)
	}
	if got := gitOutput(t, git, repo, "rev-parse", "HEAD"); got != sha {
		t.Fatalf("local HEAD = %q, want the new commit %q", got, sha)
	}
}
