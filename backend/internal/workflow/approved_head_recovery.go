package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// approved_head_recovery.go — the commit an approval was given for, for runs
// whose ledger never wrote it down.
//
// The incident this file exists for: a review approved a target, fix cycles
// advanced the branch, and verification later stopped with
// verify_workspace_changed and a provenance class of UNKNOWN — not because
// anything was wrong, but because AO could not name the commit the approved
// fingerprint had been read at. Every downstream question (has the history been
// rewritten? did the branch merely grow? is this uncommitted work?) is asked
// against that commit, so an unknown approved head makes all of them
// unanswerable and the run parks. ContinueRun re-derives the same unknown and
// parks again, forever.
//
// pinReviewTargetHead (review_dispatch.go) closes this for every review cycle
// dispatched from now on: it binds each cycle's target fingerprint to the
// commit it names, durably, at dispatch. This file is about the runs that are
// ALREADY on disk, whose rows were written before that pin existed.
//
// It answers with exactly two mechanisms and nothing in between:
//
//  1. RECONSTRUCTION, when the answer is deterministically derivable from facts
//     AO already holds. A WorkspaceFingerprint of a CLEAN worktree is a pure
//     function of one input — the commit — so the commit can be recovered by
//     searching the branch's own history for the unique preimage. This is a
//     proof, not an inference: sha256 is not guessed backwards, it is
//     recomputed forwards until it matches.
//
//  2. An explicit, human-only, bounded OPERATOR RECOVERY when it is not.
//     RecoverUnprovableApprovedHead never invents a commit and never verifies
//     against an approval AO cannot locate. It does the one safe thing left:
//     discards the unlocatable approval and asks for one fresh review of what
//     is actually in the worktree — the same fresh review every other
//     recovery in this package asks for, through the same dispatch identity.
//
// What is deliberately absent is a third option. AO never fabricates an
// approved head, never falls back to "the commit that looks closest", and never
// promotes an unproven baseline into the uncommitted-drift branch. Where
// provenance genuinely cannot be proven the behaviour stays fail-closed; what
// changes is that the run is no longer permanently useless.

// CommitHistory lists the commits reachable from a worktree's HEAD, newest
// first. It is a port of its own rather than a method on integration.Git
// because that interface is deliberately scoped to the integration
// coordinator's needs, and this is a read no integration ever makes.
//
// Optional: a coordinator with none wired falls back to the exec
// implementation, and a repository that cannot be read produces no candidates,
// which is a refusal rather than a wrong answer.
type CommitHistory interface {
	// ListCommits returns up to limit commit SHAs reachable from the worktree's
	// current HEAD, newest first.
	ListCommits(ctx stdctx.Context, worktreePath string, limit int) ([]string, error)
}

type execCommitHistory struct{}

func (execCommitHistory) ListCommits(ctx stdctx.Context, worktreePath string, limit int) ([]string, error) {
	if strings.TrimSpace(worktreePath) == "" || limit <= 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath,
		"rev-list", fmt.Sprintf("--max-count=%d", limit), "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("workflow: listing commits in %s: %w", worktreePath, err)
	}
	var shas []string
	for _, line := range strings.Split(string(out), "\n") {
		if sha := strings.TrimSpace(line); sha != "" {
			shas = append(shas, sha)
		}
	}
	return shas, nil
}

func (c *Coordinator) commitHistoryOrDefault() CommitHistory {
	if c.commitHistory != nil {
		return c.commitHistory
	}
	return execCommitHistory{}
}

