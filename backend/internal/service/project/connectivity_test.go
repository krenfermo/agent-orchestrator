package project_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

func TestClassifyTransport(t *testing.T) {
	// classifyTransport is unexported; exercise it indirectly through the
	// Project/WorkspaceRepo read-models it feeds, which is the only way it is
	// ever observed by a caller.
	configureCommitter(t)
	ctx := context.Background()
	m := newManager(t)

	cases := []struct {
		name      string
		remote    string
		transport string
	}{
		{"https", "https://github.com/krenfermo/medusa.git", "https"},
		{"scp-like-github", "git@github.com:krenfermo/medusa.git", "ssh"},
		{"ssh-host-alias", "github-nuevo:DarkaMX/MEDUSASASBACK.git", "ssh"},
		{"explicit-ssh-scheme", "ssh://git@example.com/repo.git", "ssh"},
		{"no-remote", "", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gitRepoWithCommitWithOrigin(t, dir, tc.remote)
			proj, err := m.Add(ctx, project.AddInput{Path: dir, ProjectID: ptr("transport-" + tc.name)})
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if proj.Transport != tc.transport {
				t.Fatalf("Transport = %q, want %q (remote %q)", proj.Transport, tc.transport, tc.remote)
			}
		})
	}
}

func TestSanitizeRemoteURLStripsEmbeddedCredentials(t *testing.T) {
	configureCommitter(t)
	ctx := context.Background()
	m := newManager(t)

	dir := t.TempDir()
	gitRepoWithCommitWithOrigin(t, dir, "https://x-access-token:ghp_secret123@github.com/org/repo.git")
	proj, err := m.Add(ctx, project.AddInput{Path: dir, ProjectID: ptr("creds")})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if proj.Repo != "https://github.com/org/repo.git" {
		t.Fatalf("Repo = %q, want credentials stripped", proj.Repo)
	}
}

