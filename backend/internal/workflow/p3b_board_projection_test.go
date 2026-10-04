package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3b_board_projection_test.go — P3-B: hierarchy, counts, ordering, filtering
// and paging, asserted on the Board projection itself.
//
// The shapes here are the ones AO actually produced: a repair run created in
// the same project as the run it repairs (which is why it used to appear as a
// third card), an attempt row left behind by a cancelled review cycle (which is
// why a repair that did not exist used to be counted as active), and a board
// whose ordering came out of whatever order the rows were read in.

const p3bRepairOriginPhase = "workflow_repair_run_origin"
const p3bRepairDispatchPhase = "workflow_repair_dispatched"

type p3bFixture struct {
	c     *workflowcore.Coordinator
	store *countingStore
	clk   *fakeClock
}

// countingStore counts the reads the Board actually performs, so §19's "no
// N+1" can be asserted as a SHAPE rather than as a latency number the hardware
// would decide. It overrides only the read methods; everything else is the
// ordinary fake, promoted.
type countingStore struct {
	*fakeStore
	mu sync.Mutex
	n  int
}

func (s *countingStore) count() {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
}

func (s *countingStore) resetReads() {
	s.mu.Lock()
	s.n = 0
	s.mu.Unlock()
}

func (s *countingStore) reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *countingStore) ListWorkflowRuns(ctx context.Context, projectID string) ([]domain.WorkflowRun, error) {
	s.count()
	return s.fakeStore.ListWorkflowRuns(ctx, projectID)
}

func (s *countingStore) GetWorkflowRun(ctx context.Context, id string) (domain.WorkflowRun, bool, error) {
	s.count()
	return s.fakeStore.GetWorkflowRun(ctx, id)
}

func (s *countingStore) ListWorkflowSteps(ctx context.Context, runID string) ([]domain.WorkflowStep, error) {
	s.count()
	return s.fakeStore.ListWorkflowSteps(ctx, runID)
}

func (s *countingStore) ListWorkflowAttempts(ctx context.Context, stepID string) ([]domain.WorkflowAttempt, error) {
	s.count()
	return s.fakeStore.ListWorkflowAttempts(ctx, stepID)
}

func (s *countingStore) ListWorkflowCheckpoints(ctx context.Context, runID string) ([]domain.WorkflowCheckpoint, error) {
	s.count()
	return s.fakeStore.ListWorkflowCheckpoints(ctx, runID)
}

func (s *countingStore) GetLatestWorkflowCheckpointByStep(ctx context.Context, stepID string) (domain.WorkflowCheckpoint, bool, error) {
	s.count()
	return s.fakeStore.GetLatestWorkflowCheckpointByStep(ctx, stepID)
}

func (s *countingStore) ListWorkflowRunIDsByCheckpointPhase(ctx context.Context, phase string) ([]string, error) {
	s.count()
	return s.fakeStore.ListWorkflowRunIDsByCheckpointPhase(ctx, phase)
}

func newP3BFixture(t *testing.T) *p3bFixture {
	t.Helper()
	store := &countingStore{fakeStore: newFakeStore()}
	clk := &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	seq := 0
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Clock: clk.Now,
		NewID: func() string {
			seq++
			return fmt.Sprintf("p3b%d", seq)
		},
	})
	return &p3bFixture{c: c, store: store, clk: clk}
}

func (f *p3bFixture) run(t *testing.T, objective string) string {
	t.Helper()
	created, err := f.c.CreateRun(context.Background(), "proj-1", objective)
	if err != nil {
		t.Fatalf("CreateRun(%q): %v", objective, err)
	}
	return created.Run.ID
}

func (f *p3bFixture) checkpoint(t *testing.T, runID, phase, payload string) {
	t.Helper()
	if _, err := f.store.CreateWorkflowCheckpoint(context.Background(), domain.WorkflowCheckpoint{
		ID: fmt.Sprintf("wfc-%s-%s-%d", runID, phase, f.clk.Now().UnixNano()), WorkflowRunID: runID,
		ProjectID: "proj-1", DurablePhase: phase, PayloadVersion: "v1",
		RetryState: payload, CreatedAt: f.clk.Now(),
	}); err != nil {
		t.Fatalf("checkpoint %s on %s: %v", phase, runID, err)
	}
}

