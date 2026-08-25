package contextrouter

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultBudgetsAreCoherent(t *testing.T) {
	budgets := DefaultBudgets()
	if err := budgets.Validate(); err != nil {
		t.Fatalf("the documented defaults do not validate: %v", err)
	}
	for _, role := range Roles() {
		budget := budgets.For(role)
		if got := budget.LimitFor(TierCompact); got != budget.CompactTokens {
			t.Fatalf("role %q compact limit = %d, want %d", role, got, budget.CompactTokens)
		}
		if got := budget.LimitFor(TierExpanded); got != budget.ExpandedTokens {
			t.Fatalf("role %q expanded limit = %d, want %d", role, got, budget.ExpandedTokens)
		}
	}
}

// LimitFor clamps to the hard cap even for a budget that was never validated,
// which is what makes the cap an invariant of the assembly path rather than of
// the configuration path alone.
func TestLimitForClampsToTheHardCap(t *testing.T) {
	budget := Budget{CompactTokens: 9_000, ExpandedTokens: 50_000, HardCapTokens: 1_000}
	if got := budget.LimitFor(TierCompact); got != 1_000 {
		t.Fatalf("compact limit = %d, want the cap 1000", got)
	}
	if got := budget.LimitFor(TierExpanded); got != 1_000 {
		t.Fatalf("expanded limit = %d, want the cap 1000", got)
	}
}

func TestBudgetValidateRejectsIncoherentLimits(t *testing.T) {
	cases := map[string]Budget{
		"zero compact":        {CompactTokens: 0, ExpandedTokens: 10, HardCapTokens: 20},
		"negative cap":        {CompactTokens: 5, ExpandedTokens: 10, HardCapTokens: -1},
		"compact over expand": {CompactTokens: 20, ExpandedTokens: 10, HardCapTokens: 30},
		"expand over cap":     {CompactTokens: 5, ExpandedTokens: 40, HardCapTokens: 30},
	}
	for name, budget := range cases {
		t.Run(name, func(t *testing.T) {
			if err := budget.Validate(); !errors.Is(err, ErrBudget) {
				t.Fatalf("got %v, want ErrBudget", err)
			}
		})
	}
}

func TestBudgetSetValidateRequiresEveryRole(t *testing.T) {
	incomplete := DefaultBudgets()
	delete(incomplete, RoleVerify)
	if err := incomplete.Validate(); !errors.Is(err, ErrBudget) {
		t.Fatalf("a table missing a role validated: %v", err)
	}
	unknown := DefaultBudgets()
	unknown[Role("architect")] = Budget{CompactTokens: 1, ExpandedTokens: 2, HardCapTokens: 3}
	if err := unknown.Validate(); !errors.Is(err, ErrBudget) {
		t.Fatalf("a table with an unknown role validated: %v", err)
	}
	empty := BudgetSet{}
	if err := empty.Validate(); !errors.Is(err, ErrBudget) {
		t.Fatalf("an empty table validated: %v", err)
	}
}

func TestBudgetSetWithDoesNotMutateTheOriginal(t *testing.T) {
	base := DefaultBudgets()
	updated, err := base.With(RoleFix, Budget{CompactTokens: 10, ExpandedTokens: 20, HardCapTokens: 30})
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	if got := updated.For(RoleFix).HardCapTokens; got != 30 {
		t.Fatalf("override not applied: cap = %d", got)
	}
	if got := base.For(RoleFix).HardCapTokens; got != DefaultBudgets().For(RoleFix).HardCapTokens {
		t.Fatalf("With mutated the original table: cap = %d", got)
	}
	if _, err := base.With("architect", Budget{CompactTokens: 1, ExpandedTokens: 2, HardCapTokens: 3}); !errors.Is(err, ErrBudget) {
		t.Fatalf("With accepted an unknown role: %v", err)
	}
	if _, err := base.With(RoleFix, Budget{CompactTokens: 40, ExpandedTokens: 20, HardCapTokens: 30}); !errors.Is(err, ErrBudget) {
		t.Fatalf("With accepted an incoherent budget: %v", err)
	}
}

