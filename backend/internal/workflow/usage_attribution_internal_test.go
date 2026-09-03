package workflow

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type recordingWindowStore struct {
	windows []domain.UsageAttributionWindow
	err     error
}

func (r *recordingWindowStore) OpenUsageAttributionWindow(_ stdctx.Context, w domain.UsageAttributionWindow) error {
	if r.err != nil {
		return r.err
	}
	r.windows = append(r.windows, w)
	return nil
}

func attributionCoordinator(t *testing.T, store *recordingWindowStore, now time.Time) *Coordinator {
	t.Helper()
	c := New(Deps{Clock: func() time.Time { return now }})
	c.usageWindows = store
	return c
}

func TestUsageWindowDedupeKeyIsAFunctionOfTheObligationOnly(t *testing.T) {
	// The exactly-once property of role attribution rests entirely on this. A
	// dispatch is replayed by failover, by resume after a restart, and by a
	// wake; each replay carries a different clock. If the clock reached the
	// key, each replay would open a NEW window and split the role's tokens.
	run := domain.WorkflowRun{ID: "wf-1", ProjectID: "p"}
	spec := usageWindowSpec{
		SessionID: "s-1", Role: domain.WorkflowRoleWorker, Run: run,
		StepID: "step-1", AttemptID: "att-1", AttemptOrdinal: 1, Cycle: 0,
	}
	first := usageWindowDedupeKey(spec, domain.SessionSubject("s-1"))

	spec.OpenedAt = time.Now().Add(72 * time.Hour)
	spec.Harness, spec.Provider, spec.Model = "codex", "openai", "gpt-5"
	if again := usageWindowDedupeKey(spec, domain.SessionSubject("s-1")); again != first {
		t.Fatal("the dedupe key moved with the clock or with display metadata; a replayed dispatch would split the role's tokens")
	}

	// But a genuinely different obligation must be a different window.
	for name, mutate := range map[string]func(*usageWindowSpec){
		"a different repair cycle": func(s *usageWindowSpec) { s.Cycle = 1 },
		"a different failover hop": func(s *usageWindowSpec) { s.AttemptOrdinal = 2 },
		"a different attempt":      func(s *usageWindowSpec) { s.AttemptID = "att-2" },
		"a different role":         func(s *usageWindowSpec) { s.Role = domain.WorkflowRoleFixWorker },
		"a different step":         func(s *usageWindowSpec) { s.StepID = "step-2" },
	} {
		other := spec
		mutate(&other)
		if usageWindowDedupeKey(other, domain.SessionSubject("s-1")) == first {
			t.Fatalf("%s produced the same window key; two obligations would share one attribution", name)
		}
	}
	if usageWindowDedupeKey(spec, domain.SessionSubject("s-2")) == first {
		t.Fatal("a different session produced the same window key")
	}
}

func TestOpenUsageWindow_DerivesProviderAndParentFromTheRun(t *testing.T) {
	parent := "wf-parent"
	task := "task-7"
	run := domain.WorkflowRun{
		ID: "wf-child", ProjectID: "proj", ParentWorkflowID: &parent, PlannedTaskID: &task,
	}
	now := time.Unix(1700000000, 0).UTC()
	store := &recordingWindowStore{}
	c := attributionCoordinator(t, store, now)

	c.openUsageWindow(stdctx.Background(), usageWindowSpec{
		SessionID: "s-1", Role: domain.WorkflowRoleWorker, Run: run,
		StepID: "step-1", Harness: "claude-code", OpenedAt: now,
	})
	if len(store.windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(store.windows))
	}
	w := store.windows[0]
	if w.Provider != "anthropic" {
		t.Fatalf("provider = %q, want the static harness->vendor mapping", w.Provider)
	}
	if w.ParentWorkflowRunID != parent {
		t.Fatalf("parent = %q, want %q — a child's spend must be reachable from its parent's budget", w.ParentWorkflowRunID, parent)
	}
	if w.TaskID != task {
		t.Fatalf("taskId = %q, want %q", w.TaskID, task)
	}
	if w.ProjectID != "proj" {
		t.Fatalf("projectId = %q", w.ProjectID)
	}
}