// linkRepair reproduces exactly what the Repair Agent writes: the dispatch
// intent on the ORIGIN, and the origin marker on the repair run itself, both
// before the repair run is started.
func (f *p3bFixture) linkRepair(t *testing.T, originID, repairID string, generation int) {
	t.Helper()
	intent, err := json.Marshal(domain.RepairIntent{
		ID: fmt.Sprintf("ri-%s-%d", originID, generation), WorkflowRunID: originID,
		TargetRunID: originID, RepairRunID: repairID, Generation: generation,
		ProjectID: "proj-1", ConditionReason: workflowcore.ReasonFixBudgetExhausted,
	})
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	f.checkpoint(t, originID, p3bRepairDispatchPhase, string(intent))
	f.checkpoint(t, repairID, p3bRepairOriginPhase,
		fmt.Sprintf(`{"originRunId":%q,"generation":%d}`, originID, generation))
}

// transition moves a run's state and refuses to continue if the durable
// compare-and-swap did not take: a fixture that silently failed to reach the
// state under test is a test that asserts nothing.
func (f *p3bFixture) transition(t *testing.T, runID string, from, to domain.WorkflowRunState) {
	t.Helper()
	ok, err := f.store.UpdateWorkflowRunState(context.Background(), runID, from, to, f.clk.Now())
	if err != nil || !ok {
		t.Fatalf("fixture could not move %s from %s to %s (ok=%v err=%v)", runID, from, to, ok, err)
	}
}

// working puts a run genuinely into its work step, which is what makes its
// derived stage `working` rather than `preparing`.
func (f *p3bFixture) working(t *testing.T, runID string) {
	t.Helper()
	ctx := context.Background()
	f.transition(t, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning)
	steps, err := f.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepPlan:
			f.stepState(t, s, domain.WorkflowStepReady)
			f.stepState(t, domain.WorkflowStep{ID: s.ID, Kind: s.Kind, State: domain.WorkflowStepReady}, domain.WorkflowStepRunning)
			f.stepState(t, domain.WorkflowStep{ID: s.ID, Kind: s.Kind, State: domain.WorkflowStepRunning}, domain.WorkflowStepCompleted)
		case domain.WorkflowStepWork:
			f.stepState(t, s, domain.WorkflowStepReady)
			f.stepState(t, domain.WorkflowStep{ID: s.ID, Kind: s.Kind, State: domain.WorkflowStepReady}, domain.WorkflowStepRunning)
		}
	}
}

func (f *p3bFixture) stepState(t *testing.T, step domain.WorkflowStep, next domain.WorkflowStepState) {
	t.Helper()
	ok, err := f.store.UpdateWorkflowStepState(context.Background(), step.ID, step.State, next, f.clk.Now())
	if err != nil || !ok {
		t.Fatalf("fixture could not move step %s (%s) to %s (ok=%v err=%v)", step.ID, step.Kind, next, ok, err)
	}
}

func (f *p3bFixture) park(t *testing.T, runID, reason string) {
	t.Helper()
	ctx := context.Background()
	run, _, err := f.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if _, err := f.store.UpdateWorkflowRunState(ctx, runID, run.State, domain.WorkflowRunNeedsAttention, f.clk.Now()); err != nil {
		t.Fatalf("park %s: %v", runID, err)
	}
	f.checkpoint(t, runID, reason, "{}")
}

func (f *p3bFixture) board(t *testing.T, q workflowcore.BoardQuery) workflowcore.BoardView {
	t.Helper()
	if q.Retention == 0 {
		q.Retention = boardRetention
	}
	view, err := f.c.ProjectBoardView(context.Background(), "proj-1", q)
	if err != nil {
		t.Fatalf("ProjectBoardView: %v", err)
	}
	return view
}

func boardIDs(view workflowcore.BoardView) []string {
	out := make([]string, 0, len(view.Entries))
	for _, e := range view.Entries {
		out = append(out, e.Run.ID)
	}
	return out
}

