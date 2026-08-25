package workflow_test

// The bounded evidence snapshot, and the two claims the Incident Advisor makes
// with it.
//
// What the old ambiguous stops looked like, in full:
//
//	worker idle with no verifiable change — needs human review
//
// AO held, at that instant: the run and step and attempt states, the session
// row and its activity, a runtime liveness answer, the dispatch boundary the
// worker was launched at, the branch and HEAD and porcelain status, both
// workspace fingerprints, the plan's expected artifacts, the last observed
// worker progress, the run's whole ledger and its parent. None of it travelled
// with the stop, so "¿Qué hago?" had a conclusion to show a person and nothing
// under it.
//
// Two tests, one for each half of the fix:
//
//	fully-available evidence  -> the Advisor renders every field, none missing
//	absent provenance         -> the Advisor says UNATTRIBUTED, and names nobody

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- migration 0133's reads, on the fake store ------------------------------

func (f *fakeStore) ListWorkflowDispatchCheckpointsByStep(_ context.Context, stepID string) ([]domain.WorkflowDispatchCheckpoint, error) {
	var out []domain.WorkflowDispatchCheckpoint
	for _, cp := range f.dispatchCheckpoints {
		if cp.WorkflowStepID != nil && *cp.WorkflowStepID == stepID {
			out = append(out, cp)
		}
	}
	return out, nil
}

