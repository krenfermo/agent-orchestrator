package projectmemory_test

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// sync_test.go — P2-B's lifecycle trigger (§2) and its single-flight (§3).
//
// The property under test throughout is the one the completion bar names: a
// WARM project's normal path must not be a scan. Four roles reaching for one
// repository at one commit must cost one sync at most, and usually none.

func syncFixture(t *testing.T, mode projectmemory.MemoryMode) (*fixture, *projectmemory.Service, *projectmemory.Syncer, string) {
	t.Helper()
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = mode
	root := goRepo(t)
	return f, svc, projectmemory.NewSyncer(svc, cfg), root
}

// A cold project indexes once, and the pass that does it reports doing so.
func TestEnsureFreshColdProjectIndexesOnce(t *testing.T) {
	f, svc, syncer, root := syncFixture(t, projectmemory.ModeAssisted)

	first := syncer.EnsureFresh(f.ctx, testProject, root)
	if first.Kind != projectmemory.SyncFull {
		t.Fatalf("a cold project synced as %q, want a full index: %+v", first.Kind, first)
	}
	if !first.Healthy() {
		t.Fatalf("the cold index left memory unusable: %+v", first)
	}
	status, ok, err := svc.Status(f.ctx, testProject, root)
	if err != nil || !ok {
		t.Fatalf("status: ok=%v err=%v", ok, err)
	}
	if status.Counts.Valid == 0 {
		t.Fatal("the cold index produced no valid facts")
	}
}

// The warm path, and the one the completion bar is about: a second task
// against an unchanged repository does NO indexing and reads NO files.
//
// It needs a real git checkout, and that is the honest shape of the guarantee
// rather than a test convenience: the warm path is a no-op because AO can PROVE
// memory is at the current commit, and a directory that reports no commit
// offers nothing to prove it against. See
// TestEnsureFreshWithoutACommitStaysUsableButNeverWarm.
func TestEnsureFreshWarmProjectIsANoOp(t *testing.T) {
	f, _, syncer, root := syncFixture(t, projectmemory.ModeAssisted)
	initGitRepo(t, root)
	if first := syncer.EnsureFresh(f.ctx, testProject, root); !first.Healthy() {
		t.Fatalf("cold index failed: %+v", first)
	}

	second := syncer.EnsureFresh(f.ctx, testProject, root)
	switch {
	case second.Kind != projectmemory.SyncNone:
		t.Fatalf("a warm project synced as %q, want none: %+v", second.Kind, second)
	case second.FilesRead != 0:
		t.Fatalf("the warm path read %d files", second.FilesRead)
	case !second.Healthy():
		t.Fatalf("the warm path did not report memory as usable: %+v", second)
	}

	if !second.Current {
		t.Fatalf("a warm git checkout could not prove currency: %+v", second)
	}
	stats := syncer.Stats()
	if stats.NoOp != 1 || stats.Full != 1 {
		t.Fatalf("stats = %+v, want exactly one full pass and one no-op", stats)
	}
}

// A project whose commit AO cannot read still gets memory — it just cannot
// prove currency, so it re-confirms rather than assuming.
//
// This is the deliberate split between Usable and Current: withholding memory
// from a scratch directory would be failing closed against a condition that is
// not a staleness, while claiming the warm path for it would assert a currency
// nothing established.
func TestEnsureFreshWithoutACommitStaysUsableButNeverWarm(t *testing.T) {
	f, svc, syncer, root := syncFixture(t, projectmemory.ModeAssisted)

	first := syncer.EnsureFresh(f.ctx, testProject, root)
	if !first.Usable {
		t.Fatalf("a commitless project got no usable memory: %+v", first)
	}
	if first.Current {
		t.Fatalf("a commitless project claimed provable currency: %+v", first)
	}

	second := syncer.EnsureFresh(f.ctx, testProject, root)
	if second.Kind == projectmemory.SyncNone {
		t.Fatalf("a commitless project took the warm path it cannot prove: %+v", second)
	}
	if !second.Usable {
		t.Fatalf("the re-confirming pass left memory unusable: %+v", second)
	}

	// And the memory is genuinely servable, which is the point of Usable.
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if pack.Empty() {
		t.Fatalf("a commitless project was served no memory: %s", pack.Stats.FallbackReason)
	}
}

// Four roles arriving together on the same authoritative state cost ONE sync.
// This is the coalescing the trigger design exists for.
func TestEnsureFreshCoalescesConcurrentCallers(t *testing.T) {
	f, _, syncer, root := syncFixture(t, projectmemory.ModeAssisted)

	const callers = 4
	var wg sync.WaitGroup
	results := make([]projectmemory.Freshness, callers)
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			results[i] = syncer.EnsureFresh(f.ctx, testProject, root)
		}()
	}
	wg.Wait()

	full, coalesced, noop := 0, 0, 0
	for _, r := range results {
		switch r.Kind {
		case projectmemory.SyncFull:
			full++
		case projectmemory.SyncCoalesced:
			coalesced++
		case projectmemory.SyncNone:
			noop++
		}
	}
	if full > 1 {
		t.Fatalf("%d of %d concurrent callers each ran a full index", full, callers)
	}
	if full+coalesced+noop != callers {
		t.Fatalf("callers resolved as full=%d coalesced=%d noop=%d, want %d total",
			full, coalesced, noop, callers)
	}
	// Whatever the interleaving, exactly one caller may have read files.
	readers := 0
	for _, r := range results {
		if r.FilesRead > 0 {
			readers++
		}
	}
	if readers > 1 {
		t.Fatalf("%d callers reported reading files; a coalesced sync must be counted once", readers)
	}
}

