package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workspace"
)

// placement_override.go — P1-E §B/§C: the per-task placement OVERRIDE request,
// and the explicit generation TRANSITION.
//
// P1-D deferred this deliberately, and recorded why: every placement route it
// added is read-only, because a write that re-points a placement is a write
// that can aim a running agent at a different checkout. The affordance is not
// unsafe in itself; it needed its own model rather than being appended to an
// observability surface. This file is that model.
//
// # The two halves, and why they are not one operation
//
//	REQUEST      what somebody wants, recorded before anything is frozen.
//	             Consumed once, by the freeze. Changes nothing on its own.
//	TRANSITION   the replacement of one frozen placement generation by another.
//	             Requires operator authority, an asserted lifecycle state, and a
//	             quiescence proof over durable facts.
//
// Collapsing them would produce exactly the shape §B.3 forbids: a standing
// override that silently re-points live work the next time a reconcile happens.
// A request that arrives after the freeze is still RECORDED — an operator's
// intent is not discarded — but it does not move anything, and the caller is
// told so by name.
//
// # What quiescence is, and what it is not
//
// It is an AND over durable facts, each asked of the component that owns the
// answer: the run's state, the placement's own state, the provider-attempt
// ledger, the capacity scheduler, the branch-lock manager, the worktree
// records. It is NOT an inspection of the filesystem. A checkout that looks
// idle proves nothing about whether a provider is mid-write, a merge is
// half-applied, or a repair holds a ceded lock — which is why §C says not to
// infer safety from filesystem state alone, and why every field of
// domain.PlacementQuiescence names an authority rather than an observation.
//
// # What this file deliberately does NOT provide
//
// There is no operation that changes a worktree path while something is running
// in it. A transition retires a generation and freezes a new one; the
// replacement is materialised by the ordinary launch path, under the ordinary
// admission gate. "Re-point this running agent" is not expressible here, which
// is the point.

// PlacementOverrides is the durable override/transition surface. Satisfied by
// *storage/sqlite/store.Store.
//
// Optional, following the same nil-dependency convention as every other
// capability in this package: a nil store means no override can be recorded and
// no transition can be performed, and both operations refuse by name rather
// than silently doing nothing.
type PlacementOverrides interface {
	RequestExecutionPlacementOverride(ctx stdctx.Context, o domain.ExecutionPlacementOverride) (bool, error)
	GetOutstandingPlacementOverride(ctx stdctx.Context, runID, taskID, stepID string) (domain.ExecutionPlacementOverride, bool, error)
	ResolvePlacementOverride(ctx stdctx.Context, id string, next domain.PlacementOverrideState, generation int64, detail string, now time.Time) (bool, error)
	ListPlacementOverridesForRun(ctx stdctx.Context, runID string) ([]domain.ExecutionPlacementOverride, error)
	RecordPlacementTransition(ctx stdctx.Context, t domain.ExecutionPlacementTransition) (bool, error)
	CompletePlacementTransition(ctx stdctx.Context, id string, toGeneration int64, toType domain.ExecutionPlacementType, detail string, now time.Time) (bool, error)
	GetSurvivingPlacementTransition(ctx stdctx.Context, runID, taskID, stepID string, fromGeneration int64) (domain.ExecutionPlacementTransition, bool, error)
	ListPlacementTransitionsForRun(ctx stdctx.Context, runID string) ([]domain.ExecutionPlacementTransition, error)
}

// placementOverridesEnabled reports whether the override authority is wired.
func (c *Coordinator) placementOverridesEnabled() bool { return c.placementOverrides != nil }

// PlacementOverrideRequestInput is one operator request.
type PlacementOverrideRequestInput struct {
	RunID  string
	TaskID string
	// Requested is `auto`, `direct_branch` or `isolated_worktree`.
	Requested domain.PlacementOverrideRequest
	// RequestedBy names the operator. Required: an unattributed re-pointing of
	// an obligation is refused rather than stored.
	RequestedBy string
	Reason      string
}

// PlacementOverrideOutcome is what happened to a request.
type PlacementOverrideOutcome struct {
	Override domain.ExecutionPlacementOverride
	// AppliesAtFreeze reports that nothing is frozen yet, so the next freeze
	// will consume this request. This is the ordinary path.
	AppliesAtFreeze bool
	// RequiresTransition reports that a placement is ALREADY frozen. The
	// request is recorded, and it changes nothing until a transition consumes
	// it — §B.3 stated as a return value rather than as a comment.
	RequiresTransition bool
	// CurrentPlacement is the frozen placement, when there is one.
	CurrentPlacement domain.ExecutionPlacement
}

