package workflow_test

// Three properties of the evidence snapshot that are not about any one field.
//
//	the ceiling binds the ARTIFACT   len(JSON()) <= MaxBytes, always
//	evidence is STEP-scoped          never another step's session/tree/owner
//	session identity is DURABLE      review and fix steps have one too
//
// Each of the three was a real defect. The ceiling used to count value+note
// lengths and ignore keys, labels, statuses, section titles, ids, the digest
// and JSON's own punctuation — over half the row — so a "5 KB" snapshot was
// really about 12 KB. The step scoping used to fall back to the run, which
// answers a question about the work step with the fix step's tree. And session
// identity was read off workflow_steps.session_id, a column only work dispatch
// ever writes, so review and fix steps reported "no session" while their
// session id sat on their own checkpoints.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- 1. the serialized ceiling ---------------------------------------------

// Maximal everything: an objective, a changed-file list, a provenance reason, a
// plan and a ledger all far past their caps. The row that comes out must still
// be valid JSON, must be within the ceiling, and must say what it dropped.
func TestSnapshotSerializedLengthIsBoundedWithMaximalFields(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedDurableFacts()
	f.seedMaximalFields()
	f.seedMaximalChangedFiles()

	snap := f.collect()
	raw := snap.JSON()

	if !json.Valid([]byte(raw)) {
		t.Fatal("the budgeted snapshot is not valid JSON")
	}
	if len(raw) > snap.MaxBytes {
		t.Fatalf("serialized snapshot is %d bytes, over its own %d-byte ceiling", len(raw), snap.MaxBytes)
	}
	// Bytes must be the real length of this very value, not a tally of parts.
	if snap.Bytes != len(raw) {
		t.Fatalf("snapshot.Bytes = %d but len(JSON()) = %d: the accounting describes something "+
			"other than the artifact", snap.Bytes, len(raw))
	}
	if snap.DroppedForBudgetCount == 0 {
		t.Fatal("maximal fields did not trip the budget at all; the ceiling is not binding")
	}
	if len(snap.DroppedForBudget) > 12 {
		t.Fatalf("the dropped-field list is itself unbounded (%d entries)", len(snap.DroppedForBudget))
	}
	if snap.DroppedForBudgetCount < len(snap.DroppedForBudget) {
		t.Fatal("the dropped count under-reports the dropped list")
	}
	// Useful evidence survives: the identity of what this snapshot is even
	// about is the last thing to go.
	if _, ok := snap.Field("workflow.runId"); !ok {
		t.Error("the budget dropped the run identity, which makes the whole snapshot unattributable")
	}
	if _, ok := snap.Field("step.id"); !ok {
		t.Error("the budget dropped the step identity")
	}
	// And it round-trips.
	decoded, ok := workflowcore.DecodeWorkerEvidenceSnapshot(raw)
	if !ok {
		t.Fatal("the budgeted snapshot does not decode")
	}
	if decoded.RunID != snap.RunID || decoded.Digest != snap.Digest {
		t.Fatal("the budgeted snapshot does not round-trip faithfully")
	}
}

// The same bound must hold for the row the gate actually writes to the ledger,
// which is the copy anybody reads after a restart.
func TestRecordedAmbiguityEvidenceRowIsWithinTheSerializedCeiling(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedDurableFacts()
	f.seedMaximalFields()
	f.driveToAmbiguity()

	cp, ok := f.checkpointWithPhase(workflowcore.AmbiguousWorkerStateEvidencePhase)
	if !ok {
		t.Fatal("no evidence row was written")
	}
	if !json.Valid([]byte(cp.RetryState)) {
		t.Fatal("the recorded evidence row is not valid JSON")
	}
	snap, ok := workflowcore.DecodeWorkerEvidenceSnapshot(cp.RetryState)
	if !ok {
		t.Fatalf("the recorded evidence row does not decode: %.200q", cp.RetryState)
	}
	if len(cp.RetryState) > snap.MaxBytes {
		t.Fatalf("the recorded evidence row is %d bytes, over the %d-byte ceiling",
			len(cp.RetryState), snap.MaxBytes)
	}
}

// An ordinary snapshot must NOT be mangled by the budget. A ceiling that cuts
// every normal case is not a tighter bound, it is a broken one.
func TestAnOrdinarySnapshotIsNotTrimmedByTheBudget(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedFullEvidence()

	snap := f.collect()
	if snap.DroppedForBudgetCount != 0 {
		t.Fatalf("an ordinary snapshot lost %d fields to the budget: %v",
			snap.DroppedForBudgetCount, snap.DroppedForBudget)
	}
	if len(snap.JSON()) > snap.MaxBytes {
		t.Fatalf("serialized snapshot is %d bytes, over %d", len(snap.JSON()), snap.MaxBytes)
	}
}

