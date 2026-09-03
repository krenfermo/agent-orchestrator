package workflow

import (
	stdctx "context"
	"fmt"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// execution_placement.go — P1-D §A/§B: the FROZEN execution placement, and the
// generation that fences it.
//
// The defect this closes is small to state and large to live with. "Where does
// this task's work happen" used to be answered by reading the project's
// execution mode at the moment somebody asked. That is a derivation over
// MUTABLE configuration, and it has exactly one failure mode that matters:
//
//	a task starts in isolated_worktree, creates a worktree, writes code
//	  -> a person switches the project to direct_branch
//	  -> the next reconcile derives "direct branch", looks for a branch lock,
//	     finds no worktree it believes in, and recovers a run into a placement
//	     that never existed
//
// After the freeze, the STORED RECORD WINS. Project configuration is not
// consulted again for this obligation, and recovery reads the record rather
// than recomputing policy. Selection policy still runs — but only once, and
// only before anything has been mutated.
//
// # Two generations, deliberately
//
// The placement carries its OWN generation, separate from the task's lifecycle
// generation, because they supersede for different reasons:
//
//   - a LIFECYCLE generation advances when the obligation is retried;
//   - a PLACEMENT generation advances only when the physical placement itself
//     is replaced;
//   - a PROVIDER ATTEMPT advances neither (see provider_attempt.go).
//
// Collapsing them would mean every retry minted a new worktree, and every
// failover looked like new work.
//
// # What a stale placement generation may do
//
// Nothing. It may not acquire a branch lock, create or reuse a worktree, launch
// a worker, authorize a review/fix/verify, integrate, or GC a newer placement.
// That is not a list of call sites each remembering to check: it is
// requireCurrentPlacement, called from the one gate every launch passes
// (admission.go), plus explicit refusals on the integration and cleanup paths.

// ExecutionPlacements is the durable placement authority the coordinator
// depends on. Satisfied by *storage/sqlite/store.Store.
//
// Optional, like every other dependency in this package: a nil store means no
// placement is ever frozen, which is exactly the pre-P1-D behaviour every
// existing test double gets, and admission then reports placement readiness as
// "not enforced" rather than fabricating one.
type ExecutionPlacements interface {
	FreezeExecutionPlacement(ctx stdctx.Context, p domain.ExecutionPlacement) (bool, error)
	GetLiveExecutionPlacement(ctx stdctx.Context, runID, taskID, stepID string) (domain.ExecutionPlacement, bool, error)
	GetExecutionPlacement(ctx stdctx.Context, runID, taskID, stepID string, generation int64) (domain.ExecutionPlacement, bool, error)
	MaxExecutionPlacementGeneration(ctx stdctx.Context, runID, taskID, stepID string) (int64, error)
	TransitionExecutionPlacement(ctx stdctx.Context, runID, taskID, stepID string, generation int64, expected, next domain.ExecutionPlacementState, waitingReason, detail string, now time.Time) (bool, error)
	RecordExecutionPlacementPreparation(ctx stdctx.Context, runID, taskID, stepID string, generation int64, baseSHA, worktreePath, worktreeRecordID string, now time.Time) (bool, error)
	MarkExecutionPlacementIntegrated(ctx stdctx.Context, runID, taskID, stepID string, generation int64, integratedSHA string, now time.Time) (bool, error)
	RetireSupersededExecutionPlacements(ctx stdctx.Context, runID, taskID, stepID string, generation int64, detail string, now time.Time) (int64, error)
	ListExecutionPlacementsForRun(ctx stdctx.Context, runID string) ([]domain.ExecutionPlacement, error)
}

// placementScope is the obligation a placement belongs to.
//
// It is (run, task) rather than (run, task, step) on purpose. A placement must
// survive a step retry and a provider failover unchanged — those are new
// attempts at the SAME work in the SAME checkout — so binding it to a step id
// would advance the placement generation for exactly the events §I says must
// leave it alone. The step field exists because the schema keys on it and a
// future step-scoped placement would need it; today it is always empty.
type placementScope struct {
	runID  string
	taskID string
	stepID string
}

// placementScopeFor resolves the obligation a run's work belongs to. A child
// run of a master objective is placed by its planned task; a plain task run is
// placed by the run itself.
func placementScopeFor(run domain.WorkflowRun) placementScope {
	taskID := ""
	if run.PlannedTaskID != nil {
		taskID = *run.PlannedTaskID
	}
	return placementScope{runID: run.ID, taskID: taskID}
}

// placementEnabled reports whether the durable placement authority is wired.
func (c *Coordinator) placementEnabled() bool { return c.placements != nil }

// ErrPlacementUnprovable is the fail-closed answer for a run that has already
// executed but whose placement cannot be established from durable facts. AO
// will not guess it: guessing is how a direct branch gets written without a
// lock, and how a worktree with somebody's only copy of the work gets orphaned.
var ErrPlacementUnprovable = fmt.Errorf("%w: this run's execution placement cannot be established from durable facts", ErrInvalid)

// EnsureExecutionPlacement returns the frozen placement for a run's work,
// freezing one if this obligation does not have one yet.
//
// Three paths, and which one is taken is decided by evidence rather than by
// convenience:
//
//  1. A live placement exists. It is returned unchanged. Project configuration
//     is not consulted, which is the entire point of the freeze.
//  2. No placement, and the run has NOT executed anything yet. Selection policy
//     runs — project kind, project execution mode, the plan's per-task
//     downgrade — and the answer is frozen before anything is mutated.
//  3. No placement, and the run HAS executed. This is a legacy run, and its
//     placement is RECOVERED from durable facts or not at all. See
//     recoverLegacyPlacement.
func (c *Coordinator) EnsureExecutionPlacement(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.ExecutionPlacement, bool, error) {
	if !c.placementEnabled() {
		return domain.ExecutionPlacement{}, false, nil
	}
	scope := placementScopeFor(run)
	if live, found, err := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID); err != nil {
		return domain.ExecutionPlacement{}, false, err
	} else if found {
		return live, true, nil
	}

	proposed, err := c.selectExecutionPlacement(ctx, run, step, scope)
	if err != nil {
		return domain.ExecutionPlacement{}, false, err
	}
	created, err := c.placements.FreezeExecutionPlacement(ctx, proposed)
	if err != nil {
		return domain.ExecutionPlacement{}, false, err
	}
	if created {
		// P1-E §B.2: the request is discharged by the generation that consumed
		// it, so a standing override cannot be applied twice and the row names
		// which placement it produced. Only the pass that actually froze
		// resolves it -- a pass that lost the race consumed nothing.
		if pending, ok := c.pendingPlacementOverride(ctx, scope); ok {
			c.resolveConsumedOverride(ctx, pending, proposed.PlacementGeneration,
				"consumed by the freeze of placement generation "+strconv.FormatInt(proposed.PlacementGeneration, 10))
		}
	}
	if !created {
		// Another pass froze first. That pass's record is the authority; this
		// one adopts it rather than retrying with its own proposal, which is
		// what makes two racing dispatches converge on ONE worktree.
		live, found, gerr := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID)
		if gerr != nil {
			return domain.ExecutionPlacement{}, false, gerr
		}
		return live, found, nil
	}
	return proposed, true, nil
}