// §6/§22: the headline regression. A repair run is an ordinary top-level run in
// the same project, so before P3-B one stopped workflow plus its two repairs
// read as three cards and "3 need attention". It is one card with two repairs
// inline, and one workflow needing attention.
func TestRepairsNestUnderTheirOriginAndAreNotCountedTwice(t *testing.T) {
	f := newP3BFixture(t)
	origin := f.run(t, "ship the thing")
	repair1 := f.run(t, "repair generation 1")
	repair2 := f.run(t, "repair generation 2")
	f.linkRepair(t, origin, repair1, 1)
	f.linkRepair(t, origin, repair2, 2)
	// Both repairs are over: generation 1 failed, generation 2 failed, and the
	// stop is now genuinely the person's. This is the shape §22 names — one
	// incident that used to read as three needing attention.
	for _, repair := range []string{repair1, repair2} {
		f.transition(t, repair, domain.WorkflowRunPending, domain.WorkflowRunRunning)
		f.transition(t, repair, domain.WorkflowRunRunning, domain.WorkflowRunFailed)
	}
	f.park(t, origin, workflowcore.ReasonFixBudgetExhausted)

	view := f.board(t, workflowcore.BoardQuery{})
	if ids := boardIDs(view); len(ids) != 1 || ids[0] != origin {
		t.Fatalf("board cards = %v, want exactly the origin %q", ids, origin)
	}
	entry := view.Entries[0]
	if len(entry.Repairs) != 2 {
		t.Fatalf("inline repairs = %d, want 2", len(entry.Repairs))
	}
	if entry.Repairs[0].Attempt != 1 || entry.Repairs[1].Attempt != 2 {
		t.Fatalf("repair attempts = %d,%d; want 1,2 in generation order",
			entry.Repairs[0].Attempt, entry.Repairs[1].Attempt)
	}
	if entry.Repairs[0].RunID != repair1 || entry.Repairs[1].RunID != repair2 {
		t.Fatalf("repair run ids = %q,%q; a hidden repair must stay reachable",
			entry.Repairs[0].RunID, entry.Repairs[1].RunID)
	}
	for _, r := range entry.Repairs {
		if r.Active {
			t.Fatalf("repair %q reported active while its run is terminal", r.RunID)
		}
		if !r.Failed {
			t.Fatalf("repair %q did not report the failure a person has to act on", r.RunID)
		}
	}
	if view.Counts.NeedsAttention != 1 {
		t.Fatalf("needsAttention = %d, want 1: one origin and its repairs are one incident",
			view.Counts.NeedsAttention)
	}
	if view.Counts.Active != 1 {
		t.Fatalf("active = %d, want 1", view.Counts.Active)
	}
}

// §26: a repair whose origin is not on this board does not vanish. Hiding it
// under nothing would be a run gone missing, which is worse than an untidy
// card.
func TestARepairWithNoOriginOnTheBoardStaysVisibleAndLabelled(t *testing.T) {
	f := newP3BFixture(t)
	repair := f.run(t, "repair of a run in another project")
	f.checkpoint(t, repair, p3bRepairOriginPhase, `{"originRunId":"wf-elsewhere","generation":1}`)

	view := f.board(t, workflowcore.BoardQuery{})
	if len(view.Entries) != 1 || view.Entries[0].Run.ID != repair {
		t.Fatalf("board cards = %v, want the orphaned repair to remain visible", boardIDs(view))
	}
	if view.Entries[0].RepairOfRunID != "wf-elsewhere" {
		t.Fatalf("repairOfRunId = %q, want the origin it names", view.Entries[0].RepairOfRunID)
	}
}