// Collecting twice over unchanged rows must produce the same bytes. A budget
// that resolved differently run to run would make the digest — and therefore
// the tie between a diagnosis and its evidence — meaningless.
func TestSnapshotBudgetingIsDeterministic(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedDurableFacts()
	f.seedMaximalFields()
	f.seedMaximalChangedFiles()

	first := f.collect()
	second := f.collect()
	if first.Digest != second.Digest {
		t.Fatal("two collections over the same rows produced different digests")
	}
	if len(first.JSON()) != len(second.JSON()) {
		t.Fatalf("serialized lengths differ across collections: %d vs %d",
			len(first.JSON()), len(second.JSON()))
	}
	if first.DroppedForBudgetCount != second.DroppedForBudgetCount {
		t.Fatal("the budget dropped a different number of fields the second time")
	}
}

// ---- 2. cross-step isolation ------------------------------------------------

// A run has several steps, each with its own session, its own worktree and its
// own observations. A snapshot about one of them must contain nothing from
// another, in any field.
func TestEvidenceNeverBorrowsFromAnotherStep(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedDurableFacts()
	// The rival's rows are the NEWEST on the run, and the work step has no
	// observation of its own. That is exactly the shape in which a run-scoped
	// read silently answers with the wrong step's evidence — and it is why this
	// case does not drive the production path first: an observation of its own
	// would satisfy the lookup before it ever reached the rival.
	f.clk.Advance(time.Hour)
	rival := f.seedRivalStep()

	snap := f.collect()
	rendered := snap.Render()
	for _, leaked := range []struct{ what, value string }{
		{"rival session", rival.sessionID},
		{"rival worktree", rival.worktree},
		{"rival branch", rival.branch},
		{"rival HEAD", rival.headSHA},
		{"rival fingerprint", rival.fingerprint},
		{"rival changed file", rival.changedFile},
		{"rival provenance harness", rival.harness},
		{"rival provenance reason", rival.reason},
	} {
		if strings.Contains(rendered, leaked.value) {
			t.Errorf("the work step's snapshot contains the %s (%q): evidence was borrowed "+
				"from another step\n%s", leaked.what, leaked.value, rendered)
		}
	}

	// And the work step's own facts are all still there — scoping must not have
	// been achieved by reporting nothing.
	for _, want := range []string{"sess-evidence", "/ws/evidence", "ao/wf-evidence", "base-sha-1"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the work step's snapshot lost its own fact %q:\n%s", want, rendered)
		}
	}

	// With no observation of its own, the fields that can only come from one
	// must say so rather than borrow the neighbour's.
	for _, key := range []string{"liveness.runtime", "git.status", "fingerprints.observed"} {
		field, ok := snap.Field(key)
		if !ok {
			t.Fatalf("no %s field", key)
		}
		if field.Status != workflowcore.EvidenceUnavailable {
			t.Errorf("%s = %q (%s), want unavailable: this step has no observation of its own",
				key, field.Value, field.Status)
		}
	}
	// The step-scoped fingerprints must be this step's, never the rival's.
	for _, key := range []string{"fingerprints.addressed", "fingerprints.recorded"} {
		field, ok := snap.Field(key)
		if !ok {
			t.Fatalf("no %s field", key)
		}
		if strings.Contains(field.Value, rival.fingerprint) {
			t.Errorf("%s = %q: the neighbouring step's fingerprint was reported as this step's",
				key, field.Value)
		}
	}
}

// The rival step's own provenance must not be reported as this step's. With no
// step-scoped record of its own, the answer is `unattributed` — never the
// neighbour's real, correct, and completely irrelevant owner.
func TestProvenanceIsUnattributedRatherThanBorrowedFromAnotherStep(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedDurableFacts()
	f.clk.Advance(time.Hour)
	rival := f.seedRivalStep()
	// This step has none of its own; only the rival does — the run-scoped read
	// would find a real, correct record and attach it to the wrong step.
	f.store.mutationProvenance = f.store.mutationProvenance[:0]
	f.seedRivalProvenanceOnly(rival)

	snap := f.collect()
	owner, ok := snap.Field("provenance.owner")
	if !ok {
		t.Fatal("no provenance.owner field")
	}
	if owner.Status != workflowcore.EvidenceUnattributed {
		t.Fatalf("provenance.owner status = %q, want unattributed; value = %q",
			owner.Status, owner.Value)
	}
	if strings.Contains(snap.Render(), rival.harness) || strings.Contains(snap.Render(), rival.reason) {
		t.Error("the neighbouring step's provenance was reported as this step's owner")
	}
}