// RequestPlacementOverride records what an operator wants this obligation's
// placement to be.
//
// Before the freeze it is an input to selection. After it, it is recorded and
// inert: the frozen record still wins, and moving it requires a transition. The
// outcome says which of the two happened, so an operator is never left assuming
// a request took effect when it did not.
//
// Idempotent in the way that matters: a repeated request supersedes the
// previous outstanding one rather than stacking, so double-clicking produces
// one outstanding wish, not two.
func (c *Coordinator) RequestPlacementOverride(ctx stdctx.Context, req PlacementOverrideRequestInput) (PlacementOverrideOutcome, error) {
	if !c.placementOverridesEnabled() {
		return PlacementOverrideOutcome{}, fmt.Errorf("%w: no placement override authority is wired", ErrInvalid)
	}
	if !req.Requested.IsKnown() {
		// Never coerced to auto. Substituting the default would hand an
		// operator a placement they did not ask for, with no signal.
		return PlacementOverrideOutcome{}, fmt.Errorf("%w: %q is not a placement AO understands", ErrInvalid, req.Requested)
	}
	if strings.TrimSpace(req.RequestedBy) == "" {
		return PlacementOverrideOutcome{}, fmt.Errorf("%w: a placement override must name who requested it", ErrInvalid)
	}
	run, found, err := c.store.GetWorkflowRun(ctx, req.RunID)
	if err != nil {
		return PlacementOverrideOutcome{}, err
	}
	if !found {
		return PlacementOverrideOutcome{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, req.RunID)
	}
	scope := placementScopeFor(run)
	if req.TaskID != "" {
		scope.taskID = req.TaskID
	}

	now := c.clock()
	override := domain.ExecutionPlacementOverride{
		ID:            "plo-" + c.newID(),
		WorkflowRunID: scope.runID, TaskID: scope.taskID, WorkflowStepID: scope.stepID,
		ProjectID: run.ProjectID,
		Requested: req.Requested, RequestedBy: strings.TrimSpace(req.RequestedBy),
		Reason:    strings.TrimSpace(req.Reason),
		State:     domain.PlacementOverrideRequested,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := c.placementOverrides.RequestExecutionPlacementOverride(ctx, override); err != nil {
		return PlacementOverrideOutcome{}, err
	}

	outcome := PlacementOverrideOutcome{Override: override}
	if !c.placementEnabled() {
		outcome.AppliesAtFreeze = true
		return outcome, nil
	}
	live, hasLive, err := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID)
	if err != nil {
		return PlacementOverrideOutcome{}, err
	}
	if hasLive {
		outcome.RequiresTransition = true
		outcome.CurrentPlacement = live
		return outcome, nil
	}
	outcome.AppliesAtFreeze = true
	return outcome, nil
}

// pendingPlacementOverride reads the request the next freeze should consume.
//
// Read-only and fail-soft: an unreadable override store means selection policy
// decides, which is the behaviour every deployment without the wiring already
// has. It is safe to be soft HERE and not in the transition path because a
// missed override produces the placement AO would have chosen anyway, while a
// missed authority in a transition would produce a move that was not proven
// safe.
func (c *Coordinator) pendingPlacementOverride(ctx stdctx.Context, scope placementScope) (domain.ExecutionPlacementOverride, bool) {
	if !c.placementOverridesEnabled() {
		return domain.ExecutionPlacementOverride{}, false
	}
	o, found, err := c.placementOverrides.GetOutstandingPlacementOverride(ctx, scope.runID, scope.taskID, scope.stepID)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not read the outstanding placement override", "run", scope.runID, "err", err)
		}
		return domain.ExecutionPlacementOverride{}, false
	}
	return o, found
}

