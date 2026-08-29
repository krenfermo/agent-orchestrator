package workflow

// P0-B regression: the production failure this block exists for.
//
//	review approved a target
//	fix cycles advanced the branch
//	verify stopped with workspace_provenance / verify_workspace_changed
//	ContinueRun parked again, forever
//
// because AO could not name the commit the approved fingerprint had been read
// at, and every question downstream of that (rewritten history? merely grown
// branch? uncommitted work?) is asked against that commit.
//
// Two mechanisms answer it, and NOTHING else is allowed to:
//
//   - reconstruction, when the answer is deterministically derivable;
//   - an explicit, human-only, bounded operator recovery when it is not.
//
// The properties asserted here are, in order of importance: no fabricated
// approved head, ever; a proven one when a proof exists; and a way out that is
// not "park forever" when it does not.

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// fakeCommitHistory is the branch's own history, in the order git rev-list
// would give it.
type fakeCommitHistory struct {
	commits []string
	err     error
	calls   int
}

func (h *fakeCommitHistory) ListCommits(_ stdctx.Context, _ string, limit int) ([]string, error) {
	h.calls++
	if h.err != nil {
		return nil, h.err
	}
	if limit > 0 && len(h.commits) > limit {
		return h.commits[:limit], nil
	}
	return h.commits, nil
}

// ---------------------------------------------------------------------------
// Reconstruction
// ---------------------------------------------------------------------------

// A clean worktree's fingerprint is a pure function of its commit, so the
// commit an approval was read at can be RECOVERED by recomputing that function
// forwards over the branch until it matches. That is a proof, not a guess:
// nothing else hashes to the approved value.
func TestApprovedHeadIsReconstructedFromTheBranchWhenTheLedgerNeverRecordedIt(t *testing.T) {
	c, ctx, run, reviewStep, workCP, history := reconstructionFixture(t,
		[]string{"cccc3333", "bbbb2222", "aaaa1111"})
	approved := cleanWorkspaceFingerprint("bbbb2222")

	got := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, approved, workCP)
	if got != "bbbb2222" {
		t.Fatalf("approved head = %q, want bbbb2222 recovered from the branch's own history", got)
	}
	if history.calls != 1 {
		t.Fatalf("history reads = %d, want exactly 1", history.calls)
	}

	// The answer is durable, and it is durable in the ordinary shape: a row
	// carrying the fingerprint AND the head, which rule 3 resolves like any
	// other observation. A second read must not re-run git.
	if got := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, approved, workCP); got != "bbbb2222" {
		t.Fatalf("second read = %q, want the recorded answer", got)
	}
	if history.calls != 1 {
		t.Fatalf("history reads = %d after a second lookup, want the answer to have been recorded", history.calls)
	}
}

// The reconstruction cannot produce a false positive for an approval that was
// read at a DIRTY tree: such a fingerprint hashes uncommitted change lines too,
// so no clean-tree hash can equal it. The arithmetic refuses on its own; there
// is no separate "was it clean?" test to get wrong.
func TestADirtyApprovalIsNeverMatchedToACommit(t *testing.T) {
	c, ctx, run, reviewStep, workCP, _ := reconstructionFixture(t,
		[]string{"cccc3333", "bbbb2222", "aaaa1111"})
	dirty := WorkspaceFingerprint(ports.WorkspaceObservation{
		HeadSHA: "bbbb2222", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "a.go", Status: "M"}},
	})
	if got := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, dirty, workCP); got != "" {
		t.Fatalf("approved head = %q, want \"\": an approval read at a dirty tree names no commit AO can recover", got)
	}
}

// A fingerprint that is on no commit of this branch answers "", and the
// completed negative search is remembered so a poll loop does not re-run git
// forever to re-derive the same no.
func TestAnUnreconstructibleApprovedHeadAnswersNothingAndIsNotRetriedForever(t *testing.T) {
	c, ctx, run, reviewStep, workCP, history := reconstructionFixture(t,
		[]string{"cccc3333", "bbbb2222"})
	orphan := cleanWorkspaceFingerprint("dddd4444")
	for i := 0; i < 3; i++ {
		if got := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, orphan, workCP); got != "" {
			t.Fatalf("approved head = %q, want \"\"", got)
		}
	}
	if history.calls != 1 {
		t.Fatalf("history reads = %d, want 1: a completed negative search is an answer and is recorded", history.calls)
	}
}

