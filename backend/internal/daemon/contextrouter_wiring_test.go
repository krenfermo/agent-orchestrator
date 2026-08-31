package daemon

import (
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
)

// The wiring's default: with the flag unset there is no router, which is what
// makes wfrouter.Instrument a no-op and leaves every provider adapter on the
// pre-existing full-context path.
func TestContextRouterDisabledByDefault(t *testing.T) {
	if _, set := os.LookupEnv(contextrouter.FlagEnv); set {
		t.Setenv(contextrouter.FlagEnv, "")
		os.Unsetenv(contextrouter.FlagEnv)
	}
	if router := contextRouterFor(nil, nil, nil); router != nil {
		t.Fatalf("%s is unset yet a router was built", contextrouter.FlagEnv)
	}
}

func TestContextRouterBuiltWhenEnabled(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv(contextrouter.FlagEnv, "1")
	router := contextRouterFor(nil, nil, nil)
	if router == nil {
		t.Fatal("the flag is on yet no router was built")
	}
	if got, want := router.BudgetFor(contextrouter.RolePlanner), contextrouter.DefaultBudgets().For(contextrouter.RolePlanner); got != want {
		t.Fatalf("planner budget = %+v, want the documented default %+v", got, want)
	}
}

// A nil memory repository is the pre-P2-A wiring, and must still build a
// router: project memory is additive evidence, never a precondition for
// routing.
func TestContextRouterBuiltWithoutDurableMemory(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv(contextrouter.FlagEnv, "1")
	if router := contextRouterFor(nil, nil, nil); router == nil {
		t.Fatal("no router was built without a durable memory repository")
	}
}

// A mistyped budget override disables routing rather than silently applying a
// budget the operator did not write.
func TestContextRouterDisabledByRejectedBudgetOverride(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv(contextrouter.FlagEnv, "1")
	t.Setenv(contextrouter.BudgetEnv, "planner=30/20/10")
	if router := contextRouterFor(nil, nil, nil); router != nil {
		t.Fatal("an incoherent budget override still produced a router")
	}
}

func TestContextRouterAppliesBudgetOverride(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv(contextrouter.FlagEnv, "1")
	t.Setenv(contextrouter.BudgetEnv, "verify=100/200/300")
	router := contextRouterFor(nil, nil, nil)
	if router == nil {
		t.Fatal("no router was built")
	}
	want := contextrouter.Budget{CompactTokens: 100, ExpandedTokens: 200, HardCapTokens: 300}
	if got := router.BudgetFor(contextrouter.RoleVerify); got != want {
		t.Fatalf("verify budget = %+v, want %+v", got, want)
	}
}
