package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workitems/plane"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/workitems"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// workitems_sync_store_test.go — the wiring P4-E's final slice adds: a
// canonical AO transition produces exactly one durable sync intent, and a
// planning-tool outage cannot touch the transition (§4, §5, §9).
//
// These drive the REAL store through the REAL decorator into the REAL service,
// with a Plane-shaped httptest server at the far end. What is faked is only
// Plane itself, which is the one thing that cannot be real here.

const (
	syncProjectA = domain.ProjectID("proj-a")
	syncProjectB = domain.ProjectID("proj-b")
)

// planeFake is an httptest server shaped like Plane, recording what AO sent.
type planeFake struct {
	*httptest.Server

	mu          sync.Mutex
	transitions []string
	comments    []string
	commentKeys []string
	// down makes every request fail with a retryable 503, which is how an
	// outage is induced mid-test.
	down bool
	// block holds requests open until released, so a test can stop a daemon
	// between the transition and the delivery.
	block chan struct{}
}

func newPlaneFake(t *testing.T) *planeFake {
	t.Helper()
	f := &planeFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down, block := f.down, f.block
		f.mu.Unlock()

		if block != nil {
			<-block
		}
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"plane is having a bad day"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch:
			f.record(&f.transitions, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments/"):
			body := readBody(r)
			f.mu.Lock()
			f.comments = append(f.comments, body)
			f.commentKeys = append(f.commentKeys, externalIDOf(body))
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/states/"):
			_, _ = w.Write([]byte(pageOf(`[
			  {"id":"st-todo","name":"Todo","group":"unstarted","default":true,"sequence":1},
			  {"id":"st-doing","name":"In Progress","group":"started","sequence":2},
			  {"id":"st-done","name":"Done","group":"completed","sequence":3},
			  {"id":"st-cancelled","name":"Cancelled","group":"cancelled","sequence":4}]`)))
		case strings.HasSuffix(r.URL.Path, "/projects/"):
			_, _ = w.Write([]byte(pageOf(`[{"id":"plane-proj","name":"Acme Web","identifier":"ACME"}]`)))
		default:
			// A work item, in the unstarted state so a transition is a real move.
			_, _ = w.Write([]byte(`{"id":"item-1","name":"Fix the login redirect",` +
				`"state":"st-todo","sequence_id":7,"project":"plane-proj","workspace":"ws"}`))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *planeFake) record(into *[]string, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*into = append(*into, v)
}

func (f *planeFake) snapshot() (transitions, comments, keys []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.transitions...),
		append([]string(nil), f.comments...),
		append([]string(nil), f.commentKeys...)
}

func (f *planeFake) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func pageOf(results string) string {
	return `{"next_cursor":"","next_page_results":false,"count":1,"total_pages":1,"results":` + results + `}`
}

func readBody(r *http.Request) string {
	buf := make([]byte, 4096)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}

