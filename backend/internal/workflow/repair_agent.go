package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// repair_agent.go — P1-B §E/§F/§H: a bounded Repair Agent for the user's own
// workflow.
//
// The Incident Advisor already has a repair agent, and it is deliberately not
// this one: incident_repair.go repairs AO ITSELF, in AO's own checkout, on an
// explicit human approval, after an LLM diagnosis. What is missing is the far
// more common case — the run stopped because the code it was writing does not
// build, or its tests fail, or the reviewer keeps asking for the same change —
// where the user's remedy today is to read the diagnostics and paste them into
// a second workflow by hand.
//
// # A repair is a workflow run
//
// The same argument incident_repair.go makes applies unchanged, and is the
// reason this file is small. Everything a repair needs after "the agent made a
// change" already exists and is already hardened: an independent reviewer with
// cross-provider routing, deterministic verification, bounded fix cycles,
// stale-approval detection, worktree isolation or branch locking, and boot
// reconciliation for all of it. That is what a workflow run IS. So a repair is
// a bounded TASK run (P1-A), created against the same project, and nothing
// here re-implements any of it.
//
// # What is genuinely new
//
//   - ELIGIBILITY is deterministic and closed. It comes from the canonical
//     attention reason's own disposition (attention.go), where `Repairable` is
//     false by default. A stop AO cannot name, and every stop about provenance,
//     credentials, permissions, destructive ambiguity or policy, can never have
//     a code-writing agent aimed at it -- by construction, not by a deny-list.
//   - The INTENT is durable, and carries the evidence digest of the failure it
//     is repairing, so "one failure must not create unbounded repair agents"
//     is checkable rather than hoped for.
//   - The GENERATION is a compare-and-set. A repair carrying a stale
//     generation, or aimed at a lifecycle generation the run has already moved
//     past, may not mutate anything.
//   - The BUDGET is frozen per run and escalates to a person when spent.

// repairDispatchPhase records one launched repair and the authority for it.
const repairDispatchPhase = "workflow_repair_dispatched"

// repairEscalatedPhase records a repairable stop whose budget is spent, so the
// escalation to a person is a durable fact rather than an absence.
const repairEscalatedPhase = "workflow_repair_escalated"

// repairResolvedPhase records a repair run reaching a terminal state and what
// AO did about the origin run as a result.
const repairResolvedPhase = "workflow_repair_resolved"

// ErrRepairIneligible means the stop is one AO must never aim a code-writing
// agent at. It is a refusal, not a fallback.
var ErrRepairIneligible = errors.New("workflow: this stop is not a repairable condition, so AO will not launch a repair agent")

// ErrRepairUnsafeTarget means the target project runs on the user's own
// checkout and the stopped run still holds its branch, so a repair run would
// queue behind the very run it exists to unblock.
var ErrRepairUnsafeTarget = errors.New("workflow: this project runs on the user's own checkout and the stopped run holds its branch, so a repair run would deadlock behind it")

// repairIdempotencyKey is the single-flight identity of one repair launch.
//
// It is keyed by the run, the FAILURE being repaired and the generation --
// not by the run alone. Two different failures on one run are two repairs;
// the same failure re-observed by a poll, a restart or a double click is one.
func repairIdempotencyKey(runID, evidenceDigest string, generation int) string {
	return "workflow-repair:" + runID + ":" + evidenceDigest + ":gen" + strconv.Itoa(generation)
}

// repairIntentID derives an intent's identity from the run and generation, so
// a replay computes the one that already exists rather than minting a second.
func repairIntentID(runID string, generation int) string {
	sum := sha256.Sum256([]byte("workflow-repair-intent\x00" + runID + "\x00" + strconv.Itoa(generation)))
	return "wfr-" + hex.EncodeToString(sum[:])[:24]
}

// repairsSpentFor counts this run's durable repair dispatches. It is the
// budget's whole enforcement, and it is a fold over the append-only ledger, so
// a restart cannot lose a repair that really happened and hand the run a fresh
// budget.
func (c *Coordinator) repairsSpentFor(ctx stdctx.Context, runID string) int {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return 0
	}
	spent := 0
	for _, cp := range checkpoints {
		if cp.DurablePhase == repairDispatchPhase {
			spent++
		}
	}
	return spent
}

