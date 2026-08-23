// Package integration is the single lane every finished task's work passes
// through on its way onto a target branch.
//
// It exists because integrating concurrent work is the one part of parallel
// execution that cannot itself be parallel. Tasks may run, review and verify
// in as many worktrees as the scheduler allows; but the moment two of them
// move one branch, the second one is reasoning about a target that the first
// one has already changed underneath it. Everything here follows from making
// that impossible:
//
//   - One target integration at a time. The lane is a branch lock keyed on
//     repository+branch (lock.go), the same key direct-branch execution uses,
//     so an integration excludes a direct writer of the same branch and
//     excludes nothing else. Tasks on other targets, and every task still
//     working or reviewing, are unaffected -- a busy lane returns ErrLockBusy
//     rather than blocking.
//
//   - The target is read INSIDE the lane, never before it. A task's own record
//     of where the target was is a fact about when it started, and the whole
//     hazard is that it has since moved.
//
//   - Work that has to be moved onto a changed target is re-verified before it
//     lands. A task verified its work against the base it was written on; once
//     it is replayed onto a different target that verification describes
//     content that no longer exists.
//
//   - The ref update is a compare-and-set. Even holding the lane, the target
//     is only written if it is still where it was read, so an integration can
//     never overwrite a change it did not see.
//
//   - A conflict that is not provably trivial stops for a person, with the
//     exact files and the three SHAs, and stops nothing else.
//
// Two things this package deliberately cannot do: it never runs a mutating git
// command anywhere but AO's own task worktree (see Git's doc comment and
// Request.validate), and it never integrates a task whose review or
// verification did not pass (see Readiness).
package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var (
	// ErrNotReady means the task never passed the gate that precedes
	// integration. It is returned before the lane is entered and before any git
	// command runs at all, so a task with a failed review or a failed
	// verification cannot reach the target branch even by mistake.
	ErrNotReady = errors.New("integration: task is not ready for integration")
	// ErrInvalidRequest means the request could not describe an integration.
	ErrInvalidRequest = errors.New("integration: invalid request")
)

// Request is one task asking to be integrated.
type Request struct {
	ProjectID     domain.ProjectID
	WorkflowRunID string
	TaskID        string
	// SessionID is the task's worker, recorded as the lock's session scope. It
	// is optional: a run may integrate on its own behalf.
	SessionID string

	// RepoPath is the repository whose target branch moves. It is read from
	// and its ref is updated; nothing in it is ever checked out or written.
	RepoPath string
	RepoName string
	// WorktreePath is the task's own worktree, and the only place this package
	// runs a mutating git command. It must not be RepoPath.
	WorktreePath string

	TargetBranch string
	SourceBranch string
	// BaseSHA is the target commit the task's work was built on, as the task
	// recorded it when it started. The coordinator compares it against the
	// target's actual head to explain drift; it never trusts it as the head.
	BaseSHA string

	Readiness Readiness
	Policy    Policy
}

func (r *Request) validate() error {
	trimmed(&r.WorkflowRunID, &r.TaskID, &r.SessionID, &r.RepoPath, &r.RepoName,
		&r.WorktreePath, &r.TargetBranch, &r.SourceBranch, &r.BaseSHA)
	switch {
	case r.TaskID == "":
		return fmt.Errorf("%w: task id is required", ErrInvalidRequest)
	case r.RepoPath == "":
		return fmt.Errorf("%w: repository path is required", ErrInvalidRequest)
	case r.WorktreePath == "":
		return fmt.Errorf("%w: worktree path is required", ErrInvalidRequest)
	case r.TargetBranch == "":
		return fmt.Errorf("%w: target branch is required", ErrInvalidRequest)
	case r.SourceBranch == "":
		return fmt.Errorf("%w: source branch is required", ErrInvalidRequest)
	case r.WorkflowRunID == "" && r.SessionID == "":
		// The lock is owned by a run or by a session. An acquisition with
		// neither could never be released by either and would strand the lane.
		return fmt.Errorf("%w: neither a workflow run nor a session owns this integration", ErrInvalidRequest)
	case r.WorktreePath == r.RepoPath:
		// This is the one invariant worth refusing rather than working around.
		// Every replay this package performs -- rebase, cherry-pick, merge,
		// checkout, conflict resolution -- happens in WorktreePath, so allowing
		// it to be the repository itself would put all of them inside a
		// checkout AO does not own.
		return fmt.Errorf("%w: worktree path must not be the repository itself (%s)", ErrInvalidRequest, r.RepoPath)
	}
	return nil
}

