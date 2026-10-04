package projectmemory_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// freshness_test.go — Frente 3 / 3B: context generated for commit A is never
// silently presented as current for commit B. Every attached pack carries an
// AO-written freshness notice (CURRENT / STALE / UNVERIFIED / PARTIAL), and
// the cases that must not be served at all are withheld.

func headOf(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

type freshFixture struct {
	f    *fixture
	svc  *projectmemory.Service
	root string
}

func newFreshFixture(t *testing.T, opts ...projectmemory.ServiceOption) freshFixture {
	t.Helper()
	requireGit(t)
	f := newFixture(t)
	root := goRepo(t)
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-q", "-m", "c1")
	opts = append([]projectmemory.ServiceOption{projectmemory.WithCodeGraph(codegraph.NewIndex(f.store))}, opts...)
	return freshFixture{f: f, svc: projectmemory.NewService(f.store, opts...), root: root}
}

func (x freshFixture) provision(t *testing.T, timeout time.Duration) projectmemory.Provisioned {
	t.Helper()
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	if timeout > 0 {
		cfg.SyncTimeout = timeout
	}
	return projectmemory.NewProvisioner(x.svc, cfg).Provision(x.f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: x.root, Role: projectmemory.RoleWorker,
	})
}

func (x freshFixture) commit(t *testing.T, msg string, files map[string]string) string {
	t.Helper()
	writeTree(t, x.root, files)
	gitRun(t, x.root, "add", "-A")
	gitRun(t, x.root, "commit", "-q", "-m", msg)
	return headOf(t, x.root)
}

func assertVerdict(t *testing.T, out projectmemory.Provisioned, want projectmemory.FreshnessVerdict, mustSay ...string) {
	t.Helper()
	if !out.Attached() {
		t.Fatalf("nothing attached: %s", out.Metrics.FallbackReason)
	}
	if got := out.Verdict(); got != want {
		t.Fatalf("verdict = %s, want %s\n%s", got, want, out.Render())
	}
	rendered := out.Render()
	for _, s := range mustSay {
		if !strings.Contains(rendered, s) {
			t.Fatalf("rendered pack does not say %q:\n%s", s, rendered)
		}
	}
}

func TestFreshnessFollowsCommitsAndBranchSwitches(t *testing.T) {
	x := newFreshFixture(t)
	c1 := headOf(t, x.root)
	assertVerdict(t, x.provision(t, 0), projectmemory.FreshnessCurrent, "MEMORY FRESHNESS: CURRENT", c1[:12])

	c2 := x.commit(t, "c2", map[string]string{"docs/architecture.md": "# Architecture\n\nNow with a queue.\n"})
	out := x.provision(t, 0)
	assertVerdict(t, out, projectmemory.FreshnessCurrent, c2[:12])
	if strings.Contains(out.Render(), c1[:12]) {
		t.Fatalf("a pack for %s still names %s as its commit", c2[:12], c1[:12])
	}

	gitRun(t, x.root, "checkout", "-q", "-b", "feature")
	c3 := x.commit(t, "c3", map[string]string{"internal/store/extra.go": "package store\n\nfunc Extra() {}\n"})
	assertVerdict(t, x.provision(t, 0), projectmemory.FreshnessCurrent, c3[:12])

	gitRun(t, x.root, "checkout", "-q", "main")
	assertVerdict(t, x.provision(t, 0), projectmemory.FreshnessCurrent, c2[:12])
}

// The sync that should bring memory forward does not complete (here: a 1ns
// budget). Memory from the earlier commit may still orient an agent, but it is
// labelled STALE with both commits -- never presented as current.
func TestMemoryFromAnEarlierCommitIsLabelledStale(t *testing.T) {
	x := newFreshFixture(t)
	c1 := headOf(t, x.root)
	assertVerdict(t, x.provision(t, 0), projectmemory.FreshnessCurrent)

	c2 := x.commit(t, "c2", map[string]string{"README.md": "# App\n\nRewritten.\n"})
	out := x.provision(t, time.Nanosecond)
	assertVerdict(t, out, projectmemory.FreshnessStale, "MEMORY FRESHNESS: STALE", c1[:12], c2[:12])
	if strings.Contains(out.Render(), "CURRENT") {
		t.Fatal("a stale pack also claims to be current")
	}
}