func (f *fakeStore) ListWorkflowMutationProvenanceByStep(_ context.Context, stepID string) ([]domain.WorkflowMutationProvenance, error) {
	var out []domain.WorkflowMutationProvenance
	for _, p := range f.mutationProvenance {
		if p.WorkflowStepID != nil && *p.WorkflowStepID == stepID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) ListWorkflowMutationProvenanceByRun(_ context.Context, runID string) ([]domain.WorkflowMutationProvenance, error) {
	var out []domain.WorkflowMutationProvenance
	for _, p := range f.mutationProvenance {
		if p.WorkflowRunID == runID {
			out = append(out, p)
		}
	}
	return out, nil
}

// fakeWorkerLiveness answers the optional runtime probe. `known` false models a
// probe that could not tell, which AO must never read as death.
type fakeWorkerLiveness struct {
	alive bool
	known bool
	err   error
}

func (f *fakeWorkerLiveness) SessionAlive(_ context.Context, _ domain.SessionID) (bool, bool, error) {
	return f.alive, f.known, f.err
}

// ---- the two tests ----------------------------------------------------------

// With every durable fact available, the Advisor's pack must carry all of them
// and report none of them missing. The list below is the objective's own list:
// workflow/step/attempt state, session lifecycle, process liveness, harness
// launch/exit, branch/HEAD, git status, fingerprints, mutation provenance,
// expected artifacts, worker result, recent checkpoints, parent/child.
func TestIncidentAdvisorRendersEveryAvailableEvidenceField(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedFullEvidence()

	_, pack, err := f.c.IncidentPackFor(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("IncidentPackFor: %v", err)
	}
	rendered := pack.Render()

	section := packSection(t, pack, "Durable evidence snapshot (bounded)")
	if section.Dropped {
		t.Fatal("the evidence snapshot was dropped from the pack: it is priority 2 and must never be")
	}
	if section.Truncated {
		t.Fatalf("the evidence snapshot was truncated inside the pack (%d bytes)", len(section.Body))
	}

	// Nothing may be reported as missing when everything is obtainable.
	if strings.Contains(section.Body, "NOT AVAILABLE") {
		t.Fatalf("the Advisor reported obtainable evidence as missing:\n%s", section.Body)
	}

	// And every fact the objective names must actually be on the page.
	for _, want := range []struct{ what, value string }{
		{"run id", f.runID},
		{"run state", string(domain.WorkflowRunNeedsAttention)},
		{"step kind", string(domain.WorkflowStepWork)},
		{"attempt harness", "claude-code"},
		{"attempt deadline", "2026-08-22T13:00:00Z"},
		{"attempt review target", "fp-reviewed"},
		{"session activity", string(domain.ActivityIdle)},
		{"session first signal", "2026-08-22T12:01:00Z"},
		{"process liveness", "alive (probed"},
		{"harness launch phase", string(domain.DispatchPhaseWorkerDispatched)},
		{"harness launch outcome", string(domain.LaunchOutcomeDispatched)},
		{"git branch", "ao/wf-evidence"},
		{"git HEAD", "base-sha-1"},
		{"git base", "base-sha-1"},
		{"git status", "dirty=false staged=false untracked=false changes=0"},
		{"changed files", "none — the observed tree had no reported change"},
		{"observed fingerprint", "observed fingerprint: "},
		{"recorded fingerprint", "fp-reviewed"},
		{"mutation provenance class", string(domain.MutationAuthorizedWork)},
		{"mutation provenance owner", "sess-evidence"},
		{"expected artifacts", "go build ./..."},
		{"worker result", "worker_observed_worker_idle"},
		{"recent checkpoints", "worker_dispatched"},
		{"parent/child relationship", "none — this is a top-level run"},
	} {
		if !strings.Contains(section.Body, want.value) {
			t.Errorf("the evidence snapshot is missing the %s (%q):\n%s", want.what, want.value, section.Body)
		}
	}

	// The pack a Diagnostic Agent actually receives carries it too.
	if !strings.Contains(rendered, "Durable evidence snapshot") {
		t.Fatalf("the rendered pack does not include the evidence snapshot:\n%s", rendered)
	}
	if pack.Bytes > pack.MaxBytes {
		t.Fatalf("pack is %d bytes, over its own %d-byte budget", pack.Bytes, pack.MaxBytes)
	}

	// And the collector itself agrees: nothing is unavailable.
	snap := f.collect()
	if missing := snap.UnavailableKeys(); len(missing) != 0 {
		t.Fatalf("evidence AO can obtain was reported unavailable: %v", missing)
	}
	if unattributed := snap.UnattributedKeys(); len(unattributed) != 0 {
		t.Fatalf("evidence AO holds provenance for was reported unattributed: %v", unattributed)
	}
	if snap.Bytes > snap.MaxBytes {
		t.Fatalf("snapshot is %d bytes, over its own %d-byte budget", snap.Bytes, snap.MaxBytes)
	}
}

// With no provenance recorded, the Advisor must say so — and must not hand the
// change to the only plausible candidate it can see. The whole point of the
// field is that "this run's own worker probably did it" is exactly the
// attribution that sends a person to revert the wrong work.
func TestIncidentAdvisorReportsAbsentProvenanceAsUnattributed(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedFullEvidence()
	// The one difference from the test above: migration 0133's provenance table
	// holds nothing for this run, exactly as it does for every run that
	// predates it.
	f.store.mutationProvenance = nil

	_, pack, err := f.c.IncidentPackFor(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("IncidentPackFor: %v", err)
	}
	section := packSection(t, pack, "Durable evidence snapshot (bounded)")

	if !strings.Contains(section.Body, "UNATTRIBUTED") {
		t.Fatalf("absent provenance was not reported as unattributed:\n%s", section.Body)
	}
	if !strings.Contains(section.Body, "will not guess") {
		t.Fatalf("the unattributed field does not say AO is refusing to guess:\n%s", section.Body)
	}

	// Nobody is named. The session, the harness and the mutation class are all
	// still in the pack elsewhere (they are facts about the SESSION), but the
	// attribution lines themselves must carry no owner.
	for _, line := range strings.Split(section.Body, "\n") {
		if !strings.Contains(line, "UNATTRIBUTED") {
			continue
		}
		for _, fabricated := range []string{"sess-evidence", "claude-code", string(domain.MutationAuthorizedWork)} {
			if strings.Contains(line, fabricated) {
				t.Errorf("an unattributed field named %q as the owner: %s", fabricated, line)
			}
		}
	}

	snap := f.collect()
	unattributed := snap.UnattributedKeys()
	if len(unattributed) == 0 {
		t.Fatal("absent provenance produced no unattributed field at all")
	}
	for _, key := range unattributed {
		field, ok := snap.Field(key)
		if !ok {
			t.Fatalf("UnattributedKeys named %q, which is not in the snapshot", key)
		}
		if field.Value != "" {
			t.Errorf("unattributed field %q carries a value %q: AO invented an attribution", key, field.Value)
		}
	}
	owner, ok := snap.Field("provenance.owner")
	if !ok {
		t.Fatal("the snapshot has no provenance.owner field")
	}
	if owner.Status != workflowcore.EvidenceUnattributed {
		t.Fatalf("provenance.owner status = %q, want %q", owner.Status, workflowcore.EvidenceUnattributed)
	}
	// Crucially, NOT "unavailable": AO can read the provenance table perfectly
	// well. It read it, and it is empty. Those are different answers and the
	// remedies for them are different too.
	if owner.Status == workflowcore.EvidenceUnavailable {
		t.Fatal("an empty provenance table was reported as an unreadable one")
	}
}

// The ambiguity gate, end to end over durable state: a work step that goes idle
// with nothing to show cannot reach ambiguous_worker_state without the snapshot
// landing on the ledger first.
func TestAnAmbiguousWorkerStopAlwaysCarriesItsEvidence(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedFullEvidence()

	attempts, err := f.store.ListWorkflowAttempts(context.Background(), f.workStepID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	raised := false
	for _, a := range attempts {
		if a.ErrorClass == domain.WorkflowErrorAmbiguousWorkerState {
			raised = true
		}
	}
	if !raised {
		t.Fatal("the idle-with-nothing-to-show worker did not reach ambiguous_worker_state at all")
	}

	cps, err := f.store.ListWorkflowCheckpoints(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var evidence domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == workflowcore.AmbiguousWorkerStateEvidencePhase {
			evidence, found = cp, true
		}
	}
	if !found {
		t.Fatal("ambiguous_worker_state was raised with no evidence snapshot recorded next to it")
	}
	snap, ok := workflowcore.DecodeWorkerEvidenceSnapshot(evidence.RetryState)
	if !ok {
		t.Fatalf("the recorded evidence did not decode: %q", evidence.RetryState)
	}
	if snap.RunID != f.runID || snap.StepID != f.workStepID {
		t.Fatalf("the recorded evidence is about run %q step %q, want %q / %q",
			snap.RunID, snap.StepID, f.runID, f.workStepID)
	}
	if len(snap.Fields()) == 0 {
		t.Fatal("the recorded evidence snapshot is empty")
	}
}

// ---- fixture ----------------------------------------------------------------

type evidenceFixture struct {
	t              *testing.T
	c              *workflowcore.Coordinator
	store          *fakeStore
	clk            *fakeClock
	sessionFacts   *fakeSessionFacts
	workspaceFacts *fakeWorkspaceFacts
	liveness       *fakeWorkerLiveness

	runID      string
	workStepID string
	sessionID  string
}

func newEvidenceFixture(t *testing.T) *evidenceFixture {
	t.Helper()
	f := &evidenceFixture{
		t:              t,
		store:          newFakeStore(),
		clk:            &fakeClock{t: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)},
		sessionFacts:   newFakeSessionFacts(),
		workspaceFacts: &fakeWorkspaceFacts{},
		liveness:       &fakeWorkerLiveness{alive: true, known: true},
		sessionID:      "sess-evidence",
	}
	var idSeq int
	f.c = workflowcore.New(workflowcore.Deps{
		Store:          f.store,
		SessionFacts:   f.sessionFacts,
		WorkspaceFacts: f.workspaceFacts,
		WorkerLiveness: f.liveness,
		Clock:          f.clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("ev%d", idSeq)
		},
	})
	created, err := f.c.CreateRun(context.Background(), "proj-evidence", "ship the evidence collector")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	for _, sd := range created.Steps {
		if sd.Step.Kind == domain.WorkflowStepWork {
			f.workStepID = sd.Step.ID
		}
	}
	return f
}

