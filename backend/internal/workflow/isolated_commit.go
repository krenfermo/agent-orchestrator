package workflow

import (
	stdctx "context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// isolated_commit.go — F5: an isolated task's verified result becomes a commit.
//
// THE INCIDENT. A three-task objective reported `tasksIntegrated: 3, status:
// ok` and completed, while the third task's entire deliverable — a new
// src/farewell.js plus two edited files — existed only as dirty, untracked
// state in a worktree the lifecycle had already retired. Integration had
// resolved that task's branch head, found it identical to the target, taken the
// fast-forward path, and recorded base == head as a successful integration.
//
// Nothing was wrong with the integration arithmetic. The task's work had simply
// never become a commit. Two of the three workers happened to run `git commit`
// on their own; the third did not, and nothing in the prompt asks them to (see
// plan.go's guardrails, which say only "do not push, do not merge, do not touch
// another branch"). Whether an objective's work survives must not depend on
// that.
//
// THE FIX, and why it belongs exactly here. completeVerifiedRun already commits
// a DIRECT-BRANCH run's work in the window between "verified" and "completed",
// while the branch lock still proves the repository is this run's to write, and
// it already refuses to complete a run whose commit failed:
//
//	"reporting a run as completed while its work sits uncommitted would be
//	 exactly the kind of untruthful state this codebase refuses everywhere else"
//
// That is precisely the guarantee F5 lacked. autonomousLocalCommit simply
// returned nil for every non-direct-branch run, so an isolated task never
// reached it. This file is the isolated half of that same boundary, with the
// same authority window and the same fail-closed answer.
//
// WHY AFTER VERIFY RATHER THAN BEFORE REVIEW. Review and verify both observe
// the WORKTREE, and their verdicts are recorded against a workspace
// fingerprint. Committing before them would change the thing they are about to
// look at and invalidate every fingerprint downstream. Committing after them,
// in this window, means the commit captures the exact tree that was reviewed
// and verified — which this file then proves rather than assumes, by requiring
// the tree it is about to commit to carry the same fingerprint the passing
// verification recorded.

// isolatedCommitPhase is the durable record that an isolated task's verified
// result became a commit. Deliberately the SAME phase direct-branch uses:
// task_integration_baseline.go, mutation_boundaries.go and incident_repair.go
// all already read it as "the verified result was committed at this SHA", and
// a second phase name meaning the same thing would leave each of them with one
// case they do not handle.
const isolatedCommitPhase = autonomousLocalCommitPhase

// isolatedNoChangePhase records a mutating isolated task that finished with
// nothing to commit. It is a distinct fact from "committed at SHA", and it is
// recorded rather than inferred so a later reader can tell a task that produced
// no change from one whose change was lost. No empty commit is ever created.
const isolatedNoChangePhase = "isolated_no_change_outcome"

// commitIsolatedWorktree is the isolated-placement half of autonomousLocalCommit.
//
// It returns an error only when the work is real and AO could not durably
// capture it; every "nothing to do" answer is a nil, because a task with no
// pending changes has no obligation here.
func (c *Coordinator) commitIsolatedWorktree(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) error {
	if c.workspaceCommitter == nil || c.workspaceFacts == nil {
		// Nothing to do, and deliberately not an error. A deployment without a
		// committer cannot capture the result here, but it also cannot lose it
		// silently: integrateIsolatedTask refuses to integrate a worktree that
		// still holds uncommitted work, so the fail-closed half of F5 does not
		// depend on this half being wired. Erroring here instead would fail
		// every run in a build that never had a committer, which is a different
		// bug rather than a stricter version of this one.
		return nil
	}
	target, ok := c.isolatedCommitTarget(ctx, run)
	if !ok {
		// No isolated worktree AO can prove it owns. Not an error: a run
		// without a readable isolated placement is not one this boundary
		// governs, and the integration side refuses it separately rather than
		// having this write into a directory it cannot attribute.
		return nil
	}

	before, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path: target.worktreePath, Branch: target.branch, ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return fmt.Errorf("observing this run's worktree before committing its verified result: %w", err)
	}
	// §5: the worktree must still be the one this run owns. A placement that
	// says branch X and a directory that is on branch Y is not this task's
	// worktree any more, and committing into it would put this task's name on
	// somebody else's work.
	if strings.TrimSpace(before.Branch) != "" && strings.TrimSpace(target.branch) != "" &&
		strings.TrimSpace(before.Branch) != strings.TrimSpace(target.branch) {
		return fmt.Errorf("this run's worktree is on branch %q but its placement says %q, so AO cannot prove the pending work is this task's",
			before.Branch, target.branch)
	}

	if !before.Dirty {
		// §16: nothing to commit. Never an empty commit. Whether the branch
		// already carries the work (the worker committed it itself) or the task
		// genuinely changed nothing is not this function's question — it is
		// recorded as an outcome and answered by the integration gate, which
		// compares the branch against its base.
		c.recordIsolatedNoChange(ctx, run, step, target, before.HeadSHA)
		return nil
	}

	// §4: the tree about to be committed must be the tree that was verified.
	// A worktree that changed after verification is not covered by that
	// verdict, and committing it would launder an unverified state into an
	// integration candidate.
	if err := c.requireVerifiedTree(ctx, run, before); err != nil {
		return err
	}

	sha, committed, err := c.workspaceCommitter.CommitAll(ctx, ports.WorkspaceInfo{
		Path: target.worktreePath, Branch: target.branch,
		ProjectID: domain.ProjectID(run.ProjectID), RepoPath: target.repoPath,
	}, autonomousCommitMessage(run))
	if err != nil {
		return fmt.Errorf("committing this run's verified result in %s: %w", target.worktreePath, err)
	}

	// §6/§17-B: the re-probe is also the idempotency proof. CommitAll on an
	// already-clean tree commits nothing and reports committed=false, so a
	// restart between the commit and this checkpoint re-observes the same HEAD
	// and records it once rather than creating a second, identical commit.
	after, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path: target.worktreePath, Branch: target.branch, ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return fmt.Errorf("re-reading this run's worktree after committing its verified result: %w", err)
	}
	if after.Dirty {
		// A commit that leaves the tree dirty has not captured the result. The
		// honest answer is to stop, because the integration candidate would not
		// be the verified state.
		return fmt.Errorf("this run's worktree still has pending changes after AO committed its verified result, so the commit does not represent what was verified")
	}
	if sha == "" {
		sha = after.HeadSHA
	}
	if !committed && sha == "" {
		return fmt.Errorf("AO could not determine the commit that captures this run's verified result")
	}
	return c.recordIsolatedCommit(ctx, run, step, target, sha)
}

