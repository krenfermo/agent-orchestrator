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
}

// DefaultWorkflowPolicy is the fixed v1 policy every Checkpoint 8D run is
// seeded with. A later checkpoint may make this configurable per-project or
// per-run; nothing in this checkpoint does.
func DefaultWorkflowPolicy() WorkflowPolicy {
	return WorkflowPolicy{Version: "v1", MaxFixCycles: 3}
}
