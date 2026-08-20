// Package directbranch implements ports.Workspace for projects configured with
// domain.ExecutionDirectBranch (Checkpoint 8P-E.11).
//
// Unlike gitworktree, it materialises nothing: the "workspace" for a session is
// the registered repository itself, checked out on the project's configured
// branch. That inverts three of the invariants the worktree adapter relies on,
// and every one of them is load-bearing here:
//
//   - Create must never create a branch or a worktree. It verifies the
//     repository and the configured branch, switches to that branch only when
//     doing so is safe, and returns the repository path.
//   - Destroy/ForceDestroy are no-ops. The path is the user's own repository;
//     there is no AO-owned artifact to remove, and deleting it would be
//     catastrophic. Cleanup for this mode is genuinely empty, not deferred.
//   - StashUncommitted/ApplyPreserved are no-ops. AO never rewrites, stashes,
//     or replays over a working tree it does not own. The concurrency and
//     dirty-state guarantees come from the durable branch lock and from
//     Create's own dirty refusal instead of from physical isolation.
package directbranch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const (
	defaultGitBinary            = "git"
	maxObservedWorkspaceChanges = 200
	maxObservedWorkspaceCommits = 20
)

// Adapter-local aliases of the port sentinels, following gitworktree's own
// aliasing convention so tests inside this package read naturally while
// callers outside match on the port-level errors.
var (
	ErrBranchInvalid      = ports.ErrWorkspaceBranchInvalid
	ErrBranchNotFetched   = ports.ErrWorkspaceBranchNotFetched
	ErrRepositoryDirty    = ports.ErrWorkspaceRepositoryDirty
	ErrNotSupported       = ports.ErrWorkspaceOperationUnsupported
	ErrNotARepository     = errors.New("directbranch: path is not a git repository")
	ErrBranchUnconfigured = errors.New("directbranch: no branch configured for this repository")
)

// RepoResolver maps a project to the absolute path of its registered repo. It
// is structurally identical to gitworktree.RepoResolver so the daemon's single
// projectRepoResolver satisfies both without an adapter shim.
type RepoResolver interface {
	RepoPath(projectID domain.ProjectID) (string, error)
}

// Options configures a direct-branch Workspace. RepoResolver is required.
type Options struct {
	Binary       string
	RepoResolver RepoResolver
}

// Workspace runs sessions directly inside registered repositories.
type Workspace struct {
	binary string
	repos  RepoResolver
	run    commandRunner
}

type commandRunner func(ctx context.Context, binary string, args ...string) ([]byte, error)

var _ ports.Workspace = (*Workspace)(nil)
var _ ports.WorkspaceProject = (*Workspace)(nil)
var _ ports.WorkspaceObserver = (*Workspace)(nil)
var _ ports.WorkspaceCommitter = (*Workspace)(nil)
var _ ports.WorkspacePreflighter = (*Workspace)(nil)

// New builds a direct-branch Workspace.
func New(opts Options) (*Workspace, error) {
	if opts.RepoResolver == nil {
		return nil, errors.New("directbranch: RepoResolver is required")
	}
	binary := opts.Binary
	if binary == "" {
		binary = defaultGitBinary
	}
	return &Workspace{binary: binary, repos: opts.RepoResolver, run: runCommand}, nil
}