// A one-file change syncs incrementally: only the changed path is read.
func TestEnsureFreshOneFileChangeIsIncremental(t *testing.T) {
	f, _, syncer, root := syncFixture(t, projectmemory.ModeAssisted)
	repo := initGitRepo(t, root)

	if first := syncer.EnsureFresh(f.ctx, testProject, root); !first.Healthy() {
		t.Fatalf("cold index failed: %+v", first)
	}

	writeTree(t, root, map[string]string{
		"docs/architecture.md": "# Architecture\n\nThe daemon owns all state, and now the queue too.\n",
	})
	repo.commit(t, "change one document")

	second := syncer.EnsureFresh(f.ctx, testProject, root)
	if second.Kind != projectmemory.SyncIncremental {
		t.Fatalf("a one-file change synced as %q, want incremental: %+v", second.Kind, second)
	}
	if second.FilesRead != 1 {
		t.Fatalf("an incremental sync read %d files for a one-file change", second.FilesRead)
	}
	if !second.Healthy() {
		t.Fatalf("the incremental sync left memory unusable: %+v", second)
	}
}

// A disabled mode does nothing at all, which is what makes the default off a
// real default.
func TestEnsureFreshDoesNothingWhenMemoryIsOff(t *testing.T) {
	f, svc, syncer, root := syncFixture(t, projectmemory.ModeOff)

	fresh := syncer.EnsureFresh(f.ctx, testProject, root)
	if fresh.Kind != projectmemory.SyncSkipped {
		t.Fatalf("kind = %q with memory off", fresh.Kind)
	}
	if _, ok, _ := svc.Status(f.ctx, testProject, root); ok {
		t.Fatal("a disabled syncer registered a repository for indexing")
	}
}

// An unreachable repository is a degradation, never a failure.
func TestEnsureFreshOnAMissingRepositoryDegrades(t *testing.T) {
	f, _, syncer, _ := syncFixture(t, projectmemory.ModeAssisted)

	fresh := syncer.EnsureFresh(f.ctx, testProject, filepath.Join(t.TempDir(), "does-not-exist"))
	if fresh.Kind != projectmemory.SyncSkipped {
		t.Fatalf("kind = %q for a missing repository", fresh.Kind)
	}
	if fresh.Healthy() {
		t.Fatal("a missing repository reported healthy memory")
	}
	if fresh.Reason == "" {
		t.Fatal("the degradation does not say why")
	}
}

// A sync that overruns its budget is abandoned and the caller proceeds. The
// timeout is what makes "memory never blocks a dispatch" a bound.
func TestEnsureFreshAbandonsAnOverrunningSync(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	cfg.SyncTimeout = time.Millisecond
	syncer := projectmemory.NewSyncer(svc, cfg)

	root := t.TempDir()
	files := map[string]string{}
	for i := range 400 {
		files[filepath.Join("pkg", "m"+itoa(i), "file.go")] = strings.Repeat("// filler\n", 200)
	}
	writeTree(t, root, files)

	fresh := syncer.EnsureFresh(f.ctx, testProject, root)
	// Either it finished inside the budget (a fast machine) or it was
	// abandoned — but it must never return an unusable half-state without
	// saying so.
	if fresh.Kind == projectmemory.SyncSkipped && fresh.Reason == "" {
		t.Fatal("an abandoned sync did not say why")
	}
	if fresh.Kind == projectmemory.SyncSkipped && fresh.Healthy() {
		t.Fatal("an abandoned sync claimed memory was current")
	}
}

// Invalidating a task's changed paths retires exactly the memory they proved,
// which is what stops a reviewer being handed a pre-change summary.
func TestSyncerInvalidatePathsRetiresOnlyThoseFacts(t *testing.T) {
	f, svc, syncer, root := syncFixture(t, projectmemory.ModeAssisted)
	if first := syncer.EnsureFresh(f.ctx, testProject, root); !first.Healthy() {
		t.Fatalf("cold index failed: %+v", first)
	}

	n, err := syncer.InvalidatePaths(f.ctx, testProject, root, []string{"AGENTS.md"}, "changed by task t1")
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("invalidating a changed path retired nothing")
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleReviewer,
	})
	if strings.Contains(pack.Render(), "Keep every change surgical") {
		t.Fatal("a fact the task's own change disproved is still being served")
	}
}

// --- helpers ---------------------------------------------------------------

type gitRepo struct{ root string }

// initGitRepo makes the fixture a real git checkout, which the incremental
// path needs: without a commit history there is no change set to derive, and
// Sync correctly falls back to a full pass.
func initGitRepo(t *testing.T, root string) gitRepo {
	t.Helper()
	repo := gitRepo{root: root}
	repo.run(t, "init", "-q", "-b", "main")
	repo.run(t, "config", "user.email", "test@example.com")
	repo.run(t, "config", "user.name", "test")
	repo.commit(t, "initial")
	return repo
}

func (g gitRepo) run(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (g gitRepo) commit(t *testing.T, message string) {
	t.Helper()
	g.run(t, "add", "-A")
	g.run(t, "commit", "-q", "--allow-empty", "-m", message)
}

func itoa(n int) string { return strconv.Itoa(n) }
