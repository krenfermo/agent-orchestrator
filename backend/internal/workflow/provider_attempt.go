package workflow

import (
	stdctx "context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// provider_attempt.go — P1-D §F/§G/§H/§I/§J: the durable provider-attempt
// ledger, its state machine, and mid-execution failover after PROVEN no
// mutation.
//
// P1-D's first pass gave the four safety classes a name. A name is not enough:
// a projection computed at read time cannot answer, after a restart, "which
// provider is authoritative for this obligation right now, and what did the
// previous one prove before it stopped". Those are the two questions a failover
// turns on, so they are rows.
//
// # The distinction the whole file exists to hold
//
//	A PROVIDER ATTEMPT IS NOT A TASK GENERATION.
//
// The run, the step, the task and the lifecycle generation are the OBLIGATION,
// and a failover leaves every one of them exactly where it was. The frozen
// execution placement is likewise untouched (§I): provider B inherits provider
// A's worktree, because the authority over the checkout never moved — only the
// attempt to discharge the obligation did. Minting a new placement because the
// provider changed would throw away a worktree that provably holds nothing
// wrong, and would make every failover look like new work.
//
// # What a stale attempt may do
//
// Nothing. It may not launch, mutate, release its successor's capacity, release
// its successor's branch authority, write a completion, become authoritative
// again, or authorize a review or verification. That is enforced in two places
// at once, on purpose: `ProviderAttemptState.Authoritative()` answers false for
// every terminal state, and every write in the store CASes on the exact attempt
// id AND its expected state, so a stale writer matches zero rows even if a
// caller forgot to ask.

// ProviderAttemptLedger is the durable provider-attempt surface the coordinator
// depends on. Satisfied by *storage/sqlite/store.Store. Optional: a nil ledger
// means attempts are not recorded, which is the pre-P1-D behaviour, and
// failover then falls back to the structural rule dispatch.go already enforces.
type ProviderAttemptLedger interface {
	CreateProviderAttempt(ctx stdctx.Context, a domain.ProviderAttempt) (bool, error)
	GetProviderAttempt(ctx stdctx.Context, id string) (domain.ProviderAttempt, bool, error)
	GetAuthoritativeProviderAttempt(ctx stdctx.Context, runID, stepID string, lifecycleGeneration int64) (domain.ProviderAttempt, bool, error)
	MaxProviderAttemptOrdinal(ctx stdctx.Context, runID, stepID string, lifecycleGeneration int64) (int64, error)
	TransitionProviderAttempt(ctx stdctx.Context, id string, expected, next domain.ProviderAttemptState, reason string, class domain.WorkflowErrorClass, safety domain.FailoverSafety, evidence string, now time.Time) (bool, error)
	BindProviderAttemptRuntime(ctx stdctx.Context, id, sessionID, capacityClaimID string, now time.Time) (bool, error)
	LinkProviderAttemptSuccessor(ctx stdctx.Context, id, successorID string, now time.Time) (bool, error)
	ListProviderAttemptsForObligation(ctx stdctx.Context, runID, stepID string, lifecycleGeneration int64) ([]domain.ProviderAttempt, error)
	ListProviderAttemptsForRun(ctx stdctx.Context, runID string) ([]domain.ProviderAttempt, error)
	AbandonProviderAttemptsForRun(ctx stdctx.Context, runID, reason string, now time.Time) (int64, error)
}

// providerAttemptsEnabled reports whether the ledger is wired.
func (c *Coordinator) providerAttemptsEnabled() bool { return c.providerAttempts != nil }

// EnsureProviderAttempt returns the attempt currently entitled to discharge one
// obligation, creating the FIRST one if none exists.
//
// It deliberately does not create a second: a second live attempt is a
// failover, and a failover has to pass the safety gate in
// FailoverProviderAttempt. This function only ever mints ordinal 1, which is
// what makes "a provider attempt appears only through the front door or the
// safety gate" true by construction rather than by review.
//
// Idempotent: a repeated dispatch, a wake, or a restart finds the existing
// attempt and returns it, so the ledger records launches rather than passes.
func (c *Coordinator) EnsureProviderAttempt(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	placement domain.ExecutionPlacement, harness domain.AgentHarness, profile domain.ProviderProfileID,
) (domain.ProviderAttempt, bool, error) {
	if !c.providerAttemptsEnabled() {
		return domain.ProviderAttempt{}, false, nil
	}
	generation := c.stepDispatchGeneration(ctx, step.ID)
	if existing, found, err := c.providerAttempts.GetAuthoritativeProviderAttempt(ctx, run.ID, step.ID, generation); err != nil {
		return domain.ProviderAttempt{}, false, err
	} else if found {
		return existing, true, nil
	}
	// A terminal chain with no authoritative member means every attempt this
	// obligation was allowed has been spent. Minting a fresh ordinal-1 attempt
	// here would silently reset the failover budget on every reconcile, which
	// is precisely the A->B->A loop §J forbids.
	ordinal, err := c.providerAttempts.MaxProviderAttemptOrdinal(ctx, run.ID, step.ID, generation)
	if err != nil {
		return domain.ProviderAttempt{}, false, err
	}
	if ordinal > 0 {
		return domain.ProviderAttempt{}, false, nil
	}
	now := c.clock()
	attempt := domain.ProviderAttempt{
		ID: "pa-" + c.newID(), WorkflowRunID: run.ID, WorkflowStepID: step.ID,
		TaskID: placement.TaskID, ProjectID: run.ProjectID,
		LifecycleGeneration: generation, PlacementGeneration: placement.PlacementGeneration,
		Ordinal: 1, Provider: harness, Profile: profile,
		State: domain.ProviderAttemptPlanned, CreatedAt: now, UpdatedAt: now,
	}
	created, err := c.providerAttempts.CreateProviderAttempt(ctx, attempt)
	if err != nil {
		return domain.ProviderAttempt{}, false, err
	}
	if !created {
		// Another pass created it first; that row is the authority.
		return c.providerAttempts.GetAuthoritativeProviderAttempt(ctx, run.ID, step.ID, generation)
	}
	return attempt, true, nil
}

// advanceProviderAttempt CASes one attempt forward. It is best-effort with
// respect to the caller — a ledger write that fails must not turn a successful
// launch into a failure — but it reports whether the transition actually
// happened, because a caller that needs authority (rather than a record) reads
// that result.
func (c *Coordinator) advanceProviderAttempt(ctx stdctx.Context, attempt domain.ProviderAttempt, next domain.ProviderAttemptState, reason string, class domain.WorkflowErrorClass, safety domain.FailoverSafety, evidence string) bool {
	if !c.providerAttemptsEnabled() || attempt.ID == "" {
		return false
	}
	ok, err := c.providerAttempts.TransitionProviderAttempt(ctx, attempt.ID, attempt.State, next, reason, class, safety, evidence, c.clock())
	if err != nil && c.log != nil {
		c.log.Warn("workflow: provider attempt transition failed", "attempt", attempt.ID, "to", next, "err", err)
	}
	return ok
}

// bindProviderAttemptRuntime records which runtime and which capacity claim an
// attempt launched under, so §K's tuple is reconstructable after a restart.
func (c *Coordinator) bindProviderAttemptRuntime(ctx stdctx.Context, attempt domain.ProviderAttempt, sessionID, capacityClaimID string) {
	if !c.providerAttemptsEnabled() || attempt.ID == "" {
		return
	}
	if _, err := c.providerAttempts.BindProviderAttemptRuntime(ctx, attempt.ID, sessionID, capacityClaimID, c.clock()); err != nil && c.log != nil {
		c.log.Debug("workflow: could not bind runtime to provider attempt", "attempt", attempt.ID, "err", err)
	}
}

// ProviderFailoverBudget reads the DURABLE failover budget for one obligation:
// the policy's preferred provider and fallback order, its maximum hop count,
// and how many hops have actually been recorded.
//
// The ordinal comes from the ledger rather than from memory, which is the
// entire §J requirement: a restart re-reads the highest ordinal ever written,
// so a budget cannot be reset by rebooting, and Codex->Claude->Codex cannot
// loop by forgetting it already went to Codex once.
func (c *Coordinator) ProviderFailoverBudget(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) domain.ProviderFailoverBudget {
	policy := policyForRun(run)
	budget := domain.ProviderFailoverBudget{
		// MaxWorkProviderAttempts counts ATTEMPTS, and a hop budget is one
		// fewer: two permitted attempts is one permitted failover.
		MaxFailovers: effectiveMaxWorkProviderAttempts(policy) - 1,
	}
	if budget.MaxFailovers < 0 {
		budget.MaxFailovers = 0
	}
	owner := c.runOwner(ctx, run.ID)
	snapshot := policy.EffectiveExecutionPolicy()
	userPolicy, eligible, _, _ := c.routingInputsForRole(ctx, owner, domain.WorkflowRoleWorker, snapshot)
	for _, id := range userPolicy.WorkerPriority {
		profile, ok := eligible[id]
		if !ok {
			continue
		}
		if budget.Preferred == "" {
			budget.Preferred = profile.Harness
			continue
		}
		if profile.Harness != budget.Preferred {
			budget.FallbackOrder = append(budget.FallbackOrder, profile.Harness)
		}
	}
	if c.providerAttemptsEnabled() {
		if n, err := c.providerAttempts.MaxProviderAttemptOrdinal(ctx, run.ID, step.ID, c.stepDispatchGeneration(ctx, step.ID)); err == nil {
			budget.CurrentOrdinal = int(n)
		}
	}
	return budget
}

// FailoverProviderAttempt is the ONE gate a provider hop passes through.
//
// The order of the checks is the safety model, and it is not negotiable:
//
//  1. the safety class must PERMIT failover. Ambiguous execution and completed
//     execution never do, whatever the error class says and whatever a
//     workspace probe would report right now.
//  2. the current attempt must still be authoritative. A stale attempt cannot
//     hand its obligation on -- it does not hold it.
//  3. the durable budget must permit another hop, read from the ledger so a
//     restart cannot refill it.
//  4. the destination must not be a provider this obligation has already tried,
//     which is what stops A->B->A independently of the count.
//
// Only then is the current attempt made terminal (failed_safe), a successor
// created at the next ordinal, and the two linked. The successor carries the
// SAME placement generation and the SAME lifecycle generation, because §I says
// a safe failover keeps its placement and §F says the obligation does not move.
func (c *Coordinator) FailoverProviderAttempt(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	current domain.ProviderAttempt, to domain.AgentHarness, profile domain.ProviderProfileID,
	safety domain.FailoverSafety, class domain.WorkflowErrorClass, reason, evidence string,
) (domain.ProviderAttempt, bool, error) {
	if !c.providerAttemptsEnabled() {
		return domain.ProviderAttempt{}, false, nil
	}
	if !safety.PermitsFailover() {
		// Terminal, and terminal in the shape that says why. An ambiguous
		// attempt is recorded as failed_ambiguous so that every later reader --
		// recovery, the API, an operator -- sees a refusal AO made on purpose
		// rather than a hop that mysteriously did not happen.
		next := domain.ProviderAttemptFailedAmbiguous
		if safety == domain.FailoverCompletedExecution {
			next = domain.ProviderAttemptCompleted
		}
		c.advanceProviderAttempt(ctx, current, next, reason, class, safety, evidence)
		return domain.ProviderAttempt{}, false, nil
	}
	if !current.State.Authoritative() {
		return domain.ProviderAttempt{}, false, nil
	}
	if safety == domain.FailoverSafeAfterProvenNoMutation && evidence == "" {
		// The class that requires positive proof may not be claimed without
		// carrying it. Failing closed here rather than trusting the caller is
		// what keeps "git status looks clean right now" from becoming a proof.
		return domain.ProviderAttempt{}, false, fmt.Errorf("%w: safe_after_proven_no_mutation requires mutation evidence", ErrInvalid)
	}
	if to == "" || to == current.Provider {
		return domain.ProviderAttempt{}, false, nil
	}

	budget := c.ProviderFailoverBudget(ctx, run, step)
	if budget.Exhausted() {
		c.advanceProviderAttempt(ctx, current, domain.ProviderAttemptFailedSafe,
			reason+" (failover budget exhausted)", class, safety, evidence)
		return domain.ProviderAttempt{}, false, nil
	}

	attempts, err := c.providerAttempts.ListProviderAttemptsForObligation(ctx, run.ID, step.ID, current.LifecycleGeneration)
	if err != nil {
		return domain.ProviderAttempt{}, false, err
	}
	for _, a := range attempts {
		if a.Provider == to {
			// This obligation has already been offered to that provider. The
			// count alone would permit the hop; the history refuses it, which
			// is what makes A->B->A impossible rather than merely bounded.
			c.advanceProviderAttempt(ctx, current, domain.ProviderAttemptFailedSafe,
				reason+" (no untried provider remains)", class, safety, evidence)
			return domain.ProviderAttempt{}, false, nil
		}
	}

	// Terminate the predecessor FIRST. The authoritative partial unique index
	// admits at most one live attempt per obligation, so the successor cannot
	// exist until this lands -- and a crash between the two leaves a terminal
	// predecessor and no successor, which the next pass resolves by reading the
	// budget and creating the successor. The reverse order would allow, for one
	// instant, two live attempts on one worktree.
	if !c.advanceProviderAttempt(ctx, current, domain.ProviderAttemptFailedSafe, reason, class, safety, evidence) {
		// Somebody else moved it. This pass's view is stale and it must not
		// mint a successor for an obligation it no longer speaks for.
		return domain.ProviderAttempt{}, false, nil
	}

	now := c.clock()
	successor := domain.ProviderAttempt{
		ID: "pa-" + c.newID(), WorkflowRunID: run.ID, WorkflowStepID: step.ID,
		TaskID: current.TaskID, ProjectID: run.ProjectID,
		// UNCHANGED, on purpose: the obligation and the placement do not move
		// when the provider does.
		LifecycleGeneration: current.LifecycleGeneration,
		PlacementGeneration: current.PlacementGeneration,
		Ordinal:             current.Ordinal + 1,
		Provider:            to, Profile: profile,
		State:                domain.ProviderAttemptPlanned,
		PredecessorAttemptID: current.ID,
		CreatedAt:            now, UpdatedAt: now,
	}
	created, err := c.providerAttempts.CreateProviderAttempt(ctx, successor)
	if err != nil {
		return domain.ProviderAttempt{}, false, err
	}
	if !created {
		existing, found, gerr := c.providerAttempts.GetAuthoritativeProviderAttempt(ctx, run.ID, step.ID, current.LifecycleGeneration)
		return existing, found, gerr
	}
	if _, err := c.providerAttempts.LinkProviderAttemptSuccessor(ctx, current.ID, successor.ID, now); err != nil && c.log != nil {
		c.log.Debug("workflow: could not link provider attempt successor", "attempt", current.ID, "err", err)
	}
	return successor, true, nil
}

// abandonProviderAttemptsForRun closes out a terminal run's outstanding
// attempts. Called from the same terminal paths that release capacity and
// branch locks, and for the same reason: an attempt left authoritative for a
// run that is over would let a late signal act on a dead obligation.
func (c *Coordinator) abandonProviderAttemptsForRun(ctx stdctx.Context, runID, reason string) {
	if !c.providerAttemptsEnabled() || runID == "" {
		return
	}
	if _, err := c.providerAttempts.AbandonProviderAttemptsForRun(ctx, runID, reason, c.clock()); err != nil && c.log != nil {
		c.log.Warn("workflow: could not abandon a terminal run's provider attempts", "run", runID, "err", err)
	}
}

// ProviderAttemptIsAuthoritative is the guard every authority-bearing operation
// consults before acting on behalf of an attempt.
//
// Fail-closed on a read error: an unreadable ledger means no authority, never
// assumed authority.
func (c *Coordinator) ProviderAttemptIsAuthoritative(ctx stdctx.Context, attemptID string) bool {
	if !c.providerAttemptsEnabled() {
		return true
	}
	if attemptID == "" {
		return false
	}
	attempt, found, err := c.providerAttempts.GetProviderAttempt(ctx, attemptID)
	if err != nil || !found {
		return false
	}
	return attempt.State.Authoritative()
}

// ---------------------------------------------------------------------------
// §H — failover after PROVEN no mutation
// ---------------------------------------------------------------------------

// MutationProof is the positive evidence that a provider attempt which got as
// far as having a runtime nevertheless changed nothing.
//
// Every field is a fact AO already records. None of them is optional, and that
// is the point: §H says "do NOT equate 'git status clean right now' with
// sufficient proof if AO has ambiguous runtime evidence", so a clean workspace
// on its own does not appear here at all. It is one of five conditions.
type MutationProof struct {
	// RuntimeIdentified means AO knows the exact runtime/session the attempt
	// launched -- not that one probably existed.
	RuntimeIdentified bool
	// RuntimeTerminal means that exact runtime has provably stopped. A provider
	// that might still be typing cannot be proven to have written nothing.
	RuntimeTerminal bool
	// AttemptTerminal means the provider attempt itself reached a terminal,
	// non-ambiguous state.
	AttemptTerminal bool
	// FingerprintBefore and FingerprintAfter are the workspace fingerprints
	// taken at launch and now. Equality is necessary and nowhere near
	// sufficient.
	FingerprintBefore string
	FingerprintAfter  string
	// NoAuthoritativeMutation means AO holds no commit, no integration record
	// and no recorded write for this attempt.
	NoAuthoritativeMutation bool
}

// Proven reports whether every condition holds. It is deliberately an AND with
// no shortcuts: a proof that can be satisfied by a subset is not a proof, it is
// a heuristic with a confident name.
func (p MutationProof) Proven() bool {
	return p.RuntimeIdentified &&
		p.RuntimeTerminal &&
		p.AttemptTerminal &&
		p.NoAuthoritativeMutation &&
		p.FingerprintBefore != "" &&
		p.FingerprintAfter != "" &&
		p.FingerprintBefore == p.FingerprintAfter
}

// Digest is the evidence string stored on the attempt. It names what was
// compared, so a later reader can tell which fingerprints the claim rested on
// rather than having to trust the word "proven".
func (p MutationProof) Digest() string {
	if !p.Proven() {
		return ""
	}
	return "workspace_unchanged:" + p.FingerprintBefore
}

// ProveNoMutation assembles the proof for one provider attempt.
//
// It never invents a missing fact. A dependency that is not wired, a session
// that cannot be read, a workspace that cannot be observed -- each simply
// leaves its condition false, and the proof fails. That direction is chosen
// deliberately: an unprovable failover is a stopped run, and an unsound one is
// two providers on one checkout.
func (c *Coordinator) ProveNoMutation(
	ctx stdctx.Context, attempt domain.ProviderAttempt, placement domain.ExecutionPlacement, launchFingerprint string,
) MutationProof {
	proof := MutationProof{FingerprintBefore: launchFingerprint}
	if attempt.RuntimeSessionID == "" {
		// No exact runtime identity. §H's first requirement is precisely this,
		// and a launch AO cannot name is the ambiguous case, not a safe one.
		return proof
	}
	proof.RuntimeIdentified = true
	proof.AttemptTerminal = attempt.State.Terminal() && attempt.State != domain.ProviderAttemptFailedAmbiguous

	if c.sessionFacts != nil {
		if rec, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(attempt.RuntimeSessionID)); err == nil && found {
			proof.RuntimeTerminal = rec.IsTerminated
		}
	}
	if c.workspaceFacts != nil && placement.RepoPath != "" {
		path := placement.RepoPath
		if placement.Type.Isolated() && placement.WorktreePath != "" {
			path = placement.WorktreePath
		}
		obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
			Path: path, Branch: placement.ExecutionBranch,
			ProjectID: domain.ProjectID(placement.ProjectID), RepoPath: placement.RepoPath,
		})
		if err == nil {
			proof.FingerprintAfter = WorkspaceFingerprint(obs)
		}
	}
	proof.NoAuthoritativeMutation = c.noAuthoritativeMutationFor(ctx, attempt, placement)
	return proof
}