const (
	// approvedHeadReconstructionPhase records ONE attempt to recover the commit
	// an approved fingerprint was read at, and its outcome. It is written
	// whether the search succeeded or not, for two different reasons: a success
	// becomes the durable row every later read resolves through (rule 3 of
	// approvedHeadSHA finds it like any other observation), and a completed
	// failure stops every subsequent poll from re-running a git subprocess to
	// re-derive the same "no".
	approvedHeadReconstructionPhase = "approved_head_reconstructed"
	// maxApprovedHeadSearchCommits bounds the history walk. A review target is
	// never far from the branch tip — it is a commit this same run's own work or
	// fix cycle produced — and an unbounded rev-list on a large repository is a
	// cost every poll would pay. A target beyond this bound is reported as
	// unreconstructible, which is the honest answer and routes to the operator
	// path rather than to a wrong commit.
	maxApprovedHeadSearchCommits = 500
)

// approvedHeadReconstruction is the decoded form of that checkpoint's
// RetryState. Fingerprint identifies WHICH question was asked, so a row about
// one approval never answers another.
type approvedHeadReconstruction struct {
	Fingerprint  string `json:"fingerprint"`
	ReviewStepID string `json:"reviewStepId,omitempty"`
	HeadSHA      string `json:"headSha,omitempty"`
	// Searched is how many commits the walk actually examined. Recorded because
	// it is what makes a negative answer readable: "not in the last 500 commits"
	// is a different statement from "the repository could not be read", and only
	// the first is worth remembering.
	Searched int `json:"searched"`
	// WorktreePath is where the search ran, so a negative answer recorded
	// against one checkout is not reused for another.
	WorktreePath string `json:"worktreePath,omitempty"`
}

// cleanWorkspaceFingerprint is the fingerprint of a worktree at `head` with
// nothing uncommitted: no dirty files, nothing staged, nothing untracked, no
// changes.
//
// It calls the SAME WorkspaceFingerprint every observation calls, deliberately.
// The reconstruction stands entirely on the two being the identical function —
// a reimplementation here that drifted by one field would silently start
// matching the wrong commit, which is the one failure mode this whole file is
// built to exclude.
func cleanWorkspaceFingerprint(head string) string {
	return WorkspaceFingerprint(ports.WorkspaceObservation{HeadSHA: head})
}

// reconstructApprovedHead recovers the commit an approved fingerprint was read
// at, for a run whose ledger never recorded it — and returns "" whenever it
// cannot prove one.
//
// THE WARRANT, precisely. WorkspaceFingerprint hashes a canonical block whose
// first line is `head_sha=<commit>` and whose remaining lines describe
// uncommitted state. For a CLEAN worktree those remaining lines are constant
// (`dirty=false`, `staged=false`, `untracked=false`, no `change=` lines), so
// the whole block — and therefore the fingerprint — is a pure function of the
// commit. Recomputing that function forwards over the commits on this branch
// and finding one whose value equals the approved fingerprint is a PROOF that
// the approval was read at that commit: nothing else hashes to it.
//
// Three properties follow, and all three matter:
//
//   - It cannot produce a false positive. A match is a sha256 preimage, not a
//     heuristic. Two commits cannot share one.
//   - It cannot produce a false positive for a DIRTY approval either. If the
//     approved state carried uncommitted work, its fingerprint includes
//     `change=` lines and no clean-tree hash can equal it, so the search simply
//     finds nothing and the caller fails closed. This is why the function needs
//     no separate "was it clean?" test: the arithmetic already refuses.
//   - It is deterministic and repeatable, so recording the answer is caching,
//     never a decision.
func (c *Coordinator) reconstructApprovedHead(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStepID, reviewedFingerprint string,
	workCP domain.WorkflowCheckpoint,
) string {
	if reviewedFingerprint == "" || workCP.WorktreePath == "" {
		return ""
	}
	// A recorded answer wins: it is the same computation, already done.
	if rec, found := c.recordedApprovedHeadReconstruction(ctx, run.ID, reviewedFingerprint, workCP.WorktreePath); found {
		return rec.HeadSHA
	}
	commits, err := c.commitHistoryOrDefault().ListCommits(ctx, workCP.WorktreePath, maxApprovedHeadSearchCommits)
	if err != nil {
		// The repository could not be read. That is not a negative answer and
		// must not be recorded as one — the next pass, on a mounted disk or an
		// existing checkout, may well answer it.
		if c.log != nil {
			c.log.Debug("workflow: could not read commit history to reconstruct an approved head",
				"run", run.ID, "worktree", workCP.WorktreePath, "err", err)
		}
		return ""
	}
	found := ""
	for _, sha := range commits {
		if cleanWorkspaceFingerprint(sha) == reviewedFingerprint {
			found = sha
			break
		}
	}
	c.recordApprovedHeadReconstruction(ctx, run, approvedHeadReconstruction{
		Fingerprint:  reviewedFingerprint,
		ReviewStepID: reviewStepID,
		HeadSHA:      found,
		Searched:     len(commits),
		WorktreePath: workCP.WorktreePath,
	}, workCP)
	if found != "" && c.log != nil {
		c.log.Info("workflow: reconstructed the commit an approved review target was read at",
			"run", run.ID, "fingerprint", shortFingerprint(reviewedFingerprint),
			"head", shortFingerprint(found), "searched", len(commits))
	}
	return found
}