// §31, the reader-side authority filter. A fix lane that identifies attempts by
// COUNT can leave an attempt row behind when a review cycle is cancelled. The
// Board must not read such a row as a live repair: repair activity is the
// repair RUN's lifecycle, and an attempt row is not one.
func TestOrphanFixAttemptDoesNotProduceAnActiveRepair(t *testing.T) {
	f := newP3BFixture(t)
	ctx := context.Background()
	origin := f.run(t, "a run whose review cycle was cancelled")

	// The orphan: an attempt row on the fix step, with no repair intent, no
	// repair run and no lifecycle authority behind it.
	steps, err := f.store.ListWorkflowSteps(ctx, origin)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	var fixStep string
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepFix {
			fixStep = s.ID
		}
	}
	if fixStep == "" {
		t.Fatal("fixture did not reach the state under test: the run has no fix step")
	}
	for i := 0; i < 3; i++ {
		if _, err := f.store.CreateWorkflowAttempt(ctx, fmt.Sprintf("wfa-orphan-%d", i), fixStep, "claude", "sonnet", f.clk.Now()); err != nil {
			t.Fatalf("CreateWorkflowAttempt: %v", err)
		}
	}
	f.park(t, origin, workflowcore.ReasonFixBudgetExhausted)

	view := f.board(t, workflowcore.BoardQuery{})
	entry := view.Entries[0]
	if len(entry.Repairs) != 0 {
		t.Fatalf("orphan attempt rows produced %d inline repairs", len(entry.Repairs))
	}
	if entry.Presentation.AutomaticActionActive {
		t.Fatal("orphan attempt rows made the board claim AO is repairing this run")
	}
	if entry.Presentation.Stage == workflowcore.StageCorrecting {
		t.Fatal("orphan attempt rows moved the parent's stage to correcting with no repair behind it")
	}
	if !entry.Presentation.RequiresHuman {
		t.Fatal("the real stop was hidden by a repair that does not exist")
	}
}

// §21: a person acts on the top of the board, so what needs a person is at the
// top, then what AO is fixing itself, then what is moving, then what is parked,
// then what is over. Within a bucket, newest meaningful activity first — never
// database row order.
func TestBoardOrderingIsStableAndPutsHumanDecisionsFirst(t *testing.T) {
	f := newP3BFixture(t)
	completed := f.run(t, "finished work")
	working := f.run(t, "work in flight")
	stopped := f.run(t, "needs a person")
	f.transition(t, completed, domain.WorkflowRunPending, domain.WorkflowRunRunning)
	f.transition(t, completed, domain.WorkflowRunRunning, domain.WorkflowRunCompleted)
	f.working(t, working)
	f.park(t, stopped, workflowcore.ReasonFixBudgetExhausted)

	first := boardIDs(f.board(t, workflowcore.BoardQuery{}))
	if len(first) != 3 || first[0] != stopped {
		t.Fatalf("board order = %v, want the human decision first", first)
	}
	if first[len(first)-1] != completed {
		t.Fatalf("board order = %v, want the finished run last", first)
	}
	// Stability: the same durable rows must produce the same order every poll.
	for i := 0; i < 3; i++ {
		if got := boardIDs(f.board(t, workflowcore.BoardQuery{})); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("board order changed between identical polls: %v then %v", first, got)
		}
	}
}

// §20: filtering and paging are the daemon's, not the renderer's. A filtered
// board narrows the cards and leaves the counts alone, so the view tabs keep
// saying how much there is.
func TestBoardFilteringAndPagingAreServerSide(t *testing.T) {
	f := newP3BFixture(t)
	stopped := f.run(t, "needs a person")
	f.park(t, stopped, workflowcore.ReasonFixBudgetExhausted)
	for i := 0; i < 5; i++ {
		f.working(t, f.run(t, fmt.Sprintf("ordinary work %d", i)))
	}

	all := f.board(t, workflowcore.BoardQuery{})
	if all.Matched != 6 || len(all.Entries) != 6 {
		t.Fatalf("unfiltered board matched=%d entries=%d, want 6/6", all.Matched, len(all.Entries))
	}

	human := true
	filtered := f.board(t, workflowcore.BoardQuery{RequiresHuman: &human})
	if len(filtered.Entries) != 1 || filtered.Entries[0].Run.ID != stopped {
		t.Fatalf("requiresHuman filter returned %v, want only %q", boardIDs(filtered), stopped)
	}
	if filtered.Counts.Active != all.Counts.Active {
		t.Fatalf("filtering changed the counts: %d then %d", all.Counts.Active, filtered.Counts.Active)
	}

	byStage := f.board(t, workflowcore.BoardQuery{Stages: []workflowcore.Stage{workflowcore.StageWorking}})
	if len(byStage.Entries) != 5 {
		t.Fatalf("stage filter returned %d entries, want the 5 working runs", len(byStage.Entries))
	}

	found := f.board(t, workflowcore.BoardQuery{Search: "ordinary work 3"})
	if len(found.Entries) != 1 {
		t.Fatalf("search returned %d entries, want 1", len(found.Entries))
	}

	page := f.board(t, workflowcore.BoardQuery{Limit: 2})
	if len(page.Entries) != 2 || page.Matched != 6 {
		t.Fatalf("page: entries=%d matched=%d, want 2/6", len(page.Entries), page.Matched)
	}
	next := f.board(t, workflowcore.BoardQuery{Limit: 2, Offset: 2})
	if len(next.Entries) != 2 {
		t.Fatalf("second page returned %d entries, want 2", len(next.Entries))
	}
	if next.Entries[0].Run.ID == page.Entries[0].Run.ID {
		t.Fatal("paging returned the same first card twice: the order is not stable across pages")
	}
}

