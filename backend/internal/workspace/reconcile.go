package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// This file is what happens to an AO worktree after the agent stops typing:
// the durable record that its work landed, the cleanup that authorizes,
// the refusal to clean up when it did not, and the pass that finishes any of
// the three after a restart cut them in half.
//
// Everything here follows from one asymmetry. Leaving a worktree behind costs
// a directory; deleting a branch that still holds the only copy of somebody's
// work costs the work. So no step below is reversible-by-guess: each one is
// authorized by a durable fact recorded before it, and where the fact is
// missing the answer is always "leave it alone".
//
// The three crash windows this closes, in the order a task passes through them:
//
//   - Mid-creation. The record is written (state creating) before `git worktree
//     add` runs, so a restart in between finds a row that names the directory
//     and the branch. Reconcile asks the filesystem which side of the git call
//     the crash fell on and heals accordingly -- and never re-adds a worktree
//     that is already there.
//
//   - Mid-integration. The Integration Coordinator's own ledger already covers
//     the ref update (it writes the intent before the ref moves), so a restart
//     there is answered by the audit row, not by this file. What this file adds
//     is that nothing is cleaned up until that integration is a recorded fact,
//     so a half-finished integration always still has its worktree and branch
//     to be retried from.
//
//   - Post-integration, pre-cleanup. This is the window with no natural
//     witness: the work is on the target, and the worktree and branch look
//     exactly as they did before it got there. TaskWorktreeIntegrated is that
//     witness, written after the audit and before the first removal. A restart
//     inside it resumes the cleanup instead of integrating a second time.

var (
	// ErrNotIntegrated means cleanup was asked for a task whose work is not
	// durably recorded as integrated. It is a refusal rather than a no-op: a
	// caller that thinks a task is finished when its record does not say so is
	// a caller whose next step would be to delete commits nothing else holds.
	ErrNotIntegrated = errors.New("workspace: the task's work is not recorded as integrated")
	// ErrNoRecord means the task has no worktree record. A direct_branch task
	// never has one, so callers that may see either treat it as "nothing to do".
	ErrNoRecord = errors.New("workspace: the task has no worktree record")
)

// MarkIntegrated records that a task's work is durably on its target ref, at
// integratedSHA, and that its worktree and branch may therefore be cleaned up.
//
// It is the ONLY thing that authorizes cleanup, and it is deliberately a
// separate write from the cleanup itself. Doing both at once would mean a
// crash between the ref update and the teardown left a worktree that looks
// un-integrated -- the state that produces a duplicate integration -- and a
// crash between the teardown and the record left work whose only account of
// where it landed was gone.
//
// It is idempotent, and idempotent in the strict sense: marking a record that
// is already integrated (or already cleaned up) at the SAME commit is a no-op,
// and marking one at a DIFFERENT commit is an error rather than an overwrite.
// The recorded SHA is what later authorizes deleting the branch, so silently
// replacing it would let a second, wrong integration authorize throwing away
// the first one's evidence.
func (m *Manager) MarkIntegrated(ctx context.Context, taskID, integratedSHA string) (domain.TaskWorktreeRecord, error) {
	integratedSHA = strings.TrimSpace(integratedSHA)
	if integratedSHA == "" {
		return domain.TaskWorktreeRecord{}, fmt.Errorf("%w: task %s: an integrated commit is required", ErrInvalidRequest, taskID)
	}
	rec, found, err := m.store.GetTaskWorktree(ctx, taskID)
	if err != nil {
		return domain.TaskWorktreeRecord{}, fmt.Errorf("workspace: read record for task %s: %w", taskID, err)
	}
	if !found {
		return domain.TaskWorktreeRecord{}, fmt.Errorf("%w: %s", ErrNoRecord, taskID)
	}
	if rec.IntegratedSHA != "" && rec.IntegratedSHA != integratedSHA {
		return rec, fmt.Errorf("workspace: task %s is already recorded as integrated at %s, not %s",
			taskID, rec.IntegratedSHA, integratedSHA)
	}
	if rec.IntegratedSHA == integratedSHA && rec.State != domain.TaskWorktreeActive && rec.State != domain.TaskWorktreeCreating {
		// Already marked, and already at or past `integrated`. Re-marking would
		// move a released row back to integrated and re-open a cleanup that is
		// finished.
		return rec, nil
	}
	rec.IntegratedSHA = integratedSHA
	rec.State = domain.TaskWorktreeIntegrated
	rec.Detail = ""
	rec.UpdatedAt = m.now()
	if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
		return domain.TaskWorktreeRecord{}, fmt.Errorf("workspace: record task %s integrated: %w", taskID, err)
	}
	return rec, nil
}

