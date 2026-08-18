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
	// MaxAutoAnsweredQuestionsPerStep bounds how many captured questions on
	// a single step may be auto-answered by the policy resolver
	// (Checkpoint 8K-A) before AO stops trusting the auto path for that step
	// and forces state=human_required regardless of what the classifier
	// said. 8K-A has no second-LLM resolver loop yet, so this budget cannot
	// be exhausted by a resolver retry storm today — but a worker can still
	// pathologically re-ask the same policy-resolvable question after every
	// restart/checkpoint, so the loop-safety net exists from the start
	// rather than being added reactively in a later checkpoint.
	MaxAutoAnsweredQuestionsPerStep int `json:"maxAutoAnsweredQuestionsPerStep"`
	// AllowSameProviderResolver controls whether the Checkpoint 8K-B
	// Decision Resolver may fall back to spawning a resolver session from
	// the *same* provider that asked the question, when the preferred
	// (opposite) provider's harness is unavailable. Default false: a
	// same-provider resolver answering its own asking provider's question
	// carries a self-review/self-answer ambiguity risk distinct from
	// MaxAutoAnsweredQuestionsPerStep's loop-safety concern — the resolver
	// may share the same blind spots or mistaken assumptions that produced
	// the question in the first place, so this fallback is opt-in rather
	// than the default "always resolve somehow" behavior. Read by pass 2's
	// provider-selection logic; unused by any pass-1 code path.
	AllowSameProviderResolver bool `json:"allowSameProviderResolver"`
	// Routing is Checkpoint 8L's ExecutionRouter policy: per-role/complexity
	// harness preference and cross-provider review independence. Embedded in
	// WorkflowPolicy (rather than a second top-level snapshot) so every run's
	// single policy_snapshot column continues to be the one place a run's
	// full decision-making configuration is persisted and versioned
	// together. A policy snapshot decoded from before 8L has this at its
	// zero value; callers must use EffectiveRoutingPolicy, never read
	// Routing directly.
	Routing RoutingPolicy `json:"routing,omitempty"`
	// Wake is Checkpoint 8N's durable wake-up scheduler policy: how a
	// waiting run/step is rescheduled to be retried automatically. Embedded
	// the same way Routing is; a policy snapshot decoded from before 8N has
	// this at its zero value — callers must use EffectiveWakePolicy, never
	// read Wake directly.
	Wake WakePolicy `json:"wake,omitempty"`
	// Execution is Checkpoint 8P-C's UserExecutionPolicy snapshot: the
	// workflow owner's priority lists (as of run creation), fallback
	// behavior, review independence and autonomy flag, embedded the same
	// way Routing/Wake are so a later Settings edit never changes an
	// in-flight run's routing (checkpoint brief §9/§10). Only stable
	// ProviderProfileID references are stored -- never credentials. Current
	// profile eligibility (enabled/connected/capability) is always re-checked
	// live at dispatch time regardless of what the snapshot captured -- a
	// disabled/deleted profile referenced here is simply skipped, never
	// force-used (checkpoint brief §10).
	Execution ExecutionPolicySnapshot `json:"execution,omitempty"`
}

// ExecutionPolicySnapshot is the run-creation-time copy of a
// UserExecutionPolicy embedded into WorkflowPolicy (Checkpoint 8P-C). A
// distinct type from UserExecutionPolicy (rather than reusing it directly)
// because a snapshot has no ID/UserID/CreatedAt/UpdatedAt of its own -- it is
// a frozen value, not a referenceable row.
type ExecutionPolicySnapshot struct {
	Version                   string               `json:"version"`
	AutonomousMode            bool                 `json:"autonomousMode"`
	PlannerPriority           []ProviderProfileID  `json:"plannerPriority"`
	WorkerPriority            []ProviderProfileID  `json:"workerPriority"`
	ReviewerPriority          []ProviderProfileID  `json:"reviewerPriority"`
	DecisionResolverPriority  []ProviderProfileID  `json:"decisionResolverPriority"`
	FallbackBehavior          FallbackBehavior     `json:"fallbackBehavior"`
	ReviewIndependence        ReviewIndependence   `json:"reviewIndependence"`
}