// recordedApprovedHeadReconstruction returns this run's already-recorded answer
// for one fingerprint, if there is one.
//
// A recorded NEGATIVE counts as an answer and is returned as one (HeadSHA "",
// found true), so a poll loop does not re-run git every time. It is scoped to
// the worktree the search ran in: a negative proved against one checkout says
// nothing about a different one.
func (c *Coordinator) recordedApprovedHeadReconstruction(
	ctx stdctx.Context, runID, fingerprint, worktreePath string,
) (approvedHeadReconstruction, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return approvedHeadReconstruction{}, false
	}
	var newest *approvedHeadReconstruction
	for i := range cps {
		cp := &cps[i]
		if cp.DurablePhase != approvedHeadReconstructionPhase {
			continue
		}
		var rec approvedHeadReconstruction
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		if rec.Fingerprint != fingerprint {
			continue
		}
		if rec.HeadSHA == "" && rec.WorktreePath != worktreePath {
			// A negative from a different checkout proves nothing here.
			continue
		}
		copied := rec
		newest = &copied
	}
	if newest == nil {
		return approvedHeadReconstruction{}, false
	}
	return *newest, true
}

// recordApprovedHeadReconstruction appends the search's own row. Best-effort:
// losing it costs one repeated git walk, never a wrong answer.
func (c *Coordinator) recordApprovedHeadReconstruction(
	ctx stdctx.Context, run domain.WorkflowRun, rec approvedHeadReconstruction, workCP domain.WorkflowCheckpoint,
) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return
	}
	next := fmt.Sprintf(
		"approved_head_reconstructed: the commit the approved target %s was read at could not be found in the last %d commits of %s",
		shortFingerprint(rec.Fingerprint), rec.Searched, rec.WorktreePath)
	if rec.HeadSHA != "" {
		next = fmt.Sprintf(
			"approved_head_reconstructed: the approved target %s is the clean-tree state of commit %s, recovered by recomputing the fingerprint over this branch's history",
			shortFingerprint(rec.Fingerprint), shortFingerprint(rec.HeadSHA))
	}
	var stepID *string
	if rec.ReviewStepID != "" {
		id := rec.ReviewStepID
		stepID = &id
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: stepID,
		ProjectID:      run.ProjectID,
		SessionID:      workCP.SessionID,
		Branch:         workCP.Branch,
		WorktreePath:   rec.WorktreePath,
		// HeadSHA + FingerprintAfter together are what makes a SUCCESSFUL
		// reconstruction resolvable by rule 3 of approvedHeadSHA like any other
		// observation, so nothing downstream needs to know this row is special.
		// A failed search writes neither, and therefore teaches rule 3 nothing.
		HeadSHA:          rec.HeadSHA,
		FingerprintAfter: fingerprintIf(rec.HeadSHA != "", rec.Fingerprint),
		NextAction:       next,
		DurablePhase:     approvedHeadReconstructionPhase,
		PayloadVersion:   "v1",
		RetryState:       string(payload),
		CreatedAt:        c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording an approved-head reconstruction failed", "run", run.ID, "err", err)
	}
}

