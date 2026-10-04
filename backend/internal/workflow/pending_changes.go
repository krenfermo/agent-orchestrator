package workflow

import (
	stdctx "context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// pending_changes.go — P3-A §17: the commit-and-continue flow.
//
// A direct-branch run that finds uncommitted work in the repository stops. That
// is correct and it stays: AO must not write on top of somebody's unsaved
// changes, and it must not decide for them what happens to those changes. What
// it did not have was a way to SAY so usefully — the stop surfaced as
// `dirty_worktree` and left the person to work out both what was pending and
// what to do about it.
//
// This file is the two operations that close that gap, and the shape of them is
// the safety property:
//
//	PendingChanges         READ ONLY. Names the repository, the branch, and every
//	                       pending path, plus a proposed commit message the caller
//	                       may edit. Runs no git write of any kind.
//	CommitPendingChanges   Commits EXACTLY what the caller was shown, with the
//	                       message they approved, then re-probes and only then
//	                       resumes.
//
// There is deliberately no stash here, and deliberately no silent commit. A
// stash moves somebody's work somewhere they did not ask for and did not see; a
// silent commit puts their name on a message they never read. Both were
// available and both were rejected: the whole point of the flow is that the
// person decides, having been shown what they are deciding about.

// WorkspacePreflighter is the read-only repository probe. Satisfied by the
// workspace router. Optional: a nil preflighter means AO cannot report pending
// changes, which is reported as unavailable rather than as "clean" — an
// unreadable repository is never evidence that there is nothing in it.
type WorkspacePreflighter interface {
	PreflightRepository(ctx stdctx.Context, repoPath, branch string) (ports.WorkspacePreflight, error)
}

// PendingChange is one uncommitted path.
type PendingChange struct {
	Path string
	// Status is git's own porcelain code for the path, unmodified. It is the
	// technical detail beside a human line, never the line itself.
	Status string
}

// PendingChanges is what a person is shown before they decide.
type PendingChanges struct {
	// Available reports that AO could actually probe the repository. False
	// means the answer is unknown, and every other field is zero.
	Available bool
	// Unavailable names why, when it could not.
	Unavailable string
	RepoPath    string
	Branch      string
	HeadSHA     string
	Dirty       bool
	Changes     []PendingChange
	// Placement, WorktreePath and Historical say WHERE the answer came from.
	//
	// They exist because "what is pending" is meaningless without "pending
	// where", and after a run finishes that is no longer inferable from the
	// live placement index. Historical=true means the run's execution placement
	// has been retired and this answer was reconstructed from the durable
	// record: perfectly good to read, and never sufficient to commit.
	Placement    domain.ExecutionPlacementType
	WorktreePath string
	Historical   bool
	// ProposedMessage is a starting point, not a decision. It is derived from
	// the run's own objective so it says something true about why the work is
	// there, and the caller is expected to edit it.
	ProposedMessage string
}

// CommitOutcome is what CommitPendingChanges actually did.
type CommitOutcome struct {
	// Committed is false when the repository turned out to be clean — somebody
	// else committed in the meantime — which is a success, not an error: the
	// obligation the flow existed to discharge is discharged.
	Committed bool
	CommitSHA string
	// Clean reports the re-probe: the repository has no pending changes left.
	// A commit that leaves the tree dirty (an ignored-file edit, a path git
	// refused) does NOT resume the run, because resuming into a repository that
	// is still dirty walks straight back into the stop.
	Clean bool
	// Resumed reports that the run was continued afterwards.
	Resumed bool
	// Detail explains a Clean=false or Resumed=false outcome in AO's own words.
	Detail string
}

// ErrPendingChangesUnavailable is the fail-closed answer for a run whose
// repository AO cannot probe. It is deliberately not "the repository is clean":
// the two are different answers and only one of them is a fact.
var ErrPendingChangesUnavailable = fmt.Errorf("%w: this run's repository cannot be probed for pending changes", ErrInvalid)

// PendingChanges reports what is uncommitted in the repository this run works
// in. Read-only: it runs `git status` and nothing else.
func (c *Coordinator) PendingChanges(ctx stdctx.Context, runID string) (PendingChanges, error) {
	run, err := c.loadRunForCommit(ctx, runID)
	if err != nil {
		return PendingChanges{}, err
	}
	target, ok := c.inspectionTargetFor(ctx, run)
	if !ok {
		return PendingChanges{
			Unavailable: "AO has no placement recorded for this run, so it cannot say which repository and branch the work belongs to",
		}, nil
	}
	base := PendingChanges{
		RepoPath: target.repoPath, Branch: target.branch,
		Placement: target.placement, WorktreePath: target.worktreePath,
		Historical: !target.live,
	}
	if target.gone {
		// The ordinary end of an integrated task's life, and it is a different
		// answer from "AO does not know where this ran" -- which is what the
		// live-placement lookup used to say about it. AO does not recreate a
		// worktree to read it.
		base.Unavailable = "the isolated worktree for this run has already been removed, so there is nothing left to inspect"
		return base, nil
	}
	if target.inspectPath == "" {
		base.Unavailable = "AO recorded an isolated placement for this run but never recorded where its worktree was, so it cannot inspect it"
		return base, nil
	}
	if c.workspacePreflight == nil {
		base.Unavailable = "this daemon has no repository probe wired"
		return base, nil
	}
	probe, perr := c.workspacePreflight.PreflightRepository(ctx, target.inspectPath, target.branch)
	if perr != nil {
		// Not an error to the caller: "AO could not read the repository" is an
		// ANSWER a person can act on, and collapsing it into a 500 would lose
		// which of the four unavailable cases they are in. The distinction that
		// matters is preserved instead -- unavailable is never "clean".
		base.Unavailable = "the repository could not be read: " + perr.Error()
		return base, nil //nolint:nilerr // the read failure IS the reported answer; see above.
	}
	out := base
	out.Available = true
	out.HeadSHA = probe.HeadSHA
	out.Dirty = probe.Dirty
	out.ProposedMessage = proposedCommitMessage(run)
	if probe.RepoPath != "" {
		out.RepoPath = probe.RepoPath
	}
	for _, ch := range probe.Changes {
		out.Changes = append(out.Changes, PendingChange{Path: ch.Path, Status: ch.Status})
	}
	return out, nil
}

// CommitPendingChanges commits the repository's pending work under a message
// the caller supplied, and resumes the run only once the tree is provably
// clean.
//
// The order is the guarantee. Commit, RE-PROBE, then resume — never commit and
// resume on the assumption that the commit was total. A commit that left
// something behind is exactly the state that would stop the run again, and
// reporting "resumed" over it would be a lie the person acts on.
func (c *Coordinator) CommitPendingChanges(ctx stdctx.Context, runID, message string) (CommitOutcome, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return CommitOutcome{}, fmt.Errorf("%w: a commit message is required; AO does not write one on somebody's behalf", ErrInvalid)
	}
	run, err := c.loadRunForCommit(ctx, runID)
	if err != nil {
		return CommitOutcome{}, err
	}
	repoPath, branch, aerr := c.commitAuthorityFor(ctx, run)
	if aerr != nil {
		return CommitOutcome{}, aerr
	}
	if c.workspaceCommitter == nil {
		return CommitOutcome{}, ErrPendingChangesUnavailable
	}
	sha, committed, err := c.workspaceCommitter.CommitAll(ctx, ports.WorkspaceInfo{
		Path: repoPath, Branch: branch, ProjectID: domain.ProjectID(run.ProjectID), RepoPath: repoPath,
	}, message)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("commit pending changes in %s: %w", repoPath, err)
	}
	out := CommitOutcome{Committed: committed, CommitSHA: sha}

	// The re-probe. Without a preflighter AO cannot prove the tree is clean, and
	// an unprovable clean is not a clean: the run is left stopped with the
	// commit recorded, which is strictly better than resuming into the same
	// refusal.
	if c.workspacePreflight == nil {
		out.Detail = "the commit was made; AO has no repository probe wired, so it could not confirm the tree is clean and did not resume the run"
		c.recordCommitAndContinue(ctx, run, repoPath, branch, sha, out)
		return out, nil
	}
	probe, perr := c.workspacePreflight.PreflightRepository(ctx, repoPath, branch)
	if perr != nil {
		// The commit HAPPENED. Returning an error here would tell the caller the
		// write failed when it did not, and they would retry it. The honest
		// report is the outcome that says what was done and what was not.
		out.Detail = "the commit was made; the repository could not be re-read afterwards, so AO did not resume the run: " + perr.Error()
		c.recordCommitAndContinue(ctx, run, repoPath, branch, sha, out)
		return out, nil //nolint:nilerr // the commit succeeded; the re-probe failure is reported in the outcome.
	}
	out.Clean = !probe.Dirty
	if !out.Clean {
		out.Detail = "the commit was made and the repository still has pending changes, so AO did not resume the run: it would stop on the same condition again"
		c.recordCommitAndContinue(ctx, run, repoPath, branch, sha, out)
		return out, nil
	}
	c.recordCommitAndContinue(ctx, run, repoPath, branch, sha, out)
	if _, _, rerr := c.ResumeRun(ctx, runID); rerr != nil {
		// Same reason as above: the commit is durable, and reporting a failed
		// resume as a failed commit would invite a second one.
		out.Detail = "the repository is clean; resuming the run failed: " + rerr.Error()
		return out, nil //nolint:nilerr // the commit succeeded; the failed resume is reported in the outcome.
	}
	out.Resumed = true
	return out, nil
}

