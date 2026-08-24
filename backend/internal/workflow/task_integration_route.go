package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// taskBaseWasOvertaken reports whether the integration ref moved past the
// commit this task's worktree was cut from.
//
// Both empties are deliberate answers rather than missing data:
//
//   - currentSHA == "" is the first promotion of a master run. Nothing has been
//     integrated, so nothing can have been overtaken.
//   - baseSHA == "" means the dispatcher could not observe the worktree's head
//     when it started (workspaceFacts absent, or the observation failed). That
//     is not evidence of drift, and treating an unknown as drift would route
//     every task through a replay it does not need.
func (c *Coordinator) taskBaseWasOvertaken(baseSHA, currentSHA string) bool {
	baseSHA, currentSHA = strings.TrimSpace(baseSHA), strings.TrimSpace(currentSHA)
	return currentSHA != "" && baseSHA != "" && baseSHA != currentSHA
}

// integrateReadyTask is the ONE place a ready task's work reaches a target.
//
// It used to be one of three. A direct-branch task returned early into its own
// promotion, an isolated task whose cached base still matched the cached target
// head went through MaterializeIntegrationCommit, and only a task AO had
// already decided was overtaken came here. Those legacy routes did not take the
// integration lane, did not apply the readiness gate at the ref, and recorded
// no strategy/source/target-before/target-after audit — and the overtaken
// decision itself was made from CACHED state before any lock, so a target that
// moved in between was integrated against a head nobody had read.
//
// Now every mode arrives here and the Coordinator answers the same questions in
// the same order for all of them: acquire the lane, read the target inside it,
// gate on readiness, choose a strategy, land under compare-and-set, record.
// "The target did not move" is not a shortcut past that — it is simply the
// fast-forward strategy, decided from a head read under the lock rather than
// from what the dispatcher remembered.
func (c *Coordinator) integrateReadyTask(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	child RunDetail,
	workCP domain.WorkflowCheckpoint,
	state MasterIntegrationState,
	project domain.ProjectRecord,
) error {
	if domain.ResolveExecutionMode(project.Kind, project.Config) == domain.ExecutionDirectBranch {
		// The project is not passed on: a direct-branch integration reads its
		// repository from the child's own durable evidence, never from what the
		// project record says now. The two can disagree — a project whose
		// registered path changed after the task ran — and only the first
		// describes where the verified work actually is.
		return c.integrateDirectBranchTask(ctx, parent, task, child, workCP, state)
	}
	return c.integrateIsolatedTask(ctx, parent, task, child, workCP, state, project)
}

