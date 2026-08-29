package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// workspace_provenance.go — who changed this worktree, and does AO know it.
//
// The incident (wf-cd5bad10, worker agent-orchestrator-35): the worker
// completed two legitimate reviewer-requested fix cycles, produced five real
// files, and passed every check a person later ran by hand. AO stopped anyway:
//
//	verify_result   passed=false   verify_workspace_changed
//	run             needs_attention  verify_unrepairable
//
// because verification compared the tree against the fingerprint an EARLIER
// review had approved, found them different, and had no way to say WHY they
// were different. "The fingerprint moved" was the only fact it held, and that
// fact is equally true of the authorized fix worker's own output and of a
// stranger editing the directory.
//
// The remedy is not to relax the comparison. Verifying code no reviewer read
// remains forbidden, and nothing here changes that. The remedy is to record the
// PROVENANCE of every workflow mutation as it happens, so that when the
// fingerprints disagree AO can answer the second question — whose change is
// this? — and then do the right thing for the answer:
//
//	AUTHORIZED_WORK / AUTHORIZED_FIX -> get a FRESH review of what is actually
//	                                    there, then verify exactly what that
//	                                    review approved
//	everything else                  -> stop, exactly as before
//
// Note what the authorized branch does NOT do. It does not verify the drifted
// tree against the old approval. It does not skip the reviewer. It does not
// raise any budget. It converts an unexplained dead end into one more review
// cycle — the same review cycle a reviewer-requested fix would have got if AO
// had noticed the fix landed.

// WorkspaceProvenanceClass names whose change a workspace difference is. It is
// a closed vocabulary because each value carries a different consequence, and a
// value AO cannot prove is UNKNOWN rather than the most convenient neighbour.
type WorkspaceProvenanceClass string

const (
	// ProvenanceAuthorizedWork means this run's own work step produced it, in this
	// run's own worktree, on this run's own branch.
	ProvenanceAuthorizedWork WorkspaceProvenanceClass = "AUTHORIZED_WORK"
	// ProvenanceAuthorizedFix means this run's own fix step produced it, under a
	// reviewer-requested or verify-driven fix cycle AO itself dispatched.
	ProvenanceAuthorizedFix WorkspaceProvenanceClass = "AUTHORIZED_FIX"
	// ProvenancePreexisting means the difference was already there when this task was
	// dispatched. Not this task's work, and not new either.
	ProvenancePreexisting WorkspaceProvenanceClass = "PREEXISTING"
	// ProvenanceOtherAOTask means another AO task owns this worktree or this branch.
	// AO knows exactly who, and it is not this run.
	ProvenanceOtherAOTask WorkspaceProvenanceClass = "OTHER_AO_TASK"
	// ProvenanceExternal means the worktree or the branch is not the one this run was
	// authorized to work in at all.
	ProvenanceExternal WorkspaceProvenanceClass = "EXTERNAL"
	// ProvenanceConflicting means the history AO certified is no longer reachable —
	// an amend, a reset or a rebase dropped it. The reviewed work is not on the
	// branch, and only a person can say what that means.
	ProvenanceConflicting WorkspaceProvenanceClass = "CONFLICTING"
	// ProvenanceUnknown means AO cannot attribute the difference. The default, and
	// the honest answer whenever any required fact could not be read.
	ProvenanceUnknown WorkspaceProvenanceClass = "UNKNOWN"
)

// Authorized reports whether this class names a change AO itself caused through
// its own dispatched agents. It is the ONLY class family that may lead to a
// fresh review instead of a stop, and it is deliberately a method rather than a
// scattered `== ||` so there is one definition of "authorized" in AO.
func (c WorkspaceProvenanceClass) Authorized() bool {
	return c == ProvenanceAuthorizedWork || c == ProvenanceAuthorizedFix
}

const (
	// workspaceProvenancePhase is the durable record written around every
	// workflow mutation boundary AO observes, and again at every verification
	// that finds a difference. It is append-only evidence, never a decision.
	workspaceProvenancePhase = "workspace_provenance"
	// provenanceFreshReviewPhase is the decision: the difference was attributed
	// to this task's own authorized agent, and ONE fresh review of what is
	// actually in the worktree has been authorized. Written before anything is
	// reopened, so a daemon that dies mid-transition resumes the decision.
	provenanceFreshReviewPhase = "verify_provenance_fresh_review"
	// freshReviewPurposeProvenance is this mechanism's dispatch identity, so its
	// generations never collide with a verify recovery's, an integration
	// replay's, a branch advance's or an amendment's.
	freshReviewPurposeProvenance = "provenance"
	// maxProvenanceFreshReviews bounds how many times ONE run may answer a
	// workspace change this way. Three: a task can legitimately go through
	// several fix cycles whose last delivery lands after the review that
	// approved the previous one, but a worktree that keeps moving faster than a
	// reviewer can read it is a scheduling problem a fourth silent re-review
	// does not fix.
	maxProvenanceFreshReviews = 3
)