// repairIntents folds the ledger back into the intents recorded on it, newest
// last. Restart-safe by construction: every intent row is written before the
// run it describes is started.
func (c *Coordinator) repairIntents(ctx stdctx.Context, runID string) []domain.RepairIntent {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return nil
	}
	var out []domain.RepairIntent
	for _, cp := range checkpoints {
		if cp.DurablePhase != repairDispatchPhase || cp.RetryState == "" {
			continue
		}
		var intent domain.RepairIntent
		if err := json.Unmarshal([]byte(cp.RetryState), &intent); err != nil || intent.ID == "" {
			continue
		}
		out = append(out, intent)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Generation < out[j].Generation })
	return out
}

// evidenceDigestFor is the stable identity of the failure being repaired.
//
// It is built from durable identifiers and AO's own vocabulary only: the
// canonical reason, the step and its kind, the latest attempt's error class,
// and the step's dispatch generation. No prompt text, no findings body, no
// provider output, no path -- an evidence digest is an identity, not a
// disclosure.
func evidenceDigestFor(d RunDetail, reason string) (digest, stepID string, generation int64) {
	var parts []string
	parts = append(parts, "reason="+reason, "run="+d.Run.ID)
	for _, sd := range d.Steps {
		if sd.Step.State.Terminal() && sd.Step.State != domain.WorkflowStepFailed {
			continue
		}
		switch sd.Step.Kind {
		case domain.WorkflowStepWork, domain.WorkflowStepReview, domain.WorkflowStepFix, domain.WorkflowStepVerify:
		default:
			continue
		}
		if stepID == "" || sd.Step.Kind == domain.WorkflowStepVerify {
			stepID = sd.Step.ID
			generation = int64(len(sd.Attempts))
		}
		parts = append(parts, string(sd.Step.Kind)+"="+sd.Step.ID+":"+string(sd.Step.State)+":"+strconv.Itoa(len(sd.Attempts)))
		for _, at := range sd.Attempts {
			if at.ErrorClass != "" {
				parts = append(parts, "err="+string(at.ErrorClass))
			}
		}
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:32], stepID, generation
}

// RepairPlan is what a repair WOULD do, computed without doing any of it. It
// is what the API and the recovery panel show, so an operator authorizing a
// repair is authorizing something specific.
type RepairPlan struct {
	Eligibility domain.RepairEligibility
	// Intent is fully formed for an eligible repair, and zero otherwise.
	Intent domain.RepairIntent
	// Mode and Budget are the frozen policy in force.
	Mode   domain.RepairMode
	Spent  int
	Budget int
	Reason string
	// AutomaticAllowed reports whether AO may launch it without being asked.
	AutomaticAllowed bool
}

// PlanRepair computes the repair decision for a run. It writes nothing.
func (c *Coordinator) PlanRepair(ctx stdctx.Context, runID string) (RepairPlan, error) {
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return RepairPlan{}, err
	}
	return c.planRepairFor(ctx, detail)
}