// A legacy observation row with no step id at all may only be used where it
// cannot mislead: when it names the very session this step ran under.
func TestLegacyRunScopedObservationIsOnlyUsedWhenProvablyApplicable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rowSessionID  string
		wantAvailable bool
	}{
		{"names this step's session", "sess-evidence", true},
		{"names another session", "sess-somebody-else", false},
		{"names no session at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvidenceFixture(t)
			f.seedDurableFacts()
			f.seedLegacyRunScopedObservation(tc.rowSessionID)

			snap := f.collect()
			liveness, ok := snap.Field("liveness.runtime")
			if !ok {
				t.Fatal("no liveness.runtime field")
			}
			got := liveness.Status == workflowcore.EvidenceObserved
			if got != tc.wantAvailable {
				t.Fatalf("liveness observed = %v, want %v (status %q, note %q)",
					got, tc.wantAvailable, liveness.Status, liveness.Note)
			}
		})
	}
}

// ---- 3. review / fix durable session identity -------------------------------

// The production review path: a review that goes stale raises an ambiguity, and
// that raise must record an observation for the session the review ACTUALLY
// ran under — which lives on the review step's own checkpoints, because
// workflow_steps.session_id is only ever written by work dispatch.
func TestReviewAmbiguityPersistsEvidenceForItsDurableSession(t *testing.T) {
	f := newReviewEvidenceFixture(t)
	reviewerSession := f.dispatchReview()

	// Nothing on the step row — which is exactly why reading only the column
	// probed an empty session and recorded nothing at all.
	if id := f.reviewStep().SessionID; id != nil && *id != "" {
		t.Fatalf("fixture precondition changed: the review step now carries session %q", *id)
	}

	f.clk.Advance(31 * time.Minute)
	if _, err := f.c.GetRun(context.Background(), f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	cp, ok := f.checkpointForStepWithPhase(f.reviewStepID, workflowcore.WorkerObservationEvidencePhase)
	if !ok {
		t.Fatal("the review ambiguity recorded no observation for its durable session")
	}
	facts, ok := workflowcore.DecodeObservedWorkerFacts(cp.RetryState)
	if !ok {
		t.Fatalf("the recorded observation does not decode: %q", cp.RetryState)
	}
	if facts.SessionID != reviewerSession {
		t.Fatalf("recorded observation is for session %q, want the reviewer's %q",
			facts.SessionID, reviewerSession)
	}
	if !facts.LivenessKnown {
		t.Error("the review ambiguity recorded no liveness answer, so the probe was never reached")
	}
}

// And the collector resolves that identity back out of the ledger — including
// from a coordinator that never took any of the readings, which is what a
// restart is.
func TestReviewSessionIdentityIsReconstructedAfterRestart(t *testing.T) {
	f := newReviewEvidenceFixture(t)
	reviewerSession := f.dispatchReview()
	f.clk.Advance(31 * time.Minute)
	if _, err := f.c.GetRun(context.Background(), f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	restarted := f.restarted()
	run, _, err := f.store.GetWorkflowRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	snap := restarted.CollectWorkerEvidence(context.Background(), workflowcore.EvidenceRequest{
		Run: run, Step: f.reviewStep(),
	})

	if snap.SessionID != reviewerSession {
		t.Fatalf("snapshot session = %q, want the reviewer's %q recovered from the ledger",
			snap.SessionID, reviewerSession)
	}
	step, ok := snap.Field("step.sessionId")
	if !ok {
		t.Fatal("no step.sessionId field")
	}
	if !strings.Contains(step.Value, reviewerSession) {
		t.Fatalf("step.sessionId = %q, want the reviewer session", step.Value)
	}
	if !strings.Contains(step.Value, "recovered from this step's own checkpoints") {
		t.Errorf("step.sessionId = %q, want it to say where the identity came from", step.Value)
	}
	liveness, ok := snap.Field("liveness.runtime")
	if !ok {
		t.Fatal("no liveness.runtime field")
	}
	if liveness.Status != workflowcore.EvidenceObserved {
		t.Fatalf("liveness after restart = %q (%s), want the persisted answer",
			liveness.Status, liveness.Note)
	}
	// The restarted coordinator has no workspace or liveness port at all, so
	// anything it can still show came out of the ledger.
	if _, _, err := restarted.IncidentPackFor(context.Background(), f.runID); err != nil {
		t.Fatalf("IncidentPackFor after restart: %v", err)
	}
}

// A session id is never recovered from a NEIGHBOURING step's checkpoints.
func TestSessionIdentityIsNeverRecoveredFromAnotherStep(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedDurableFacts()
	rival := f.seedRivalStep()

	run, _, err := f.store.GetWorkflowRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	// A third step with no session anywhere: not on its row, not on its own
	// checkpoints. The rival's is right there in the same run and must not be
	// borrowed.
	orphan := f.stepOfKind(domain.WorkflowStepVerify)
	snap := f.c.CollectWorkerEvidence(context.Background(), workflowcore.EvidenceRequest{
		Run: run, Step: orphan,
	})
	if snap.SessionID != "" {
		t.Fatalf("a step with no session anywhere resolved to %q", snap.SessionID)
	}
	if strings.Contains(snap.Render(), rival.sessionID) {
		t.Errorf("a step with no session borrowed the neighbouring step's:\n%s", snap.Render())
	}
	field, ok := snap.Field("step.sessionId")
	if !ok {
		t.Fatal("no step.sessionId field")
	}
	if !strings.Contains(field.Value, "none recorded") {
		t.Errorf("step.sessionId = %q, want an explicit absence", field.Value)
	}
}

// ---- fixtures ---------------------------------------------------------------

// rivalStep is a second step in the same run, with its own everything.
type rivalStep struct {
	stepID      string
	sessionID   string
	worktree    string
	branch      string
	headSHA     string
	fingerprint string
	changedFile string
	harness     string
	reason      string
}

func (f *evidenceFixture) stepOfKind(kind domain.WorkflowStepKind) domain.WorkflowStep {
	f.t.Helper()
	steps, err := f.store.ListWorkflowSteps(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind == kind {
			return s
		}
	}
	f.t.Fatalf("no %s step", kind)
	return domain.WorkflowStep{}
}

// seedRivalStep gives the review step a complete, distinctive world of its own.
func (f *evidenceFixture) seedRivalStep() rivalStep {
	f.t.Helper()
	review := f.stepOfKind(domain.WorkflowStepReview)
	r := rivalStep{
		stepID:      review.ID,
		sessionID:   "sess-RIVAL",
		worktree:    "/ws/RIVAL",
		branch:      "ao/RIVAL",
		headSHA:     "RIVALHEAD",
		fingerprint: "fpRIVAL",
		changedFile: "RIVALFILE.go",
		harness:     "codex-RIVAL",
		reason:      "RIVALREASON",
	}
	stepID := r.stepID
	sid := r.sessionID
	at := f.clk.Now()

	f.storeCheckpoint(domain.WorkflowCheckpoint{
		ID: "wfc-rival-dispatch", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-evidence", SessionID: &sid,
		Branch: r.branch, WorktreePath: r.worktree, BaseSHA: r.headSHA, HeadSHA: r.headSHA,
		FingerprintBefore: r.fingerprint, FingerprintAfter: r.fingerprint,
		DurablePhase: "review_dispatched", PayloadVersion: "v1", RetryState: "{}", CreatedAt: at,
	})
	observation := workflowcore.ObservedWorkerFacts{
		ObservedAt: at, SessionID: r.sessionID, WorkspaceKnown: true,
		WorktreePath: r.worktree, Branch: r.branch, HeadSHA: r.headSHA,
		Dirty: true, ChangedFiles: []string{" M " + r.changedFile}, Fingerprint: r.fingerprint,
		LivenessKnown: true, LivenessAlive: false,
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		f.t.Fatalf("marshal rival observation: %v", err)
	}
	f.storeCheckpoint(domain.WorkflowCheckpoint{
		ID: "wfc-rival-observation", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-evidence", SessionID: &sid,
		Branch: r.branch, WorktreePath: r.worktree,
		DurablePhase:   workflowcore.WorkerObservationEvidencePhase,
		PayloadVersion: "v1", RetryState: string(payload), CreatedAt: at,
	})
	f.seedRivalProvenanceOnly(r)
	return r
}

func (f *evidenceFixture) seedRivalProvenanceOnly(r rivalStep) {
	stepID := r.stepID
	sid := r.sessionID
	f.store.mutationProvenance = append(f.store.mutationProvenance, domain.WorkflowMutationProvenance{
		ID: "wfp-rival", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		Class: domain.MutationAuthorizedFix, Harness: r.harness, SessionID: &sid,
		Branch: r.branch, WorktreePath: r.worktree,
		BaseSHA: r.headSHA, HeadSHA: r.headSHA,
		FingerprintBefore: r.fingerprint, FingerprintAfter: r.fingerprint,
		Reason: r.reason, EvidenceJSON: "{}", CreatedAt: f.clk.Now(),
	})
}

// seedLegacyRunScopedObservation writes an observation row with NO step id, as
// a build that predated step scoping would have.
func (f *evidenceFixture) seedLegacyRunScopedObservation(sessionID string) {
	f.t.Helper()
	observation := workflowcore.ObservedWorkerFacts{
		ObservedAt: f.clk.Now(), SessionID: sessionID, WorkspaceKnown: true,
		WorktreePath: "/ws/evidence", Branch: "ao/wf-evidence", HeadSHA: "base-sha-1",
		Fingerprint: "fp-legacy", LivenessKnown: true, LivenessAlive: true,
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		f.t.Fatalf("marshal legacy observation: %v", err)
	}
	f.storeCheckpoint(domain.WorkflowCheckpoint{
		ID: "wfc-legacy-observation", WorkflowRunID: f.runID, ProjectID: "proj-evidence",
		DurablePhase:   workflowcore.WorkerObservationEvidencePhase,
		PayloadVersion: "v1", RetryState: string(payload), CreatedAt: f.clk.Now(),
	})
}

// seedMaximalFields pushes every bounded source far past its cap.
//
// It deliberately leaves the observed TREE clean and at the dispatch base, so
// the ordinary work-step ambiguity still fires and callers that drive the
// production path get a maximal snapshot out of it. seedMaximalChangedFiles
// adds the huge change set for the collector-only cases, where a dirty tree
// would instead be read (correctly) as completed work.
func (f *evidenceFixture) seedMaximalFields() {
	f.t.Helper()
	huge := strings.Repeat("M", 4096)
	f.mutateRun(func(r *domain.WorkflowRun) { r.Objective = huge })
	f.mutateSteps(func(s *domain.WorkflowStep) {
		if s.Kind == domain.WorkflowStepWork {
			s.AssignedHarness = huge
			s.ExpectedArtifactsVersion = huge
		}
	})
	f.mutateAttempts(func(a *domain.WorkflowAttempt) {
		a.Harness = huge
		a.Model = huge
		a.ReviewTarget.Fingerprint = huge
		a.ReviewTarget.HeadSHA = huge
	})
	f.sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(f.sessionID), ProjectID: "proj-evidence", Kind: domain.KindWorker,
		Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI,
		Activity:      domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now()},
		FirstSignalAt: f.clk.Now(), TurnCompletedAt: f.clk.Now(),
		Metadata: domain.SessionMetadata{Branch: huge, WorkspacePath: "/ws/evidence"},
	})

	// The ledger: forty noisy checkpoints, each carrying long text, and the
	// workspace identity every checkpoint must carry forward.
	for i := range 40 {
		f.clk.Advance(time.Second)
		f.checkpoint(domain.WorkflowCheckpoint{
			DurablePhase: fmt.Sprintf("noise_phase_%02d_%s", i, huge[:48]),
			NextAction:   huge[:512],
			Branch:       huge, WorktreePath: huge, BaseSHA: "base-sha-1", HeadSHA: "base-sha-1",
			FingerprintBefore: huge, FingerprintAfter: huge,
		})
	}

	for i := range len(f.store.dispatchCheckpoints) {
		f.store.dispatchCheckpoints[i].Detail = huge
		f.store.dispatchCheckpoints[i].IdempotencyKey = huge
		f.store.dispatchCheckpoints[i].Harness = huge
	}
	for i := range len(f.store.mutationProvenance) {
		f.store.mutationProvenance[i].Reason = huge
		f.store.mutationProvenance[i].WorktreePath = huge
		f.store.mutationProvenance[i].Branch = huge
		f.store.mutationProvenance[i].Harness = huge
		f.store.mutationProvenance[i].BaseSHA = huge
		f.store.mutationProvenance[i].HeadSHA = huge
		f.store.mutationProvenance[i].FingerprintBefore = huge
		f.store.mutationProvenance[i].FingerprintAfter = huge
	}

	files := make([]workflowcore.VerificationFileCheck, 0, 60)
	cmds := make([]workflowcore.VerificationCommandCheck, 0, 60)
	for i := range 60 {
		files = append(files, workflowcore.VerificationFileCheck{
			Path: fmt.Sprintf("%03d/%s.go", i, huge[:96]), Exists: true,
		})
		cmds = append(cmds, workflowcore.VerificationCommandCheck{
			Command: "go", Args: []string{"test", fmt.Sprintf("./%03d/%s/...", i, huge[:96])},
		})
	}
	artifact := workflowcore.BuildPlanArtifact("proj-evidence", huge, "v1",
		workflowcore.VerificationPlan{Commands: cmds, Files: files})
	encoded, err := workflowcore.MarshalPlanArtifact(artifact)
	if err != nil {
		f.t.Fatalf("MarshalPlanArtifact: %v", err)
	}
	f.mutateSteps(func(s *domain.WorkflowStep) {
		if s.Kind == domain.WorkflowStepPlan {
			s.ArtifactJSON = encoded
		}
	})
}