// WorkspaceProvenanceRecord is the durable evidence. Every field is something
// AO observed or holds a durable record of; nothing here is the agent's word
// for anything.
type WorkspaceProvenanceRecord struct {
	Class WorkspaceProvenanceClass `json:"class"`
	// Generation counts this run's provenance-authorized fresh reviews, from 1.
	Generation int `json:"generation,omitempty"`

	// The two fingerprints the whole question is about.
	ApprovedFingerprint string `json:"approvedFingerprint,omitempty"`
	ObservedFingerprint string `json:"observedFingerprint,omitempty"`

	// Workspace identity at the moment of observation.
	HeadSHA         string `json:"headSha,omitempty"`
	ApprovedHeadSHA string `json:"approvedHeadSha,omitempty"`
	Branch          string `json:"branch,omitempty"`
	WorktreePath    string `json:"worktreePath,omitempty"`

	// ChangedFiles is the observed working-tree change set, bounded. It is what
	// makes a provenance record readable by a person rather than a pair of
	// hashes.
	ChangedFiles []string `json:"changedFiles,omitempty"`

	// Ownership: which workflow, which task, which worker.
	WorkflowRunID string `json:"workflowRunId,omitempty"`
	PlannedTaskID string `json:"plannedTaskId,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	Harness       string `json:"harness,omitempty"`

	// MutationPhase names the workflow phase AO believes produced the change
	// ("work", "fix", "verify", "external"), and MutationStepID the step.
	MutationPhase  string `json:"mutationPhase,omitempty"`
	MutationStepID string `json:"mutationStepId,omitempty"`

	// ExpectedWriteSet is the task's declared/observed write set when AO has
	// one, so a difference can be checked against what this task was ever
	// supposed to touch. Empty when the run has none — an absent write set is
	// never treated as an empty one.
	ExpectedWriteSet []string `json:"expectedWriteSet,omitempty"`
	// OutsideWriteSet lists observed changes that fall outside it. Non-empty is
	// not by itself a refusal (a write set is an estimate, not a contract), but
	// it is recorded because it is exactly what a person needs to see.
	OutsideWriteSet []string `json:"outsideWriteSet,omitempty"`

	// ReviewStepID / PriorReviewRunID make the fresh-review request self-closing
	// the same way every other fresh-review request is.
	ReviewStepID     string `json:"reviewStepId,omitempty"`
	PriorReviewRunID string `json:"priorReviewRunId,omitempty"`
	TargetKey        string `json:"targetKey,omitempty"`

	// ApprovedHeadUnprovable marks the ONE missing fact that makes the whole
	// classification impossible rather than merely inconclusive: AO cannot name
	// the commit the approval was given for. Everything downstream of step 2 is
	// reasoning about uncommitted work, and that reasoning is only sound once
	// "the history did not move" has been established — which needs this
	// commit. It is a separate flag rather than a class because the class is
	// still honestly UNKNOWN; what this adds is that there is a specific,
	// bounded, human recovery for it. See RecoverUnprovableApprovedHead.
	ApprovedHeadUnprovable bool `json:"approvedHeadUnprovable,omitempty"`

	// Rationale states, in words, exactly what AO proved. It is never a guess:
	// an unproven attribution is UNKNOWN with a rationale saying which fact was
	// missing.
	Rationale  string    `json:"rationale,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// freshReviewRecord renders the decision in the vocabulary dispatchReviewStep
// already speaks, so a provenance-authorized re-review is served by the SAME
// dispatch branch as every other one rather than by a fifth of its own.
// reviewStep and priorReviewRun satisfy freshReviewLedgerEntry.
func (r WorkspaceProvenanceRecord) reviewStep() string     { return r.ReviewStepID }
func (r WorkspaceProvenanceRecord) priorReviewRun() string { return r.PriorReviewRunID }

func (r WorkspaceProvenanceRecord) freshReviewRecord() VerifyFreshReviewRecord {
	return VerifyFreshReviewRecord{
		Purpose:             freshReviewPurposeProvenance,
		Generation:          r.Generation,
		TargetKey:           r.TargetKey,
		ApprovedFingerprint: r.ApprovedFingerprint,
		CurrentFingerprint:  r.ObservedFingerprint,
		HeadSHA:             r.HeadSHA,
		Branch:              r.Branch,
		WorktreePath:        r.WorktreePath,
		ReviewStepID:        r.ReviewStepID,
		PriorReviewRunID:    r.PriorReviewRunID,
	}
}

// classifyWorkspaceDrift is the whole attribution predicate, in one place, and
// its default is UNKNOWN.
//
// It never mutates anything and performs only reads: the run's own append-only
// ledger, its steps, and — for the ancestry question alone — real Git.
func (c *Coordinator) classifyWorkspaceDrift(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	steps []domain.WorkflowStep,
	reviewStep domain.WorkflowStep,
	workCP domain.WorkflowCheckpoint,
	obs ports.WorkspaceObservation,
	reviewed, observedFingerprint, targetKey string,
) WorkspaceProvenanceRecord {
	rec := WorkspaceProvenanceRecord{
		Class:               ProvenanceUnknown,
		ApprovedFingerprint: reviewed,
		ObservedFingerprint: observedFingerprint,
		HeadSHA:             obs.HeadSHA,
		Branch:              workCP.Branch,
		WorktreePath:        workCP.WorktreePath,
		WorkflowRunID:       run.ID,
		ReviewStepID:        reviewStep.ID,
		TargetKey:           targetKey,
		ObservedAt:          c.clock(),
		ChangedFiles:        changedFilePaths(obs),
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
	rec.ExpectedWriteSet, rec.OutsideWriteSet = c.writeSetComparison(ctx, run, rec.ChangedFiles)

	// 1. Is this even AO's own authorized workspace? Path and branch first: a
	//    difference in a directory this run was never authorized to write is not
	//    an attribution question, it is a different tree.
	if workCP.WorktreePath == "" {
		rec.Rationale = "AO has no durable record of the worktree this run was authorized to work in"
		return rec
	}
	if obs.Path != "" && obs.Path != workCP.WorktreePath {
		rec.Class = ProvenanceExternal
		rec.MutationPhase = "external"
		rec.Rationale = fmt.Sprintf("the verified worktree %q is not this run's own (%q)", obs.Path, workCP.WorktreePath)
		return rec
	}
	if workCP.Branch != "" && obs.Branch != "" && obs.Branch != workCP.Branch {
		// A branch AO knows belongs to another AO task is a strictly better
		// answer than "external", because the remedy differs: one is a person's
		// edit, the other is two tasks contending for one tree.
		rec.Class = ProvenanceExternal
		if c.branchBelongsToAnotherTask(ctx, run, obs.Branch) {
			rec.Class = ProvenanceOtherAOTask
		}
		rec.MutationPhase = "external"
		rec.Rationale = fmt.Sprintf("the worktree is on branch %q, not the branch %q this run was authorized to work on", obs.Branch, workCP.Branch)
		return rec
	}

	// 2. Did the commit history move? The approvals AO holds are for a commit,
	//    and a moved HEAD is a different question from uncommitted drift.
	approvedHead := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, reviewed, workCP)
	rec.ApprovedHeadSHA = approvedHead
	if approvedHead == "" && obs.HeadSHA != "" {
		// The worktree HAS a commit identity and AO cannot name the one the
		// approval was given for. Everything below this point is reasoning about
		// UNCOMMITTED work, and it is only sound once "the history did not move"
		// has been established. It has not been, so this is UNKNOWN — not a
		// silent promotion to the uncommitted-drift branch.
		// approvedHeadSHA has already tried every durable row AND the
		// reconstruction from this branch's history (approved_head_recovery.go),
		// so this is the genuinely unrecoverable case rather than a lookup that
		// was never attempted. It stays fail-closed — but it is named, so it can
		// be acted on instead of being an unexplained dead end.
		rec.ApprovedHeadUnprovable = true
		rec.Rationale = fmt.Sprintf(
			"AO holds no durable record of the commit the approved fingerprint %s was read at, and could not reconstruct it from the last %d commits of %s, "+
				"so it cannot tell a moved history from uncommitted work. AO will not verify against an approval it cannot locate; "+
				"a person may recover this run's review provenance, which discards that approval and asks for one fresh review of what is in the worktree now",
			shortFingerprint(reviewed), maxApprovedHeadSearchCommits, workCP.WorktreePath)
		return rec
	}
	if approvedHead != "" && obs.HeadSHA != "" && obs.HeadSHA != approvedHead {
		git := integration.NewExecGit("")
		contained, err := git.IsAncestor(ctx, workCP.WorktreePath, approvedHead, obs.HeadSHA)
		if err != nil {
			rec.Rationale = fmt.Sprintf("AO could not prove whether the approved commit %s is still on the branch: %s",
				shortFingerprint(approvedHead), err.Error())
			return rec
		}
		if !contained {
			rec.Class = ProvenanceConflicting
			rec.MutationPhase = "external"
			rec.Rationale = fmt.Sprintf(
				"the approved commit %s is no longer reachable from HEAD %s — the history was rewritten",
				shortFingerprint(approvedHead), shortFingerprint(obs.HeadSHA))
			return rec
		}
		// The branch grew on top of the approval. That is a real, separately
		// proven recovery (verify_branch_advanced.go) with its own six proofs,
		// and this file must not become a second, weaker route to it.
		rec.Rationale = fmt.Sprintf(
			"the branch advanced from the approved commit %s to %s; that is verify_branch_advanced.go's question, not an attribution of uncommitted work",
			shortFingerprint(approvedHead), shortFingerprint(obs.HeadSHA))
		return rec
	}

	// 3. HEAD did not move, so the difference is uncommitted work in AO's own
	//    worktree on AO's own branch. Now: which of AO's own agents wrote it?
	//
	//    The strongest possible answer first — AO itself OBSERVED the workspace
	//    at exactly this fingerprint, at a mutation boundary of one of its own
	//    steps. That is not an inference at all.
	if phase, stepID, ok := c.authorizedMutationForFingerprint(ctx, run.ID, observedFingerprint); ok {
		rec.Class = ProvenanceAuthorizedWork
		if phase == "fix" {
			rec.Class = ProvenanceAuthorizedFix
		}
		rec.MutationPhase, rec.MutationStepID = phase, stepID
		rec.Rationale = fmt.Sprintf(
			"AO itself observed this exact workspace fingerprint (%s) at the end of this run's own %s step, in its own worktree, at an unchanged HEAD",
			shortFingerprint(observedFingerprint), phase)
		return rec
	}

	// 4. No exact match. The fix worker can (and in wf-cd5bad10 did) keep
	//    working after the observation that closed its cycle, so the tree ends
	//    up at a fingerprint AO never recorded. That is still this run's own
	//    authorized agent, and AO can say so from durable facts: a fix cycle of
	//    THIS run was dispatched after the approval AO is holding, into THIS
	//    worktree, and the only session that owns this tree is this run's own.
	if fixStepID, dispatchedAt, ok := c.latestFixDispatch(ctx, run.ID, steps); ok {
		approvedAt, hasApproval := c.approvalObservedAt(ctx, run.ID, reviewStep.ID, reviewed)
		if !hasApproval || !dispatchedAt.Before(approvedAt) {
			rec.Class = ProvenanceAuthorizedFix
			rec.MutationPhase, rec.MutationStepID = "fix", fixStepID
			rec.Rationale = fmt.Sprintf(
				"this run's own fix cycle was dispatched at %s into %s at an unchanged HEAD, at or after the approval of %s — the uncommitted difference is that authorized fix worker's own output",
				dispatchedAt.UTC().Format(time.RFC3339), workCP.WorktreePath, shortFingerprint(reviewed))
			return rec
		}
	}

	// 4b. The APPROVAL ITSELF names this run's own fix step's output, at this
	//     same head.
	//
	//     Rule 4 asks whether a fix cycle was dispatched at or after the approval,
	//     because that is the shape wf-cd5bad10 had. wf-a21d98aa has the other
	//     one: fix cycle 2 delivered b0910a3d at 20:16:55, review cycle 3 read and
	//     APPROVED exactly that fingerprint, and the same worker kept writing to
	//     the same tree while the reviewer was reading it. The dispatch that
	//     produced the drift therefore predates the approval, and rule 4 cannot
	//     see it — so a run whose drift was entirely its own authorized fix worker
	//     stopped as unattributable.
	//
	//     What is provable here is narrower than "who wrote the new bytes", and it
	//     is enough: the state the approval was given for is a state AO ITSELF
	//     observed at the end of this run's own fix cycle, in this run's own
	//     worktree, and HEAD has not moved since. The only agent AO ever pointed
	//     at that tree at that head is that fix worker, and everything after it at
	//     the same head is the same cycle still landing. It is still code no
	//     reviewer has read yet, so the remedy is still a review — never a
	//     verification of it, and never a stop.
	if phase, stepID, ok := c.authorizedMutationForFingerprint(ctx, run.ID, reviewed); ok && phase == "fix" {
		rec.Class = ProvenanceAuthorizedFix
		rec.MutationPhase, rec.MutationStepID = "fix", stepID
		rec.Rationale = fmt.Sprintf(
			"the approved fingerprint %s is itself this run's own fix step's recorded output in %s, and HEAD has not moved since — the further uncommitted difference is that same authorized fix cycle still landing",
			shortFingerprint(reviewed), workCP.WorktreePath)
		return rec
	}

	// 5. Was the difference already there when this task was dispatched? A
	//    dispatch-time observation that already recorded a dirty tree means the
	//    drift is not this task's at all.
	if c.driftPredatesDispatch(ctx, run.ID, workCP) {
		rec.Class = ProvenancePreexisting
		rec.MutationPhase = "external"
		rec.Rationale = "the worktree was already carrying uncommitted changes when this task was dispatched"
		return rec
	}

	rec.Rationale = "AO holds no durable record attributing this uncommitted difference to any of its own authorized agents for this run"
	return rec
}

// authorizedMutationForFingerprint looks for a checkpoint of THIS run whose
// FingerprintAfter is exactly the observed fingerprint and whose phase is one of
// AO's own mutation boundaries — the work step's completion observation, or a
// fix step's delivery observation. Returns the phase name and the step.
//
// Only phases AO writes itself count. A fingerprint appearing anywhere else in
// the ledger (a review target, a verify result's "before") says only what the
// tree looked like, never who put it there.
func (c *Coordinator) authorizedMutationForFingerprint(ctx stdctx.Context, runID, fingerprint string) (string, string, bool) {
	if fingerprint == "" {
		return "", "", false
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return "", "", false
	}
	phase, stepID := "", ""
	var at time.Time
	for _, cp := range cps {
		if cp.FingerprintAfter != fingerprint {
			continue
		}
		var kind string
		switch {
		case strings.HasPrefix(cp.DurablePhase, "worker_observed_"):
			kind = "work"
		case strings.HasPrefix(cp.DurablePhase, "fix_observed_"):
			kind = "fix"
		default:
			continue
		}
		if cp.CreatedAt.Before(at) {
			continue
		}
		phase, at = kind, cp.CreatedAt
		if cp.WorkflowStepID != nil {
			stepID = *cp.WorkflowStepID
		}
	}
	return phase, stepID, phase != ""
}

// latestFixDispatch returns this run's newest fix dispatch and when it happened.
func (c *Coordinator) latestFixDispatch(ctx stdctx.Context, runID string, steps []domain.WorkflowStep) (string, time.Time, bool) {
	fixStepID := ""
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepFix {
			fixStepID = s.ID
		}
	}
	if fixStepID == "" {
		return "", time.Time{}, false
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return "", time.Time{}, false
	}
	var at time.Time
	for _, cp := range cps {
		if cp.DurablePhase != fixDispatchedPhase {
			continue
		}
		if cp.WorkflowStepID != nil && *cp.WorkflowStepID != fixStepID {
			continue
		}
		if cp.CreatedAt.After(at) {
			at = cp.CreatedAt
		}
	}
	if at.IsZero() {
		return "", time.Time{}, false
	}
	return fixStepID, at, true
}