// TestManager_TestRepoConnection_LocalRemote verifies the non-destructive
// connectivity probe against a real (local, file-transport) remote: no
// network, no credentials, so it is a fast, deterministic stand-in for the
// HTTPS/SSH remotes the probe is designed for.
func TestManager_TestRepoConnection_LocalRemote(t *testing.T) {
	configureCommitter(t)
	ctx := context.Background()
	m := newManager(t)

	remoteDir := t.TempDir()
	gitRepoWithCommit(t, remoteDir)

	projectDir := t.TempDir()
	gitRepoWithCommitWithOrigin(t, projectDir, remoteDir)

	if _, err := m.Add(ctx, project.AddInput{Path: projectDir, ProjectID: ptr("conn-ok")}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := m.TestRepoConnection(ctx, domain.ProjectID("conn-ok"), "")
	if err != nil {
		t.Fatalf("TestRepoConnection: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("Status = %q, want ok (message: %q)", result.Status, result.Message)
	}
}

// TestManager_TestRepoConnection_Unreachable verifies the probe reports a
// clear error (not a hang, not a panic) for a remote that cannot be reached,
// and never returns a Go error — connectivity failure is reported data, not
// an RPC failure.
func TestManager_TestRepoConnection_Unreachable(t *testing.T) {
	configureCommitter(t)
	ctx := context.Background()
	m := newManager(t)

	projectDir := t.TempDir()
	gitRepoWithCommitWithOrigin(t, projectDir, filepath.Join(t.TempDir(), "does-not-exist"))

	if _, err := m.Add(ctx, project.AddInput{Path: projectDir, ProjectID: ptr("conn-fail")}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := m.TestRepoConnection(ctx, domain.ProjectID("conn-fail"), "")
	if err != nil {
		t.Fatalf("TestRepoConnection returned a Go error instead of a failed result: %v", err)
	}
	if result.Status != "error" {
		t.Fatalf("Status = %q, want error", result.Status)
	}
	if result.Message == "" {
		t.Fatal("expected a non-empty diagnostic message")
	}
}

// TestManager_TestRepoConnection_WorkspaceChild verifies the probe can target
// a named workspace child repo independently of the root — the real MEDUSA
// scenario (root over HTTPS, backend_node over a distinct SSH remote) needs
// each repo's connectivity established on its own.
func TestManager_TestRepoConnection_WorkspaceChild(t *testing.T) {
	configureCommitter(t)
	ctx := context.Background()
	m := newManager(t)

	childRemote := t.TempDir()
	gitRepoWithCommit(t, childRemote)

	parent := t.TempDir()
	gitRepoWithCommit(t, filepath.Join(parent, "app"))
	child := gitRepoWithCommit(t, filepath.Join(parent, "child"))
	_ = child
	if out, err := exec.Command("git", "-C", filepath.Join(parent, "child"), "remote", "set-url", "origin", childRemote).CombinedOutput(); err != nil {
		t.Fatalf("git remote set-url: %v (%s)", err, out)
	}

	proj, err := m.Add(ctx, project.AddInput{Path: parent, ProjectID: ptr("ws-conn"), AsWorkspace: true})
	if err != nil {
		t.Fatalf("Add workspace: %v", err)
	}
	if len(proj.WorkspaceRepos) != 2 {
		t.Fatalf("expected 2 child repos, got %d: %#v", len(proj.WorkspaceRepos), proj.WorkspaceRepos)
	}

	result, err := m.TestRepoConnection(ctx, domain.ProjectID("ws-conn"), "child")
	if err != nil {
		t.Fatalf("TestRepoConnection(child): %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("child Status = %q, want ok (message: %q)", result.Status, result.Message)
	}

	if _, err := m.TestRepoConnection(ctx, domain.ProjectID("ws-conn"), "nope"); err == nil {
		t.Fatal("expected an error for an unknown repo name")
	}
}

// TestManager_RefreshWorkspaceRepos_RedetectsChangedBranch reproduces the
// real MEDUSA drift: a child repo's DefaultBranch is captured once at
// registration, then the repo is checked out onto a different branch.
// RefreshWorkspaceRepos must re-detect the live branch without touching the
// user's checkout or any other project state.
func TestManager_RefreshWorkspaceRepos_RedetectsChangedBranch(t *testing.T) {
	configureCommitter(t)
	ctx := context.Background()
	m := newManager(t)

	parent := t.TempDir()
	gitRepoWithCommit(t, filepath.Join(parent, "app"))
	childDir := filepath.Join(parent, "child")
	gitRepoWithCommit(t, childDir)

	proj, err := m.Add(ctx, project.AddInput{Path: parent, ProjectID: ptr("refresh"), AsWorkspace: true})
	if err != nil {
		t.Fatalf("Add workspace: %v", err)
	}
	var before *project.WorkspaceRepo
	for i := range proj.WorkspaceRepos {
		if proj.WorkspaceRepos[i].Name == "child" {
			before = &proj.WorkspaceRepos[i]
		}
	}
	if before == nil || before.DefaultBranch != "main" {
		t.Fatalf("expected child registered on main, got %#v", before)
	}

	// Simulate drift: the user switches the child repo to a different branch
	// after registration, exactly like backend_node moving to medusa_back_v2.
	if out, err := exec.Command("git", "-C", childDir, "checkout", "-b", "feature-x").CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v (%s)", err, out)
	}

	refreshed, err := m.RefreshWorkspaceRepos(ctx, domain.ProjectID("refresh"))
	if err != nil {
		t.Fatalf("RefreshWorkspaceRepos: %v", err)
	}
	var after *project.WorkspaceRepo
	for i := range refreshed.WorkspaceRepos {
		if refreshed.WorkspaceRepos[i].Name == "child" {
			after = &refreshed.WorkspaceRepos[i]
		}
	}
	if after == nil {
		t.Fatalf("child repo missing after refresh: %#v", refreshed.WorkspaceRepos)
	}
	if after.DefaultBranch != "feature-x" {
		t.Fatalf("DefaultBranch = %q after refresh, want %q", after.DefaultBranch, "feature-x")
	}

	// Re-fetching via Get must reflect the same corrected state (persisted,
	// not just returned from the refresh call).
	got, err := m.Get(ctx, domain.ProjectID("refresh"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var persisted *project.WorkspaceRepo
	for i := range got.Project.WorkspaceRepos {
		if got.Project.WorkspaceRepos[i].Name == "child" {
			persisted = &got.Project.WorkspaceRepos[i]
		}
	}
	if persisted == nil || persisted.DefaultBranch != "feature-x" {
		t.Fatalf("persisted child DefaultBranch = %#v, want feature-x", persisted)
	}
}

// TestManager_AddWorkspace_ChildBranchPrefersCheckedOutOverOriginHEAD
// reproduces the exact real-world MEDUSA bug: a workspace child repo whose
// remote still advertises "main" as its default branch (origin/HEAD) while
// the repo is actually checked out and developed on a different, permanently
// diverged branch. The child's own checked-out branch must win — it is the
// branch the user is actually working on, not the remote's stale default.
func TestManager_AddWorkspace_ChildBranchPrefersCheckedOutOverOriginHEAD(t *testing.T) {
	configureCommitter(t)
	ctx := context.Background()
	m := newManager(t)

	remoteDir := t.TempDir()
	gitRepoWithCommit(t, remoteDir)
	if out, err := exec.Command("git", "-C", remoteDir, "branch", "-m", "main").CombinedOutput(); err != nil {
		t.Fatalf("git branch -m main: %v (%s)", err, out)
	}

	parent := t.TempDir()
	gitRepoWithCommit(t, filepath.Join(parent, "app"))
	childDir := filepath.Join(parent, "child")
	gitRepoWithCommitWithOrigin(t, childDir, remoteDir)
	// Simulate the real MEDUSA/backend_node shape: origin/HEAD points at the
	// remote's "main" (set as `git clone` would), but the repo has since been
	// permanently switched to a different working branch.
	if out, err := exec.Command("git", "-C", childDir, "fetch", "origin").CombinedOutput(); err != nil {
		t.Fatalf("git fetch origin: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", childDir, "remote", "set-head", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("git remote set-head: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", childDir, "checkout", "-b", "medusa_back_v2").CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v (%s)", err, out)
	}

	proj, err := m.Add(ctx, project.AddInput{Path: parent, ProjectID: ptr("branch-priority"), AsWorkspace: true})
	if err != nil {
		t.Fatalf("Add workspace: %v", err)
	}
	var child *project.WorkspaceRepo
	for i := range proj.WorkspaceRepos {
		if proj.WorkspaceRepos[i].Name == "child" {
			child = &proj.WorkspaceRepos[i]
		}
	}
	if child == nil {
		t.Fatalf("child repo missing: %#v", proj.WorkspaceRepos)
	}
	if child.DefaultBranch != "medusa_back_v2" {
		t.Fatalf("DefaultBranch = %q, want %q (the checked-out branch, not origin/HEAD's %q)", child.DefaultBranch, "medusa_back_v2", "main")
	}
}

func TestManager_RefreshWorkspaceRepos_RejectsSingleRepoProject(t *testing.T) {
	configureCommitter(t)
	ctx := context.Background()
	m := newManager(t)

	dir := gitRepo(t)
	if _, err := m.Add(ctx, project.AddInput{Path: dir, ProjectID: ptr("single")}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := m.RefreshWorkspaceRepos(ctx, domain.ProjectID("single")); err == nil {
		t.Fatal("expected an error refreshing a non-workspace project")
	}
}