// resolveConsumedOverride marks a request as consumed by the generation that
// consumed it. Best-effort with respect to the freeze: a placement that is
// frozen and an override row that still reads `requested` is untidy, and the
// next pass resolves it; failing the freeze over the bookkeeping would be worse.
func (c *Coordinator) resolveConsumedOverride(ctx stdctx.Context, o domain.ExecutionPlacementOverride, generation int64, detail string) {
	if !c.placementOverridesEnabled() || o.ID == "" {
		return
	}
	if _, err := c.placementOverrides.ResolvePlacementOverride(ctx, o.ID,
		domain.PlacementOverrideApplied, generation, detail, c.clock()); err != nil && c.log != nil {
		c.log.Debug("workflow: could not resolve a consumed placement override", "override", o.ID, "err", err)
	}
}

// PlacementTransitionInput is one operator request to replace a frozen
// placement generation.
type PlacementTransitionInput struct {
	RunID  string
	TaskID string
	// Requested is the placement the replacement should have. `auto` re-runs
	// selection policy for the replacement.
	Requested domain.PlacementOverrideRequest
	// RequestedBy names the operator. Required.
	RequestedBy string
	Reason      string
	// ExpectedState is the placement state the requester asserts the current
	// generation is in. A mismatch is a refusal, not a correction: the request
	// describes a world that no longer holds, and applying it anyway would act
	// on the operator's stale reading rather than on the current one.
	//
	// Empty means "I did not check", which is accepted: the quiescence proof is
	// the safety property, and the asserted state is an additional guard an
	// operator may choose to use. It is recorded either way.
	ExpectedState domain.ExecutionPlacementState
	// ExpectedGeneration is the generation the requester means to supersede.
	// Zero means "whatever is current"; a non-zero value that is not current is
	// refused, which is what makes a transition safe to retry from a stale UI.
	ExpectedGeneration int64
}

// PlacementTransitionOutcome is what happened to a transition request.
type PlacementTransitionOutcome struct {
	// Applied reports whether a replacement generation now exists.
	Applied bool
	// AlreadyApplied reports that this exact generation had already been
	// superseded, and the returned transition and placement are the ones that
	// did it. A repeated request is this, not a second generation (§B.10).
	AlreadyApplied bool
	Transition     domain.ExecutionPlacementTransition
	// From and To are the superseded and replacement placements. To is zero
	// when the transition was refused.
	From domain.ExecutionPlacement
	To   domain.ExecutionPlacement
	// Refusal names the authority that said no; empty when applied.
	Refusal domain.PlacementTransitionRefusal
	Detail  string
	// Quiescence is the proof, recorded whether it succeeded or not, so a
	// refusal says which authority was outstanding rather than only that one
	// was.
	Quiescence domain.PlacementQuiescence
}

