package workflow

import (
	stdctx "context"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// review_changed_files.go -- what a task actually changed, as opposed to what
// its worktree happens to be dirty about.
//
// THE DEFECT THIS FIXES, found by a real P5-A phase 2 smoke (wf-a8dffdf6). The
// worker did the work correctly and COMMITTED it, which left the worktree
// clean. ReviewPolicy read its changed-file set from `git status` alone, saw
// nothing, and recorded `no_changed_files_observed` -- so a change that added a
// function and a test was classified as a change AO could not see. It failed in
// the safe direction (a reviewer ran) but it made the fast Task path
// unreachable for any worker that commits, which is most of them.
//
// The honest set is the task's change against the base its work started from,
// and it has two halves that must both be counted:
//
//	committed  = git diff --name-only <base>..<head>
//	uncommitted = the worktree's own porcelain status
//
// Both come from evidence AO already holds: the base is on the work step's own
// dispatch checkpoint, the head is on the observation AO just took, and the diff
// runs through RepairGit -- the narrow, injectable git port artifact resolution
// already uses, rather than a second way for this package to run git.
//
// WHAT `base..head` INCLUDES, said plainly. It is everything on this branch
// that was not there when the task started -- which is the change the branch
// proposes, and the change a reviewer would actually be handed. If a worker
// merged another branch in, that merge's files are in the set too. They are not
// attributed to the worker: CommittedChangedFileCount says how many paths came
// from history rather than the dirty tree, both SHAs are recorded so the diff
// can be re-run, and the effect is always MORE review, never less.
//
// EVERY UNPROVABLE CASE FAILS CLOSED. A base AO cannot name, a base that is not
// an ancestor of the head (a rewritten history), or a diff that errors, all
// resolve to "AO cannot prove what this task changed" -- which keeps the review
// and blocks relief, and never invents a base or drops a risk reason to make a
// change look cheap.

// changedFileSource names how a task's changed-file set was derived, so a
// decision can say where its evidence came from rather than assert it.
type changedFileSource string

const (
	// changedFilesFromDiffAndWorktree is the full answer: commits since the
	// task's base, plus whatever the worktree is still dirty about.
	changedFilesFromDiffAndWorktree changedFileSource = "base_diff_and_worktree"
	// changedFilesFromWorktree is the answer when the head is the base, so
	// there are no commits and the worktree IS the whole change. Still proven:
	// AO knows there is nothing committed to miss.
	changedFilesFromWorktree changedFileSource = "worktree_only"
	// changedFilesUnprovable means AO could not establish the task's change
	// set. It is not "no changes"; it is "no answer", and it is treated as the
	// risk it is.
	changedFilesUnprovable changedFileSource = "unprovable"
)

// taskChangeSet is the canonical answer to "what did this task change", with
// the provenance that makes it checkable.
type taskChangeSet struct {
	Paths  []string
	Source changedFileSource
	// BaseSHA and HeadSHA are the two ends of the diff, recorded so a reader
	// can re-run it. Empty when they could not be established.
	BaseSHA string
	HeadSHA string
	// CommittedCount and DirtyCount split the set by where each path came
	// from. This is what keeps a commit from being silently attributed to the
	// worker: the decision records how many paths came from the branch's
	// history rather than from work still sitting in the tree.
	CommittedCount int
	DirtyCount     int
	// Unprovable explains a `changedFilesUnprovable` source in terms a person
	// can act on.
	Unprovable string
}

// Proven reports whether AO established the set rather than failing to.
func (s taskChangeSet) Proven() bool { return s.Source != changedFilesUnprovable }

// taskChangedFiles derives the canonical changed-file set for one work step.
//
// It is deliberately pure of policy: it answers what changed and how it knows,
// and leaves every consequence to EvaluateReviewPolicy. The one judgement it
// makes is the fail-closed one -- when it cannot prove the set it says so,
// rather than returning a smaller set that would read as a smaller change.
func (c *Coordinator) taskChangedFiles(
	ctx stdctx.Context, worktreePath, baseSHA string, obs ports.WorkspaceObservation,
) taskChangeSet {
	dirty := workspaceChangedPaths(obs)
	set := taskChangeSet{
		BaseSHA:    strings.TrimSpace(baseSHA),
		HeadSHA:    strings.TrimSpace(obs.HeadSHA),
		DirtyCount: len(dirty),
	}

	// No base recorded: AO cannot say what this work started from, so it cannot
	// say what the work added. The dirty set may be complete or may be missing
	// every commit the worker made, and nothing here can tell those apart.
	if set.BaseSHA == "" {
		set.Source = changedFilesUnprovable
		set.Unprovable = "the work step recorded no base commit, so AO cannot tell committed work from work that was never done"
		set.Paths = dedupePaths(dirty)
		return set
	}
	// No head: the same problem from the other end.
	if set.HeadSHA == "" {
		set.Source = changedFilesUnprovable
		set.Unprovable = "AO could not read the worktree's HEAD, so it cannot diff the task against its base"
		set.Paths = dedupePaths(dirty)
		return set
	}

	// The head is the base: nothing was committed, so the worktree is the whole
	// change. This is a PROVEN answer, not a fallback -- AO knows there is no
	// committed work it is missing.
	if set.HeadSHA == set.BaseSHA {
		set.Source = changedFilesFromWorktree
		set.Paths = dedupePaths(dirty)
		return set
	}

	git := c.repairGit()
	// The base has to be a real commit in this checkout. A base AO cannot find
	// is a base AO must not diff against.
	switch exists, err := git.CommitExists(ctx, worktreePath, set.BaseSHA); {
	case err != nil:
		set.Source = changedFilesUnprovable
		set.Unprovable = "could not check the task's base commit: " + err.Error()
		set.Paths = dedupePaths(dirty)
		return set
	case !exists:
		set.Source = changedFilesUnprovable
		set.Unprovable = "the task's base commit " + shortSHA(set.BaseSHA) + " is not present in this checkout, so its history was rewritten or replaced"
		set.Paths = dedupePaths(dirty)
		return set
	}

	// The base must still be an ANCESTOR of the head. If it is not, the branch
	// no longer contains the point the work started from -- a rebase, a reset,
	// a force-push -- and `base..head` would describe a change that never
	// happened.
	switch contains, err := git.Contains(ctx, worktreePath, set.BaseSHA, set.HeadSHA); {
	case err != nil:
		set.Source = changedFilesUnprovable
		set.Unprovable = "could not check whether the task's base is still in this branch's history: " + err.Error()
		set.Paths = dedupePaths(dirty)
		return set
	case !contains:
		set.Source = changedFilesUnprovable
		set.Unprovable = "the task's base " + shortSHA(set.BaseSHA) + " is no longer an ancestor of " + shortSHA(set.HeadSHA) + ", so this branch's history was rewritten"
		set.Paths = dedupePaths(dirty)
		return set
	}

	committed, err := git.ChangedFiles(ctx, worktreePath, set.BaseSHA, set.HeadSHA)
	if err != nil {
		set.Source = changedFilesUnprovable
		set.Unprovable = "could not diff the task against its base: " + err.Error()
		set.Paths = dedupePaths(dirty)
		return set
	}

	set.Source = changedFilesFromDiffAndWorktree
	set.CommittedCount = len(committed)
	set.Paths = dedupePaths(append(append([]string{}, committed...), dirty...))
	return set
}

// dedupePaths merges the two halves into one sorted, duplicate-free list.
//
// Sorted because the set is persisted into a decision and compared across
// restarts, and an order that depends on which half a path came from would make
// two identical decisions look different. Deduplicated because a file that was
// committed AND is dirty again is one changed file, not two.
func dedupePaths(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// taskChangeBaseSHA answers "what commit did this task's work start from",
// which is the other half of the change set and the half that must not be
// guessed.
//
// It reads the work step's OWN checkpoint stream and takes the FIRST base any
// checkpoint recorded, because that is the point the task was dispatched at:
// dispatch.go writes it from the live worktree HEAD at launch (`the live tree
// wins over the session row's recorded base`), and every later checkpoint only
// carries it forward. Taking the first, not the latest, is deliberate:
//
//   - it covers the WHOLE task. A work step that was re-dispatched records a
//     later base -- the branch's HEAD at the second launch, which by then
//     already contains the first attempt's commits. Diffing from that base
//     would drop the earlier attempt's files from the set.
//   - it never reaches back before the task. The base is the branch's head at
//     the moment AO handed the branch to this worker, so a commit after it is
//     this task's; a commit before it is not in the diff at all. That is what
//     keeps foreign history from being attributed to the worker.
//   - it does not depend on carry-forward surviving. `the latest checkpoint`
//     resolves through an ORDER BY that ties on created_at, so a base read
//     from it is only as reliable as the tie-break; a base read from the
//     stream is the one that was actually written at dispatch.
//
// That is the one place it parts company with workDispatchCheckpoint, which
// takes the LATEST dispatch because work adoption is attributing one specific
// dispatch's commit. A review decision is about the whole task, so it starts
// where the task did.
//
// Returns the empty string when the stream records no base at all, which
// taskChangedFiles turns into a fail-closed, review-preserving decision.
func (c *Coordinator) taskChangeBaseSHA(ctx stdctx.Context, runID, workStepID string) (sha string, phase string) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return "", ""
	}
	for _, cp := range checkpoints {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != workStepID {
			continue
		}
		if base := strings.TrimSpace(cp.BaseSHA); base != "" {
			return base, cp.DurablePhase
		}
	}
	return "", ""
}