// seedMaximalChangedFiles makes the observed tree enormous. Only for cases that
// do NOT drive the production path afterwards: a dirty tree is real work
// evidence, and observeWorkStep would complete the step instead of stopping it.
func (f *evidenceFixture) seedMaximalChangedFiles() {
	f.t.Helper()
	huge := strings.Repeat("M", 4096)
	changes := make([]ports.WorkspaceChange, 0, 80)
	for i := range 80 {
		changes = append(changes, ports.WorkspaceChange{
			Status: " M", Path: fmt.Sprintf("internal/very/deep/package/path/%03d/%s.go", i, huge[:64]),
		})
	}
	f.workspaceFacts.obs = ports.WorkspaceObservation{
		Path: huge, Branch: huge, HeadSHA: "base-sha-1", Dirty: true, Changes: changes,
	}
	observation := workflowcore.NewObservedWorkspaceFacts(f.workspaceFacts.obs)
	observation.ObservedAt = f.clk.Now()
	observation.SessionID = f.sessionID
	observation.LivenessKnown, observation.LivenessAlive = true, true
	payload, err := json.Marshal(observation)
	if err != nil {
		f.t.Fatalf("marshal maximal observation: %v", err)
	}
	stepID := f.workStepID
	f.storeCheckpoint(domain.WorkflowCheckpoint{
		ID: "wfc-maximal-observation", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID:      "proj-evidence",
		DurablePhase:   workflowcore.WorkerObservationEvidencePhase,
		PayloadVersion: "v1", RetryState: string(payload), CreatedAt: f.clk.Now(),
	})
}

