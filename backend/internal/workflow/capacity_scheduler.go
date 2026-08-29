package workflow

import (
	stdctx "context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// capacity_scheduler.go — P1-C's runtime admission control.
//
// The rule this file exists to make true:
//
//	NO RUNTIME LAUNCH WITHOUT AN AUTHORITATIVE CAPACITY CLAIM.
//
// Every metered launch site (worker, reviewer, planner, repair) asks here
// first, and a refusal parks the run on the durable capacity wait AO already
// had rather than failing it. Fix delivery deliberately does NOT ask: it writes
// into a worker session that is already running, and charging a second slot for
// a message would mean a run could be blocked from finishing by its own
// occupied slot.
//
// # Why this is not the other capacity
//
// `capacity_wait.go` and `capacity_probe.go` answer "is this PROVIDER usable"
// -- auth, an installed CLI, a rate-limit cooldown. They decide which harness a
// role routes to. This decides how many things may run on this machine at all.
// A dispatch must pass both, and neither substitutes for the other. They share
// the wake reasons (ReasonWorkerCapacity and friends) and the
// `waiting_for_capacity` phase on purpose: to a person, "AO is waiting for room
// to run this" is one situation, and the reason code says which room.
//
// # Scheduling order and fairness
//
// Deterministic, and stated in one place:
//
//  1. eligible work only -- a queued claim whose run is terminal or whose
//     generation is superseded is released, never promoted;
//  2. priority ascending (repairs at 50, everything else at 100);
//  3. enqueued_at ascending, then claim id -- FIFO with a total tiebreak.
//
// Starvation is prevented by a per-workflow concurrency cap enforced inside
// the granting statement itself. A master objective with twenty runnable
// children can hold at most CapacityLimits.PerWorkflow slots, so there is
// always room for another workflow to reach the front of the queue. That is
// the entire fairness policy: a per-workflow cap plus FIFO. It is deliberately
// not a weighted fair queue -- AO schedules a handful of slots on one machine,
// and a policy nobody can predict is worse than a simple one.

// CapacityStore is the durable scheduler surface the Coordinator depends on.
//
// Optional, like every other Deps dependency in this package: a nil store
// means no admission control, which is what every pre-P1-C test double and any
// deployment without the wiring gets. The daemon always wires it, and
// TestDaemonWiresCapacityScheduler asserts that, so "no launch without a claim"
// is enforced everywhere it can be.
type CapacityStore interface {
	EnqueueCapacityClaim(ctx stdctx.Context, claim domain.CapacityClaim) (bool, error)
	AcquireCapacity(ctx stdctx.Context, dispatchKey string, generation int64, limits domain.CapacityLimits, kind domain.ExecutionKind, now time.Time) (bool, error)
	GetCapacityClaim(ctx stdctx.Context, dispatchKey string) (domain.CapacityClaim, bool, error)
	BindCapacityClaimRuntime(ctx stdctx.Context, dispatchKey, handle, instanceID string, generation int64, now time.Time) (bool, error)
	ReleaseCapacityClaim(ctx stdctx.Context, dispatchKey string, generation int64, reason string, now time.Time) (bool, error)
	ReleaseCapacityClaimsForRun(ctx stdctx.Context, runID, reason string, now time.Time) (int64, error)
	CapacityUsageByKind(ctx stdctx.Context) (map[domain.ExecutionKind]int, map[domain.ExecutionKind]int, int, error)
	ListHeldCapacityClaims(ctx stdctx.Context) ([]domain.CapacityClaim, error)
	ListQueuedCapacityClaims(ctx stdctx.Context, limit int) ([]domain.CapacityClaim, error)
	ListCapacityClaimsForRun(ctx stdctx.Context, runID string) ([]domain.CapacityClaim, error)
	ListOutstandingCapacityClaims(ctx stdctx.Context) ([]domain.CapacityClaim, error)
}

// capacityRequest is one intended launch, in the scheduler's vocabulary.
type capacityRequest struct {
	Kind domain.ExecutionKind
	Run  domain.WorkflowRun
	// StepID and TaskID place the claim; both may be empty for a planner,
	// whose claim belongs to the run.
	StepID string
	TaskID string
	// Generation is the step's dispatch generation. It is the fence, and it is
	// what makes a stale writer unable to claim, renew or release.
	Generation int64
	// IntentKey is the launch intent's own identity -- in practice the outbox
	// idempotency key the dispatch site already mints. One intent, one claim.
	IntentKey string
}

// dispatchKey is the claim's durable identity: kind, launch intent, and
// generation.
//
// The GENERATION is part of the key, not only of the fence, and that is
// load-bearing. A retry is a genuinely new launch intent -- its predecessor's
// claim was released when the lifecycle moved past it -- so it needs its own
// claim. With a generation-less key the retry found its predecessor's released
// claim and was refused forever, which is how a run parked itself permanently
// on its own second attempt (caught by the autonomous suite the moment the
// scheduler was wired into it).
//
// Within one generation the key is stable, so the idempotency the unique index
// provides is unchanged: one intended launch, one claim, however many times a
// reconcile or a wake re-derives it.
func (r capacityRequest) dispatchKey() string {
	return "cap:" + string(r.Kind) + ":" + r.IntentKey + ":gen" + strconv.FormatInt(r.Generation, 10)
}

// capacityEnabled reports whether admission control is wired.
func (c *Coordinator) capacityEnabled() bool { return c.capacity != nil }

// effectiveCapacityLimits returns the configured bounds, normalized so a
// partial configuration reads as a default rather than as "zero slots".
func (c *Coordinator) effectiveCapacityLimits() domain.CapacityLimits {
	return c.capacityLimits.Normalize()
}

// acquireCapacity is the admission gate every metered launch passes through.
//
// It always enqueues first and promotes second, which is what makes the queue
// durable: a daemon that dies between the two comes back to a queued claim and
// promotes it on the next pass, rather than to nothing at all. Both writes are
// idempotent, so the repeated call a reconcile or a wake produces converges on
// one claim.
//
// admitted=false is a normal answer, not an error. The caller parks the run on
// the durable capacity wait; nothing fails and no retry budget is spent.
func (c *Coordinator) acquireCapacity(ctx stdctx.Context, req capacityRequest) (bool, error) {
	if !c.capacityEnabled() {
		return true, nil
	}
	if !req.Kind.Valid() || req.IntentKey == "" {
		return false, fmt.Errorf("%w: capacity request needs a kind and a launch intent", ErrInvalid)
	}
	key := req.dispatchKey()
	now := c.clock()

	// An existing HELD claim for this exact intent is this launch's own
	// authority, already granted. Returning it (rather than trying to grant a
	// second) is what makes a repeated dispatch idempotent instead of
	// double-charging the meter.
	if existing, found, err := c.capacity.GetCapacityClaim(ctx, key); err != nil {
		return false, err
	} else if found {
		switch existing.State {
		case domain.CapacityClaimHeld:
			// This launch's own authority, already granted. The key carries the
			// generation, so a claim found under it can only be this exact
			// intent's.
			return true, nil
		case domain.CapacityClaimReleased:
			// Already spent and returned. Launching under a released claim is
			// exactly the false-free-slot this model exists to prevent, so the
			// caller waits and the next pass -- which will carry the newer
			// generation, and therefore a new key -- proceeds.
			return false, nil
		}
	}

	claim := domain.CapacityClaim{
		ID: "cap-" + c.newID(), Kind: req.Kind, State: domain.CapacityClaimQueued,
		WorkflowRunID: req.Run.ID, WorkflowStepID: req.StepID, TaskID: req.TaskID,
		LifecycleGeneration: req.Generation, DispatchKey: key,
		OwnerID: c.runOwner(ctx, req.Run.ID), ProjectID: req.Run.ProjectID,
		Priority: domain.PriorityForKind(req.Kind), EnqueuedAt: now, UpdatedAt: now,
	}
	if _, err := c.capacity.EnqueueCapacityClaim(ctx, claim); err != nil {
		return false, err
	}
	granted, err := c.capacity.AcquireCapacity(ctx, key, req.Generation, c.effectiveCapacityLimits(), req.Kind, now)
	if err != nil {
		return false, err
	}
	if !granted && c.log != nil {
		c.log.Debug("capacity: launch queued", "run", req.Run.ID, "kind", req.Kind, "key", key)
	}
	return granted, nil
}

// bindCapacityRuntime records which runtime incarnation a claim paid for, so
// GC can later tell this claim's own runtime from a stranger that took its
// name. Best-effort: the claim is the authority, the binding is evidence.
func (c *Coordinator) bindCapacityRuntime(ctx stdctx.Context, claim domain.CapacityClaim, handle, instanceID string) {
	if !c.capacityEnabled() || handle == "" {
		return
	}
	if claim.RuntimeHandle == handle && claim.RuntimeInstanceID == instanceID {
		return
	}
	if _, err := c.capacity.BindCapacityClaimRuntime(ctx, claim.DispatchKey, handle, instanceID, claim.LifecycleGeneration, c.clock()); err != nil && c.log != nil {
		c.log.Debug("capacity: could not bind runtime to claim", "key", claim.DispatchKey, "err", err)
	}
}

// bindRuntimesToClaims records, for each held claim, which runtime it actually
// paid for.
//
// It matters for Runtime GC rather than for scheduling: a claim that names the
// runtime it started is GC's strongest candidate source, because it identifies
// an exact incarnation AO can prove it launched. Without it GC falls back to
// the ownership token alone, which covers reviewer panes but not workers.
//
// It runs during reconciliation rather than at dispatch because that is where
// the session id is durably known: the dispatch intent is written before the
// launch, and the session it produced is a fact the step carries afterwards.
// Idempotent, and best-effort -- the claim is the authority, this is evidence.
func (c *Coordinator) bindRuntimesToClaims(ctx stdctx.Context, claims []domain.CapacityClaim, steps []domain.WorkflowStep) {
	if c.sessionFacts == nil {
		return
	}
	sessionByStep := map[string]string{}
	for _, step := range steps {
		if step.SessionID != nil && *step.SessionID != "" {
			sessionByStep[step.ID] = *step.SessionID
		}
	}
	for _, claim := range claims {
		if claim.State != domain.CapacityClaimHeld || claim.RuntimeHandle != "" {
			continue
		}
		sessionID, ok := sessionByStep[claim.WorkflowStepID]
		if !ok {
			continue
		}
		rec, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
		if err != nil || !found || rec.Metadata.RuntimeHandleID == "" {
			continue
		}
		c.bindCapacityRuntime(ctx, claim, rec.Metadata.RuntimeHandleID, "")
	}
}

// releaseCapacity returns one slot and wakes whatever was waiting for it.
//
// Generation-fenced, so a stale generation can never release a newer claim,
// and idempotent: a second release matches zero rows and is a no-op rather
// than an error or a second free slot.
func (c *Coordinator) releaseCapacity(ctx stdctx.Context, req capacityRequest, reason string) {
	if !c.capacityEnabled() {
		return
	}
	released, err := c.capacity.ReleaseCapacityClaim(ctx, req.dispatchKey(), req.Generation, reason, c.clock())
	if err != nil {
		if c.log != nil {
			c.log.Warn("capacity: release failed", "key", req.dispatchKey(), "err", err)
		}
		return
	}
	if released {
		c.wakeQueuedCapacity(ctx)
	}
}

// releaseCapacityForRun frees everything a TERMINAL run still holds.
//
// Not generation-fenced, deliberately: a terminal run can never launch
// anything again under any generation, so there is no newer claim to protect —
// and a finished run holding a slot forever is precisely the leak that turns a
// long-lived daemon into a stalled one.
func (c *Coordinator) releaseCapacityForRun(ctx stdctx.Context, runID, reason string) {
	if !c.capacityEnabled() || runID == "" {
		return
	}
	n, err := c.capacity.ReleaseCapacityClaimsForRun(ctx, runID, reason, c.clock())
	if err != nil {
		if c.log != nil {
			c.log.Warn("capacity: run release failed", "run", runID, "err", err)
		}
		return
	}
	if n > 0 {
		if c.log != nil {
			c.log.Info("capacity: released a terminal run's slots", "run", runID, "claims", n, "reason", reason)
		}
		c.wakeQueuedCapacity(ctx)
	}
}

// maxCapacityWakesPerRelease bounds how many waiting runs one release wakes.
//
// A release frees one slot, so waking one run per distinct workflow at the
// front of the queue is enough to fill it, and a small margin covers the case
// where the woken run turns out to be ineligible. Waking the whole queue on
// every release is the polling storm §I forbids.
const maxCapacityWakesPerRelease = 3

// wakeQueuedCapacity makes the eligible front of the queue runnable again.
//
// It schedules the same durable wake a capacity-parked run already uses, and
// wake.Schedule upserts by idempotency key, so a duplicate release cannot
// produce duplicate wakes. At most one wake per workflow per pass: a master
// objective with ten queued children must not consume the whole margin.
func (c *Coordinator) wakeQueuedCapacity(ctx stdctx.Context) {
	if !c.capacityEnabled() || c.wakeScheduler == nil {
		return
	}
	queued, err := c.capacity.ListQueuedCapacityClaims(ctx, maxCapacityWakesPerRelease*4)
	if err != nil || len(queued) == 0 {
		return
	}
	seen := map[string]struct{}{}
	woken := 0
	for _, claim := range queued {
		if woken >= maxCapacityWakesPerRelease {
			return
		}
		if _, dup := seen[claim.WorkflowRunID]; dup {
			continue
		}
		seen[claim.WorkflowRunID] = struct{}{}
		run, found, rerr := c.store.GetWorkflowRun(ctx, claim.WorkflowRunID)
		if rerr != nil || !found || run.State.Terminal() {
			continue
		}
		c.scheduleWake(ctx, run, nil, capacityWakeReason(claim.Kind), "")
		woken++
	}
}

// capacityWakeReason maps an execution kind onto the durable wake reason the
// run is parked under, reusing the vocabulary the provider-capacity wait
// already established rather than minting a parallel one.
func capacityWakeReason(kind domain.ExecutionKind) wake.Reason {
	switch kind {
	case domain.ExecutionKindReviewer:
		return wake.ReasonReviewerCapacity
	case domain.ExecutionKindPlanner:
		return wake.ReasonPlannerCapacity
	default:
		return wake.ReasonWorkerCapacity
	}
}

// reconcileCapacityForRun is the recovery half, run once per run per
// reconciliation pass.
//
// It answers exactly one question — does this run still legitimately hold what
// it holds — and it is per-run on purpose (§W): a run whose capacity state
// cannot be reasoned about is parked by its own reconcile, and every other run
// keeps scheduling.
//
// Two rules:
//
//   - A TERMINAL run releases everything. This is what closes crash windows 6,
//     7 and 8: a runtime that finished, or a run that ended, before its release
//     landed comes back to a claim its own reconciliation returns.
//   - A claim whose step has moved past its generation is released as
//     superseded. That is window 11: a stale generation's historical claim
//     must not keep paying for a slot nobody is using.
func (c *Coordinator) reconcileCapacityForRun(ctx stdctx.Context, run domain.WorkflowRun) {
	if !c.capacityEnabled() {
		return
	}
	if run.State.Terminal() {
		c.releaseCapacityForRun(ctx, run.ID, "run reached "+string(run.State))
		return
	}
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return
	}
	generation := map[string]int64{}
	terminal := map[string]bool{}
	for _, step := range steps {
		attempts, aerr := c.store.ListWorkflowAttempts(ctx, step.ID)
		if aerr != nil {
			return
		}
		generation[step.ID] = int64(len(attempts))
		terminal[step.ID] = step.State.Terminal()
	}
	c.reconcileCapacityWithSteps(ctx, run, steps, generation, terminal)
}