// selectExecutionPlacement computes the placement to freeze. It is the ONLY
// place project configuration is read for this purpose, and it runs at most
// once per obligation.
func (c *Coordinator) selectExecutionPlacement(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, scope placementScope) (domain.ExecutionPlacement, error) {
	generation, err := c.placements.MaxExecutionPlacementGeneration(ctx, scope.runID, scope.taskID, scope.stepID)
	if err != nil {
		return domain.ExecutionPlacement{}, err
	}
	generation++

	if c.runHasExecuted(ctx, step) {
		recovered, ok := c.recoverLegacyPlacement(ctx, run, scope)
		if !ok {
			return domain.ExecutionPlacement{}, ErrPlacementUnprovable
		}
		recovered.PlacementGeneration = generation
		recovered.LifecycleGeneration = c.stepDispatchGeneration(ctx, step.ID)
		recovered.ID = "plc-" + c.newID()
		recovered.OwnerToken = c.placementOwnerToken()
		recovered.CreatedAt, recovered.UpdatedAt = c.clock(), c.clock()
		return recovered, nil
	}

	project, ok, err := c.projects.GetProject(ctx, run.ProjectID)
	if err != nil {
		return domain.ExecutionPlacement{}, err
	}
	if !ok {
		return domain.ExecutionPlacement{}, fmt.Errorf("%w: project %q for run %q", ErrNotFound, run.ProjectID, run.ID)
	}
	mode := domain.ResolveExecutionMode(project.Kind, project.Config)
	if scope.taskID != "" {
		if taskScope, found := c.taskScopeFor(ctx, run, scope.taskID); found {
			mode = domain.ResolveTaskExecutionMode(mode, taskScope)
		}
	}
	placementType := domain.PlacementTypeForExecutionMode(mode)
	// P1-E §B.1: an operator's per-task override is an input to SELECTION, and
	// only to selection. It is consulted here -- once, before anything is
	// mutated -- and never again: after the freeze the stored record wins, so a
	// request that arrives later changes nothing until a transition consumes it.
	if pending, ok := c.pendingPlacementOverride(ctx, scope); ok && pending.Requested.Explicit() {
		placementType = pending.Requested.PlacementType()
	}
	target := project.Config.WithDefaults().DefaultBranch

	placement := domain.ExecutionPlacement{
		ID:                  "plc-" + c.newID(),
		WorkflowRunID:       scope.runID,
		TaskID:              scope.taskID,
		WorkflowStepID:      scope.stepID,
		ProjectID:           run.ProjectID,
		PlacementGeneration: generation,
		LifecycleGeneration: c.stepDispatchGeneration(ctx, step.ID),
		Type:                placementType,
		RepoPath:            project.Path,
		BaseBranch:          target,
		MergeTarget:         target,
		OwnerToken:          c.placementOwnerToken(),
		State:               domain.PlacementSelected,
		Provenance:          domain.PlacementFrozenAtSelection,
		CreatedAt:           c.clock(),
		UpdatedAt:           c.clock(),
	}
	if placementType.Isolated() {
		// The ao/* branch name is derived, not stored-only, and it is derived
		// HERE so the frozen record names the branch before it exists. The
		// worktree PATH is deliberately not: at freeze time the checkout does
		// not exist, so a path would be a plan recorded as a fact.
		placement.ExecutionBranch = placementExecutionBranch(scope, generation)
	} else {
		placement.ExecutionBranch = target
	}
	return placement, nil
}

