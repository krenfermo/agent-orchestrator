package workflow

import (
	stdctx "context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// verified_state.go — one canonical answer to "what did verify approve?".
//
// THE INCIDENT (gate-7, run wf-a5cb8fa8, step wfs-f78143). Three checkpoints,
// 108 milliseconds apart, on one verify step:
//
//	04:10:40.184  verify_result                    passed, fingerprint 3e1f79f0...
//	04:10:40.245  autonomous_local_commit_failed   "this run's worktree changed
//	                                                after its verification passed"
//	04:10:40.292  autonomous_local_commit          2a46a80f... on the task branch
//
// The run was parked for a human at .245 and committed the very same work at
// .292. Both passes resolved the SAME worktree path (every checkpoint that
// records one names greetings-svc-3), the same branch, and the same and only
// passing verify result, so neither the placement nor the verdict moved. The
// deliverable did not change either: the commit at .292 carries exactly the
// three files the task produced.
//
// What actually happened is that requireVerifiedTree compared two INDEPENDENT
// samples of `git status`, taken ~60ms apart, and treated any difference
// between them as proof that the verified work had been replaced. A worktree is
// not quiescent at that granularity — git's index carries a stat cache whose
// racily-clean entries are re-examined on the next status, and the provider's
// own session is still writing beside it — so two consecutive samples of one
// unchanged tree may legitimately disagree. The check was a point-in-time
// equality with a terminal consequence and no recorded evidence: it wrote
// neither fingerprint, which is why the incident could not be diagnosed from
// its own record.
//
// THE FIX, in three parts.
//
//  1. ONE canonical verified state. VerifiedWorkspaceState is what verify
//     records and what the commit boundary reads: not just the digest but the
//     ROOT it was computed over and the components that went into it. The
//     producer and the consumer can no longer derive "the same" fingerprint
//     from two independently-resolved worktrees, and a mismatch can now name
//     which component differed instead of only that something did.
//
//  2. A mismatch is not a person's problem. A tree that no longer matches its
//     verification means the verdict no longer describes the workspace — which
//     is a reason to VERIFY AGAIN, not a reason to stop and wait for a human.
//     The drift is recorded, the verification is invalidated, and the run
//     re-enters verify under a new attempt identity that actually re-runs the
//     checks. Bounded: after maxVerifiedStateDrifts the run does park, because
//     a tree that will not hold still across repeated verifications is a real
//     problem rather than a sampling artifact.
//
//  3. Exactly one winner. Recording the drift advances the verification target
//     key, so the succeeded attempt that was driving completeVerifiedRun can no
//     longer drive it. A run can therefore never both park and commit, and
//     never record a failed commit that a later pass of the same generation
//     contradicts.

// verifiedStateReverificationPhase is the durable record that a verified tree
// no longer matched its verdict and AO chose to verify again.
//
// Deliberately NOT a "..._failed" phase: nothing failed. The verification was
// invalidated, which is a lifecycle event, and the recovery surfaces read it as
// automatic action in flight rather than as an incident awaiting a person.
const verifiedStateReverificationPhase = "verified_state_reverification"

// maxVerifiedStateDrifts bounds part 2. Two re-verifications absorb the
// sampling artifacts this file exists for; a third drift means the worktree is
// genuinely unstable under AO's feet, and continuing to re-verify would be the
// retry loop this fix is specifically not.
const maxVerifiedStateDrifts = 2

// VerifiedWorkspaceState is the canonical representation of the filesystem
// state a verification approved.
//
// Fingerprint is the same digest WorkspaceFingerprint has always produced, so
// every existing reader keeps working. What is new is the CONTEXT: the root the
// digest was computed over, and the components behind it, which is what lets a
// consumer observe the same thing rather than something it believes is the same
// thing, and lets a mismatch be explained rather than merely reported.
type VerifiedWorkspaceState struct {
	Fingerprint  string `json:"fingerprint"`
	WorktreePath string `json:"worktreePath,omitempty"`
	Branch       string `json:"branch,omitempty"`
	HeadSHA      string `json:"headSHA,omitempty"`
	Dirty        bool   `json:"dirty"`
	Staged       bool   `json:"staged"`
	Untracked    bool   `json:"untracked"`
	// Changes is "path:status" per non-ephemeral change, sorted. It carries no
	// content hashes: this is the explanatory half, and the Fingerprint above
	// remains the authority on content.
	Changes []string `json:"changes,omitempty"`
}

// newVerifiedWorkspaceState captures an observation as a canonical state.
func newVerifiedWorkspaceState(obs ports.WorkspaceObservation) VerifiedWorkspaceState {
	state := VerifiedWorkspaceState{
		Fingerprint:  WorkspaceFingerprint(obs),
		WorktreePath: strings.TrimSpace(obs.Path),
		Branch:       strings.TrimSpace(obs.Branch),
		HeadSHA:      strings.TrimSpace(obs.HeadSHA),
		Dirty:        obs.Dirty,
		Staged:       obs.Staged,
		Untracked:    obs.Untracked,
	}
	for _, ch := range obs.Changes {
		if IsEphemeralArtifactPath(ch.Path) {
			continue
		}
		state.Changes = append(state.Changes, ch.Path+":"+ch.Status)
	}
	sort.Strings(state.Changes)
	return state
}

// differenceFrom names what moved between the state verify approved (the
// receiver) and one observed later.
//
// It exists because the incident this file fixes left a record that said only
// "the worktree changed", which is precisely the claim that turned out to be
// wrong. A drift that cannot say what differs cannot be believed.
func (s VerifiedWorkspaceState) differenceFrom(observed VerifiedWorkspaceState) string {
	var diffs []string
	add := func(field, was, now string) {
		if was != now {
			diffs = append(diffs, field+" "+strconv.Quote(was)+" -> "+strconv.Quote(now))
		}
	}
	add("head_sha", s.HeadSHA, observed.HeadSHA)
	add("dirty", strconv.FormatBool(s.Dirty), strconv.FormatBool(observed.Dirty))
	add("staged", strconv.FormatBool(s.Staged), strconv.FormatBool(observed.Staged))
	add("untracked", strconv.FormatBool(s.Untracked), strconv.FormatBool(observed.Untracked))
	add("worktree", s.WorktreePath, observed.WorktreePath)
	if strings.Join(s.Changes, ",") != strings.Join(observed.Changes, ",") {
		diffs = append(diffs, "changes ["+strings.Join(s.Changes, " ")+"] -> ["+strings.Join(observed.Changes, " ")+"]")
	}
	if len(diffs) == 0 {
		// Every named component agrees, so the difference is in file CONTENT
		// under an unchanged status — the one thing Changes deliberately does
		// not carry. Saying so is more useful than saying nothing.
		return "file content changed under unchanged git status"
	}
	return strings.Join(diffs, "; ")
}

// verifiedStateDriftError is a mismatch between a verification and the tree at
// the commit boundary.
//
// It is a distinct type rather than a message because its HANDLING is what
// changed: completeVerifiedRun must be able to tell "the verdict no longer
// describes the workspace" (verify again) from "AO could not capture the
// result" (a person's problem). Before this type the two were one error value
// and shared the second answer.
type verifiedStateDriftError struct {
	verified VerifiedWorkspaceState
	observed VerifiedWorkspaceState
}

func (e *verifiedStateDriftError) Error() string {
	return fmt.Sprintf(
		"this run's worktree no longer matches the state its verification approved (%s), so the verification is stale and AO will verify again rather than commit an unverified tree",
		e.verified.differenceFrom(e.observed))
}

// verifiedStateDrifts counts the re-verifications this run has already spent on
// a drifting tree. It is the bound on part 2, read from the durable record so a
// restart cannot reset it.
func (c *Coordinator) verifiedStateDrifts(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Unreadable: claim no drift. The run then keeps its existing attempt,
		// which is the conservative direction — it cannot manufacture a new
		// verification out of a failed read.
		return 0
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == verifiedStateReverificationPhase {
			n++
		}
	}
	return n
}