// TransitionPlacement replaces one frozen placement generation with another.
//
// The order is the safety argument, and it is the same one the branch-lock
// cession uses: prove first, write the intent second, move third.
//
//  1. resolve the obligation and the CURRENT generation;
//  2. return early if this generation has already been superseded — a repeated
//     request is idempotent, not a second replacement;
//  3. check operator authority and the asserted state;
//  4. prove quiescence over durable facts;
//  5. record the transition BEFORE performing it, so a crash leaves an
//     explanation for a move that may not have happened;
//  6. retire the old generation — preserved when it holds work that never
//     landed, terminal otherwise;
//  7. freeze the replacement;
//  8. complete the transition row with the generation the replacement got.
//
// A refusal is not an error. It is a recorded fact with a named authority, and
// the operator's next action differs per authority.
func (c *Coordinator) TransitionPlacement(ctx stdctx.Context, req PlacementTransitionInput) (PlacementTransitionOutcome, error) {
	if !c.placementEnabled() || !c.placementOverridesEnabled() {
		return PlacementTransitionOutcome{}, fmt.Errorf("%w: no placement transition authority is wired", ErrInvalid)
	}
	if !req.Requested.IsKnown() {
		return c.refuseTransition(ctx, domain.ExecutionPlacement{}, req, domain.PlacementQuiescence{},
			domain.PlacementTransitionUnknownRequest, fmt.Sprintf("%q is not a placement AO understands", req.Requested))
	}
	run, found, err := c.store.GetWorkflowRun(ctx, req.RunID)
	if err != nil {
		return PlacementTransitionOutcome{}, err
	}
	if !found {
		return PlacementTransitionOutcome{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, req.RunID)
	}
	scope := placementScopeFor(run)
	if req.TaskID != "" {
		scope.taskID = req.TaskID
	}

	current, hasCurrent, err := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID)
	if err != nil {
		return PlacementTransitionOutcome{}, err
	}
	if !hasCurrent {
		// Nothing frozen. The operator wants an override, which applies at the
		// freeze; saying so by name is more useful than freezing something here
		// on their behalf, because a freeze needs the step this run has not
		// necessarily reached.
		newest, gerr := c.placements.MaxExecutionPlacementGeneration(ctx, scope.runID, scope.taskID, scope.stepID)
		if gerr != nil {
			return PlacementTransitionOutcome{}, gerr
		}
		if newest > 0 {
			// Every generation is terminal: the obligation is over, and there
			// is nothing a replacement could discharge.
			return c.refuseTransition(ctx, domain.ExecutionPlacement{}, req, domain.PlacementQuiescence{},
				domain.PlacementTransitionRunTerminal, "every placement generation for this obligation is already terminal")
		}
		return c.refuseTransition(ctx, domain.ExecutionPlacement{}, req, domain.PlacementQuiescence{},
			domain.PlacementTransitionNoPlacement, "no placement is frozen yet; record an override instead")
	}

	// 2. Idempotency, before any check that could refuse: a request repeated
	// after the transition already happened must return what happened, not a
	// refusal about a generation that is now correctly terminal.
	if prior, ok, terr := c.placementOverrides.GetSurvivingPlacementTransition(ctx,
		scope.runID, scope.taskID, scope.stepID, current.PlacementGeneration); terr != nil {
		return PlacementTransitionOutcome{}, terr
	} else if ok && prior.State == domain.PlacementTransitionApplied {
		to, _, gerr := c.placements.GetExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID, prior.ToGeneration)
		if gerr != nil {
			return PlacementTransitionOutcome{}, gerr
		}
		return PlacementTransitionOutcome{AlreadyApplied: true, Transition: prior, From: current, To: to}, nil
	}
	if req.ExpectedGeneration > 0 && req.ExpectedGeneration != current.PlacementGeneration {
		return c.refuseTransition(ctx, current, req, domain.PlacementQuiescence{},
			domain.PlacementTransitionNotCurrent,
			fmt.Sprintf("generation %d is not current; the obligation is at %d", req.ExpectedGeneration, current.PlacementGeneration))
	}
	if strings.TrimSpace(req.RequestedBy) == "" {
		return c.refuseTransition(ctx, current, req, domain.PlacementQuiescence{},
			domain.PlacementTransitionNoAuthority, "a placement transition must name who requested it")
	}
	if req.ExpectedState != "" && req.ExpectedState != current.State {
		return c.refuseTransition(ctx, current, req, domain.PlacementQuiescence{},
			domain.PlacementTransitionStateDrifted,
			fmt.Sprintf("the placement is %s, not the %s this request was made against", current.State, req.ExpectedState))
	}

	// 4. The proof.
	quiescence := c.provePlacementQuiescence(ctx, run, current)
	if !quiescence.Quiesced() {
		return c.refuseTransition(ctx, current, req, quiescence, quiescence.Refusal(),
			"an authority still owns the current placement")
	}

	// 5. Record the intent before performing it.
	now := c.clock()
	transition := domain.ExecutionPlacementTransition{
		ID:            "plt-" + c.newID(),
		WorkflowRunID: scope.runID, TaskID: scope.taskID, WorkflowStepID: scope.stepID,
		ProjectID:      run.ProjectID,
		FromGeneration: current.PlacementGeneration,
		FromType:       current.Type, FromRepoPath: current.RepoPath,
		FromExecutionBranch: current.ExecutionBranch, FromWorktreePath: current.WorktreePath,
		FromBaseSHA: current.BaseSHA,
		Requested:   req.Requested, RequestedBy: strings.TrimSpace(req.RequestedBy),
		Reason: strings.TrimSpace(req.Reason), ExpectedState: req.ExpectedState,
		QuiescenceDigest: quiescence.Digest,
		State:            domain.PlacementTransitionRequested,
		CreatedAt:        now, UpdatedAt: now,
	}
	created, err := c.placementOverrides.RecordPlacementTransition(ctx, transition)
	if err != nil {
		return PlacementTransitionOutcome{}, err
	}
	if !created {
		// A concurrent pass wrote the transition for this generation first. Its
		// row is the authority; this pass adopts it rather than racing to
		// perform a second replacement.
		prior, ok, gerr := c.placementOverrides.GetSurvivingPlacementTransition(ctx,
			scope.runID, scope.taskID, scope.stepID, current.PlacementGeneration)
		if gerr != nil {
			return PlacementTransitionOutcome{}, gerr
		}
		if ok {
			transition = prior
		}
	}

	// 6/7. Retire, then freeze. This ordering is forced by the live partial
	// unique index -- at most one non-terminal placement per obligation -- and
	// it is the safe half of the choice: a crash between the two leaves NO live
	// placement, which the next pass freezes cleanly, rather than two.
	replacement, err := c.replacePlacementGeneration(ctx, run, current, req.Requested, transition)
	if err != nil {
		return PlacementTransitionOutcome{}, err
	}

	if _, err := c.placementOverrides.CompletePlacementTransition(ctx, transition.ID,
		replacement.PlacementGeneration, replacement.Type,
		"placement generation "+strconv.FormatInt(current.PlacementGeneration, 10)+" superseded", c.clock()); err != nil {
		return PlacementTransitionOutcome{}, err
	}
	transition.State = domain.PlacementTransitionApplied
	transition.ToGeneration = replacement.PlacementGeneration
	transition.ToType = replacement.Type

	// The request, if there was one outstanding, is now discharged by a real
	// generation and stops being outstanding.
	if pending, ok := c.pendingPlacementOverride(ctx, scope); ok {
		c.resolveConsumedOverride(ctx, pending, replacement.PlacementGeneration, "consumed by a placement transition")
	}

	return PlacementTransitionOutcome{
		Applied: true, Transition: transition, From: current, To: replacement, Quiescence: quiescence,
	}, nil
}