// noAuthoritativeMutationFor reports whether AO holds any durable evidence that
// this attempt's obligation produced a mutation.
//
// Three sources, all durable: an integrated placement (the work landed), a
// recorded commit on the placement, and any workspace-provenance record naming
// an authorized mutation for this run. Any one of them makes the answer false.
func (c *Coordinator) noAuthoritativeMutationFor(ctx stdctx.Context, attempt domain.ProviderAttempt, placement domain.ExecutionPlacement) bool {
	if placement.IntegratedSHA != "" || placement.State == domain.PlacementIntegrating || placement.State == domain.PlacementIntegrated {
		return false
	}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, attempt.WorkflowRunID)
	if err != nil {
		// Unreadable evidence is not absent evidence.
		return false
	}
	for _, cp := range checkpoints {
		switch cp.DurablePhase {
		case "autonomous_local_commit", masterIntegrationDurablePhase, taskWorktreeCleanupPhase:
			return false
		}
		if cp.HeadSHA != "" && cp.HeadSHA != placement.BaseSHA {
			return false
		}
	}
	return true
}

// mutationSafetyForLive is §H's entry point for a failure reported against a
// LIVE worker session.
//
// It returns the safety class, the proof it rests on, and the durable attempt
// the classification belongs to. The three travel together deliberately: a
// caller must not be able to act on a class without the evidence behind it, and
// FailoverProviderAttempt refuses safe_after_proven_no_mutation with an empty
// digest for exactly that reason.
//
// Fail-closed in every direction. No ledger, no attempt, no placement, no
// dispatch record, an unreadable session or an unreadable workspace — each of
// them leaves a condition of MutationProof false, and the answer is
// ambiguous_execution. The cost of that is a stopped run somebody can restart;
// the cost of the other direction is provider B writing over provider A.
func (c *Coordinator) mutationSafetyForLive(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, sessionID string,
) (domain.FailoverSafety, MutationProof, domain.ProviderAttempt) {
	if !c.providerAttemptsEnabled() {
		// No ledger: there is no durable attempt to classify and nowhere to
		// record the class, so this returns the ambiguous class with no attempt
		// to attach it to. The CALLER is what decides whether that refuses a
		// failover -- it applies the gate only when the ledger is wired, so a
		// deployment without one keeps its pre-P1-D behaviour rather than
		// silently losing failover to a proof it has no way to produce.
		return domain.FailoverAmbiguousExecution, MutationProof{}, domain.ProviderAttempt{}
	}
	generation := c.stepDispatchGeneration(ctx, step.ID)
	attempt, found, err := c.providerAttempts.GetAuthoritativeProviderAttempt(ctx, run.ID, step.ID, generation)
	if err != nil || !found {
		return domain.FailoverAmbiguousExecution, MutationProof{}, domain.ProviderAttempt{}
	}
	// The runtime identity is what §H's first requirement asks for, and the
	// live session is it. Binding it here rather than trusting the row means a
	// failure reported before the runtime binding landed is still classifiable.
	if attempt.RuntimeSessionID == "" && sessionID != "" {
		c.bindProviderAttemptRuntime(ctx, attempt, sessionID, attempt.CapacityClaimID)
		attempt.RuntimeSessionID = sessionID
	}

	placement, perr := c.requireCurrentPlacement(ctx, run, attempt.PlacementGeneration)
	if perr != nil {
		// A stale placement generation cannot be the subject of a proof: the
		// workspace the fingerprint would describe is not the one this attempt
		// was entitled to.
		return domain.FailoverAmbiguousExecution, MutationProof{}, attempt
	}
	proof := c.ProveNoMutation(ctx, attempt, placement, c.launchFingerprintFor(ctx, step.ID))
	// The attempt is still live at this point, so AttemptTerminal cannot be
	// read from its row. What §H means by it is "this provider is finished
	// with", and the caller has just reported exactly that -- a terminal,
	// failed provider attempt. It is set here, once, at the only place that
	// fact is known, rather than being inferred later from a state the caller
	// is about to write.
	proof.AttemptTerminal = true
	return ClassifyMidExecutionFailoverSafety(attempt, proof), proof, attempt
}

// launchFingerprintFor reads the workspace fingerprint recorded at this step's
// launch, from the dispatch boundary record the phased dispatch already writes.
//
// It is read rather than recomputed because the comparison only means something
// against the tree as it was WHEN THE PROVIDER STARTED. A fingerprint taken now
// and compared with another taken now proves nothing at all.
//
// Empty when no record carries one, which fails the proof — correctly: without
// a before-state there is nothing to compare against.
func (c *Coordinator) launchFingerprintFor(ctx stdctx.Context, stepID string) string {
	ps, ok := c.provenanceStore()
	if !ok || stepID == "" {
		return ""
	}
	records, err := ps.ListWorkflowDispatchCheckpointsByStep(ctx, stepID)
	if err != nil {
		return ""
	}
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].WorkspaceFingerprint != "" {
			return records[i].WorkspaceFingerprint
		}
	}
	return ""
}
