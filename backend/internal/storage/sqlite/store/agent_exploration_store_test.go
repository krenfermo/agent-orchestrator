package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// agent_exploration_store_test.go -- Frente 3 / 3C persistence, against a real
// SQLite database: observations are exactly-once, attributed to the right run,
// role and subject, isolated per run/project, and never carry a path outside
// the project scope.

func obs(key string, op domain.ToolOp, scope domain.ToolPathScope, path string, at time.Time) domain.AgentToolObservation {
	return domain.AgentToolObservation{
		Key: key, Ordinal: at.Unix(), ObservedAt: timePtrUTC(at),
		Origin: domain.OriginAgentExploration, Op: op, ToolName: "Read",
		PathScope: scope, Path: path,
	}
}

func applyTools(t *testing.T, s *sqlite.Store, source domain.UsageSourceRecord, at time.Time, events []domain.ModelUsageEvent, facts domain.AgentToolFacts) error {
	t.Helper()
	current := currentUsageSource(t, s, source)
	return s.ApplyUsageChunk(context.Background(), source.ID, current.ByteOffset, current.UpdatedAt, domain.SourceCursorState{
		ByteOffset: current.ByteOffset + 10,
		State:      domain.UsageSourceActive,
		UpdatedAt:  at,
	}, events, facts)
}

func TestToolObservations_CorrelateToRunRoleAndSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker, opened: base, harness: "claude-code"})
	openWindow(t, s, window{key: "w-fix-1", session: string(sess.ID), role: domain.WorkflowRoleFixWorker, cycle: 1, opened: base.Add(30 * time.Minute), harness: "claude-code"})

	err := applyTools(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{
		attrEvent("e1", 1000, 10, base.Add(5*time.Minute)),
		attrEvent("e2", 500, 10, base.Add(40*time.Minute)),
	}, domain.AgentToolFacts{Observations: []domain.AgentToolObservation{
		obs("o1", domain.ToolOpRead, domain.ToolPathProject, "src/a.go", base.Add(5*time.Minute)),
		obs("o2", domain.ToolOpRead, domain.ToolPathProject, "src/b.go", base.Add(41*time.Minute)),
	}})
	mustNoError(t, err)

	rows, err := s.ListRunToolObservations(ctx, attrRunID)
	mustNoError(t, err)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	byPath := map[string]domain.WorkflowRole{}
	for _, r := range rows {
		if r.Subject != domain.SessionSubject(sess.ID) || r.ProjectID != attrProjectID {
			t.Fatalf("row not correlated to its subject/project: %+v", r)
		}
		if r.AttributionBasis != domain.AttributionExact {
			t.Fatalf("timed observation attributed %q, want exact", r.AttributionBasis)
		}
		byPath[r.Path] = r.Role
	}
	if byPath["src/a.go"] != domain.WorkflowRoleWorker || byPath["src/b.go"] != domain.WorkflowRoleFixWorker {
		t.Fatalf("role attribution = %v, want a.go->worker, b.go->fix_worker", byPath)
	}
	calls, err := s.ListRunExplorationCalls(ctx, attrRunID)
	mustNoError(t, err)
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
}

func TestToolObservations_ExactlyOnceAndResultsCompleteOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{key: "w", session: string(sess.ID), role: domain.WorkflowRoleWorker, opened: base})

	first := domain.AgentToolFacts{Observations: []domain.AgentToolObservation{
		obs("o1", domain.ToolOpRead, domain.ToolPathProject, "a.go", base.Add(time.Minute)),
	}}
	mustNoError(t, applyTools(t, s, source, base, nil, first))
	// A replay of the same chunk (restart, generation reset) and the result
	// arriving in a later chunk.
	items := int64(12)
	mustNoError(t, applyTools(t, s, source, base, nil, domain.AgentToolFacts{
		Observations: first.Observations,
		Results:      []domain.AgentToolResult{{Key: "o1", ResultBytes: 300, ResultItems: &items}},
	}))
	// The same result re-read must not overwrite, and a result for a call
	// that was never recorded must write nothing.
	mustNoError(t, applyTools(t, s, source, base, nil, domain.AgentToolFacts{
		Results: []domain.AgentToolResult{{Key: "o1", ResultBytes: 999}, {Key: "never-recorded", ResultBytes: 5}},
	}))

	rows, err := s.ListRunToolObservations(ctx, attrRunID)
	mustNoError(t, err)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want exactly one after replay", len(rows))
	}
	if rows[0].ResultBytes == nil || *rows[0].ResultBytes != 300 || rows[0].ResultItems == nil || *rows[0].ResultItems != 12 {
		t.Fatalf("result = %v/%v, want 300 bytes / 12 items set once", rows[0].ResultBytes, rows[0].ResultItems)
	}
	if rows[0].ResultError == nil || *rows[0].ResultError {
		t.Fatalf("result error = %v, want observed false", rows[0].ResultError)
	}
}