// replacePlacementGeneration retires the current generation and freezes its
// successor.
//
// The replacement inherits the repository, the base branch and the merge target
// from the placement it replaces rather than re-reading project configuration
// for them. That is deliberate: a transition changes WHERE the work physically
// happens, not what it is or where it is meant to land, and re-deriving the
// target would let a configuration change ride in on an operator's placement
// decision — the exact coupling the freeze exists to break.
func (c *Coordinator) replacePlacementGeneration(
	ctx stdctx.Context, run domain.WorkflowRun, current domain.ExecutionPlacement,
	requested domain.PlacementOverrideRequest, transition domain.ExecutionPlacementTransition,
) (domain.ExecutionPlacement, error) {
	scope := placementScope{current.WorkflowRunID, current.TaskID, current.WorkflowStepID}
	now := c.clock()
	generation := current.PlacementGeneration + 1

	nextType := requested.PlacementType()
	if !requested.Explicit() {
		mode := c.projectExecutionModeFor(ctx, run, scope)
		nextType = domain.PlacementTypeForExecutionMode(mode)
	}

	// Retire the superseded generation. `preserved` rather than `terminal` when
	// it is an isolated placement whose work never landed: the agent's commits
	// on that branch may be the only copy, and a transition is exactly when
	// somebody wants to go and read them. The distinction is the same one
	// reconcilePlacementsForRun makes for a terminal run.
	retired := domain.PlacementTerminal
	if current.Type.Isolated() && current.IntegratedSHA == "" {
		retired = domain.PlacementPreserved
	}
	detail := "superseded by placement generation " + strconv.FormatInt(generation, 10) + " (transition " + transition.ID + ")"
	if _, err := c.placements.TransitionExecutionPlacement(ctx,
		scope.runID, scope.taskID, scope.stepID, current.PlacementGeneration,
		current.State, retired, "", detail, now); err != nil {
		return domain.ExecutionPlacement{}, err
	}
	// Belt and braces against a crash that left an older generation live: the
	// live index admits one placement, and a stale non-terminal row below the
	// replacement would refuse the freeze below with a constraint violation
	// rather than with an explanation.
	if _, err := c.placements.RetireSupersededExecutionPlacements(ctx,
		scope.runID, scope.taskID, scope.stepID, generation, detail, now); err != nil {
		return domain.ExecutionPlacement{}, err
	}

	replacement := domain.ExecutionPlacement{
		ID:            "plc-" + c.newID(),
		WorkflowRunID: scope.runID, TaskID: scope.taskID, WorkflowStepID: scope.stepID,
		ProjectID:           current.ProjectID,
		PlacementGeneration: generation,
		LifecycleGeneration: current.LifecycleGeneration,
		Type:                nextType,
		RepoPath:            current.RepoPath,
		BaseBranch:          current.BaseBranch,
		MergeTarget:         current.MergeTarget,
		OwnerToken:          c.placementOwnerToken(),
		State:               domain.PlacementSelected,
		Provenance:          domain.PlacementFrozenAtSelection,
		Detail:              "replaces placement generation " + strconv.FormatInt(current.PlacementGeneration, 10),
		CreatedAt:           now, UpdatedAt: now,
	}
	if nextType.Isolated() {
		replacement.ExecutionBranch = placementExecutionBranch(scope, generation)
	} else {
		replacement.ExecutionBranch = current.MergeTarget
		if replacement.ExecutionBranch == "" {
			replacement.ExecutionBranch = current.BaseBranch
		}
	}
	// BaseSHA is deliberately NOT copied. The replacement has not been cut yet,
	// and inheriting the predecessor's base commit would record, as a fact, a
	// commit this checkout was never made from.
	if _, err := c.placements.FreezeExecutionPlacement(ctx, replacement); err != nil {
		return domain.ExecutionPlacement{}, err
	}
	return replacement, nil
}