func runCommand(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := aoprocess.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", binary, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Create prepares the registered repository for a session: it verifies the
// repository and the configured branch, refuses to proceed over pre-existing
// uncommitted user work, and checks the configured branch out when the
// repository is sitting on a different one.
//
// It is fully idempotent — a second call for a second session of the same
// project on the same branch is a no-op that returns the same path. Preventing
// two *writers* from sharing that path is the branch lock's job, deliberately
// not this adapter's: an adapter that refused the second call could not tell a
// legitimate re-entry (restore, reconcile) from a real conflict.
func (w *Workspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	repo, err := w.repoPath(cfg.ProjectID, cfg.RepoPath)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	branch := resolveBranch(cfg)
	if branch == "" {
		return ports.WorkspaceInfo{}, fmt.Errorf("%w: project %q", ErrBranchUnconfigured, cfg.ProjectID)
	}
	if err := w.ensureRepository(ctx, repo); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	if err := w.validateBranchName(ctx, repo, branch); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	if err := w.ensureBranchCheckedOut(ctx, repo, branch); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return ports.WorkspaceInfo{
		Path:      repo,
		Branch:    branch,
		SessionID: cfg.SessionID,
		ProjectID: cfg.ProjectID,
		RepoPath:  repo,
	}, nil
}

// CreateWorkspaceProject prepares a multi-repository workspace project in
// place. Each registered repository is an independent Git repository living at
// its real nested path, and each is checked out on *its own* configured branch
// — the root on the project's default branch, every child on the branch
// registered for that child. The shared session branch name a worktree-mode
// workspace project would use (cfg.Branch) is deliberately ignored: there is no
// AO branch here, and forcing one name across independent repositories is
// exactly the boundary violation this mode must not commit.
//
// Preparation is ordered children-last so a failure on a child leaves the
// already-prepared repositories on their configured branches rather than in a
// half-invented state; nothing is created, so there is nothing to roll back.
func (w *Workspace) CreateWorkspaceProject(ctx context.Context, cfg ports.WorkspaceProjectConfig) (ports.WorkspaceProjectInfo, error) {
	root, err := absPath(cfg.RootRepoPath)
	if err != nil {
		return ports.WorkspaceProjectInfo{}, err
	}
	out := ports.WorkspaceProjectInfo{Worktrees: make([]ports.WorkspaceRepoInfo, 0, len(cfg.Repos)+1)}
	rootInfo, err := w.prepareProjectRepo(ctx, cfg, domain.RootWorkspaceRepoName, "", root, cfg.BaseBranch)
	if err != nil {
		return ports.WorkspaceProjectInfo{}, err
	}
	out.Root = ports.WorkspaceInfo{Path: rootInfo.Path, Branch: rootInfo.Branch, SessionID: cfg.SessionID, ProjectID: cfg.ProjectID, RepoPath: rootInfo.RepoPath}
	out.Worktrees = append(out.Worktrees, rootInfo)
	for _, child := range cfg.Repos {
		childPath, err := absPath(child.RepoPath)
		if err != nil {
			return ports.WorkspaceProjectInfo{}, fmt.Errorf("directbranch: child repo %q: %w", child.Name, err)
		}
		// A child's own configured branch is authoritative. It never falls
		// back to the parent project's default branch: medusa/backend_node
		// staying on medusa_back_v2 while medusa stays on main is the whole
		// point of preserving repository boundaries.
		info, err := w.prepareProjectRepo(ctx, cfg, child.Name, child.RelativePath, childPath, child.BaseBranch)
		if err != nil {
			return ports.WorkspaceProjectInfo{}, err
		}
		out.Worktrees = append(out.Worktrees, info)
	}
	return out, nil
}

// DestroyWorkspaceProject is a no-op: every path in a direct-branch workspace
// project is a registered repository, never an AO-created worktree.
func (w *Workspace) DestroyWorkspaceProject(context.Context, ports.WorkspaceProjectInfo) error {
	return nil
}

func (w *Workspace) prepareProjectRepo(ctx context.Context, cfg ports.WorkspaceProjectConfig, name, relativePath, repoPath, branch string) (ports.WorkspaceRepoInfo, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ports.WorkspaceRepoInfo{}, fmt.Errorf("%w: repository %q", ErrBranchUnconfigured, name)
	}
	if err := w.ensureRepository(ctx, repoPath); err != nil {
		return ports.WorkspaceRepoInfo{}, err
	}
	if err := w.validateBranchName(ctx, repoPath, branch); err != nil {
		return ports.WorkspaceRepoInfo{}, err
	}
	if err := w.ensureBranchCheckedOut(ctx, repoPath, branch); err != nil {
		return ports.WorkspaceRepoInfo{}, err
	}
	baseSHA := ""
	if out, err := w.run(ctx, w.binary, "-C", repoPath, "rev-parse", "--verify", "HEAD"); err == nil {
		baseSHA = strings.TrimSpace(string(out))
	}
	return ports.WorkspaceRepoInfo{
		RepoName:     name,
		RepoPath:     repoPath,
		Path:         repoPath,
		Branch:       branch,
		BaseSHA:      baseSHA,
		SessionID:    cfg.SessionID,
		ProjectID:    cfg.ProjectID,
		RelativePath: relativePath,
	}, nil
}

// Restore is Create: there is no per-session artifact whose absence would make
// restoring different from starting.
func (w *Workspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	return w.Create(ctx, cfg)
}

// Destroy is a no-op. See the package doc: the path is the user's repository.
func (w *Workspace) Destroy(context.Context, ports.WorkspaceInfo) error { return nil }