func (c *Coordinator) planRepairFor(ctx stdctx.Context, detail RunDetail) (RepairPlan, error) {
	policy := policyForRun(detail.Run).EffectiveRepairPolicy()
	spent := c.repairsSpentFor(ctx, detail.Run.ID)
	out := RepairPlan{Mode: policy.Mode, Spent: spent, Budget: policy.MaxRepairCycles}

	if detail.Run.State.Terminal() {
		out.Eligibility = domain.RepairIneligible
		out.Reason = "This run has ended; there is nothing to repair."
		return out, nil
	}
	reason, disp, classified := resolveAttentionReason(detail)
	if !classified {
		out.Eligibility = domain.RepairUnknownCondition
		out.Reason = "AO has no durable record of why this run stopped, so it will not aim a repair agent at it."
		return out, nil
	}

	// §I/§J: resolve the run that actually STOPPED before deciding anything.
	//
	// A master objective's own stop is `child_needs_attention` -- a mirror of
	// its child's, and never repairable in itself. Judging eligibility on the
	// mirror would make a master objective permanently unrepairable, which is
	// the opposite of §J's "repair should normally occur in the affected
	// child". So the target, its reason and its disposition are all resolved
	// first, and every decision below is taken about the run that owns the
	// problem.
	life := DeriveLifecycle(LifecycleInput{Detail: detail, Questions: detail.Questions})
	target := detail
	if life.AttentionWorkflowID != "" && life.AttentionWorkflowID != detail.Run.ID {
		child, cerr := c.GetRun(ctx, life.AttentionWorkflowID)
		switch {
		case cerr != nil:
			// Fail closed, and deliberately as an ANSWER rather than an error:
			// "AO cannot read the affected child" is a true, useful statement
			// about the run, and turning the recovery panel into a 500 would
			// tell the operator less than this does.
			if c.log != nil {
				c.log.Debug("workflow: repair could not read the affected child",
					"run", detail.Run.ID, "child", life.AttentionWorkflowID, "err", cerr)
			}
			out.Eligibility = domain.RepairUnknownCondition
			out.Reason = "AO could not read the child run this objective is stopped on, so it will not repair blind."
			return out, nil
		default:
			target = child
		}
		childReason, childDisp, childClassified := resolveAttentionReason(target)
		if !childClassified {
			out.Eligibility = domain.RepairUnknownCondition
			out.Reason = "AO has no durable record of why the affected task stopped, so it will not aim a repair agent at it."
			return out, nil
		}
		reason, disp = childReason, childDisp
	}

	// §F, the structural half: A REPAIR IS NEVER ITSELF REPAIRED.
	//
	// A repair run is an ordinary bounded task run, which is exactly what makes
	// it repairable-looking: it has a work step, a reviewer, a fix budget, and
	// it can stop on fix_budget_exhausted like anything else. Nothing before
	// this said no, so `task -> repair -> repair -> repair` was reachable, each
	// level minting a budget of its own and none of them owing an answer to the
	// incident that started it. That is the cascade the semantics forbid: an
	// incident gets ONE bounded repair generation sequence, and a repair that
	// did not repair returns its result to the ORIGINAL incident rather than
	// spawning a descendant of its own.
	//
	// Checked on the resolved TARGET, not on `detail`, so an objective whose
	// affected child is a repair run is refused for the same reason directly.
	if c.isRepairRun(ctx, target.Run) {
		out.Eligibility = domain.RepairIneligible
		out.Reason = "This run IS a repair agent's run. A repair that did not succeed returns its result to the incident that authorized it; it never becomes the parent of another repair."
		return out, nil
	}

	// The CONDITION is judged before any policy or budget, so no policy
	// setting anywhere can make an unrepairable stop repairable.
	out.Eligibility = repairEligibility(disp, policy, spent)
	switch out.Eligibility {
	case domain.RepairIneligible:
		out.Reason = fmt.Sprintf("%q is not a repairable condition: its remedy is not a code change AO can make on its own.", reason)
		return out, nil
	case domain.RepairPolicyDisabled:
		out.Reason = "This run's frozen repair policy is disabled."
		return out, nil
	case domain.RepairBudgetExhausted:
		out.Reason = fmt.Sprintf("This run has already spent its %d repair attempts. The next step is a person's.", policy.MaxRepairCycles)
		return out, nil
	}

	digest, stepID, generation := evidenceDigestFor(target, reason)
	out.Intent = domain.RepairIntent{
		ID:                  repairIntentID(detail.Run.ID, spent+1),
		WorkflowRunID:       detail.Run.ID,
		TargetRunID:         target.Run.ID,
		TargetStepID:        stepID,
		ConditionReason:     reason,
		EvidenceDigest:      digest,
		Generation:          spent + 1,
		LifecycleGeneration: generation,
		ProjectID:           target.Run.ProjectID,
		Scope: domain.RepairScope{
			WriteIntent:       domain.WorkflowWriteIntentMutating,
			SiblingsUntouched: target.Run.ID != detail.Run.ID,
		},
		AcceptanceCriteria: repairAcceptanceCriteria(reason),
		Strategy:           domain.ExecutionStrategyTask,
		PolicyVersion:      policy.Version,
		Mode:               policy.Mode,
		At:                 c.clock(),
	}
	out.Reason = disp.HumanAction
	out.AutomaticAllowed = policy.Mode == domain.RepairModeAutomatic
	return out, nil
}