// A repository AO could not read is NOT a negative answer, and must not be
// recorded as one — the next pass, on a mounted disk, may well answer it.
func TestAnUnreadableRepositoryIsNotRecordedAsANegativeAnswer(t *testing.T) {
	c, ctx, run, reviewStep, workCP, history := reconstructionFixture(t, nil)
	history.err = errors.New("not a git repository")
	approved := cleanWorkspaceFingerprint("bbbb2222")
	if got := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, approved, workCP); got != "" {
		t.Fatalf("approved head = %q, want \"\"", got)
	}
	history.err = nil
	history.commits = []string{"bbbb2222"}
	if got := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, approved, workCP); got != "bbbb2222" {
		t.Fatalf("approved head = %q, want the answer once the repository could be read", got)
	}
}

// The classifier's own refusal, when the head is genuinely unprovable, is
// NAMED and ACTIONABLE rather than an unexplained dead end — and it stays
// fail-closed: UNKNOWN, never a promotion to the uncommitted-drift branch.
func TestAnUnprovableApprovedHeadStaysFailClosedAndSaysSo(t *testing.T) {
	c, ctx, run, reviewStep, workCP, _ := reconstructionFixture(t, []string{"cccc3333"})
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	obs := ports.WorkspaceObservation{Path: workCP.WorktreePath, Branch: workCP.Branch, HeadSHA: "cccc3333", Dirty: true}
	rec := c.classifyWorkspaceDrift(ctx, run, steps, reviewStep, workCP, obs,
		cleanWorkspaceFingerprint("zzzz9999"), WorkspaceFingerprint(obs), "target")

	if rec.Class != ProvenanceUnknown {
		t.Fatalf("class = %q, want UNKNOWN: an unprovable baseline is never evidence of innocence", rec.Class)
	}
	if !rec.ApprovedHeadUnprovable {
		t.Fatal("the record does not name the missing fact, so nothing downstream can offer the recovery for it")
	}
	if !strings.Contains(rec.Rationale, "recover this run's review provenance") {
		t.Fatalf("rationale = %q, want it to name the operator recovery", rec.Rationale)
	}
}

// ---------------------------------------------------------------------------
// The operator recovery
// ---------------------------------------------------------------------------

func TestOperatorRecoveryDiscardsTheUnlocatableApprovalAndAsksForOneFreshReview(t *testing.T) {
	fx := newUnprovableProvenanceFixture(t)
	detail, err := fx.c.RecoverUnprovableApprovedHead(fx.ctx, fx.runID)
	if err != nil {
		t.Fatalf("RecoverUnprovableApprovedHead: %v", err)
	}
	if detail.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want the stop cleared", detail.Run.State)
	}
	steps, _ := fx.store.ListWorkflowSteps(fx.ctx, fx.runID)
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepReview:
			if s.State == domain.WorkflowStepCompleted {
				t.Fatal("the review step was not reopened, so no fresh review can be dispatched")
			}
		case domain.WorkflowStepVerify:
			if s.State == domain.WorkflowStepFailed {
				t.Fatal("the verify step is still terminal, so re-verification is structurally impossible")
			}
		}
	}
	got := phases(t, fx.store, fx.ctx, fx.runID)
	if !containsPhase(got, operatorProvenanceRecoveryPhase) {
		t.Fatalf("phases = %v, want the human authorization recorded", got)
	}
	if !containsPhase(got, provenanceFreshReviewPhase) {
		t.Fatalf("phases = %v, want a fresh review requested", got)
	}
	// It attests NOTHING about a commit: the record still says UNKNOWN, and no
	// approved head was invented.
	rec := latestProvenanceRecord(t, fx.store, fx.ctx, fx.runID)
	if rec.Class != ProvenanceUnknown {
		t.Fatalf("class = %q: a human decision must not change what AO proved", rec.Class)
	}
	if rec.ApprovedHeadSHA != "" {
		t.Fatalf("approved head = %q, want none: the approval is discarded, never attested", rec.ApprovedHeadSHA)
	}
	if !strings.Contains(rec.Rationale, "DISCARDS") {
		t.Fatalf("rationale = %q, want it to say plainly what was given up", rec.Rationale)
	}
}

