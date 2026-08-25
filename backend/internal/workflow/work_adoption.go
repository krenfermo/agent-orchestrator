package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// work_adoption.go — completed-work reconciliation.
//
// Both overnight incidents ended the same way. AO lost track of a worker, the
// worker's output was real, and a person committed it by hand because there was
// nowhere else for it to go:
//
//	wf-00283521 / medusa-4               -> 74f053a6, worktree clean
//	wf-cd5bad10 / agent-orchestrator-35  -> 1de0aa7e, worktree clean
//
// Then Continue was pressed, and nothing happened. AO could see a clean
// worktree, a commit on the task's own branch, and a session it had itself
// started — and had no transition that could say "that is the work; carry on".
// The only routes out of a durably failed work step were "start another worker"
// (which would write a second implementation over the first) and "mark it done"
// (which would skip review entirely). Neither is acceptable, so the run stayed
// blocked.
//
// This is the missing third route, and it is narrow by construction. AO may
// ADOPT an existing commit as a work step's result — never mark the task
// complete — only when it can PROVE all of:
//
//  1. the commit is a descendant of the base this task was dispatched at, so it
//     builds on the task's own starting point rather than replacing it;
//  2. the expected work is actually present: the range base..head is a real,
//     non-empty change, and — when AO holds a write set for the task — it
//     touches it;
//  3. the branch and worktree are the ones AO recorded for THIS task;
//  4. no conflicting writer owns the tree: every step of this run is at rest,
//     no attempt is unfinished, no question is open, and this run's own agent
//     has been silent long enough that a delivery cannot still be landing;
//  5. the worker/session evidence supports the attribution: the session AO
//     spawned for this step exists and names this same worktree;
//  6. the worktree is stable — clean, or at least unchanged across the two
//     observations this function makes around its own proofs;
//  7. and the adoption is recorded durably, bounded, before anything moves.
//
// What happens next is the ordinary machinery, unchanged: the adopted work step
// completes, review dispatches against what is actually there, changes_requested
// still means a fix cycle, and verification still runs against exactly what a
// reviewer approved. Adoption buys a run its review; it never buys it a pass.

const (
	// workAdoptionPhase is the durable decision. Written BEFORE the step moves,
	// so a daemon that dies mid-transition resumes the same adoption rather than
	// re-deriving it against a repository that may have moved again.
	workAdoptionPhase = "work_commit_adopted"
	// maxWorkAdoptions bounds how many times ONE run may be rescued this way. A
	// run whose work has now been landed by hand three times without ever
	// reaching a verdict is not being helped by a fourth silent adoption.
	maxWorkAdoptions = 3
	// workAdoptionStabilityWindow is how long the two observations this function
	// makes must agree for the worktree to count as stable. It is deliberately
	// tiny: this is a check against a write landing DURING the proofs, not a
	// second settle window — settling is proof 4's job.
	workAdoptionStabilityWindow = 0
)

// workAdoptionRecord is the durable evidence. Every field is something AO
// proved or observed; none of it is anyone's assertion.
type workAdoptionRecord struct {
	Generation int `json:"generation"`
	// DispatchBaseSHA is the commit this task was dispatched at, and AdoptedSHA
	// the commit being adopted as its result.
	DispatchBaseSHA string `json:"dispatchBaseSha"`
	AdoptedSHA      string `json:"adoptedSha"`
	Branch          string `json:"branch"`
	WorktreePath    string `json:"worktreePath"`
	SessionID       string `json:"sessionId,omitempty"`
	// Fingerprint is the adopted workspace state, which becomes the review
	// target exactly as a worker-produced completion's would.
	Fingerprint string `json:"fingerprint"`
	// PatchIdentity is the stable identity of the change base..head. Recorded so
	// "the expected work is present" is a claim a person can re-check.
	PatchIdentity string `json:"patchIdentity,omitempty"`
	// Commits are the subjects between the base and the adopted commit, bounded.
	Commits []string `json:"commits,omitempty"`
	// WriteSetOverlap names the expected-write-set paths the adopted change
	// actually touches. Empty when AO holds no write set for the task.
	WriteSetOverlap []string `json:"writeSetOverlap,omitempty"`
	// PriorStopReason is what the run was parked on when the adoption happened.
	PriorStopReason string `json:"priorStopReason,omitempty"`
	// Attribution states exactly what AO proved and what it did not. AO never
	// claims to know WHO made the commit.
	Attribution string    `json:"attribution"`
	ObservedAt  time.Time `json:"observedAt"`
}