// reconcileCapacityWithSteps is reconcileCapacityForRun's body for a caller
// that has already loaded the run's steps and their attempt counts.
//
// GetRun is such a caller, and hooking it there is what makes a slot come back
// promptly: a worker step that completes frees its slot on the very next read
// of the run, rather than waiting for a daemon restart's reconciliation. It
// costs one indexed query on a run that holds no claims, and reuses data
// GetRun has already paid for on a run that does.
func (c *Coordinator) reconcileCapacityWithSteps(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep, generation map[string]int64, terminal map[string]bool) {
	if !c.capacityEnabled() {
		return
	}
	if run.State.Terminal() {
		c.releaseCapacityForRun(ctx, run.ID, "run reached "+string(run.State))
		return
	}
	claims, err := c.capacity.ListCapacityClaimsForRun(ctx, run.ID)
	if err != nil {
		if c.log != nil {
			c.log.Warn("capacity: could not reconcile a run's claims", "run", run.ID, "err", err)
		}
		return
	}
	// The newest outstanding generation per (step, kind), computed before
	// anything is released so the decision is taken against one consistent
	// view rather than against a set that changes as it is walked.
	newestByStepKind := map[stepKindKey]int64{}
	for _, claim := range claims {
		if claim.State.Terminal() || claim.WorkflowStepID == "" {
			continue
		}
		key := stepKindKey{claim.WorkflowStepID, claim.Kind}
		if claim.LifecycleGeneration > newestByStepKind[key] {
			newestByStepKind[key] = claim.LifecycleGeneration
		}
	}

	// Record which runtime each held claim actually paid for, before deciding
	// anything: a claim that names its incarnation is what lets Runtime GC
	// tell this run's own runtime from a stranger that later took its name.
	c.bindRuntimesToClaims(ctx, claims, steps)

	freed := false
	for _, claim := range claims {
		if claim.State.Terminal() {
			continue
		}
		// A planner claim belongs to the RUN, not to a step, so the
		// step-scoped rules below cannot see it. Its authority is the plan
		// row: a plan that is not currently being generated cannot have a
		// planner running for it, so the slot goes back. This is what closes
		// crash window §N(6) for the planner -- a daemon that died mid-plan
		// would otherwise hold a planner slot until it was restarted twice.
		if claim.WorkflowStepID == "" {
			if c.plannerClaimIsStale(ctx, run) {
				if ok, rerr := c.capacity.ReleaseCapacityClaim(ctx, claim.DispatchKey, claim.LifecycleGeneration,
					"the planner this slot paid for is no longer running", c.clock()); rerr == nil && ok {
					freed = true
				}
			}
			continue
		}
		// Supersession, per (step, kind). Only the newest generation of one
		// kind of launch on one step can be running: a review step's cycle 3
		// supersedes its cycle 2, and a work step's attempt 2 supersedes its
		// attempt 1. Releasing the older ones is what keeps a long
		// review/fix cycle from accumulating a slot per round.
		//
		// It is expressed over the claims themselves rather than over any
		// external counter, so it holds for every kind whatever its generation
		// happens to count.
		if newest, ok := newestByStepKind[stepKindKey{claim.WorkflowStepID, claim.Kind}]; ok && claim.LifecycleGeneration < newest {
			if ok, rerr := c.capacity.ReleaseCapacityClaim(ctx, claim.DispatchKey, claim.LifecycleGeneration,
				"superseded by a newer launch on the same step", c.clock()); rerr == nil && ok {
				freed = true
			}
			continue
		}

		current, known := generation[claim.WorkflowStepID]
		switch {
		case !known:
			// A claim naming a step this run does not have. Nothing can prove
			// what it is paying for, so it is released rather than left to
			// hold a slot forever -- but it is released under its OWN
			// generation, so it cannot take a newer claim with it.
			if ok, rerr := c.capacity.ReleaseCapacityClaim(ctx, claim.DispatchKey, claim.LifecycleGeneration,
				"claim references a step this run does not have", c.clock()); rerr == nil && ok {
				freed = true
			}
		case terminal[claim.WorkflowStepID]:
			if ok, rerr := c.capacity.ReleaseCapacityClaim(ctx, claim.DispatchKey, claim.LifecycleGeneration,
				"step reached a terminal state", c.clock()); rerr == nil && ok {
				freed = true
			}
		case claim.Kind != domain.ExecutionKindReviewer && claim.LifecycleGeneration < current:
			// Worker and repair claims are fenced on the step's ATTEMPT count,
			// which is what their generation counts. A reviewer's is not --
			// its generation is the review cycle -- so this rule deliberately
			// does not apply to it; the per-(step,kind) rule above is what
			// supersedes a reviewer.
			if ok, rerr := c.capacity.ReleaseCapacityClaim(ctx, claim.DispatchKey, claim.LifecycleGeneration,
				"superseded by a newer dispatch generation", c.clock()); rerr == nil && ok {
				freed = true
			}
		}
	}
	if freed {
		c.wakeQueuedCapacity(ctx)
	}
}