// approvalObservedAt returns when AO recorded the review target the approval it
// is holding was given for. Used to order "the approval" against "the fix
// dispatch" without trusting either side's own timestamps.
func (c *Coordinator) approvalObservedAt(ctx stdctx.Context, runID, reviewStepID, reviewedFingerprint string) (time.Time, bool) {
	if reviewedFingerprint == "" {
		return time.Time{}, false
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return time.Time{}, false
	}
	var at time.Time
	for _, cp := range cps {
		if cp.DurablePhase != reviewTargetDurablePhase || cp.FingerprintAfter != reviewedFingerprint {
			continue
		}
		if cp.WorkflowStepID != nil && *cp.WorkflowStepID != reviewStepID {
			continue
		}
		if cp.CreatedAt.After(at) {
			at = cp.CreatedAt
		}
	}
	return at, !at.IsZero()
}

// driftPredatesDispatch reports whether the work step's own dispatch checkpoint
// already described a dirty tree.
func (c *Coordinator) driftPredatesDispatch(ctx stdctx.Context, runID string, workCP domain.WorkflowCheckpoint) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != workspaceProvenancePhase {
			continue
		}
		var rec WorkspaceProvenanceRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		if rec.MutationPhase == "dispatch" && rec.WorktreePath == workCP.WorktreePath && len(rec.ChangedFiles) > 0 {
			return true
		}
	}
	return false
}

