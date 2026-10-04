package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/postrunqa"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// The lifecycle store is the gate's durable store; nothing else persists a
// QARun.
var _ postrunqa.Store = (*sqlite.Store)(nil)

func TestPostRunQARun_SaveLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, time.August, 22, 10, 0, 0, 0, time.UTC)
	completed := started.Add(4 * time.Minute)

	run := postrunqa.QARun{
		ID:          "qa-round-trip",
		SubjectKind: postrunqa.SubjectTask,
		SubjectID:   "task-77",
		Phase:       postrunqa.PhaseNeedsAttention,
		Findings: []postrunqa.Finding{
			{
				Source:      "go build ./...",
				Signal:      "internal/postrunqa/qa.go: undefined: Foo",
				Evidence:    "internal/postrunqa/qa.go:12:2: undefined: Foo",
				Attribution: postrunqa.AttributionNew,
				Severity:    postrunqa.SeverityBlocker,
			},
			{
				Source:      "go vet ./...",
				Signal:      "internal/legacy/x.go: unreachable code",
				Evidence:    "internal/legacy/x.go:44:2: unreachable code",
				Attribution: postrunqa.AttributionBaseline,
				Severity:    postrunqa.SeverityMinor,
			},
		},
		RepairCycleCount: 2,
		MaxRepairCycles:  2,
		Result:           postrunqa.ResultNeedsAttention,
		StartedAt:        started,
		CompletedAt:      &completed,
	}

	saved, err := s.SaveQARun(ctx, run)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, ok, err := s.LoadQARun(ctx, run.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatal("load: saved run not found")
	}
	assertQARunEqual(t, "save return value", saved, run)
	assertQARunEqual(t, "loaded run", loaded, run)
}

func TestPostRunQARun_ZeroValueFieldsLoadWithDefaults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, time.August, 22, 11, 0, 0, 0, time.UTC)

	// Neither phase nor the repair budget is set: the durable envelope has to
	// come back usable, not with a zero budget that can never repair anything.
	if _, err := s.SaveQARun(ctx, postrunqa.QARun{
		ID:          "qa-defaults",
		SubjectKind: postrunqa.SubjectWorkflow,
		SubjectID:   "run-9",
		StartedAt:   started,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, ok, err := s.LoadQARun(ctx, "qa-defaults")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatal("load: saved run not found")
	}
	if loaded.Phase != postrunqa.PhasePending {
		t.Fatalf("phase = %q, want %q", loaded.Phase, postrunqa.PhasePending)
	}
	if loaded.MaxRepairCycles != 2 {
		t.Fatalf("max repair cycles = %d, want the default 2", loaded.MaxRepairCycles)
	}
	if loaded.RepairCycleCount != 0 {
		t.Fatalf("repair cycle count = %d, want 0", loaded.RepairCycleCount)
	}
	if loaded.Result != postrunqa.ResultUnset {
		t.Fatalf("result = %q, want unset", loaded.Result)
	}
	if loaded.CompletedAt != nil {
		t.Fatalf("completed_at = %v, want nil for an unfinished run", loaded.CompletedAt)
	}
	if len(loaded.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", loaded.Findings)
	}
}

func TestPostRunQARun_SaveAdvancesTheSameRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

	run := postrunqa.QARun{
		ID:          "qa-advance",
		SubjectKind: postrunqa.SubjectTask,
		SubjectID:   "task-42",
		Phase:       postrunqa.PhaseChecking,
		StartedAt:   started,
	}
	if _, err := s.SaveQARun(ctx, run); err != nil {
		t.Fatalf("save checking: %v", err)
	}

	run.Phase = postrunqa.PhaseAutoFixing
	run.RepairCycleCount = 1
	run.Findings = []postrunqa.Finding{{
		Source:      "go test ./...",
		Signal:      "TestFoo failed",
		Evidence:    "--- FAIL: TestFoo (0.01s)",
		Attribution: postrunqa.AttributionNew,
		Severity:    postrunqa.SeverityBlocker,
	}}
	if _, err := s.SaveQARun(ctx, run); err != nil {
		t.Fatalf("save auto_fixing: %v", err)
	}

	loaded, ok, err := s.LatestQARunForSubject(ctx, postrunqa.SubjectTask, "task-42")
	if err != nil {
		t.Fatalf("latest for subject: %v", err)
	}
	if !ok {
		t.Fatal("latest for subject: no run found")
	}
	if loaded.Phase != postrunqa.PhaseAutoFixing || loaded.RepairCycleCount != 1 {
		t.Fatalf("advanced run = phase %q cycle %d, want auto_fixing/1", loaded.Phase, loaded.RepairCycleCount)
	}
	if len(loaded.Findings) != 1 || loaded.Findings[0].Signal != "TestFoo failed" {
		t.Fatalf("findings = %+v, want the single failing-test finding", loaded.Findings)
	}
}