// placementTaskKey is the task identity a derived branch name uses. A plain
// task run has no planned task, and naming its branch after the run is what
// keeps two runs on one project from sharing an ao/* branch.
func placementTaskKey(scope placementScope) string {
	if scope.taskID != "" {
		return scope.taskID
	}
	return scope.runID
}

// placementOwnerToken names the daemon instance that froze a placement, which
// is what makes restart reconciliation decidable without guessing from
// timestamps. It is AO's own local identifier and carries no secret.
func (c *Coordinator) placementOwnerToken() string {
	return "ao-placement:" + c.instanceToken
}

// runHasExecuted reports whether this obligation has already run something.
//
// It is the legacy test, and it is deliberately generous in the direction of
// caution: any recorded attempt, or a step that already names a session, means
// the run predates its placement record and the placement must be RECOVERED
// rather than re-selected. Selecting a fresh placement for a run that has
// already written code is precisely the config-drift bug this file exists to
// close.
func (c *Coordinator) runHasExecuted(ctx stdctx.Context, step domain.WorkflowStep) bool {
	if step.SessionID != nil && *step.SessionID != "" {
		return true
	}
	if attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID); err == nil && len(attempts) > 0 {
		return true
	}
	return false
}

// recoverLegacyPlacement reconstructs a placement for a run that executed
// before placements were durable — but ONLY from facts that PROVE the mode.
//
// Two proofs, and nothing else counts:
//
//   - an AO worktree record for this task proves isolated_worktree. AO does not
//     create those in any other mode, and the record carries the path, the
//     branch, the base commit and the target, so nothing is invented.
//   - a branch lock held by this run proves direct_branch. AO does not take one
//     in any other mode.
//
// A run with NEITHER is ambiguous, and ambiguous fails closed: the caller gets
// ErrPlacementUnprovable and the run stops for a person rather than being given
// a placement AO guessed. A run with BOTH is also ambiguous — that shape means
// the project's mode changed under a live run, which is the exact situation the
// freeze exists to make impossible and the exact situation nobody should
// resolve by picking one.
func (c *Coordinator) recoverLegacyPlacement(ctx stdctx.Context, run domain.WorkflowRun, scope placementScope) (domain.ExecutionPlacement, bool) {
	isolated, hasWorktree := c.legacyWorktreeEvidence(ctx, run, scope)
	direct, hasLock := c.legacyBranchLockEvidence(ctx, run)
	switch {
	case hasWorktree && hasLock:
		return domain.ExecutionPlacement{}, false
	case hasWorktree:
		isolated.Provenance = domain.PlacementRecoveredFromDurableFacts
		return isolated, true
	case hasLock:
		direct.Provenance = domain.PlacementRecoveredFromDurableFacts
		return direct, true
	default:
		return domain.ExecutionPlacement{}, false
	}
}