// integrateIsolatedTask integrates work that lives in an AO-owned worktree.
func (c *Coordinator) integrateIsolatedTask(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	child RunDetail,
	workCP domain.WorkflowCheckpoint,
	state MasterIntegrationState,
	project domain.ProjectRecord,
) error {
	if c.integrationLocks == nil {
		// Without the lane there is no exclusion, and integrating two tasks onto
		// one ref concurrently is the exact failure this path exists to prevent.
		// Refusing is what turns a missing dependency into a visible stop rather
		// than a silent loss.
		return c.recordIntegrationFailure(ctx, parent, task,
			"integration_lane_unavailable: this task's base was overtaken and no integration lock manager is configured")
	}
	if workCP.Branch == "" {
		// A replay needs commits to replay. Without a branch there is only a
		// working tree, and nothing that can be rebased onto the moved target.
		return c.recordIntegrationFailure(ctx, parent, task,
			"integration_no_source_branch: this task's base was overtaken but its worktree has no branch to replay")
	}

	artifact, err := c.planArtifactForRun(ctx, child.Run)
	if err != nil {
		return err
	}
	coordinator, err := integration.New(integration.Deps{
		Git:      integration.NewExecGit(""),
		Locks:    c.integrationLocks,
		Verifier: integrationVerifier{c: c, plan: artifact.Verification},
		Recorder: integrationLedger{c: c, parent: parent},
		Clock:    c.clock,
	})
	if err != nil {
		return c.recordIntegrationFailure(ctx, parent, task, err.Error())
	}

	sessionID := ""
	if workCP.SessionID != nil {
		sessionID = *workCP.SessionID
	}
	// What this task's plan says has to be on the target before it may land,
	// paired with where each of those integrations actually left the ref. The
	// Coordinator proves the second half against the ref itself; nothing here
	// is evidence that anything landed.
	deps, err := c.integrationDependencies(ctx, parent, task)
	if err != nil {
		return err
	}
	outcome, err := coordinator.Integrate(ctx, integration.Request{
		ProjectID:     domain.ProjectID(parent.ProjectID),
		WorkflowRunID: parent.ID,
		TaskID:        task.ID,
		SessionID:     sessionID,
		RepoPath:      project.Path,
		WorktreePath:  workCP.WorktreePath,
		Dependencies:  deps,
		// Only the ref is named; the lane is derived from it (targetLaneName).
		// Two master runs integrating their own refs in one repository
		// therefore do not serialize against each other, while two tasks of ONE
		// run always do.
		TargetRef:    state.RefName,
		SourceBranch: workCP.Branch,
		BaseSHA:      workCP.BaseSHA,
		Readiness:    taskReadiness(child),
		// The verification the child already passed, and the identity of the
		// worktree as it is now. The Coordinator reuses the first only while it
		// still describes the second — a worktree touched after it was verified
		// gets re-verified rather than credited with an old verdict.
		//
		// Observed before the lane, unlike direct-branch's: this worktree is
		// AO's own, created for this one task, and nothing outside this
		// integration writes to it.
		Verified:          c.taskVerificationEvidence(ctx, child, artifact.Verification, ""),
		SourceFingerprint: c.observedWorktreeFingerprint(ctx, parent, workCP),
	})
	if handled, herr := c.handleIntegrationOutcome(ctx, parent, task, child, workCP, outcome, err); handled {
		return herr
	}

	// The ref has moved. Record the promotion on the master's own ledger in the
	// same shape the serial path uses, so integration state folds identically
	// however the task got there.
	payload, _ := json.Marshal(masterIntegrationPromotionPayload{
		TaskID:  task.ID,
		RefName: state.RefName,
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  parent.ID,
		ProjectID:      parent.ProjectID,
		SessionID:      workCP.SessionID,
		BaseSHA:        outcome.Record.TargetBeforeSHA,
		HeadSHA:        outcome.Record.TargetAfterSHA,
		RetryState:     string(payload),
		DurablePhase:   masterIntegrationDurablePhase,
		PayloadVersion: masterIntegrationPayloadVersion,
		CreatedAt:      c.clock(),
	}); err != nil {
		return err
	}

	// Only now. The promotion is durable, so no later pass will try to read
	// this task's ao/* branch in order to integrate it again -- which is the
	// single precondition for being allowed to delete that branch at all.
	// Cleaning up any earlier would mean a crash between the two left a task
	// that still has to be integrated and no longer has the commits to
	// integrate. See task_worktree_lifecycle.go.
	c.finishTaskWorktree(ctx, parent, task, outcome.Record.TargetAfterSHA)
	return nil
}

// errIntegrationBusy means the lane was occupied. It is separated from
// errIntegrationFailed because it must NOT park the master in needs_attention:
// nothing is wrong, and the next reconcile pass will try again.
var errIntegrationBusy = errors.New("workflow: target integration lane is busy")