// Preserve marks a task's worktree as evidence to keep: the task failed, was
// cancelled, or was otherwise abandoned, and whatever its agent committed is
// not on any target branch.
//
// It is the durable "do not clean this up". Without it, a cancelled task's
// leftovers are indistinguishable from an abandoned directory, and the next
// tidy-up pass has to decide from the filesystem alone whether the commits on
// an ao/* branch matter to anyone -- a question the filesystem cannot answer.
//
// A record whose work HAS integrated is never preserved: it is already past
// the point where its branch holds anything unique, and calling it evidence
// would strand a cleanup that is safe to finish.
func (m *Manager) Preserve(ctx context.Context, taskID, reason string) (domain.TaskWorktreeRecord, bool, error) {
	rec, found, err := m.store.GetTaskWorktree(ctx, taskID)
	if err != nil {
		return domain.TaskWorktreeRecord{}, false, fmt.Errorf("workspace: read record for task %s: %w", taskID, err)
	}
	if !found {
		// A direct_branch task, or one that never got a worktree. Nothing was
		// created, so there is nothing to preserve and nothing went wrong.
		return domain.TaskWorktreeRecord{}, false, nil
	}
	if rec.State == domain.TaskWorktreePreserved {
		return rec, true, nil
	}
	if rec.IntegratedSHA != "" {
		return rec, true, nil
	}
	rec.State = domain.TaskWorktreePreserved
	rec.Detail = strings.TrimSpace(reason)
	rec.UpdatedAt = m.now()
	if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
		return domain.TaskWorktreeRecord{}, false, fmt.Errorf("workspace: record task %s preserved: %w", taskID, err)
	}
	return rec, true, nil
}

// Cleanup is what a task's worktree and branch get after its work has landed:
// the directory removed, the ao/* branch deleted, and the record closed.
//
// Its whole contract is that every destructive step is authorized by something
// durable and provable, in this order:
//
//  1. The record must say the work integrated, and at which commit. Anything
//     else is ErrNotIntegrated and nothing is touched.
//  2. The directory is removed WITHOUT force, so uncommitted agent work makes
//     the removal fail rather than vanish.
//  3. The record is closed to `released` BEFORE the branch is deleted, so a
//     crash in between leaves a row that says "the checkout is gone, the branch
//     is not" -- which is exactly what a later pass needs to finish the job.
//  4. The branch is deleted only when its tip is provably reachable from the
//     commit the work landed at, and only from that exact tip. A branch that
//     has moved, or that holds commits the target does not, keeps every one of
//     them and says so.
//
// It is idempotent at every step: a directory already gone is pruned rather
// than removed, a branch already gone is recorded as gone, and a fully cleaned
// record is a no-op. Running it twice -- which a restart guarantees -- can
// therefore never do the second half of anything twice.
func (m *Manager) Cleanup(ctx context.Context, taskID string) (CleanupResult, error) {
	rec, found, err := m.store.GetTaskWorktree(ctx, taskID)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("workspace: read record for task %s: %w", taskID, err)
	}
	if !found {
		return CleanupResult{}, fmt.Errorf("%w: %s", ErrNoRecord, taskID)
	}
	return m.cleanup(ctx, rec)
}

// CleanupResult is what one cleanup actually did, so a caller can report it
// without re-reading the repository.
type CleanupResult struct {
	Record domain.TaskWorktreeRecord
	// WorktreeRemoved is true only when this call removed the directory. False
	// when it was already gone (the stale registration is still pruned).
	WorktreeRemoved bool
	// BranchDeleted is true when the ao/* branch is gone by the end of the
	// call, whether this call deleted it or found it already absent.
	BranchDeleted bool
	// BranchKept explains why the branch is still there. Empty when it is not.
	// It is the field to read when cleanup "did nothing": a branch is kept only
	// for a reason that names work AO refused to throw away.
	BranchKept string
}

