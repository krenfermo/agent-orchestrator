package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
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
//
// autonomousOverride is Checkpoint 8P-D.1's explicit per-run Manual/
// Autonomous choice from the create-workflow UI: nil inherits the caller's
// stored/default UserExecutionPolicy unchanged (pre-8P-D.1 behavior);
// non-nil overrides AutonomousMode in THIS run's frozen snapshot only --
// the caller's stored UserExecutionPolicy is never mutated by a per-run
// choice, so a later run with no override still gets the caller's real
// default back.
func (c *Coordinator) ApplyExecutionPolicySnapshot(ctx stdctx.Context, runID string, userID domain.UserID, autonomousOverride *bool) error {
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
	execution := c.executionPolicyForSnapshot(ctx, userID)
	if autonomousOverride != nil {
		execution.AutonomousMode = *autonomousOverride
	}
	// CP3: record where these values came from, in the same write that
	// installs them. A run whose provenance says "frozen" can never be
	// mistaken later for one whose freeze was lost.
	execution.Provenance = domain.ExecutionPolicyProvenance{
		Source:              domain.ExecutionPolicyFrozen,
		OwnerID:             userID,
		AutonomousRequested: autonomousOverride,
		At:                  c.clock(),
	}
	policy.Execution = execution
	snapshotJSON, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if _, err := c.store.UpdateWorkflowRunPolicySnapshot(ctx, runID, string(snapshotJSON), c.clock()); err != nil {
		return err
	}
	c.maybeKickoffAutonomousPlanning(ctx, run, policy.Execution)
	return nil
}

// maybeKickoffAutonomousPlanning is Checkpoint 8P-D's auto-planner-start:
// called once, synchronously, right after a master/objective run's frozen
// execution policy snapshot is written (still inside the original HTTP
// create request, but this only persists a durable wake row -- it never
// invokes the planner itself, so the request never waits on Planner/agent
// work). If the run is a master-plan run whose plan has not been generated
// yet (Status == Pending) and the just-applied snapshot says
// AutonomousMode == true, it schedules the same ReasonAutonomousProgress
// wake reconcileMasterTasks re-schedules later -- the daemon poller then
// calls ContinueRun, which (see workflow.go's ContinueRun) resolves a
// Pending-plan master run straight into GeneratePlan. Best-effort and
// idempotent (Schedule upserts by idempotency key): a nil wakeScheduler, a
// non-master run, a manual-mode run, or an already-generated plan are all
// silent no-ops.
func (c *Coordinator) maybeKickoffAutonomousPlanning(ctx stdctx.Context, run domain.WorkflowRun, execution domain.ExecutionPolicySnapshot) {
	if !execution.AutonomousMode || c.wakeScheduler == nil {
		return
	}
	if c.planStore != nil {
		plan, isMaster, err := c.planStore.GetWorkflowPlan(ctx, run.ID)
		if err != nil {
			return
		}
		if isMaster {
			if plan.Status == domain.WorkflowPlanPending {
				c.scheduleWake(ctx, run, nil, wake.ReasonAutonomousProgress, "")
			}
			return
		}
	}
	// P1-A: the same kickoff for a `task` run, which has no plan to generate
	// and would otherwise sit at `pending` until a person pressed Start --
	// leaving the new strategy usable only by hand. The wake poller calls
	// ContinueRun, which starts a pending autonomous task run (see
	// ContinueRun's own task branch).
	//
	// Deliberately gated on a RECORDED task selection rather than a resolved
	// one: a legacy run maps to `task` too, and a pre-P1-A single-task run
	// must keep waiting for its person exactly as it always has.
	if run.ParentWorkflowID != nil || run.State != domain.WorkflowRunPending {
		return
	}
	if sel, ok := recordedStrategy(run); !ok || sel.Effective != domain.ExecutionStrategyTask {
		return
	}
	c.scheduleWake(ctx, run, nil, wake.ReasonAutonomousProgress, "")
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
	// CP19: the old guard returned early whenever the parent's snapshot had
	// no routing priorities, which meant AutonomousMode was NOT inherited for
	// exactly the parents most likely to be autonomous-but-profile-less. The
	// whole frozen execution policy is copied now; routingInputsForRole
	// already handles a priority-less snapshot by resolving fresh, so copying
	// one changes no routing decision, and it stops the child from silently
	// disagreeing with its parent about autonomy.
	//
	// Nothing is copied from a parent that predates provenance AND carries no
	// priorities: that is a pure legacy default, and rewriting the child's
	// snapshot from it would change no value while inventing a provenance
	// record for history that has none.
	if parentPolicy.Execution.Provenance.Source == "" && !snapshotHasPriorities(parentPolicy.Execution) {
		return nil
	}
	child, ok, err := c.store.GetWorkflowRun(ctx, childRunID)
	if err != nil || !ok {
		return err
	}
	childPolicy := policyForRun(child)
	childPolicy.Execution = parentPolicy.Execution
	childPolicy.Execution.Provenance = domain.ExecutionPolicyProvenance{
		Source:              domain.ExecutionPolicyInherited,
		OwnerID:             parentPolicy.Execution.Provenance.OwnerID,
		ParentRunID:         parent.ID,
		AutonomousRequested: parentPolicy.Execution.Provenance.AutonomousRequested,
		At:                  c.clock(),
	}
	snapshotJSON, err := json.Marshal(childPolicy)
	if err != nil {
		return err
	}
	_, err = c.store.UpdateWorkflowRunPolicySnapshot(ctx, childRunID, string(snapshotJSON), c.clock())
	return err
}

