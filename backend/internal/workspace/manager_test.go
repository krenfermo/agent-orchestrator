package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/worktree"
)

// fakeStore is an in-memory Store keyed by task, matching the real table's
// one-row-per-task shape.
type fakeStore struct {
	rows  map[string]domain.TaskWorktreeRecord
	saves []domain.TaskWorktreeRecord
	err   error
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]domain.TaskWorktreeRecord{}} }

func (f *fakeStore) UpsertTaskWorktree(_ context.Context, rec domain.TaskWorktreeRecord) error {
	if f.err != nil {
		return f.err
	}
	f.rows[rec.TaskID] = rec
	f.saves = append(f.saves, rec)
	return nil
}

// ListUnfinishedTaskWorktrees mirrors the real query's predicate: everything
// the manager is not done with, plus a released row whose branch is still
// there.
func (f *fakeStore) ListUnfinishedTaskWorktrees(_ context.Context) ([]domain.TaskWorktreeRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.TaskWorktreeRecord
	for _, rec := range f.rows {
		switch rec.State {
		case domain.TaskWorktreeCreating, domain.TaskWorktreeActive, domain.TaskWorktreeIntegrated:
			out = append(out, rec)
		case domain.TaskWorktreeReleased:
			if !rec.BranchDeleted {
				out = append(out, rec)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out, nil
}

func (f *fakeStore) GetTaskWorktree(_ context.Context, taskID string) (domain.TaskWorktreeRecord, bool, error) {
	if f.err != nil {
		return domain.TaskWorktreeRecord{}, false, f.err
	}
	rec, ok := f.rows[taskID]
	return rec, ok, nil
}

// fakeGit records every call so a test can assert not just what happened but
// that nothing happened at all.
type fakeGit struct {
	calls    []string
	commits  map[string]string
	branches map[string]bool
	// ancestors is keyed "<ancestor>..<descendant>" and answers IsAncestor.
	// Absent means "no", which is the safe direction for a lookup a test
	// forgot to script: it keeps a branch rather than deleting one.
	ancestors   map[string]bool
	addErr      error
	rmErr       error
	ancestorErr error
	deleteErr   error
}

func newFakeGit() *fakeGit {
	return &fakeGit{commits: map[string]string{}, branches: map[string]bool{}, ancestors: map[string]bool{}}
}

func (g *fakeGit) ResolveCommit(_ context.Context, _, rev string) (string, error) {
	g.calls = append(g.calls, "rev-parse "+rev)
	if sha, ok := g.commits[rev]; ok {
		return sha, nil
	}
	return "", errors.New("unknown rev " + rev)
}

func (g *fakeGit) BranchExists(_ context.Context, _, branch string) (bool, error) {
	g.calls = append(g.calls, "branch-exists "+branch)
	return g.branches[branch], nil
}

func (g *fakeGit) AddWorktreeNewBranch(_ context.Context, _, path, branch, baseSHA string) error {
	g.calls = append(g.calls, "add -b "+branch+" "+path+" "+baseSHA)
	if g.addErr != nil {
		return g.addErr
	}
	g.branches[branch] = true
	return nil
}

func (g *fakeGit) AddWorktreeExistingBranch(_ context.Context, _, path, branch string) error {
	g.calls = append(g.calls, "add "+path+" "+branch)
	return g.addErr
}

func (g *fakeGit) RemoveWorktree(_ context.Context, _, path string) error {
	g.calls = append(g.calls, "remove "+path)
	return g.rmErr
}

func (g *fakeGit) Prune(_ context.Context, _ string) error {
	g.calls = append(g.calls, "prune")
	return nil
}

func (g *fakeGit) IsAncestor(_ context.Context, _, ancestor, descendant string) (bool, error) {
	g.calls = append(g.calls, "is-ancestor "+ancestor+" "+descendant)
	if g.ancestorErr != nil {
		return false, g.ancestorErr
	}
	return g.ancestors[ancestor+".."+descendant], nil
}

func (g *fakeGit) DeleteBranch(_ context.Context, _, branch, expectedSHA string) error {
	g.calls = append(g.calls, "delete-branch "+branch+" "+expectedSHA)
	if g.deleteErr != nil {
		return g.deleteErr
	}
	delete(g.branches, branch)
	return nil
}

// fakeFS reports directory presence from a set the test controls.
type fakeFS struct{ present map[string]bool }

func (f fakeFS) DirExists(path string) (bool, error) { return f.present[path], nil }

func newTestManager(t *testing.T, git worktree.Git, store Store, fs FS) *Manager {
	t.Helper()
	clock := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	m, err := New(Options{
		Root:  filepath.Join(t.TempDir(), "worktrees"),
		Git:   git,
		Store: store,
		FS:    fs,
		Now:   func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m
}

func isolatedRequest() Request {
	return Request{
		ProjectID:     "proj",
		WorkflowRunID: "wf-1",
		TaskID:        "t1",
		RepoPath:      filepath.Join(string(filepath.Separator), "repos", "proj"),
		TargetBranch:  "main",
		Mode:          domain.ExecutionIsolatedWorktree,
	}
}

// A direct-branch task must come away with nothing: no directory, no branch,
// no row, and -- the part that is easy to lose in a refactor -- no git command
// at all. The mode's whole promise is that AO works in the user's own
// checkout without ceremony around it.
func TestEnsureDirectBranchCreatesNothing(t *testing.T) {
	git, store := newFakeGit(), newFakeStore()
	m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})

	req := isolatedRequest()
	req.Mode = domain.ExecutionDirectBranch
	lease, err := m.Ensure(context.Background(), req)
	if err != nil {
		t.Fatalf("ensure direct branch: %v", err)
	}
	if lease.Isolated {
		t.Fatalf("lease.Isolated = true, want false for direct branch")
	}
	if lease.Path != req.RepoPath {
		t.Fatalf("lease.Path = %q, want the primary repo %q", lease.Path, req.RepoPath)
	}
	if !reflect.DeepEqual(lease.Record, domain.TaskWorktreeRecord{}) {
		t.Fatalf("lease.Record = %#v, want zero", lease.Record)
	}
	if len(git.calls) != 0 {
		t.Fatalf("git calls = %v, want none for direct branch", git.calls)
	}
	if len(store.saves) != 0 {
		t.Fatalf("store writes = %d, want none for direct branch", len(store.saves))
	}
}

// Both worktree-bearing modes must behave identically here: smart_parallel is
// a scheduling grant, not a different way to materialise a tree.
func TestEnsureRecordsEveryDurableFact(t *testing.T) {
	for _, mode := range []domain.ExecutionMode{domain.ExecutionIsolatedWorktree, domain.ExecutionSmartParallelWorktrees} {
		t.Run(string(mode), func(t *testing.T) {
			git, store := newFakeGit(), newFakeStore()
			git.commits["refs/heads/main"] = "basesha"
			git.commits["refs/heads/ao/wf-1/dep"] = "depsha"
			store.rows["dep"] = domain.TaskWorktreeRecord{TaskID: "dep", Branch: "ao/wf-1/dep"}
			m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})

			req := isolatedRequest()
			req.Mode = mode
			req.DependencyTaskIDs = []string{"dep", "ghost"}
			lease, err := m.Ensure(context.Background(), req)
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			rec := lease.Record
			if !lease.Isolated || lease.Path != rec.Path {
				t.Fatalf("lease = %#v", lease)
			}
			if rec.Branch != "ao/wf-1/t1" {
				t.Fatalf("branch = %q, want ao/wf-1/t1", rec.Branch)
			}
			if !strings.HasPrefix(rec.Branch, BranchPrefix) {
				t.Fatalf("branch %q is not AO-namespaced", rec.Branch)
			}
			if rec.TargetBranch != "main" || rec.BaseSHA != "basesha" {
				t.Fatalf("target/base = %q/%q, want main/basesha", rec.TargetBranch, rec.BaseSHA)
			}
			if rec.TaskID != "t1" || rec.WorkflowRunID != "wf-1" || rec.ProjectID != "proj" {
				t.Fatalf("identity = %#v", rec)
			}
			if rec.ExecutionMode != mode || rec.State != domain.TaskWorktreeActive {
				t.Fatalf("mode/state = %q/%q", rec.ExecutionMode, rec.State)
			}
			if rec.RepoPath != req.RepoPath {
				t.Fatalf("repo path = %q, want %q", rec.RepoPath, req.RepoPath)
			}
			// A dependency with its own worktree pins to that branch's head; a
			// dependency with none pins to the base, because its work is
			// already there.
			want := []domain.TaskWorktreeDependency{{TaskID: "dep", SHA: "depsha"}, {TaskID: "ghost", SHA: "basesha"}}
			if len(rec.Dependencies) != len(want) {
				t.Fatalf("dependencies = %#v, want %#v", rec.Dependencies, want)
			}
			for i := range want {
				if rec.Dependencies[i] != want[i] {
					t.Fatalf("dependency %d = %#v, want %#v", i, rec.Dependencies[i], want[i])
				}
			}
			// The record must be durable BEFORE the directory exists, so a
			// crash mid-create leaves evidence rather than an orphan.
			if len(store.saves) < 2 || store.saves[0].State != domain.TaskWorktreeCreating {
				t.Fatalf("saves = %#v, want a creating record before the active one", store.saves)
			}
		})
	}
}

// A create that fails must leave the failure written down, with the reason,
// rather than a row claiming the worktree is fine.
func TestEnsureRecordsFailure(t *testing.T) {
	git, store := newFakeGit(), newFakeStore()
	git.commits["refs/heads/main"] = "basesha"
	git.addErr = errors.New("path already exists")
	m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})

	if _, err := m.Ensure(context.Background(), isolatedRequest()); err == nil {
		t.Fatal("ensure error = nil, want the git failure")
	}
	rec := store.rows["t1"]
	if rec.State != domain.TaskWorktreeFailed {
		t.Fatalf("state = %q, want failed", rec.State)
	}
	if !strings.Contains(rec.Detail, "path already exists") {
		t.Fatalf("detail = %q, want the git failure in it", rec.Detail)
	}
}

