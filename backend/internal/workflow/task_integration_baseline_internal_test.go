package workflow

// What "the verified commit" means after a branch has moved
// (task_integration_baseline.go).
//
// The authority is a fresh review AND a fresh verification that both passed
// against the SAME head, and nothing since. Ancestry alone is deliberately not
// enough, so most of these tests are about the ways that pair can fail to hold.

import (
	stdctx "context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const (
	blWorkCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // A: what the task wrote
	blHeadB      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" // B: the branch moved here
	blHeadC      = "cccccccccccccccccccccccccccccccccccccccc" // C: and later here
	blReviewID   = "review-of-B"
)

// blObs is the observation for a head; the fingerprints the review and the
// verification agree on must be the REAL hash of it, not a label, because
// proof 4 re-observes and re-hashes.
func blObs(head string) ports.WorkspaceObservation {
	return ports.WorkspaceObservation{Path: "/tmp/bl", Branch: "main", HeadSHA: head}
}

var (
	blFpB = WorkspaceFingerprint(blObs(blHeadB))
	blFpC = WorkspaceFingerprint(blObs(blHeadC))
)

type baselineFixture struct {
	t     *testing.T
	coord *Coordinator
	store *sqlite.Store
	rr    *baselineReviews
	ws    *baselineWorkspace
	ctx   stdctx.Context
	runID string
	n     int
}

type baselineWorkspace struct{ obs ports.WorkspaceObservation }

func (w *baselineWorkspace) ObserveWorkspace(_ stdctx.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	return w.obs, nil
}

// baselineReviews implements only the one method this path calls; the embedded
// interface satisfies the rest and would panic loudly if anything else were
// reached, which is the point.
type baselineReviews struct {
	ReviewRuns
	runs map[string]domain.ReviewRun
}

func (r *baselineReviews) GetReviewRun(_ stdctx.Context, id string) (domain.ReviewRun, bool, error) {
	run, ok := r.runs[id]
	return run, ok, nil
}

// newBaselineFixture: the task wrote A, the branch is now at B, an approving
// review of B exists, and a verification passed against B.
func newBaselineFixture(t *testing.T) *baselineFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	runID, taskID := "wf-bl", "task-bl"
	steps := []domain.WorkflowStep{
		{ID: "wfs-work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 2, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 3, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
	}
	run := domain.WorkflowRun{ID: runID, ProjectID: "p", Objective: "o", State: domain.WorkflowRunCompleted,
		PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now, PlannedTaskID: &taskID}
	if _, _, err := store.CreateWorkflowRun(ctx, run, steps); err != nil {
		t.Fatal(err)
	}

	fx := &baselineFixture{t: t, store: store, ctx: ctx, runID: runID,
		rr: &baselineReviews{runs: map[string]domain.ReviewRun{
			blReviewID: {ID: blReviewID, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, TargetSHA: blFpB},
		}},
		ws: &baselineWorkspace{obs: blObs(blHeadB)},
	}
	// The work checkpoint (the authorized workspace) and the task's own commit.
	workStepID := "wfs-work"
	fx.cp(domain.WorkflowCheckpoint{ID: "wfc-work", WorkflowStepID: &workStepID,
		Branch: "main", WorktreePath: "/tmp/bl", DurablePhase: "worker_observed", RetryState: "{}"})
	fx.cp(domain.WorkflowCheckpoint{ID: "wfc-commit", HeadSHA: blWorkCommit, Branch: "main",
		WorktreePath: "/tmp/bl", DurablePhase: autonomousLocalCommitPhase, RetryState: "{}"})
	fx.coord = New(Deps{Store: store, Projects: store, ReviewRuns: fx.rr, WorkspaceFacts: fx.ws,
		Clock: func() time.Time { return time.Now().UTC() }})
	return fx
}

func (fx *baselineFixture) cp(cp domain.WorkflowCheckpoint) {
	fx.t.Helper()
	fx.n++
	cp.WorkflowRunID = fx.runID
	cp.ProjectID = "p"
	if cp.PayloadVersion == "" {
		cp.PayloadVersion = "v1"
	}
	if cp.RetryState == "" {
		cp.RetryState = "{}"
	}
	if cp.ID == "" {
		cp.ID = "wfc-bl-" + string(rune('a'+fx.n))
	}
	cp.CreatedAt = time.Now().UTC().Add(time.Duration(fx.n) * time.Millisecond)
	if _, err := fx.store.CreateWorkflowCheckpoint(fx.ctx, cp); err != nil {
		fx.t.Fatal(err)
	}
}