func (f *evidenceFixture) mutateRun(mutate func(*domain.WorkflowRun)) {
	f.t.Helper()
	run, ok := f.store.runs[f.runID]
	if !ok {
		f.t.Fatalf("run %s is gone", f.runID)
	}
	mutate(&run)
	f.store.runs[f.runID] = run
}

func (f *evidenceFixture) storeCheckpoint(cp domain.WorkflowCheckpoint) {
	f.t.Helper()
	if _, err := f.store.CreateWorkflowCheckpoint(context.Background(), cp); err != nil {
		f.t.Fatalf("CreateWorkflowCheckpoint: %v", err)
	}
}

// ---- review fixture ---------------------------------------------------------

type reviewEvidenceFixture struct {
	t              *testing.T
	c              *workflowcore.Coordinator
	store          *fakeStore
	clk            *fakeClock
	sessionFacts   *fakeSessionFacts
	workspaceFacts *fakeWorkspaceFacts
	reviewRuns     *fakeReviewRuns
	liveness       *fakeWorkerLiveness
	runID          string
	reviewStepID   string
	idSeq          int
}

func newReviewEvidenceFixture(t *testing.T) *reviewEvidenceFixture {
	t.Helper()
	f := &reviewEvidenceFixture{
		t:              t,
		store:          newFakeStore(),
		clk:            &fakeClock{t: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)},
		sessionFacts:   newFakeSessionFacts(),
		workspaceFacts: &fakeWorkspaceFacts{},
		reviewRuns:     newFakeReviewRuns(),
		liveness:       &fakeWorkerLiveness{alive: true, known: true},
	}
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}},
		facts: f.sessionFacts,
	}
	f.c = workflowcore.New(workflowcore.Deps{
		Store:            f.store,
		Spawner:          spawner,
		SessionFacts:     f.sessionFacts,
		WorkspaceFacts:   f.workspaceFacts,
		ReviewRuns:       f.reviewRuns,
		ReviewerLauncher: &fakeReviewerLauncher{},
		WorkerLiveness:   f.liveness,
		Clock:            f.clk.Now,
		NewID: func() string {
			f.idSeq++
			return fmt.Sprintf("rv%d", f.idSeq)
		},
	})
	created, err := f.c.CreateRun(context.Background(), "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	return f
}