func TestParseBudgetOverrides(t *testing.T) {
	budgets, err := ParseBudgetOverrides("")
	if err != nil {
		t.Fatalf("empty spec: %v", err)
	}
	if got, want := budgets.For(RolePlanner), DefaultBudgets().For(RolePlanner); got != want {
		t.Fatalf("empty spec changed the defaults: %+v vs %+v", got, want)
	}

	budgets, err = ParseBudgetOverrides(" planner=8000/20000/26000 , verify=400/900/1200 ")
	if err != nil {
		t.Fatalf("ParseBudgetOverrides: %v", err)
	}
	if got := budgets.For(RolePlanner); got != (Budget{CompactTokens: 8000, ExpandedTokens: 20000, HardCapTokens: 26000}) {
		t.Fatalf("planner override not applied: %+v", got)
	}
	if got := budgets.For(RoleVerify); got != (Budget{CompactTokens: 400, ExpandedTokens: 900, HardCapTokens: 1200}) {
		t.Fatalf("verify override not applied: %+v", got)
	}
	if got, want := budgets.For(RoleWorker), DefaultBudgets().For(RoleWorker); got != want {
		t.Fatalf("an un-overridden role drifted: %+v vs %+v", got, want)
	}

	for name, spec := range map[string]string{
		"no equals":     "planner:1/2/3",
		"unknown role":  "architect=1/2/3",
		"two limits":    "planner=1/2",
		"not a number":  "planner=a/2/3",
		"incoherent":    "planner=30/20/10",
		"non positive":  "planner=0/2/3",
		"trailing junk": "planner=1/2/3,,worker=oops",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBudgetOverrides(spec); !errors.Is(err, ErrBudget) {
				t.Fatalf("spec %q was accepted: %v", spec, err)
			}
		})
	}
}

func TestBudgetsFromEnv(t *testing.T) {
	t.Setenv(BudgetEnv, "fix=100/200/300")
	budgets, err := BudgetsFromEnv()
	if err != nil {
		t.Fatalf("BudgetsFromEnv: %v", err)
	}
	if got := budgets.For(RoleFix); got != (Budget{CompactTokens: 100, ExpandedTokens: 200, HardCapTokens: 300}) {
		t.Fatalf("env override not applied: %+v", got)
	}

	t.Setenv(BudgetEnv, "fix=nonsense")
	if _, err := BudgetsFromEnv(); !errors.Is(err, ErrBudget) {
		t.Fatalf("a malformed env spec was accepted: %v", err)
	}
}

func TestDescribeListsEveryRole(t *testing.T) {
	described := DefaultBudgets().Describe()
	for _, role := range Roles() {
		if !strings.Contains(described, string(role)+"=") {
			t.Fatalf("Describe() = %q, missing role %q", described, role)
		}
	}
}

func TestNewRejectsAnInvalidBudgetTable(t *testing.T) {
	broken := DefaultBudgets()
	delete(broken, RoleWorker)
	if _, err := New(Options{Budgets: broken}); !errors.Is(err, ErrBudget) {
		t.Fatalf("New accepted an invalid table: %v", err)
	}
}

// A router copies the table it was given, so an edit to the caller's map
// cannot change the budgets in force.
func TestRouterCopiesItsBudgetTable(t *testing.T) {
	budgets := DefaultBudgets()
	router, err := New(Options{Budgets: budgets})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	budgets[RolePlanner] = Budget{CompactTokens: 1, ExpandedTokens: 2, HardCapTokens: 3}
	if got := router.BudgetFor(RolePlanner).HardCapTokens; got != DefaultBudgets().For(RolePlanner).HardCapTokens {
		t.Fatalf("the router observed an edit to the caller's table: cap = %d", got)
	}
	returned := router.Budgets()
	returned[RolePlanner] = Budget{CompactTokens: 1, ExpandedTokens: 2, HardCapTokens: 3}
	if got := router.BudgetFor(RolePlanner).HardCapTokens; got != DefaultBudgets().For(RolePlanner).HardCapTokens {
		t.Fatalf("Budgets() handed out the live table: cap = %d", got)
	}
}