// repairAcceptanceCriteria is what "repaired" means, written by AO from the
// condition. It is deliberately not proposed by the agent that will be judged
// against it: a repair whose success criteria were written by the repairer is
// not verified, it is asserted.
func repairAcceptanceCriteria(reason string) []string {
	base := []string{
		"The condition named above no longer reproduces.",
		"No unrelated behaviour is changed, and no unrelated file is modified.",
		"The project's existing checks pass.",
	}
	switch reason {
	case ReasonVerifyBudgetExhausted:
		return append([]string{"The failing verification command exits zero for the reason it was written to check, not because the check was weakened or removed."}, base...)
	case ReasonFixBudgetExhausted:
		return append([]string{"Every outstanding reviewer finding is addressed in the code, not by changing what is reviewed."}, base...)
	case ReasonFixNoVerifiableChange:
		return append([]string{"The change the reviewer asked for exists in the worktree and is attributable to this task."}, base...)
	}
	return base
}

// LaunchRepair creates the bounded repair run for an eligible stop.
//
// authorizedBy names the person who asked. It is empty only for a
// policy-authorized automatic repair, and that distinction is recorded on the
// intent rather than inferred later.
func (c *Coordinator) LaunchRepair(ctx stdctx.Context, runID, authorizedBy string) (domain.RepairIntent, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.RepairIntent{}, err
	}
	if !ok {
		return domain.RepairIntent{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	// Fold in any repair generation that has already finished before deciding
	// whether this run needs another one.
	//
	// THE INCIDENT (wf-724a1e97). Generation 1 completed at 01:03:38. Twenty-
	// five seconds later generation 2 was minted for the same evidence digest,
	// because the single-flight guard below only refuses while the previous
	// generation's repair run is NON-terminal, and nothing had yet folded
	// generation 1's completion into the origin. Its result was then thrown
	// away: reconcileRepairOutcome, running for the first time hours later,
	// found generation 1 behind the current count and recorded it "superseded
	// ... and will not resume anything". A repair that WORKED was discarded by
	// a repair that was launched because nobody had looked at it yet.
	//
	// Reconciling first makes that unrepresentable. A completed generation
	// discharges the origin's obligation here, in this call, before eligibility
	// is judged -- so either the run is no longer stopped and there is nothing
	// to repair, or it is still stopped for a reason generation 1 did not fix
	// and generation 2 is genuinely the next attempt. Idempotent over its own
	// ledger rows, so calling it here costs nothing when there is nothing to
	// fold.
	c.reconcileRepairOutcome(ctx, run)

	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return domain.RepairIntent{}, err
	}
	plan, err := c.planRepairFor(ctx, detail)
	if err != nil {
		return domain.RepairIntent{}, err
	}
	if !plan.Eligibility.Allowed() {
		if plan.Eligibility == domain.RepairBudgetExhausted {
			c.recordRepairEscalation(ctx, detail.Run, plan)
		}
		return domain.RepairIntent{}, fmt.Errorf("%w (%s): %s", ErrRepairIneligible, plan.Eligibility, plan.Reason)
	}
	if authorizedBy == "" && plan.Mode != domain.RepairModeAutomatic {
		return domain.RepairIntent{}, fmt.Errorf("%w: this run's repair policy is %q, so a repair needs an operator's authorization", ErrInvalid, plan.Mode)
	}

	// P1-D §L: direct-branch repairs.
	//
	// P1-B refused these outright, because on the user's own checkout the
	// stopped run holds the branch lock for its whole life and a second run
	// against it would queue behind the very run it exists to unblock. The
	// refusal was correct and it left the most common single-developer setup
	// unable to use the feature at all.
	//
	// The lock now MOVES instead, on terms both sides can prove (see
	// repair_branch_cession.go). What is checked here is only that the move is
	// possible at all; the transfer itself happens after the repair run exists,
	// because a cession to a run that does not exist yet would leave the branch
	// owned by nothing.
	// P3-C §28: whether a cession is needed is a fact about the TARGET RUN's
	// execution placement, not about the project's default mode. An explicit
	// direct-branch run inside an isolated-default project holds a real branch
	// lock, and reading the project here answered "isolated" for it -- so the
	// repair was allowed to proceed with no cession path and would have queued
	// behind the very lock it exists to release. See placement_semantics.go.
	directBranch := c.runIDPlacementIsDirectBranch(ctx, plan.Intent.TargetRunID)
	if directBranch {
		if _, ok := c.branchLocks.(branchLockCeder); !ok || c.branchLocks == nil {
			// No way to transfer authority, so the deadlock P1-B named is
			// still real. Fail closed, exactly as before.
			return domain.RepairIntent{}, ErrRepairUnsafeTarget
		}
	}

	intent := plan.Intent
	intent.AuthorizedBy = authorizedBy

	// §F, enforced rather than asserted: ONE failure gets ONE repair agent.
	//
	// The single-flight claim below is keyed by generation as well as by
	// failure, so on its own it would let the same failure buy a second repair
	// as soon as the first spent a generation -- which is exactly the
	// unbounded-repair shape §F rules out. An unresolved repair already aimed
	// at this evidence digest IS this failure's repair; report it.
	for _, existing := range c.repairIntents(ctx, runID) {
		if existing.EvidenceDigest != intent.EvidenceDigest || existing.RepairRunID == "" {
			continue
		}
		repairRun, found, rerr := c.store.GetWorkflowRun(ctx, existing.RepairRunID)
		switch {
		case rerr != nil:
			return domain.RepairIntent{}, rerr
		case !found, !repairRun.State.Terminal():
			return existing, nil
		}
	}

	entry, claimed, err := c.claimIncidentOutboxSlot(ctx, detail.Run,
		repairIdempotencyKey(runID, intent.EvidenceDigest, intent.Generation),
		fmt.Sprintf(`{"repairIntentId":%q,"generation":%d}`, intent.ID, intent.Generation))
	if err != nil {
		return domain.RepairIntent{}, err
	}
	if !claimed {
		// Another pass already created this generation's repair for this exact
		// failure. Report the one that exists rather than making a second --
		// which is precisely §F's "one failure must not create unbounded
		// repair agents", enforced rather than asserted.
		for _, existing := range c.repairIntents(ctx, runID) {
			if existing.EvidenceDigest == intent.EvidenceDigest && existing.Generation == intent.Generation {
				return existing, nil
			}
		}
		return domain.RepairIntent{}, fmt.Errorf("%w: a repair for this failure is already being created", ErrPlanLocked)
	}

	created, err := c.CreateTaskRun(ctx, TaskRunRequest{
		ProjectID:          intent.ProjectID,
		Objective:          buildRepairObjective(intent, plan),
		Strategy:           c.repairChildStrategy(intent),
		AcceptanceCriteria: intent.AcceptanceCriteria,
		WriteIntent:        domain.WorkflowWriteIntentMutating,
		Verification:       repairVerificationFor(ctx, c, intent),
	})
	if err != nil {
		c.releaseIncidentRepairClaim(ctx, entry, err)
		return domain.RepairIntent{}, err
	}
	intent.RepairRunID = created.Run.ID

	// P1-C: mark the repair run as a repair BEFORE it is started, so the
	// scheduler meters its worker dispatch against the repair budget rather
	// than the ordinary worker one. Written first for the same reason the
	// intent is: a run that starts before anything says what it is would be
	// charged to the wrong meter, and the repair limit is the narrower budget.
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  created.Run.ID,
		ProjectID:      created.Run.ProjectID,
		DurablePhase:   repairRunOriginPhase,
		NextAction:     fmt.Sprintf("repair run for %s (%s), generation %d", intent.TargetRunID, intent.ConditionReason, intent.Generation),
		PayloadVersion: "v1",
		RetryState:     fmt.Sprintf(`{"originRunId":%q,"generation":%d}`, intent.WorkflowRunID, intent.Generation),
		CreatedAt:      c.clock(),
	})

	// The intent is durable BEFORE the repair run is started, so a crash
	// between the two leaves a discoverable repair and a spent generation --
	// recovery finds a repair, never an orphan and never a free retry.
	payload, err := json.Marshal(intent)
	if err != nil {
		c.releaseIncidentRepairClaim(ctx, entry, err)
		return domain.RepairIntent{}, err
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  detail.Run.ID,
		ProjectID:      detail.Run.ProjectID,
		DurablePhase:   repairDispatchPhase,
		NextAction:     fmt.Sprintf("repair generation %d dispatched for %s as run %s", intent.Generation, intent.ConditionReason, created.Run.ID),
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}); err != nil {
		c.releaseIncidentRepairClaim(ctx, entry, err)
		return domain.RepairIntent{}, err
	}

	// P1-D §L: hand the branch over BEFORE the repair run starts, and only
	// after the intent is durable. Ordering is the whole safety property here:
	// a repair that started before it held the branch would either block or
	// write without authority, and a cession recorded before the intent would
	// describe a transfer to a repair nobody can account for.
	//
	// A failed cession is a refusal, not a warning: the repair run exists and
	// is linked, but it must not start without the authority it needs, and the
	// origin keeps the branch it still holds.
	if directBranch {
		if _, cerr := c.cedeBranchLockToRepair(ctx, detail.Run, intent); cerr != nil {
			if c.log != nil {
				c.log.Warn("workflow: direct-branch repair could not take the branch; it will not be started",
					"run", runID, "repairRun", created.Run.ID, "err", cerr)
			}
			return domain.RepairIntent{}, cerr
		}
	}

	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		// The run exists and is linked; starting it again is what recovery and
		// the ordinary resume path already do. Rolling back would discard the
		// audit trail of an authorization that really happened.
		if c.log != nil {
			c.log.Warn("workflow: repair run created but not started",
				"run", runID, "repairRun", created.Run.ID, "err", err)
		}
	}
	if c.log != nil {
		c.log.Info("workflow: repair run launched", "run", runID, "target", intent.TargetRunID,
			"generation", intent.Generation, "repairRun", created.Run.ID, "reason", intent.ConditionReason,
			"authorizedBy", authorizedBy, "mode", intent.Mode)
	}
	return intent, nil
}