// legacyWorktreeEvidence reads an existing AO worktree record as proof of an
// isolated placement. Everything on the returned record is copied from the
// worktree row; nothing is derived from current configuration.
func (c *Coordinator) legacyWorktreeEvidence(ctx stdctx.Context, run domain.WorkflowRun, scope placementScope) (domain.ExecutionPlacement, bool) {
	if c.taskWorktreeRecords == nil {
		return domain.ExecutionPlacement{}, false
	}
	lookupRun := run.ID
	if run.ParentWorkflowID != nil && *run.ParentWorkflowID != "" {
		lookupRun = *run.ParentWorkflowID
	}
	records, err := c.taskWorktreeRecords.ListTaskWorktreesByRun(ctx, lookupRun)
	if err != nil {
		return domain.ExecutionPlacement{}, false
	}
	key := placementTaskKey(scope)
	for _, rec := range records {
		if rec.TaskID != key && rec.TaskID != scope.taskID {
			continue
		}
		if rec.Path == "" || rec.Branch == "" || rec.RepoPath == "" {
			// A record that cannot name what it created is not proof of
			// anything. Fabricating the missing half is what §A forbids.
			continue
		}
		return domain.ExecutionPlacement{
			WorkflowRunID: scope.runID, TaskID: scope.taskID, WorkflowStepID: scope.stepID,
			ProjectID: run.ProjectID, Type: domain.PlacementIsolatedWorktree,
			RepoPath: rec.RepoPath, BaseBranch: rec.TargetBranch, BaseSHA: rec.BaseSHA,
			ExecutionBranch: rec.Branch, WorktreePath: rec.Path, WorktreeRecordID: rec.TaskID,
			MergeTarget: rec.TargetBranch, State: domain.PlacementActive,
		}, true
	}
	return domain.ExecutionPlacement{}, false
}

// legacyBranchLockEvidence reads a lock this run holds as proof of a
// direct-branch placement. AO takes one in no other mode.
func (c *Coordinator) legacyBranchLockEvidence(ctx stdctx.Context, run domain.WorkflowRun) (domain.ExecutionPlacement, bool) {
	if c.branchLocks == nil {
		return domain.ExecutionPlacement{}, false
	}
	locks, err := c.branchLocks.HeldByRun(ctx, run.ID)
	if err != nil || len(locks) == 0 {
		return domain.ExecutionPlacement{}, false
	}
	lock := locks[0]
	if lock.RepoPath == "" || lock.Branch == "" {
		return domain.ExecutionPlacement{}, false
	}
	return domain.ExecutionPlacement{
		WorkflowRunID: run.ID, ProjectID: run.ProjectID, Type: domain.PlacementDirectBranch,
		RepoPath: lock.RepoPath, BaseBranch: lock.Branch, BaseSHA: lock.BaseSHA,
		ExecutionBranch: lock.Branch, MergeTarget: lock.Branch,
		State: domain.PlacementActive,
	}, true
}

