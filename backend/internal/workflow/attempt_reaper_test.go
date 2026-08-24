package workflow_test

// Closing attempts a restart or a crash abandoned (attempt_reaper.go).
//
// The failure this prevents is quiet: a workflow_attempts row with no outcome
// means "work may be in flight", every guard downstream believes it, and a row
// left behind by a dead daemon therefore refuses every recovery there is,
// forever, in a way indistinguishable from a correct refusal. The tests below
// are as much about what the reaper REFUSES to close as about what it closes —
// a wrongly-reaped attempt would tell those same guards the tree is quiet while
// an agent is still writing to it.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type reaperFixture struct {
	t     *testing.T
	coord *workflowcore.Coordinator
	store *fakeStore
	clk   *fakeClock
	facts *fakeSessionFacts
	ws    *mutableWorkspaceFacts
	runID string
	sid   string
	now   time.Time
}

// newReaperFixture builds a run parked in needs_attention whose fix step holds
// ONE attempt that never recorded an outcome, and whose review step carries a
// checkpoint written after that attempt started — the durable proof the run
// carried on without it. This is the shape a daemon killed mid-fix leaves
// behind, and the shape Task 8 was actually found in.
func newReaperFixture(t *testing.T) *reaperFixture {
	t.Helper()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	runID := "wf-reaper"
	sid := "sess-reaper"
	fx := &reaperFixture{t: t, store: newFakeStore(), clk: &fakeClock{t: now}, runID: runID, sid: sid, now: now}

	fx.store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &fx.sid},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepWaiting},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepFailed},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending},
	}
	fx.store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "project-1", Objective: "reaper objective",
		State: domain.WorkflowRunNeedsAttention, PolicyVersion: "v1",
		PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now.Add(-8 * time.Hour), UpdatedAt: now,
	}

	// The abandoned attempt: opened 8 hours ago, never closed.
	fx.store.attempts["fix"] = []domain.WorkflowAttempt{{
		ID: "wfa-orphan", WorkflowStepID: "fix", AttemptNumber: 1,
		StartedAt: now.Add(-8 * time.Hour),
	}}

	workStepID, reviewStepID := "work", "review"
	fx.store.checkpoints[runID] = []domain.WorkflowCheckpoint{
		{
			ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "project-1",
			SessionID: &fx.sid, Branch: "main", WorktreePath: "/tmp/reaper",
			DurablePhase: "worker_observed", CreatedAt: now.Add(-9 * time.Hour),
		},
		// Proof 3: AO dispatched a review, on another step, an hour AFTER the
		// fix attempt opened. It cannot have done that while the fix was live.
		{
			ID: "review-cp", WorkflowRunID: runID, WorkflowStepID: &reviewStepID, ProjectID: "project-1",
			DurablePhase: "review_dispatched", CreatedAt: now.Add(-7 * time.Hour),
		},
	}

	// The agent has been gone for hours.
	fx.facts = newFakeSessionFacts()
	fx.facts.put(domain.SessionRecord{
		ID: domain.SessionID(sid), ProjectID: "project-1",
		Activity:        domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-7 * time.Hour)},
		TurnCompletedAt: now.Add(-7 * time.Hour),
		Metadata:        domain.SessionMetadata{Branch: "main", WorkspacePath: "/tmp/reaper"},
	})
	fx.ws = &mutableWorkspaceFacts{obs: ports.WorkspaceObservation{Path: "/tmp/reaper", Branch: "main", HeadSHA: "deadbeef"}}
	fx.coord = fx.newCoordinator()
	return fx
}

// newCoordinator over the same durable state. A second one IS a restart.
func (fx *reaperFixture) newCoordinator() *workflowcore.Coordinator {
	ids := 0
	return workflowcore.New(workflowcore.Deps{
		Store: fx.store, ReviewRuns: newFakeReviewRuns(), WorkspaceFacts: fx.ws,
		SessionFacts: fx.facts, MessageSender: &fakeMessageSender{},
		Clock: fx.clk.Now,
		NewID: func() string { ids++; return "reap-id" },
	})
}