// repairChildStrategy is §I, in one place: a repair is always a bounded TASK.
//
// It is never autonomous and never master, whatever the run being repaired is.
// A repair that decomposes is a repair nobody can reason about, and a master
// repair of a master objective is the recursive decomposition §I forbids.
func (c *Coordinator) repairChildStrategy(intent domain.RepairIntent) domain.ExecutionStrategySelection {
	return domain.ExecutionStrategySelection{
		Effective:     domain.ExecutionStrategyTask,
		Source:        domain.ExecutionStrategyPolicy,
		PolicyVersion: domain.ExecutionStrategyPolicyVersion,
		Reason:        domain.ExecutionStrategyReasonBoundedWork,
		At:            intent.At,
	}
}

// repairVerificationFor reuses the verification plan of the step being
// repaired when there is one, so the repair is judged by the same
// deterministic checks that caught the failure -- never by checks the repairer
// or AO invents for the occasion.
func repairVerificationFor(ctx stdctx.Context, c *Coordinator, intent domain.RepairIntent) VerificationPlan {
	target, err := c.GetRun(ctx, intent.TargetRunID)
	if err != nil {
		return VerificationPlan{}
	}
	for _, sd := range target.Steps {
		if sd.Step.Kind != domain.WorkflowStepPlan {
			continue
		}
		artifact, aerr := UnmarshalPlanArtifact(sd.Step.ArtifactJSON)
		if aerr != nil {
			return VerificationPlan{}
		}
		return artifact.Verification
	}
	return VerificationPlan{}
}