// branchBelongsToAnotherTask reports whether AO's own worktree records name a
// DIFFERENT task as the owner of this branch.
func (c *Coordinator) branchBelongsToAnotherTask(ctx stdctx.Context, run domain.WorkflowRun, branch string) bool {
	if c.taskWorktreeRecords == nil || run.ParentWorkflowID == nil || branch == "" {
		return false
	}
	records, err := c.taskWorktreeRecords.ListTaskWorktreesByRun(ctx, *run.ParentWorkflowID)
	if err != nil {
		return false
	}
	mine := ""
	if run.PlannedTaskID != nil {
		mine = *run.PlannedTaskID
	}
	for _, r := range records {
		if r.Branch == branch && r.TaskID != mine {
			return true
		}
	}
	return false
}

// expectedWriteSetFor returns the write set AO holds for this run's planned
// task, when there is one. An absent scope returns nil and is never treated as
// an empty write set — "AO does not know what this task was supposed to touch"
// and "this task was supposed to touch nothing" are different facts.
func (c *Coordinator) expectedWriteSetFor(ctx stdctx.Context, run domain.WorkflowRun) []string {
	if c.planStore == nil || run.PlannedTaskID == nil || run.ParentWorkflowID == nil {
		return nil
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, *run.ParentWorkflowID)
	if err != nil {
		return nil
	}
	for _, t := range tasks {
		if t.ID != *run.PlannedTaskID {
			continue
		}
		scope, serr := UnmarshalTaskScope(t.ScopeJSON)
		if serr != nil {
			return nil
		}
		return scope.WritePaths
	}
	return nil
}