// §24: a restart must reconstruct the same board. Nothing in the projection may
// come from renderer state, an in-memory timer or a previous message — a second
// coordinator over the same rows has none of those and must still agree.
func TestBoardIsReconstructedIdenticallyByAFreshCoordinator(t *testing.T) {
	f := newP3BFixture(t)
	origin := f.run(t, "ship the thing")
	repair := f.run(t, "its repair")
	f.linkRepair(t, origin, repair, 1)
	f.park(t, origin, workflowcore.ReasonFixBudgetExhausted)
	f.run(t, "a second workflow")

	before := f.board(t, workflowcore.BoardQuery{})

	// A new coordinator over the same store is what a daemon restart is: no
	// carried state, the durable rows and nothing else.
	restarted := workflowcore.New(workflowcore.Deps{
		Store: f.store, Clock: f.clk.Now,
		NewID: func() string { return "restart-id" },
	})
	after, err := restarted.ProjectBoardView(context.Background(), "proj-1", workflowcore.BoardQuery{Retention: boardRetention})
	if err != nil {
		t.Fatalf("ProjectBoardView after restart: %v", err)
	}

	if strings.Join(boardIDs(before), ",") != strings.Join(boardIDs(after), ",") {
		t.Fatalf("restart changed the board: %v then %v", boardIDs(before), boardIDs(after))
	}
	if before.Counts != after.Counts {
		t.Fatalf("restart changed the counts: %+v then %+v", before.Counts, after.Counts)
	}
	for i := range before.Entries {
		b, a := before.Entries[i], after.Entries[i]
		if b.Presentation.Stage != a.Presentation.Stage ||
			b.Presentation.SummaryCode != a.Presentation.SummaryCode ||
			b.Presentation.RequiresHuman != a.Presentation.RequiresHuman ||
			b.Presentation.AutomaticActionActive != a.Presentation.AutomaticActionActive ||
			b.Presentation.RecommendedAction != a.Presentation.RecommendedAction {
			t.Fatalf("restart changed %q's semantics: %+v then %+v", b.Run.ID, b.Presentation, a.Presentation)
		}
		if len(b.Repairs) != len(a.Repairs) {
			t.Fatalf("restart changed %q's inline repairs: %d then %d", b.Run.ID, len(b.Repairs), len(a.Repairs))
		}
		if !b.Presentation.LastMeaningfulActivityAt.Equal(a.Presentation.LastMeaningfulActivityAt) {
			t.Fatalf("restart changed %q's last meaningful activity", b.Run.ID)
		}
	}
}

// §17: a Task's objective is its full specification and may be very long. The
// card carries a title and a bounded summary; nothing is truncated on disk.
func TestLongSpecificationIsSummarisedOnTheCardAndKeptWhole(t *testing.T) {
	f := newP3BFixture(t)
	spec := "Add the backup API\n\n" + strings.Repeat("Detailed acceptance criteria. ", 400)
	id := f.run(t, spec)

	entry := f.board(t, workflowcore.BoardQuery{}).Entries[0]
	if entry.ObjectiveTitle != "Add the backup API" {
		t.Fatalf("title = %q, want the objective's first line", entry.ObjectiveTitle)
	}
	if !entry.ObjectiveTruncated {
		t.Fatal("a 12 KB specification was not reported as summarised")
	}
	if len(entry.ObjectiveSummary) >= len(spec) {
		t.Fatal("the card carried the whole specification")
	}
	stored, _, err := f.store.GetWorkflowRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if stored.Objective != spec {
		t.Fatal("the stored objective was truncated; only the presentation may be")
	}
}