// adoptionRefusal is a refusal precise enough to log or put in front of a
// person. The predicate's default is refusal.
type adoptionRefusal string

// resumeAdoptedTaskCommit is the ContinueRun entry point.
//
// Its only caller is ContinueRun, for the same reason every other resume in
// this package has only that caller: it takes a work step out of a terminal
// state, and a 2s Board poll is not a person. It is a no-op for every run
// without exactly the durable evidence below, and every proof is re-derived
// against the repository as it stands NOW.
func (c *Coordinator) resumeAdoptedTaskCommit(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, bool, error) {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run, false, nil
	}
	var workStep, reviewStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		}
	}
	if workStep == nil || reviewStep == nil {
		return run, false, nil
	}
	// Only a work step AO gave up on. A completed one has already been reviewed
	// (or is about to be), and a running one is not lost.
	if workStep.State != domain.WorkflowStepFailed && workStep.State != domain.WorkflowStepWaiting {
		return run, false, nil
	}
	// And only before a reviewer has read anything. Adopting under a review that
	// has already run would be substituting a different tree for the one that
	// was read, which is the exact thing workspace_provenance.go exists to stop.
	if reviewStep.State != domain.WorkflowStepPending && reviewStep.State != domain.WorkflowStepReady {
		return run, false, nil
	}

	generation := c.workAdoptionGenerations(ctx, run.ID) + 1
	if generation > maxWorkAdoptions {
		return run, false, nil
	}

	rec, refusal := c.adoptableWorkCommit(ctx, run, steps, *workStep)
	if refusal != "" {
		if c.log != nil {
			c.log.Debug("workflow: no adoptable task commit for this run", "run", run.ID, "reason", string(refusal))
		}
		return run, false, nil
	}
	rec.Generation = generation
	if reason, _, ok := c.stopReason(ctx, run); ok {
		rec.PriorStopReason = reason
	}

	// The decision, durably, before anything moves. Not best-effort: a work step
	// completed on an adoption nobody wrote down would be untraceable work.
	if err := c.recordWorkAdoption(ctx, run, *workStep, rec); err != nil {
		return run, false, err
	}

	now := c.clock()
	// Out of the terminal state, then forward through the ordinary transitions:
	// failed -> ready -> running -> completed. Every hop is a real transition
	// the state machine already allows, so nothing here is a back door around
	// ValidWorkflowStepTransition.
	if workStep.State == domain.WorkflowStepFailed {
		if _, err := c.store.ReopenFailedWorkflowStep(ctx, workStep.ID, now); err != nil {
			return run, false, err
		}
		workStep.State = domain.WorkflowStepReady
	}
	if workStep.State == domain.WorkflowStepReady {
		if _, err := c.store.UpdateWorkflowStepState(ctx, workStep.ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil {
			return run, false, err
		}
		workStep.State = domain.WorkflowStepRunning
	}
	if workStep.State != domain.WorkflowStepRunning && workStep.State != domain.WorkflowStepWaiting {
		return run, false, nil
	}
	if _, err := c.store.UpdateWorkflowStepState(ctx, workStep.ID, workStep.State, domain.WorkflowStepCompleted, now); err != nil {
		return run, false, err
	}
	workStep.State = domain.WorkflowStepCompleted

	// The completion checkpoint, in exactly the shape observeWorkStep writes for
	// a worker-produced completion — same durable phase family, same
	// FingerprintAfter, same carried branch/worktree/session. That is what makes
	// review dispatch treat an adopted result identically to a produced one,
	// rather than needing a branch of its own.
	stepID := workStep.ID
	var sessionIDPtr *string
	if rec.SessionID != "" {
		sid := rec.SessionID
		sessionIDPtr = &sid
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:               "wfc-" + c.newID(),
		WorkflowRunID:    run.ID,
		WorkflowStepID:   &stepID,
		ProjectID:        run.ProjectID,
		SessionID:        sessionIDPtr,
		Branch:           rec.Branch,
		WorktreePath:     rec.WorktreePath,
		BaseSHA:          rec.DispatchBaseSHA,
		HeadSHA:          rec.AdoptedSHA,
		FingerprintAfter: rec.Fingerprint,
		NextAction:       "start_review",
		DurablePhase:     "worker_observed_" + string(WorkerResultAvailable),
		PayloadVersion:   "v1",
		RetryState:       "{}",
		CreatedAt:        now,
	}); err != nil {
		return run, false, err
	}

	// Close the abandoned attempt honestly: the attempt did not produce this
	// result — a person landing a commit did — so it is recorded as failed with
	// whatever class it failed on, and the adoption record is what says the work
	// nonetheless exists. Overwriting it as "succeeded" would be a lie in the
	// ledger about which agent did what.
	if latest, ok, aerr := c.store.GetLatestWorkflowAttempt(ctx, workStep.ID); aerr == nil && ok && latest.FinishedAt == nil {
		_ = c.store.UpdateWorkflowAttemptOutcome(ctx, latest.ID, now, domain.WorkflowAttemptFailed, latest.ErrorClass)
	}

	// Un-park. The stop this releases is one whose whole content was "AO lost
	// the worker", and AO has just proved where the work went.
	run = c.unparkRun(ctx, run, rec.PriorStopReason,
		fmt.Sprintf("an existing commit on this task's own branch was adopted as its work (%s), and review has not run yet", shortFingerprint(rec.AdoptedSHA)))
	if c.log != nil {
		c.log.Info("workflow: adopted an existing task commit as the work step's result",
			"run", run.ID, "generation", rec.Generation, "branch", rec.Branch,
			"base", shortFingerprint(rec.DispatchBaseSHA), "adopted", shortFingerprint(rec.AdoptedSHA))
	}
	return run, true, nil
}