// placementExecutionBranch is the ao/* branch a generation writes to.
//
// Generation 1 keeps the historical name exactly, so nothing about an ordinary
// run changes. A replacement generation gets a SUFFIXED name rather than a
// deeper path, because git refuses `refs/heads/a/b` while `refs/heads/a` exists
// — a nested name would make the replacement branch uncreatable precisely when
// the predecessor is being preserved, which is the case that needs it most.
func placementExecutionBranch(scope placementScope, generation int64) string {
	base := workspace.BranchFor(scope.runID, placementTaskKey(scope))
	if generation <= 1 {
		return base
	}
	return base + "-g" + strconv.FormatInt(generation, 10)
}

// projectExecutionModeFor resolves the selection policy an `auto` request
// defers to: the project's execution mode, with the planner's per-task
// downgrade applied. Fail-soft to the isolated default, which is the mode that
// never writes into somebody's own checkout.
func (c *Coordinator) projectExecutionModeFor(ctx stdctx.Context, run domain.WorkflowRun, scope placementScope) domain.ExecutionMode {
	project, ok, err := c.projects.GetProject(ctx, run.ProjectID)
	if err != nil || !ok {
		return domain.ExecutionIsolatedWorktree
	}
	mode := domain.ResolveExecutionMode(project.Kind, project.Config)
	if scope.taskID != "" {
		if taskScope, found := c.taskScopeFor(ctx, run, scope.taskID); found {
			mode = domain.ResolveTaskExecutionMode(mode, taskScope)
		}
	}
	return mode
}