// dispatchReview drives work completion and the review dispatch, and returns
// the reviewer's session id — which review dispatch records on the review
// step's own checkpoint and never on the step row.
func (f *reviewEvidenceFixture) dispatchReview() string {
	f.t.Helper()
	completeWorkStep(f.t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.runID)
	got, err := f.c.ContinueRun(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	f.reviewStepID = review.Step.ID
	if review.Step.ReviewRunID == nil {
		f.t.Fatal("the review step has no review run: the dispatch did not happen")
	}
	rr, ok, err := f.reviewRuns.GetReviewRun(context.Background(), *review.Step.ReviewRunID)
	if err != nil || !ok {
		f.t.Fatalf("GetReviewRun: %v (found=%v)", err, ok)
	}
	reviewerSession := string(rr.SessionID)
	// A reviewer session that is alive and recently active, so the staleness
	// branch is reached rather than the capacity-stall one.
	f.sessionFacts.put(domain.SessionRecord{
		ID: rr.SessionID, ProjectID: "proj-1", Harness: domain.HarnessClaudeCode,
		Activity:      domain.Activity{State: domain.ActivityActive, LastActivityAt: f.clk.Now()},
		FirstSignalAt: f.clk.Now(),
		Metadata:      domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
	})
	return reviewerSession
}

func (f *reviewEvidenceFixture) reviewStep() domain.WorkflowStep {
	f.t.Helper()
	steps, err := f.store.ListWorkflowSteps(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.ID == f.reviewStepID {
			return s
		}
	}
	f.t.Fatalf("review step %s is gone", f.reviewStepID)
	return domain.WorkflowStep{}
}

func (f *reviewEvidenceFixture) checkpointForStepWithPhase(stepID, phase string) (domain.WorkflowCheckpoint, bool) {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase != phase || cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		if !found || !cp.CreatedAt.Before(newest.CreatedAt) {
			newest, found = cp, true
		}
	}
	return newest, found
}