// reviewedAt pins the review's target fingerprint to a head.
func (fx *baselineFixture) reviewedAt(head, fingerprint string) {
	stepID := "wfs-review"
	fx.cp(domain.WorkflowCheckpoint{WorkflowStepID: &stepID, HeadSHA: head,
		FingerprintAfter: fingerprint, DurablePhase: reviewTargetDurablePhase,
		RetryState: `{"cycle":1}`})
}

// verifiedAt records a verification result for a fingerprint.
func (fx *baselineFixture) verifiedAt(fingerprint string, passed bool) {
	res := VerifyResult{Version: verifyResultVersion, Passed: passed,
		ReviewedFingerprint: fingerprint, PreFingerprint: fingerprint, PostFingerprint: fingerprint}
	raw, _ := json.Marshal(res)
	stepID := "wfs-verify"
	fx.cp(domain.WorkflowCheckpoint{WorkflowStepID: &stepID, DurablePhase: verifyResultPhase, RetryState: string(raw)})
}

func (fx *baselineFixture) reconcile() bool {
	fx.t.Helper()
	steps, err := fx.store.ListWorkflowSteps(fx.ctx, fx.runID)
	if err != nil {
		fx.t.Fatal(err)
	}
	// The review link is set on the in-memory step rather than through the
	// store: the step->review_run foreign key would require a whole review
	// lineage in the database, and none of it is what these tests are about.
	rid := blReviewID
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepReview {
			steps[i].ReviewRunID = &rid
		}
	}
	run, _, err := fx.store.GetWorkflowRun(fx.ctx, fx.runID)
	if err != nil {
		fx.t.Fatal(err)
	}
	wrote, err := fx.coord.reconcileVerifiedIntegrationBaseline(fx.ctx, run, steps)
	if err != nil {
		fx.t.Fatal(err)
	}
	return wrote
}

func (fx *baselineFixture) baseline() (VerifiedIntegrationBaseline, bool) {
	return fx.coord.verifiedIntegrationBaseline(fx.ctx, fx.runID)
}

// ---- 1. work commit A, branch advanced to B, review+verify B -> baseline B --

func TestFreshReviewAndVerifyAtBEstablishBAsTheBaseline(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)
	fx.verifiedAt(blFpB, true)

	if !fx.reconcile() {
		t.Fatal("a review and a verification that both passed at B did not establish B")
	}
	got, ok := fx.baseline()
	if !ok || got.VerifiedIntegrationCommit != blHeadB {
		t.Fatalf("baseline = %q ok=%v, want B", got.VerifiedIntegrationCommit, ok)
	}
	if got.ReviewRunID != blReviewID || got.Fingerprint != blFpB {
		t.Fatalf("the baseline does not carry the evidence it rests on: %+v", got)
	}
}

// ---- 6. the original work commit is preserved -----------------------------

func TestBaselinePreservesTheOriginalWorkCommit(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)
	fx.verifiedAt(blFpB, true)
	fx.reconcile()

	got, _ := fx.baseline()
	if got.OriginalWorkCommit != blWorkCommit {
		t.Fatalf("originalWorkCommit = %q, want A: the answer to 'what did this task write' must survive", got.OriginalWorkCommit)
	}
	if got.VerifiedIntegrationCommit == got.OriginalWorkCommit {
		t.Fatal("the baseline overwrote the work commit instead of standing beside it")
	}
	// And the work-commit checkpoint itself is untouched.
	cps, err := fx.store.ListWorkflowCheckpoints(fx.ctx, fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == autonomousLocalCommitPhase && cp.HeadSHA == blWorkCommit {
			found = true
		}
	}
	if !found {
		t.Fatal("the original work-commit checkpoint was removed or rewritten")
	}
}

// ---- 2. the branch moved again after the verification -> refuse ------------

func TestBranchMovingAfterTheVerificationRefusesToBaseline(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)
	fx.verifiedAt(blFpB, true)
	// Something landed after the verification.
	fx.ws.obs.HeadSHA = blHeadC

	if fx.reconcile() {
		t.Fatal("baselined a head that moved after the verification — nobody has reviewed or verified C")
	}
	if _, ok := fx.baseline(); ok {
		t.Fatal("a baseline was written for a head nothing certified")
	}
}

// The same refusal expressed through the workspace identity rather than the
// head: the tree changed even though the commit did not.
func TestAChangedWorkspaceRefusesToBaseline(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)
	fx.verifiedAt(blFpB, true)
	// Uncommitted work appeared: same commit, different workspace.
	fx.ws.obs.Dirty = true
	fx.ws.obs.Changes = []ports.WorkspaceChange{{Path: "somebody/edited.go", Status: " M"}}

	if fx.reconcile() {
		t.Fatal("baselined a workspace that no longer hashes to what was reviewed and verified")
	}
}