// adoptableWorkCommit is the whole predicate, in one place, and its default is
// refusal. It never mutates anything.
func (c *Coordinator) adoptableWorkCommit(
	ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep, workStep domain.WorkflowStep,
) (workAdoptionRecord, adoptionRefusal) {
	refuse := func(format string, args ...any) (workAdoptionRecord, adoptionRefusal) {
		return workAdoptionRecord{}, adoptionRefusal(fmt.Sprintf(format, args...))
	}
	if c.workspaceFacts == nil {
		return refuse("AO has no way to observe the workspace")
	}
	dispatchCP, ok := c.workDispatchCheckpoint(ctx, run.ID, workStep.ID)
	if !ok {
		return refuse("AO has no durable record of this work step's dispatch")
	}
	if dispatchCP.WorktreePath == "" || dispatchCP.Branch == "" {
		return refuse("the dispatch record names no worktree or no branch, so nothing can be attributed to this task")
	}
	if dispatchCP.BaseSHA == "" {
		return refuse("AO has no durable record of the commit this task was dispatched at, so descent cannot be proved")
	}
	sessionID := ""
	if dispatchCP.SessionID != nil {
		sessionID = strings.TrimSpace(*dispatchCP.SessionID)
	}

	// Proof 5 — the session AO spawned for this step still exists and names this
	// same worktree. Not being able to read it refuses: "AO does not know whose
	// worktree this is" is exactly the ambiguity adoption must not absorb.
	if c.sessionFacts == nil || sessionID == "" {
		return refuse("AO cannot tell which session owns this worktree")
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil || !found {
		return refuse("AO could not read session %s, so the worktree's owner is unproven", sessionID)
	}
	if sess.Metadata.WorkspacePath != "" && sess.Metadata.WorkspacePath != dispatchCP.WorktreePath {
		return refuse("session %s names worktree %q, not this task's %q", sessionID, sess.Metadata.WorkspacePath, dispatchCP.WorktreePath)
	}

	// Proof 4 — nothing of AO's is moving.
	if refusal, settled := c.workspaceIsSettled(ctx, run, steps, dispatchCP); !settled {
		return refuse("%s", refusal)
	}

	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path: dispatchCP.WorktreePath, Branch: dispatchCP.Branch,
		SessionID: domain.SessionID(sessionID), ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return refuse("AO could not observe the worktree: %s", err.Error())
	}
	// Proof 3 — the tree AO is looking at is the one it recorded.
	if obs.Path != "" && obs.Path != dispatchCP.WorktreePath {
		return refuse("the observed worktree %q is not this task's own (%q)", obs.Path, dispatchCP.WorktreePath)
	}
	if obs.Branch != "" && obs.Branch != dispatchCP.Branch {
		return refuse("the worktree is on branch %q, not this task's %q", obs.Branch, dispatchCP.Branch)
	}
	if obs.HeadSHA == "" {
		return refuse("AO could not read the worktree's HEAD")
	}
	if obs.HeadSHA == dispatchCP.BaseSHA {
		return refuse("HEAD is still the dispatch base %s, so no commit has landed to adopt", shortFingerprint(dispatchCP.BaseSHA))
	}

	git := integration.NewExecGit("")
	// Proof 3b — the head is the TIP of this task's own branch, not some other
	// commit that merely happens to contain the base.
	branchTip, exists, err := git.ResolveCommitIfExists(ctx, dispatchCP.WorktreePath, dispatchCP.Branch)
	if err != nil {
		return refuse("AO could not read branch %q: %s", dispatchCP.Branch, err.Error())
	}
	if !exists || branchTip != obs.HeadSHA {
		return refuse("the worktree's HEAD %s is not the tip of branch %q", shortFingerprint(obs.HeadSHA), dispatchCP.Branch)
	}
	// Proof 1 — descent from the dispatch base. A commit that is not a
	// descendant is not this task building on its own start; it is a different
	// history, and only a person can say what it means.
	if _, exists, err := git.ResolveCommitIfExists(ctx, dispatchCP.WorktreePath, dispatchCP.BaseSHA); err != nil || !exists {
		return refuse("the dispatch base %s is no longer in the repository, or could not be read", shortFingerprint(dispatchCP.BaseSHA))
	}
	descends, err := git.IsAncestor(ctx, dispatchCP.WorktreePath, dispatchCP.BaseSHA, obs.HeadSHA)
	if err != nil {
		return refuse("AO could not prove HEAD %s descends from the dispatch base %s: %s",
			shortFingerprint(obs.HeadSHA), shortFingerprint(dispatchCP.BaseSHA), err.Error())
	}
	if !descends {
		return refuse("HEAD %s does not descend from the commit this task was dispatched at (%s)",
			shortFingerprint(obs.HeadSHA), shortFingerprint(dispatchCP.BaseSHA))
	}

	// Proof 2 — the expected work is actually there. An empty range is a branch
	// that moved without contributing anything, which is not a result to adopt.
	patch, err := git.PatchIdentity(ctx, dispatchCP.WorktreePath, dispatchCP.BaseSHA, obs.HeadSHA)
	if err != nil {
		return refuse("AO could not read the change between the dispatch base and HEAD: %s", err.Error())
	}
	if strings.TrimSpace(patch) == "" {
		return refuse("the commits on top of the dispatch base contribute no change at all")
	}

	// Proof 6 — the tree is stable: it must not still be being written. A second
	// observation that disagrees with the first means something is mid-write.
	obs2, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path: dispatchCP.WorktreePath, Branch: dispatchCP.Branch,
		SessionID: domain.SessionID(sessionID), ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return refuse("AO could not re-observe the worktree to prove it is stable: %s", err.Error())
	}
	if WorkspaceFingerprint(obs2) != WorkspaceFingerprint(obs) {
		return refuse("the worktree changed between two observations, so it is not stable enough to adopt")
	}

	rec := workAdoptionRecord{
		DispatchBaseSHA: dispatchCP.BaseSHA,
		AdoptedSHA:      obs.HeadSHA,
		Branch:          dispatchCP.Branch,
		WorktreePath:    dispatchCP.WorktreePath,
		SessionID:       sessionID,
		Fingerprint:     WorkspaceFingerprint(obs),
		PatchIdentity:   patch,
		ObservedAt:      c.clock(),
	}
	for _, commit := range obs.Commits {
		if commit.SHA == dispatchCP.BaseSHA {
			break
		}
		rec.Commits = append(rec.Commits, commit.SHA+" "+commit.Subject)
	}
	// The write-set overlap, when AO holds one. An absent write set is never
	// treated as a refusal: most runs have only an estimate, and an estimate is
	// not a contract. It is recorded because it is what a person checks first.
	if expected := c.expectedWriteSetFor(ctx, run); len(expected) > 0 {
		changed := changedFilePaths(obs)
		for _, p := range changed {
			for _, e := range expected {
				if p == e || (strings.HasSuffix(e, "/") && strings.HasPrefix(p, e)) {
					rec.WriteSetOverlap = append(rec.WriteSetOverlap, p)
					break
				}
			}
		}
	}
	rec.Attribution = fmt.Sprintf(
		"HEAD %s is the tip of this task's own branch %q in its own worktree %q, descends from the commit it was dispatched at (%s), and contributes a non-empty change. AO does not claim to know who authored it; what it proved is that the change is on the branch this task was authorized to work on and builds on this task's own starting point.",
		shortFingerprint(obs.HeadSHA), dispatchCP.Branch, dispatchCP.WorktreePath, shortFingerprint(dispatchCP.BaseSHA))
	return rec, ""
}