// recordCommitAndContinue writes the durable account of a human-authorised
// commit. Best-effort, like every other observation in this package: a failed
// audit write must not undo a commit that has already happened.
func (c *Coordinator) recordCommitAndContinue(ctx stdctx.Context, run domain.WorkflowRun, repoPath, branch, sha string, out CommitOutcome) {
	if c.store == nil {
		return
	}
	next := "operator_commit_and_continue"
	if out.CommitSHA != "" {
		next += ": " + sha + " on " + branch
	}
	if out.Detail != "" {
		next += " (" + out.Detail + ")"
	}
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		Branch:         branch,
		WorktreePath:   repoPath,
		HeadSHA:        sha,
		NextAction:     next,
		DurablePhase:   "operator_commit_and_continue",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	})
}

// loadRunForCommit loads the run this flow acts on.
func (c *Coordinator) loadRunForCommit(ctx stdctx.Context, runID string) (domain.WorkflowRun, error) {
	run, found, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.WorkflowRun{}, err
	}
	if !found {
		return domain.WorkflowRun{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	return run, nil
}

// inspectionTarget is what a READ may look at, and where.
type inspectionTarget struct {
	placement    domain.ExecutionPlacementType
	repoPath     string
	branch       string
	worktreePath string
	// inspectPath is the directory git is actually asked about: the worktree
	// for an isolated placement, the repository itself for a direct one.
	inspectPath string
	// live reports that the placement is still this run's execution authority.
	live bool
	// gone marks an isolated worktree AO recorded and that is no longer on
	// disk, which is an ANSWER rather than a failure.
	gone bool
}

// inspectionTargetFor resolves where a read-only inspection should look.
//
// It uses the RECALL, not the live index, and that is the whole of this fix: a
// run that has finished still has a repository and a branch, and refusing to
// name them because execution has ended tells a person nothing they can use at
// exactly the moment they want to look. Nothing here writes, locks, spawns or
// materialises anything.
//
// It also resolves the isolated case correctly for the first time. Pending
// changes for an isolated placement live in the WORKTREE; probing the parent
// repository would report the user's own unrelated working state as if it were
// the task's output.
func (c *Coordinator) inspectionTargetFor(ctx stdctx.Context, run domain.WorkflowRun) (inspectionTarget, bool) {
	recall, ok := c.recallPlacement(ctx, run)
	if !ok || recall.RepoPath == "" || recall.ExecutionBranch == "" {
		return inspectionTarget{}, false
	}
	out := inspectionTarget{
		placement: recall.Type, repoPath: recall.RepoPath, branch: recall.ExecutionBranch,
		worktreePath: recall.WorktreePath, live: recall.Live,
	}
	if recall.Type.Isolated() {
		out.inspectPath = recall.WorktreePath
		if recall.WorktreePath != "" && !directoryExists(recall.WorktreePath) {
			out.gone = true
			out.inspectPath = ""
		}
		return out, true
	}
	out.inspectPath = recall.RepoPath
	return out, true
}

// ErrPendingChangesNoAuthority is the fail-closed answer for a commit AO cannot
// prove it is still entitled to make.
//
// It is deliberately distinct from ErrPendingChangesUnavailable: that one means
// "AO cannot see the repository", this one means "AO can see it and must not
// write to it". Collapsing them would let a person read the reason for a
// refusal to touch their branch as a transient probe failure worth retrying.
var ErrPendingChangesNoAuthority = fmt.Errorf("%w: this run can no longer prove it is entitled to commit to this branch", ErrInvalid)

// commitAuthorityFor names the repository and branch this run may commit to,
// or refuses.
//
// Reading a placement and being entitled to WRITE through it are now separate
// questions, and this is the second one. Every clause below is a refusal AO
// would otherwise have had no way to make, and the reason they are all here
// rather than spread across the caller is that a mutation authorised by four of
// five proofs is not authorised:
//
//  1. THE RUN IS NOT OVER. This flow exists to unblock a live run stopped on a
//     dirty repository, and it ends by resuming it. A terminal run has nothing
//     to resume, and committing "on its behalf" would put a finished run's name
//     on a change nobody asked it for. Read stays open; write does not.
//  2. THE PLACEMENT IS LIVE. A recollection is enough to describe where work
//     happened and never enough to aim a commit: the record is retired
//     precisely because AO has stopped asserting that the run owns that
//     repository.
//  3. THE PLACEMENT PERMITS EXECUTION. A placement waiting, conflicted or
//     preserved is not one to write through, whatever its row still says about
//     repository and branch.
//  4. IT IS A DIRECT-BRANCH PLACEMENT. The commit-and-continue flow was written
//     for the direct-branch stop and only ever aimed at RepoPath; for an
//     isolated placement that path is the parent checkout, so committing there
//     would sweep the user's own unrelated working state into the task's
//     branch. Refusing is both the safe answer and the honest one.
//  5. THE RUN STILL HOLDS THE BRANCH. Branch locks are exclusive, so a run that
//     holds the lock for this repository and branch is a run no newer workflow
//     has taken it from. This is the ownership proof: it fails closed for a
//     stale context, for a lock released at completion, and for a branch a
//     later run has since acquired -- without AO having to enumerate who else
//     might want it.
func (c *Coordinator) commitAuthorityFor(ctx stdctx.Context, run domain.WorkflowRun) (string, string, error) {
	if run.State.Terminal() {
		return "", "", fmt.Errorf("%w: workflow run %q is already %s, so there is nothing to commit and continue",
			ErrPendingChangesNoAuthority, run.ID, run.State)
	}
	recall, ok := c.recallPlacement(ctx, run)
	if !ok || recall.RepoPath == "" || recall.ExecutionBranch == "" {
		return "", "", ErrPendingChangesUnavailable
	}
	if !recall.Live {
		return "", "", fmt.Errorf("%w: this run's execution placement has been retired, so AO can describe where it worked but not write there",
			ErrPendingChangesNoAuthority)
	}
	if !recall.State.PermitsLaunch() {
		return "", "", fmt.Errorf("%w: this run's placement is %s, which is not a state AO commits through",
			ErrPendingChangesNoAuthority, recall.State)
	}
	if recall.Type.Isolated() {
		return "", "", fmt.Errorf("%w: this run works in an isolated worktree, and the commit-and-continue flow only commits a direct-branch placement's own repository",
			ErrPendingChangesNoAuthority)
	}
	if err := c.assertBranchStillOwned(ctx, run, recall.RepoPath, recall.ExecutionBranch); err != nil {
		return "", "", err
	}
	return recall.RepoPath, recall.ExecutionBranch, nil
}

// branchLockHolderReader is the optional capability assertBranchStillOwned needs
// to ask "who owns this branch right now". Asserted at the call site, mirroring
// branchLockCeder, so a lock manager or test double without it degrades rather
// than failing to compile.
type branchLockHolderReader interface {
	Holder(ctx stdctx.Context, repoPath, branch string) (domain.BranchLock, bool, error)
}

// assertBranchStillOwned proves that NOBODY ELSE owns the repository and branch
// this run is about to write to.
//
// The question it asks changed in P3-C, and the reason is the whole
// commit-and-continue flow. This clause used to demand that the run HOLD the
// lock -- and the one stop the flow exists to clear is `dirty_worktree`, which
// is recorded precisely BECAUSE the acquisition was refused. A run parked on a
// dirty repository therefore holds no lock by construction, so the flow refused
// its own primary case every time; the P3-C closing smoke hit it on the first
// try, with "this run does not hold the execution lock".
//
// What clause 5 was actually protecting is stated in its own comment: "a branch
// a later run has since acquired". That is a statement about somebody ELSE
// holding it, and it is satisfied by an unheld branch just as well as by one
// this run holds -- an unheld branch is exactly the state AO is about to
// acquire. So the refusal is now the honest one: another run owns this branch.
//
// Nothing is asserted when no branch-lock authority is wired at all: that is
// the pre-P1-D deployment shape, where the lock cannot answer the question
// either way, and inventing a refusal from its absence would break the flow for
// every caller that never had locks. A lock manager that IS wired but cannot
// answer who holds a key fails closed instead — "AO cannot ask" is not an
// answer of "free".
func (c *Coordinator) assertBranchStillOwned(ctx stdctx.Context, run domain.WorkflowRun, repoPath, branch string) error {
	if c.branchLocks == nil {
		return nil
	}
	locks, err := c.branchLocks.HeldByRun(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, lock := range locks {
		if lock.RepoPath == repoPath && lock.Branch == branch {
			return nil
		}
	}
	reader, ok := c.branchLocks.(branchLockHolderReader)
	if !ok {
		// AO cannot ask who owns this branch, so it cannot show that nobody
		// does. Fail closed, exactly as this clause did before P3-C: an
		// unanswerable ownership question is not an answer of "free".
		return fmt.Errorf("%w: this run does not hold the execution lock on %s in %s, and AO cannot ask who does, so it will not write there",
			ErrPendingChangesNoAuthority, branch, repoPath)
	}
	holder, held, err := reader.Holder(ctx, repoPath, branch)
	if err != nil {
		return err
	}
	if !held {
		// Free. This is the dirty-worktree case: AO could not take the branch
		// because of the user's own uncommitted work, and committing that work
		// is what lets it take the branch next.
		return nil
	}
	return fmt.Errorf("%w: workflow run %s currently holds the execution lock on %s in %s, so AO will not write to it on this run's behalf",
		ErrPendingChangesNoAuthority, holder.OwnerKey(), branch, repoPath)
}

// proposedCommitMessage derives a starting point from the run's own objective.
//
// Bounded to one short subject line, because a 128 KiB task specification is
// not a commit message and pasting one into the subject would be worse than
// offering nothing. The caller edits it; nothing here is committed unedited
// unless a person chose to.
func proposedCommitMessage(run domain.WorkflowRun) string {
	subject := strings.TrimSpace(run.Objective)
	if idx := strings.IndexAny(subject, "\r\n"); idx >= 0 {
		subject = strings.TrimSpace(subject[:idx])
	}
	const maxSubject = 72
	if len(subject) > maxSubject {
		subject = strings.TrimSpace(subject[:maxSubject])
	}
	if subject == "" {
		return "wip: save pending changes before AO starts"
	}
	return "wip: " + subject
}