// ForceDestroy is a no-op for the same reason Destroy is. In particular it
// must never reach gitworktree's os.RemoveAll backstop.
func (w *Workspace) ForceDestroy(context.Context, ports.WorkspaceInfo) error { return nil }

// StashUncommitted is a no-op returning no ref: AO never captures, moves, or
// rewrites uncommitted work in a repository it does not own. Returning ""
// (rather than an error) keeps the session manager's save/restore lifecycle —
// which calls StashUncommitted before every replace — working unchanged; ""
// already means "nothing was preserved" everywhere it is consumed.
func (w *Workspace) StashUncommitted(context.Context, ports.WorkspaceInfo) (string, error) {
	return "", nil
}

// ApplyPreserved is a no-op: this adapter never produces a preserve ref, so
// there is never one of its own to replay.
func (w *Workspace) ApplyPreserved(context.Context, ports.WorkspaceInfo, string) error { return nil }

// AddExclude appends ignore patterns to the repository's local
// .git/info/exclude, exactly as the worktree adapter does. This is local-only,
// never committed, and is how daemon-generated files (pasted task attachments)
// stay out of the user's git status.
func (w *Workspace) AddExclude(ctx context.Context, info ports.WorkspaceInfo, patterns ...string) error {
	if len(patterns) == 0 {
		return nil
	}
	path := strings.TrimSpace(info.Path)
	if path == "" {
		return errors.New("directbranch: exclude requires a workspace path")
	}
	out, err := w.run(ctx, w.binary, "-C", path, "rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("directbranch: resolve git dir: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	excludePath := filepath.Join(gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o750); err != nil {
		return fmt.Errorf("directbranch: exclude dir: %w", err)
	}
	existing, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("directbranch: read exclude: %w", err)
	}
	present := map[string]struct{}{}
	for _, line := range strings.Split(string(existing), "\n") {
		present[strings.TrimSpace(line)] = struct{}{}
	}
	var add []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := present[p]; ok {
			continue
		}
		present[p] = struct{}{}
		add = append(add, p)
	}
	if len(add) == 0 {
		return nil
	}
	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += strings.Join(add, "\n") + "\n"
	if err := os.WriteFile(excludePath, []byte(body), 0o600); err != nil {
		return fmt.Errorf("directbranch: write exclude: %w", err)
	}
	return nil
}

// MaterializeIntegrationCommit is deliberately unsupported. It exists to let a
// master workflow propagate task N's verified state into task N+1's *separate*
// worktree; in direct-branch mode every task already shares one working tree on
// one branch, so there is no second worktree to propagate into and any
// synthesized refs/ao/* integration head would be a fiction. Returning a typed
// error surfaces the unsupported combination (master workflow + direct branch)
// as integration_failed rather than silently producing a meaningless commit.
func (w *Workspace) MaterializeIntegrationCommit(context.Context, ports.WorkspaceInfo, string, string, string, []string) (string, string, bool, error) {
	return "", "", false, fmt.Errorf("directbranch: integration commits: %w", ErrNotSupported)
}

// CommitAll stages every tracked and non-ignored untracked change in the
// repository and commits it on the current branch with AO's own identity. This
// is the one place AO writes to the user's repository history, and it happens
// only when the project's GitPolicy.LocalCommit is automatic (or a human
// approved the commit) — the policy check lives in the workflow, never here.
//
// committed=false with a nil error means the working tree was clean: there was
// nothing to commit, which is a normal completion, not a failure.
func (w *Workspace) CommitAll(ctx context.Context, info ports.WorkspaceInfo, message string) (string, bool, error) {
	path := strings.TrimSpace(info.Path)
	if path == "" {
		return "", false, errors.New("directbranch: commit requires a workspace path")
	}
	if strings.TrimSpace(message) == "" {
		return "", false, errors.New("directbranch: commit requires a message")
	}
	dirty, err := w.isDirty(ctx, path)
	if err != nil {
		return "", false, err
	}
	if !dirty {
		return "", false, nil
	}
	if _, err := w.run(ctx, w.binary, "-C", path, "add", "-A"); err != nil {
		return "", false, fmt.Errorf("directbranch: stage changes: %w", err)
	}
	args := []string{
		"-C", path,
		"-c", "user.name=" + ports.WorkspaceCommitAuthorName,
		"-c", "user.email=" + ports.WorkspaceCommitAuthorEmail,
		"commit", "--no-verify", "-m", message,
	}
	if _, err := w.run(ctx, w.binary, args...); err != nil {
		return "", false, fmt.Errorf("directbranch: commit: %w", err)
	}
	head, err := w.run(ctx, w.binary, "-C", path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", true, fmt.Errorf("directbranch: resolve committed HEAD: %w", err)
	}
	return strings.TrimSpace(string(head)), true, nil
}