// seedFullEvidence writes every durable fact AO could possibly hold about one
// stopped work step. The states are set directly rather than driven through the
// dispatch path, because what is under test is the READ, and driving it would
// make the test depend on a dozen behaviours it is not about.
func (f *evidenceFixture) seedFullEvidence() {
	f.t.Helper()
	f.seedDurableFacts()
	f.driveToAmbiguity()
}

// seedDurableFacts writes every durable fact AO could hold about one work step,
// and stops short of the stop itself.
func (f *evidenceFixture) seedDurableFacts() {
	f.t.Helper()
	ctx := context.Background()

	// Every step but the work step is left pending, so the Advisor's focus step
	// is unambiguously the one this evidence is about.
	sessionID := f.sessionID
	f.mutateSteps(func(s *domain.WorkflowStep) {
		switch s.Kind {
		case domain.WorkflowStepWork:
			s.State = domain.WorkflowStepRunning
			s.AssignedHarness = "claude-code"
			s.SessionID = &sessionID
			s.ExpectedArtifactsVersion = "artifacts-v1"
		case domain.WorkflowStepPlan:
			s.State = domain.WorkflowStepCompleted
		default:
			s.State = domain.WorkflowStepPending
		}
	})

	// The plan's expected artifacts.
	artifact := workflowcore.BuildPlanArtifact("proj-evidence", "ship the evidence collector", "v1",
		workflowcore.VerificationPlan{
			Commands: []workflowcore.VerificationCommandCheck{
				{Command: "go", Args: []string{"build", "./..."}, RequiredExitCode: 0},
			},
			Files: []workflowcore.VerificationFileCheck{
				{Path: "internal/workflow/evidence_snapshot.go", Exists: true},
			},
		})
	encoded, err := workflowcore.MarshalPlanArtifact(artifact)
	if err != nil {
		f.t.Fatalf("MarshalPlanArtifact: %v", err)
	}
	f.mutateSteps(func(s *domain.WorkflowStep) {
		if s.Kind == domain.WorkflowStepPlan {
			s.ArtifactJSON = encoded
		}
	})

	// The attempt, complete with migration 0133's deadline and review target.
	deadline := f.clk.Now().Add(time.Hour)
	reviewRunID := "rr-evidence"
	if _, err := f.store.CreateWorkflowAttempt(ctx, "wfa-evidence", f.workStepID, "claude-code", "sonnet", f.clk.Now()); err != nil {
		f.t.Fatalf("CreateWorkflowAttempt: %v", err)
	}
	f.mutateAttempts(func(a *domain.WorkflowAttempt) {
		a.DeadlineAt = &deadline
		a.ReviewTarget = domain.WorkflowReviewTarget{
			ReviewRunID: &reviewRunID, Fingerprint: "fp-reviewed", HeadSHA: "head-sha-1",
		}
	})

	// The session, alive and idle, with a hook signal and a completed turn.
	f.sessionFacts.put(domain.SessionRecord{
		ID:              domain.SessionID(f.sessionID),
		ProjectID:       "proj-evidence",
		Kind:            domain.KindWorker,
		Harness:         domain.HarnessClaudeCode,
		Mode:            domain.SessionModeTUI,
		Activity:        domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now().Add(2 * time.Minute)},
		FirstSignalAt:   f.clk.Now().Add(time.Minute),
		TurnCompletedAt: f.clk.Now().Add(3 * time.Minute),
		Metadata:        domain.SessionMetadata{Branch: "ao/wf-evidence", WorkspacePath: "/ws/evidence"},
	})

	// The worktree AO will observe: clean, still at the dispatch base. That is
	// what an ambiguous work stop IS — a worker that went idle with nothing to
	// show — so this is the observation the production path really takes here,
	// and the one it must write down.
	f.workspaceFacts.obs = ports.WorkspaceObservation{
		Path: "/ws/evidence", Branch: "ao/wf-evidence", HeadSHA: "base-sha-1",
	}

	// The ledger: a dispatch, a worker observation, and the stop itself.
	stepID := f.workStepID
	sid := f.sessionID
	f.checkpoint(domain.WorkflowCheckpoint{
		DurablePhase: "worker_dispatched", NextAction: "observe_worker",
		Branch: "ao/wf-evidence", WorktreePath: "/ws/evidence",
		BaseSHA: "base-sha-1", HeadSHA: "head-sha-1", FingerprintBefore: "fp-dispatch",
	})
	f.clk.Advance(time.Minute)
	f.checkpoint(domain.WorkflowCheckpoint{
		DurablePhase: "worker_observed_worker_idle", NextAction: "waiting on the worker",
		Branch: "ao/wf-evidence", WorktreePath: "/ws/evidence",
		BaseSHA: "base-sha-1", HeadSHA: "head-sha-2", FingerprintAfter: "fp-reviewed",
	})

	// Migration 0133's dispatch boundary and mutation provenance.
	f.store.dispatchCheckpoints = append(f.store.dispatchCheckpoints, domain.WorkflowDispatchCheckpoint{
		ID: "wfd-evidence", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		Phase: domain.DispatchPhaseWorkerDispatched, IdempotencyKey: "workflow-step-work:" + stepID,
		Harness: "claude-code", SessionID: &sid,
		LaunchStage: domain.LaunchStageSpawn, LaunchOutcome: domain.LaunchOutcomeDispatched,
		EvidenceJSON: "{}", Detail: "the worker process started", CreatedAt: f.clk.Now(),
	})
	observedAt := f.clk.Now()
	f.store.mutationProvenance = append(f.store.mutationProvenance, domain.WorkflowMutationProvenance{
		ID: "wfp-evidence", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		Class: domain.MutationAuthorizedWork, Harness: "claude-code", SessionID: &sid,
		Branch: "ao/wf-evidence", WorktreePath: "/ws/evidence",
		BaseSHA: "base-sha-1", HeadSHA: "head-sha-2",
		FingerprintBefore: "fp-dispatch", FingerprintAfter: "fp-reviewed",
		Reason: "this run's own work step", EvidenceJSON: "{}",
		ObservedAt: &observedAt, CreatedAt: f.clk.Now(),
	})

}