// restarted is a coordinator over the same rows with no workspace and no
// liveness port at all: whatever it can still show came out of the ledger.
func (f *reviewEvidenceFixture) restarted() *workflowcore.Coordinator {
	f.t.Helper()
	var idSeq int
	return workflowcore.New(workflowcore.Deps{
		Store:        f.store,
		SessionFacts: f.sessionFacts,
		ReviewRuns:   f.reviewRuns,
		Clock:        f.clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("rst%d", idSeq)
		},
	})
}

// ---- oversized top-level identifiers ----------------------------------------

// The identifiers sit OUTSIDE Sections, so the budget pass — which drops fields
// and then whole sections — can never reach them. Before they were capped, a
// snapshot about a run with a huge id could drain every field and every section
// it had and still be over the ceiling, with nothing left to drop.
//
// The three shapes below are the ones that matter: plain bytes, multi-byte
// runes (a naive byte slice splits one and encoding/json rewrites the tail as
// U+FFFD, three bytes for one), and characters JSON must escape six-fold.
func TestSnapshotIsBoundedWithOversizedTopLevelIdentifiers(t *testing.T) {
	for _, tc := range []struct{ name, filler string }{
		{"ascii", "R"},
		{"two-byte runes", "é"},
		// Four bytes each: the kept prefix lands mid-rune, so a naive byte
		// slice would emit invalid UTF-8 here and nowhere else.
		{"four-byte runes", "😀"},
		{"json-escaped control characters", "\x00"},
		{"quotes and backslashes", `"\`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvidenceFixture(t)
			f.seedDurableFacts()
			ids := f.oversizedIdentifiers(tc.filler)

			snap := f.collectWithOversizedIdentifiers(ids)
			raw := snap.JSON()

			if !json.Valid([]byte(raw)) {
				t.Fatal("the snapshot is not valid JSON")
			}
			if len(raw) > snap.MaxBytes {
				t.Fatalf("serialized snapshot is %d bytes, over its own %d-byte ceiling",
					len(raw), snap.MaxBytes)
			}
			if snap.Bytes != len(raw) {
				t.Fatalf("snapshot.Bytes = %d but len(JSON()) = %d", snap.Bytes, len(raw))
			}
			if _, ok := workflowcore.DecodeWorkerEvidenceSnapshot(raw); !ok {
				t.Fatal("the snapshot does not decode")
			}

			// All FOUR top-level identifiers, each from its own source, each
			// independently oversized. The distinct prefixes are what prove
			// they are separately capped rather than one of them standing in
			// for the others.
			for _, id := range []struct{ name, got, want string }{
				{"RunID", snap.RunID, ids.run},
				{"StepID", snap.StepID, ids.step},
				{"AttemptID", snap.AttemptID, ids.attempt},
				{"SessionID", snap.SessionID, ids.session},
			} {
				if len(id.got) > 128 {
					t.Errorf("%s is %d bytes, over the identifier cap", id.name, len(id.got))
				}
				if !utf8.ValidString(id.got) {
					t.Errorf("%s is not valid UTF-8 after truncation: %q", id.name, id.got)
				}
				if !strings.Contains(id.got, "[truncated]") {
					t.Errorf("%s = %q, want it to disclose that it was capped", id.name, id.got)
				}
				// Useful identity is preserved, and it is THIS identifier's.
				prefix := strings.TrimSuffix(id.got, "…[truncated]")
				if prefix == "" || !strings.HasPrefix(id.want, prefix) {
					t.Errorf("%s = %q, want a genuine prefix of its own original id", id.name, id.got)
				}
			}
		})
	}
}

// Same input twice must produce the same bytes, or the digest that ties a
// diagnosis to its evidence means nothing.
func TestOversizedIdentifierTruncationIsDeterministic(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedDurableFacts()
	ids := f.oversizedIdentifiers("😀")

	first := f.collectWithOversizedIdentifiers(ids)
	second := f.collectWithOversizedIdentifiers(ids)
	if first.RunID != second.RunID || first.StepID != second.StepID ||
		first.AttemptID != second.AttemptID || first.SessionID != second.SessionID {
		t.Fatal("identifier truncation is not deterministic")
	}
	if first.Digest != second.Digest {
		t.Fatal("two collections over the same oversized input produced different digests")
	}
	if len(first.JSON()) != len(second.JSON()) {
		t.Fatalf("serialized lengths differ: %d vs %d", len(first.JSON()), len(second.JSON()))
	}
}

// And an ordinary snapshot must be untouched by any of this: real identifiers
// are nowhere near the cap, so they must come through exactly as they are.
func TestOrdinaryIdentifiersAreNotTruncated(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedFullEvidence()

	snap := f.collect()
	if snap.RunID != f.runID {
		t.Errorf("RunID = %q, want the untouched %q", snap.RunID, f.runID)
	}
	if snap.StepID != f.workStepID {
		t.Errorf("StepID = %q, want the untouched %q", snap.StepID, f.workStepID)
	}
	if snap.SessionID != f.sessionID {
		t.Errorf("SessionID = %q, want the untouched %q", snap.SessionID, f.sessionID)
	}
	if snap.AttemptID != "wfa-evidence" {
		t.Errorf("AttemptID = %q, want the untouched attempt id", snap.AttemptID)
	}
	if strings.Contains(snap.JSON(), "[truncated]") {
		t.Error("an ordinary snapshot reports a truncation that did not happen")
	}
}

// oversizedIDs are four independently oversized top-level identifiers. Each
// carries its own short ASCII prefix so an assertion can tell them apart: a cap
// that quietly used one identifier's value for another would still be "within
// the ceiling", and would still be wrong.
type oversizedIDs struct {
	run     string
	step    string
	attempt string
	session string
}

func (f *evidenceFixture) oversizedIdentifiers(filler string) oversizedIDs {
	f.t.Helper()
	// 40 KB each, far past both the identifier cap and the whole-snapshot
	// ceiling, so nothing here can pass by being accidentally small.
	body := strings.Repeat(filler, 40*1024)
	return oversizedIDs{
		run:     "RUN-" + body,
		step:    "STEP-" + body,
		attempt: "ATT-" + body,
		session: "SESS-" + body,
	}
}

// collectWithOversizedIdentifiers seeds an attempt under the oversized step so
// the ATTEMPT id is resolved the way production resolves it — through
// GetLatestWorkflowAttempt — rather than being handed in directly. Without the
// seeded row the lookup simply misses and AttemptID comes back empty, which is
// how it escaped these assertions in the first place.
func (f *evidenceFixture) collectWithOversizedIdentifiers(ids oversizedIDs) workflowcore.WorkerEvidenceSnapshot {
	f.t.Helper()
	f.store.attempts[ids.step] = []domain.WorkflowAttempt{{
		ID:             ids.attempt,
		WorkflowStepID: ids.step,
		AttemptNumber:  1,
		Harness:        ids.attempt,
		Model:          ids.attempt,
		StartedAt:      f.clk.Now(),
	}}
	sessionID := ids.session
	return f.c.CollectWorkerEvidence(context.Background(), workflowcore.EvidenceRequest{
		Run: domain.WorkflowRun{
			ID: ids.run, ProjectID: ids.run, Objective: ids.run,
			State: domain.WorkflowRunNeedsAttention,
		},
		Step: domain.WorkflowStep{
			ID: ids.step, Kind: domain.WorkflowStepWork, State: domain.WorkflowStepWaiting,
			AssignedHarness: ids.step, SessionID: &sessionID, ExpectedArtifactsVersion: ids.step,
		},
	})
}
