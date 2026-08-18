package workflow

import (
	stdctx "context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// runOwner resolves a workflow run's owner, or "" for an unowned/pre-8P-A
// run -- the single lookup every routing entry point uses before it knows
// which harness it will even pick (see workflow.Store.GetWorkflowRunOwner's
// doc comment).
func (c *Coordinator) runOwner(ctx stdctx.Context, runID string) domain.UserID {
	owner, err := c.store.GetWorkflowRunOwner(ctx, runID)
	if err != nil || owner == nil {
		return ""
	}
	return *owner
}

// resolvedProfiles returns userID's live owned profiles.
//
// c.providerProfiles == nil is this package's standard "dependency never
// wired" signal (the same convention RuntimeIsolation/WakeScheduler/etc. all
// use), and c.trustedLocal mirrors config.Config.TrustedLocalMode -- either
// one means a bootstrap admin/pre-8P-B fixture with zero configured
// ProviderProfile rows keeps working exactly like a pre-8P-C install,
// synthesized from the real registry adapters (Claude Code, Codex) rather
// than waiting forever. Multi-user mode (trustedLocal=false, providerProfiles
// wired) never applies this: a real user with zero owned profiles correctly
// gets none back, and therefore waits (checkpoint brief §18: "no profile =
// never selected"). Always live -- never frozen -- so a disabled/deleted
// profile stops being eligible immediately, regardless of what any run's
// policy snapshot once referenced (checkpoint brief §10).
func (c *Coordinator) resolvedProfiles(ctx stdctx.Context, userID domain.UserID) []domain.ProviderProfile {
	var profiles []domain.ProviderProfile
	if c.providerProfiles != nil && userID != "" {
		profiles, _ = c.providerProfiles.ListProviderProfilesByUser(ctx, userID)
	}
	if len(profiles) == 0 && (c.providerProfiles == nil || c.trustedLocal) {
		profiles = legacyCompatibilityProfiles()
	}
	return profiles
}

// legacyCompatibilityProfiles synthesizes one profile per real (Available)
// registry adapter, standing in for "the daemon's own installed/
// authenticated CLI" the way every pre-8P-B install implicitly worked. IDs
// are stable-but-synthetic ("legacy-<harness>") and never persisted --
// capacity for them resolves through the legacy/global agent_health_events
// read (healthScope{} is unscoped whenever userID is empty), exactly
// reproducing pre-8P-C health/capacity behavior. In trusted-local mode with
// a real (non-empty) owner, capacity is still scoped to that owner, same as
// any other profile.
func legacyCompatibilityProfiles() []domain.ProviderProfile {
	descriptors := registry.ProviderDescriptors()
	profiles := make([]domain.ProviderProfile, 0, len(descriptors))
	for _, d := range descriptors {
		if !d.Available {
			continue
		}
		var authMethod domain.ProviderAuthMethod
		if len(d.AuthMethods) > 0 {
			authMethod = d.AuthMethods[0]
		}
		profiles = append(profiles, domain.ProviderProfile{
			ID:           domain.ProviderProfileID("legacy-" + string(d.Harness)),
			Provider:     d.Provider,
			Harness:      d.Harness,
			DisplayName:  d.DisplayName,
			Enabled:      true,
			AuthState:    domain.ProviderAuthStateAuthenticated,
			AuthMethod:   authMethod,
			Capabilities: d.Capabilities,
		})
	}
	return profiles
}

// executionPolicyForSnapshot builds the domain.ExecutionPolicySnapshot to
// embed into a new workflow run's policy_snapshot at creation time
// (Checkpoint 8P-C §9): the owner's live UserExecutionPolicy if one is
// stored, otherwise a bootstrap default built from their current profiles
// (documented compatibility builder, checkpoint brief §8). Captured ONCE,
// here -- routing later reads this frozen copy back via
// WorkflowPolicy.EffectiveExecutionPolicy, never re-fetching live policy for
// an already-created run, so a later Settings edit cannot reroute an
// in-flight workflow (checkpoint brief §10).
func (c *Coordinator) executionPolicyForSnapshot(ctx stdctx.Context, userID domain.UserID) domain.ExecutionPolicySnapshot {
	profiles := c.resolvedProfiles(ctx, userID)
	if c.executionPolicies != nil && userID != "" {
		if p, ok, err := c.executionPolicies.GetUserExecutionPolicyByUser(ctx, userID); err == nil && ok {
			return domain.ExecutionPolicySnapshotFrom(p)
		}
	}
	return domain.ExecutionPolicySnapshotFrom(domain.DefaultUserExecutionPolicy(userID, profiles))
}

// ApplyExecutionPolicySnapshot embeds userID's execution policy into an
// already-created run's policy_snapshot (Checkpoint 8P-C §9). Callers
// (httpd/controllers/workflow.go's create handler) call this exactly once,
// immediately after CreateRun/CreateObjectiveRun and after resolving the
// caller's identity via c.stampOwner's sibling ownership stamp -- the same
// "create first, then stamp owner-derived facts as a second step" pattern
// Checkpoint 8P-A's stampOwner already established, so CreateRun's own
// signature (used by ~50 existing call sites/tests) never needs a userID
// parameter. A no-op (returns nil) if the run doesn't exist or userID is
// empty (unowned/trusted-local-without-identity creation), leaving the
// run's default policy_snapshot exactly as CreateRun already wrote it.
func (c *Coordinator) ApplyExecutionPolicySnapshot(ctx stdctx.Context, runID string, userID domain.UserID) error {
	if userID == "" {
		return nil
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	policy := policyForRun(run)
	policy.Execution = c.executionPolicyForSnapshot(ctx, userID)
	snapshotJSON, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = c.store.UpdateWorkflowRunPolicySnapshot(ctx, runID, string(snapshotJSON), c.clock())
	return err
}

// inheritExecutionPolicySnapshot copies parent's already-frozen execution
// policy verbatim onto a just-created child (master-task) run's own policy
// snapshot -- never re-derived from live policy/profiles, so every task in
// one master objective routes under the exact same rules the objective was
// created with (checkpoint brief §9's "no recalcules historia", applied the
// same way Routing/Wake already are for a single run). See
// routingInputsForRole's snapshotReferencesAny guard for what happens if the
// child has no ownership of its own to resolve these profile IDs against.
func (c *Coordinator) inheritExecutionPolicySnapshot(ctx stdctx.Context, childRunID string, parent domain.WorkflowRun) error {
	parentPolicy := policyForRun(parent)
	if !snapshotHasPriorities(parentPolicy.Execution) {
		return nil
	}
	child, ok, err := c.store.GetWorkflowRun(ctx, childRunID)
	if err != nil || !ok {
		return err
	}
	childPolicy := policyForRun(child)
	childPolicy.Execution = parentPolicy.Execution
	snapshotJSON, err := json.Marshal(childPolicy)
	if err != nil {
		return err
	}
	_, err = c.store.UpdateWorkflowRunPolicySnapshot(ctx, childRunID, string(snapshotJSON), c.clock())
	return err
}

// snapshotHasPriorities reports whether snapshot carries at least one
// recorded priority entry for any role -- an all-empty snapshot means either
// a pre-8P-C run (predates Execution entirely) or a genuinely empty policy;
// routingInputsForRole treats both the same way: rebuild a fresh bootstrap
// default from live profiles rather than routing against a snapshot that
// can never select anything.
func snapshotHasPriorities(s domain.ExecutionPolicySnapshot) bool {
	return len(s.PlannerPriority) > 0 || len(s.WorkerPriority) > 0 || len(s.ReviewerPriority) > 0 || len(s.DecisionResolverPriority) > 0
}

// snapshotReferencesAny reports whether at least one profile ID anywhere in
// snapshot's priority lists matches a currently-resolved profile -- guards
// against a snapshot whose IDs are entirely foreign to the current
// resolution (see routingInputsForRole's doc comment).
func snapshotReferencesAny(s domain.ExecutionPolicySnapshot, profiles []domain.ProviderProfile) bool {
	ids := make(map[domain.ProviderProfileID]struct{}, len(profiles))
	for _, p := range profiles {
		ids[p.ID] = struct{}{}
	}
	for _, list := range [][]domain.ProviderProfileID{s.PlannerPriority, s.WorkerPriority, s.ReviewerPriority, s.DecisionResolverPriority} {
		for _, id := range list {
			if _, ok := ids[id]; ok {
				return true
			}
		}
	}
	return false
}

// routingInputsForRole resolves everything RouteExecution needs for one
// role/run pair (Checkpoint 8P-C): the run's frozen execution policy
// snapshot (falling back to a fresh bootstrap default when the snapshot
// predates 8P-C or was never populated), the owner's CURRENT
// eligible-for-this-role profiles (owned, enabled, connected, capable --
// domain.EligibleProfiles), and a capacity snapshot scoped to exactly those
// profiles. Every dispatch site (worker, reviewer, planner, decision
// resolver) calls this instead of re-deriving eligibility/capacity/policy
// independently.
func (c *Coordinator) routingInputsForRole(ctx stdctx.Context, userID domain.UserID, role domain.WorkflowRole, snapshot domain.ExecutionPolicySnapshot) (domain.ExecutionPolicySnapshot, map[domain.ProviderProfileID]domain.ProviderProfile, map[domain.ProviderProfileID]domain.RoutingReason, map[domain.ProviderProfileID]domain.CapacityState) {
	profiles := c.resolvedProfiles(ctx, userID)
	if !snapshotHasPriorities(snapshot) || !snapshotReferencesAny(snapshot, profiles) {
		// Either a pre-8P-C/empty snapshot, or one whose referenced profile
		// IDs no longer resolve against this owner at all (e.g. a master
		// task's child run that inherited its parent's snapshot verbatim
		// but has no ownership of its own yet to re-derive live profiles
		// from, or a run created directly through the store bypassing the
		// normal create-then-ApplyExecutionPolicySnapshot flow) -- resolve
		// fresh the same way run creation itself would (live policy if the
		// owner has stored one, else the bootstrap default), rather than
		// routing against IDs that can never match and would otherwise wait
		// forever.
		snapshot = c.executionPolicyForSnapshot(ctx, userID)
	}
	capability, ok := domain.RequiredCapability(role)
	if !ok {
		return snapshot, nil, nil, nil
	}
	eligible, ineligible := domain.EligibleProfiles(profiles, registry.ProviderDescriptors(), capability)
	capacity := c.capacitySnapshotForProfiles(ctx, userID, eligible)
	return snapshot, eligible, ineligible, capacity
}