// taskScopeFor reads a planned task's durable scope, which is where the
// planner's per-task execution-mode downgrade lives.
func (c *Coordinator) taskScopeFor(ctx stdctx.Context, run domain.WorkflowRun, taskID string) (domain.WorkflowTaskScope, bool) {
	lookupRun := run.ID
	if run.ParentWorkflowID != nil && *run.ParentWorkflowID != "" {
		lookupRun = *run.ParentWorkflowID
	}
	if c.planStore == nil {
		return domain.WorkflowTaskScope{}, false
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, lookupRun)
	if err != nil {
		return domain.WorkflowTaskScope{}, false
	}
	for _, task := range tasks {
		if task.ID != taskID {
			continue
		}
		scope, err := UnmarshalTaskScope(task.ScopeJSON)
		if err != nil {
			return domain.WorkflowTaskScope{}, false
		}
		return scope, true
	}
	return domain.WorkflowTaskScope{}, false
}

// PlacementIsCurrent reports whether a placement generation is still the
// newest recorded for its obligation.
//
// It is the single staleness predicate, and it reads the MAXIMUM generation
// rather than the live row: a placement that has been superseded and whose
// successor has itself finished leaves no live row at all, and a stale holder
// must still be refused in that state. Fail-closed on a read error — an
// unreadable placement authority means no launch, never a launch.
func (c *Coordinator) PlacementIsCurrent(ctx stdctx.Context, scope placementScope, generation int64) bool {
	if !c.placementEnabled() {
		return true
	}
	newest, err := c.placements.MaxExecutionPlacementGeneration(ctx, scope.runID, scope.taskID, scope.stepID)
	if err != nil {
		return false
	}
	return generation > 0 && generation >= newest
}

// requireCurrentPlacement is the guard every authority-bearing operation calls.
//
// It answers with the placement itself so the caller uses the record it was
// authorized against, rather than re-reading one that may have moved between
// the check and the use.
func (c *Coordinator) requireCurrentPlacement(ctx stdctx.Context, run domain.WorkflowRun, generation int64) (domain.ExecutionPlacement, error) {
	if !c.placementEnabled() {
		return domain.ExecutionPlacement{}, nil
	}
	scope := placementScopeFor(run)
	if !c.PlacementIsCurrent(ctx, scope, generation) {
		return domain.ExecutionPlacement{}, fmt.Errorf("%w: placement generation %d has been superseded", ErrInvalid, generation)
	}
	placement, found, err := c.placements.GetExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID, generation)
	if err != nil {
		return domain.ExecutionPlacement{}, err
	}
	if !found {
		return domain.ExecutionPlacement{}, fmt.Errorf("%w: no placement generation %d for run %s", ErrNotFound, generation, run.ID)
	}
	return placement, nil
}

// transitionPlacement is the CAS every placement state change goes through.
// Repeated reconciliation is idempotent by construction: a transition whose
// expected state no longer holds matches zero rows and reports false, which
// every caller reads as "somebody already did this" rather than as a failure.
func (c *Coordinator) transitionPlacement(ctx stdctx.Context, placement domain.ExecutionPlacement, expected, next domain.ExecutionPlacementState, waitingReason, detail string) bool {
	if !c.placementEnabled() {
		return false
	}
	ok, err := c.placements.TransitionExecutionPlacement(ctx,
		placement.WorkflowRunID, placement.TaskID, placement.WorkflowStepID, placement.PlacementGeneration,
		expected, next, waitingReason, detail, c.clock())
	if err != nil && c.log != nil {
		c.log.Warn("workflow: placement transition failed", "run", placement.WorkflowRunID, "gen", placement.PlacementGeneration, "err", err)
	}
	return ok
}

