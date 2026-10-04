package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// repair_promotion.go — the repaired candidate becomes the task's result.
//
// A repair that works in its own checkout leaves AO with two artifacts and one
// task: the original branch, still carrying the code the reviewer rejected, and
// the repair's branch, carrying the code that fixes it. Left there, the origin
// resumes onto its own unchanged tree, the reviewer says exactly what it said
// before, and the repair has cost a generation to produce a commit nobody will
// ever review. §11 of the brief names that shape and forbids it:
//
//	Do not leave: original task branch = old artifact, repair branch = fixed
//	artifact, parent still integrating original.
//
// # Promotion is a fast-forward, and only a fast-forward
//
// The repair's branch was cut from the origin's exact committed head
// (repair_artifact.go), so the origin's head is an ancestor of the repaired
// commit by construction and `merge --ff-only` is the whole operation. That is
// deliberate rather than convenient: a merge commit, a rebase or a reset are all
// operations that can produce a tree neither side ever had, and none of them is
// something AO may do to a checkout on its own authority.
//
// Two preconditions, both refusals rather than best-effort:
//
//   - the origin's checkout is still at the commit the repair was cut from. If
//     it has moved, the repaired candidate is not a strict improvement on what
//     is there and AO has no basis for choosing between them.
//   - the origin's checkout is clean. Fast-forwarding over uncommitted work is
//     destroying somebody's only copy of it, which this codebase refuses
//     everywhere else and refuses here.
//
// A refused promotion is not a lost repair. The commit and its branch are named
// on the ledger and in the stop a person reads, and merging it by hand is one
// command. What is never done is AO writing over a tree it cannot account for.
//
// # After a successful promotion
//
// Nothing here reviews, verifies or integrates anything. The origin's branch has
// simply moved to a state no review has judged, which is the exact condition
// head_convergence.go already owns: the superseded changes_requested verdict
// stops being the reason the run is stopped, and ONE fresh authoritative review
// of the new state is dispatched. Review, verify and integration then run on the
// repaired candidate because it is the only candidate the task has -- not
// because anything here told them to.

// repairCandidatePromotedPhase records one promotion, on the ORIGIN's ledger.
// It is the idempotence key for the fold: a generation already promoted is
// never promoted twice, however many reconciliation passes run.
const repairCandidatePromotedPhase = "repair_candidate_promoted"

// repairCandidateNoChangePhase records a completed repair that produced no
// commit of its own. It is a distinct durable fact from a promotion, for the
// same reason isolatedNoChangePhase is: "nothing to promote" and "promoted
// nothing" are different states, and only one of them is an error.
const repairCandidateNoChangePhase = "repair_candidate_no_change"

// repairPromotionRefusedPhase records a promotion AO would not make.
//
// It is deliberately NOT one of the two phases the fold treats as final. A
// refusal is about the origin's checkout at one instant -- it had uncommitted
// work, or it had moved -- and both of those are things a person clears in
// seconds. Recording it as done would mean the repaired commit stayed stranded
// after the obstruction was gone, which is the §11 shape this file exists to
// prevent, arrived at by a different road.
const repairPromotionRefusedPhase = "repair_promotion_refused"

// repairPromotionRecord is what a promotion row carries.
type repairPromotionRecord struct {
	Generation   int    `json:"generation"`
	RepairRunID  string `json:"repairRunId"`
	OriginRunID  string `json:"originRunId"`
	OriginBranch string `json:"originBranch,omitempty"`
	// FromSHA is the commit the origin's checkout was at, which is also the
	// commit the repair was cut from. ToSHA is the repaired candidate.
	FromSHA string `json:"fromSha,omitempty"`
	ToSHA   string `json:"toSha,omitempty"`
	// Worktree is the checkout that moved.
	Worktree string `json:"worktree,omitempty"`
	// Refused, when set, explains a promotion that did not happen.
	Refused string `json:"refused,omitempty"`
}