// ObserveWorkspace reports the repository's durable git facts. Identical in
// shape to the worktree adapter's observation so every consumer (handoff,
// verify, fingerprinting) works unchanged across both modes.
func (w *Workspace) ObserveWorkspace(ctx context.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	path := strings.TrimSpace(info.Path)
	if path == "" {
		return ports.WorkspaceObservation{}, errors.New("directbranch: observe workspace path is required")
	}
	head, err := w.run(ctx, w.binary, "-C", path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return ports.WorkspaceObservation{}, fmt.Errorf("directbranch: observe HEAD: %w", err)
	}
	branch := strings.TrimSpace(info.Branch)
	if out, branchErr := w.run(ctx, w.binary, "-C", path, "branch", "--show-current"); branchErr == nil {
		if current := strings.TrimSpace(string(out)); current != "" {
			branch = current
		}
	}
	statusOut, err := w.run(ctx, w.binary, "-C", path, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return ports.WorkspaceObservation{}, fmt.Errorf("directbranch: observe status: %w", err)
	}
	changes, staged, untracked := parseChanges(string(statusOut), maxObservedWorkspaceChanges)
	logOut, err := w.run(ctx, w.binary, "-C", path, "log", "-n", fmt.Sprintf("%d", maxObservedWorkspaceCommits), "--pretty=format:%H%x1f%s%x1f%aI%x1e")
	if err != nil {
		return ports.WorkspaceObservation{}, fmt.Errorf("directbranch: observe log: %w", err)
	}
	return ports.WorkspaceObservation{
		Path:      path,
		Branch:    branch,
		HeadSHA:   strings.TrimSpace(string(head)),
		Dirty:     len(changes) > 0,
		Staged:    staged,
		Untracked: untracked,
		Changes:   changes,
		Commits:   parseCommits(string(logOut)),
	}, nil
}

// PreflightRepository is the read-only safety probe the workflow runs before it
// commits an autonomous run to a repository/branch pair: is this a repository,
// is the configured branch reachable, what is currently checked out, and is
// there pre-existing uncommitted work. It never mutates anything, so a caller
// can surface dirty_worktree without having already half-started.
func (w *Workspace) PreflightRepository(ctx context.Context, repoPath, branch string) (ports.WorkspacePreflight, error) {
	repo, err := absPath(repoPath)
	if err != nil {
		return ports.WorkspacePreflight{}, err
	}
	out := ports.WorkspacePreflight{RepoPath: repo, ConfiguredBranch: strings.TrimSpace(branch)}
	if err := w.ensureRepository(ctx, repo); err != nil {
		return out, err
	}
	if current, cerr := w.run(ctx, w.binary, "-C", repo, "branch", "--show-current"); cerr == nil {
		out.CurrentBranch = strings.TrimSpace(string(current))
	}
	if head, herr := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--verify", "HEAD"); herr == nil {
		out.HeadSHA = strings.TrimSpace(string(head))
	}
	status, err := w.run(ctx, w.binary, "-C", repo, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return out, fmt.Errorf("directbranch: preflight status: %w", err)
	}
	changes, _, _ := parseChanges(string(status), maxObservedWorkspaceChanges)
	out.Dirty = len(changes) > 0
	out.Changes = changes
	return out, nil
}

func (w *Workspace) repoPath(projectID domain.ProjectID, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return absPath(override)
	}
	if w.repos == nil {
		return "", errors.New("directbranch: no repo resolver configured")
	}
	path, err := w.repos.RepoPath(projectID)
	if err != nil {
		return "", err
	}
	return absPath(path)
}

func (w *Workspace) ensureRepository(ctx context.Context, repo string) error {
	if _, err := os.Stat(repo); err != nil {
		return fmt.Errorf("directbranch: repository %q: %w", repo, err)
	}
	if _, err := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%w: %q", ErrNotARepository, repo)
	}
	return nil
}