// requireInheritedExecutionPolicy is CP19's fail-closed gate: a master task's
// child must never dispatch a provider process while it cannot PROVE it is
// running under its parent objective's frozen execution policy.
//
// It is deliberately scoped by what the parent itself can prove. A parent
// whose own snapshot predates provenance has nothing to inherit and nothing
// to check, so this is a no-op for every pre-existing run -- the compatibility
// stance stampChildOwnership/requireChildOwnershipForDispatch already take.
// When the parent IS proven, the child must carry an "inherited" record
// naming that parent, and must agree with it on autonomy. Anything else means
// the inheritance write did not land, and the child would otherwise run under
// a substituted default while every durable row looked healthy.
func (c *Coordinator) requireInheritedExecutionPolicy(ctx stdctx.Context, childRunID string, parent domain.WorkflowRun) error {
	parentPolicy := policyForRun(parent)
	if !parentPolicy.Execution.Provenance.Proven() {
		return nil
	}
	child, ok, err := c.store.GetWorkflowRun(ctx, childRunID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: child run %s", ErrNotFound, childRunID)
	}
	childPolicy := policyForRun(child)
	prov := childPolicy.Execution.Provenance
	if prov.Source != domain.ExecutionPolicyInherited || prov.ParentRunID != parent.ID {
		return fmt.Errorf("%w: child run %s cannot prove it inherited parent %s's frozen execution policy (source=%q parent=%q)",
			ErrInvalid, childRunID, parent.ID, prov.Source, prov.ParentRunID)
	}
	if childPolicy.Execution.AutonomousMode != parentPolicy.Execution.AutonomousMode {
		return fmt.Errorf("%w: child run %s autonomy (%t) disagrees with parent %s (%t)",
			ErrInvalid, childRunID, childPolicy.Execution.AutonomousMode, parent.ID, parentPolicy.Execution.AutonomousMode)
	}
	return nil
}

// unfrozenExecutionPolicy stamps the "freeze still owed" marker onto a policy
// at run-creation time. See domain.ExecutionPolicyUnfrozen.
func unfrozenExecutionPolicy(policy domain.WorkflowPolicy, now time.Time) domain.WorkflowPolicy {
	policy.Execution.Provenance = domain.ExecutionPolicyProvenance{Source: domain.ExecutionPolicyUnfrozen, At: now}
	return policy
}

// ensureFrozenExecutionPolicy is CP3's recovery half: heal a run whose
// creation recorded that a freeze was owed and whose freeze never landed, and
// refuse to keep driving it if the heal cannot prove a policy.
//
// Three populations, three answers:
//
//   - Provenance already proven, or a legacy snapshot (Source == ""): no-op.
//     Legacy runs are never touched and never refused.
//   - Unfrozen with no resolved owner: also a no-op. There is no identity to
//     freeze against, so the default policy is the honest answer and this is
//     exactly pre-existing behaviour.
//   - Unfrozen and owned: the crash window. Re-freeze from the owner's stored
//     policy and record it as "recovered", never as the original create-time
//     freeze -- the create request's own per-run autonomy choice was never
//     durable and is not invented here. If the re-freeze still cannot produce
//     a proven snapshot, this returns an error and the caller parks the run,
//     rather than letting it run on a policy nobody chose.
//
// A child run is skipped entirely: its policy comes from its parent, not from
// its owner's live settings, and dispatchMasterTask's recovery branch is what
// re-runs that inheritance.
func (c *Coordinator) ensureFrozenExecutionPolicy(ctx stdctx.Context, run domain.WorkflowRun) (domain.WorkflowRun, error) {
	if run.ParentWorkflowID != nil {
		return run, nil
	}
	if !policyForRun(run).Execution.Provenance.Unproven() {
		return run, nil
	}
	owner := c.runOwner(ctx, run.ID)
	if owner == "" {
		return run, nil
	}
	if err := c.applyRecoveredExecutionPolicySnapshot(ctx, run, owner); err != nil {
		return run, err
	}
	refreshed, ok, err := c.store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		return run, err
	}
	if !ok {
		return run, nil
	}
	if !policyForRun(refreshed).Execution.Provenance.Proven() {
		return refreshed, fmt.Errorf("%w: run %s is owned by %s but its execution policy was never frozen and cannot be re-proven",
			ErrInvalid, run.ID, owner)
	}
	return refreshed, nil
}

// applyRecoveredExecutionPolicySnapshot writes the recovery freeze. It is a
// sibling of ApplyExecutionPolicySnapshot rather than a call into it, because
// the two must not claim the same provenance: this one re-derives from the
// owner's stored policy after the fact and says so.
func (c *Coordinator) applyRecoveredExecutionPolicySnapshot(ctx stdctx.Context, run domain.WorkflowRun, owner domain.UserID) error {
	policy := policyForRun(run)
	execution := c.executionPolicyForSnapshot(ctx, owner)
	execution.Provenance = domain.ExecutionPolicyProvenance{
		Source:  domain.ExecutionPolicyRecovered,
		OwnerID: owner,
		At:      c.clock(),
	}
	policy.Execution = execution
	snapshotJSON, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if _, err := c.store.UpdateWorkflowRunPolicySnapshot(ctx, run.ID, string(snapshotJSON), c.clock()); err != nil {
		return err
	}
	c.maybeKickoffAutonomousPlanning(ctx, run, policy.Execution)
	return nil
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