// Deps are the coordinator's collaborators. Git and Locks are required;
// Verifier may be nil only for a caller that can guarantee it will never need
// a replay, which in practice no caller can, so leaving it out is treated as a
// configuration error at the moment it would have been needed rather than
// silently integrating unverified content.
type Deps struct {
	Git      Git
	Locks    Locker
	Verifier Verifier
	Recorder Recorder
	Clock    func() time.Time
}

// Coordinator integrates one task at a time per target branch.
type Coordinator struct {
	git      Git
	locks    Locker
	verifier Verifier
	recorder Recorder
	clock    func() time.Time
}

// New returns a Coordinator, or an error if it could not do its job at all.
func New(deps Deps) (*Coordinator, error) {
	if deps.Git == nil {
		return nil, errors.New("integration: a Git is required")
	}
	if deps.Locks == nil {
		return nil, errors.New("integration: a Locker is required")
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Coordinator{git: deps.Git, locks: deps.Locks, verifier: deps.Verifier, recorder: deps.Recorder, clock: clock}, nil
}

// Integrate puts one ready task's work onto its target branch, or explains
// exactly why it did not.
//
// The ordering of the first two steps is load-bearing. The readiness gate runs
// BEFORE the lane is entered, so a task that should never be integrated cannot
// occupy the lane while being turned away, and cannot have run a single git
// command by the time it is refused.
func (c *Coordinator) Integrate(ctx context.Context, req Request) (Outcome, error) {
	if err := req.validate(); err != nil {
		return Outcome{}, err
	}
	if ok, why := req.Readiness.Ready(); !ok {
		return Outcome{}, fmt.Errorf("%w: task %s: %s", ErrNotReady, req.TaskID, why)
	}

	handle, err := c.locks.Acquire(ctx, LockRequest{
		ProjectID:     req.ProjectID,
		WorkflowRunID: req.WorkflowRunID,
		SessionID:     req.SessionID,
		TaskID:        req.TaskID,
		RepoName:      req.RepoName,
		RepoPath:      req.RepoPath,
		TargetBranch:  req.TargetBranch,
	})
	if err != nil {
		return Outcome{}, err
	}

	outcome, err := c.integrateLocked(ctx, req)
	if err == nil {
		// The record is written while the lane is still held. The moment it is
		// given back another integration may move the target, and the three
		// SHAs in this record are the only account of where it was.
		if rerr := c.record(ctx, outcome.Record); rerr != nil {
			err = rerr
		}
	}
	// The lane is given back on every path: a coordinator that failed still has
	// to let the next task try.
	relReason := "integration finished"
	switch {
	case err != nil:
		relReason = "integration failed"
	case outcome.Record.Attention != nil:
		relReason = "integration needs attention: " + string(outcome.Record.Attention.Reason)
	}
	if rerr := c.locks.Release(ctx, handle, relReason); rerr != nil && err == nil {
		err = fmt.Errorf("integration: releasing the target lane: %w", rerr)
	}
	return outcome, err
}

// integrateLocked is everything that happens while this task owns the lane.
func (c *Coordinator) integrateLocked(ctx context.Context, req Request) (Outcome, error) {
	// Read the target from the repository, now, inside the lane. req.BaseSHA is
	// what the task believed when it started and is kept only to describe the
	// drift.
	targetBefore, err := c.git.ResolveCommit(ctx, req.RepoPath, req.TargetBranch)
	if err != nil {
		return Outcome{}, err
	}
	sourceSHA, err := c.git.ResolveCommit(ctx, req.RepoPath, req.SourceBranch)
	if err != nil {
		return Outcome{}, err
	}

	rec := Record{
		TaskID:          req.TaskID,
		WorkflowRunID:   req.WorkflowRunID,
		ProjectID:       string(req.ProjectID),
		RepoPath:        req.RepoPath,
		TargetBranch:    req.TargetBranch,
		SourceBranch:    req.SourceBranch,
		SourceSHA:       sourceSHA,
		TargetBeforeSHA: targetBefore,
		BaseSHA:         req.BaseSHA,
		IntegratedAt:    c.clock(),
	}
	if rec.BaseSHA == "" {
		rec.BaseSHA = targetBefore
	}

	contained, err := c.git.IsAncestor(ctx, req.RepoPath, targetBefore, sourceSHA)
	if err != nil {
		return Outcome{}, err
	}
	if contained {
		// The target is still an ancestor of the source, so the source already
		// contains every commit the target has: moving the ref forward loses
		// nothing and re-verifying would re-answer a question the task already
		// answered against this exact content.
		rec.Strategy = StrategyFastForward
		return c.land(ctx, req, rec, sourceSHA)
	}

	// The target moved while this task was working. Everything below replays
	// the task's work onto where the target actually is, and re-verifies the
	// result, because what the task verified is no longer what would land.
	strategy, err := c.selectStrategy(ctx, req, targetBefore, sourceSHA)
	if err != nil {
		return Outcome{}, err
	}
	if strategy == "" {
		rec.Outcome = OutcomeNeedsAttention
		rec.Attention = &Attention{
			Reason:    ReasonNoApplicableStrategy,
			BaseSHA:   rec.BaseSHA,
			TargetSHA: targetBefore,
			SourceSHA: sourceSHA,
			Detail: "the task branch and the target share no common ancestor, " +
				"so there is no change that can be replayed from one onto the other",
		}
		return needsAttention(rec), nil
	}
	rec.Strategy = strategy
	rec.Replayed = true

	replayed, attention, resolved, err := c.replay(ctx, req, strategy, targetBefore, sourceSHA)
	if err != nil {
		return Outcome{}, err
	}
	rec.AutoResolvedPaths = resolved
	if attention != nil {
		attention.BaseSHA, attention.TargetSHA, attention.SourceSHA = rec.BaseSHA, targetBefore, sourceSHA
		attention.Strategy = strategy
		rec.Outcome, rec.Attention = OutcomeNeedsAttention, attention
		return needsAttention(rec), nil
	}
	rec.SourceSHA = replayed

	verification, err := c.verify(ctx, req, replayed, targetBefore)
	if err != nil {
		return Outcome{}, err
	}
	rec.Verification = verification
	if !verification.Passed {
		// The work is correct against the base it was written on and wrong
		// against the target, which only its author can act on. The replay is
		// left in place rather than undone: a rebase that succeeded has already
		// put the task's commits on top of the current target, which is exactly
		// where the failure has to be fixed. Only a staging checkout is undone
		// (undoReplay), so the worktree is back on the task's own branch.
		//
		// SourceSHA names the REPLAYED commit, because that is the commit whose
		// verification failed; the pre-replay one would send a reader to
		// content that passes.
		c.undoReplay(ctx, req, strategy)
		rec.Outcome = OutcomeNeedsAttention
		rec.Attention = &Attention{
			Reason:    ReasonVerificationFailed,
			BaseSHA:   rec.BaseSHA,
			TargetSHA: targetBefore,
			SourceSHA: replayed,
			Strategy:  strategy,
			Detail:    "verification failed after replaying onto the current target: " + verification.Summary,
		}
		return needsAttention(rec), nil
	}
	return c.land(ctx, req, rec, replayed)
}

// selectStrategy answers "how may this task's work be moved onto a target that
// has advanced", preferring the strategy that rewrites and invents the least.
// It returns "" when no strategy applies, which is a Needs attention rather
// than an error: the histories are simply not joinable here.
func (c *Coordinator) selectStrategy(ctx context.Context, req Request, targetBefore, sourceSHA string) (Strategy, error) {
	base, err := c.git.MergeBase(ctx, req.RepoPath, targetBefore, sourceSHA)
	if err != nil {
		return "", err
	}
	if base == "" {
		// The task's branch and the target share no ancestor, so there is no
		// change to replay: every one of these strategies is defined relative
		// to a common base. Joining them anyway would mean deciding, file by
		// file, which of two unrelated trees the project meant to keep.
		return "", nil
	}
	hasMerges, err := c.git.HasMergeCommits(ctx, req.RepoPath, base, sourceSHA)
	if err != nil {
		return "", err
	}
	if !hasMerges {
		// A linear range is exactly what rebase replays faithfully: every task
		// commit survives individually and the target history stays linear.
		return StrategyRebaseFastForward, nil
	}
	// The range contains a merge. `git rebase` would drop it and silently
	// reshape the history it claims to be preserving, so rebase is out.
	if req.Policy.AllowMergeCommit {
		// A merge commit is the only strategy that records both histories
		// exactly as they are, so where policy permits one it is the better
		// answer for a history that already contains merges.
		return StrategyMergeCommit, nil
	}
	// Otherwise the task's cumulative change lands as one commit on the target.
	// It keeps every line of the work and loses only the shape of a history
	// rebase would have flattened anyway -- but says so, in the strategy it
	// records, instead of pretending the history came through intact.
	return StrategyCherryPick, nil
}

// replay moves the task's work onto targetBefore in the task's own worktree
// and returns the commit that should become the new target. A non-nil
// Attention means it stopped on a conflict a person has to resolve; the
// worktree is restored before returning either way.
func (c *Coordinator) replay(ctx context.Context, req Request, strategy Strategy, targetBefore, sourceSHA string) (string, *Attention, []string, error) {
	op, err := replayOp(strategy)
	if err != nil {
		return "", nil, nil, err
	}

	// Cherry-pick and merge build the result on a detached HEAD at the target
	// and leave the task branch untouched; rebase moves the task branch itself.
	if strategy != StrategyRebaseFastForward {
		if err := c.git.CheckoutDetached(ctx, req.WorktreePath, targetBefore); err != nil {
			return "", nil, nil, err
		}
	}

	switch strategy {
	case StrategyRebaseFastForward:
		err = c.git.Rebase(ctx, req.WorktreePath, targetBefore)
	case StrategyCherryPick:
		err = c.git.CherryPick(ctx, req.WorktreePath, sourceSHA)
	case StrategyMergeCommit:
		err = c.git.Merge(ctx, req.WorktreePath, sourceSHA,
			fmt.Sprintf("AO integration: merge task %s into %s", req.TaskID, req.TargetBranch))
	}

	var resolved []string
	if errors.Is(err, ErrReplayConflict) {
		var attention *Attention
		resolved, attention, err = c.resolveConflicts(ctx, req, op)
		if err != nil {
			return "", nil, resolved, err
		}
		if attention != nil {
			c.undoReplay(ctx, req, strategy)
			return "", attention, resolved, nil
		}
	}
	if err != nil {
		c.undoReplay(ctx, req, strategy)
		return "", nil, resolved, err
	}
	if strategy == StrategyCherryPick {
		// A squashed replay only ever stages; the commit that turns it into the
		// new target head is made here, whether or not it had conflicts.
		if err := c.git.Commit(ctx, req.WorktreePath,
			fmt.Sprintf("AO integration: task %s onto %s", req.TaskID, req.TargetBranch)); err != nil {
			c.undoReplay(ctx, req, strategy)
			return "", nil, resolved, err
		}
	}

	head, err := c.git.ResolveCommit(ctx, req.WorktreePath, "HEAD")
	if err != nil {
		return "", nil, resolved, err
	}
	return head, nil, resolved, nil
}

// resolveConflicts applies the one automatic rule, repeatedly: a replay of
// several commits can stop on a conflict more than once, and each stop is a
// fresh set of unmerged paths.
func (c *Coordinator) resolveConflicts(ctx context.Context, req Request, op ReplayOp) ([]string, *Attention, error) {
	var resolved []string
	// The bound is a safety net rather than a policy: each successful
	// continuation strictly advances the replay, so a replay that keeps
	// conflicting on the same paths is a bug and must not spin forever.
	for round := 0; round < 64; round++ {
		paths, err := c.git.UnmergedPaths(ctx, req.WorktreePath)
		if err != nil {
			return resolved, nil, err
		}
		if len(paths) == 0 {
			return resolved, nil, nil
		}
		if req.Policy.DisableAutoResolve {
			return resolved, &Attention{Reason: ReasonMergeConflict, ConflictFiles: paths,
				Detail: "conflicting files must be resolved by hand (automatic resolution is disabled for this project)"}, nil
		}
		for _, path := range paths {
			ok, err := c.autoResolve(ctx, req.WorktreePath, path)
			if err != nil {
				return resolved, nil, err
			}
			if !ok {
				// Report every conflicting path, not only the one that could
				// not be resolved: a person deciding what to do needs the whole
				// set, and half of them being resolvable is not their problem.
				return resolved, &Attention{Reason: ReasonMergeConflict, ConflictFiles: paths,
					Detail: fmt.Sprintf("%s cannot be resolved automatically: the two changes overlap", path)}, nil
			}
			resolved = append(resolved, path)
		}
		if err := c.git.ContinueReplay(ctx, req.WorktreePath, op); err != nil {
			if errors.Is(err, ErrReplayConflict) {
				continue
			}
			return resolved, nil, err
		}
		return resolved, nil, nil
	}
	return resolved, nil, errors.New("integration: replay kept conflicting after 64 resolution rounds")
}

// autoResolve resolves one conflicted path if and only if conflict.go's
// append-only rule applies to it.
func (c *Coordinator) autoResolve(ctx context.Context, worktree, path string) (bool, error) {
	base, hasBase, err := c.git.StageBlob(ctx, worktree, path, 1)
	if err != nil {
		return false, err
	}
	sideA, hasA, err := c.git.StageBlob(ctx, worktree, path, 2)
	if err != nil {
		return false, err
	}
	sideB, hasB, err := c.git.StageBlob(ctx, worktree, path, 3)
	if err != nil {
		return false, err
	}
	// A missing stage means one side added or deleted the file rather than
	// appending to it, and there is no ancestor to append to.
	if !hasBase || !hasA || !hasB {
		return false, nil
	}
	merged, ok := appendOnlyResolution(base, sideA, sideB)
	if !ok {
		return false, nil
	}
	if err := c.git.WriteResolution(ctx, worktree, path, merged); err != nil {
		return false, err
	}
	return true, nil
}

// undoReplay puts the task's worktree back the way its author left it. It is
// best-effort by design: the integration's decision has already been made, and
// a failure to tidy up must not turn a clean "needs attention" into an error
// that hides it.
func (c *Coordinator) undoReplay(ctx context.Context, req Request, strategy Strategy) {
	if op, err := replayOp(strategy); err == nil {
		_ = c.git.AbortReplay(ctx, req.WorktreePath, op)
	}
	if strategy != StrategyRebaseFastForward {
		_ = c.git.CheckoutBranch(ctx, req.WorktreePath, req.SourceBranch)
	}
}

// verify re-runs the task's verification against the replayed content.
func (c *Coordinator) verify(ctx context.Context, req Request, head, targetSHA string) (Verification, error) {
	if c.verifier == nil {
		// Reaching here means work was replayed onto a target it was never
		// verified against and there is nothing that could verify it. Landing
		// it would be exactly the silent regression this package exists to
		// prevent, so it is an error rather than an assumed pass.
		return Verification{}, errors.New("integration: work had to be replayed onto a moved target but no Verifier is configured")
	}
	verification, err := c.verifier.Verify(ctx, VerifyRequest{
		TaskID:        req.TaskID,
		WorkflowRunID: req.WorkflowRunID,
		ProjectID:     string(req.ProjectID),
		WorktreePath:  req.WorktreePath,
		HeadSHA:       head,
		TargetSHA:     targetSHA,
	})
	if err != nil {
		return Verification{}, err
	}
	verification.Ran = true
	return verification, nil
}

// land performs the atomic step and completes the record.
func (c *Coordinator) land(ctx context.Context, req Request, rec Record, next string) (Outcome, error) {
	if next == rec.TargetBeforeSHA {
		// The target already contains this task. Recording it as integrated is
		// truthful -- its work IS on the target -- and re-running the ref
		// update would be a no-op anyway.
		rec.TargetAfterSHA = rec.TargetBeforeSHA
		rec.Outcome = OutcomeIntegrated
		return Outcome{Integrated: true, Record: rec}, nil
	}
	if err := c.git.CompareAndSetBranch(ctx, req.RepoPath, req.TargetBranch, next, rec.TargetBeforeSHA); err != nil {
		// The lane guarantees no other integration moved the target, so a
		// failed compare-and-set means something outside AO wrote the branch
		// while we held it. That is a fact a person needs, not a retry.
		rec.Outcome = OutcomeNeedsAttention
		rec.Attention = &Attention{
			Reason:    ReasonTargetMoved,
			BaseSHA:   rec.BaseSHA,
			TargetSHA: rec.TargetBeforeSHA,
			SourceSHA: next,
			Strategy:  rec.Strategy,
			Detail:    "the target branch changed outside AO while this integration held the lane: " + err.Error(),
		}
		if rec.Replayed {
			c.undoReplay(ctx, req, rec.Strategy)
		}
		return needsAttention(rec), nil
	}
	rec.TargetAfterSHA = next
	rec.Outcome = OutcomeIntegrated
	if rec.Strategy != StrategyRebaseFastForward {
		// The result was built on a detached HEAD; give the worktree its branch
		// back now that the ref has moved.
		_ = c.git.CheckoutBranch(ctx, req.WorktreePath, req.SourceBranch)
	}
	return Outcome{Integrated: true, Record: rec}, nil
}

func (c *Coordinator) record(ctx context.Context, rec Record) error {
	if c.recorder == nil || rec.Strategy == "" && rec.Attention == nil {
		return nil
	}
	return c.recorder.RecordIntegration(ctx, rec)
}

// needsAttention is the one place an attention Record becomes an Outcome, so
// Outcome.Attention and Record.Attention can never disagree about whether a
// person has to act.
func needsAttention(rec Record) Outcome {
	return Outcome{Record: rec, Attention: rec.Attention}
}

func replayOp(strategy Strategy) (ReplayOp, error) {
	switch strategy {
	case StrategyRebaseFastForward:
		return ReplayRebase, nil
	case StrategyCherryPick:
		return ReplayCherryPick, nil
	case StrategyMergeCommit:
		return ReplayMerge, nil
	default:
		return "", fmt.Errorf("integration: %q is not a replay strategy", strategy)
	}
}