// describeIntegrationAttention renders an Attention into the one-line reason
// the master's attention stop carries. It names the files and all three SHAs,
// because a stop that says "conflict" without them cannot be acted on.
func describeIntegrationAttention(att integration.Attention) string {
	var b strings.Builder
	b.WriteString(string(att.Reason))
	b.WriteString(": ")
	b.WriteString(att.Detail)
	if len(att.ConflictFiles) > 0 {
		b.WriteString(" — conflicting files: ")
		b.WriteString(strings.Join(att.ConflictFiles, ", "))
	}
	fmt.Fprintf(&b, " — base %s, target %s, source %s", shortSHA(att.BaseSHA), shortSHA(att.TargetSHA), shortSHA(att.SourceSHA))
	return b.String()
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// integrateDirectBranchTask integrates work the task committed straight onto
// the project's own branch — through the SAME Integration Coordinator every
// other mode goes through.
//
// It used to be a second road. It took the lane, gated on readiness and wrote
// an audit row, but it did all three itself, alongside the Coordinator rather
// than inside it, on the argument that a promotion with no git operation had
// nothing to route. That argument is wrong twice over. "No git operation" is a
// property of one strategy, not of a mode — the Coordinator now names it
// (StrategyNoOp) and supports it explicitly — and a second implementation of
// the lane, the gate and the audit is a second place for all three to drift,
// which is exactly what the first version of this path did (it recorded no
// strategy or SHAs at all until a reviewer noticed).
//
// So what is left here is only what is genuinely different about the mode, and
// all of it is expressed as INPUTS to the one integration path:
//
//   - NoReplay, because the work is already on the target ref and this package
//     must never rebase a checkout AO does not own;
//   - a Precondition, which is the "the branch still holds exactly what was
//     reviewed and verified" proof, evaluated inside the lane against the
//     branch as it actually is, and returning the identity it observed;
//   - the durable verification the child already passed, so the audit records
//     what authorized this instead of "verificationRan: false".
func (c *Coordinator) integrateDirectBranchTask(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	child RunDetail,
	workCP domain.WorkflowCheckpoint,
	state MasterIntegrationState,
) error {
	if c.integrationLocks == nil {
		return c.recordIntegrationFailure(ctx, parent, task,
			"integration_lane_unavailable: no integration lock manager is configured")
	}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, child.Run.ID)
	if err != nil {
		return err
	}
	evidence, ok := directBranchPromotionEvidence(checkpoints, workCP)
	// A task whose branch moved past its own work commit, and which has since
	// had an independent review AND a verification pass against the head as it
	// now stands, is certified fit to integrate at THAT head. The work commit
	// is unchanged and still on the record; what the baseline replaces is only
	// the answer to "what was last certified", which is the question freshness
	// is actually asking. See task_integration_baseline.go.
	if ok && evidence.Kind == directBranchEvidenceCommitted {
		if baseline, has := c.verifiedIntegrationBaseline(ctx, child.Run.ID); has && baseline.VerifiedIntegrationCommit != "" {
			evidence.CommitSHA = baseline.VerifiedIntegrationCommit
		}
	}
	if !ok {
		return c.recordIntegrationFailure(ctx, parent, task,
			"directbranch: the execution run recorded no durable proof that its verified result reached the target branch (its local-commit policy may have deferred or skipped the commit)")
	}
	branch := strings.TrimSpace(evidence.Branch)
	if branch == "" {
		return c.recordIntegrationFailure(ctx, parent, task,
			"directbranch: the verified result records no branch, so there is no lane to integrate on")
	}

	artifact, err := c.planArtifactForRun(ctx, child.Run)
	if err != nil {
		return err
	}
	coordinator, err := integration.New(integration.Deps{
		Git:   integration.NewExecGit(""),
		Locks: c.integrationLocks,
		// Supplied even though a direct-branch request has no AO worktree to
		// run commands in: the Coordinator refuses to use it without one, and
		// leaving it out would make that refusal look like a missing dependency
		// rather than the deliberate confinement it is.
		Verifier: integrationVerifier{c: c, plan: artifact.Verification},
		Recorder: integrationLedger{c: c, parent: parent},
		Clock:    c.clock,
	})
	if err != nil {
		return c.recordIntegrationFailure(ctx, parent, task, err.Error())
	}

	sessionID := ""
	if workCP.SessionID != nil {
		sessionID = *workCP.SessionID
	}
	deps, err := c.integrationDependencies(ctx, parent, task)
	if err != nil {
		return err
	}
	// The head the proof actually ran against, captured by the precondition so
	// the promotion checkpoint below records the branch as it was INSIDE the
	// lane rather than as anyone remembered it.
	provenHead := ""
	outcome, err := coordinator.Integrate(ctx, integration.Request{
		ProjectID:     domain.ProjectID(parent.ProjectID),
		WorkflowRunID: parent.ID,
		TaskID:        task.ID,
		SessionID:     sessionID,
		RepoPath:      evidence.RepoPath,
		// No worktree: this integration may not run a mutating git command
		// anywhere, and NoReplay is what makes that a guarantee rather than a
		// convention.
		WorktreePath: "",
		TargetBranch: branch,
		SourceBranch: branch,
		BaseSHA:      evidence.CommitSHA,
		Dependencies: deps,
		NoReplay:     true,
		Readiness:    taskReadiness(child),
		Verified:     c.taskVerificationEvidence(ctx, child, artifact.Verification, directBranchVerifiedIdentity(evidence)),
		Precondition: func(pctx stdctx.Context, targetSHA, _ string) (string, error) {
			observed, identity, perr := c.observeDirectBranchTarget(pctx, parent, evidence)
			if perr != nil {
				return "", perr
			}
			provenHead = observed.HeadSHA
			return identity, c.directBranchFreshness(pctx, evidence, observed)
		},
	})
	if handled, herr := c.handleIntegrationOutcome(ctx, parent, task, child, workCP, outcome, err); handled {
		return herr
	}

	if provenHead == "" {
		provenHead = outcome.Record.TargetAfterSHA
	}
	payload, _ := json.Marshal(masterIntegrationPromotionPayload{
		TaskID: task.ID, Mode: masterIntegrationModeDirectBranch, Branch: branch,
	})
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  parent.ID,
		ProjectID:      parent.ProjectID,
		Branch:         branch,
		WorktreePath:   evidence.RepoPath,
		SessionID:      workCP.SessionID,
		BaseSHA:        state.CurrentSHA,
		HeadSHA:        provenHead,
		RetryState:     string(payload),
		DurablePhase:   masterIntegrationDurablePhase,
		PayloadVersion: masterIntegrationPayloadVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// observeDirectBranchTarget reads the target repository under the lane and
// returns both the observation and the content identity to compare the task's
// verification against.
//
// The identity is scheme-matched to the evidence on purpose. A committed result
// is identified by its commit, because that commit IS the durable form of the
// verified content — AO made it from the verified workspace while still holding
// the branch lock. A result that had nothing to commit is identified by the
// workspace fingerprint, because there is no commit of its own to name.
func (c *Coordinator) observeDirectBranchTarget(ctx stdctx.Context, parent domain.WorkflowRun, evidence directBranchEvidence) (ports.WorkspaceObservation, string, error) {
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path: evidence.RepoPath, Branch: evidence.Branch,
		ProjectID: domain.ProjectID(parent.ProjectID), RepoPath: evidence.RepoPath,
	})
	if err != nil {
		return ports.WorkspaceObservation{}, "", errors.New("target branch could not be observed: " + err.Error())
	}
	if evidence.Kind == directBranchEvidenceCommitted {
		return obs, obs.HeadSHA, nil
	}
	return obs, WorkspaceFingerprint(obs), nil
}