// workDispatchCheckpoint finds the work step's own dispatch record — the row
// recordDispatchSuccess wrote, naming the session, branch, worktree and the base
// commit the task started from.
func (c *Coordinator) workDispatchCheckpoint(ctx stdctx.Context, runID, stepID string) (domain.WorkflowCheckpoint, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.WorkflowCheckpoint{}, false
	}
	var found domain.WorkflowCheckpoint
	ok := false
	for _, cp := range cps {
		if cp.DurablePhase != workerDispatchedDurablePhase {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		if !ok || !cp.CreatedAt.Before(found.CreatedAt) {
			found, ok = cp, true
		}
	}
	return found, ok
}

// workAdoptionGenerations counts the adoptions already recorded for a run.
func (c *Coordinator) workAdoptionGenerations(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return 0
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == workAdoptionPhase {
			n++
		}
	}
	return n
}

// recordWorkAdoption writes the durable decision.
func (c *Coordinator) recordWorkAdoption(ctx stdctx.Context, run domain.WorkflowRun, workStep domain.WorkflowStep, rec workAdoptionRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	stepID := workStep.ID
	var sessionIDPtr *string
	if rec.SessionID != "" {
		sid := rec.SessionID
		sessionIDPtr = &sid
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		// Session, branch, worktree, base, head and fingerprint are carried on
		// this row as well as on the completion row that follows it. Both are
		// written at the same instant, so "the latest checkpoint for this step"
		// can legitimately resolve to either — and review dispatch reads exactly
		// these fields off it to find the worktree it must launch a reviewer
		// against. A row missing them would park the run on
		// review_dispatch_ambiguous immediately after a successful adoption.
		SessionID:        sessionIDPtr,
		Branch:           rec.Branch,
		WorktreePath:     rec.WorktreePath,
		BaseSHA:          rec.DispatchBaseSHA,
		HeadSHA:          rec.AdoptedSHA,
		FingerprintAfter: rec.Fingerprint,
		RetryState:       string(payload),
		NextAction: fmt.Sprintf(
			"work_commit_adopted: %s on branch %s is adopted as this task's work (adoption %d of %d); it goes through the ordinary review and verification, and nothing about it is treated as approved",
			shortFingerprint(rec.AdoptedSHA), rec.Branch, rec.Generation, maxWorkAdoptions),
		DurablePhase:   workAdoptionPhase,
		PayloadVersion: "v1",
		CreatedAt:      c.clock(),
	})
	return err
}