func (m *Manager) cleanup(ctx context.Context, rec domain.TaskWorktreeRecord) (CleanupResult, error) {
	switch {
	case rec.State == domain.TaskWorktreeReleased && rec.BranchDeleted:
		return CleanupResult{Record: rec, BranchDeleted: true}, nil
	case rec.State == domain.TaskWorktreeIntegrated, rec.State == domain.TaskWorktreeReleased:
		// `integrated` is the ordinary entry point; `released` is a cleanup
		// whose branch half never finished (a crash between step 3 and step 4).
	default:
		return CleanupResult{Record: rec}, fmt.Errorf("%w: task %s is %s", ErrNotIntegrated, rec.TaskID, rec.State)
	}
	if rec.IntegratedSHA == "" {
		// Only reachable for a pre-0131 row that was released before the
		// integrated commit was recorded. The checkout can still be torn down;
		// the branch cannot, because nothing here can prove what is on it.
		return m.cleanupWithoutBranch(ctx, rec)
	}

	result := CleanupResult{Record: rec}
	if rec.State != domain.TaskWorktreeReleased {
		removed, err := m.tearDownCheckout(ctx, rec)
		if err != nil {
			return result, err
		}
		result.WorktreeRemoved = removed

		now := m.now()
		rec.State = domain.TaskWorktreeReleased
		rec.Detail = ""
		rec.UpdatedAt = now
		rec.ReleasedAt = &now
		if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
			return result, fmt.Errorf("workspace: record task %s released: %w", rec.TaskID, err)
		}
		result.Record = rec
	}

	deleted, kept, err := m.deleteBranchIfSafe(ctx, rec)
	if err != nil {
		return result, err
	}
	result.BranchDeleted, result.BranchKept = deleted, kept
	if deleted && !rec.BranchDeleted {
		rec.BranchDeleted = true
		rec.UpdatedAt = m.now()
		if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
			return result, fmt.Errorf("workspace: record task %s branch deleted: %w", rec.TaskID, err)
		}
	}
	result.Record = rec
	return result, nil
}

// cleanupWithoutBranch tears the checkout down for a record that cannot prove
// where its work landed. The branch is kept, with the reason recorded, because
// "I do not know" and "it is safe to delete" must never collapse into one
// answer here.
//
// The one thing it can still settle is absence: a branch that is not there
// needs no proof and no further passes, so the record is closed rather than
// left advertising an obligation nothing can discharge.
func (m *Manager) cleanupWithoutBranch(ctx context.Context, rec domain.TaskWorktreeRecord) (CleanupResult, error) {
	result := CleanupResult{Record: rec}
	if rec.State != domain.TaskWorktreeReleased {
		removed, err := m.tearDownCheckout(ctx, rec)
		if err != nil {
			return result, err
		}
		result.WorktreeRemoved = removed
		now := m.now()
		rec.State = domain.TaskWorktreeReleased
		rec.UpdatedAt = now
		rec.ReleasedAt = &now
		if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
			return result, fmt.Errorf("workspace: record task %s released: %w", rec.TaskID, err)
		}
		result.Record = rec
	}

	if rec.Branch != "" {
		exists, err := m.git.BranchExists(ctx, rec.RepoPath, rec.Branch)
		if err != nil {
			return result, err
		}
		if exists {
			result.BranchKept = fmt.Sprintf(
				"%s is still there and the commit this task's work integrated at was never recorded, so nothing here can prove it is safe to delete",
				rec.Branch)
			return result, nil
		}
	}
	result.BranchDeleted = true
	if !rec.BranchDeleted {
		rec.BranchDeleted = true
		rec.UpdatedAt = m.now()
		if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
			return result, fmt.Errorf("workspace: record task %s branch deleted: %w", rec.TaskID, err)
		}
		result.Record = rec
	}
	return result, nil
}

