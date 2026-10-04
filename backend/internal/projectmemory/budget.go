package projectmemory

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// budget.go — P2-B's per-role context budgets (§7).
//
// A budget here bounds the MEMORY PACK, which is a different thing from
// internal/contextrouter's token budget over a whole routed payload. Both
// exist, and neither subsumes the other: the router budgets everything a
// dispatch carries, while this budget answers "how much of that may be
// memory". Keeping them separate is what lets memory be capped independently
// of the documents and the diff it competes with — and it is why turning
// memory on cannot silently push a payload past the router's own hard cap.
//
// Four dimensions rather than one, because they fail differently. Bytes bound
// the payload. Items bound the reading: forty one-line facts is a pack nobody
// reads, whatever it weighs. Tokens are the estimate a provider actually
// charges for. Documents bound how many whole source documents a pack may
// stand in for, which is the dimension ModePreferred spends.

// ErrBudget is the sentinel every rejected budget configuration wraps.
var ErrBudget = errors.New("projectmemory: invalid budget")

// PackPolicyVersion identifies the selection and eviction policy this build
// implements. It participates in the pack cache key, so changing the policy
// invalidates every cached pack rather than serving one assembled under the
// previous rules.
const PackPolicyVersion = 1

// RoleBudget is one role's bound on the memory it may be handed.
type RoleBudget struct {
	// MaxBytes caps the rendered pack.
	MaxBytes int
	// MaxItems caps how many facts it carries.
	MaxItems int
	// MaxDocuments caps how many whole source documents this role's pack may
	// replace under ModePreferred. Zero means it may replace none, which is
	// the correct default for a role whose legacy context is small.
	MaxDocuments int
}

// EstimatedMaxTokens is MaxBytes at the shared four-bytes-per-token estimate.
// It is derived rather than configured so the two can never disagree, and it
// is named "estimated" everywhere it surfaces because that is what it is.
func (b RoleBudget) EstimatedMaxTokens() int {
	return (b.MaxBytes + packBytesPerToken - 1) / packBytesPerToken
}

// Validate rejects a budget that cannot be honoured.
func (b RoleBudget) Validate() error {
	if b.MaxBytes <= 0 {
		return fmt.Errorf("%w: maxBytes must be positive, got %d", ErrBudget, b.MaxBytes)
	}
	if b.MaxItems <= 0 {
		return fmt.Errorf("%w: maxItems must be positive, got %d", ErrBudget, b.MaxItems)
	}
	if b.MaxDocuments < 0 {
		return fmt.Errorf("%w: maxDocuments cannot be negative, got %d", ErrBudget, b.MaxDocuments)
	}
	return nil
}

// packBudget projects a role budget onto the pack builder's own bound.
func (b RoleBudget) packBudget() PackBudget {
	return PackBudget{MaxBytes: b.MaxBytes, MaxItems: b.MaxItems}
}

// BudgetSet is the per-role table.
type BudgetSet map[PackRole]RoleBudget

// DefaultBudgets are the documented defaults.
//
// The shape mirrors the context router's central policy claim, applied to
// memory: a role reasoning about code it has not seen gets room to be told
// about it; a role acting on a conclusion someone else already reached gets
// what that conclusion rests on and nothing more.
//
//	role      bytes   ~tokens  items  docs   why
//	planner   24 KiB    6144      40     4   plans over a whole objective, and spans every repository
//	reviewer  16 KiB    4096      30     2   judges a change against its surroundings
//	worker    16 KiB    4096      24     2   implements one task; the changed area dominates
//	repair    12 KiB    3072      20     0   acts on specific findings, never a project tour
func DefaultBudgets() BudgetSet {
	return BudgetSet{
		RolePlanner:  {MaxBytes: 24 * 1024, MaxItems: 40, MaxDocuments: 4},
		RoleReviewer: {MaxBytes: 16 * 1024, MaxItems: 30, MaxDocuments: 2},
		RoleWorker:   {MaxBytes: 16 * 1024, MaxItems: 24, MaxDocuments: 2},
		RoleRepair:   {MaxBytes: 12 * 1024, MaxItems: 20, MaxDocuments: 0},
	}
}