func fingerprintIf(cond bool, fingerprint string) string {
	if cond {
		return fingerprint
	}
	return ""
}

// ---- the operator recovery ---------------------------------------------------

const (
	// operatorProvenanceRecoveryPhase records the human authorization itself,
	// separately from the fresh-review request it produces, so the ledger says
	// WHO decided as well as WHAT was decided.
	operatorProvenanceRecoveryPhase = "verify_operator_provenance_recovery"
	// maxOperatorProvenanceRecoveries bounds it. A person holding the button is
	// still a loop if the button can be pressed forever, and two attempts is
	// already one more than the situation has ever needed: if discarding the
	// unlocatable approval and re-reviewing the live workspace did not resolve
	// the run twice, a third identical attempt is not what it is missing.
	maxOperatorProvenanceRecoveries = 2
)

// RecoverUnprovableApprovedHead is the explicit operator recovery for a run
// parked because AO cannot name the commit its approved review target was read
// at, and cannot reconstruct it either.
//
// It is HUMAN-ONLY by construction. Nothing calls it: not reconcileRun, not
// ContinueRun, no wake reason and not the autonomous heartbeat. That is
// deliberate and load-bearing — an automatic caller would turn "AO cannot
// locate the approval" into an unattended loop of re-reviews spending provider
// budget with nobody watching, which is exactly the failure the CP7 reopen
// contract warns about one layer up.
//
// What it does NOT do is the point:
//
//   - It does not invent, attest or infer an approved commit. The unprovable
//     baseline stays unprovable, and it is discarded rather than guessed.
//   - It does not verify the drifted tree against the old approval. Verifying
//     code no reviewer read remains forbidden, here as everywhere.
//   - It does not skip the reviewer, raise any budget, or touch the reviewer
//     lifecycle. It converts a permanent dead end into ONE more ordinary review
//     cycle, served by the same dispatch branch as every other fresh review.
//
// It refuses, with a reason, on everything it cannot establish: a run that is
// not parked, parked on some other stop, whose failure was not a workspace
// change, whose approved head turns out to be provable after all (in which case
// the ordinary paths can handle it and no human decision is needed), or whose
// own recovery budget is spent.
func (c *Coordinator) RecoverUnprovableApprovedHead(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State.Terminal() {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, run.State)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is not stopped, so there is nothing to recover", ErrInvalid, runID)
	}
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	var workStep, reviewStep, verifyStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		case domain.WorkflowStepVerify:
			verifyStep = &steps[i]
		}
	}
	if workStep == nil || reviewStep == nil || verifyStep == nil {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q has no work/review/verify step to recover", ErrInvalid, runID)
	}

	// The stop must be one this recovery is for. Every other human-owned stop
	// is left exactly where it is: a person pressing the wrong button must not
	// silently discard an approval that was never the problem.
	reason, _, hasReason := c.stopReason(ctx, run)
	if !hasReason || (reason != ReasonVerifyApprovedHeadUnprovable &&
		reason != ReasonVerifyWorkspaceUnattributable && reason != ReasonVerifyUnrepairable) {
		return RunDetail{}, fmt.Errorf(
			"%w: workflow run %q is stopped on %q, which is not an unprovable review provenance", ErrInvalid, runID, reason)
	}
	result, hasResult, err := c.latestVerifyResult(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !hasResult || result.Passed || result.ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged {
		return RunDetail{}, fmt.Errorf(
			"%w: workflow run %q did not stop on a workspace change, so its review provenance is not what is blocking it", ErrInvalid, runID)
	}

	generation := c.operatorProvenanceRecoveries(ctx, runID) + 1
	if generation > maxOperatorProvenanceRecoveries {
		return RunDetail{}, fmt.Errorf(
			"%w: this run's review provenance has already been recovered %d times by hand and AO will not do it again — inspect the worktree, or cancel and re-run the task",
			ErrInvalid, maxOperatorProvenanceRecoveries)
	}

	workCP, hasWorkCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return RunDetail{}, err
	}
	if !hasWorkCP || workCP.WorktreePath == "" || workCP.SessionID == nil || *workCP.SessionID == "" || c.workspaceFacts == nil {
		return RunDetail{}, fmt.Errorf(
			"%w: AO holds no durable record of this run's worktree, so it cannot re-review what is in it", ErrInvalid)
	}

	// The one condition that would make this decision unnecessary: if the
	// approved head IS provable (a later pin, or a reconstruction that now
	// succeeds), the ordinary automatic paths own this run and a human must not
	// discard an approval AO can still locate.
	if head := c.approvedHeadSHA(ctx, runID, reviewStep.ID, result.ReviewedFingerprint, workCP); head != "" {
		return RunDetail{}, fmt.Errorf(
			"%w: AO can prove this run's approved target %s was read at commit %s, so its provenance needs no human recovery — continue the run instead",
			ErrInvalid, shortFingerprint(result.ReviewedFingerprint), shortFingerprint(head))
	}

	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(*workCP.SessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return RunDetail{}, fmt.Errorf("%w: this run's worktree could not be read: %s", ErrInvalid, err.Error())
	}
	// Nothing of AO's may be moving while this is decided — the same settle
	// proof every other attribution in this package makes. A tree an agent is
	// still writing to is not a tree anyone can send to a reviewer.
	if refusal, settled := c.workspaceIsSettled(ctx, run, steps, workCP); !settled {
		return RunDetail{}, fmt.Errorf("%w: %s", ErrInvalid, refusal)
	}
	current := WorkspaceFingerprint(obs)

	rec := WorkspaceProvenanceRecord{
		// The attribution is NOT changed by a human decision. AO still cannot
		// say whose change this is, and the record keeps saying so.
		Class:               ProvenanceUnknown,
		Generation:          c.provenanceFreshReviewGenerations(ctx, runID) + 1,
		ApprovedFingerprint: result.ReviewedFingerprint,
		ObservedFingerprint: current,
		HeadSHA:             obs.HeadSHA,
		Branch:              workCP.Branch,
		WorktreePath:        workCP.WorktreePath,
		WorkflowRunID:       runID,
		ReviewStepID:        reviewStep.ID,
		TargetKey:           result.TargetKey,
		ChangedFiles:        changedFilePaths(obs),
		ObservedAt:          c.clock(),
		Rationale: fmt.Sprintf(
			"AO holds no durable record of the commit the approved target %s was read at and could not reconstruct one from this branch's history; "+
				"a person authorized recovery %d of %d, which DISCARDS that unlocatable approval and asks for one fresh review of the workspace as it is now (%s). "+
				"No approved commit was inferred and nothing is verified against the old approval",
			shortFingerprint(result.ReviewedFingerprint), generation, maxOperatorProvenanceRecoveries, shortFingerprint(current)),
	}
	if run.PlannedTaskID != nil {
		rec.PlannedTaskID = *run.PlannedTaskID
	}
	if workCP.SessionID != nil {
		rec.SessionID = strings.TrimSpace(*workCP.SessionID)
	}
	if reviewStep.ReviewRunID != nil {
		rec.PriorReviewRunID = *reviewStep.ReviewRunID
	}

	// The authorization is durable BEFORE anything moves, so a daemon that dies
	// mid-transition resumes a decision that was already taken rather than
	// losing it or taking it twice.
	if err := c.recordOperatorProvenanceRecovery(ctx, run, *verifyStep, rec, generation); err != nil {
		return RunDetail{}, err
	}
	if err := c.recordWorkspaceProvenance(ctx, run, verifyStep.ID, rec); err != nil {
		return RunDetail{}, err
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return RunDetail{}, err
	}
	stepID := verifyStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     runID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		Branch:            rec.Branch,
		WorktreePath:      rec.WorktreePath,
		HeadSHA:           rec.HeadSHA,
		RetryState:        string(payload),
		FingerprintBefore: rec.ApprovedFingerprint,
		FingerprintAfter:  rec.ObservedFingerprint,
		NextAction: fmt.Sprintf(
			"verify_provenance_fresh_review: a person authorized discarding an approval AO could not locate a commit for; one fresh independent review of %s is due before anything is verified",
			shortFingerprint(rec.ObservedFingerprint)),
		DurablePhase:   provenanceFreshReviewPhase,
		PayloadVersion: verifyResultVersion,
		CreatedAt:      c.clock(),
	}); err != nil {
		return RunDetail{}, err
	}

	// The identical, idempotent reopen every fresh-review mechanism in this
	// package ends on: review out of `completed`, verify out of `failed`, run
	// out of `needs_attention`, resting at `waiting` on a reviewer.
	_, reopened, err := c.applyFreshReviewReopen(ctx, run, *reviewStep, *verifyStep, rec.freshReviewRecord())
	if err != nil {
		return RunDetail{}, err
	}
	if c.log != nil {
		c.log.Warn("workflow: a person recovered a run whose approved review provenance AO could not prove; the unlocatable approval was discarded and one fresh review requested",
			"run", runID, "generation", generation, "reopened", reopened,
			"approved", shortFingerprint(rec.ApprovedFingerprint), "observed", shortFingerprint(rec.ObservedFingerprint))
	}
	return c.GetRun(ctx, runID)
}