// verifyTargetAdvancedByVerifiedStateDrift is the third authorized reason a
// verification target may differ from the one an attempt recorded, alongside a
// verify-driven fix and an AO-asked fresh review: AO itself invalidated the
// verification because the tree no longer matched it.
//
// Same shape as verifyTargetAdvancedByFix deliberately — a target that advanced
// for a reason AO put on the record is not the drift that guard exists to catch.
func (c *Coordinator) verifyTargetAdvancedByVerifiedStateDrift(ctx stdctx.Context, runID string, attempt domain.WorkflowAttempt) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase == verifiedStateReverificationPhase && !cp.CreatedAt.Before(attempt.StartedAt) {
			return true
		}
	}
	return false
}

// recordVerifiedStateDrift persists the invalidation.
//
// NOT best-effort. This checkpoint is the authority for two separate things —
// the bound on re-verification, and the target-key advance that makes the stale
// attempt stop driving completion — so a drift AO cannot record is one it must
// not act on. Failing here leaves the run exactly where it was, which the next
// pass re-derives.
func (c *Coordinator) recordVerifiedStateDrift(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, drift *verifiedStateDriftError) error {
	stepID := step.ID
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		Branch:         drift.verified.Branch,
		WorktreePath:   drift.verified.WorktreePath,
		HeadSHA:        drift.observed.HeadSHA,
		// The diagnostic gap the incident exposed: both sides of the comparison
		// are now on the record, so a future drift can be explained from its own
		// checkpoint instead of reconstructed from the filesystem.
		FingerprintBefore: drift.verified.Fingerprint,
		FingerprintAfter:  drift.observed.Fingerprint,
		NextAction:        "reverification_required: " + drift.verified.differenceFrom(drift.observed),
		DurablePhase:      verifiedStateReverificationPhase,
		PayloadVersion:    "v1",
		RetryState:        "{}",
		CreatedAt:         c.clock(),
	})
	return err
}

