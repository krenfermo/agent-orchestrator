package contextrouter

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ErrBudget is the sentinel every rejected budget configuration wraps.
var ErrBudget = errors.New("contextrouter: invalid budget")

// Budget is one role's three token limits.
//
// The three are not redundant. CompactTokens is what a first pass aims for and
// is deliberately small — most dispatches never need more. ExpandedTokens is
// what an expansion aims for once the compact pass proved insufficient.
// HardCapTokens is the line the router will not cross for this role under any
// circumstances, including a forced expansion: it clamps both targets, so a
// misconfiguration that sets ExpandedTokens above it buys nothing rather than
// silently leaking a larger payload.
type Budget struct {
	// CompactTokens targets the first, cheap retrieval pass.
	CompactTokens int `json:"compactTokens"`
	// ExpandedTokens targets an expansion.
	ExpandedTokens int `json:"expandedTokens"`
	// HardCapTokens is never exceeded.
	HardCapTokens int `json:"hardCapTokens"`
}

// Validate rejects a budget that cannot be honoured: a non-positive limit, or
// limits that do not increase from compact through expanded to the cap. A
// compact target above the cap would make every first pass truncate; an
// expanded target below the compact one would make expansion shrink the
// payload, which is the opposite of what the caller asked for.
func (b Budget) Validate() error {
	if b.CompactTokens <= 0 || b.ExpandedTokens <= 0 || b.HardCapTokens <= 0 {
		return fmt.Errorf("%w: every limit must be positive (compact=%d expanded=%d cap=%d)", ErrBudget, b.CompactTokens, b.ExpandedTokens, b.HardCapTokens)
	}
	if b.CompactTokens > b.ExpandedTokens {
		return fmt.Errorf("%w: compact %d exceeds expanded %d", ErrBudget, b.CompactTokens, b.ExpandedTokens)
	}
	if b.ExpandedTokens > b.HardCapTokens {
		return fmt.Errorf("%w: expanded %d exceeds hard cap %d", ErrBudget, b.ExpandedTokens, b.HardCapTokens)
	}
	return nil
}

// LimitFor returns the token target to pack a tier against, already clamped to
// the hard cap. Every assembly goes through it, which is why no code path can
// pack against a target the cap does not allow.
func (b Budget) LimitFor(tier Tier) int {
	limit := b.CompactTokens
	if tier == TierExpanded {
		limit = b.ExpandedTokens
	}
	if limit > b.HardCapTokens {
		return b.HardCapTokens
	}
	return limit
}

// BudgetSet is the per-role budget table.
type BudgetSet map[Role]Budget

// DefaultBudgets are the documented defaults.
//
// The shape is deliberate and is the router's central policy claim: a planner
// or a reviewer is reasoning about code it has not seen, so it gets the room
// to look; a fix or a verify dispatch is acting on a conclusion that has
// already been reached elsewhere, so it gets what that conclusion rests on and
// nothing more.
//
//	role      compact  expanded  hard cap    why
//	planner     6000     18000     24000     plans over a whole objective; reads documents first
//	reviewer    5000     15000     20000     judges a change against its surroundings
//	worker      4000     10000     14000     implements one task; the diff and its symbols dominate
//	fix         2000      5000      7000     delivered into a session that already holds the history
//	verify       500      1200      1600     runs a command; needs the task and what changed
//
// Every figure is an estimate in the bytes/4 heuristic this package shares with
// the baseline harness, not a provider token count.
func DefaultBudgets() BudgetSet {
	return BudgetSet{
		RolePlanner:  {CompactTokens: 6000, ExpandedTokens: 18000, HardCapTokens: 24000},
		RoleReviewer: {CompactTokens: 5000, ExpandedTokens: 15000, HardCapTokens: 20000},
		RoleWorker:   {CompactTokens: 4000, ExpandedTokens: 10000, HardCapTokens: 14000},
		RoleFix:      {CompactTokens: 2000, ExpandedTokens: 5000, HardCapTokens: 7000},
		RoleVerify:   {CompactTokens: 500, ExpandedTokens: 1200, HardCapTokens: 1600},
	}
}