// buildRepairObjective renders the intent as the repair run's objective.
//
// It carries AO's own durable vocabulary and nothing else: the condition, the
// run and step it belongs to, and the guardrails. No provider output, no
// findings body, no credentials, no paths outside the project.
func buildRepairObjective(intent domain.RepairIntent, plan RepairPlan) string {
	var b strings.Builder
	b.WriteString("Repair a stopped AO workflow task.\n\n")
	fmt.Fprintf(&b, "Stopped run: %s\nCondition: %s\n", intent.TargetRunID, intent.ConditionReason)
	if intent.TargetStepID != "" {
		fmt.Fprintf(&b, "Affected step: %s\n", intent.TargetStepID)
	}
	fmt.Fprintf(&b, "Repair generation: %d of %d\n\n", intent.Generation, plan.Budget)
	if plan.Reason != "" {
		fmt.Fprintf(&b, "What AO knows about the stop:\n%s\n\n", plan.Reason)
	}
	b.WriteString("Read the failing checks and the existing changes in this worktree, then implement the smallest correct fix for the condition above.\n\n")
	b.WriteString(`Do not weaken, skip or delete a check in order to make it pass. Do not run any destructive git operation: no reset, no stash, no checkout that discards work, no force, no branch deletion, no history rewrite. Do not touch work belonging to any other task. Do not approve your own change — an independent reviewer reads it next and deterministic checks run after that. If you conclude the condition is not repairable by a code change, stop and say so rather than changing something else.`)
	return b.String()
}

