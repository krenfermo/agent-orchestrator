package workflow_test

// incident_wf724a1e97_sqlite_e2e_test.go — the incident against real SQLite.
//
// The unit fixtures elsewhere run over an in-memory store, which is the right
// tool for pinning one rule. This one exists for the part of the incident that
// is about DURABLE ORDER: every checkpoint, step transition, review binding and
// supersession going through the real schema, its constraints and its triggers,
// in the sequence ~/.ao/data actually recorded — and then continuing past the
// point where the real run stopped.
//
// The identifiers below are the incident's own, so the durable trail this test
// writes can be read side by side with the one it reproduces.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// The heads the real run moved through, in order. bf54196b5 is the tree the
// fourth review judged; ccefd07b0 is what the human-applied fix produced and
// what review 33c08c40 judged; 247d3bc5f is the commit that answers 33c08c40's
// finding and that AO never looked at.
const (
	incidentHeadAfterFix3 = "bf54196b5c0a0000000000000000000000000000"
	incidentHeadHumanFix  = "ccefd07b08833619f4230d69318b0794f0e9e441"
	incidentHeadResolving = "247d3bc5f0000000000000000000000000000000"
)

type incidentE2E struct {
	t     *testing.T
	ctx   context.Context
	store *sqlite.Store
	facts *fakeSessionFacts
	ws    *mutableWorkspaceFacts
	rl    *fakeReviewerLauncher
	sn    *fakeMessageSender
	sp    *e2eSpawner
	clk   *fakeClock
	wake  *wake.Scheduler
	c     *workflowcore.Coordinator
	runID string
	idSeq int
}

func newIncidentE2E(t *testing.T) *incidentE2E {
	t.Helper()
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	worktree := t.TempDir()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "agent-orchestrator", Path: worktree, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	facts := newFakeSessionFacts()
	f := &incidentE2E{
		t: t, ctx: ctx, store: store, facts: facts,
		ws: &mutableWorkspaceFacts{obs: ports.WorkspaceObservation{
			Path: worktree, HeadSHA: "31bc6116f0000000000000000000000000000000",
		}},
		rl: &fakeReviewerLauncher{}, sn: &fakeMessageSender{},
		sp:  &e2eSpawner{t: t, store: store, facts: facts, worktree: worktree},
		clk: &fakeClock{t: time.Date(2026, 8, 30, 0, 10, 5, 0, time.UTC)},
	}
	f.wake = wake.New(store, f.clk.Now, autoIDSeq("wk-e2e"), wake.Config{})
	f.build()

	created, err := f.c.CreateRun(ctx, "agent-orchestrator", "Audit existing context/memory system and document findings")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	return f
}

func (f *incidentE2E) build() {
	f.c = workflowcore.New(workflowcore.Deps{
		Store:          f.store,
		Projects:       f.store,
		QuestionsStore: f.store,
		Spawner:        f.sp,
		// The REAL session store, not a double: repair quiescence proves "no live
		// writer" against session rows, and a fixture that answered that question
		// from a side map would be testing the map.
		SessionFacts:     f.store,
		WorkspaceFacts:   f.ws,
		ReviewRuns:       f.store,
		ReviewerLauncher: f.rl,
		MessageSender:    f.sn,
		// The real admission ledger and the real wake scheduler. Both are
		// load-bearing for repair quiescence (repair_quiescence.go proves
		// "holds no live slot" and "has no automatic transition pending"
		// against exactly these), and wiring them here means every test on this
		// fixture runs through them rather than around them.
		Capacity:      f.store,
		WakeScheduler: f.wake,
		Clock:         f.clk.Now,
		NewID: func() string {
			f.idSeq++
			return fmt.Sprintf("e2e%d", f.idSeq)
		},
	})
}

// restart is what a daemon restart actually is: a new Coordinator over the same
// rows. Nothing about the convergence may live in memory.
func (f *incidentE2E) restart() { f.build() }

// newCoordinatorOverSameStore is a SECOND live daemon over the same rows, for
// the tests that need two passes racing rather than one replacing the other.
func (f *incidentE2E) newCoordinatorOverSameStore() *workflowcore.Coordinator {
	current := f.c
	f.build()
	second := f.c
	f.c = current
	return second
}

func (f *incidentE2E) detail() workflowcore.RunDetail {
	f.t.Helper()
	f.clk.Advance(2 * time.Second)
	got, err := f.c.GetRun(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("GetRun: %v", err)
	}
	return got
}