// tearDownCheckout removes the worktree directory, or prunes its registration
// when the directory is already gone. It reports whether a removal happened.
//
// It never forces. A worktree holding uncommitted changes makes git refuse,
// and that refusal is returned as-is: the task's work integrated, but whatever
// is dirty in the directory did not, and deleting it to tidy up is not a trade
// this manager is allowed to make. The record is deliberately left at
// `integrated` rather than marked failed, so the obligation to finish this
// cleanup survives and the next pass retries it.
func (m *Manager) tearDownCheckout(ctx context.Context, rec domain.TaskWorktreeRecord) (bool, error) {
	present, err := m.fs.DirExists(rec.Path)
	if err != nil {
		return false, err
	}
	if !present {
		// Nothing on disk; the registration may still be there. Prune deletes
		// no files, so this cannot lose anything.
		if err := m.git.Prune(ctx, rec.RepoPath); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := m.git.RemoveWorktree(ctx, rec.RepoPath, rec.Path); err != nil {
		return false, fmt.Errorf("workspace: remove worktree for task %s: %w", rec.TaskID, err)
	}
	return true, nil
}

// deleteBranchIfSafe deletes the task's ao/* branch, and returns the reason it
// did not when it did not.
//
// "Safe" is one specific proof and no inference: the branch's current tip must
// be reachable from the commit the integration recorded, so every commit on it
// already exists on the target and deleting the ref loses nothing. A branch
// that fails the test is not a problem to be reported as an error -- it is work
// AO is choosing to keep -- so it comes back as a reason.
func (m *Manager) deleteBranchIfSafe(ctx context.Context, rec domain.TaskWorktreeRecord) (bool, string, error) {
	if rec.Branch == "" {
		return false, "the record names no branch", nil
	}
	exists, err := m.git.BranchExists(ctx, rec.RepoPath, rec.Branch)
	if err != nil {
		return false, "", err
	}
	if !exists {
		// Already gone -- a previous cleanup that crashed after the delete but
		// before recording it, or a person who removed it. Either way the
		// cleanup is finished, and saying so is what stops every later pass
		// from looking again.
		return true, "", nil
	}
	tip, err := m.git.ResolveCommit(ctx, rec.RepoPath, "refs/heads/"+rec.Branch)
	if err != nil {
		return false, "", err
	}
	contained, err := m.git.IsAncestor(ctx, rec.RepoPath, tip, rec.IntegratedSHA)
	if err != nil {
		return false, "", err
	}
	if !contained {
		return false, fmt.Sprintf(
			"%s is at %s, which is not contained in the commit this task integrated at (%s), so it holds work that exists nowhere else",
			rec.Branch, shortSHA(tip), shortSHA(rec.IntegratedSHA)), nil
	}
	// Compare-and-delete from the tip that was just proved. A branch somebody
	// moved between the proof and the delete fails here and keeps its commits.
	if err := m.git.DeleteBranch(ctx, rec.RepoPath, rec.Branch, tip); err != nil {
		return false, "", fmt.Errorf("workspace: delete branch %s for task %s: %w", rec.Branch, rec.TaskID, err)
	}
	return true, "", nil
}

// ReconcileAction is what a reconciliation pass did about one record.
type ReconcileAction string

// The actions, in rough order of how much they did.
const (
	// ReconcileAdopted is the answer that matters most: the record and the
	// directory agree, so nothing at all happened. An existing worktree is
	// never recreated, and adopting it is how that is guaranteed rather than
	// hoped for.
	ReconcileAdopted ReconcileAction = "adopted"
	// ReconcileRecovered is a record that crashed mid-creation whose directory
	// turned out to exist: the `git worktree add` landed and only the state
	// write did not, so the row is moved to active without touching git.
	ReconcileRecovered ReconcileAction = "recovered"
	// ReconcilePruned is a record whose directory is gone. The stale
	// registration is dropped (which deletes nothing) and the row is left as it
	// is: Ensure re-materialises it onto its EXISTING branch when the task next
	// runs, which is what keeps whatever the agent committed.
	ReconcilePruned ReconcileAction = "pruned"
	// ReconcileCleanedUp is an integration whose cleanup was interrupted and
	// has now finished.
	ReconcileCleanedUp ReconcileAction = "cleaned_up"
	// ReconcileKept is a record deliberately left alone: preserved evidence, or
	// a cleanup whose branch could not be proved safe to delete.
	ReconcileKept ReconcileAction = "kept"
	// ReconcileBlocked is a record whose reconciliation failed. It is reported
	// rather than raised: one unreadable repository must not stop the daemon
	// from reconciling every other task.
	ReconcileBlocked ReconcileAction = "blocked"
)

// ReconcileEntry is what happened to one record.
type ReconcileEntry struct {
	TaskID string
	Action ReconcileAction
	// Detail explains a kept or blocked entry, and is empty otherwise.
	Detail string
	Record domain.TaskWorktreeRecord
}

// ReconcileReport is one pass, in full. It is returned rather than logged so
// the caller decides what is worth saying.
type ReconcileReport struct {
	Entries []ReconcileEntry
}

// TaskIDs returns the tasks a pass took one particular action on, in the order
// it took them.
func (r ReconcileReport) TaskIDs(action ReconcileAction) []string {
	var out []string
	for _, e := range r.Entries {
		if e.Action == action {
			out = append(out, e.TaskID)
		}
	}
	return out
}

// Reconcile matches every AO worktree record the manager is not finished with
// against what is actually in the repository, and heals the difference.
//
// This is the boot pass. It is the only place that reads the durable records
// and the filesystem together, and every rule in it is a rule about which of
// the two to believe:
//
//	creating  + directory present -> the git call landed and the state write
//	                                 did not. Believe the directory: move to
//	                                 active. Never re-add.
//	creating  + directory absent  -> the crash fell before the git call, or on
//	                                 it. Prune the registration and leave the
//	                                 row: Ensure re-materialises it onto the
//	                                 EXISTING branch, which is what keeps any
//	                                 commits the branch already holds.
//	active    + directory present -> they agree. Do nothing, run no git.
//	active    + directory absent  -> the registration outlived the directory.
//	                                 Prune; the row still names the branch, so
//	                                 the work is not lost and Ensure recovers.
//	integrated                    -> the crash fell between the integration and
//	                                 its cleanup. Finish the cleanup; do NOT
//	                                 integrate again, which is the whole reason
//	                                 the state exists.
//	released, branch still there  -> the crash fell inside the cleanup. Finish
//	                                 the branch half.
//	preserved / failed            -> a decision somebody already made. Left
//	                                 exactly as it is, forever, by design.
//
// It is idempotent by construction: every rule's postcondition is a state the
// same rule would leave alone on the next pass, so running it twice does the
// second half of nothing twice.
func (m *Manager) Reconcile(ctx context.Context) (ReconcileReport, error) {
	records, err := m.store.ListUnfinishedTaskWorktrees(ctx)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("workspace: list unfinished worktree records: %w", err)
	}
	report := ReconcileReport{}
	for _, rec := range records {
		report.Entries = append(report.Entries, m.reconcileOne(ctx, rec))
	}
	return report, nil
}