// For returns a role's budget, falling back to the worker budget for a role
// the table does not name.
//
// The fallback is the worker's rather than the largest, because an unknown
// role is one whose context needs nobody has reasoned about, and the safe
// guess for such a role is a modest bound rather than a generous one.
func (s BudgetSet) For(role PackRole) RoleBudget {
	if b, ok := s[role]; ok {
		return b
	}
	if b, ok := s[RoleWorker]; ok {
		return b
	}
	return DefaultBudgets()[RoleWorker]
}

// Clone returns an independent copy.
func (s BudgetSet) Clone() BudgetSet {
	out := make(BudgetSet, len(s))
	for role, b := range s {
		out[role] = b
	}
	return out
}

// Merged returns this table with the overrides applied, leaving both operands
// untouched. An override names only the roles it changes.
func (s BudgetSet) Merged(overrides BudgetSet) BudgetSet {
	out := s.Clone()
	for role, b := range overrides {
		out[role] = b
	}
	return out
}

// Validate checks that every role this build assembles packs for is budgeted
// and every budget is honourable. A missing role is an error rather than a
// silent fallback: a role budgeted by whatever default happened to be nearby
// is exactly the invisible policy this table exists to prevent.
func (s BudgetSet) Validate() error {
	if len(s) == 0 {
		return fmt.Errorf("%w: budget table is empty", ErrBudget)
	}
	for _, role := range []PackRole{RolePlanner, RoleWorker, RoleReviewer, RoleRepair} {
		b, ok := s[role]
		if !ok {
			return fmt.Errorf("%w: role %q has no budget", ErrBudget, role)
		}
		if err := b.Validate(); err != nil {
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

// Describe renders the table in a stable order for operator output.
func (s BudgetSet) Describe() string {
	roles := make([]string, 0, len(s))
	for role := range s {
		roles = append(roles, string(role))
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		b := s[PackRole(role)]
		parts = append(parts, fmt.Sprintf("%s=%dB/%di/~%dt", role, b.MaxBytes, b.MaxItems, b.EstimatedMaxTokens()))
	}
	return strings.Join(parts, ",")
}

// ParseBudgets reads an operator override, as
// `role=bytes/items[/documents][,role=...]`.
//
// Sizes accept a `k`/`m` suffix, because an operator writing a budget thinks
// in kilobytes and a config that forces them to write 16384 is a config that
// gets mistyped.
func ParseBudgets(raw string) (BudgetSet, error) {
	out := BudgetSet{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, spec, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("%w: %q is not role=bytes/items", ErrBudget, entry)
		}
		role := PackRole(strings.ToLower(strings.TrimSpace(name)))
		if !role.Valid() {
			return nil, fmt.Errorf("%w: unknown role %q", ErrBudget, name)
		}
		fields := strings.Split(strings.TrimSpace(spec), "/")
		if len(fields) < 2 || len(fields) > 3 {
			return nil, fmt.Errorf("%w: %q is not bytes/items[/documents]", ErrBudget, spec)
		}
		bytes, err := parseSize(fields[0])
		if err != nil {
			return nil, fmt.Errorf("%w: role %q bytes: %w", ErrBudget, role, err)
		}
		items, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return nil, fmt.Errorf("%w: role %q items: %w", ErrBudget, role, err)
		}
		budget := RoleBudget{MaxBytes: bytes, MaxItems: items}
		if len(fields) == 3 {
			docs, err := strconv.Atoi(strings.TrimSpace(fields[2]))
			if err != nil {
				return nil, fmt.Errorf("%w: role %q documents: %w", ErrBudget, role, err)
			}
			budget.MaxDocuments = docs
		}
		if err := budget.Validate(); err != nil {
			return nil, err
		}
		out[role] = budget
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: override names no role", ErrBudget)
	}
	return out, nil
}

func parseSize(raw string) (int, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	mult := 1
	switch {
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "kb"):
		mult, s = 1024, strings.TrimSuffix(strings.TrimSuffix(s, "b"), "k")
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "mb"):
		mult, s = 1024*1024, strings.TrimSuffix(strings.TrimSuffix(s, "b"), "m")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("want an integer with an optional k/m suffix, got %q", raw)
	}
	return n * mult, nil
}