// ReplaceExecutionPlacement mints a NEW placement generation for an obligation
// whose physical placement must change, and retires the old one.
//
// This is the only way a placement type or location changes after a freeze, and
// it is explicit for the reason §I gives: a provider failover must NOT come
// through here. Once it returns, the old generation is terminal, so every
// guard in this file refuses the previous holder — it cannot lock, launch,
// integrate or GC the replacement.
func (c *Coordinator) ReplaceExecutionPlacement(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, reason string) (domain.ExecutionPlacement, error) {
	if !c.placementEnabled() {
		return domain.ExecutionPlacement{}, fmt.Errorf("%w: no placement authority is wired", ErrInvalid)
	}
	scope := placementScopeFor(run)
	now := c.clock()
	newest, err := c.placements.MaxExecutionPlacementGeneration(ctx, scope.runID, scope.taskID, scope.stepID)
	if err != nil {
		return domain.ExecutionPlacement{}, err
	}
	// Retire first. The live partial unique index admits at most one
	// non-terminal placement per obligation, so the old row has to become
	// terminal before the new one can exist -- which also means a crash between
	// the two leaves NO live placement rather than two, and the next pass
	// freezes cleanly.
	if _, err := c.placements.RetireSupersededExecutionPlacements(ctx,
		scope.runID, scope.taskID, scope.stepID, newest+1, reason, now); err != nil {
		return domain.ExecutionPlacement{}, err
	}
	proposed, err := c.selectExecutionPlacement(ctx, run, step, scope)
	if err != nil {
		return domain.ExecutionPlacement{}, err
	}
	if _, err := c.placements.FreezeExecutionPlacement(ctx, proposed); err != nil {
		return domain.ExecutionPlacement{}, err
	}
	return proposed, nil
}

// PlacementView is the read-side projection of a run's placements, for the API,
// the CLI and recovery output. It exposes identity and state and deliberately
// no token.
type PlacementView struct {
	Type                domain.ExecutionPlacementType
	PlacementGeneration int64
	LifecycleGeneration int64
	State               domain.ExecutionPlacementState
	Provenance          domain.PlacementProvenance
	TaskID              string
	RepoPath            string
	BaseBranch          string
	BaseSHA             string
	ExecutionBranch     string
	WorktreePath        string
	MergeTarget         string
	IntegratedSHA       string
	WaitingReason       string
	Detail              string
	Current             bool
}

// ListPlacements returns every placement recorded for a run, newest generation
// per obligation flagged as current.
func (c *Coordinator) ListPlacements(ctx stdctx.Context, runID string) ([]PlacementView, error) {
	if !c.placementEnabled() {
		return nil, nil
	}
	records, err := c.placements.ListExecutionPlacementsForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	newest := map[placementScope]int64{}
	for _, r := range records {
		key := placementScope{r.WorkflowRunID, r.TaskID, r.WorkflowStepID}
		if r.PlacementGeneration > newest[key] {
			newest[key] = r.PlacementGeneration
		}
	}
	out := make([]PlacementView, 0, len(records))
	for _, r := range records {
		key := placementScope{r.WorkflowRunID, r.TaskID, r.WorkflowStepID}
		out = append(out, PlacementView{
			Type: r.Type, PlacementGeneration: r.PlacementGeneration,
			LifecycleGeneration: r.LifecycleGeneration, State: r.State, Provenance: r.Provenance,
			TaskID: r.TaskID, RepoPath: r.RepoPath, BaseBranch: r.BaseBranch, BaseSHA: r.BaseSHA,
			ExecutionBranch: r.ExecutionBranch, WorktreePath: r.WorktreePath,
			MergeTarget: r.MergeTarget, IntegratedSHA: r.IntegratedSHA,
			WaitingReason: r.WaitingReason, Detail: r.Detail,
			Current: r.PlacementGeneration == newest[key],
		})
	}
	return out, nil
}