// writeSetComparison returns the task's expected write set (when AO holds one)
// and the observed paths falling outside it.
func (c *Coordinator) writeSetComparison(ctx stdctx.Context, run domain.WorkflowRun, changed []string) ([]string, []string) {
	expected := c.expectedWriteSetFor(ctx, run)
	if len(expected) == 0 {
		return nil, nil
	}
	inSet := make(map[string]bool, len(expected))
	for _, p := range expected {
		inSet[p] = true
	}
	var outside []string
	for _, p := range changed {
		if inSet[p] {
			continue
		}
		covered := false
		for _, e := range expected {
			if strings.HasSuffix(e, "/") && strings.HasPrefix(p, e) {
				covered = true
				break
			}
		}
		if !covered {
			outside = append(outside, p)
		}
	}
	return expected, outside
}

// changedFilePaths renders a bounded, sorted, deduplicated list of the observed
// change set.
func changedFilePaths(obs ports.WorkspaceObservation) []string {
	if len(obs.Changes) == 0 {
		return nil
	}
	const maxProvenanceChangedFiles = 200
	seen := make(map[string]bool, len(obs.Changes))
	paths := make([]string, 0, len(obs.Changes))
	for _, ch := range obs.Changes {
		if ch.Path == "" || seen[ch.Path] {
			continue
		}
		seen[ch.Path] = true
		paths = append(paths, ch.Path)
	}
	sort.Strings(paths)
	if len(paths) > maxProvenanceChangedFiles {
		paths = paths[:maxProvenanceChangedFiles]
	}
	return paths
}