func (w *Workspace) validateBranchName(ctx context.Context, repo, branch string) error {
	if _, err := w.run(ctx, w.binary, "-C", repo, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("directbranch: branch %q: %w", branch, ErrBranchInvalid)
	}
	return nil
}

// ensureBranchCheckedOut is where BRANCH FIDELITY is enforced. The configured
// branch is authoritative: it is never silently replaced by origin/HEAD, by
// main, or by whatever happened to be checked out. Switching is attempted only
// from a clean tree, so a checkout can never carry a user's uncommitted work
// onto a different branch.
func (w *Workspace) ensureBranchCheckedOut(ctx context.Context, repo, branch string) error {
	current := ""
	if out, err := w.run(ctx, w.binary, "-C", repo, "branch", "--show-current"); err == nil {
		current = strings.TrimSpace(string(out))
	}
	if current == branch {
		return nil
	}
	dirty, err := w.isDirty(ctx, repo)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("directbranch: repository %q is on branch %q with uncommitted changes; refusing to switch to %q: %w", repo, displayBranch(current), branch, ErrRepositoryDirty)
	}
	if w.localBranchExists(ctx, repo, branch) {
		if _, err := w.run(ctx, w.binary, "-C", repo, "checkout", branch); err != nil {
			return fmt.Errorf("directbranch: checkout %q: %w", branch, err)
		}
		return nil
	}
	// No local branch. A remote-tracking branch of the same name is the only
	// acceptable source: it is still the configured branch, just not fetched
	// into a local head yet. Anything else is a missing branch, surfaced as
	// such rather than invented from the current HEAD.
	remote := "origin/" + branch
	if w.refExists(ctx, repo, "refs/remotes/"+remote) {
		if _, err := w.run(ctx, w.binary, "-C", repo, "checkout", "-b", branch, "--track", remote); err != nil {
			return fmt.Errorf("directbranch: checkout tracking %q: %w", remote, err)
		}
		return nil
	}
	return fmt.Errorf("directbranch: branch %q does not exist in %q: %w", branch, repo, ErrBranchNotFetched)
}

func (w *Workspace) localBranchExists(ctx context.Context, repo, branch string) bool {
	return w.refExists(ctx, repo, "refs/heads/"+branch)
}

func (w *Workspace) refExists(ctx context.Context, repo, ref string) bool {
	out, err := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--verify", "--quiet", ref)
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func (w *Workspace) isDirty(ctx context.Context, path string) (bool, error) {
	out, err := w.run(ctx, w.binary, "-C", path, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("directbranch: status: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// resolveBranch picks the branch the session works on. Branch is the caller's
// explicit choice; BaseBranch is the project/repository configuration. There is
// no adapter-level fallback to "main": an unconfigured branch is an error, not
// a guess (see BRANCH FIDELITY).
func resolveBranch(cfg ports.WorkspaceConfig) string {
	if b := strings.TrimSpace(cfg.Branch); b != "" {
		return b
	}
	return strings.TrimSpace(cfg.BaseBranch)
}

func displayBranch(branch string) string {
	if branch == "" {
		return "(detached HEAD)"
	}
	return branch
}

func absPath(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", errors.New("directbranch: empty repository path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("directbranch: resolve %q: %w", path, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

func parseChanges(output string, limit int) ([]ports.WorkspaceChange, bool, bool) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	changes := make([]ports.WorkspaceChange, 0, min(len(lines), limit))
	var staged, untracked bool
	for _, line := range lines {
		if len(line) < 3 {
			continue
		}
		status := line[:2]
		path := strings.TrimSpace(line[3:])
		if path == "" {
			continue
		}
		if status == "??" {
			untracked = true
		} else if status[0] != ' ' && status[0] != '?' {
			staged = true
		}
		if len(changes) < limit {
			changes = append(changes, ports.WorkspaceChange{Path: path, Status: status})
		}
	}
	return changes, staged, untracked
}

func parseCommits(output string) []ports.WorkspaceCommit {
	records := strings.Split(output, "\x1e")
	commits := make([]ports.WorkspaceCommit, 0, len(records))
	for _, record := range records {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\x1f", 3)
		if len(fields) != 3 {
			continue
		}
		commits = append(commits, ports.WorkspaceCommit{
			SHA:        strings.TrimSpace(fields[0]),
			Subject:    strings.TrimSpace(fields[1]),
			AuthoredAt: strings.TrimSpace(fields[2]),
		})
	}
	return commits
}