// A dirty worktree: memory is labelled with HEAD, and the consumer is told the
// checkout has uncommitted changes, because the indexer read them from disk.
func TestDirtyWorktreeIsDeclared(t *testing.T) {
	x := newFreshFixture(t)
	assertVerdict(t, x.provision(t, 0), projectmemory.FreshnessCurrent)
	writeTree(t, x.root, map[string]string{"README.md": "# App\n\nUncommitted edit.\n"})
	out := x.provision(t, 0)
	if out.Freshness.DirtyTracked != 1 {
		t.Fatalf("DirtyTracked = %d, want 1", out.Freshness.DirtyTracked)
	}
	if !strings.Contains(out.Render(), "uncommitted changes to 1 tracked file") {
		t.Fatalf("a dirty checkout is not declared:\n%s", out.Render())
	}
}

// A pass that stopped at its file bound is PARTIAL, and the consumer is told
// that absence is not evidence. (Also proves AO_MEMORY_MAX_FILES-style limits
// now reach the pass.)
func TestPartialIndexIsDeclared(t *testing.T) {
	limits := projectmemory.DefaultIndexLimits()
	limits.MaxFiles = 3
	x := newFreshFixture(t, projectmemory.WithIndexerLimits(limits))
	out := x.provision(t, 0)
	assertVerdict(t, out, projectmemory.FreshnessPartial, "MEMORY FRESHNESS: PARTIAL", "3-file bound", "not evidence of absence")
}

// A pass that crashed mid-flight (a claimed pass never completed) is not a
// complete view: the pack either stays on the last completed commit with a
// STALE notice, or is withheld -- never a CURRENT claim.
func TestCrashedRefreshIsNeverServedAsCurrent(t *testing.T) {
	x := newFreshFixture(t)
	first := x.provision(t, 0)
	assertVerdict(t, first, projectmemory.FreshnessCurrent)

	c2 := x.commit(t, "c2", map[string]string{"README.md": "# App\n\nMoved on.\n"})
	// Another process claimed a pass at c2 and died before completing it.
	if _, ok, err := x.f.store.ClaimProjectMemoryIndexPass(x.f.ctx, testProject, first.Freshness.RepoID, c2, "main", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	out := x.provision(t, 0)
	if out.Attached() {
		if out.Verdict() == projectmemory.FreshnessCurrent {
			t.Fatalf("memory was presented as current while a pass at %s is incomplete:\n%s", c2[:12], out.Render())
		}
		if !strings.Contains(out.Render(), "MEMORY FRESHNESS:") {
			t.Fatal("attached pack carries no freshness notice")
		}
	}
}

// A file the extractor cannot parse does not take the pack down and is not
// silently served as structure: the notice states what the graph could not do,
// or the graph stays usable for everything else.
func TestParserFailureIsVisibleNotSilent(t *testing.T) {
	x := newFreshFixture(t)
	writeTree(t, x.root, map[string]string{"internal/broken/broken.go": "package broken\n\nfunc (((( {\n"})
	gitRun(t, x.root, "add", "-A")
	gitRun(t, x.root, "commit", "-q", "-m", "broken")
	out := x.provision(t, 0)
	if !out.Attached() {
		t.Fatalf("a parse failure in one file withheld all memory: %s", out.Metrics.FallbackReason)
	}
	rendered := out.Render()
	if !strings.Contains(rendered, "MEMORY FRESHNESS:") {
		t.Fatal("no freshness notice")
	}
	if out.Freshness.Graph.Attempted && !out.Freshness.Graph.Usable && !strings.Contains(rendered, "CODE GRAPH: UNAVAILABLE") {
		t.Fatalf("an unusable graph is not declared:\n%s", rendered)
	}
}

// Memory is withheld -- not degraded -- when no pass ever completed.
func TestNoCompletedPassIsWithheld(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := projectmemory.NewService(f.store)
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	cfg.SyncTimeout = time.Nanosecond // the first pass cannot complete
	out := projectmemory.NewProvisioner(svc, cfg).Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if out.Attached() {
		t.Fatalf("memory attached with no completed pass:\n%s", out.Render())
	}
	_ = domain.MemoryStateValid
}