func (f *incidentE2E) phases() []string {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	out := make([]string, 0, len(cps))
	for _, cp := range cps {
		out = append(out, cp.DurablePhase)
	}
	return out
}

func (f *incidentE2E) countPhase(phase string) int {
	f.t.Helper()
	n := 0
	for _, p := range f.phases() {
		if p == phase {
			n++
		}
	}
	return n
}

// completeWork drives the run to a completed work step, exactly as the real one
// did at 00:22:05.
func (f *incidentE2E) completeWork() {
	f.t.Helper()
	detail, err := f.c.StartRun(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	if work.Step.SessionID == nil {
		f.t.Fatal("work step has no session")
	}
	sess, found, err := f.store.GetSession(f.ctx, domain.SessionID(*work.Step.SessionID))
	if err != nil || !found {
		f.t.Fatalf("GetSession(%s): %v (found=%v)", *work.Step.SessionID, err, found)
	}
	sess.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now()}
	sess.Metadata.WorkspacePath = f.ws.obs.Path
	sess.Metadata.Branch = "feat/engineering-control-center"
	sess.UpdatedAt = f.clk.Now()
	if err := f.store.UpdateSession(f.ctx, sess); err != nil {
		f.t.Fatalf("UpdateSession: %v", err)
	}
	f.facts.put(sess)
	f.ws.obs.Dirty = true
	f.clk.Advance(10 * time.Second)
	if st := workStepFrom(f.detail()).Step.State; st != domain.WorkflowStepCompleted {
		f.t.Fatalf("work step state = %q, want completed", st)
	}
}

// answerReview records the reviewer's verdict for whichever review currently
// holds authority, and returns that review's id.
func (f *incidentE2E) answerReview(verdict domain.ReviewVerdict, body string) string {
	f.t.Helper()
	got := f.detail()
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		f.t.Fatalf("no review run is bound; review step is %q", review.Step.State)
	}
	id := *review.Step.ReviewRunID
	ok, err := f.store.UpdateReviewRunResult(f.ctx, id, domain.ReviewRunComplete, verdict, body, "", true)
	if err != nil || !ok {
		f.t.Fatalf("UpdateReviewRunResult(%s): %v (applied=%v)", id, err, ok)
	}
	f.clk.Advance(time.Second)
	return id
}

// moveHead is a commit landing on the branch.
func (f *incidentE2E) moveHead(sha string) {
	f.ws.obs.HeadSHA = sha
	f.clk.Advance(time.Minute)
}

// goQuiet makes the worker provably not in flight: it finished a turn and has
// said nothing for an hour, which is the shape the real session had.
func (f *incidentE2E) goQuiet() {
	f.t.Helper()
	sessions, err := f.store.ListSessions(f.ctx, "agent-orchestrator")
	if err != nil {
		f.t.Fatalf("ListSessions: %v", err)
	}
	for _, rec := range sessions {
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now().Add(-time.Hour)}
		rec.TurnCompletedAt = f.clk.Now().Add(-time.Hour)
		rec.UpdatedAt = f.clk.Now()
		if uerr := f.store.UpdateSession(f.ctx, rec); uerr != nil {
			f.t.Fatalf("UpdateSession(%s): %v", rec.ID, uerr)
		}
		f.facts.put(rec)
	}
}