// An existing, materialised worktree is reused as-is. Re-running git for a
// directory that is already there is how an idempotent step becomes a
// destructive one.
func TestEnsureReusesLiveWorktree(t *testing.T) {
	git, store := newFakeGit(), newFakeStore()
	git.commits["refs/heads/main"] = "basesha"
	m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})

	first, err := m.Ensure(context.Background(), isolatedRequest())
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	m.fs = fakeFS{present: map[string]bool{first.Path: true}}
	before := len(git.calls)

	second, err := m.Ensure(context.Background(), isolatedRequest())
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second.Path != first.Path || second.Record.BaseSHA != first.Record.BaseSHA {
		t.Fatalf("second = %#v, want the first lease back", second)
	}
	if len(git.calls) != before {
		t.Fatalf("git calls after reuse = %v, want no new ones", git.calls[before:])
	}
}

// A record whose directory was deleted out of band must be recovered onto the
// branch it already has. Re-cutting from base would discard whatever the task
// had committed, which is the one failure mode recovery exists to avoid.
func TestEnsureRecoversMissingDirectoryOntoExistingBranch(t *testing.T) {
	git, store := newFakeGit(), newFakeStore()
	git.commits["refs/heads/main"] = "basesha"
	m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})

	first, err := m.Ensure(context.Background(), isolatedRequest())
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	git.calls = nil

	if _, err := m.Ensure(context.Background(), isolatedRequest()); err != nil {
		t.Fatalf("recovering ensure: %v", err)
	}
	joined := strings.Join(git.calls, "\n")
	if !strings.Contains(joined, "prune") {
		t.Fatalf("calls = %v, want the stale registration pruned", git.calls)
	}
	if !strings.Contains(joined, "add "+first.Path+" ao/wf-1/t1") {
		t.Fatalf("calls = %v, want the existing-branch add form", git.calls)
	}
	if strings.Contains(joined, "add -b") {
		t.Fatalf("calls = %v, want no fresh branch to be cut over the existing one", git.calls)
	}
}