func TestPostRunQARun_LatestForSubjectPicksTheNewestPass(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := time.Date(2026, time.August, 22, 13, 0, 0, 0, time.UTC)

	// A subject re-entering the gate gets a new pass; the old one is history,
	// not something to overwrite.
	for _, tc := range []struct {
		id      string
		started time.Time
		result  postrunqa.QAResult
	}{
		{"qa-pass-1", first, postrunqa.ResultNeedsAttention},
		{"qa-pass-2", first.Add(time.Hour), postrunqa.ResultClean},
	} {
		completed := tc.started.Add(time.Minute)
		if _, err := s.SaveQARun(ctx, postrunqa.QARun{
			ID:          tc.id,
			SubjectKind: postrunqa.SubjectWorkflow,
			SubjectID:   "run-multi",
			Phase:       postrunqa.PhaseClean,
			Result:      tc.result,
			StartedAt:   tc.started,
			CompletedAt: &completed,
		}); err != nil {
			t.Fatalf("save %s: %v", tc.id, err)
		}
	}

	latest, ok, err := s.LatestQARunForSubject(ctx, postrunqa.SubjectWorkflow, "run-multi")
	if err != nil {
		t.Fatalf("latest for subject: %v", err)
	}
	if !ok {
		t.Fatal("latest for subject: no run found")
	}
	if latest.ID != "qa-pass-2" || latest.Result != postrunqa.ResultClean {
		t.Fatalf("latest = %s/%q, want qa-pass-2/clean", latest.ID, latest.Result)
	}

	// The earlier pass is still readable by id.
	if earlier, found, err := s.LoadQARun(ctx, "qa-pass-1"); err != nil || !found {
		t.Fatalf("load earlier pass: found=%v err=%v", found, err)
	} else if earlier.Result != postrunqa.ResultNeedsAttention {
		t.Fatalf("earlier pass result = %q, want needs_attention", earlier.Result)
	}
}

func TestPostRunQARun_MissingRunIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, ok, err := s.LoadQARun(ctx, "nope"); err != nil || ok {
		t.Fatalf("load missing run: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, ok, err := s.LatestQARunForSubject(ctx, postrunqa.SubjectTask, "nope"); err != nil || ok {
		t.Fatalf("latest for unknown subject: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestPostRunQARun_SaveRejectsAnInvalidRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, time.August, 22, 14, 0, 0, 0, time.UTC)

	if _, err := s.SaveQARun(ctx, postrunqa.QARun{
		ID:          "qa-invalid",
		SubjectKind: "session",
		SubjectID:   "s-1",
		StartedAt:   started,
	}); err == nil {
		t.Fatal("save accepted an unknown subject kind")
	}
}

func assertQARunEqual(t *testing.T, what string, got, want postrunqa.QARun) {
	t.Helper()
	if got.ID != want.ID || got.SubjectKind != want.SubjectKind || got.SubjectID != want.SubjectID {
		t.Fatalf("%s: identity = %s/%s/%s, want %s/%s/%s", what,
			got.ID, got.SubjectKind, got.SubjectID, want.ID, want.SubjectKind, want.SubjectID)
	}
	if got.Phase != want.Phase || got.Result != want.Result {
		t.Fatalf("%s: phase/result = %q/%q, want %q/%q", what, got.Phase, got.Result, want.Phase, want.Result)
	}
	if got.RepairCycleCount != want.RepairCycleCount || got.MaxRepairCycles != want.MaxRepairCycles {
		t.Fatalf("%s: repair cycles = %d/%d, want %d/%d", what,
			got.RepairCycleCount, got.MaxRepairCycles, want.RepairCycleCount, want.MaxRepairCycles)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("%s: started_at = %v, want %v", what, got.StartedAt, want.StartedAt)
	}
	switch {
	case got.CompletedAt == nil && want.CompletedAt != nil,
		got.CompletedAt != nil && want.CompletedAt == nil:
		t.Fatalf("%s: completed_at = %v, want %v", what, got.CompletedAt, want.CompletedAt)
	case got.CompletedAt != nil && !got.CompletedAt.Equal(*want.CompletedAt):
		t.Fatalf("%s: completed_at = %v, want %v", what, *got.CompletedAt, *want.CompletedAt)
	}
	if len(got.Findings) != len(want.Findings) {
		t.Fatalf("%s: %d findings, want %d", what, len(got.Findings), len(want.Findings))
	}
	for i := range want.Findings {
		if got.Findings[i] != want.Findings[i] {
			t.Fatalf("%s: finding %d = %+v, want %+v", what, i, got.Findings[i], want.Findings[i])
		}
	}
}