// directBranchVerifiedIdentity is the identity the child's verification
// describes, in the same scheme observeDirectBranchTarget reports.
func directBranchVerifiedIdentity(evidence directBranchEvidence) string {
	if evidence.Kind == directBranchEvidenceCommitted {
		return evidence.CommitSHA
	}
	return evidence.Fingerprint
}

// handleIntegrationOutcome is the single answer to "the Coordinator came back"
// — shared by both modes, because after the Coordinator there is nothing
// mode-specific left to decide.
//
// handled=false means the integration landed and the caller should record its
// own promotion. Everything else is terminal here.
func (c *Coordinator) handleIntegrationOutcome(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	child RunDetail,
	workCP domain.WorkflowCheckpoint,
	outcome integration.Outcome,
	err error,
) (bool, error) {
	switch {
	case errors.Is(err, integration.ErrDependencyPending):
		// A task this one requires has not landed yet. Nothing is wrong and
		// nothing is recorded: the task stays exactly where it is, its siblings
		// are untouched, and the pass that follows the dependency's own
		// integration finds it ready.
		return true, fmt.Errorf("%w: %s", errIntegrationWaitingOnDependency, err.Error())
	case errors.Is(err, integration.ErrLockBusy):
		// Another task of this run owns the lane. Nothing is wrong and nothing
		// is recorded: this reconcile pass leaves the task where it is, and the
		// next one (every GetRun, i.e. every board poll) retries.
		return true, fmt.Errorf("%w: another task is integrating this target", errIntegrationBusy)
	case errors.Is(err, integration.ErrNotReady):
		// The gate refused. This is the guarantee that a failed review or a
		// failed verification cannot reach the target, enforced at the ref
		// rather than only at the caller that decided to try.
		return true, c.recordIntegrationFailure(ctx, parent, task, "integration_not_ready: "+err.Error())
	case err != nil && outcome.Integrated:
		// The physical side is done and the audit is not — the two live in
		// different stores and no ordering of the two writes survives every
		// crash. Never revert the physical side to match a failed audit, and
		// never report success without one: the promotion is refused, the
		// outstanding audit is recorded, and a later pass finishes it without
		// redoing anything physical.
		return true, c.recordIntegrationAuditPending(ctx, parent, task, outcome.Record, err)
	case err != nil:
		return true, c.recordIntegrationFailure(ctx, parent, task, err.Error())
	case outcome.Attention != nil && outcome.Attention.Reason == integration.ReasonStaleReviewAfterRebase:
		// The one attention AO can answer by itself. The work is fine and the
		// target is fine; what went stale is a reviewer's opinion of a change
		// the replay altered, and AO can go and ask for a new one rather than
		// parking a person on it. It parks only when it cannot ask.
		return true, c.requestIntegrationFreshReview(ctx, parent, task, child, workCP, outcome.Record)
	case outcome.Attention != nil:
		// A conflict belongs to the TASK, not to the objective. Parking the
		// master here would stop every unrelated sibling on one task's merge
		// problem — the exact opposite of what parallel execution is for.
		return true, c.recordTaskIntegrationConflict(ctx, parent, task, *outcome.Attention)
	}
	// The audit landed, so any earlier pending marker for this task is answered.
	c.clearIntegrationAuditPending(ctx, parent, task)
	// And so is any fresh review this integration asked for on the way here.
	c.closeIntegrationFreshReview(ctx, child)
	return false, nil
}