// promoteRepairedCandidate moves a successful repair's commit onto the origin
// task's own branch.
//
// It returns true when the origin's artifact is now the repaired one -- either
// because this pass moved it, or because a previous pass already did. False
// means the origin still carries the old artifact, and the caller must not
// resume it as though it did not.
func (c *Coordinator) promoteRepairedCandidate(ctx stdctx.Context, origin domain.WorkflowRun, intent domain.RepairIntent) bool {
	a := intent.Origin
	if !a.Promotable() {
		// Nothing to promote onto: a direct-branch repair already wrote into
		// the origin's own checkout, and an origin with no artifact has no
		// branch of its own for a candidate to land on.
		return true
	}
	// The generation fence, first, for the same reason proveRepairQuiescent
	// applies it first: a superseded repair moving a branch is a stale writer
	// mutating a lifecycle a newer generation owns.
	if current := len(c.repairIntents(ctx, origin.ID)); intent.Generation != current {
		return false
	}
	// A generation already folded is done. Only the two FINAL phases count here
	// -- a refusal is deliberately not one of them, so an obstruction the
	// operator has since cleared is retried rather than written off.
	if _, done := c.promotedRepairGeneration(ctx, origin.ID, intent.Generation); done {
		return true
	}

	// The repaired candidate is read STRICTLY: only the phases that record a
	// commit count. The HeadSHA column is shared with the review dispatch
	// trail, which stores a workspace FINGERPRINT in it, and fast-forwarding a
	// checkout onto a fingerprint is a promotion that can only ever fail --
	// confusingly, and after the fact. See ledgerCommittedHead.
	candidate := c.ledgerCommittedHead(ctx, intent.RepairRunID)
	if candidate == "" || candidate == a.BaseSHA {
		// A repair that completed without committing anything has nothing to
		// promote. It is recorded rather than inferred, so a later reader can
		// tell it from a promotion that failed.
		c.recordRepairPromotion(ctx, origin, repairCandidateNoChangePhase, repairPromotionRecord{
			Generation: intent.Generation, RepairRunID: intent.RepairRunID, OriginRunID: origin.ID,
			OriginBranch: a.OriginBranch, FromSHA: a.BaseSHA, Worktree: a.OriginWorktreePath,
		}, fmt.Sprintf("repair run %s completed without committing a change, so run %s keeps the artifact it had",
			intent.RepairRunID, origin.ID))
		return true
	}

	err := c.repairGit().FastForward(ctx, a.OriginWorktreePath, a.BaseSHA, candidate)
	if err != nil {
		detail := fmt.Sprintf(
			"repair run %s produced %s on branch %s, and AO could not fast-forward %s onto it: %v",
			intent.RepairRunID, shortSHA(candidate), c.repairRunBranch(ctx, intent.RepairRunID), a.OriginWorktreePath, err)
		c.recordRepairPromotion(ctx, origin, repairPromotionRefusedPhase, repairPromotionRecord{
			Generation: intent.Generation, RepairRunID: intent.RepairRunID, OriginRunID: origin.ID,
			OriginBranch: a.OriginBranch, FromSHA: a.BaseSHA, ToSHA: candidate,
			Worktree: a.OriginWorktreePath, Refused: err.Error(),
		}, detail)
		// One stop per condition, not one per pass. Reconciliation re-enters
		// this on every boot and every fold, and the obstruction is a standing
		// condition rather than an event -- minting a row each time is the
		// write storm checkpoint_authority.go was written about.
		if reason, _, ok := c.stopReason(ctx, origin); !ok || reason != ReasonRepairPromotionBlocked {
			c.recordAttentionStop(ctx, origin, nil, ReasonRepairPromotionBlocked, detail)
		}
		if c.log != nil {
			c.log.Warn("workflow: a repaired candidate could not be promoted onto its origin branch",
				"run", origin.ID, "repairRun", intent.RepairRunID, "candidate", candidate, "err", err)
		}
		return false
	}

	c.recordRepairPromotion(ctx, origin, repairCandidatePromotedPhase, repairPromotionRecord{
		Generation: intent.Generation, RepairRunID: intent.RepairRunID, OriginRunID: origin.ID,
		OriginBranch: a.OriginBranch, FromSHA: a.BaseSHA, ToSHA: candidate, Worktree: a.OriginWorktreePath,
	}, fmt.Sprintf("repair_candidate_promoted: branch %s advanced from %s to %s, which is now this task's artifact — review, verification and integration all read it from here on",
		a.OriginBranch, shortSHA(a.BaseSHA), shortSHA(candidate)))
	if c.log != nil {
		c.log.Info("workflow: repaired candidate promoted onto the origin task's branch",
			"run", origin.ID, "repairRun", intent.RepairRunID,
			"branch", a.OriginBranch, "from", a.BaseSHA, "to", candidate)
	}
	return true
}

// promotedRepairGeneration folds the ledger for a promotion already recorded
// for one generation, so the fold acts exactly once however often it runs.
func (c *Coordinator) promotedRepairGeneration(ctx stdctx.Context, runID string, generation int) (repairPromotionRecord, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return repairPromotionRecord{}, false
	}
	for _, cp := range cps {
		// The refusal phase is deliberately absent: a blocked promotion is
		// retried once the obstruction clears, not written off.
		if cp.DurablePhase != repairCandidatePromotedPhase && cp.DurablePhase != repairCandidateNoChangePhase {
			continue
		}
		var rec repairPromotionRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil || rec.Generation != generation {
			continue
		}
		return rec, true
	}
	return repairPromotionRecord{}, false
}

func (c *Coordinator) recordRepairPromotion(ctx stdctx.Context, run domain.WorkflowRun, phase string, rec repairPromotionRecord, detail string) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             repairPromotionFoldID(phase, run.ID, rec.Generation),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		Branch:         rec.OriginBranch,
		WorktreePath:   rec.Worktree,
		BaseSHA:        rec.FromSHA,
		HeadSHA:        rec.ToSHA,
		NextAction:     detail,
		DurablePhase:   phase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: repair promotion not recorded", "run", run.ID, "err", err)
	}
}

// repairPromotionFoldID identifies a promotion row by what it is ABOUT, so a
// racing daemon or a restart re-running the fold collides on the primary key
// instead of writing a second account of one promotion. The phase is part of
// the identity because a refusal and the promotion that later succeeds are two
// different facts about the same generation.
func repairPromotionFoldID(phase, runID string, generation int) string {
	return fmt.Sprintf("wfc-%s-%s-g%d", phase, runID, generation)
}

// repairRunBranch names the branch a repair's commit sits on, for the sentence
// a person reads when a promotion is refused. A repair whose branch AO cannot
// read still names its run, which is enough to find it.
func (c *Coordinator) repairRunBranch(ctx stdctx.Context, repairRunID string) string {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, repairRunID)
	if err != nil {
		return repairRunID
	}
	branch := ""
	for _, cp := range cps {
		if cp.Branch != "" {
			branch = cp.Branch
		}
	}
	if branch == "" {
		return repairRunID
	}
	return branch
}