// Clone returns an independent copy, so a caller can hand a router a budget
// table without the router observing later edits to it.
func (s BudgetSet) Clone() BudgetSet {
	out := make(BudgetSet, len(s))
	for role, budget := range s {
		out[role] = budget
	}
	return out
}

// Validate checks that every role is budgeted and every budget is honourable.
// A missing role is an error rather than a silent fallback: a role with no
// budget would otherwise be routed against whatever default happened to be
// nearby, which is exactly the kind of invisible policy this table exists to
// make explicit.
func (s BudgetSet) Validate() error {
	if len(s) == 0 {
		return fmt.Errorf("%w: budget table is empty", ErrBudget)
	}
	for _, role := range Roles() {
		budget, ok := s[role]
		if !ok {
			return fmt.Errorf("%w: role %q has no budget", ErrBudget, role)
		}
		if err := budget.Validate(); err != nil {
			return fmt.Errorf("role %q: %w", role, err)
		}
	}
	for role := range s {
		if !role.Valid() {
			return fmt.Errorf("%w: unknown role %q", ErrBudget, role)
		}
	}
	return nil
}

// For returns a role's budget. It is only ever called after Validate, so a
// missing role cannot reach it; the zero value it would return is caught by
// Budget.Validate at construction rather than producing an unbounded payload.
func (s BudgetSet) For(role Role) Budget { return s[role] }

// With returns a validated copy of the table with one role's budget replaced.
// It copies rather than mutating so a rejected override cannot leave a caller
// holding a half-applied table.
func (s BudgetSet) With(role Role, budget Budget) (BudgetSet, error) {
	if !role.Valid() {
		return nil, fmt.Errorf("%w: unknown role %q", ErrBudget, role)
	}
	if err := budget.Validate(); err != nil {
		return nil, fmt.Errorf("role %q: %w", role, err)
	}
	out := s.Clone()
	out[role] = budget
	return out, nil
}

// BudgetEnv is the environment variable that overrides the defaults.
const BudgetEnv = "AO_CONTEXT_ROUTER_BUDGETS"

// ParseBudgetOverrides applies a comma-separated override spec on top of the
// defaults and returns the resulting table. The spec's form is
//
//	role=compact/expanded/cap[,role=compact/expanded/cap...]
//
// for example "planner=8000/20000/26000,verify=400/900/1200". An empty spec
// yields the defaults unchanged. A malformed entry, an unknown role, or an
// incoherent budget is an error rather than a skipped entry: an operator who
// mistyped a budget must find out at startup, not from a payload that quietly
// kept the default size.
func ParseBudgetOverrides(spec string) (BudgetSet, error) {
	budgets := DefaultBudgets()
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return budgets, nil
	}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, limits, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("%w: %q is not role=compact/expanded/cap", ErrBudget, entry)
		}
		role := Role(strings.ToLower(strings.TrimSpace(name)))
		if !role.Valid() {
			return nil, fmt.Errorf("%w: unknown role %q in %q", ErrBudget, name, entry)
		}
		parts := strings.Split(strings.TrimSpace(limits), "/")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%w: %q needs three limits (compact/expanded/cap)", ErrBudget, entry)
		}
		values := make([]int, 0, 3)
		for _, raw := range parts {
			value, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return nil, fmt.Errorf("%w: %q in %q is not a token count", ErrBudget, raw, entry)
			}
			values = append(values, value)
		}
		next, err := budgets.With(role, Budget{CompactTokens: values[0], ExpandedTokens: values[1], HardCapTokens: values[2]})
		if err != nil {
			return nil, err
		}
		budgets = next
	}
	return budgets, nil
}

// BudgetsFromEnv reads the override spec from BudgetEnv and applies it to the
// defaults.
func BudgetsFromEnv() (BudgetSet, error) {
	return ParseBudgetOverrides(os.Getenv(BudgetEnv))
}

// Describe renders the table in role order, for startup logs and for the
// operator who wants to see which budgets are actually in force.
func (s BudgetSet) Describe() string {
	roles := make([]Role, 0, len(s))
	for role := range s {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		budget := s[role]
		parts = append(parts, fmt.Sprintf("%s=%d/%d/%d", role, budget.CompactTokens, budget.ExpandedTokens, budget.HardCapTokens))
	}
	return strings.Join(parts, " ")
}