// recordWorkspaceProvenance appends one provenance record to the run's ledger.
//
// Best-effort like every other observer: a provenance row AO failed to write
// must never change what the run's own state says happened. What it must never
// do is be SKIPPED silently on a decision path — every caller that acts on a
// classification writes the record first, so the decision and its evidence land
// together or not at all.
func (c *Coordinator) recordWorkspaceProvenance(ctx stdctx.Context, run domain.WorkflowRun, stepID string, rec WorkspaceProvenanceRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	var step *string
	if stepID != "" {
		v := stepID
		step = &v
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    step,
		ProjectID:         run.ProjectID,
		Branch:            rec.Branch,
		WorktreePath:      rec.WorktreePath,
		HeadSHA:           rec.HeadSHA,
		RetryState:        string(payload),
		FingerprintBefore: rec.ApprovedFingerprint,
		FingerprintAfter:  rec.ObservedFingerprint,
		NextAction: fmt.Sprintf("workspace_provenance: %s — %s",
			rec.Class, rec.Rationale),
		DurablePhase:   workspaceProvenancePhase,
		PayloadVersion: "v1",
		CreatedAt:      c.clock(),
	})
	return err
}

// provenanceFreshReviewGenerations counts the provenance-authorized fresh
// reviews already recorded for a run, which is what maxProvenanceFreshReviews
// is applied to.
func (c *Coordinator) provenanceFreshReviewGenerations(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return 0
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == provenanceFreshReviewPhase {
			n++
		}
	}
	return n
}

// requestProvenanceFreshReview is the transition: a verification that found an
// ATTRIBUTABLE difference gets one fresh, independent review of what is actually
// in the worktree, and then verifies exactly what that review approved.
//
// Every write is idempotent against re-entry from any poll, any Continue and any
// restart, using the same shapes every other fresh-review request uses: the
// decision checkpoint is written once per generation, the superseded
// VerifyResult and attempt outcome close this attempt exactly once, and both
// step transitions are compare-and-swaps.
func (c *Coordinator) requestProvenanceFreshReview(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep, verifyStep domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	result VerifyResult,
	rec WorkspaceProvenanceRecord,
) (domain.WorkflowRun, domain.WorkflowStep, error) {
	// The evidence, then the decision, then anything that moves.
	if err := c.recordWorkspaceProvenance(ctx, run, verifyStep.ID, rec); err != nil {
		return run, verifyStep, err
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return run, verifyStep, err
	}
	stepID := verifyStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		Branch:            rec.Branch,
		WorktreePath:      rec.WorktreePath,
		HeadSHA:           rec.HeadSHA,
		RetryState:        string(payload),
		FingerprintBefore: rec.ApprovedFingerprint,
		FingerprintAfter:  rec.ObservedFingerprint,
		NextAction: fmt.Sprintf(
			"verify_provenance_fresh_review: the worktree is %s, not the approved %s, and AO attributed the difference to this task's own %s (%s of %d) — one fresh independent review of what is there now is due before anything is verified",
			shortFingerprint(rec.ObservedFingerprint), shortFingerprint(rec.ApprovedFingerprint),
			rec.Class, ordinalOf(rec.Generation), maxProvenanceFreshReviews),
		DurablePhase:   provenanceFreshReviewPhase,
		PayloadVersion: verifyResultVersion,
		CreatedAt:      c.clock(),
	}); err != nil {
		return run, verifyStep, err
	}

	// This attempt really did fail, and on the class it failed on. The flag says
	// the failure is a question AO went on to ask, not the answer to it.
	result.SupersededByFreshReview = true
	if err := c.persistVerifyResult(ctx, run, verifyStep, attempt, result, provenanceFreshReviewPhase); err != nil {
		return run, verifyStep, err
	}
	now := c.clock()
	if err := c.store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now, domain.WorkflowAttemptFailed, result.ErrorClass); err != nil {
		return run, verifyStep, err
	}
	if _, err := c.store.ReopenCompletedWorkflowStep(ctx, reviewStep.ID, now); err != nil {
		return run, verifyStep, err
	}
	// The verify step rests at `waiting`, never `failed`: a terminal verify step
	// would make the re-verification this transition exists to enable
	// structurally impossible.
	if verifyStep.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, verifyStep.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
			return run, verifyStep, err
		}
		verifyStep.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunRunning, domain.WorkflowRunWaiting, now); err != nil {
			return run, verifyStep, err
		}
		run.State = domain.WorkflowRunWaiting
	}
	if c.log != nil {
		c.log.Info("workflow: the workspace change was attributed to this task's own authorized agent; asking for one fresh review of what is there now",
			"run", run.ID, "class", rec.Class, "generation", rec.Generation,
			"approved", shortFingerprint(rec.ApprovedFingerprint), "observed", shortFingerprint(rec.ObservedFingerprint))
	}
	return run, verifyStep, nil
}

