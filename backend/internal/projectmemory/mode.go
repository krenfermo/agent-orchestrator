package projectmemory

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// mode.go — P2-B's rollout switch and its configuration.
//
// P2-A shipped project memory as a capability nothing reached for. P2-B makes
// it part of the normal execution cycle, and that is exactly the kind of change
// that must be rolled out in stages rather than switched on: what an agent is
// handed is the most consequential thing AO can change about a dispatch.
//
// Hence three modes, not a boolean. The middle one is the point: memory can be
// ADDED to a dispatch long before AO is willing to let it REPLACE anything, and
// those two claims need different amounts of evidence. Adding a bounded pack
// costs bytes and can be measured; replacing a legacy document asserts that the
// memory is equivalent and current, which has to be proved per document, not
// assumed per rollout.
//
// The default is off. `AO_MEMORY_MODE=assisted` is the intended first
// production step; `preferred` is not enabled by default and must not be until
// the before/after in docs/project-memory-optimization.md is reproduced on the
// project in question.

// MemoryMode is how much authority project memory has over a dispatch's
// assembled context.
type MemoryMode string

// Memory modes, in increasing order of authority.
const (
	// ModeOff is the pre-P2-B behaviour, byte for byte. No sync is triggered,
	// no pack is built, and no dispatch payload is touched.
	ModeOff MemoryMode = "off"
	// ModeAssisted adds a bounded memory pack to a dispatch and leaves every
	// legacy context source exactly as it was. It can only ever grow a
	// payload, so its risk is cost rather than correctness — which is what
	// makes it the safe first step.
	ModeAssisted MemoryMode = "assisted"
	// ModePreferred additionally lets memory REPLACE a legacy source, but only
	// where equivalence and freshness can be demonstrated for that specific
	// source (see Deduper). Anything that cannot be proved equivalent is sent
	// as it was. There is no mode in which memory replaces something on the
	// strength of the mode alone.
	ModePreferred MemoryMode = "preferred"
)

// Valid reports whether the mode is one this build implements.
func (m MemoryMode) Valid() bool {
	switch m {
	case ModeOff, ModeAssisted, ModePreferred:
		return true
	default:
		return false
	}
}

// Enabled reports whether memory participates in dispatch at all.
func (m MemoryMode) Enabled() bool { return m == ModeAssisted || m == ModePreferred }

// MayReplace reports whether this mode permits memory to stand in for a legacy
// context source. It is a necessary condition, never a sufficient one: the
// deduper still has to prove the specific source is covered and current.
func (m MemoryMode) MayReplace() bool { return m == ModePreferred }

// Environment variables. They are read once, at composition, and the resolved
// Config travels explicitly from there — so a test can construct a Config
// directly and nothing reads the process environment at dispatch time.
const (
	// ModeEnv selects the rollout stage.
	ModeEnv = "AO_MEMORY_MODE"
	// SyncTimeoutEnv bounds how long a lifecycle-triggered sync may take
	// before the caller gives up and proceeds on legacy context.
	SyncTimeoutEnv = "AO_MEMORY_SYNC_TIMEOUT"
	// CacheEnv switches the pack cache off. It is on by default; the switch
	// exists so an operator diagnosing a suspected staleness can remove the
	// cache from the picture without disabling memory.
	CacheEnv = "AO_MEMORY_CACHE"
	// MaxFilesEnv overrides the initial-index file bound.
	MaxFilesEnv = "AO_MEMORY_MAX_FILES"
	// MaxFileBytesEnv overrides the per-file bound.
	MaxFileBytesEnv = "AO_MEMORY_MAX_FILE_BYTES"
	// BudgetEnv overrides the per-role pack budgets, as
	// `role=bytes/items[,role=bytes/items]`.
	BudgetEnv = "AO_MEMORY_BUDGETS"
)

// DefaultSyncTimeout bounds a lifecycle-triggered sync.
//
// It is deliberately short. A sync on the dispatch path is an optimisation; if
// it cannot finish quickly the correct answer is to dispatch on legacy context
// and let a later boundary catch up, never to hold a worker launch behind an
// index. The timeout is what makes "memory never blocks AO" a bound rather
// than an intention.
const DefaultSyncTimeout = 20 * time.Second

// Config is the resolved P2-B policy for one daemon.
type Config struct {
	// Mode is the rollout stage.
	Mode MemoryMode
	// SyncTimeout bounds a lifecycle-triggered sync.
	SyncTimeout time.Duration
	// CacheEnabled switches the pack cache on.
	CacheEnabled bool
	// IndexLimits bounds the initial index.
	IndexLimits IndexLimits
	// Budgets are the per-role pack budgets.
	Budgets BudgetSet
}

// DefaultConfig is conservative on purpose: memory off, short sync timeout,
// cache on, documented index bounds and role budgets.
func DefaultConfig() Config {
	return Config{
		Mode:         ModeOff,
		SyncTimeout:  DefaultSyncTimeout,
		CacheEnabled: true,
		IndexLimits:  DefaultIndexLimits(),
		Budgets:      DefaultBudgets(),
	}
}

// ConfigFromEnv resolves the policy from the environment.
//
// A malformed value is an ERROR rather than a fallback to the default. The
// failure mode of "the setting I wrote did not take effect" is a puzzled
// operator; the failure mode of silently ignoring it is a dispatch sized by a
// policy nobody chose — the same reasoning the context router's budget parser
// already applies.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()

	if raw, ok := lookupNonEmpty(ModeEnv); ok {
		mode := MemoryMode(strings.ToLower(strings.TrimSpace(raw)))
		if !mode.Valid() {
			return Config{}, fmt.Errorf("%s: unknown memory mode %q (want off, assisted or preferred)", ModeEnv, raw)
		}
		cfg.Mode = mode
	}
	if raw, ok := lookupNonEmpty(SyncTimeoutEnv); ok {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", SyncTimeoutEnv, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("%s: timeout must be positive, got %s", SyncTimeoutEnv, d)
		}
		cfg.SyncTimeout = d
	}
	if raw, ok := lookupNonEmpty(CacheEnv); ok {
		on, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", CacheEnv, err)
		}
		cfg.CacheEnabled = on
	}
	if raw, ok := lookupNonEmpty(MaxFilesEnv); ok {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("%s: want a positive integer, got %q", MaxFilesEnv, raw)
		}
		cfg.IndexLimits.MaxFiles = n
	}
	if raw, ok := lookupNonEmpty(MaxFileBytesEnv); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("%s: want a positive integer, got %q", MaxFileBytesEnv, raw)
		}
		cfg.IndexLimits.MaxFileBytes = n
	}
	if raw, ok := lookupNonEmpty(BudgetEnv); ok {
		budgets, err := ParseBudgets(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", BudgetEnv, err)
		}
		cfg.Budgets = cfg.Budgets.Merged(budgets)
	}

	cfg.IndexLimits = cfg.IndexLimits.Normalized()
	return cfg, nil
}

func lookupNonEmpty(key string) (string, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", false
	}
	return raw, true
}

// Describe renders the policy for an operator, so a log line or `ao memory
// status` can say what is actually in force rather than what the defaults are.
func (c Config) Describe() string {
	return fmt.Sprintf("mode=%s syncTimeout=%s cache=%t maxFiles=%d budgets=%s",
		c.Mode, c.SyncTimeout, c.CacheEnabled, c.IndexLimits.MaxFiles, c.Budgets.Describe())
}