// If the head turns out to be provable after all, the ordinary automatic paths
// own the run and a person must not be allowed to discard an approval AO can
// still locate.
func TestOperatorRecoveryRefusesWhenTheApprovedHeadIsProvable(t *testing.T) {
	fx := newUnprovableProvenanceFixture(t)
	fx.history.commits = []string{fx.headSHA, fx.approvedHead}
	_, err := fx.c.RecoverUnprovableApprovedHead(fx.ctx, fx.runID)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "needs no human recovery") {
		t.Fatalf("err = %v, want a refusal naming the provable commit", err)
	}
}

func TestOperatorRecoveryRefusesARunThatIsNotStoppedOnThis(t *testing.T) {
	fx := newUnprovableProvenanceFixture(t)
	// A run that is running has nothing to recover.
	if _, err := fx.store.UpdateWorkflowRunState(fx.ctx, fx.runID,
		domain.WorkflowRunNeedsAttention, domain.WorkflowRunRunning, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.c.RecoverUnprovableApprovedHead(fx.ctx, fx.runID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for a run that is not stopped", err)
	}
}

func TestOperatorRecoveryIsBounded(t *testing.T) {
	fx := newUnprovableProvenanceFixture(t)
	for i := 0; i < maxOperatorProvenanceRecoveries; i++ {
		if _, err := fx.c.RecoverUnprovableApprovedHead(fx.ctx, fx.runID); err != nil {
			t.Fatalf("recovery %d: %v", i+1, err)
		}
		fx.reparkOnTheSameStop()
	}
	_, err := fx.c.RecoverUnprovableApprovedHead(fx.ctx, fx.runID)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "will not do it again") {
		t.Fatalf("err = %v, want a bounded refusal", err)
	}
}