// §19: the Board is read on a timer, so its cost must be proportional to the
// board rather than to the project's whole history. This does not assert a
// latency number — the hardware would decide it — it asserts the SHAPE: the
// number of store reads per run must not grow as more runs are added.
func TestBoardReadCostPerRunDoesNotGrowWithTheBoard(t *testing.T) {
	measure := func(runs int) int {
		f := newP3BFixture(t)
		ctx := context.Background()
		for i := 0; i < runs; i++ {
			f.working(t, f.run(t, fmt.Sprintf("workflow %d", i)))
		}
		f.store.resetReads()
		if _, err := f.c.ProjectBoardView(ctx, "proj-1", workflowcore.BoardQuery{Retention: boardRetention, Limit: 0}); err != nil {
			t.Fatalf("ProjectBoardView: %v", err)
		}
		return f.store.reads()
	}
	// 10 / 100 / 500, the sizes §19 names. The assertion is on the SHAPE — the
	// per-run cost must stay flat — because a latency number would be a
	// property of the machine that ran the test rather than of the projection.
	perRun := map[int]float64{}
	for _, size := range []int{10, 100, 500} {
		reads := measure(size)
		perRun[size] = float64(reads) / float64(size)
		t.Logf("board store reads: %d for %d runs (%.1f per run)", reads, size, perRun[size])
	}
	if perRun[100] > perRun[10]*1.2 || perRun[500] > perRun[10]*1.2 {
		t.Fatalf("per-run read cost grew with the board (%.1f at 10, %.1f at 100, %.1f at 500): there is an N+1",
			perRun[10], perRun[100], perRun[500])
	}
}

// --- master / child hierarchy -------------------------------------------
//
// The fan-out lives on the real SQLite store rather than the in-memory fake:
// the plan row and the workflow_tasks rows ARE the hierarchy, and a double that
// does not have them could not tell whether the Board read them or guessed.

type p3bMasterFixture struct {
	c     *workflowcore.Coordinator
	store *sqlite.Store
	now   time.Time
}

func newP3BMasterFixture(t *testing.T) *p3bMasterFixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	seq := 0
	c := workflowcore.New(workflowcore.Deps{
		Store: st, Projects: st, Clock: func() time.Time { return now },
		NewID: func() string { seq++; return fmt.Sprintf("p3bm%d", seq) },
	})
	return &p3bMasterFixture{c: c, store: st, now: now}
}

func (f *p3bMasterFixture) run(t *testing.T, objective string) string {
	t.Helper()
	created, err := f.c.CreateRun(context.Background(), "proj-1", objective)
	if err != nil {
		t.Fatalf("CreateRun(%q): %v", objective, err)
	}
	return created.Run.ID
}

func (f *p3bMasterFixture) transition(t *testing.T, runID string, from, to domain.WorkflowRunState) {
	t.Helper()
	ok, err := f.store.UpdateWorkflowRunState(context.Background(), runID, from, to, f.now)
	if err != nil || !ok {
		t.Fatalf("fixture could not move %s from %s to %s (ok=%v err=%v)", runID, from, to, ok, err)
	}
}

// working puts a run genuinely into its work step, which is what makes its
// derived stage `working` rather than `preparing`.
func (f *p3bMasterFixture) working(t *testing.T, runID string) {
	t.Helper()
	ctx := context.Background()
	f.transition(t, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning)
	steps, err := f.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		var walk []domain.WorkflowStepState
		switch s.Kind {
		case domain.WorkflowStepPlan:
			walk = []domain.WorkflowStepState{domain.WorkflowStepReady, domain.WorkflowStepRunning, domain.WorkflowStepCompleted}
		case domain.WorkflowStepWork:
			walk = []domain.WorkflowStepState{domain.WorkflowStepReady, domain.WorkflowStepRunning}
		default:
			continue
		}
		from := s.State
		for _, to := range walk {
			ok, err := f.store.UpdateWorkflowStepState(ctx, s.ID, from, to, f.now)
			if err != nil || !ok {
				t.Fatalf("fixture could not move step %s from %s to %s (ok=%v err=%v)", s.ID, from, to, ok, err)
			}
			from = to
		}
	}
}

