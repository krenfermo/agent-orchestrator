package router_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/directbranch"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// p5_placement_real_git_test.go — the placement lane against the REAL git
// binary and the REAL adapters, rather than against recorders.
//
// The recorder tests prove the router asks the right adapter. They cannot prove
// the thing that actually went wrong in production, because that needs a
// repository: a task frozen into isolated_worktree, in a project configured for
// direct_branch, whose worker ends up writing on the operator's own branch in
// the operator's own checkout. What makes that invisible is precisely that both
// answers are valid directories -- so the assertions here are made against git.
//
// The lifecycle covered is one obligation's whole path: explicit selection,
// creation, re-attaching an existing worktree, a retry, a recovery after a
// restart with the project reconfigured underneath it, cancellation, and the
// target branch advancing while the work sits on its own.

type p5Repos map[domain.ProjectID]string

func (r p5Repos) RepoPath(id domain.ProjectID) (string, error) { return r[id], nil }

type p5Projects struct {
	projects map[string]domain.ProjectRecord
}

func (p p5Projects) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	rec, ok := p.projects[id]
	return rec, ok, nil
}

type p5Fixture struct {
	binary  string
	repo    string
	managed string
	router  *workspacerouter.Workspace
	// mode is a pointer into the project record the router reads, so a test can
	// change the project's configuration mid-flight exactly as a person would.
	projects map[string]domain.ProjectRecord
}