// Nothing automatic may reach the operator recovery.
func TestNothingAutomaticRecoversAnUnprovableApprovedHead(t *testing.T) {
	fx := newUnprovableProvenanceFixture(t)
	for i := 0; i < 3; i++ {
		if err := fx.c.Reconcile(fx.ctx); err != nil {
			t.Fatal(err)
		}
		_, _ = fx.c.ContinueRun(fx.ctx, fx.runID)
	}
	if containsPhase(phases(t, fx.store, fx.ctx, fx.runID), operatorProvenanceRecoveryPhase) {
		t.Fatal("an automatic pass authorized an operator recovery")
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func reconstructionFixture(t *testing.T, commits []string) (
	*Coordinator, stdctx.Context, domain.WorkflowRun, domain.WorkflowStep, domain.WorkflowCheckpoint, *fakeCommitHistory,
) {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	worktree := t.TempDir()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: worktree, RegisteredAt: base}); err != nil {
		t.Fatal(err)
	}
	run := domain.WorkflowRun{ID: "wf-recon", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: base, UpdatedAt: base}
	sid := "sess-recon"
	seed := []domain.WorkflowStep{
		{ID: "wfs-work", WorkflowRunID: run.ID, Kind: domain.WorkflowStepWork, Ordinal: 1,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", SessionID: &sid, CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-review", WorkflowRunID: run.ID, Kind: domain.WorkflowStepReview, Ordinal: 2,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-verify", WorkflowRunID: run.ID, Kind: domain.WorkflowStepVerify, Ordinal: 3,
			State: domain.WorkflowStepRunning, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
	}
	if _, _, err := st.CreateWorkflowRun(ctx, run, seed); err != nil {
		t.Fatal(err)
	}
	// A ledger that records the work step's completion at a DIFFERENT
	// fingerprint, which is precisely why rules 1-4 cannot answer.
	workStepID := seed[0].ID
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "cp-work", WorkflowRunID: run.ID, WorkflowStepID: &workStepID, ProjectID: "p",
		Branch: "feat/x", WorktreePath: worktree, HeadSHA: "aaaa1111",
		FingerprintAfter: cleanWorkspaceFingerprint("aaaa1111"),
		DurablePhase:     "worker_observed_worker_result_available", PayloadVersion: "v1",
		RetryState: "{}", CreatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	history := &fakeCommitHistory{commits: commits}
	c := New(Deps{Store: st, Projects: st, CommitHistory: history,
		Clock: func() time.Time { return time.Now().UTC() }})
	workCP, found, err := st.GetLatestWorkflowCheckpointByStep(ctx, workStepID)
	if err != nil || !found {
		t.Fatalf("work checkpoint: found=%v err=%v", found, err)
	}
	return c, ctx, run, seed[1], workCP, history
}

type unprovableProvenanceFixture struct {
	t            *testing.T
	c            *Coordinator
	store        *sqlite.Store
	ctx          stdctx.Context
	runID        string
	history      *fakeCommitHistory
	headSHA      string
	approvedHead string
	approvedFP   string
}

// newUnprovableProvenanceFixture reproduces the production shape: a run parked
// on a workspace change whose approved fingerprint AO can neither look up nor
// reconstruct.
func newUnprovableProvenanceFixture(t *testing.T) *unprovableProvenanceFixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	worktree := t.TempDir()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: worktree, RegisteredAt: base}); err != nil {
		t.Fatal(err)
	}
	runID := "wf-unprovable"
	run := domain.WorkflowRun{ID: runID, ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunNeedsAttention, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: base, UpdatedAt: base}
	// The store assigns the session id, so it is created FIRST and everything
	// that references it uses what came back.
	worker, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker,
		Harness:  domain.HarnessClaudeCode,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: base},
		Metadata: domain.SessionMetadata{Branch: "feat/x", WorkspacePath: worktree},
		// Long finished, so the "nothing of AO's is moving" proof holds.
		IsTerminated: true, CreatedAt: base, UpdatedAt: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	sid := string(worker.ID)
	seed := []domain.WorkflowStep{
		{ID: "wfs-work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 1,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", SessionID: &sid, CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 2,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 3,
			State: domain.WorkflowStepFailed, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
	}
	if _, _, err := st.CreateWorkflowRun(ctx, run, seed); err != nil {
		t.Fatal(err)
	}
	workStepID, verifyStepID := seed[0].ID, seed[2].ID
	approvedHead := "bbbb2222"
	approvedFP := cleanWorkspaceFingerprint(approvedHead)
	headSHA := "cccc3333"
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "cp-work", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "p",
		SessionID: &sid, Branch: "feat/x", WorktreePath: worktree, HeadSHA: "aaaa1111",
		FingerprintAfter: cleanWorkspaceFingerprint("aaaa1111"),
		DurablePhase:     "worker_observed_worker_result_available", PayloadVersion: "v1",
		RetryState: "{}", CreatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	// The verification that stopped, and the stop that named it.
	result := VerifyResult{Version: verifyResultVersion, TargetKey: "target",
		ReviewedFingerprint: approvedFP, ErrorClass: domain.WorkflowErrorVerifyWorkspaceChanged}
	raw, _ := json.Marshal(result)
	payload := string(raw)
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "cp-verify", WorkflowRunID: runID, WorkflowStepID: &verifyStepID, ProjectID: "p",
		RetryState: payload, DurablePhase: verifyResultPhase, PayloadVersion: verifyResultVersion,
		FingerprintBefore: approvedFP, CreatedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "cp-stop", WorkflowRunID: runID, WorkflowStepID: &verifyStepID, ProjectID: "p",
		NextAction:   "verify stopped: the approved head cannot be proved",
		DurablePhase: ReasonVerifyApprovedHeadUnprovable, PayloadVersion: "v1",
		RetryState: "{}", CreatedAt: base.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// The live worktree, which is at a commit no approval names.
	ws := &staticWorkspace{obs: ports.WorkspaceObservation{
		Path: worktree, Branch: "feat/x", HeadSHA: headSHA, Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "new.go", Status: "??"}},
	}}
	history := &fakeCommitHistory{commits: []string{headSHA}}
	c := New(Deps{Store: st, Projects: st, WorkspaceFacts: ws, SessionFacts: st,
		CommitHistory: history, Clock: func() time.Time { return time.Now().UTC() }})
	return &unprovableProvenanceFixture{t: t, c: c, store: st, ctx: ctx, runID: runID,
		history: history, headSHA: headSHA, approvedHead: approvedHead, approvedFP: approvedFP}
}