// pendingProvenanceFreshReview is the read dispatchReviewStep makes for a review
// step reopened by an attributable workspace change. Self-closing exactly like
// pendingBranchAdvancedFreshReview: the moment the step points at a review run
// other than the stale approval, the request is answered.
func (c *Coordinator) pendingProvenanceFreshReview(ctx stdctx.Context, runID, reviewStepID string) (VerifyFreshReviewRecord, bool) {
	return pendingFreshReviewFromPhase[WorkspaceProvenanceRecord](c, ctx, runID, reviewStepID, provenanceFreshReviewPhase)
}

// ordinalOf renders a small generation count for a human-readable line.
func ordinalOf(n int) string {
	return fmt.Sprintf("recovery %d", n)
}

// resumeProvenanceWorkspaceChange is the same transition entered from a run that
// is ALREADY parked on a workspace change — one decided and persisted before
// this mechanism existed (which is precisely wf-cd5bad10's state), or by a pass
// whose attribution did not hold at the time.
//
// Its only caller is ContinueRun, for the same reason resumeBranchAdvancedVerify's
// is: a terminal verify step is taken out of its terminal state here and nowhere
// else, and a 2s Board poll is not a person. The live path in maybeVerify needs
// no such licence because it acts on the verification running right now.
//
// The attribution is re-derived against the worktree as it stands NOW, never
// against the facts as they stood when the run was parked, and it refuses
// everything it cannot prove. Returns whether this call reopened anything.
func (c *Coordinator) resumeProvenanceWorkspaceChange(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, bool, error) {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run, false, nil
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
		return run, false, nil
	}
	if verifyStep.State != domain.WorkflowStepFailed || reviewStep.State != domain.WorkflowStepCompleted {
		return run, false, nil
	}
	// The stop must be one of the verification stops. Every other human-owned
	// stop is left exactly where it is.
	reason, _, ok := c.stopReason(ctx, run)
	// verify_approved_head_unprovable joins the two attribution stops here on
	// purpose. It names a run that parked because AO could not locate its
	// approval's commit -- and the classification below is re-derived from
	// scratch, against a daemon that can now RECONSTRUCT that commit from the
	// branch's own history (approved_head_recovery.go). A run parked on a fact
	// AO can now prove must be able to move on its own, without a person; the
	// operator recovery exists for the runs where the fact is still unprovable,
	// not for the ones a newer daemon can simply answer.
	if !ok || (!recoverableVerifyStopReasons[reason] &&
		reason != ReasonVerifyWorkspaceUnattributable &&
		reason != ReasonVerifyApprovedHeadUnprovable) {
		return run, false, nil
	}
	// And the failure must actually be a workspace change.
	result, hasResult, err := c.latestVerifyResult(ctx, run.ID)
	if err != nil {
		return run, false, err
	}
	if !hasResult || result.Passed || result.ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged {
		return run, false, nil
	}
	generation := c.provenanceFreshReviewGenerations(ctx, run.ID) + 1
	if generation > maxProvenanceFreshReviews {
		return run, false, nil
	}

	workCP, hasWorkCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return run, false, err
	}
	if !hasWorkCP || workCP.WorktreePath == "" || workCP.SessionID == nil || *workCP.SessionID == "" || c.workspaceFacts == nil {
		return run, false, nil
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(*workCP.SessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		// Unreadable is not "attributable". The run stays where it is.
		//nolint:nilerr // an unobservable workspace attributes nothing; retry next pass.
		return run, false, nil
	}
	current := WorkspaceFingerprint(obs)
	if current == result.ReviewedFingerprint {
		// The tree came back to the approved state on its own; there is nothing
		// to attribute, and the ordinary verify path can simply run again.
		return run, false, nil
	}
	// Nothing of AO's may be moving while this is decided. The same settle proof
	// verify_branch_advanced.go makes, for the same reason: an agent mid-turn
	// whose output is still landing is not a tree anyone can attribute.
	if refusal, settled := c.workspaceIsSettled(ctx, run, steps, workCP); !settled {
		if c.log != nil {
			c.log.Debug("workflow: a parked workspace change is not settled enough to attribute", "run", run.ID, "reason", refusal)
		}
		return run, false, nil
	}

	prov := c.classifyWorkspaceDrift(ctx, run, steps, *reviewStep, workCP, obs, result.ReviewedFingerprint, current, result.TargetKey)
	prov.Generation = generation
	if !prov.Class.Authorized() {
		// Record the attribution anyway: a refusal that says WHICH class it
		// found is the whole difference between this stop and the unexplained
		// one this mechanism exists to remove.
		if perr := c.recordWorkspaceProvenance(ctx, run, verifyStep.ID, prov); perr != nil && c.log != nil {
			c.log.Warn("workflow: recording workspace provenance failed", "run", run.ID, "err", perr)
		}
		return run, false, nil
	}

	if err := c.recordWorkspaceProvenance(ctx, run, verifyStep.ID, prov); err != nil {
		return run, false, err
	}
	payload, err := json.Marshal(prov)
	if err != nil {
		return run, false, err
	}
	stepID := verifyStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		Branch:            prov.Branch,
		WorktreePath:      prov.WorktreePath,
		HeadSHA:           prov.HeadSHA,
		RetryState:        string(payload),
		FingerprintBefore: prov.ApprovedFingerprint,
		FingerprintAfter:  prov.ObservedFingerprint,
		NextAction: fmt.Sprintf(
			"verify_provenance_fresh_review: a person reopened this stop and AO attributed the worktree difference to this task's own %s (%s of %d) — one fresh independent review of %s is due before anything is verified",
			prov.Class, ordinalOf(prov.Generation), maxProvenanceFreshReviews, shortFingerprint(prov.ObservedFingerprint)),
		DurablePhase:   provenanceFreshReviewPhase,
		PayloadVersion: verifyResultVersion,
		CreatedAt:      c.clock(),
	}); err != nil {
		return run, false, err
	}

	// applyFreshReviewReopen is the identical, idempotent state mutation the
	// verify-recovery and branch-advance paths use: review out of `completed`,
	// verify out of `failed`, run out of `needs_attention`, resting at `waiting`
	// on a reviewer. Sharing it keeps every fresh-review mechanism converging on
	// one resting state.
	run, reopened, err := c.applyFreshReviewReopen(ctx, run, *reviewStep, *verifyStep)
	if err != nil {
		return run, false, err
	}
	if reopened && c.log != nil {
		c.log.Info("workflow: reopened a parked workspace change whose difference is this task's own authorized work",
			"run", run.ID, "class", prov.Class, "generation", prov.Generation,
			"approved", shortFingerprint(prov.ApprovedFingerprint), "observed", shortFingerprint(prov.ObservedFingerprint))
	}
	return run, reopened, nil
}

