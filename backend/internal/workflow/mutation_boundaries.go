package workflow

import (
	stdctx "context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// mutation_boundaries.go — the call sites (P2-D §5).
//
// mutation_provenance.go says HOW a boundary is recorded. This file says WHICH
// moments are boundaries, and it is deliberately short: there are three
// writers, at three places AO already had to stop and think about durability,
// and adding a fourth should require the same argument these three make.
//
//	verified / work_result   completeVerifiedRun, after the autonomous commit
//	                         and while the branch lock is still held. The first
//	                         instant the work is both good and durable.
//	repair_result            the same place, when a repair produced the head
//	                         rather than the original worker.
//	integrated               handleIntegrationOutcome, the single point where
//	                         both execution modes agree the target ref moved.
//
// None of them is on a hot path, all three are best-effort, and none of them
// can change what the run's own state says happened.

// boundaryGeneration is the durable generation a boundary is recorded under.
//
// It is the number of attempts the step has made, which has the three
// properties the CAS needs and no others do:
//
//   - **Durable.** It is a count of rows, not of anything this process
//     remembers, so it survives a restart unchanged.
//   - **Monotonic.** A new attempt is the only thing that raises it, and a new
//     attempt IS a new generation of the work.
//   - **Stable under duplicate delivery.** Two callbacks about the same
//     attempt compute the same number, which is what lets the idempotency key
//     collapse them instead of producing two rows a generation apart.
//
// A step whose attempts cannot be read yields 0 rather than a guess. Zero is
// the weakest generation, so it can be superseded by anything and supersedes
// nothing — which is the correct behaviour for "AO does not know how many
// attempts this was".
func (c *Coordinator) boundaryGeneration(ctx stdctx.Context, stepID string) int64 {
	if c.store == nil || strings.TrimSpace(stepID) == "" {
		return 0
	}
	attempts, err := c.store.ListWorkflowAttempts(ctx, stepID)
	if err != nil {
		return 0
	}
	return int64(len(attempts))
}

// memoryRepoIdentity reads the durable identity of the repository a run works
// in, for stamping onto provenance and memory.
//
// A repository AO cannot identify yields the empty identity, and the empty
// identity never matches anything — including another empty one. That is the
// fail-closed direction: an unidentifiable checkout costs a re-derivation, and
// can never let one project inherit another's knowledge (P2-D §9).
func (c *Coordinator) memoryRepoIdentity(ctx stdctx.Context, repoPath string) domain.RepoIdentity {
	if strings.TrimSpace(repoPath) == "" {
		return ""
	}
	return projectmemory.RepoIdentityOf(ctx, repoPath)
}

// recordVerifiedBoundary records what a verified, committed run produced.
//
// It is called from completeVerifiedRun after autonomousLocalCommit and while
// the branch lock is still held — the same instant recordTaskMemory is called,
// and for the same reason: it is the first moment at which "this is the work,
// and it is durably at this commit" is a fact rather than an expectation.
//
// It writes TWO boundaries when a repair produced the result, not one. The
// worker's result and the repair's result are different moments with different
// authors, and collapsing them would make the repair invisible to anything
// that later asks who produced the head — which is exactly the attribution
// P2-D §16 requires to survive.
func (c *Coordinator) recordVerifiedBoundary(
	ctx stdctx.Context, run domain.WorkflowRun, verifyStep domain.WorkflowStep, headSHA string,
) {
	if c.mutationProvenance == nil {
		return
	}
	_, repoPath, ok := c.memoryProjectRoot(ctx, run)
	if !ok {
		return
	}
	placement := c.mutationPlacementFor(ctx, run)

	// The work step's own dispatch record: the branch, the worktree and the
	// commit AO authorized the work against. Without it there is no "before",
	// and a boundary with no before is an observation rather than an
	// attribution — so it is recorded honestly with empty fields rather than
	// with values borrowed from somewhere plausible.
	var workCP domain.WorkflowCheckpoint
	workStepID, repairStepID, repaired := "", "", false
	if steps, err := c.store.ListWorkflowSteps(ctx, run.ID); err == nil {
		for _, s := range steps {
			switch s.Kind {
			case domain.WorkflowStepWork:
				workStepID = s.ID
				if cp, found := c.workDispatchCheckpoint(ctx, run.ID, s.ID); found {
					workCP = cp
				}
			case domain.WorkflowStepFix:
				// A fix step that actually ran is a repair: the head below is
				// not the original worker's output, and must not be attributed
				// to it.
				if s.State == domain.WorkflowStepCompleted || s.State == domain.WorkflowStepRunning {
					repairStepID, repaired = s.ID, true
				}
			}
		}
	}

	sessionID := ""
	if workCP.SessionID != nil {
		sessionID = *workCP.SessionID
	}
	base := mutationBoundary{
		run:          run,
		taskID:       taskRefFor(run),
		placement:    placement,
		class:        domain.MutationAuthorizedWork,
		repoPath:     repoPath,
		repoIdentity: c.memoryRepoIdentity(ctx, repoPath),
		branch:       workCP.Branch,
		worktreePath: workCP.WorktreePath,
		sessionID:    sessionID,
		baseSHA:      workCP.BaseSHA,
		headSHA:      headSHA,
	}
	if placement == domain.MutationPlacementDirectBranch {
		// Direct-branch work lands on the target ref itself: there is no
		// separate integration operation, and the commit IS the integration.
		base.integrationMethod = domain.IntegrationDirectCommit
	}

	// P3-A: whether an autonomous local commit actually happened, read from the
	// durable record rather than assumed from the code path.
	//
	// autonomousLocalCommit returns success in four cases where it commits
	// nothing -- no committer wired, a project not in direct-branch mode, no
	// branch lock held, and a git policy of require_approval or never -- and
	// the reasons below used to state a commit in every one of them. A
	// provenance reason is read long after the fact by somebody deciding
	// whether the work is safe, so it has to describe what was proved, not the
	// path that was expected. commitHeldRepositories writes its checkpoint only
	// when git reported a commit, so the checkpoint's presence IS the proof.
	committed := c.recordedAutonomousCommit(ctx, run.ID)

	if repaired {
		repair := base
		repair.step = &domain.WorkflowStep{ID: repairStepID}
		repair.boundary = domain.BoundaryRepairResult
		repair.class = domain.MutationAuthorizedFix
		repair.generation = c.boundaryGeneration(ctx, repairStepID)
		repair.reason = "a fix step ran after review, so the verified head is the repair's result and not the original worker's"
		c.recordMutationBoundary(ctx, repair)
	} else {
		work := base
		work.boundary = domain.BoundaryWorkResult
		work.generation = c.boundaryGeneration(ctx, workStepID)
		work.reason = "the work step's result, observed at the end of verification" + commitSuffix(committed)
		c.recordMutationBoundary(ctx, work)
	}

	// The verified boundary is recorded whether or not a repair ran, because
	// it answers a different question: not "who produced this head" but "what
	// did verification actually pass on". Memory pins its VerifiedCommit to
	// this row, and nothing else.
	verified := base
	verified.step = &verifyStep
	verified.boundary = domain.BoundaryVerified
	verified.generation = c.boundaryGeneration(ctx, verifyStep.ID)
	verified.reason = "verification passed while the branch lock was still held" + commitSuffix(committed)
	verified.evidence = map[string]any{
		"repaired":  repaired,
		"placement": string(placement),
	}
	c.recordMutationBoundary(ctx, verified)
}

// commitSuffix says what happened to the work, in the two words that are
// actually different for whoever reads this row later.
func commitSuffix(committed bool) string {
	if committed {
		return "; AO committed the result locally"
	}
	return "; the result was left uncommitted in the workspace"
}

// recordedAutonomousCommit reports whether this run durably recorded an
// autonomous local commit. The checkpoint is written only after git confirmed
// one, so its absence is the absence of a commit rather than of a record.
func (c *Coordinator) recordedAutonomousCommit(ctx stdctx.Context, runID string) bool {
	if c.store == nil || runID == "" {
		return false
	}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	for _, cp := range checkpoints {
		if cp.DurablePhase == autonomousLocalCommitPhase {
			return true
		}
	}
	return false
}

// recordIntegratedBoundary records that a task's work reached a target ref.
//
// Everything it writes comes from the Integration Coordinator's own Record,
// which is the only account of the operation that is not re-derivable: the
// target SHAs are gone the moment the ref moves again, and the strategy is a
// decision rather than an observation. Copying them here is what makes a
// promotion checkable long after the fact.
func (c *Coordinator) recordIntegratedBoundary(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	child RunDetail,
	workCP domain.WorkflowCheckpoint,
	rec integration.Record,
) {
	if c.mutationProvenance == nil {
		return
	}
	if strings.TrimSpace(rec.TargetAfterSHA) == "" {
		// An integration that cannot name where the target ended up has not
		// proven anything, and a row saying so would be worse than no row: a
		// promotion would find it, see boundary = integrated, and treat the
		// absence of the SHA as an oversight rather than as the refusal it is.
		if c.log != nil {
			c.log.Warn("workflow: not recording an integration boundary with no target head",
				"run", parent.ID, "task", task.ID)
		}
		return
	}

	repoPath := strings.TrimSpace(rec.RepoPath)
	if repoPath == "" {
		if _, root, ok := c.memoryProjectRoot(ctx, parent); ok {
			repoPath = root
		}
	}
	placement := domain.MutationPlacementIsolatedWorktree
	if rec.Strategy == integration.StrategyNoOp {
		placement = domain.MutationPlacementDirectBranch
	}
	sessionID := ""
	if workCP.SessionID != nil {
		sessionID = *workCP.SessionID
	}

	// The generation is the CHILD run's verify generation, not the parent's
	// and not a count of integrations. An integration callback belonging to an
	// older attempt of the same task carries an older number and is refused;
	// a duplicate callback about this attempt carries the same one and is
	// collapsed by the idempotency key.
	generation := int64(0)
	for _, sd := range child.Steps {
		if sd.Step.Kind == domain.WorkflowStepVerify {
			generation = int64(len(sd.Attempts))
			break
		}
	}

	c.recordMutationBoundary(ctx, mutationBoundary{
		run:          parent,
		taskID:       task.ID,
		boundary:     domain.BoundaryIntegrated,
		placement:    placement,
		class:        domain.MutationAuthorizedWork,
		generation:   generation,
		repoPath:     repoPath,
		repoIdentity: c.memoryRepoIdentity(ctx, repoPath),
		branch:       rec.SourceBranch,
		worktreePath: workCP.WorktreePath,
		sessionID:    sessionID,
		baseSHA:      rec.BaseSHA,
		// HeadSHA is the SOURCE commit that actually reached the target --
		// after any rebase, so it is what landed rather than what the task
		// last reported.
		headSHA: rec.SourceSHA,

		integrationTargetRef:       rec.TargetRef,
		integrationTargetBeforeSHA: rec.TargetBeforeSHA,
		integrationTargetAfterSHA:  rec.TargetAfterSHA,
		integrationMethod:          integrationMethodOf(rec.Strategy),

		reason: "the integration coordinator moved the target ref under its lane",
		evidence: map[string]any{
			"strategy":     string(rec.Strategy),
			"targetBranch": rec.TargetBranch,
			"replayed":     rec.Replayed,
			"reviewReused": rec.ReviewReused,
		},
	})
}