// operatorProvenanceRecoveries counts this run's durable operator recoveries —
// the bound that keeps a human-held button from being an unbounded loop.
func (c *Coordinator) operatorProvenanceRecoveries(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Unreadable budget is a spent budget. Conservative in the direction
		// that stops, exactly as plannerRetryCount is.
		return maxOperatorProvenanceRecoveries
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == operatorProvenanceRecoveryPhase {
			n++
		}
	}
	return n
}

func (c *Coordinator) recordOperatorProvenanceRecovery(
	ctx stdctx.Context, run domain.WorkflowRun, verifyStep domain.WorkflowStep, rec WorkspaceProvenanceRecord, generation int,
) error {
	stepID := verifyStep.ID
	state, err := json.Marshal(struct {
		Generation          int       `json:"generation"`
		Max                 int       `json:"max"`
		ApprovedFingerprint string    `json:"approvedFingerprint,omitempty"`
		ObservedFingerprint string    `json:"observedFingerprint,omitempty"`
		HeadSHA             string    `json:"headSha,omitempty"`
		WorktreePath        string    `json:"worktreePath,omitempty"`
		AuthorizedAt        time.Time `json:"authorizedAt"`
	}{
		Generation: generation, Max: maxOperatorProvenanceRecoveries,
		ApprovedFingerprint: rec.ApprovedFingerprint, ObservedFingerprint: rec.ObservedFingerprint,
		HeadSHA: rec.HeadSHA, WorktreePath: rec.WorktreePath, AuthorizedAt: c.clock(),
	})
	if err != nil {
		return err
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		Branch:         rec.Branch,
		WorktreePath:   rec.WorktreePath,
		HeadSHA:        rec.HeadSHA,
		NextAction: fmt.Sprintf(
			"verify_operator_provenance_recovery: a person authorized recovery %d of %d for an approved target (%s) whose commit AO could not prove or reconstruct; the approval is discarded, not attested",
			generation, maxOperatorProvenanceRecoveries, shortFingerprint(rec.ApprovedFingerprint)),
		DurablePhase:   operatorProvenanceRecoveryPhase,
		PayloadVersion: "v1",
		RetryState:     string(state),
		CreatedAt:      c.clock(),
	})
	return err
}