// driveToAmbiguity stops the run the way production does: an ordinary GetRun
// poll, observeWorkStep's own idle/no-evidence decision, and the gate it has to
// pass through. NOTHING about the stop is written by this fixture — the
// observation record, the evidence snapshot and the parked run are all produced
// by the code under test, which is the only way the Advisor assertions below
// mean anything.
func (f *evidenceFixture) driveToAmbiguity() {
	f.t.Helper()
	f.clk.Advance(time.Minute)
	if _, err := f.c.GetRun(context.Background(), f.runID); err != nil {
		f.t.Fatalf("GetRun: %v", err)
	}
	if got := f.runState(); got != domain.WorkflowRunNeedsAttention {
		f.t.Fatalf("run state = %q, want needs_attention: the ambiguous stop was never reached", got)
	}
	if n := f.countPhase(workflowcore.AmbiguousWorkerStateEvidencePhase); n == 0 {
		f.t.Fatal("the ambiguous stop wrote no evidence snapshot")
	}
}

func (f *evidenceFixture) runState() domain.WorkflowRunState {
	f.t.Helper()
	run, ok, err := f.store.GetWorkflowRun(context.Background(), f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	return run.State
}

func (f *evidenceFixture) countPhase(phase string) int {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

// checkpointWithPhase returns the newest checkpoint carrying a phase.
func (f *evidenceFixture) checkpointWithPhase(phase string) (domain.WorkflowCheckpoint, bool) {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == phase && (!found || !cp.CreatedAt.Before(newest.CreatedAt)) {
			newest, found = cp, true
		}
	}
	return newest, found
}

func (f *evidenceFixture) collect() workflowcore.WorkerEvidenceSnapshot {
	f.t.Helper()
	ctx := context.Background()
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	steps, err := f.store.ListWorkflowSteps(ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	var work domain.WorkflowStep
	for _, s := range steps {
		if s.ID == f.workStepID {
			work = s
		}
	}
	return f.c.CollectWorkerEvidence(ctx, workflowcore.EvidenceRequest{Run: run, Step: work})
}

func (f *evidenceFixture) checkpoint(cp domain.WorkflowCheckpoint) {
	f.t.Helper()
	stepID := f.workStepID
	sid := f.sessionID
	cp.ID = fmt.Sprintf("wfc-ev-%s-%s", cp.DurablePhase, f.clk.Now().Format("150405.000000000"))
	cp.WorkflowRunID = f.runID
	cp.WorkflowStepID = &stepID
	cp.ProjectID = "proj-evidence"
	cp.SessionID = &sid
	cp.PayloadVersion = "v1"
	if cp.RetryState == "" {
		cp.RetryState = "{}"
	}
	cp.CreatedAt = f.clk.Now()
	if _, err := f.store.CreateWorkflowCheckpoint(context.Background(), cp); err != nil {
		f.t.Fatalf("CreateWorkflowCheckpoint: %v", err)
	}
}

func (f *evidenceFixture) mutateSteps(mutate func(*domain.WorkflowStep)) {
	steps := f.store.steps[f.runID]
	for i := range steps {
		mutate(&steps[i])
	}
	f.store.steps[f.runID] = steps
}

func (f *evidenceFixture) mutateAttempts(mutate func(*domain.WorkflowAttempt)) {
	attempts := f.store.attempts[f.workStepID]
	for i := range attempts {
		mutate(&attempts[i])
	}
	f.store.attempts[f.workStepID] = attempts
}

func packSection(t *testing.T, pack workflowcore.IncidentContextPack, title string) workflowcore.IncidentPackSection {
	t.Helper()
	for _, s := range pack.Sections {
		if s.Title == title {
			return s
		}
	}
	t.Fatalf("the pack has no %q section; it has %v", title, packSectionTitles(pack))
	return workflowcore.IncidentPackSection{}
}

func packSectionTitles(pack workflowcore.IncidentContextPack) []string {
	out := make([]string, 0, len(pack.Sections))
	for _, s := range pack.Sections {
		out = append(out, s.Title)
	}
	return out
}

// ---- the evidence row must be additive --------------------------------------

// The evidence row is written for a STEP, which makes it "the latest checkpoint
// for this step" — the row ~20 readers resolve the worktree, the branch, the
// dispatch base, the worker session and the delivered fingerprint from.
//
// So it must carry that identity forward, exactly as observeWorkStep's own
// checkpoint and recordFirstSignalReconciliation's do, and for exactly the
// reason both of them say so. A bare evidence row does not merely fail to
// help: it DESTROYS already-correct work.
//
// On the fix step the loss is permanent and total:
//
//	observeFixStep      latestCP.SessionID == nil        -> returns early, forever
//	review_dispatch     fixCP.FingerprintAfter == ""     -> cycle N+1 never dispatches
//
// A fix worker's genuinely delivered change is still in the worktree; AO has
// simply thrown away the record that says it landed, and has disabled the one
// observer that could ever record it again.
func TestAmbiguityEvidencePreservesTheStepsWorkspaceIdentity(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	before := latestCheckpointForStep(t, f.store, f.fixStepID)
	if before.SessionID == nil || *before.SessionID == "" {
		t.Fatalf("fixture precondition: the fix dispatch checkpoint carries no session id")
	}

	// The incident's own shape: a cycle nobody started, past the grace window.
	f.silentSinceBeforeDispatch()
	f.clk.Advance(11 * time.Minute)
	f.poll(1)

	if n := f.countCheckpointPhase(workflowcore.ReasonFixCycleNotStarted); n == 0 {
		t.Fatal("fixture precondition: the ambiguous fix stop was never reached")
	}
	if n := f.countCheckpointPhase(workflowcore.AmbiguousWorkerStateEvidencePhase); n == 0 {
		t.Fatal("fixture precondition: no evidence row was written for the ambiguous stop")
	}

	after := latestCheckpointForStep(t, f.store, f.fixStepID)
	if after.SessionID == nil || *after.SessionID == "" {
		t.Fatalf("the fix step's latest checkpoint (%s) lost the worker session id: "+
			"observeFixStep's own guard now returns early on every future poll, so this "+
			"fix cycle can never be observed again", after.DurablePhase)
	}
	if *after.SessionID != *before.SessionID {
		t.Fatalf("session id changed from %q to %q", *before.SessionID, *after.SessionID)
	}
	for _, field := range []struct{ name, was, now string }{
		{"branch", before.Branch, after.Branch},
		{"worktree path", before.WorktreePath, after.WorktreePath},
		{"base SHA", before.BaseSHA, after.BaseSHA},
		{"head SHA", before.HeadSHA, after.HeadSHA},
		{"fingerprint before", before.FingerprintBefore, after.FingerprintBefore},
		{"fingerprint after", before.FingerprintAfter, after.FingerprintAfter},
	} {
		if field.was != "" && field.now == "" {
			t.Errorf("the fix step's latest checkpoint (%s) dropped the %s (%q): "+
				"an evidence row must annotate a step, never strip it",
				after.DurablePhase, field.name, field.was)
		}
	}
}

// The same invariant on the work step. Here the loss is a crash window rather
// than a permanent one — observeWorkStep writes its own full checkpoint
// immediately afterwards — but the evidence row is deliberately written FIRST
// so that a daemon dying between the two leaves readable evidence. A bare row
// would make that window leave the reviewer with no worktree to launch into,
// which is the exact failure 8C's real E2E run hit.
func TestAmbiguityEvidenceOnTheWorkStepCarriesTheWorktreeForward(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedFullEvidence()

	cps, err := f.store.ListWorkflowCheckpoints(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var evidence domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == workflowcore.AmbiguousWorkerStateEvidencePhase {
			evidence, found = cp, true
		}
	}
	if !found {
		t.Fatal("no evidence row was written for the ambiguous work stop")
	}
	if evidence.Branch == "" || evidence.WorktreePath == "" {
		t.Errorf("the evidence row carries branch=%q worktree=%q: a restart in the window "+
			"between it and the stop's own checkpoint would leave the reviewer with no "+
			"worktree to launch into", evidence.Branch, evidence.WorktreePath)
	}
	if evidence.BaseSHA == "" {
		t.Error("the evidence row carries no base SHA, so a restart would compare the " +
			"worktree against an empty dispatch base and read any HEAD at all as new work")
	}
}

func latestCheckpointForStep(t *testing.T, store *fakeStore, stepID string) domain.WorkflowCheckpoint {
	t.Helper()
	cp, ok, err := store.GetLatestWorkflowCheckpointByStep(context.Background(), stepID)
	if err != nil {
		t.Fatalf("GetLatestWorkflowCheckpointByStep: %v", err)
	}
	if !ok {
		t.Fatalf("step %s has no checkpoint at all", stepID)
	}
	return cp
}

// ---- the observation is written by production code, not by a fixture --------

// The collector reads only durable rows, which is worth nothing unless
// production actually writes them. Nothing in this fixture creates a
// worker_observation_evidence checkpoint: the one asserted here is written by
// observeWorkStep's own gate, from the observation that decision was taken on,
// before the snapshot is built.
func TestTheAmbiguityGateDurablyRecordsTheObservationItDecidedOn(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedFullEvidence()

	if n := f.countPhase(workflowcore.WorkerObservationEvidencePhase); n != 1 {
		t.Fatalf("worker_observation_evidence rows = %d, want exactly 1 written by the raise path", n)
	}
	cp, ok := f.checkpointWithPhase(workflowcore.WorkerObservationEvidencePhase)
	if !ok {
		t.Fatal("the production raise path recorded no observation")
	}
	facts, ok := workflowcore.DecodeObservedWorkerFacts(cp.RetryState)
	if !ok {
		t.Fatalf("the recorded observation did not decode: %q", cp.RetryState)
	}
	if !facts.WorkspaceKnown {
		t.Error("the recorded observation does not say a workspace reading was taken")
	}
	if facts.HeadSHA != "base-sha-1" || facts.Branch != "ao/wf-evidence" {
		t.Errorf("recorded observation head=%q branch=%q, want the tree observeWorkStep actually read",
			facts.HeadSHA, facts.Branch)
	}
	if facts.Fingerprint == "" {
		t.Error("the recorded observation carries no fingerprint, so nothing can be compared against it later")
	}
	if !facts.LivenessKnown || !facts.LivenessAlive {
		t.Errorf("recorded liveness known=%v alive=%v, want the probe's own answer persisted",
			facts.LivenessKnown, facts.LivenessAlive)
	}
	if facts.ObservedAt.IsZero() {
		t.Error("the recorded observation is undated")
	}
	// It must also be readable by a coordinator that never took the reading —
	// which is the whole reason for writing it down.
	restarted := f.restartedCoordinator()
	_, pack, err := restarted.IncidentPackFor(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("IncidentPackFor after restart: %v", err)
	}
	section := packSection(t, pack, "Durable evidence snapshot (bounded)")
	if strings.Contains(section.Body, "NOT AVAILABLE") {
		t.Fatalf("after a restart the Advisor lost evidence the stop was taken on:\n%s", section.Body)
	}
}

// The same path on the fix step, where the observed tree is legitimately dirty:
// the bounded changed-file list and the dirty flags must survive into the
// durable record, not just a "something was observed" bit.
func TestTheFixAmbiguityGateRecordsTheFullObservedTree(t *testing.T) {
	f := newFixRecoveryFixture(t)
	// Set before the drive so the fingerprint this tree hashes to is the one
	// review approves — which is what makes the later stop "no verifiable NEW
	// change" rather than a delivered fix.
	f.workspaceFacts.obs = ports.WorkspaceObservation{
		Path: "/ws/wf", Branch: "ao/wf", HeadSHA: "head-1",
		Changes: []ports.WorkspaceChange{
			{Status: " M", Path: "internal/workflow/evidence_snapshot.go"},
			{Status: "??", Path: "internal/workflow/ambiguous_worker_state.go"},
		},
	}
	f.driveToFixDispatch()
	// The worker picked the cycle up and went idle again without changing
	// anything new: activity that POSTDATES the dispatch, plus a first signal.
	dispatchedAt := f.intentCreatedAt()
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: dispatchedAt.Add(time.Minute)}
		rec.TurnCompletedAt = dispatchedAt.Add(time.Minute)
		rec.FirstSignalAt = dispatchedAt.Add(30 * time.Second)
		rec.IsTerminated = false
	})

	f.clk.Advance(2 * time.Minute)
	f.poll(1)

	if n := f.countCheckpointPhase(workflowcore.ReasonFixNoVerifiableChange); n == 0 {
		t.Fatal("fixture precondition: the fix ambiguity was never reached")
	}
	cp := latestCheckpointWithPhase(t, f.store, f.runID, workflowcore.WorkerObservationEvidencePhase)
	facts, ok := workflowcore.DecodeObservedWorkerFacts(cp.RetryState)
	if !ok {
		t.Fatalf("the recorded observation did not decode: %q", cp.RetryState)
	}
	if !facts.Dirty {
		t.Error("the recorded observation lost the dirty flag")
	}
	if len(facts.ChangedFiles) != 2 {
		t.Fatalf("recorded changed files = %v, want both observed paths", facts.ChangedFiles)
	}
	if !strings.Contains(strings.Join(facts.ChangedFiles, " "), "evidence_snapshot.go") {
		t.Errorf("recorded changed files = %v, want the porcelain status and path", facts.ChangedFiles)
	}
}

