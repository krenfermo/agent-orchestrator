package domain

// WorkflowPolicy is the centralized, versioned set of tunable knobs a
// workflow run's execution obeys (Checkpoint 8D). It is serialized into
// workflow_runs.policy_snapshot at CreateRun time and read back from there by
// every decision point that needs it — never a bare constant scattered
// elsewhere in code ("no constantes dispersas").
type WorkflowPolicy struct {
	Version string `json:"version"`
	// MaxFixCycles bounds how many review->fix cycles the automatic loop may
	// run before it stops dispatching another fix and instead surfaces
	// next_action: "human_attention" on the run.
	MaxFixCycles int `json:"maxFixCycles"`
	// MaxWorkProviderAttempts bounds how many total provider attempts
	// (Checkpoint 8H: one per harness tried, e.g. Codex then Claude) a work
	// step's dispatch may make before it stops trying and instead surfaces
	// next_action: needs_attention. Distinct from MaxFixCycles, which bounds
	// the review<->fix loop, not initial dispatch/failover.
	MaxWorkProviderAttempts int `json:"maxWorkProviderAttempts"`
	// MaxReviewProviderAttempts bounds how many total provider attempts a
	// review step's dispatch may make (Checkpoint 8H). 8H does not yet wire a
	// reviewer failover target, so this only bounds retries on the single
	// configured reviewer harness; it exists now so the budget field is not
	// scattered in a later checkpoint.
	MaxReviewProviderAttempts int `json:"maxReviewProviderAttempts"`
}

// DefaultWorkflowPolicy is the fixed v1 policy every Checkpoint 8D run is
// seeded with. A later checkpoint may make this configurable per-project or
// per-run; nothing in this checkpoint does.
func DefaultWorkflowPolicy() WorkflowPolicy {
	return WorkflowPolicy{
		Version:                   "v1",
		MaxFixCycles:              3,
		MaxWorkProviderAttempts:   3,
		MaxReviewProviderAttempts: 3,
	}
}