func TestToolObservations_UnobservedResultStaysNullNotZero(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{key: "w", session: string(sess.ID), role: domain.WorkflowRoleWorker, opened: base})
	mustNoError(t, applyTools(t, s, source, base, nil, domain.AgentToolFacts{Observations: []domain.AgentToolObservation{
		obs("o1", domain.ToolOpSearch, domain.ToolPathProject, ".", base),
	}}))
	rows, err := s.ListRunToolObservations(context.Background(), attrRunID)
	mustNoError(t, err)
	if len(rows) != 1 || rows[0].ResultBytes != nil || rows[0].ResultItems != nil || rows[0].ResultError != nil {
		t.Fatalf("unobserved result must read back as NULL: %+v", rows)
	}
}

func TestToolObservations_CrossRunAndProjectIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sessA, sourceA := seedNamedAttributionSession(t, s, base, "a")
	sessB, sourceB := seedNamedAttributionSession(t, s, base, "b")
	openWindow(t, s, window{key: "wa", session: string(sessA.ID), run: "wf-a", role: domain.WorkflowRoleWorker, opened: base})
	openWindow(t, s, window{key: "wb", session: string(sessB.ID), run: "wf-b", role: domain.WorkflowRoleWorker, opened: base})
	mustNoError(t, applyTools(t, s, sourceA, base, nil, domain.AgentToolFacts{Observations: []domain.AgentToolObservation{
		obs("same-key", domain.ToolOpRead, domain.ToolPathProject, "only-in-a.go", base.Add(time.Minute)),
	}}))
	mustNoError(t, applyTools(t, s, sourceB, base, nil, domain.AgentToolFacts{Observations: []domain.AgentToolObservation{
		// The same observation key in another binding is a different fact.
		obs("same-key", domain.ToolOpRead, domain.ToolPathProject, "only-in-b.go", base.Add(time.Minute)),
	}}))
	for run, want := range map[string]string{"wf-a": "only-in-a.go", "wf-b": "only-in-b.go"} {
		rows, err := s.ListRunToolObservations(ctx, run)
		mustNoError(t, err)
		if len(rows) != 1 || rows[0].Path != want {
			t.Fatalf("run %s rows = %+v, want only %s", run, rows, want)
		}
	}
	rows, err := s.ListRunToolObservations(ctx, "wf-unknown")
	mustNoError(t, err)
	if len(rows) != 0 {
		t.Fatalf("an unknown run must see nothing, got %d", len(rows))
	}
}

func TestToolObservations_RefuseAPathOutsideTheProjectScope(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{key: "w", session: string(sess.ID), role: domain.WorkflowRoleWorker, opened: base})
	before := currentUsageSource(t, s, source).ByteOffset
	for name, bad := range map[string]domain.AgentToolObservation{
		"outside with path": obs("o1", domain.ToolOpRead, domain.ToolPathOutside, "/Users/someone/.ssh/id_rsa", base),
		"secret with path":  obs("o2", domain.ToolOpRead, domain.ToolPathSecret, ".env", base),
		"absolute project":  obs("o3", domain.ToolOpRead, domain.ToolPathProject, "/etc/passwd", base),
		"escaping project":  obs("o4", domain.ToolOpRead, domain.ToolPathProject, "../x", base),
		"unknown op":        obs("o5", domain.ToolOp("exfiltrate"), domain.ToolPathNone, "", base),
	} {
		err := applyTools(t, s, source, base, nil, domain.AgentToolFacts{Observations: []domain.AgentToolObservation{bad}})
		if err == nil {
			t.Fatalf("%s: write accepted, want refusal", name)
		}
	}
	if after := currentUsageSource(t, s, source).ByteOffset; after != before {
		t.Fatalf("cursor moved %d -> %d on a refused chunk", before, after)
	}
	rows, err := s.ListRunToolObservations(context.Background(), attrRunID)
	mustNoError(t, err)
	if len(rows) != 0 {
		t.Fatalf("refused observations were written: %+v", rows)
	}
}

func TestGetUsageSourceForIngestionCarriesTheSubjectWorkspaceRoot(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(1700000000, 0).UTC()
	_, source := seedAttributionSession(t, s, base)
	got, ok, err := s.GetUsageSourceForIngestion(context.Background(), source.ID)
	mustNoError(t, err)
	if !ok || got.WorkspaceRoot != "/ws" {
		t.Fatalf("workspace root = %q (ok=%v), want the session's recorded workspace /ws", got.WorkspaceRoot, ok)
	}
}