// verifiedWorkIsStillOnBranch reports whether the commit this task's
// verification passed on is still reachable from the branch head.
//
// It answers with git rather than with a stored fact on purpose: the question
// is about the ref as it is now, and only the repository can answer it. An
// unreadable repository, a missing commit or any error at all answers NO — the
// caller then parks for a person, which is the safe direction. Claiming the
// work is intact because a read failed would be exactly the assumption this
// whole check exists to refuse.
func (c *Coordinator) verifiedWorkIsStillOnBranch(ctx stdctx.Context, evidence directBranchEvidence, headSHA string) bool {
	if evidence.RepoPath == "" || evidence.CommitSHA == "" || headSHA == "" {
		return false
	}
	git := integration.NewExecGit("")
	if _, exists, err := git.ResolveCommitIfExists(ctx, evidence.RepoPath, evidence.CommitSHA); err != nil || !exists {
		return false
	}
	contained, err := git.IsAncestor(ctx, evidence.RepoPath, evidence.CommitSHA, headSHA)
	return err == nil && contained
}

// directBranchFreshness is the "is this still the thing that was verified"
// check. It is unchanged in substance from the original promotion; what changed
// is that it now runs under the lane, as the Coordinator's Precondition.
func (c *Coordinator) directBranchFreshness(ctx stdctx.Context, evidence directBranchEvidence, obs ports.WorkspaceObservation) error {
	if obs.Branch != "" && evidence.Branch != "" && obs.Branch != evidence.Branch {
		return fmt.Errorf("the target repository is on branch %q but the verified result was produced on %q", obs.Branch, evidence.Branch)
	}
	switch evidence.Kind {
	case directBranchEvidenceCommitted:
		if obs.HeadSHA != evidence.CommitSHA {
			// Two very different situations wear the same shape here, and
			// telling them apart is the difference between stopping a person
			// and asking a reviewer.
			//
			// If the verified commit is still an ANCESTOR of the head, nothing
			// was lost: the branch simply grew past this task while it waited,
			// which is ordinary on a branch several tasks share. The work is
			// intact and conflicts with nothing; what went stale is the
			// reviewer's opinion, and the verification, of a tree that has
			// since moved on. AO can ask for both again — bounded — instead of
			// parking someone.
			//
			// If it is NOT an ancestor, the verified work is not on the branch
			// any more: an amend, a rebase, a reset. That is a fact only a
			// person can act on, and it still parks.
			if c.verifiedWorkIsStillOnBranch(ctx, evidence, obs.HeadSHA) {
				return fmt.Errorf("%w: the branch advanced from the verified commit %s to %s, which still contains it",
					integration.ErrPreconditionStaleReview, shortSHA(evidence.CommitSHA), shortSHA(obs.HeadSHA))
			}
			return fmt.Errorf("the target branch moved after verification (expected the verified commit %s, found %s) — nothing was promoted",
				evidence.CommitSHA, obs.HeadSHA)
		}
		return nil
	case directBranchEvidenceAlreadyClean:
		if hasNonEphemeralChanges(obs) {
			return errors.New("the target repository holds uncommitted changes, so the verified result is not durably integrated on the branch")
		}
		if fp := WorkspaceFingerprint(obs); fp != evidence.Fingerprint {
			return errors.New("the target workspace no longer matches the fingerprint verification passed on — nothing was promoted")
		}
		return nil
	default:
		return fmt.Errorf("unrecognised direct-branch evidence kind %q", evidence.Kind)
	}
}