// SchedulerSnapshot is the observability surface: configured limits, live
// usage, the queue and what is holding each slot.
//
// It exposes claim identity, kind, run/step and generation, and deliberately
// nothing else -- no runtime tokens, no prompts, no provider credentials. A
// runtime handle and instance id are AO's own local names for a tmux session
// and are safe; they are what make an operator able to correlate a held slot
// with something they can see.
func (c *Coordinator) SchedulerSnapshot(ctx stdctx.Context) (domain.SchedulerSnapshot, error) {
	limits := c.effectiveCapacityLimits()
	out := domain.SchedulerSnapshot{Limits: limits, ObservedAt: c.clock()}
	if !c.capacityEnabled() {
		out.Global = domain.CapacityUsage{Limit: limits.Global}
		for _, kind := range domain.ExecutionKinds() {
			out.PerKind = append(out.PerKind, domain.CapacityUsage{Kind: kind, Limit: limits.LimitFor(kind)})
		}
		return out, nil
	}
	held, queued, total, err := c.capacity.CapacityUsageByKind(ctx)
	if err != nil {
		return domain.SchedulerSnapshot{}, err
	}
	totalQueued := 0
	for _, n := range queued {
		totalQueued += n
	}
	out.Global = domain.CapacityUsage{Limit: limits.Global, Held: total, Queued: totalQueued}
	for _, kind := range domain.ExecutionKinds() {
		out.PerKind = append(out.PerKind, domain.CapacityUsage{
			Kind: kind, Limit: limits.LimitFor(kind), Held: held[kind], Queued: queued[kind],
		})
	}
	if out.HeldClaims, err = c.capacity.ListHeldCapacityClaims(ctx); err != nil {
		return domain.SchedulerSnapshot{}, err
	}
	if out.QueuedFirst, err = c.capacity.ListQueuedCapacityClaims(ctx, 20); err != nil {
		return domain.SchedulerSnapshot{}, err
	}
	sort.SliceStable(out.PerKind, func(i, j int) bool { return out.PerKind[i].Kind < out.PerKind[j].Kind })
	return out, nil
}