// activateAdmittedPlacement moves a placement a launch has just been admitted
// for into `active`, and records the worktree identity if one exists by now.
//
// Best-effort with respect to the dispatch: a placement transition that loses
// its CAS means another pass got there first, which is the correct outcome and
// not a reason to fail a launch that admission already authorized. What it must
// never do is move a placement generation this pass is not entitled to, and it
// cannot: every write is conditioned on the exact generation the admission
// decision named.
func (c *Coordinator) activateAdmittedPlacement(ctx stdctx.Context, run domain.WorkflowRun, decision domain.AdmissionDecision) {
	if !c.placementEnabled() || decision.PlacementGeneration <= 0 {
		return
	}
	scope := placementScopeFor(run)
	placement, found, err := c.placements.GetExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID, decision.PlacementGeneration)
	if err != nil || !found {
		return
	}
	c.adoptWorktreeIdentityForPlacement(ctx, run, placement)
	if placement.State == domain.PlacementActive {
		return
	}
	c.transitionPlacement(ctx, placement, placement.State, domain.PlacementActive, "", "a launch was admitted for this placement")
}

// adoptWorktreeIdentityForPlacement copies the durable worktree record's path,
// branch and base commit onto an isolated placement once the checkout actually
// exists.
//
// The placement is frozen before the worktree is created — that ordering is the
// whole point, so a crash mid-creation leaves evidence of what was intended —
// which means the path is genuinely not a fact at freeze time. This is where it
// becomes one. Idempotent, and conditioned on the generation, so a stale pass
// cannot describe a replacement placement's checkout.
func (c *Coordinator) adoptWorktreeIdentityForPlacement(ctx stdctx.Context, run domain.WorkflowRun, placement domain.ExecutionPlacement) {
	if !c.placementEnabled() || !placement.Type.Isolated() || c.taskWorktreeRecords == nil {
		return
	}
	if placement.WorktreePath != "" && placement.BaseSHA != "" {
		return
	}
	lookupRun := run.ID
	if run.ParentWorkflowID != nil && *run.ParentWorkflowID != "" {
		lookupRun = *run.ParentWorkflowID
	}
	records, err := c.taskWorktreeRecords.ListTaskWorktreesByRun(ctx, lookupRun)
	if err != nil {
		return
	}
	key := placementTaskKey(placementScopeFor(run))
	for _, rec := range records {
		if rec.TaskID != key && rec.TaskID != placement.TaskID {
			continue
		}
		if rec.Path == "" {
			continue
		}
		if _, err := c.placements.RecordExecutionPlacementPreparation(ctx,
			placement.WorkflowRunID, placement.TaskID, placement.WorkflowStepID, placement.PlacementGeneration,
			rec.BaseSHA, rec.Path, rec.TaskID, c.clock()); err != nil && c.log != nil {
			c.log.Debug("workflow: could not record placement preparation", "run", run.ID, "err", err)
		}
		return
	}
}

// retirePlacementsForTerminalRun retires everything a run that has just
// reached a terminal state still holds.
//
// P1-E §O found the gap this closes. A terminal run returned its capacity slots
// at the instant it finished (P1-C, completeRun/CancelRun) but its PLACEMENT was
// only retired by reconcilePlacementsForRun, which is reached from
// Coordinator.Reconcile — and the daemon runs that at BOOT. On a long-lived
// daemon the retirement therefore never happened during ordinary operation, so
// a finished run's placement sat `active` forever.
//
// That is not merely untidy. The placement sweep will only remove an isolated
// checkout when the record says `integrated` and names the commit its work
// landed at, so a placement stuck in `active` is one the sweep is right to
// refuse — and every run that completed without integrating (a direct-branch
// run, a read-only task, a cancelled run) left an AO worktree that nothing
// would ever collect until the next restart.
//
// The fix is the argument P1-C already made for capacity, applied to the other
// authority: a run that is over releases what it holds at the moment it is
// over, not at the next boot. It reuses reconcilePlacementsForRun rather than
// repeating its rules, so "preserved when the work never landed, terminal
// otherwise" has exactly one definition. Best-effort and idempotent: every
// write inside is a CAS on the state it read, so a second pass matches nothing
// and a failure costs a retirement the next reconcile performs anyway.
func (c *Coordinator) retirePlacementsForTerminalRun(ctx stdctx.Context, runID string, state domain.WorkflowRunState) {
	if !c.placementEnabled() || runID == "" || !state.Terminal() {
		return
	}
	run, found, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil || !found {
		if err != nil && c.log != nil {
			c.log.Warn("workflow: could not read a terminal run to retire its placements", "run", runID, "err", err)
		}
		return
	}
	// The caller has just CAS'd the transition, so the row may or may not have
	// been re-read since. Use the state the caller proved rather than whatever
	// the read returned, so a retirement can never be skipped because of a
	// stale read of the very transition that triggered it.
	run.State = state
	c.reconcilePlacementsForRun(ctx, run)
}