// ---- a raise whose evidence cannot be written is not a raise ----------------

// If the durable snapshot cannot be written, the ambiguity must not happen at
// all. The alternative is the exact dead end this mechanism exists to abolish:
// ambiguous_worker_state on the ledger with no stop-time evidence under it, so
// that after a restart the Advisor has a conclusion and nothing to show for it.
func TestAnAmbiguityIsNotPersistedWhenItsEvidenceCannotBeWritten(t *testing.T) {
	f := newEvidenceFixture(t)
	f.seedDurableFacts()
	f.store.checkpointWriteErr = func(cp domain.WorkflowCheckpoint) error {
		if cp.DurablePhase == workflowcore.AmbiguousWorkerStateEvidencePhase {
			return errors.New("disk full")
		}
		return nil
	}

	f.clk.Advance(time.Minute)
	_, err := f.c.GetRun(context.Background(), f.runID)
	if err == nil {
		t.Fatal("the poll succeeded even though the stop's evidence could not be written")
	}
	if !strings.Contains(err.Error(), "refusing to raise ambiguous_worker_state") {
		t.Fatalf("error = %v, want the gate's own refusal", err)
	}

	// And nothing about the ambiguity reached durable state.
	if got := f.runState(); got == domain.WorkflowRunNeedsAttention {
		t.Error("the run was parked on an ambiguity whose evidence was never written")
	}
	steps, err := f.store.ListWorkflowSteps(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.ID != f.workStepID {
			continue
		}
		if s.State != domain.WorkflowStepRunning {
			t.Errorf("work step state = %q, want still running: the transition should not have been taken", s.State)
		}
	}
	attempts, err := f.store.ListWorkflowAttempts(context.Background(), f.workStepID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	for _, a := range attempts {
		if a.ErrorClass == domain.WorkflowErrorAmbiguousWorkerState {
			t.Error("ambiguous_worker_state was persisted on an attempt with no evidence behind it")
		}
	}
	if n := f.countPhase(workflowcore.AmbiguousWorkerStateEvidencePhase); n != 0 {
		t.Errorf("evidence rows = %d, want 0", n)
	}

	// Once the store recovers, the very next poll takes the stop properly.
	f.store.checkpointWriteErr = nil
	f.clk.Advance(time.Minute)
	if _, err := f.c.GetRun(context.Background(), f.runID); err != nil {
		t.Fatalf("GetRun after the store recovered: %v", err)
	}
	if f.countPhase(workflowcore.AmbiguousWorkerStateEvidencePhase) != 1 {
		t.Error("the recovered poll did not take the stop it had refused")
	}
}

// restartedCoordinator models a daemon that came back over the same rows: it
// never took any of the readings the stop was made on, so anything it can still
// show a person came out of the ledger.
func (f *evidenceFixture) restartedCoordinator() *workflowcore.Coordinator {
	f.t.Helper()
	var idSeq int
	return workflowcore.New(workflowcore.Deps{
		Store:        f.store,
		SessionFacts: f.sessionFacts,
		Clock:        f.clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("rst%d", idSeq)
		},
	})
}