func (m *Manager) reconcileOne(ctx context.Context, rec domain.TaskWorktreeRecord) ReconcileEntry {
	switch rec.State {
	case domain.TaskWorktreeCreating, domain.TaskWorktreeActive:
		return m.reconcileCheckout(ctx, rec)
	case domain.TaskWorktreeIntegrated, domain.TaskWorktreeReleased:
		result, err := m.cleanup(ctx, rec)
		switch {
		case err != nil:
			return blocked(rec, err)
		case result.BranchKept != "":
			return ReconcileEntry{TaskID: rec.TaskID, Action: ReconcileKept, Detail: result.BranchKept, Record: result.Record}
		default:
			return ReconcileEntry{TaskID: rec.TaskID, Action: ReconcileCleanedUp, Record: result.Record}
		}
	default:
		// preserved, failed, and anything a newer build writes that this one
		// does not understand. Doing nothing is the only safe answer to a state
		// whose meaning is not known here.
		return ReconcileEntry{TaskID: rec.TaskID, Action: ReconcileKept,
			Detail: "the record is " + string(rec.State), Record: rec}
	}
}

// reconcileCheckout is the creating/active half: believe the filesystem about
// whether the directory is there, and never create one.
func (m *Manager) reconcileCheckout(ctx context.Context, rec domain.TaskWorktreeRecord) ReconcileEntry {
	present, err := m.fs.DirExists(rec.Path)
	if err != nil {
		return blocked(rec, err)
	}
	if present {
		if rec.State == domain.TaskWorktreeActive {
			return ReconcileEntry{TaskID: rec.TaskID, Action: ReconcileAdopted, Record: rec}
		}
		// The worktree was created and the state write never landed. Promoting
		// the row is the whole repair; git is not asked to do anything, which
		// is what "does not recreate an existing worktree" means in practice.
		rec.State = domain.TaskWorktreeActive
		rec.Detail = ""
		rec.UpdatedAt = m.now()
		if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
			return blocked(rec, err)
		}
		return ReconcileEntry{TaskID: rec.TaskID, Action: ReconcileRecovered, Record: rec}
	}
	// No directory. Drop the registration so a later Ensure can re-materialise
	// the path, and leave the row's state alone -- it still names the branch,
	// and Ensure checks out that branch rather than cutting a new one from
	// base, so nothing the agent committed is lost.
	if err := m.git.Prune(ctx, rec.RepoPath); err != nil {
		return blocked(rec, err)
	}
	return ReconcileEntry{TaskID: rec.TaskID, Action: ReconcilePruned, Record: rec,
		Detail: "the worktree directory is gone; the branch still holds the task's commits"}
}

func blocked(rec domain.TaskWorktreeRecord, err error) ReconcileEntry {
	return ReconcileEntry{TaskID: rec.TaskID, Action: ReconcileBlocked, Detail: err.Error(), Record: rec}
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