// Releasing removes the directory, marks the record released, and KEEPS the
// branch: the commits are the task's work, and the record has to stay able to
// name where they are.
func TestReleaseRemovesDirectoryAndKeepsBranch(t *testing.T) {
	git, store := newFakeGit(), newFakeStore()
	git.commits["refs/heads/main"] = "basesha"
	m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})

	lease, err := m.Ensure(context.Background(), isolatedRequest())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	rec, released, err := m.Release(context.Background(), "t1")
	if err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	if rec.State != domain.TaskWorktreeReleased || rec.ReleasedAt == nil {
		t.Fatalf("record = %#v, want released with a timestamp", rec)
	}
	if rec.Branch != lease.Record.Branch || rec.BaseSHA != lease.Record.BaseSHA {
		t.Fatalf("released record lost its branch/base: %#v", rec)
	}
	if !strings.Contains(strings.Join(git.calls, "\n"), "remove "+lease.Path) {
		t.Fatalf("calls = %v, want the worktree removed", git.calls)
	}
	// Releasing twice is a no-op, not a second removal.
	before := len(git.calls)
	if _, _, err := m.Release(context.Background(), "t1"); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if len(git.calls) != before {
		t.Fatalf("second release ran %v", git.calls[before:])
	}
}

// A task with no record -- a direct-branch task, or one that never got a
// worktree -- releases to "nothing to do" rather than an error.
func TestReleaseUnknownTaskIsNotAnError(t *testing.T) {
	git, store := newFakeGit(), newFakeStore()
	m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})
	_, released, err := m.Release(context.Background(), "never-had-one")
	if err != nil {
		t.Fatalf("release unknown: %v", err)
	}
	if released {
		t.Fatal("released = true, want false for a task with no worktree")
	}
	if len(git.calls) != 0 {
		t.Fatalf("git calls = %v, want none", git.calls)
	}
}