func (fx *reaperFixture) continueRun() {
	fx.t.Helper()
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
		fx.t.Fatalf("ContinueRun: %v", err)
	}
}

func (fx *reaperFixture) orphan() domain.WorkflowAttempt {
	fx.t.Helper()
	for _, a := range fx.store.attempts["fix"] {
		if a.ID == "wfa-orphan" {
			return a
		}
	}
	fx.t.Fatal("the orphaned attempt vanished from the store")
	return domain.WorkflowAttempt{}
}

// reapRecordView mirrors the reaper's durable payload. The test reads the
// ledger as a person would — by its JSON — rather than through an exported
// type, so a change to the record's shape shows up here as a failing claim.
type reapRecordView struct {
	Reason         string    `json:"reason"`
	AttemptID      string    `json:"attemptId"`
	StepID         string    `json:"stepId"`
	StepKind       string    `json:"stepKind"`
	StartedAt      time.Time `json:"startedAt"`
	EvidencePhase  string    `json:"evidencePhase"`
	EvidenceStepID string    `json:"evidenceStepId"`
	EvidenceAt     time.Time `json:"evidenceAt"`
	SessionID      string    `json:"sessionId"`
}

func (fx *reaperFixture) reapRecords() []reapRecordView {
	fx.t.Helper()
	var out []reapRecordView
	for _, cp := range checkpointsByPhase(fx.store, fx.runID, "attempt_reaped_orphaned") {
		var rec reapRecordView
		if err := json.Unmarshal([]byte(cp.RetryState), &rec); err != nil {
			fx.t.Fatalf("reap checkpoint is unreadable: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

// ---- 1. the abandoned attempt is closed, with its reason on the record -----

func TestOrphanedAttemptIsClosedWithADurableReason(t *testing.T) {
	fx := newReaperFixture(t)
	fx.continueRun()

	a := fx.orphan()
	if a.FinishedAt == nil {
		t.Fatal("the abandoned attempt is still open, so every guard downstream still believes work is in flight")
	}
	if a.Outcome != domain.WorkflowAttemptCancelled {
		t.Fatalf("outcome = %q, want cancelled: AO never observed this attempt fail, only that it was abandoned", a.Outcome)
	}
	if a.ErrorClass != "" {
		t.Fatalf("error class = %q, want empty: recording one would put a failure AO never saw into the run's history", a.ErrorClass)
	}

	recs := fx.reapRecords()
	if len(recs) != 1 {
		t.Fatalf("reap records = %d, want exactly 1", len(recs))
	}
	if recs[0].Reason != "orphaned_after_restart" {
		t.Fatalf("reason = %q, want orphaned_after_restart", recs[0].Reason)
	}
	if recs[0].AttemptID != "wfa-orphan" {
		t.Fatalf("record names attempt %q, want wfa-orphan", recs[0].AttemptID)
	}
	// The evidence must be on the record, not merely consulted, so the claim is
	// re-checkable by a person reading the ledger.
	if recs[0].EvidencePhase != "review_dispatched" || recs[0].EvidenceStepID != "review" {
		t.Fatalf("evidence = %q on %q, want review_dispatched on review", recs[0].EvidencePhase, recs[0].EvidenceStepID)
	}
	if !recs[0].EvidenceAt.After(recs[0].StartedAt) {
		t.Fatal("the recorded evidence does not postdate the attempt it justifies closing")
	}
}

// ---- 2. false-positive protection ------------------------------------------

// A live agent is the case that must never be reaped: closing its attempt would
// tell every downstream guard the tree is quiet while it is being written to.
func TestAttemptOfALiveAgentIsNeverReaped(t *testing.T) {
	for _, tc := range []struct {
		name string
		sess domain.SessionRecord
	}{
		{"actively running", domain.SessionRecord{
			Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC)},
		}},
		{"quiet for less than the settle window", domain.SessionRecord{
			Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Date(2026, 8, 24, 11, 55, 0, 0, time.UTC)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newReaperFixture(t)
			sess := tc.sess
			sess.ID, sess.ProjectID = domain.SessionID(fx.sid), "project-1"
			sess.Metadata = domain.SessionMetadata{Branch: "main", WorkspacePath: "/tmp/reaper"}
			fx.facts.put(sess)

			fx.continueRun()

			if fx.orphan().FinishedAt != nil {
				t.Fatal("reaped the attempt of an agent that may still be writing to the tree")
			}
			if len(fx.reapRecords()) != 0 {
				t.Fatal("wrote a reap record for an attempt it did not reap")
			}
		})
	}
}

// A session AO cannot read is not evidence of absence. Unknown must refuse.
func TestUnreadableSessionRefusesToReap(t *testing.T) {
	fx := newReaperFixture(t)
	delete(fx.facts.byID, domain.SessionID(fx.sid))

	fx.continueRun()

	if fx.orphan().FinishedAt != nil {
		t.Fatal("reaped an attempt whose owning agent AO could not account for")
	}
}

// An attempt the run never went past is unfinished, not abandoned.
func TestAttemptWithNoLaterProgressIsNotReaped(t *testing.T) {
	fx := newReaperFixture(t)
	// Remove the only evidence recorded after the attempt opened.
	var kept []domain.WorkflowCheckpoint
	for _, cp := range fx.store.checkpoints[fx.runID] {
		if cp.ID != "review-cp" {
			kept = append(kept, cp)
		}
	}
	fx.store.checkpoints[fx.runID] = kept

	fx.continueRun()

	if fx.orphan().FinishedAt != nil {
		t.Fatal("reaped an attempt with no durable evidence the run ever moved past it")
	}
}

// A step still in flight OWNS its attempt.
func TestAttemptOfARunningStepIsNotReaped(t *testing.T) {
	for _, state := range []domain.WorkflowStepState{domain.WorkflowStepRunning, domain.WorkflowStepReady} {
		t.Run(string(state), func(t *testing.T) {
			fx := newReaperFixture(t)
			steps := fx.store.steps[fx.runID]
			for i := range steps {
				if steps[i].ID == "fix" {
					steps[i].State = state
				}
			}
			fx.store.steps[fx.runID] = steps

			fx.continueRun()

			if fx.orphan().FinishedAt != nil {
				t.Fatalf("reaped the attempt of a step that is %s — that is live work, not a fossil", state)
			}
		})
	}
}

// A dispatch that opened moments ago must not be mistaken for an abandoned one.
func TestYoungAttemptIsNotReaped(t *testing.T) {
	fx := newReaperFixture(t)
	fx.store.attempts["fix"] = []domain.WorkflowAttempt{{
		ID: "wfa-orphan", WorkflowStepID: "fix", AttemptNumber: 1,
		StartedAt: fx.now.Add(-2 * time.Minute),
	}}

	fx.continueRun()

	if fx.orphan().FinishedAt != nil {
		t.Fatal("reaped an attempt young enough to be a dispatch still in progress")
	}
}

// ---- 3. idempotence, and crash-safety of the two writes --------------------

// Many Continues, one reap, one ledger entry.
func TestRepeatedContinueReapsExactlyOnce(t *testing.T) {
	fx := newReaperFixture(t)
	for i := 0; i < 20; i++ {
		fx.clk.Advance(time.Second)
		fx.continueRun()
	}
	if n := len(fx.reapRecords()); n != 1 {
		t.Fatalf("reap records = %d after 20 Continues, want exactly 1", n)
	}
	if fx.orphan().FinishedAt == nil {
		t.Fatal("the attempt was never closed")
	}
}

// A crash BETWEEN the two writes — the record landed, the row did not — must
// resume: finish closing the row, and add nothing to the ledger.
func TestRestartBetweenTheReapRecordAndTheRowResumes(t *testing.T) {
	fx := newReaperFixture(t)
	fx.continueRun()
	if fx.orphan().FinishedAt == nil {
		t.Fatal("precondition: the attempt should have been closed")
	}

	// Rewind the row to exactly the half-written state a crash leaves: the
	// checkpoint is durable, the outcome never landed.
	fx.store.attempts["fix"] = []domain.WorkflowAttempt{{
		ID: "wfa-orphan", WorkflowStepID: "fix", AttemptNumber: 1,
		StartedAt: fx.now.Add(-8 * time.Hour),
	}}
	if len(fx.reapRecords()) != 1 {
		t.Fatal("precondition: the reap record should be durable")
	}

	// A new Coordinator over the same store is the restart.
	fx.clk.Advance(time.Minute)
	fx.coord = fx.newCoordinator()
	fx.continueRun()

	if fx.orphan().FinishedAt == nil {
		t.Fatal("the restart did not finish closing the row a crash left open")
	}
	if n := len(fx.reapRecords()); n != 1 {
		t.Fatalf("reap records = %d after the restart, want exactly 1 — the ledger must not be duplicated", n)
	}
}

// ---- 4. the reaper spends no budget ----------------------------------------

// Reaping must not look like a fix cycle: it creates no attempt row, dispatches
// nothing, and leaves the policy's budget exactly where it was.
func TestReapingCreatesNoAttemptAndSpendsNoBudget(t *testing.T) {
	fx := newReaperFixture(t)
	before := len(fx.store.attempts["fix"])

	fx.continueRun()

	if after := len(fx.store.attempts["fix"]); after != before {
		t.Fatalf("fix attempts = %d, want %d: reaping must never create an attempt row", after, before)
	}
	if snap := fx.store.runs[fx.runID].PolicySnapshot; snap != `{"maxFixCycles":3}` {
		t.Fatalf("policy snapshot changed to %q: reaping must never touch a budget", snap)
	}
}

// ---- 5. the reap must not become the run's stop reason ---------------------

// The reap record is written on a parked run, from the very Continue that is
// about to ask why the run is parked. If it counted as the newest checkpoint it
// would displace the real stop phase, and the run would lose the ability to say
// why it stopped — silently, and at exactly the moment a person is asking.
//
// This is not hypothetical: it is what the reaper did to Task 8's run on its
// first live use, which refused the recovery that was otherwise provable.
func TestReapRecordDoesNotDisplaceTheRunsStopReason(t *testing.T) {
	fx := newReaperFixture(t)
	// A real stop, of the class that is recoverable, recorded before the reap.
	verifyStepID := "verify"
	fx.store.checkpoints[fx.runID] = append(fx.store.checkpoints[fx.runID], domain.WorkflowCheckpoint{
		ID: "stop-cp", WorkflowRunID: fx.runID, WorkflowStepID: &verifyStepID, ProjectID: "project-1",
		DurablePhase: "verify_unrepairable", NextAction: "verify failed (verify_workspace_changed)",
		CreatedAt: fx.now.Add(-time.Hour),
	})

	before, _, okBefore := fx.coord.StopReasonForTest(context.Background(), fx.runID)
	if !okBefore || before != "verify_unrepairable" {
		t.Fatalf("precondition: stop reason = %q (ok=%v), want verify_unrepairable", before, okBefore)
	}

	fx.continueRun()
	if fx.orphan().FinishedAt == nil {
		t.Fatal("precondition: the attempt should have been reaped")
	}

	after, _, okAfter := fx.coord.StopReasonForTest(context.Background(), fx.runID)
	if !okAfter || after != before {
		t.Fatalf("the reap rewrote the run's stop reason to %q (ok=%v), want %q left intact — "+
			"a run that cannot say why it stopped is the failure this exclusion exists to prevent",
			after, okAfter, before)
	}
}