// workerCapacityRequest builds the admission request for a work step's
// dispatch. The intent key is the outbox idempotency key the dispatch site
// already mints, so the claim and the launch it authorises share one identity.
//
// The generation is the attempt number this dispatch is about. A stale pass
// carrying an older attempt number therefore cannot claim, renew or release
// the current one.
func (c *Coordinator) workerCapacityRequest(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) capacityRequest {
	kind := domain.ExecutionKindWorker
	if c.isRepairRun(ctx, run) {
		kind = domain.ExecutionKindRepair
	}
	return capacityRequest{
		Kind: kind, Run: run, StepID: step.ID,
		Generation: c.stepDispatchGeneration(ctx, step.ID),
		IntentKey:  workStepOutboxIdempotencyKey(step.ID),
	}
}

// stepDispatchGeneration is the generation this dispatch is about: the attempt
// number it is going to produce.
//
// It is defined as "attempts recorded so far, plus one" so that it agrees
// EXACTLY with reconcileCapacityWithSteps' supersession rule, which releases a
// claim once the step's attempt count has moved past it. Two definitions of a
// generation would mean a claim released while its own launch was still
// running, or one held after the launch it paid for was replaced -- and both
// are ways to oversubscribe.
func (c *Coordinator) stepDispatchGeneration(ctx stdctx.Context, stepID string) int64 {
	attempts, err := c.store.ListWorkflowAttempts(ctx, stepID)
	if err != nil {
		return 1
	}
	return int64(len(attempts)) + 1
}