// ExecutionPolicySnapshotFrom captures a point-in-time copy of a
// UserExecutionPolicy for embedding into a workflow run's policy snapshot.
func ExecutionPolicySnapshotFrom(p UserExecutionPolicy) ExecutionPolicySnapshot {
	return ExecutionPolicySnapshot{
		Version:                  p.Version,
		AutonomousMode:           p.AutonomousMode,
		PlannerPriority:          p.PlannerPriority,
		WorkerPriority:           p.WorkerPriority,
		ReviewerPriority:         p.ReviewerPriority,
		DecisionResolverPriority: p.DecisionResolverPriority,
		FallbackBehavior:         p.FallbackBehavior,
		ReviewIndependence:       p.ReviewIndependence,
	}
}

// PriorityFor mirrors UserExecutionPolicy.PriorityFor for the frozen
// snapshot shape, so routing code can treat both the same way.
func (s ExecutionPolicySnapshot) PriorityFor(role WorkflowRole) []ProviderProfileID {
	switch role {
	case WorkflowRolePlanner:
		return s.PlannerPriority
	case WorkflowRoleWorker, WorkflowRoleFixWorker:
		return s.WorkerPriority
	case WorkflowRoleReviewer:
		return s.ReviewerPriority
	case WorkflowRoleDecisionResolver:
		return s.DecisionResolverPriority
	default:
		return nil
	}
}

// EffectiveRoutingPolicy returns p.Routing, falling back to
// DefaultRoutingPolicy() when the snapshot predates Checkpoint 8L (zero
// Version). Mirrors effectiveMaxWorkProviderAttempts's own
// forward-compatible zero-value fallback pattern.
func (p WorkflowPolicy) EffectiveRoutingPolicy() RoutingPolicy {
	if p.Routing.Version != "" {
		return p.Routing
	}
	return DefaultRoutingPolicy()
}

// EffectiveExecutionPolicy returns p.Execution, or a policy with empty
// priority lists (sane fallback/independence defaults, no profiles) when
// the snapshot predates Checkpoint 8P-C (zero Version) -- the caller
// (workflow.routingInputsForRole) recognizes an empty priority list as "no
// frozen preference recorded" and falls back to
// domain.DefaultUserExecutionPolicy built fresh from the owner's live
// profiles, exactly the same forward-compatible pattern
// EffectiveRoutingPolicy/EffectiveWakePolicy already use.
func (p WorkflowPolicy) EffectiveExecutionPolicy() ExecutionPolicySnapshot {
	if p.Execution.Version != "" {
		return p.Execution
	}
	return ExecutionPolicySnapshot{
		Version:            UserExecutionPolicyVersion,
		FallbackBehavior:   FallbackUseNextAvailable,
		ReviewIndependence: ReviewIndependenceRequireDifferentProvider,
	}
}

// EffectiveWakePolicy returns p.Wake, falling back to DefaultWakePolicy()
// when the snapshot predates Checkpoint 8N (zero Version). Mirrors
// EffectiveRoutingPolicy's own forward-compatible zero-value fallback
// pattern exactly.
func (p WorkflowPolicy) EffectiveWakePolicy() WakePolicy {
	if p.Wake.Version != "" {
		return p.Wake
	}
	return DefaultWakePolicy()
}

// DefaultWorkflowPolicy is the fixed v1 policy every Checkpoint 8D run is
// seeded with. A later checkpoint may make this configurable per-project or
// per-run; nothing in this checkpoint does.
func DefaultWorkflowPolicy() WorkflowPolicy {
	return WorkflowPolicy{
		Version:                         "v1",
		MaxFixCycles:                    3,
		MaxWorkProviderAttempts:         3,
		MaxReviewProviderAttempts:       3,
		MaxAutoAnsweredQuestionsPerStep: 5,
		Routing:                         DefaultRoutingPolicy(),
		Wake:                            DefaultWakePolicy(),
	}
}