// ---- 3. review approved B but the verification failed -> never baseline ----

func TestApprovedReviewWithAFailingVerificationNeverBaselines(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)
	fx.verifiedAt(blFpB, false)

	if fx.reconcile() {
		t.Fatal("baselined B on a verification that FAILED — this is the unverified promotion the whole file exists to prevent")
	}
	if _, ok := fx.baseline(); ok {
		t.Fatal("a baseline exists with no passing verification behind it")
	}
}

// No verification at all is the same answer.
func TestApprovedReviewWithNoVerificationNeverBaselines(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)

	if fx.reconcile() {
		t.Fatal("baselined B with no verification behind it")
	}
}

// ---- 4. the verification passed but not on what the review approved -------

func TestVerificationOfADifferentWorkspaceThanTheReviewRefuses(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)
	// A verification that passed — of a different tree.
	fx.verifiedAt(blFpC, true)

	if fx.reconcile() {
		t.Fatal("baselined on a verification of a workspace the reviewer never read")
	}
}

// And the mirror: the review approved a fingerprint that was never pinned to
// any head, so "approved B" cannot be checked at all.
func TestAnApprovalThatCannotBeTiedToAHeadRefuses(t *testing.T) {
	fx := newBaselineFixture(t)
	// No review_target_observed row at all.
	fx.verifiedAt(blFpB, true)

	if fx.reconcile() {
		t.Fatal("baselined an approval that could not be tied to a commit")
	}
}

// A review that did not approve is not authority, whatever verification says.
func TestANonApprovingReviewNeverBaselines(t *testing.T) {
	fx := newBaselineFixture(t)
	r := fx.rr.runs[blReviewID]
	r.Verdict = domain.VerdictChangesRequested
	fx.rr.runs[blReviewID] = r
	fx.reviewedAt(blHeadB, blFpB)
	fx.verifiedAt(blFpB, true)

	if fx.reconcile() {
		t.Fatal("baselined on a review that requested changes")
	}
}

// ---- 5. idempotence and restart-safety ------------------------------------

func TestBaselineIsIdempotentAcrossPollsAndRestarts(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)
	fx.verifiedAt(blFpB, true)

	if !fx.reconcile() {
		t.Fatal("precondition")
	}
	for i := 0; i < 20; i++ {
		if fx.reconcile() {
			t.Fatalf("pass %d wrote a second baseline for the same commit", i)
		}
	}
	// A second Coordinator over the same store IS a restart.
	fx.coord = New(Deps{Store: fx.store, Projects: fx.store, ReviewRuns: fx.rr, WorkspaceFacts: fx.ws,
		Clock: func() time.Time { return time.Now().UTC() }})
	if fx.reconcile() {
		t.Fatal("a restart wrote a duplicate baseline")
	}
	got, ok := fx.baseline()
	if !ok || got.VerifiedIntegrationCommit != blHeadB {
		t.Fatalf("baseline after restart = %q ok=%v, want B", got.VerifiedIntegrationCommit, ok)
	}
	rows := 0
	cps, _ := fx.store.ListWorkflowCheckpoints(fx.ctx, fx.runID)
	for _, cp := range cps {
		if cp.DurablePhase == integrationBaselineVerifiedPhase {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("baseline rows = %d, want exactly 1", rows)
	}
}

// A genuinely later commit that earns its own review and verification advances
// the baseline, and the earlier one stays as history.
func TestALaterCertifiedCommitAdvancesTheBaseline(t *testing.T) {
	fx := newBaselineFixture(t)
	fx.reviewedAt(blHeadB, blFpB)
	fx.verifiedAt(blFpB, true)
	if !fx.reconcile() {
		t.Fatal("precondition")
	}
	// The branch moved to C, and C earned its own review and verification.
	r := fx.rr.runs[blReviewID]
	r.TargetSHA = blFpC
	fx.rr.runs[blReviewID] = r
	fx.reviewedAt(blHeadC, blFpC)
	fx.verifiedAt(blFpC, true)
	fx.ws.obs.HeadSHA = blHeadC

	if !fx.reconcile() {
		t.Fatal("a commit that earned its own review and verification did not advance the baseline")
	}
	got, _ := fx.baseline()
	if got.VerifiedIntegrationCommit != blHeadC {
		t.Fatalf("baseline = %q, want C", got.VerifiedIntegrationCommit)
	}
	if got.OriginalWorkCommit != blWorkCommit {
		t.Fatal("advancing the baseline lost the original work commit")
	}
}