func (f *p3bMasterFixture) park(t *testing.T, runID, reason string) {
	t.Helper()
	ctx := context.Background()
	run, _, err := f.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	f.transition(t, runID, run.State, domain.WorkflowRunNeedsAttention)
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-park-" + runID, WorkflowRunID: runID, ProjectID: "proj-1",
		DurablePhase: reason, PayloadVersion: "v1", RetryState: "{}", CreatedAt: f.now,
	}); err != nil {
		t.Fatalf("park checkpoint: %v", err)
	}
}

// child creates one dispatched task child: a run whose parent_workflow_id is
// the master, with the same step chain any task run has.
func (f *p3bMasterFixture) child(t *testing.T, masterID, objective string, ordinal int) string {
	t.Helper()
	ctx := context.Background()
	id := fmt.Sprintf("%s-child-%d", masterID, ordinal)
	parent := masterID
	run := domain.WorkflowRun{
		ID: id, ProjectID: "proj-1", Objective: objective, State: domain.WorkflowRunPending,
		PolicyVersion: "v1", PolicySnapshot: "{}", ParentWorkflowID: &parent,
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	kinds := []domain.WorkflowStepKind{
		domain.WorkflowStepPlan, domain.WorkflowStepWork, domain.WorkflowStepReview,
		domain.WorkflowStepFix, domain.WorkflowStepVerify, domain.WorkflowStepAdvance,
	}
	steps := make([]domain.WorkflowStep, 0, len(kinds))
	for i, kind := range kinds {
		steps = append(steps, domain.WorkflowStep{
			ID: fmt.Sprintf("%s-step-%d", id, i), WorkflowRunID: id, Kind: kind,
			Ordinal: int64(i + 1), State: domain.WorkflowStepPending, ArtifactJSON: "{}",
			CreatedAt: f.now, UpdatedAt: f.now,
		})
	}
	if _, _, err := f.store.CreateWorkflowRun(ctx, run, steps); err != nil {
		t.Fatalf("CreateWorkflowRun child: %v", err)
	}
	return id
}

// master builds a master run with its plan row and one planned task per child
// objective, each already dispatched to a child run.
func (f *p3bMasterFixture) master(t *testing.T, objective string, childObjectives []string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	master := f.run(t, objective)
	if _, err := f.store.CreateWorkflowPlan(ctx, master, domain.WorkflowPlanApprovalAuto, "v1", f.now); err != nil {
		t.Fatalf("CreateWorkflowPlan: %v", err)
	}
	children := make([]string, 0, len(childObjectives))
	tasks := make([]domain.WorkflowTask, 0, len(childObjectives))
	for i, childObjective := range childObjectives {
		childID := f.child(t, master, childObjective, i+1)
		children = append(children, childID)
		child := childID
		tasks = append(tasks, domain.WorkflowTask{
			ID:            fmt.Sprintf("%s-task-%d", master, i+1),
			WorkflowRunID: master,
			PlanStepID:    fmt.Sprintf("step-%d", i+1),
			Ordinal:       int64(i + 1),
			Title:         childObjective,
			// A non-empty description is a schema CHECK, not a nicety: an empty
			// one makes the INSERT OR IGNORE drop the row silently.
			Description: childObjective + " description",
			// Planned first, dispatched second: SetWorkflowTaskExecutionRun is
			// conditional on `eligible`, which is what makes dispatch race-free.
			State:     domain.WorkflowTaskEligible,
			ScopeJSON: "{}", AcceptanceCriteriaJSON: "[]", VerifyJSON: "{}",
			// The plan's current revision: a task minted for a superseded plan
			// is structurally invisible to every reader, the Board included.
			PlanRevision:   1,
			ExecutionRunID: &child,
			CreatedAt:      f.now, UpdatedAt: f.now,
		})
	}
	if err := f.store.InsertWorkflowTasks(ctx, tasks); err != nil {
		t.Fatalf("InsertWorkflowTasks: %v", err)
	}
	// The dispatch link is its own write in the real store, exactly as it is in
	// production: a task is planned first and pointed at a run when it is
	// dispatched.
	for i, task := range tasks {
		ok, err := f.store.SetWorkflowTaskExecutionRun(ctx, task.ID, children[i], f.now)
		if err != nil || !ok {
			t.Fatalf("SetWorkflowTaskExecutionRun(%s): ok=%v err=%v", task.ID, ok, err)
		}
	}
	stored, err := f.store.ListWorkflowTasks(ctx, master)
	if err != nil || len(stored) != len(children) {
		t.Fatalf("fixture did not reach the state under test: %d tasks stored, want %d (err=%v)",
			len(stored), len(children), err)
	}
	f.transition(t, master, domain.WorkflowRunPending, domain.WorkflowRunRunning)
	return master, children
}

func (f *p3bMasterFixture) board(t *testing.T) workflowcore.BoardView {
	t.Helper()
	view, err := f.c.ProjectBoardView(context.Background(), "proj-1", workflowcore.BoardQuery{Retention: boardRetention})
	if err != nil {
		t.Fatalf("ProjectBoardView: %v", err)
	}
	return view
}

// §4/§5: a master run is ONE card with its tasks under it, and its headline is
// decided by an explicit authority ordering rather than by whichever child row
// was written last. A child that needs a person makes the parent say so, and
// says WHICH stop it is.
func TestParentAggregatesItsChildrenByAuthorityNotRecency(t *testing.T) {
	f := newP3BMasterFixture(t)
	master, children := f.master(t, "the objective", []string{"task 1", "task 2"})
	blocked, busy := children[0], children[1]
	f.working(t, busy)
	f.park(t, blocked, workflowcore.ReasonFixBudgetExhausted)

	view := f.board(t)
	if ids := boardIDs(view); len(ids) != 1 || ids[0] != master {
		t.Fatalf("board cards = %v, want only the master %q: children are not cards", ids, master)
	}
	entry := view.Entries[0]
	if len(entry.ChildTasks) != 2 {
		t.Fatalf("child tasks = %d, want 2", len(entry.ChildTasks))
	}
	if !entry.Presentation.RequiresHuman {
		t.Fatal("a child stopped on a person did not propagate to its parent")
	}
	if entry.Presentation.Stage != workflowcore.StageNeedsAttention {
		t.Fatalf("parent stage = %q, want needs_attention while a child needs one", entry.Presentation.Stage)
	}
	if entry.Presentation.SummaryCode != workflowcore.ReasonFixBudgetExhausted {
		t.Fatalf("parent summary = %q, want the child's own stop named", entry.Presentation.SummaryCode)
	}
	if violations := workflowcore.CheckPresentationInvariants(entry.Presentation); len(violations) > 0 {
		t.Fatalf("aggregated parent violates the projection invariants: %v", violations)
	}
	var reachable bool
	for _, child := range entry.ChildTasks {
		if child.RunID == blocked {
			reachable = true
		}
	}
	if !reachable {
		t.Fatal("the child a person has to act on is not reachable from the parent card")
	}
	if view.Counts.NeedsAttention != 1 {
		t.Fatalf("needsAttention = %d, want 1: a parent and its children are one workflow", view.Counts.NeedsAttention)
	}
}

// Without a stopped child, a working child is what the parent's headline says:
// "running" is the vaguer of two true answers, and the parent's own row is the
// vaguest.
func TestParentShowsWhatItsWorkingChildIsDoing(t *testing.T) {
	f := newP3BMasterFixture(t)
	_, children := f.master(t, "the objective", []string{"task 1"})
	f.working(t, children[0])

	entry := f.board(t).Entries[0]
	if entry.Presentation.Stage != workflowcore.StageWorking {
		t.Fatalf("parent stage = %q, want working — what its child is actually doing", entry.Presentation.Stage)
	}
	if entry.Presentation.RequiresHuman {
		t.Fatal("a parent whose child is working claimed a person was needed")
	}
}