// reviewerCapacityRequest builds the admission request for a review dispatch.
// The generation is the review CYCLE, because that is what actually advances
// for a reviewer: one review step runs cycle 1, then a fix, then cycle 2, and
// each cycle is its own launch. Fencing on the step's attempt count instead
// left every cycle at generation 1, so nothing ever superseded anything and a
// single review step accumulated a held slot per cycle until the run hit its
// own per-workflow bound and parked itself.
func (c *Coordinator) reviewerCapacityRequest(run domain.WorkflowRun, step domain.WorkflowStep, intentKey string, cycleNumber int) capacityRequest {
	generation := int64(cycleNumber)
	if generation <= 0 {
		generation = 1
	}
	return capacityRequest{
		Kind: domain.ExecutionKindReviewer, Run: run, StepID: step.ID,
		Generation: generation, IntentKey: intentKey,
	}
}

// plannerCapacityRequest builds the admission request for a planner
// invocation. A planner claim belongs to the RUN: an objective has one plan
// step and one planner at a time, and the plan row's own status is what fences
// a second one.
// The retry count is its generation, which dispatchKey folds into the claim's
// identity -- so a planner retry is its own intent rather than a collision
// with the attempt it replaces.
func (c *Coordinator) plannerCapacityRequest(run domain.WorkflowRun, generation int64) capacityRequest {
	return capacityRequest{
		Kind: domain.ExecutionKindPlanner, Run: run, Generation: generation,
		IntentKey: "workflow-plan:" + run.ID,
	}
}

