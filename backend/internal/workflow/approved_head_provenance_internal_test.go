package workflow

// The wf-a21d98aa root cause, at the exact function that produced it.
//
// approvedHeadSHA answers "which commit was the approval given for?". It used to
// answer it with an unconditional fallback to the WORK step's completion commit
// whenever no review_target_observed row named the approved fingerprint — and no
// such row exists for any review cycle after the first, because
// reviewTargetFingerprint pins the target only for a first-cycle dispatch.
//
// So on a run whose fix cycles commit, the fallback is a claim about a commit
// two cycles stale, and it is made with no hedge. classifyWorkspaceDrift then
// compares the live HEAD against it, finds them different, and reports a branch
// advance that never happened:
//
//	workspace_provenance: UNKNOWN — the branch advanced from the approved commit
//	77aad8d69358… to 095bf89fd5d0…
//
// while 095bf89f had been HEAD since before the approval was given.

import (
	stdctx "context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const (
	// The two commits from the incident, verbatim.
	incidentWorkHead     = "77aad8d69358fe9476c22a3e509567f6abb12f16"
	incidentFixHead      = "095bf89fd5d0dacb734662e7ed15e9b767eefae5"
	incidentWorkFP       = "45b2e769d8ce54e1e49a8cd8f576bae189481ac8d52d86833209106f0d8f5018"
	incidentFixCycle1FP  = "c2561265f58e0000000000000000000000000000000000000000000000000000"
	incidentApprovedFP   = "b0910a3dfcaba0250171eb769e873a011cb46ad22a24637ddeb9afdd510f66fa"
	incidentObservedFP   = "d137ce80ea07d458d895bfdf954452be3643c98df319521728e9da05c6da2451"
	incidentWorktreePath = "/tmp/ao-incident-worktree"
	incidentBranch       = "feat/engineering-control-center"
)

// incidentFixture rebuilds wf-a21d98aa's durable timeline exactly as the
// production database holds it, and returns the coordinator, the run, its steps
// and the work checkpoint the classifier is handed.
func incidentFixture(t *testing.T) (*Coordinator, stdctx.Context, domain.WorkflowRun, []domain.WorkflowStep, domain.WorkflowStep, domain.WorkflowCheckpoint) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	base := time.Date(2026, 8, 28, 19, 58, 0, 0, time.UTC)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: base}); err != nil {
		t.Fatal(err)
	}
	run := domain.WorkflowRun{
		ID: "wf-a21d98aa", ProjectID: "p", Objective: "audit the worker launch state machine",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: base, UpdatedAt: base,
	}
	seed := []domain.WorkflowStep{
		{ID: "wfs-work", WorkflowRunID: run.ID, Kind: domain.WorkflowStepWork, Ordinal: 1,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-review", WorkflowRunID: run.ID, Kind: domain.WorkflowStepReview, Ordinal: 2,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-fix", WorkflowRunID: run.ID, Kind: domain.WorkflowStepFix, Ordinal: 3,
			State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-verify", WorkflowRunID: run.ID, Kind: domain.WorkflowStepVerify, Ordinal: 4,
			State: domain.WorkflowStepRunning, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
	}
	if _, _, err := store.CreateWorkflowRun(ctx, run, seed); err != nil {
		t.Fatal(err)
	}
	workStep, reviewStep, fixStep := seed[0], seed[1], seed[2]
	write := func(id, phase, stepID, head, before, after string, at time.Time) {
		t.Helper()
		sp := stepID
		if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID: id, WorkflowRunID: run.ID, WorkflowStepID: &sp, ProjectID: "p",
			Branch: incidentBranch, WorktreePath: incidentWorktreePath, HeadSHA: head,
			FingerprintBefore: before, FingerprintAfter: after,
			DurablePhase: phase, PayloadVersion: "v1", RetryState: "{}", CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 20:07:35 — work completes at 77aad8d6, and review cycle 1 pins that head.
	write("cp1", "worker_observed_worker_result_available", workStep.ID, incidentWorkHead, "", incidentWorkFP, base.Add(9*time.Minute+35*time.Second))
	write("cp2", reviewTargetDurablePhase, reviewStep.ID, incidentWorkHead, incidentWorkFP, incidentWorkFP, base.Add(9*time.Minute+35*time.Second))
	// 20:12:55 — fix cycle 1 delivers. 20:14:43 — the fix worker COMMITS: HEAD
	// becomes 095bf89f, which no review cycle after the first ever wrote down.
	write("cp3", "fix_observed_waiting", fixStep.ID, incidentWorkHead, "", incidentFixCycle1FP, base.Add(14*time.Minute+55*time.Second))
	// 20:16:55 — fix cycle 2 delivers, at the new head.
	write("cp4", "fix_observed_waiting", fixStep.ID, incidentFixHead, "", incidentApprovedFP, base.Add(18*time.Minute+55*time.Second))

	workCP, found, err := store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil || !found {
		t.Fatalf("work checkpoint: %v (found=%v)", err, found)
	}
	coord := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return base.Add(21 * time.Minute) }})
	steps, err := store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return coord, ctx, run, steps, reviewStep, workCP
}