// errIntegrationTaskConflict is an integration the Coordinator could not
// complete for a reason that belongs to ONE task: a conflict it may not resolve
// by itself, a replay that failed verification, a target that moved off the
// verified work.
//
// It is separated from errIntegrationFailed for the same reason
// errIntegrationBusy is: it must not park the master. A conflicted task stops;
// its independent siblings keep integrating, and the objective only stops when
// nothing is left that can move.
var errIntegrationTaskConflict = errors.New("workflow: task integration needs attention")

// taskIntegrationConflictPhase is the durable, TASK-scoped record of that stop.
const taskIntegrationConflictPhase = "task_integration_conflict"

// recordTaskIntegrationConflict parks the TASK on the conflict, durably, with
// everything a person needs to act on it.
//
// The state transition is the load-bearing part, and it is what the reviewer's
// first finding was about. Recording a checkpoint and leaving the task at
// "running" made the stop decorative: reconciliation runs on every board poll,
// read the task as ready every time, and re-attempted the same integration —
// re-rebasing the same worktree onto the same target, hitting the same
// conflict, and writing another checkpoint and another notification, forever,
// with nothing surviving a restart. `needs_attention` is a state reconciliation
// skips and only a human resume clears, so the conflict is attempted exactly
// once per human decision.
//
// The lane is already released by the time this runs — Coordinator.Integrate
// gives it back on every path — so parking here never holds up a sibling.
func (c *Coordinator) recordTaskIntegrationConflict(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	att integration.Attention,
) error {
	attention := domain.WorkflowTaskAttention{
		Reason:              string(att.Reason),
		ConflictingFiles:    att.ConflictFiles,
		SourceSHA:           att.SourceSHA,
		BaseSHA:             att.BaseSHA,
		TargetBeforeSHA:     att.TargetSHA,
		IntegrationStrategy: string(att.Strategy),
		RecommendedAction:   recommendedConflictAction(att),
		Detail:              att.Detail,
		// The task carries its own history: a stop that follows a resume is the
		// second attempt, not a repeat of the first. It is what makes "the
		// resume produced exactly one new attempt" a checkable property rather
		// than an intention.
		Attempt: task.Attention.Attempt + 1,
	}
	parked, err := c.planStore.ParkWorkflowTaskForAttention(ctx, task.ID,
		domain.WorkflowTaskRunning, string(att.Reason), attention, c.clock())
	if err != nil {
		return err
	}
	if !parked {
		// The task was not where this pass believed it was — cancelled, or
		// already parked by a concurrent pass. Either way somebody else has
		// decided, and writing a second conflict record for one conflict is the
		// duplication this state exists to end.
		return fmt.Errorf("%w: %s", errIntegrationTaskConflict, att.Reason)
	}

	payload, _ := json.Marshal(struct {
		TaskID            string   `json:"taskId"`
		Reason            string   `json:"reason"`
		ConflictingFiles  []string `json:"conflictingFiles,omitempty"`
		SourceSHA         string   `json:"sourceSha,omitempty"`
		TargetBeforeSHA   string   `json:"targetBeforeSha,omitempty"`
		BaseSHA           string   `json:"baseSha,omitempty"`
		Strategy          string   `json:"strategyAttempted,omitempty"`
		RecommendedAction string   `json:"recommendedAction"`
		Attempt           int      `json:"attempt"`
	}{
		TaskID: task.ID, Reason: string(att.Reason),
		ConflictingFiles: att.ConflictFiles, SourceSHA: att.SourceSHA,
		TargetBeforeSHA: att.TargetSHA, BaseSHA: att.BaseSHA,
		Strategy: string(att.Strategy), RecommendedAction: attention.RecommendedAction,
		Attempt: attention.Attempt,
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: parent.ID,
		ProjectID:     parent.ProjectID,
		BaseSHA:       att.BaseSHA,
		HeadSHA:       att.TargetSHA,
		NextAction: fmt.Sprintf("task_integration_conflict: task %d (%s) — %s",
			task.Ordinal, task.Title, describeIntegrationAttention(att)),
		DurablePhase:   taskIntegrationConflictPhase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}); err != nil {
		return err
	}
	if c.log != nil {
		c.log.Warn("workflow: a task's integration needs attention; its siblings are unaffected",
			"run", parent.ID, "task", task.ID, "reason", att.Reason, "files", att.ConflictFiles)
	}
	return fmt.Errorf("%w: %s", errIntegrationTaskConflict, att.Reason)
}