// recordRepairEscalation writes the durable fact that a repairable stop has
// run out of repair budget and is now a person's. §F asks for the escalation;
// this is what makes it a fact on the ledger rather than the absence of one.
func (c *Coordinator) recordRepairEscalation(ctx stdctx.Context, run domain.WorkflowRun, plan RepairPlan) {
	c.recordAttentionStop(ctx, run, nil, repairEscalatedPhase,
		fmt.Sprintf("automatic repair is exhausted for this run (%d of %d spent); the next step is a person's", plan.Spent, plan.Budget))
}

// maybeAutoRepair is the ONLY automatic entry point, and it is gated three
// ways: the frozen policy must say automatic, the condition must be
// explicitly repairable, and the budget must be unspent. It is called from
// boot reconciliation, where every other bounded self-remedy already lives.
//
// Best-effort: a failure to launch is logged and leaves the run exactly as it
// was, stopped and waiting for a person, which is where it already was.
func (c *Coordinator) maybeAutoRepair(ctx stdctx.Context, run domain.WorkflowRun) {
	if run.State != domain.WorkflowRunNeedsAttention {
		return
	}
	if policyForRun(run).EffectiveRepairPolicy().Mode != domain.RepairModeAutomatic {
		return
	}
	if _, err := c.LaunchRepair(ctx, run.ID, ""); err != nil && c.log != nil {
		c.log.Debug("workflow: automatic repair not launched", "run", run.ID, "err", err)
	}
}

// reconcileRepairOutcome closes the loop: a repair run that has reached a
// terminal state either discharges the origin run's obligation or hands it
// back to a person, and either way it says so on the ledger exactly once.
//
// Stale by generation is refused here rather than acted on: an intent whose
// generation is behind the run's current repair count describes a repair the
// run has already moved past, and letting it resume anything would be letting
// a stale writer drive a newer lifecycle.
func (c *Coordinator) reconcileRepairOutcome(ctx stdctx.Context, run domain.WorkflowRun) {
	intents := c.repairIntents(ctx, run.ID)
	if len(intents) == 0 {
		return
	}
	current := len(intents)
	resolved := c.resolvedRepairGenerations(ctx, run.ID)
	for _, intent := range intents {
		if intent.RepairRunID == "" {
			continue
		}
		if _, done := resolved[intent.Generation]; done {
			continue
		}
		if intent.Generation < current {
			// A superseded generation. It is recorded as resolved so it stops
			// being reconsidered, and it drives nothing: a generation the run
			// has already moved past must never resume a lifecycle a newer one
			// owns.
			//
			// Two things it must still do. It gives the branch back, because a
			// superseded repair holding a direct-branch lock is the original
			// deadlock wearing a new hat -- the newer generation, and the
			// origin after it, both queue behind a repair nobody is watching.
			// And it says on the ledger what that repair had actually reached,
			// because "superseded" alone reads as "it was going nowhere", which
			// is exactly the wrong thing to record about a repair that had
			// COMPLETED (wf-724a1e97's generation 1). LaunchRepair now folds a
			// completed generation in before minting the next one, so this
			// combination should no longer arise; recording it precisely is
			// what will make it visible if it ever does again.
			if rerr := c.returnBranchLockFromRepair(ctx, run, intent); rerr != nil && c.log != nil {
				c.log.Warn("workflow: superseded repair could not return the branch lock", "run", run.ID, "err", rerr)
			}
			reached := "unknown"
			if superseded, found, gerr := c.store.GetWorkflowRun(ctx, intent.RepairRunID); gerr == nil && found {
				reached = string(superseded.State)
			}
			c.recordRepairResolution(ctx, run, intent, "superseded",
				fmt.Sprintf("repair generation %d (run %s, %s) was superseded by generation %d and will not resume anything",
					intent.Generation, intent.RepairRunID, reached, current))
			continue
		}
		repairRun, ok, err := c.store.GetWorkflowRun(ctx, intent.RepairRunID)
		if err != nil || !ok || !repairRun.State.Terminal() {
			continue
		}
		if repairRun.State != domain.WorkflowRunCompleted {
			// A repair that ended without repairing still has to give the
			// branch back: the origin owns it again either way, and a failed
			// repair holding a branch forever is the deadlock in a new hat.
			if rerr := c.returnBranchLockFromRepair(ctx, run, intent); rerr != nil && c.log != nil {
				c.log.Warn("workflow: failed repair could not return the branch lock", "run", run.ID, "err", rerr)
			}
			c.recordRepairResolution(ctx, run, intent, string(repairRun.State),
				fmt.Sprintf("repair run %s ended %s without repairing %s; the next step is a person's",
					intent.RepairRunID, repairRun.State, intent.ConditionReason))
			continue
		}
		// P1-D §L: the branch comes back before the origin is resumed, and
		// only if this repair generation is still current. Resuming a run that
		// does not hold its own branch would be resuming it into a refusal.
		if rerr := c.returnBranchLockFromRepair(ctx, run, intent); rerr != nil && c.log != nil {
			c.log.Warn("workflow: repair could not return the branch lock", "run", run.ID, "err", rerr)
		}
		c.recordRepairResolution(ctx, run, intent, "completed",
			fmt.Sprintf("repair run %s completed; resuming the obligation it was blocking", intent.RepairRunID))
		// The origin run resumes through the ordinary evidence-gated path, so
		// a repair cannot advance anything the run's own guards would refuse.
		if _, err := c.ContinueRun(ctx, run.ID); err != nil && c.log != nil {
			c.log.Debug("workflow: repaired run did not resume", "run", run.ID, "err", err)
		}
	}
}