// The approval was given for incidentApprovedFP, and that fingerprint was read
// at 095bf89f — the commit the fix step's own delivery observation recorded.
func TestApprovedHeadFollowsTheCycleThatWasActuallyApproved(t *testing.T) {
	coord, ctx, run, _, reviewStep, workCP := incidentFixture(t)
	got := coord.approvedHeadSHA(ctx, run.ID, reviewStep.ID, incidentApprovedFP, workCP)
	if got != incidentFixHead {
		t.Fatalf("approved head = %q, want %q — the work step's commit %q is two fix cycles stale",
			got, incidentFixHead, incidentWorkHead)
	}
}

// And an approval AO holds no observation for gets no answer at all, rather than
// the nearest one. A baseline that cannot be proved is not evidence of anything,
// and every consumer of this value refuses on "".
func TestApprovedHeadIsUnknownRatherThanTheWorkStepsCommit(t *testing.T) {
	coord, ctx, run, _, reviewStep, workCP := incidentFixture(t)
	if got := coord.approvedHeadSHA(ctx, run.ID, reviewStep.ID, "a-fingerprint-nothing-observed", workCP); got != "" {
		t.Fatalf("approved head = %q for an unobserved fingerprint, want \"\"", got)
	}
	// The first cycle's own fingerprint still resolves, from the pin it wrote.
	if got := coord.approvedHeadSHA(ctx, run.ID, reviewStep.ID, incidentWorkFP, workCP); got != incidentWorkHead {
		t.Fatalf("approved head for the first cycle = %q, want %q", got, incidentWorkHead)
	}
}

// The whole classification, at the state verification actually found: the head
// is 095bf89f (unchanged since the approval) and the tree carries the fix
// worker's further uncommitted output. That is AUTHORIZED_FIX — code no reviewer
// has read YET, whose remedy is a review — not the UNKNOWN branch advance that
// parked the run.
func TestIncidentDriftIsAttributedToTheFixWorkerNotAPhantomBranchAdvance(t *testing.T) {
	coord, ctx, run, steps, reviewStep, workCP := incidentFixture(t)
	obs := ports.WorkspaceObservation{
		Path: incidentWorktreePath, Branch: incidentBranch, HeadSHA: incidentFixHead,
		Dirty: true, Changes: []ports.WorkspaceChange{{Path: "docs/plans/cp7.md", Status: " M"}},
	}
	prov := coord.classifyWorkspaceDrift(ctx, run, steps, reviewStep, workCP, obs,
		incidentApprovedFP, incidentObservedFP, "target-key")
	if prov.ApprovedHeadSHA != incidentFixHead {
		t.Fatalf("classified against approved head %q, want %q", prov.ApprovedHeadSHA, incidentFixHead)
	}
	if prov.Class == ProvenanceUnknown {
		t.Fatalf("drift classified UNKNOWN: %s", prov.Rationale)
	}
	if !prov.Class.Authorized() {
		t.Fatalf("drift classified %s (%s), want an authorized class", prov.Class, prov.Rationale)
	}
}

// The true positive the false one must never be traded for: when the head really
// HAS moved past the approval, the uncommitted-drift branch stays unreachable.
func TestARealBranchAdvanceIsStillNotAttributedAsUncommittedDrift(t *testing.T) {
	coord, ctx, run, steps, reviewStep, workCP := incidentFixture(t)
	obs := ports.WorkspaceObservation{
		Path: incidentWorktreePath, Branch: incidentBranch,
		HeadSHA: "a35bd4f8200000000000000000000000000000ff",
	}
	prov := coord.classifyWorkspaceDrift(ctx, run, steps, reviewStep, workCP, obs,
		incidentApprovedFP, incidentObservedFP, "target-key")
	if prov.Class.Authorized() {
		t.Fatalf("a moved head was attributed as this run's own uncommitted work: %s", prov.Rationale)
	}
}

// (j) The stop line's fix-cycle accounting must match the durable rows.
//
// wf-a21d98aa's stop read "verify failed (verify_workspace_changed) after 0 fix
// cycles" on a run that had delivered two. Both the 0 and the 2 werereal: `used`
// counted VERIFY-driven re-entries, of which there were none, while the fix step
// carried two succeeded attempts from the reviewer-driven loop. The sentence was
// the thing that was wrong, and an operator reading it concluded that nothing had
// touched the tree since the work step.
func TestFixCycleAccountingNamesEveryDeliveredCycle(t *testing.T) {
	coord, ctx, run, _, _, _ := incidentFixture(t)
	base := time.Date(2026, 8, 28, 19, 58, 0, 0, time.UTC)
	for i, at := range []time.Time{base.Add(14 * time.Minute), base.Add(18 * time.Minute)} {
		id := "wfa-fix-" + string(rune('1'+i))
		if _, err := coord.store.CreateWorkflowAttempt(ctx, id, "wfs-fix", "codex", "target", at); err != nil {
			t.Fatal(err)
		}
		if err := coord.store.UpdateWorkflowAttemptOutcome(ctx, id, at.Add(time.Minute), domain.WorkflowAttemptSucceeded, ""); err != nil {
			t.Fatal(err)
		}
	}
	got := coord.fixCycleAccounting(ctx, run.ID, 0)
	if !strings.Contains(got, "2 delivered fix cycles") {
		t.Fatalf("accounting = %q, want it to name the 2 durable delivered cycles", got)
	}
	if !strings.Contains(got, "0 of them verify-driven") {
		t.Fatalf("accounting = %q, want it to keep the verify-driven count honest too", got)
	}
}