// reverifyAfterVerifiedStateDrift is part 2 and part 3 of the fix: the run
// invalidates its own verification and re-enters verify, instead of stopping
// for a person.
//
// Nothing here presses on toward the commit. Recording the drift advances the
// verification target key (verify.go), which retires the succeeded attempt that
// was driving this completion — so the next pass opens a NEW attempt and
// actually re-runs the checks. That single record is also what makes the two
// outcomes mutually exclusive: a run that recorded a drift cannot go on to
// commit under the verdict it just invalidated, and a run that committed never
// reaches here.
func (c *Coordinator) reverifyAfterVerifiedStateDrift(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, drift *verifiedStateDriftError) (domain.WorkflowRun, domain.WorkflowStep, error) {
	if spent := c.verifiedStateDrifts(ctx, run.ID); spent >= maxVerifiedStateDrifts {
		// The bound. A tree that will not hold still across this many
		// verifications is not the sampling artifact this path exists to
		// absorb, and re-verifying again would be a retry loop rather than a
		// recovery. Park it, and say which it is.
		return c.failRunOnCommitError(ctx, run, step, fmt.Errorf(
			"this run's worktree changed again after %d re-verifications (%s), so AO cannot establish a verified state to commit",
			spent, drift.verified.differenceFrom(drift.observed)))
	}
	if err := c.recordVerifiedStateDrift(ctx, run, step, drift); err != nil {
		// Unrecorded means unauthorized: without the checkpoint the target key
		// does not advance, the stale attempt still drives completion, and this
		// pass would have invalidated a verification nobody can see. Surface it
		// and let the cascade re-derive.
		return run, step, err
	}
	if c.log != nil {
		c.log.Info("workflow: a verified tree drifted before its commit, so AO is verifying it again",
			"run", run.ID, "step", step.ID, "difference", drift.verified.differenceFrom(drift.observed))
	}
	// The verify step stays as it is: it is still this run's open verify step,
	// and the cascade re-enters it with a target key that now demands a fresh
	// attempt. Advancing it here would be a second authority over the same
	// transition, which is exactly what part 3 removes.
	return run, step, nil
}