func newP5Fixture(t *testing.T, mode domain.ExecutionMode) *p5Fixture {
	t.Helper()
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	// macOS resolves /var to /private/var, and both adapters return absolute,
	// symlink-resolved paths. Comparing an unresolved TempDir against them would
	// fail on the spelling rather than on the placement.
	repo := p5Resolved(t, t.TempDir())
	managed := p5Resolved(t, t.TempDir())
	p5Git(t, binary, repo, "init", "--initial-branch=main")
	p5Git(t, binary, repo, "config", "user.email", "ao@example.test")
	p5Git(t, binary, repo, "config", "user.name", "AO Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p5Git(t, binary, repo, "add", ".")
	p5Git(t, binary, repo, "commit", "-m", "base")

	projects := map[string]domain.ProjectRecord{
		"p1": {ID: "p1", Path: repo, Kind: domain.ProjectKindSingleRepo,
			Config: domain.ProjectConfig{ExecutionMode: mode, DefaultBranch: "main"}},
	}
	git, err := gitworktree.New(gitworktree.Options{
		Binary: binary, ManagedRoot: managed, DefaultBranch: "main",
		RepoResolver: p5Repos{"p1": repo},
	})
	if err != nil {
		t.Fatalf("gitworktree: %v", err)
	}
	direct, err := directbranch.New(directbranch.Options{Binary: binary, RepoResolver: p5Repos{"p1": repo}})
	if err != nil {
		t.Fatalf("directbranch: %v", err)
	}
	return &p5Fixture{
		binary: binary, repo: repo, managed: managed, projects: projects,
		router: workspacerouter.New(workspacerouter.Deps{
			Git: git, DirectBranch: direct, Projects: p5Projects{projects: projects},
		}),
	}
}

func (f *p5Fixture) setMode(mode domain.ExecutionMode) {
	rec := f.projects["p1"]
	rec.Config.ExecutionMode = mode
	f.projects["p1"] = rec
}

func p5Resolved(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	return resolved
}

func p5Git(t *testing.T, binary, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// isolatedConfig is one obligation's spawn config, explicitly placed.
func (f *p5Fixture) isolatedConfig(session, branch string) ports.WorkspaceConfig {
	return ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: domain.SessionID(session), Kind: domain.KindWorker,
		SessionPrefix: "ao", Branch: branch, BaseBranch: "main",
		Placement: domain.PlacementIsolatedWorktree,
	}
}

// The headline, end to end. A project configured for direct-branch execution;
// one task explicitly placed in an isolated worktree. Every stage of that
// task's life must keep it out of the operator's checkout.
func TestExplicitIsolatedPlacementNeverTouchesTheOperatorsCheckoutAcrossItsWholeLife(t *testing.T) {
	f := newP5Fixture(t, domain.ExecutionDirectBranch)
	ctx := context.Background()
	cfg := f.isolatedConfig("s-1", "ao/wf-1/t1")

	// SELECTION + CREATION.
	created, err := f.router.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Path == f.repo {
		t.Fatal("an explicitly isolated task was materialised in the operator's own repository")
	}
	if !strings.HasPrefix(created.Path, f.managed) {
		t.Fatalf("worktree %q is not under the managed root %q", created.Path, f.managed)
	}
	if created.Branch != "ao/wf-1/t1" {
		t.Fatalf("branch = %q, want the frozen ao/* branch", created.Branch)
	}
	// git's own answer, not the adapter's: the operator's checkout is still on
	// main, and the work branch is a real branch somewhere else.
	if got := p5Git(t, f.binary, f.repo, "branch", "--show-current"); got != "main" {
		t.Fatalf("the operator's repository moved to %q", got)
	}
	if got := p5Git(t, f.binary, created.Path, "branch", "--show-current"); got != "ao/wf-1/t1" {
		t.Fatalf("worktree is on %q, want ao/wf-1/t1", got)
	}

	// The worker writes. This is what makes every later stage matter: from here
	// on, routing this task to the repository loses work.
	if err := os.WriteFile(filepath.Join(created.Path, "worker.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p5Git(t, f.binary, created.Path, "add", ".")
	p5Git(t, f.binary, created.Path, "commit", "-m", "worker output")
	workSHA := p5Git(t, f.binary, created.Path, "rev-parse", "HEAD")

	// AN EXISTING WORKTREE / A RETRY. A second Create for the same session is
	// what a retry of the same obligation performs, and it must re-attach
	// rather than make a second checkout or fall through to the repository.
	again, err := f.router.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if again.Path != created.Path {
		t.Fatalf("retry got worktree %q, want the existing %q", again.Path, created.Path)
	}
	if got := p5Git(t, f.binary, again.Path, "rev-parse", "HEAD"); got != workSHA {
		t.Fatalf("retry re-attached to %s, want the work commit %s", got, workSHA)
	}

	// RECOVERY AFTER A RESTART, with the project reconfigured underneath. This
	// is the production shape: the placement is derived from the durable facts
	// the session was created with, exactly as the session manager derives it.
	// The project is switched WHILE the task is in flight -- the drift the
	// frozen placement exists to survive -- and then switched back to the mode
	// that makes an unplaced restore dangerous.
	f.setMode(domain.ExecutionIsolatedWorktree)
	f.setMode(domain.ExecutionDirectBranch)
	meta := domain.SessionMetadata{WorkspacePath: created.Path, WorkspaceRepoPath: created.RepoPath}
	derived := sessionmanager.PlacementFromSessionFacts(meta, f.repo)
	if derived != domain.PlacementIsolatedWorktree {
		t.Fatalf("derived placement = %q, want isolated_worktree", derived)
	}
	restored, err := f.router.Restore(ctx, ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s-1", Kind: domain.KindWorker, SessionPrefix: "ao",
		Branch: created.Branch, Path: created.Path, Placement: derived,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Path != created.Path {
		t.Fatalf("restore re-attached to %q, want the worktree %q", restored.Path, created.Path)
	}
	if got := p5Git(t, f.binary, restored.Path, "rev-parse", "HEAD"); got != workSHA {
		t.Fatalf("restored worktree HEAD = %s, want the work commit %s", got, workSHA)
	}

	// BRANCH ADVANCE. The merge target moves while the task holds its own
	// branch. The task's commit must be untouched by that, and the operator's
	// checkout must still be the only thing that moved.
	if err := os.WriteFile(filepath.Join(f.repo, "other.txt"), []byte("someone else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p5Git(t, f.binary, f.repo, "add", ".")
	p5Git(t, f.binary, f.repo, "commit", "-m", "main advances")
	mainSHA := p5Git(t, f.binary, f.repo, "rev-parse", "main")
	if mainSHA == workSHA {
		t.Fatal("main and the task branch resolved to one commit; the fixture is not isolating anything")
	}
	if got := p5Git(t, f.binary, created.Path, "rev-parse", "HEAD"); got != workSHA {
		t.Fatalf("the task's checkout moved to %s when main advanced", got)
	}

	// CANCELLATION. Destroy is routed by the project, which now says
	// direct-branch -- and a direct-branch Destroy is a deliberate no-op,
	// because the path is the user's own repository. The isolated worktree is
	// therefore PRESERVED rather than deleted, which is the safe half of the
	// asymmetry: AO never force-removes a checkout holding work.
	if err := f.router.Destroy(ctx, ports.WorkspaceInfo{
		Path: created.Path, RepoPath: created.RepoPath, ProjectID: "p1", SessionID: "s-1",
		Branch: created.Branch,
	}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := os.Stat(f.repo); err != nil {
		t.Fatalf("the operator's repository was disturbed by a cancellation: %v", err)
	}
	if got := p5Git(t, f.binary, f.repo, "rev-parse", "main"); got != mainSHA {
		t.Fatalf("main moved to %s during cancellation, want %s", got, mainSHA)
	}
}

// The bug, stated as the test that would have caught it: a restore that carries
// NO placement is routed by the project, and in a direct-branch project that
// hands back the operator's own repository for a session whose work is in a
// worktree.
func TestARestoreWithoutAPlacementReturnsTheOperatorsRepository(t *testing.T) {
	f := newP5Fixture(t, domain.ExecutionDirectBranch)
	ctx := context.Background()
	created, err := f.router.Create(ctx, f.isolatedConfig("s-1", "ao/wf-1/t1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	withoutPlacement, err := f.router.Restore(ctx, ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s-1", Kind: domain.KindWorker, SessionPrefix: "ao",
		Branch: created.Branch, Path: created.Path,
	})
	// Either shape is the defect: the operator's repository handed back, or an
	// error. What must NOT happen is silently getting the repository while the
	// caller believes it holds the worktree.
	if err == nil && withoutPlacement.Path == created.Path {
		t.Fatal("a placement-less restore already re-attached the worktree; this test no longer describes the router")
	}

	// And the fix: the same restore, carrying the placement derived from the
	// session's own durable facts, comes back to the worktree.
	derived := sessionmanager.PlacementFromSessionFacts(domain.SessionMetadata{
		WorkspacePath: created.Path, WorkspaceRepoPath: created.RepoPath,
	}, f.repo)
	withPlacement, err := f.router.Restore(ctx, ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s-1", Kind: domain.KindWorker, SessionPrefix: "ao",
		Branch: created.Branch, Path: created.Path, Placement: derived,
	})
	if err != nil {
		t.Fatalf("restore with the derived placement: %v", err)
	}
	if withPlacement.Path != created.Path {
		t.Fatalf("restore = %q, want the worktree %q", withPlacement.Path, created.Path)
	}
}

// The converse, so isolation is not simply always chosen: a task explicitly
// placed on the branch, in a project configured for isolated worktrees, really
// does get the repository -- and no worktree is created anywhere.
func TestExplicitDirectBranchPlacementUsesTheRepositoryWithRealGit(t *testing.T) {
	f := newP5Fixture(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	created, err := f.router.Create(ctx, ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s-1", Kind: domain.KindWorker, SessionPrefix: "ao",
		Branch: "main", BaseBranch: "main", Placement: domain.PlacementDirectBranch,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Path != f.repo {
		t.Fatalf("workspace = %q, want the repository %q", created.Path, f.repo)
	}
	worktrees := p5Git(t, f.binary, f.repo, "worktree", "list", "--porcelain")
	if strings.Contains(worktrees, f.managed) {
		t.Fatalf("a worktree was created under the managed root for a direct-branch placement:\n%s", worktrees)
	}
}