// reconcilePlacementsForRun is the recovery half, run once per run per
// reconciliation pass.
//
// Two rules, and both are refusals rather than repairs:
//
//   - a TERMINAL run's outstanding placements are retired. Nothing will ever
//     launch into them again, and a live placement for a finished run would
//     keep the obligation's live index occupied forever.
//   - a placement whose generation has been superseded is retired. That is the
//     stale-writer window: a pass that crashed mid-replacement leaves an old
//     generation non-terminal, and leaving it there would let the old holder's
//     guards keep passing.
//
// It never re-selects a placement from project configuration. That would be the
// exact config-drift bug the freeze exists to close, performed by the very pass
// meant to protect against it.
func (c *Coordinator) reconcilePlacementsForRun(ctx stdctx.Context, run domain.WorkflowRun) {
	if !c.placementEnabled() {
		return
	}
	records, err := c.placements.ListExecutionPlacementsForRun(ctx, run.ID)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not reconcile a run's placements", "run", run.ID, "err", err)
		}
		return
	}
	newest := map[placementScope]int64{}
	for _, r := range records {
		key := placementScope{r.WorkflowRunID, r.TaskID, r.WorkflowStepID}
		if r.PlacementGeneration > newest[key] {
			newest[key] = r.PlacementGeneration
		}
	}
	now := c.clock()
	for _, r := range records {
		if r.State.Terminal() {
			continue
		}
		key := placementScope{r.WorkflowRunID, r.TaskID, r.WorkflowStepID}
		switch {
		case run.State.Terminal():
			// Preserved, not merely terminal, when the work never landed: the
			// agent's commits on that branch may be the only copy, and a run
			// ending badly is exactly when somebody wants to read them.
			next := domain.PlacementTerminal
			if r.Type.Isolated() && r.IntegratedSHA == "" {
				next = domain.PlacementPreserved
			}
			_, _ = c.placements.TransitionExecutionPlacement(ctx, r.WorkflowRunID, r.TaskID, r.WorkflowStepID,
				r.PlacementGeneration, r.State, next, "", "the run reached "+string(run.State), now)
		case r.PlacementGeneration < newest[key]:
			_, _ = c.placements.TransitionExecutionPlacement(ctx, r.WorkflowRunID, r.TaskID, r.WorkflowStepID,
				r.PlacementGeneration, r.State, domain.PlacementTerminal, "",
				"superseded by a newer placement generation", now)
		}
	}
}

// frozenPlacementTarget is the placement type a launch must materialise, and
// the branch it must check out there.
//
// It reads the LIVE record and never re-derives selection policy: after the
// freeze the stored record is the authority, and asking project configuration
// again at launch time is exactly the drift P1-D closed for recovery and P3-A
// closes for the workspace router.
//
// An empty answer means "no placement authority is wired, or none is frozen
// yet", and the workspace router then falls back to project configuration —
// which is the pre-P3-A behaviour. It is deliberately not defaulted to
// isolated: guessing a placement is how a direct branch gets written without a
// lock, and how an explicit branch choice becomes a worktree nobody asked for.
func (c *Coordinator) frozenPlacementTarget(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.ExecutionPlacementType, string) {
	if !c.placementEnabled() {
		return "", ""
	}
	placement, ok, err := c.EnsureExecutionPlacement(ctx, run, step)
	if err != nil || !ok {
		return "", ""
	}
	if !placement.Type.IsKnown() {
		return "", ""
	}
	return placement.Type, placement.ExecutionBranch
}