// (c, d, i) The generation-conditional refusal itself: an approved review does
// not authorize a mutation, a review the step no longer speaks for does not
// authorize one either, and an open verify re-entry is the one thing that does.
func TestFixAuthorityIsGenerationConditional(t *testing.T) {
	coord, ctx, run, _, _, _ := incidentFixture(t)
	base := time.Date(2026, 8, 28, 19, 58, 0, 0, time.UTC)
	// Real rows: workflow_steps.review_run_id has a foreign key, so a fixture
	// that only pretended a review run existed could not bind the step at all.
	sess, err := coord.store.(*sqlite.Store).CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: base, UpdatedAt: base})
	if err != nil {
		t.Fatal(err)
	}
	st := coord.store.(*sqlite.Store)
	if err := st.UpsertReview(ctx, domain.Review{ID: "rev-1", SessionID: sess.ID, ProjectID: "p",
		Harness: domain.ReviewerCodex, CreatedAt: base, UpdatedAt: base}); err != nil {
		t.Fatal(err)
	}
	approved := domain.ReviewRun{ID: "rr-approved", ReviewID: "rev-1", SessionID: sess.ID, Harness: domain.ReviewerCodex,
		TriggerSource: domain.ReviewTriggerAuto, Status: domain.ReviewRunComplete,
		Verdict: domain.VerdictApproved, TargetSHA: incidentApprovedFP, CreatedAt: base}
	stale := domain.ReviewRun{ID: "rr-stale", ReviewID: "rev-1", SessionID: sess.ID, Harness: domain.ReviewerCodex,
		TriggerSource: domain.ReviewTriggerAuto, Status: domain.ReviewRunComplete,
		Verdict: domain.VerdictChangesRequested, TargetSHA: incidentWorkFP, CreatedAt: base}
	for _, r := range []domain.ReviewRun{approved, stale} {
		if err := st.InsertReviewRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	coord.reviewRuns = st

	fixStep := domain.WorkflowStep{ID: "wfs-fix", WorkflowRunID: run.ID, Kind: domain.WorkflowStepFix}
	bind := func(id string) {
		t.Helper()
		if _, err := st.SetWorkflowStepReviewRun(ctx, "wfs-review", id, base); err != nil {
			t.Fatal(err)
		}
	}

	// The step speaks for the approval. Neither review may move the tree.
	bind(approved.ID)
	if refusal := coord.fixAuthorityRefusal(ctx, run, fixStep, approved); refusal == "" {
		t.Fatal("an approved review authorized a fix cycle")
	}
	if refusal := coord.fixAuthorityRefusal(ctx, run, fixStep, stale); refusal == "" {
		t.Fatal("a review the step no longer speaks for authorized a fix cycle")
	}

	// A verification that asked for a fix is the one authorization an approved
	// review carries — and only until it has been answered.
	stepID := "wfs-verify"
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "cp-reentry", WorkflowRunID: run.ID, WorkflowStepID: &stepID, ProjectID: "p",
		DurablePhase: ReasonVerifyFixReentry, PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: base.Add(20 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if refusal := coord.fixAuthorityRefusal(ctx, run, fixStep, approved); refusal != "" {
		t.Fatalf("an open verify re-entry did not authorize its fix cycle: %s", refusal)
	}
	if _, err := st.CreateWorkflowAttempt(ctx, "wfa-answer", "wfs-fix", "codex", "t", base.Add(21*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if refusal := coord.fixAuthorityRefusal(ctx, run, fixStep, approved); refusal == "" {
		t.Fatal("an already-answered verify re-entry authorized a second fix cycle")
	}
	// And the stale review is refused throughout, whatever the re-entry says.
	if refusal := coord.fixAuthorityRefusal(ctx, run, fixStep, stale); refusal == "" {
		t.Fatal("a stale review generation authorized a fix cycle under an open re-entry")
	}
}

// authorityReviewRuns answers only the question the authority check asks. The
// embedded interface is nil on purpose: any OTHER method reaching this fake is a
// test asserting something it did not mean to, and a panic says so immediately.
type authorityReviewRuns struct {
	ReviewRuns
	runs map[string]domain.ReviewRun
}

func (f *authorityReviewRuns) GetReviewRun(_ stdctx.Context, id string) (domain.ReviewRun, bool, error) {
	r, ok := f.runs[id]
	return r, ok, nil
}