// provePlacementQuiescence is the AND over durable facts §C requires.
//
// It returns no error, and that is the model rather than a convenience: an
// authority AO cannot read is a proof that FAILED, which the caller must handle
// as a refusal an operator can act on. Surfacing it as an error instead would
// lose which authority was unreadable behind a 500.
//
// Every field is a question put to the component that owns the answer, and
// every read failure is a refusal rather than a default: an authority AO cannot
// read is never evidence that it is free. That asymmetry is the whole point —
// the cost of a false "quiesced" is two providers on one checkout, and the cost
// of a false "not yet" is an operator retrying in a minute.
func (c *Coordinator) provePlacementQuiescence(ctx stdctx.Context, run domain.WorkflowRun, placement domain.ExecutionPlacement) domain.PlacementQuiescence {
	q := domain.PlacementQuiescence{}
	facts := []string{}
	note := func(format string, args ...any) { facts = append(facts, fmt.Sprintf(format, args...)) }

	// The run itself. A terminal run's placement will never be launched into
	// again, so a replacement would only create a second thing to clean up.
	q.RunActive = !run.State.Terminal()
	note("run_state=%s", run.State)

	// Integration authority (§B.7). `reviewing` counts: a reviewer is reading
	// the checkout, and `integrating` counts because a merge is half-applied by
	// definition while it holds. `integrated` and `conflict` count because both
	// name work whose disposition the old placement still owns.
	switch placement.State {
	case domain.PlacementReviewing, domain.PlacementIntegrating,
		domain.PlacementIntegrated, domain.PlacementConflict:
		q.NoIntegrationAuthority = false
	default:
		q.NoIntegrationAuthority = true
	}
	note("placement_state=%s", placement.State)

	// The provider-attempt ledger. An authoritative attempt is a provider that
	// may be writing right now, whether or not a runtime probe agrees.
	q.NoProviderAttempt = true
	if c.providerAttemptsEnabled() {
		attempts, err := c.providerAttempts.ListProviderAttemptsForRun(ctx, run.ID)
		if err != nil {
			return refuseUnreadableQuiescence(q, facts, "provider_attempts")
		}
		live := 0
		for _, a := range attempts {
			if a.State.Authoritative() {
				live++
			}
		}
		q.NoProviderAttempt = live == 0
		note("authoritative_provider_attempts=%d", live)
	} else {
		note("authoritative_provider_attempts=unwired")
	}

	// The capacity scheduler. An outstanding claim means a runtime slot is paid
	// for, which is AO's own proof that a worker, reviewer, fix or repair
	// runtime may exist — §B.6 answered from the ledger rather than from a
	// process probe that can lie in both directions.
	q.NoCapacityClaim = true
	q.NoLiveRuntime = true
	if c.capacityEnabled() {
		claims, err := c.capacity.ListCapacityClaimsForRun(ctx, run.ID)
		if err != nil {
			return refuseUnreadableQuiescence(q, facts, "capacity_claims")
		}
		outstanding, withRuntime := 0, 0
		for _, claim := range claims {
			if claim.State == domain.CapacityClaimReleased {
				continue
			}
			outstanding++
			if claim.RuntimeHandle != "" {
				withRuntime++
			}
		}
		q.NoCapacityClaim = outstanding == 0
		q.NoLiveRuntime = withRuntime == 0
		note("outstanding_capacity_claims=%d runtime_bound_claims=%d", outstanding, withRuntime)
	} else {
		note("outstanding_capacity_claims=unwired")
	}

	// Branch authority, including a lock ceded to a repair run: the cession
	// leaves the row `held` by the repair, and HeldByRun on the origin
	// correctly reports none — which is why the repair's own hold is checked
	// through the same call for the run that owns the obligation.
	q.NoBranchAuthority = true
	if c.branchLocks != nil {
		locks, err := c.branchLocks.HeldByRun(ctx, run.ID)
		if err != nil {
			return refuseUnreadableQuiescence(q, facts, "branch_locks")
		}
		q.NoBranchAuthority = len(locks) == 0
		note("held_branch_locks=%d", len(locks))
	} else {
		note("held_branch_locks=unwired")
	}

	sort.Strings(facts)
	q.Digest = quiescenceDigest(facts)
	return q
}

// refuseUnreadableQuiescence turns an unreadable authority into a proof that
// fails, with the digest naming which read failed. It never returns an error:
// "AO could not check" is an operational answer an operator can act on, and
// surfacing it as a 500 would lose the reason.
func refuseUnreadableQuiescence(q domain.PlacementQuiescence, facts []string, authority string) domain.PlacementQuiescence {
	q.RunActive = false
	facts = append(facts, "unreadable="+authority)
	sort.Strings(facts)
	q.Digest = quiescenceDigest(facts)
	return q
}

// quiescenceDigest is the recorded form of a proof: the sorted facts, and a
// short hash of them so two proofs can be compared without reading prose.
func quiescenceDigest(facts []string) string {
	joined := strings.Join(facts, " ")
	sum := sha256.Sum256([]byte(joined))
	return joined + " sha256=" + hex.EncodeToString(sum[:])[:16]
}