// reparkOnTheSameStop puts the run back exactly where the recovery found it, as
// a second unresolved verification would.
func (f *unprovableProvenanceFixture) reparkOnTheSameStop() {
	f.t.Helper()
	now := time.Now().UTC()
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	if run.State != domain.WorkflowRunNeedsAttention {
		if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			f.t.Fatal(err)
		}
	}
	steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepReview:
			if s.State != domain.WorkflowStepCompleted {
				if _, err := f.store.UpdateWorkflowStepState(f.ctx, s.ID, s.State, domain.WorkflowStepCompleted, now); err != nil {
					f.t.Fatal(err)
				}
			}
		case domain.WorkflowStepVerify:
			if s.State != domain.WorkflowStepFailed {
				if _, err := f.store.UpdateWorkflowStepState(f.ctx, s.ID, s.State, domain.WorkflowStepFailed, now); err != nil {
					f.t.Fatal(err)
				}
			}
			id := s.ID
			if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
				ID: "wfc-repark-" + now.Format("150405.000000000"), WorkflowRunID: f.runID,
				WorkflowStepID: &id, ProjectID: "p",
				NextAction:     "verify stopped again on the same unprovable approval",
				DurablePhase:   ReasonVerifyApprovedHeadUnprovable,
				PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
			}); err != nil {
				f.t.Fatal(err)
			}
		}
	}
}

type staticWorkspace struct{ obs ports.WorkspaceObservation }

func (w *staticWorkspace) ObserveWorkspace(_ stdctx.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	return w.obs, nil
}

