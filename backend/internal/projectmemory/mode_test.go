package projectmemory_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// mode_test.go — the rollout switch and its configuration (P2-B §4, §19).
//
// Two properties matter more than the parsing itself: the default is off, and
// a malformed override is an ERROR rather than a silent fallback. The second
// is what stops a mistyped setting from producing a dispatch shaped by a policy
// nobody chose.

func TestDefaultConfigIsConservative(t *testing.T) {
	cfg := projectmemory.DefaultConfig()
	switch {
	case cfg.Mode != projectmemory.ModeOff:
		t.Errorf("default mode = %q, want off", cfg.Mode)
	case cfg.Mode.Enabled():
		t.Error("the default mode reports itself enabled")
	case !cfg.CacheEnabled:
		t.Error("the cache is off by default; it should be on")
	case cfg.SyncTimeout <= 0:
		t.Error("the default sync timeout is not positive")
	}
	if err := cfg.Budgets.Validate(); err != nil {
		t.Fatalf("the default budgets do not validate: %v", err)
	}
}

func TestModeAuthority(t *testing.T) {
	for _, tc := range []struct {
		mode            projectmemory.MemoryMode
		enabled, mayAdd bool
	}{
		{projectmemory.ModeOff, false, false},
		{projectmemory.ModeAssisted, true, false},
		{projectmemory.ModePreferred, true, true},
	} {
		if tc.mode.Enabled() != tc.enabled {
			t.Errorf("%s.Enabled() = %v", tc.mode, tc.mode.Enabled())
		}
		if tc.mode.MayReplace() != tc.mayAdd {
			t.Errorf("%s.MayReplace() = %v", tc.mode, tc.mode.MayReplace())
		}
	}
}

func TestConfigFromEnvReadsEveryKnob(t *testing.T) {
	t.Setenv(projectmemory.ModeEnv, "preferred")
	t.Setenv(projectmemory.SyncTimeoutEnv, "5s")
	t.Setenv(projectmemory.CacheEnv, "false")
	t.Setenv(projectmemory.MaxFilesEnv, "1234")
	t.Setenv(projectmemory.BudgetEnv, "worker=8k/12/1")

	cfg, err := projectmemory.ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case cfg.Mode != projectmemory.ModePreferred:
		t.Errorf("mode = %q", cfg.Mode)
	case cfg.SyncTimeout != 5*time.Second:
		t.Errorf("sync timeout = %s", cfg.SyncTimeout)
	case cfg.CacheEnabled:
		t.Error("cache was not disabled")
	case cfg.IndexLimits.MaxFiles != 1234:
		t.Errorf("maxFiles = %d", cfg.IndexLimits.MaxFiles)
	}
	worker := cfg.Budgets.For(projectmemory.RoleWorker)
	if worker.MaxBytes != 8*1024 || worker.MaxItems != 12 || worker.MaxDocuments != 1 {
		t.Fatalf("worker budget = %+v", worker)
	}
	// An override names only the roles it changes; the rest keep their
	// documented defaults.
	if planner := cfg.Budgets.For(projectmemory.RolePlanner); planner != projectmemory.DefaultBudgets()[projectmemory.RolePlanner] {
		t.Fatalf("planner budget = %+v, want the default", planner)
	}
}

// A malformed value must be refused, not ignored.
func TestConfigFromEnvRefusesMalformedOverrides(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{projectmemory.ModeEnv, "aggressive"},
		{projectmemory.SyncTimeoutEnv, "soon"},
		{projectmemory.SyncTimeoutEnv, "-5s"},
		{projectmemory.CacheEnv, "sometimes"},
		{projectmemory.MaxFilesEnv, "0"},
		{projectmemory.MaxFilesEnv, "lots"},
		{projectmemory.BudgetEnv, "worker=8k"},
		{projectmemory.BudgetEnv, "nobody=8k/12"},
		{projectmemory.BudgetEnv, "worker=0/12"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := projectmemory.ConfigFromEnv(); err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.value)
			}
		})
	}
}

func TestParseBudgetsAcceptsSizeSuffixes(t *testing.T) {
	budgets, err := projectmemory.ParseBudgets("planner=32k/50/6,worker=16384/20")
	if err != nil {
		t.Fatal(err)
	}
	if got := budgets[projectmemory.RolePlanner]; got.MaxBytes != 32*1024 || got.MaxDocuments != 6 {
		t.Fatalf("planner = %+v", got)
	}
	if got := budgets[projectmemory.RoleWorker]; got.MaxBytes != 16384 || got.MaxDocuments != 0 {
		t.Fatalf("worker = %+v", got)
	}
}

// A budget table missing a role AO builds packs for is refused: a role budgeted
// by whatever default happened to be nearby is the invisible policy the table
// exists to prevent.
func TestBudgetSetRequiresEveryRole(t *testing.T) {
	partial := projectmemory.BudgetSet{
		projectmemory.RolePlanner: {MaxBytes: 1024, MaxItems: 4},
	}
	if err := partial.Validate(); err == nil {
		t.Fatal("a table missing three roles validated")
	}
}

func TestRoleBudgetEstimatesTokensFromBytes(t *testing.T) {
	b := projectmemory.RoleBudget{MaxBytes: 4096, MaxItems: 10}
	if got := b.EstimatedMaxTokens(); got != 1024 {
		t.Fatalf("estimated tokens = %d, want 1024", got)
	}
}

func TestConfigDescribeNamesThePolicyInForce(t *testing.T) {
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	got := cfg.Describe()
	for _, want := range []string{"mode=assisted", "cache=true", "planner=", "worker="} {
		if !strings.Contains(got, want) {
			t.Errorf("describe() = %q, missing %q", got, want)
		}
	}
}