// isolatedCommitTarget is where an isolated run's verified work lives.
type isolatedCommitTarget struct {
	repoPath     string
	worktreePath string
	branch       string
}

// isolatedCommitTarget resolves the worktree from the run's OWN durable
// placement, never from the project's current configuration: the question is
// where this run's work physically is, and only the placement answers it.
func (c *Coordinator) isolatedCommitTarget(ctx stdctx.Context, run domain.WorkflowRun) (isolatedCommitTarget, bool) {
	recall, ok := c.recallPlacement(ctx, run)
	if !ok || recall.Type != domain.PlacementIsolatedWorktree {
		return isolatedCommitTarget{}, false
	}
	worktree := strings.TrimSpace(recall.WorktreePath)
	branch := strings.TrimSpace(recall.ExecutionBranch)
	if worktree == "" || branch == "" {
		return isolatedCommitTarget{}, false
	}
	return isolatedCommitTarget{
		repoPath: strings.TrimSpace(recall.RepoPath), worktreePath: worktree, branch: branch,
	}, true
}

// requireVerifiedTree proves the pending tree is the one the run's passing
// verification actually judged.
//
// A run whose plan asked for no verification has no fingerprint to compare
// against, and refusing it would make this boundary stricter than the
// verification contract itself — so an absent verdict is not treated as a
// mismatch. What is refused is a verdict that exists and describes a DIFFERENT
// tree, which is the case where committing would misrepresent what passed.
func (c *Coordinator) requireVerifiedTree(ctx stdctx.Context, run domain.WorkflowRun, observed ports.WorkspaceObservation) error {
	_, result, ok := latestPassingVerifyResult(ctx, c, run.ID)
	if !ok {
		return nil
	}
	verified := strings.TrimSpace(result.PostFingerprint)
	if verified == "" {
		verified = strings.TrimSpace(result.PreFingerprint)
	}
	if verified == "" {
		return nil
	}
	if WorkspaceFingerprint(observed) == verified {
		return nil
	}
	return fmt.Errorf("this run's worktree changed after its verification passed, so the pending work is not the state that was verified and AO will not commit it as the verified result")
}

// recordIsolatedCommit persists the committed candidate. Unlike the rest of the
// observations in this area it is NOT best-effort: the commit exists, and a
// caller that cannot record it must not go on to report the run completed, or
// the next pass has a commit nobody can attribute.
func (c *Coordinator) recordIsolatedCommit(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, target isolatedCommitTarget, sha string) error {
	stepID := step.ID
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		Branch:         target.branch,
		WorktreePath:   target.worktreePath,
		HeadSHA:        sha,
		NextAction:     "local_commit_created: " + sha + " on " + target.branch,
		DurablePhase:   isolatedCommitPhase,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	})
	return err
}

// recordIsolatedNoChange records that a mutating isolated task finished with
// nothing pending. Best-effort: it explains an outcome rather than authorising
// one, and the integration gate reaches the same answer from the branch itself.
func (c *Coordinator) recordIsolatedNoChange(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, target isolatedCommitTarget, head string) {
	stepID := step.ID
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		Branch:         target.branch,
		WorktreePath:   target.worktreePath,
		HeadSHA:        head,
		NextAction:     "no_pending_changes: this task's worktree had nothing left to commit at " + head,
		DurablePhase:   isolatedNoChangePhase,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	})
}