// resolvedRepairGenerations folds the ledger's resolution rows, so a repair
// outcome is acted on exactly once however many times reconciliation runs.
func (c *Coordinator) resolvedRepairGenerations(ctx stdctx.Context, runID string) map[int]struct{} {
	out := map[int]struct{}{}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return out
	}
	for _, cp := range checkpoints {
		if cp.DurablePhase != repairResolvedPhase {
			continue
		}
		var body struct {
			Generation int `json:"generation"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &body) == nil && body.Generation > 0 {
			out[body.Generation] = struct{}{}
		}
	}
	return out
}

func (c *Coordinator) recordRepairResolution(ctx stdctx.Context, run domain.WorkflowRun, intent domain.RepairIntent, outcome, detail string) {
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		DurablePhase:   repairResolvedPhase,
		NextAction:     detail,
		PayloadVersion: "v1",
		RetryState:     fmt.Sprintf(`{"generation":%d,"repairRunId":%q,"outcome":%q}`, intent.Generation, intent.RepairRunID, outcome),
		CreatedAt:      c.clock(),
	})
}

// ApplyRepairPolicy freezes a run's auto-repair mode.
//
// It mirrors ApplyExecutionPolicySnapshot: run creation always stamps the safe
// default (suggest), and this is the explicit post-creation step a caller
// takes when the create request named something else. Splitting it that way is
// what keeps CreateRun's signature -- and its ~50 call sites -- unchanged.
//
// It refuses once the run has left `pending`. A repair policy is a statement
// about what AO may do to a run unattended, and widening that for a run
// already in flight would change the terms under which work already started.
// Restarts therefore cannot change it either: they never re-enter this call.
func (c *Coordinator) ApplyRepairPolicy(ctx stdctx.Context, runID string, mode domain.RepairMode) error {
	if !mode.Valid() {
		return fmt.Errorf("%w: %q is not a repair mode", ErrInvalid, mode)
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State != domain.WorkflowRunPending {
		return fmt.Errorf("%w: workflow run %q is already %s; its repair policy is frozen", ErrInvalid, runID, run.State)
	}
	return c.rewriteFrozenPolicy(ctx, run, func(p *domain.WorkflowPolicy) {
		frozen := p.EffectiveRepairPolicy()
		frozen.Mode = mode
		frozen.At = c.clock()
		p.Repair = frozen
	})
}

// RunRepairPolicy reads back a run's frozen repair policy.
func (c *Coordinator) RunRepairPolicy(ctx stdctx.Context, runID string) (domain.RepairPolicySnapshot, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.RepairPolicySnapshot{}, err
	}
	if !ok {
		return domain.RepairPolicySnapshot{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	return policyForRun(run).EffectiveRepairPolicy(), nil
}