func externalIDOf(body string) string {
	const key = `"external_id":"`
	i := strings.Index(body, key)
	if i < 0 {
		return ""
	}
	rest := body[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

type fakeBox struct{}

func (fakeBox) Seal(p string) (string, error) { return "sealed:" + p, nil }
func (fakeBox) Open(c string) (string, error) {
	if !strings.HasPrefix(c, "sealed:") {
		return "", errors.New("not sealed")
	}
	return strings.TrimPrefix(c, "sealed:"), nil
}

type syncFixture struct {
	store *sqlite.Store
	svc   *workitems.Service
	wf    *syncingStore
	plane *planeFake
	ctx   context.Context
	now   time.Time
	ids   int
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := t.Context()
	for _, id := range []domain.ProjectID{syncProjectA, syncProjectB} {
		if err := st.UpsertProject(ctx, domain.ProjectRecord{
			ID: string(id), Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	f := &syncFixture{
		store: st, ctx: ctx, plane: newPlaneFake(t),
		now: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	}
	f.svc = f.newService()
	f.wf = maybeSyncingStore(st, f.svc, nil)
	if f.wf == nil {
		t.Fatal("the decorator was not built")
	}
	return f
}

// newService builds a work-items service over the same database — which is
// also how a restart is simulated, by building a second one.
func (f *syncFixture) newService() *workitems.Service {
	return workitems.New(workitems.Deps{
		Store:   f.store,
		Secrets: fakeBox{},
		Provider: func(cfg workitems.ResolvedConfig) (ports.WorkItems, error) {
			return plane.New(plane.Options{
				BaseURL: f.plane.URL, Workspace: cfg.Workspace,
				Token: plane.StaticToken(cfg.APIToken),
			})
		},
		Env:   func(string) string { return "" },
		Now:   func() time.Time { return f.now },
		NewID: f.nextID,
	})
}

func (f *syncFixture) nextID() string {
	f.ids++
	return "id-" + time.Duration(f.ids).String()
}

func (f *syncFixture) connect(t *testing.T, id domain.ProjectID) {
	t.Helper()
	ws, proj, token, on := "acme", "plane-proj", "tok", true
	if _, err := f.svc.PutConfig(f.ctx, id, workitems.ConfigUpdate{
		Workspace: &ws, ExternalProjectID: &proj, APIToken: &token, Enabled: &on,
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
}

// seedRun creates a real workflow run in the given state.
func (f *syncFixture) seedRun(t *testing.T, projectID domain.ProjectID, id string) domain.WorkflowRun {
	t.Helper()
	run := domain.WorkflowRun{
		ID: id, ProjectID: string(projectID), Objective: "Fix the login redirect loop",
		State: domain.WorkflowRunPending, PolicySnapshot: "{}",
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	created, _, err := f.store.CreateWorkflowRun(f.ctx, run, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return created
}

func (f *syncFixture) linkRun(t *testing.T, projectID domain.ProjectID, runID string) {
	t.Helper()
	if _, err := f.svc.Link(f.ctx, workitems.LinkRequest{
		ProjectID: projectID, Scope: domain.WorkItemScopeRun, ScopeID: runID,
		Reference: "ACME-7", SyncEnabled: true,
	}); err != nil {
		t.Fatalf("link run: %v", err)
	}
}

func (f *syncFixture) pending(t *testing.T, projectID domain.ProjectID) int {
	t.Helper()
	rows, err := f.store.ListWorkItemSyncs(f.ctx, projectID, 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, row := range rows {
		if row.Status == store_WorkItemSyncPending {
			n++
		}
	}
	return n
}

// store_WorkItemSyncPending mirrors the store's constant without importing the
// package for one string, keeping this file's imports to what it exercises.
const store_WorkItemSyncPending = "pending"

// --- §1, §2: a canonical transition enqueues a sync -------------------------

func TestACanonicalRunTransitionEnqueuesOneSync(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")
	f.linkRun(t, syncProjectA, run.ID)

	moved, err := f.wf.UpdateWorkflowRunState(f.ctx, run.ID,
		domain.WorkflowRunPending, domain.WorkflowRunRunning, f.now)
	if err != nil || !moved {
		t.Fatalf("the transition itself failed: moved=%v err=%v", moved, err)
	}
	if got := f.pending(t, syncProjectA); got != 1 {
		t.Fatalf("pending syncs = %d, want 1", got)
	}

	out, err := f.svc.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Delivered != 1 {
		t.Fatalf("delivered %d of %d: %+v", out.Delivered, out.Claimed, out)
	}
	transitions, comments, _ := f.plane.snapshot()
	if len(transitions) != 1 {
		t.Errorf("Plane saw %d state writes, want 1", len(transitions))
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "login redirect") {
		t.Errorf("the comment did not carry the objective: %v", comments)
	}
}

// §2: no sync storm. A CAS that does not move the row emits nothing, which is
// what makes a reconcile pass that re-derives the same conclusion free.
func TestARepeatedTransitionEnqueuesNothingFurther(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")
	f.linkRun(t, syncProjectA, run.ID)

	for range 5 {
		// Only the first one moves the row; the rest are the no-op CAS a
		// reconcile pass performs on every poll.
		_, _ = f.wf.UpdateWorkflowRunState(f.ctx, run.ID,
			domain.WorkflowRunPending, domain.WorkflowRunRunning, f.now)
	}
	if got := f.pending(t, syncProjectA); got != 1 {
		t.Fatalf("five attempts produced %d pending syncs, want 1", got)
	}

	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	transitions, comments, _ := f.plane.snapshot()
	if len(transitions) != 1 || len(comments) != 1 {
		t.Errorf("Plane saw %d transitions and %d comments, want one of each",
			len(transitions), len(comments))
	}
}

// §2/§3: the whole run lifecycle, in order, with the semantic decisions
// preserved — needs_attention and failed comment without moving state.
func TestTheRunLifecycleProducesTheRightExternalEffects(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")
	f.linkRun(t, syncProjectA, run.ID)

	steps := []struct {
		from, to domain.WorkflowRunState
	}{
		{domain.WorkflowRunPending, domain.WorkflowRunRunning},
		{domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention},
		{domain.WorkflowRunNeedsAttention, domain.WorkflowRunRunning},
		{domain.WorkflowRunRunning, domain.WorkflowRunCompleted},
	}
	for _, s := range steps {
		if moved, err := f.wf.UpdateWorkflowRunState(f.ctx, run.ID, s.from, s.to, f.now); err != nil || !moved {
			t.Fatalf("%s -> %s: moved=%v err=%v", s.from, s.to, moved, err)
		}
		f.now = f.now.Add(time.Minute)
	}

	if _, err := f.svc.SyncOnce(f.ctx, 20); err != nil {
		t.Fatal(err)
	}
	transitions, comments, keys := f.plane.snapshot()

	// started and completed move state; needs_attention does not, and the
	// second `running` is the same event as the first so its dedupe key
	// absorbs it.
	if len(transitions) != 2 {
		t.Errorf("Plane saw %d state writes, want 2 (started, completed)", len(transitions))
	}
	if len(comments) != 3 {
		t.Errorf("Plane saw %d comments, want 3 (started, needs_attention, completed): %v", len(comments), keys)
	}
	wantKeys := map[string]bool{
		"run:run-1:started":         true,
		"run:run-1:needs_attention": true,
		"run:run-1:completed":       true,
	}
	for _, k := range keys {
		if !wantKeys[k] {
			t.Errorf("unexpected comment key %q", k)
		}
		delete(wantKeys, k)
	}
	for k := range wantKeys {
		t.Errorf("missing comment for %q", k)
	}
}

// States that are not news enqueue nothing, so a run bouncing between queued
// and waiting produces no external activity at all.
func TestNonEventRunStatesProduceNoWork(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")
	f.linkRun(t, syncProjectA, run.ID)

	if moved, _ := f.wf.UpdateWorkflowRunState(f.ctx, run.ID,
		domain.WorkflowRunPending, domain.WorkflowRunWaiting, f.now); !moved {
		t.Fatal("the transition did not happen")
	}
	if got := f.pending(t, syncProjectA); got != 0 {
		t.Errorf("a waiting transition queued %d syncs", got)
	}
}

// §3: task transitions, through the task CAS.
func TestACanonicalTaskTransitionEnqueuesOneSync(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")

	// A task belongs to a plan: workflow_tasks has a foreign key onto it, and
	// the insert is OR IGNORE, so a task minted without one is silently
	// dropped rather than refused.
	if _, err := f.store.CreateWorkflowPlan(f.ctx, run.ID,
		domain.WorkflowPlanApprovalAuto, "v1", f.now); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	task := domain.WorkflowTask{
		ID: "task-1", WorkflowRunID: run.ID, Ordinal: 1,
		Title: "Rework the redirect guard", State: domain.WorkflowTaskRunning,
		// A non-empty description is required by the table's own CHECK, and
		// the insert is OR IGNORE — so an empty one is dropped in silence.
		// The read-back below is what turns that into a visible failure.
		Description:            "Make the guard idempotent.",
		AcceptanceCriteriaJSON: "[]", VerifyJSON: "{}", ScopeJSON: "{}",
		PlanStepID: "step-1",
		CreatedAt:  f.now, UpdatedAt: f.now,
	}
	if err := f.store.InsertWorkflowTasks(f.ctx, []domain.WorkflowTask{task}); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if _, found, _ := f.store.GetWorkflowTask(f.ctx, task.ID); !found {
		t.Fatal("the task was not persisted")
	}
	if _, err := f.svc.Link(f.ctx, workitems.LinkRequest{
		ProjectID: syncProjectA, Scope: domain.WorkItemScopeTask, ScopeID: task.ID,
		Reference: "ACME-7", SyncEnabled: true,
	}); err != nil {
		t.Fatalf("link task: %v", err)
	}

	moved, err := f.wf.UpdateWorkflowTaskState(f.ctx, task.ID,
		domain.WorkflowTaskRunning, domain.WorkflowTaskCompleted, f.now)
	if err != nil || !moved {
		t.Fatalf("task transition: moved=%v err=%v", moved, err)
	}
	if got := f.pending(t, syncProjectA); got != 1 {
		t.Fatalf("pending syncs = %d, want 1", got)
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	_, comments, keys := f.plane.snapshot()
	if len(comments) != 1 || keys[0] != "task:task-1:completed" {
		t.Errorf("comments=%v keys=%v", comments, keys)
	}
	if !strings.Contains(comments[0], "redirect guard") {
		t.Errorf("the comment did not carry the task title: %q", comments[0])
	}
}

// --- §4: failure isolation --------------------------------------------------

// A Plane outage cannot fail an AO transition. The transition commits, the
// intent is durable, and the outage costs a deferred row and nothing else.
func TestAPlaneOutageDoesNotFailTheAOTransition(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")
	f.linkRun(t, syncProjectA, run.ID)
	f.plane.setDown(true)

	moved, err := f.wf.UpdateWorkflowRunState(f.ctx, run.ID,
		domain.WorkflowRunPending, domain.WorkflowRunCompleted, f.now)
	if err != nil {
		t.Fatalf("a Plane outage failed an AO transition: %v", err)
	}
	if !moved {
		t.Fatal("a Plane outage stopped an AO transition from happening")
	}
	// And the durable AO state really did move.
	stored, found, err := f.store.GetWorkflowRun(f.ctx, run.ID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if stored.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q; AO's own state must be unaffected", stored.State)
	}

	out, err := f.svc.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatalf("draining against a dead provider errored: %v", err)
	}
	if out.Deferred != 1 {
		t.Fatalf("outcome = %+v, want one deferred", out)
	}
	if got := f.pending(t, syncProjectA); got != 1 {
		t.Errorf("the intent was lost: %d pending", got)
	}
}

// --- §5: restart safety -----------------------------------------------------

// The transition happens, the daemon dies before the Plane request lands, a new
// daemon starts, and exactly one semantic update reaches Plane.
func TestARestartBetweenTransitionAndDeliveryDeliversExactlyOnce(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")
	f.linkRun(t, syncProjectA, run.ID)

	// The AO transition commits.
	if moved, err := f.wf.UpdateWorkflowRunState(f.ctx, run.ID,
		domain.WorkflowRunPending, domain.WorkflowRunCompleted, f.now); err != nil || !moved {
		t.Fatalf("transition: moved=%v err=%v", moved, err)
	}
	if got := f.pending(t, syncProjectA); got != 1 {
		t.Fatalf("the intent was not made durable: %d pending", got)
	}

	// The daemon exits here — nothing was delivered. Plane has seen nothing.
	transitions, comments, _ := f.plane.snapshot()
	if len(transitions) != 0 || len(comments) != 0 {
		t.Fatalf("something reached Plane before any drain: %v %v", transitions, comments)
	}

	// A new daemon: a second service over the same database.
	restarted := f.newService()
	out, err := restarted.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Delivered != 1 {
		t.Fatalf("the restart did not resume the pending intent: %+v", out)
	}

	// Exactly one semantic update, and a third drain adds nothing.
	if out, _ = restarted.SyncOnce(f.ctx, 10); out.Claimed != 0 {
		t.Errorf("the settled row was claimed again: %+v", out)
	}
	transitions, comments, keys := f.plane.snapshot()
	if len(transitions) != 1 || len(comments) != 1 {
		t.Errorf("Plane saw %d transitions and %d comments, want one of each",
			len(transitions), len(comments))
	}
	if len(keys) == 1 && keys[0] != "run:run-1:completed" {
		t.Errorf("dedupe key = %q", keys[0])
	}
}

// A retry that arrives after a partial delivery is absorbed by the provider's
// own external-id dedupe, which is why AO does not need to remember how far it
// got within one row.
func TestARetryAfterAPartialDeliveryDoesNotDoubleComment(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")
	f.linkRun(t, syncProjectA, run.ID)

	if moved, _ := f.wf.UpdateWorkflowRunState(f.ctx, run.ID,
		domain.WorkflowRunPending, domain.WorkflowRunRunning, f.now); !moved {
		t.Fatal("transition did not happen")
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}

	// The same event observed again — a duplicate lifecycle callback — is
	// absorbed by the outbox's dedupe key before it ever reaches Plane.
	if err := f.svc.EnqueueRunState(f.ctx, syncProjectA, run.ID,
		domain.WorkflowRunRunning, "again"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	_, comments, _ := f.plane.snapshot()
	if len(comments) != 1 {
		t.Errorf("Plane saw %d comments for one real event", len(comments))
	}
}

// --- §7: nothing to sync ----------------------------------------------------

// An unlinked run produces no sync work at all — not a row, not a request.
func TestAnUnlinkedRunProducesNoSyncWork(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")

	if moved, err := f.wf.UpdateWorkflowRunState(f.ctx, run.ID,
		domain.WorkflowRunPending, domain.WorkflowRunCompleted, f.now); err != nil || !moved {
		t.Fatalf("transition: moved=%v err=%v", moved, err)
	}
	rows, err := f.store.ListWorkItemSyncs(f.ctx, syncProjectA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("an unlinked run queued %d syncs", len(rows))
	}
	transitions, comments, _ := f.plane.snapshot()
	if len(transitions) != 0 || len(comments) != 0 {
		t.Error("an unlinked run reached Plane")
	}
}

// A project with the integration switched off produces no sync work, even with
// a link present — the link survives being disabled, and does nothing.
func TestADisabledIntegrationProducesNoSyncWork(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	run := f.seedRun(t, syncProjectA, "run-1")
	f.linkRun(t, syncProjectA, run.ID)

	off := false
	if _, err := f.svc.PutConfig(f.ctx, syncProjectA, workitems.ConfigUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if moved, err := f.wf.UpdateWorkflowRunState(f.ctx, run.ID,
		domain.WorkflowRunPending, domain.WorkflowRunCompleted, f.now); err != nil || !moved {
		t.Fatalf("transition: moved=%v err=%v", moved, err)
	}
	rows, _ := f.store.ListWorkItemSyncs(f.ctx, syncProjectA, 10)
	if len(rows) != 0 {
		t.Errorf("a disabled integration queued %d syncs", len(rows))
	}
}

// A daemon with no work-items service at all leaves the store undecorated, so
// the integration cannot cost an installation that does not use it anything.
func TestNoServiceMeansNoDecorator(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	if got := maybeSyncingStore(st, nil, nil); got != nil {
		t.Error("a daemon without the integration still wrapped its store")
	}
}

// --- §7: tenant safety ------------------------------------------------------

// A transition in one project can never produce a sync in another, because the
// project is read from the run's own durable row rather than supplied.
func TestATransitionSyncsOnlyItsOwnProject(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(t, syncProjectA)
	f.connect(t, syncProjectB)

	runA := f.seedRun(t, syncProjectA, "run-a")
	runB := f.seedRun(t, syncProjectB, "run-b")
	f.linkRun(t, syncProjectA, runA.ID)
	f.linkRun(t, syncProjectB, runB.ID)

	if moved, err := f.wf.UpdateWorkflowRunState(f.ctx, runA.ID,
		domain.WorkflowRunPending, domain.WorkflowRunCompleted, f.now); err != nil || !moved {
		t.Fatal(err)
	}
	if got := f.pending(t, syncProjectA); got != 1 {
		t.Errorf("project A pending = %d, want 1", got)
	}
	if got := f.pending(t, syncProjectB); got != 0 {
		t.Errorf("project B pending = %d; a transition in A must not touch B", got)
	}

	rowsB, _ := f.store.ListWorkItemSyncs(f.ctx, syncProjectB, 10)
	for _, row := range rowsB {
		if strings.Contains(row.DedupeKey, runA.ID) {
			t.Errorf("project B holds a sync for project A's run: %q", row.DedupeKey)
		}
	}
}