// repairRunOriginPhase marks a run created by P1-B's Repair Agent, so the
// scheduler can meter it as a repair rather than as an ordinary worker.
//
// It is a durable checkpoint on the repair run itself, written before the run
// is started, for the same reason the incident repair link is: it answers a
// question asked FROM that run ("why does this exist, and what should it be
// charged as"), and it must survive a restart.
const repairRunOriginPhase = "workflow_repair_run_origin"

// isRepairRun reports whether this run is a Repair Agent's own run.
//
// Fail-safe direction: a run whose ledger cannot be read is metered as an
// ordinary worker. Mis-metering a repair as a worker costs a worker slot;
// mis-metering a worker as a repair would let it past the repair limit, which
// is the narrower budget.
func (c *Coordinator) isRepairRun(ctx stdctx.Context, run domain.WorkflowRun) bool {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return false
	}
	for _, cp := range checkpoints {
		if cp.DurablePhase == repairRunOriginPhase {
			return true
		}
	}
	return false
}

// errNoRuntimeCapacity is the cause recorded when a launch is queued because
// the machine is full rather than because a provider is unavailable. It is a
// wait, never a failure: nothing retries on it and no budget is spent.
var errNoRuntimeCapacity = fmt.Errorf("no runtime execution slot is currently free")

// plannerClaimIsStale reports whether a run-scoped (planner) capacity claim
// can no longer correspond to a live planner.
//
// Fail-safe direction: a plan row that cannot be read is treated as NOT stale,
// so an unreadable state never frees a slot a planner might still be using.
// Holding a slot too long is a delay; freeing one that is in use is
// oversubscription.
func (c *Coordinator) plannerClaimIsStale(ctx stdctx.Context, run domain.WorkflowRun) bool {
	if c.planStore == nil {
		return true
	}
	plan, isMaster, err := c.planStore.GetWorkflowPlan(ctx, run.ID)
	if err != nil {
		return false
	}
	if !isMaster {
		return true
	}
	return plan.Status != domain.WorkflowPlanRunning
}

// stepKindKey groups a run's claims by the launch they belong to: one step,
// one kind of runtime.
type stepKindKey struct {
	stepID string
	kind   domain.ExecutionKind
}