// Teardown of a dirty worktree must fail and be recorded as failed. Forcing it
// would delete an agent's uncommitted work to tidy up a directory.
func TestReleaseDirtyWorktreeFailsAndIsRecorded(t *testing.T) {
	git, store := newFakeGit(), newFakeStore()
	git.commits["refs/heads/main"] = "basesha"
	m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})
	if _, err := m.Ensure(context.Background(), isolatedRequest()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	git.rmErr = errors.New("contains modified or untracked files")

	if _, released, err := m.Release(context.Background(), "t1"); err == nil || released {
		t.Fatalf("release = %v, %v; want the refusal surfaced", released, err)
	}
	if got := store.rows["t1"].State; got != domain.TaskWorktreeFailed {
		t.Fatalf("state = %q, want failed", got)
	}
}

func TestEnsureRejectsUnusableRequests(t *testing.T) {
	cases := map[string]func(Request) Request{
		"no repo":         func(r Request) Request { r.RepoPath = ""; return r },
		"no target":       func(r Request) Request { r.TargetBranch = ""; return r },
		"ao target":       func(r Request) Request { r.TargetBranch = "ao/wf-1/t1"; return r },
		"traversal task":  func(r Request) Request { r.TaskID = "../../escape"; return r },
		"traversal run":   func(r Request) Request { r.WorkflowRunID = ".."; return r },
		"unknown mode":    func(r Request) Request { r.Mode = "sideways"; return r },
		"empty project":   func(r Request) Request { r.ProjectID = ""; return r },
		"separator in id": func(r Request) Request { r.TaskID = "a/b"; return r },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			git, store := newFakeGit(), newFakeStore()
			git.commits["refs/heads/main"] = "basesha"
			m := newTestManager(t, git, store, fakeFS{present: map[string]bool{}})
			if _, err := m.Ensure(context.Background(), mutate(isolatedRequest())); err == nil {
				t.Fatal("ensure error = nil, want a refusal")
			}
			if len(git.calls) != 0 {
				t.Fatalf("git calls = %v, want none for a rejected request", git.calls)
			}
			if len(store.saves) != 0 {
				t.Fatalf("store writes = %v, want none for a rejected request", store.saves)
			}
		})
	}
}

// The worktree must never land inside the user's repository: that is exactly
// the tree isolation exists to keep AO out of.
func TestEnsureRefusesPathInsidePrimaryRepo(t *testing.T) {
	git, store := newFakeGit(), newFakeStore()
	git.commits["refs/heads/main"] = "basesha"
	root := t.TempDir()
	m, err := New(Options{Root: filepath.Join(root, "worktrees"), Git: git, Store: store, FS: fakeFS{present: map[string]bool{}}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := isolatedRequest()
	req.RepoPath = root // the managed root now sits inside the "repository"

	_, err = m.Ensure(context.Background(), req)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if len(git.calls) != 0 {
		t.Fatalf("git calls = %v, want none", git.calls)
	}
}