// The whole incident, in order, against the real schema — and then past it.
func TestIncidentWF724A1E97_SQLiteEndToEnd(t *testing.T) {
	f := newIncidentE2E(t)
	f.completeWork()
	// The work -> review handoff, which is the one non-stopped Continue AO has.
	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun (review handoff): %v", err)
	}

	// Cycles 1..3: review requests changes, a fix cycle runs, the head moves.
	// max_fix_cycles is 3, so the fourth verdict is the one that stops the run.
	for cycle := 1; cycle <= 3; cycle++ {
		f.detail()
		f.answerReview(domain.VerdictChangesRequested, "cycle: still not right")
		f.detail()
		f.moveHead(fmt.Sprintf("%09dfix%s", cycle, "0000000000000000000000000000000")[:40])
		f.detail()
	}
	f.moveHead(incidentHeadAfterFix3)
	f.detail()
	f.answerReview(domain.VerdictChangesRequested, "the audit doc still claims harness reads are observed")
	f.detail()

	// The stop the incident reached, with arithmetic that reconciles.
	if n := f.countPhase(workflowcore.ReasonFixBudgetExhausted); n != 1 {
		t.Fatalf("fix_budget_exhausted checkpoints = %d, want exactly 1\nphases: %v", n, f.phases())
	}
	if st := f.detail().Run.State; st != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", st)
	}

	// 01:21 — a change appears that AO did not make, and its worker has been
	// silent since. This is the human-applied fix (HEAD ccefd07b0).
	f.goQuiet()
	f.moveHead(incidentHeadHumanFix)
	launchesBefore := f.rl.launchCalls

	// 01:21 — a RESTART, then reconciliation alone. Nobody presses anything.
	f.restart()
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if n := f.countPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed = %d, want 1\nphases: %v", n, f.phases())
	}
	if got := f.rl.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches for the adopted state = %d, want exactly 1", got)
	}
	freshOfCcefd := *reviewStepFrom(f.detail()).Step.ReviewRunID

	// 01:23 — that review requests changes too. In the real run this re-parked
	// the workflow forever.
	f.answerReview(domain.VerdictChangesRequested, "harness reads are still described as observed")
	f.detail()
	if st := f.detail().Run.State; st != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state after the second exhaustion = %q, want needs_attention", st)
	}

	// And then 247d3bc5f — the commit that answers exactly that finding. This
	// is where the real run stopped moving.
	f.goQuiet()
	f.moveHead(incidentHeadResolving)
	f.restart()
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile after the resolving commit: %v", err)
	}

	got := f.detail()
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatalf("no review is bound after the resolving commit\nphases: %v", f.phases())
	}
	freshOf247 := *review.Step.ReviewRunID
	if freshOf247 == freshOfCcefd {
		t.Fatalf("the run is still bound to the review of %s: a verdict about an older head is still the authority",
			incidentHeadHumanFix)
	}
	if n := f.countPhase("human_applied_fix_observed"); n != 2 {
		t.Fatalf("human_applied_fix_observed = %d, want 2 (one per adopted state)\nphases: %v", n, f.phases())
	}
	prev, found, err := f.store.GetReviewRun(f.ctx, freshOfCcefd)
	if err != nil || !found {
		t.Fatalf("GetReviewRun(%s): %v (found=%v)", freshOfCcefd, err, found)
	}
	if prev.SupersededBy != freshOf247 {
		t.Fatalf("review of %s superseded_by = %q, want %q",
			incidentHeadHumanFix, prev.SupersededBy, freshOf247)
	}

	// A restart here must not open a second review for the same state.
	f.restart()
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.detail()
	if id := *reviewStepFrom(f.detail()).Step.ReviewRunID; id != freshOf247 {
		t.Fatalf("authority moved across a restart: %q -> %q", freshOf247, id)
	}
	if n := f.countPhase("human_applied_fix_observed"); n != 2 {
		t.Fatalf("human_applied_fix_observed after restart = %d, want still 2", n)
	}

	// The reviewer approves 247d3bc5f, and the run converges instead of parking.
	f.answerReview(domain.VerdictApproved, "the audit document now states it correctly")
	got = f.detail()
	if st := reviewStepFrom(got).Step.State; st != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed on the approval of %s", st, incidentHeadResolving)
	}
	if st := got.Run.State; st == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q: an approved current head still left the run parked", st)
	}
	// And the run never re-parked on a verdict about an older head.
	if n := f.countPhase(workflowcore.ReasonFixBudgetExhausted); n != 2 {
		t.Fatalf("fix_budget_exhausted checkpoints = %d, want exactly 2 (the two real exhaustions)\nphases: %v",
			n, f.phases())
	}
}

// e2eSpawner creates a REAL session row before handing the record back, because
// workflow_dispatch_checkpoints.session_id is a foreign key into sessions and
// this test exists precisely to exercise the schema's own constraints rather
// than to route around them.
type e2eSpawner struct {
	t        *testing.T
	store    *sqlite.Store
	facts    *fakeSessionFacts
	worktree string
	calls    int
}

func (s *e2eSpawner) Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.calls++
	now := time.Now().UTC()
	rec, err := s.store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: cfg.ProjectID, Kind: cfg.Kind, Harness: cfg.Harness, IssueID: cfg.IssueID,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	rec.Metadata.WorkspacePath = s.worktree
	rec.Metadata.Branch = "feat/engineering-control-center"
	rec.Activity = domain.Activity{State: domain.ActivityActive}
	s.facts.put(rec)
	return rec, len(cfg.Prompt), 0, nil
}