func latestProvenanceRecord(t *testing.T, st *sqlite.Store, ctx stdctx.Context, runID string) WorkspaceProvenanceRecord {
	t.Helper()
	cps, err := st.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var out WorkspaceProvenanceRecord
	for _, cp := range cps {
		if cp.DurablePhase != workspaceProvenancePhase {
			continue
		}
		var rec WorkspaceProvenanceRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil {
			out = rec
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The production failure, end to end
// ---------------------------------------------------------------------------

// This is the incident's own shape: a run whose review approved a fingerprint,
// whose fix worker kept writing to the same tree, and whose verification then
// stopped because AO could not name the commit that approval was read at.
//
// Before the reconstruction, ContinueRun re-derived the same missing fact and
// parked again -- forever. Now the commit is RECOVERED from the branch's own
// history, the drift is attributed to this run's own authorized fix worker, and
// the run leaves the stop through the same fresh-review transition every other
// attributable drift takes. No person is needed, and no approved head is
// invented: it is proved.
func TestALegacyRunParkedOnAnUnprovableApprovedHeadRecoversOnItsOwn(t *testing.T) {
	fx := newLegacyProvenanceFixture(t)

	if _, err := fx.c.ContinueRun(fx.ctx, fx.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	run, _, _ := fx.store.GetWorkflowRun(fx.ctx, fx.runID)
	if run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want the stop cleared once the approved commit was recovered", run.State)
	}
	rec := latestProvenanceRecord(t, fx.store, fx.ctx, fx.runID)
	if !rec.Class.Authorized() {
		t.Fatalf("class = %q (%s), want the drift attributed to this run's own authorized agent", rec.Class, rec.Rationale)
	}
	if rec.ApprovedHeadSHA != fx.head {
		t.Fatalf("approved head = %q, want the reconstructed %q", rec.ApprovedHeadSHA, fx.head)
	}
	if !containsPhase(phases(t, fx.store, fx.ctx, fx.runID), approvedHeadReconstructionPhase) {
		t.Fatal("the reconstruction was not recorded, so nothing can audit how the commit was resolved")
	}
	if !containsPhase(phases(t, fx.store, fx.ctx, fx.runID), provenanceFreshReviewPhase) {
		t.Fatal("the run was unparked without asking for a review of the code no reviewer has read")
	}
	// And it did NOT verify against the old approval: the review step is open
	// again, which is the whole difference between this and skipping the
	// reviewer.
	steps, _ := fx.store.ListWorkflowSteps(fx.ctx, fx.runID)
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepReview && s.State == domain.WorkflowStepCompleted {
			t.Fatal("the review step was not reopened")
		}
	}
}

type legacyProvenanceFixture struct {
	c     *Coordinator
	store *sqlite.Store
	ctx   stdctx.Context
	runID string
	head  string
}

func newLegacyProvenanceFixture(t *testing.T) *legacyProvenanceFixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	worktree := t.TempDir()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: worktree, RegisteredAt: base}); err != nil {
		t.Fatal(err)
	}
	runID := "wf-legacy"
	worker, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		Activity:     domain.Activity{State: domain.ActivityIdle, LastActivityAt: base},
		Metadata:     domain.SessionMetadata{Branch: "feat/x", WorkspacePath: worktree},
		IsTerminated: true, CreatedAt: base, UpdatedAt: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	sid := string(worker.ID)
	run := domain.WorkflowRun{ID: runID, ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunNeedsAttention, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: base, UpdatedAt: base}
	seed := []domain.WorkflowStep{
		{ID: "wfs-work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 1,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", SessionID: &sid, CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 2,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 3,
			State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 4,
			State: domain.WorkflowStepFailed, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
	}
	if _, _, err := st.CreateWorkflowRun(ctx, run, seed); err != nil {
		t.Fatal(err)
	}
	head := "bbbb2222"
	approvedFP := cleanWorkspaceFingerprint(head)
	write := func(id, phase, stepID, headSHA, after string, at time.Time) {
		t.Helper()
		sp := stepID
		if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID: id, WorkflowRunID: runID, WorkflowStepID: &sp, ProjectID: "p",
			SessionID: &sid, Branch: "feat/x", WorktreePath: worktree, HeadSHA: headSHA,
			FingerprintAfter: after, DurablePhase: phase, PayloadVersion: "v1",
			RetryState: "{}", CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The work step finished at a DIFFERENT state, which is why the old
	// unconditional fallback to its commit was wrong and why rules 1-4 answer
	// nothing here.
	write("cp-work", "worker_observed_worker_result_available", "wfs-work", "aaaa1111", cleanWorkspaceFingerprint("aaaa1111"), base)
	// The fix cycle that produced the approved state. It is the LEGACY shape:
	// a delivery observation with no HeadSHA, written by a daemon that did not
	// yet record one — so nothing in the ledger names the approval's commit.
	write("cp-fix", "fix_observed_waiting", "wfs-fix", "", approvedFP, base.Add(time.Minute))

	// The verification that stopped, and the stop that named it.
	result := VerifyResult{Version: verifyResultVersion, TargetKey: "target",
		ReviewedFingerprint: approvedFP, ErrorClass: domain.WorkflowErrorVerifyWorkspaceChanged}
	raw, _ := json.Marshal(result)
	verifyStepID := "wfs-verify"
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "cp-verify", WorkflowRunID: runID, WorkflowStepID: &verifyStepID, ProjectID: "p",
		RetryState: string(raw), DurablePhase: verifyResultPhase, PayloadVersion: verifyResultVersion,
		FingerprintBefore: approvedFP, CreatedAt: base.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "cp-stop", WorkflowRunID: runID, WorkflowStepID: &verifyStepID, ProjectID: "p",
		NextAction: "verify stopped: AO holds no record of the commit the approval was read at",
		// The exact reason a pre-reconstruction daemon would have written.
		DurablePhase: ReasonVerifyApprovedHeadUnprovable, PayloadVersion: "v1",
		RetryState: "{}", CreatedAt: base.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// The tree as it stands: the SAME commit the approval was read at, with the
	// fix worker's further uncommitted output on top.
	ws := &staticWorkspace{obs: ports.WorkspaceObservation{
		Path: worktree, Branch: "feat/x", HeadSHA: head, Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "still-landing.go", Status: "??"}},
	}}
	c := New(Deps{Store: st, Projects: st, WorkspaceFacts: ws, SessionFacts: st,
		CommitHistory: &fakeCommitHistory{commits: []string{head, "aaaa1111"}},
		Clock:         func() time.Time { return base.Add(time.Hour) }})
	return &legacyProvenanceFixture{c: c, store: st, ctx: ctx, runID: runID, head: head}
}