// ResumeTaskAfterAttention is the explicit human transition out of a parked
// task, and the only one.
//
// Two properties it must have, both enforced by the conditional UPDATE it rests
// on rather than by anything here:
//
//   - Idempotent. Resuming a task that is not parked matches no row, changes
//     nothing, and returns without error. A double-click, a retried HTTP
//     request or a replayed command cannot produce two integration attempts.
//   - Restart-safe. The task's state IS the record — there is no in-memory
//     queue of resumed tasks to lose — so a daemon that dies immediately after
//     this comes back with the task running and integrates it once.
//
// It deliberately does NOT integrate anything itself. It hands the task back to
// the ordinary reconcile path, which is the one place that knows how to
// integrate a task; a resume that did its own integration would be a third
// route, which is precisely what Task 5 removed.
func (c *Coordinator) ResumeTaskAfterAttention(ctx stdctx.Context, runID, taskID string) error {
	tasks, err := c.planStore.ListWorkflowTasks(ctx, runID)
	if err != nil {
		return err
	}
	var target *domain.WorkflowTask
	for i := range tasks {
		if tasks[i].ID == taskID {
			target = &tasks[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("%w: run %s has no task %s", ErrNotFound, runID, taskID)
	}
	if !target.State.Parked() {
		// Already resumed, or never parked. Saying nothing happened is the
		// idempotent answer; an error here would make a harmless repeat look
		// like a failure.
		return nil
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	resumed, err := c.planStore.ResumeWorkflowTaskFromAttention(ctx, taskID, domain.WorkflowTaskRunning, c.clock())
	if err != nil {
		return err
	}
	if !resumed {
		// Lost a race with another resume. Same answer, same reason.
		return nil
	}
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: runID,
		ProjectID:     run.ProjectID,
		NextAction: fmt.Sprintf("task_integration_resumed: task %d (%s) was released from %s after human intervention; attempt %d follows",
			target.Ordinal, target.Title, target.AttentionReason, target.Attention.Attempt+1),
		DurablePhase:   taskIntegrationResumedPhase,
		PayloadVersion: "v1",
		RetryState: fmt.Sprintf(`{"taskId":%q,"clearedReason":%q,"nextAttempt":%d}`,
			taskID, target.AttentionReason, target.Attention.Attempt+1),
		CreatedAt: c.clock(),
	})
	if c.log != nil {
		c.log.Info("workflow: a parked task was resumed by a person",
			"run", runID, "task", taskID, "clearedReason", target.AttentionReason)
	}

	// The objective may have reflected this task's stop. Whether it can still
	// hold is a question about the plan as it now is, and the next reconcile
	// pass re-derives it from scratch — so put the run back to running and let
	// it. That pass is never far away: GetRun reconciles a master run, and an
	// autonomous run's heartbeat resumes the moment the stop is gone.
	//
	// Deliberately NOT reconciled here. Reconciling inline would attempt the
	// integration before this call returns, so a retried request would arrive
	// after the task had already stopped again and would read as a second human
	// decision — turning the one property this transition must have into a
	// timing accident.
	if run.State == domain.WorkflowRunNeedsAttention {
		if _, err := c.store.UpdateWorkflowRunState(ctx, runID, run.State, domain.WorkflowRunRunning, c.clock()); err != nil {
			return err
		}
	}
	return nil
}

// taskIntegrationResumedPhase is the durable record of a human releasing a
// parked task. It is the other half of taskIntegrationConflictPhase: together
// they say how many times a task was stopped and how many times somebody chose
// to try again.
const taskIntegrationResumedPhase = "task_integration_resumed"

// recommendedConflictAction says what a person can actually do about it, in the
// vocabulary of the thing that went wrong rather than a generic "resolve it".
func recommendedConflictAction(att integration.Attention) string {
	switch att.Reason {
	case integration.ReasonVerificationFailed:
		return "the work is correct against the base it was written on and fails against the current target; fix it on the task's branch, which is already rebased onto that target, then continue"
	case integration.ReasonNoApplicableStrategy:
		return "the task branch and the target share no history; re-cut this task's worktree from the current target and redo the work"
	case integration.ReasonTargetMovedAfterVerification:
		return "something moved the target branch after this task was verified; confirm what is on it now, then re-run this task's verification"
	case integration.ReasonDependencyMissingFromTarget:
		return "the target no longer contains a task this one depends on; restore the target to a state that includes it, or re-run the dependency, before continuing this task"
	case integration.ReasonStaleReviewAfterRebase:
		return "rebasing this task changed the diff its reviewer approved, and AO could not obtain a fresh review; review the rebased work on the task's own branch, then continue this run"
	default:
		return "resolve the conflicting files on the task's own branch, then continue this run"
	}
}

// integrationAuditPendingPhase marks an integration whose physical side is done
// and whose audit is not.
//
// It exists because the two live in different stores — git and SQLite — and no
// ordering of the two writes survives every crash. The rule this encodes is:
// never revert the physical side to match a failed audit, and never report
// success without one. What is recorded instead is exactly enough for a later
// pass to tell "already integrated, audit outstanding" from "not integrated":
// the ref and the SHA it is expected to be at.
const integrationAuditPendingPhase = "integration_audit_pending"

// recordIntegrationAuditPending records that state and refuses the promotion.
//
// Refusing is the point. The task is not promoted, the master does not count it
// as integrated, and the next reconcile pass re-enters the same path — where the
// physical work is already done, the freshness check still passes, and only the
// audit is retried.
func (c *Coordinator) recordIntegrationAuditPending(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	rec integration.Record,
	cause error,
) error {
	expected := rec.TargetAfterSHA
	if expected == "" {
		expected = rec.TargetBeforeSHA
	}
	payload, _ := json.Marshal(struct {
		TaskID      string `json:"taskId"`
		TargetRef   string `json:"targetRef"`
		ExpectedSHA string `json:"expectedSha"`
		Strategy    string `json:"strategy,omitempty"`
		Cause       string `json:"cause"`
	}{
		TaskID: task.ID, TargetRef: rec.TargetRef, ExpectedSHA: expected,
		Strategy: string(rec.Strategy), Cause: cause.Error(),
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: parent.ID,
		ProjectID:     parent.ProjectID,
		Branch:        rec.TargetBranch,
		WorktreePath:  rec.RepoPath,
		HeadSHA:       expected,
		NextAction: fmt.Sprintf(
			"integration_audit_pending: task %d (%s) is physically integrated at %s but its audit record could not be written (%v) — the promotion is NOT complete and will be finished by a later pass",
			task.Ordinal, task.Title, shortSHA(expected), cause),
		DurablePhase:   integrationAuditPendingPhase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}); err != nil {
		return err
	}
	if c.log != nil {
		c.log.Warn("workflow: integration audit could not be written; promotion deferred",
			"run", parent.ID, "task", task.ID, "expectedSha", expected, "err", cause)
	}
	return fmt.Errorf("integration audit could not be recorded for task %s: %w", task.ID, cause)
}

// clearIntegrationAuditPending answers an outstanding marker once the audit has
// actually landed, so the ledger does not keep advertising a condition that is
// over. Best-effort by design: the audit row is the fact, and this is only its
// bookkeeping.
func (c *Coordinator) clearIntegrationAuditPending(ctx stdctx.Context, parent domain.WorkflowRun, task domain.WorkflowTask) {
	if !c.hasPendingIntegrationAudit(ctx, parent.ID, task.ID) {
		return
	}
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: parent.ID,
		ProjectID:     parent.ProjectID,
		NextAction: fmt.Sprintf("integration_audit_recorded: task %d (%s)'s outstanding audit was written; the promotion is complete",
			task.Ordinal, task.Title),
		DurablePhase:   "integration_audit_recorded",
		PayloadVersion: "v1",
		RetryState:     fmt.Sprintf(`{"taskId":%q}`, task.ID),
		CreatedAt:      c.clock(),
	})
}

// hasPendingIntegrationAudit reports whether this task has an outstanding audit
// marker that has not since been answered.
func (c *Coordinator) hasPendingIntegrationAudit(ctx stdctx.Context, runID, taskID string) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	pending := false
	for _, cp := range cps {
		if !strings.Contains(cp.RetryState, `"taskId":"`+taskID+`"`) {
			continue
		}
		switch cp.DurablePhase {
		case integrationAuditPendingPhase:
			pending = true
		case "integration_audit_recorded":
			pending = false
		}
	}
	return pending
}