// workspaceIsSettled is the "nothing of AO's is moving" proof, shared by every
// path that attributes a worktree difference. It is the same set of conditions
// verify_branch_advanced.go proves, extracted so the two cannot drift apart.
func (c *Coordinator) workspaceIsSettled(
	ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep, workCP domain.WorkflowCheckpoint,
) (string, bool) {
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepWork, domain.WorkflowStepFix:
			if s.State == domain.WorkflowStepRunning || s.State == domain.WorkflowStepReady {
				return fmt.Sprintf("this run's %s step is still %s, so the tree may still be changing", s.Kind, s.State), false
			}
			attempts, aerr := c.store.ListWorkflowAttempts(ctx, s.ID)
			if aerr != nil {
				return fmt.Sprintf("AO could not read this run's %s attempts to prove nothing is in flight", s.Kind), false
			}
			for _, a := range attempts {
				if a.FinishedAt == nil {
					return fmt.Sprintf("this run's %s step has an attempt that never finished, so the tree may still be changing", s.Kind), false
				}
			}
		}
	}
	if open, qerr := c.hasOpenQuestion(ctx, run.ID, nil); qerr != nil || open {
		return "this run has an unresolved question open, so nothing about its workspace is settled", false
	}
	sessionID := ""
	if workCP.SessionID != nil {
		sessionID = strings.TrimSpace(*workCP.SessionID)
	}
	if c.sessionFacts == nil || sessionID == "" {
		return "AO cannot tell whether this run's own worker is still writing to the tree", false
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil || !found {
		return "AO could not read this run's worker session to prove no agent is writing to the tree", false
	}
	if agentMayStillBeDelivering(sess, c.clock()) {
		return fmt.Sprintf("this run's agent was active within the last %s, so the tree may still be changing", humanFixSettleWindow), false
	}
	return "", true
}