func TestOpenUsageWindow_IsBestEffortAndNeverFailsADispatch(t *testing.T) {
	// Attribution is observability. Losing a workflow because a telemetry row
	// could not be written would be a far worse outcome than a coarser
	// breakdown, so the write is best-effort by construction: it returns
	// nothing, and a nil store is an ordinary state rather than a degraded one.
	now := time.Unix(1700000000, 0).UTC()
	failing := attributionCoordinator(t, &recordingWindowStore{err: errStoreDown}, now)
	failing.openUsageWindow(stdctx.Background(), usageWindowSpec{
		SessionID: "s", Role: domain.WorkflowRoleWorker, Run: domain.WorkflowRun{ID: "wf"},
	})

	unwired := New(Deps{Clock: func() time.Time { return now }})
	unwired.usageWindows = nil
	unwired.openUsageWindow(stdctx.Background(), usageWindowSpec{
		SessionID: "s", Role: domain.WorkflowRoleWorker, Run: domain.WorkflowRun{ID: "wf"},
	})
}

func TestOpenUsageWindow_RefusesAWindowWithNoSubject(t *testing.T) {
	// A window with no session or no role could never resolve an event, and
	// storing one would put a row in the ledger that nothing can ever explain.
	now := time.Unix(1700000000, 0).UTC()
	store := &recordingWindowStore{}
	c := attributionCoordinator(t, store, now)
	c.openUsageWindow(stdctx.Background(), usageWindowSpec{Role: domain.WorkflowRoleWorker, Run: domain.WorkflowRun{ID: "wf"}})
	c.openUsageWindow(stdctx.Background(), usageWindowSpec{SessionID: "s", Run: domain.WorkflowRun{ID: "wf"}})
	if len(store.windows) != 0 {
		t.Fatalf("windows = %d, want none", len(store.windows))
	}
}

func TestPlannerUsageSubjectNamesOneInvocation(t *testing.T) {
	// The planner shells out to `claude --print`, so it spends real provider
	// tokens under its own subject -- never a session's, and never shared with a
	// previous attempt. A retry is its own invocation because a retried planner
	// call spends its own tokens.
	first := plannerUsageSubject("wf-1", 1)
	if first.Kind != domain.UsageSubjectPlannerInvocation {
		t.Fatalf("kind = %q, want planner_invocation", first.Kind)
	}
	if !first.Valid() {
		t.Fatalf("subject %+v must be storable", first)
	}
	if second := plannerUsageSubject("wf-1", 2); second == first {
		t.Fatal("a retried planner attempt must be its own subject, not an heir to the previous one's tokens")
	}
	if other := plannerUsageSubject("wf-2", 1); other == first {
		t.Fatal("two runs must not share a planner subject")
	}
}

func TestUsageWindowSubjectDefaultsToTheSession(t *testing.T) {
	// The worker path names only a session id, exactly as it did before subjects
	// existed. It must still key on the session subject rather than becoming
	// invalid.
	now := time.Unix(1700000000, 0).UTC()
	store := &recordingWindowStore{}
	c := attributionCoordinator(t, store, now)
	c.openUsageWindow(stdctx.Background(), usageWindowSpec{
		SessionID: "sess-1", Role: domain.WorkflowRoleWorker, Run: domain.WorkflowRun{ID: "wf"},
	})
	if len(store.windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(store.windows))
	}
	if got := store.windows[0].Subject(); got != domain.SessionSubject("sess-1") {
		t.Fatalf("subject = %+v, want the session subject", got)
	}
}

func TestUsageWindowSubjectDistinguishesKinds(t *testing.T) {
	// A reviewer pane and a session that happen to share an id string are
	// different subjects. Collapsing them would let a pane inherit a session's
	// tokens.
	spec := usageWindowSpec{Role: domain.WorkflowRoleReviewer, Run: domain.WorkflowRun{ID: "wf"}}
	asSession := usageWindowDedupeKey(spec, domain.SessionSubject("x"))
	asPane := usageWindowDedupeKey(spec, domain.RuntimePaneSubject("x"))
	if asSession == asPane {
		t.Fatal("subject KIND must be part of the window identity")
	}
}

var errStoreDown = errStoreDownType{}

type errStoreDownType struct{}

func (errStoreDownType) Error() string { return "store down" }