// refuseTransition records a refusal and returns it as an outcome.
//
// Refusals are durable on purpose. A refusal an operator cannot read afterwards
// is one they will run into again, and the audit trail of what was asked and
// why AO said no is what makes the closed refusal vocabulary useful rather than
// merely tidy. Refused rows are excluded from the surviving-transition index,
// so a "not yet" never becomes a permanent no.
func (c *Coordinator) refuseTransition(
	ctx stdctx.Context, current domain.ExecutionPlacement, req PlacementTransitionInput,
	q domain.PlacementQuiescence, reason domain.PlacementTransitionRefusal, detail string,
) (PlacementTransitionOutcome, error) {
	now := c.clock()
	row := domain.ExecutionPlacementTransition{
		ID:            "plt-" + c.newID(),
		WorkflowRunID: req.RunID, TaskID: current.TaskID, WorkflowStepID: current.WorkflowStepID,
		ProjectID:      current.ProjectID,
		FromGeneration: current.PlacementGeneration,
		FromType:       current.Type, FromRepoPath: current.RepoPath,
		FromExecutionBranch: current.ExecutionBranch, FromWorktreePath: current.WorktreePath,
		FromBaseSHA: current.BaseSHA,
		Requested:   req.Requested, RequestedBy: strings.TrimSpace(req.RequestedBy),
		Reason: strings.TrimSpace(req.Reason), ExpectedState: req.ExpectedState,
		QuiescenceDigest: q.Digest,
		State:            domain.PlacementTransitionRefused,
		RefusalReason:    reason, Detail: detail,
		CreatedAt: now, UpdatedAt: now,
	}
	if row.TaskID == "" && req.TaskID != "" {
		row.TaskID = req.TaskID
	}
	if !row.Requested.IsKnown() {
		// The row's CHECK constraint admits only the three known values, and a
		// refusal ABOUT an unreadable request must still be recordable. `auto`
		// is the honest stand-in: it is what AO would have done, and the
		// refusal reason says what was actually asked for.
		row.Requested = domain.PlacementOverrideAuto
	}
	if c.placementOverridesEnabled() {
		if _, err := c.placementOverrides.RecordPlacementTransition(ctx, row); err != nil && c.log != nil {
			c.log.Warn("workflow: could not record a placement transition refusal", "run", req.RunID, "err", err)
		}
	}
	return PlacementTransitionOutcome{
		Transition: row, From: current, Refusal: reason, Detail: detail, Quiescence: q,
	}, nil
}

// PlacementOverrideView is the read-side projection of one request.
type PlacementOverrideView struct {
	Requested         domain.PlacementOverrideRequest
	RequestedBy       string
	Reason            string
	State             domain.PlacementOverrideState
	AppliedGeneration int64
	TaskID            string
	Detail            string
	CreatedAt         time.Time
}

// PlacementTransitionView is the read-side projection of one transition.
type PlacementTransitionView struct {
	FromGeneration int64
	ToGeneration   int64
	FromType       domain.ExecutionPlacementType
	ToType         domain.ExecutionPlacementType
	Requested      domain.PlacementOverrideRequest
	RequestedBy    string
	Reason         string
	ExpectedState  domain.ExecutionPlacementState
	State          domain.PlacementTransitionState
	RefusalReason  domain.PlacementTransitionRefusal
	Quiescence     string
	TaskID         string
	Detail         string
	CreatedAt      time.Time
}

// ListPlacementOverrides returns a run's override history.
func (c *Coordinator) ListPlacementOverrides(ctx stdctx.Context, runID string) ([]PlacementOverrideView, error) {
	if !c.placementOverridesEnabled() {
		return nil, nil
	}
	rows, err := c.placementOverrides.ListPlacementOverridesForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]PlacementOverrideView, 0, len(rows))
	for _, r := range rows {
		out = append(out, PlacementOverrideView{
			Requested: r.Requested, RequestedBy: r.RequestedBy, Reason: r.Reason,
			State: r.State, AppliedGeneration: r.AppliedGeneration,
			TaskID: r.TaskID, Detail: r.Detail, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// ListPlacementTransitions returns a run's transition history, refusals
// included.
func (c *Coordinator) ListPlacementTransitions(ctx stdctx.Context, runID string) ([]PlacementTransitionView, error) {
	if !c.placementOverridesEnabled() {
		return nil, nil
	}
	rows, err := c.placementOverrides.ListPlacementTransitionsForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]PlacementTransitionView, 0, len(rows))
	for _, r := range rows {
		out = append(out, PlacementTransitionView{
			FromGeneration: r.FromGeneration, ToGeneration: r.ToGeneration,
			FromType: r.FromType, ToType: r.ToType, Requested: r.Requested,
			RequestedBy: r.RequestedBy, Reason: r.Reason, ExpectedState: r.ExpectedState,
			State: r.State, RefusalReason: r.RefusalReason, Quiescence: r.QuiescenceDigest,
			TaskID: r.TaskID, Detail: r.Detail, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}
