package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// seedWorkflowQuestionResolutionFixture seeds a project, a workflow run, and
// one captured question (auto_resolvable/resolving), returning the question
// id. Mirrors the fixture shape used by detector_store_test.go, minus the
// classifier/detector plumbing this package doesn't import.
func seedWorkflowQuestionResolutionFixture(t *testing.T, s interface {
	InsertWorkflowQuestion(context.Context, domain.WorkflowQuestion) (domain.WorkflowQuestion, bool, error)
}, runID string, now time.Time) domain.WorkflowQuestionID {
	t.Helper()
	q, inserted, err := s.InsertWorkflowQuestion(context.Background(), domain.WorkflowQuestion{
		ID:             domain.WorkflowQuestionID(runID + "-q1"),
		WorkflowRunID:  domain.WorkflowRunID(runID),
		Fingerprint:    runID + "-fp1",
		QuestionText:   "Which existing helper should I use to format this?",
		Certainty:      domain.QuestionCertaintyInferred,
		Classification: domain.QuestionClassificationAutoResolvable,
		State:          domain.QuestionStateResolving,
		CreatedAt:      now,
	})
	if err != nil {
		t.Fatalf("seed workflow question: %v", err)
	}
	if !inserted {
		t.Fatalf("expected fresh question insert")
	}
	return q.ID
}

func TestInsertAndGetWorkflowQuestionResolution(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wqr-1")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wqr-1", "wqr-run-1", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	questionID := seedWorkflowQuestionResolutionFixture(t, s, "wqr-run-1", now)

	certainty := domain.QuestionCertaintyInferred
	created, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-1",
		WorkflowQuestionID: questionID,
		WorkflowRunID:      "wqr-run-1",
		ResolverHarness:    domain.AgentHarness("codex"),
		Status:             domain.ResolutionStatusRunning,
		EvidenceReferences: []string{"internal/foo/bar.go:10-20"},
		Certainty:          &certainty,
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	if err != nil {
		t.Fatalf("InsertWorkflowQuestionResolution: %v", err)
	}
	if created.ID != "res-1" || created.Status != domain.ResolutionStatusRunning {
		t.Fatalf("created = %+v, want id=res-1 status=running", created)
	}
	if len(created.EvidenceReferences) != 1 || created.EvidenceReferences[0] != "internal/foo/bar.go:10-20" {
		t.Fatalf("evidence references = %+v, want round-tripped single entry", created.EvidenceReferences)
	}

	got, ok, err := s.GetWorkflowQuestionResolution(ctx, "res-1")
	if err != nil || !ok {
		t.Fatalf("GetWorkflowQuestionResolution: ok=%v err=%v", ok, err)
	}
	if got.WorkflowQuestionID != questionID || got.ResolverHarness != domain.AgentHarness("codex") {
		t.Fatalf("got = %+v, want matching question id and resolver harness", got)
	}

	if _, ok, err := s.GetWorkflowQuestionResolution(ctx, "does-not-exist"); err != nil || ok {
		t.Fatalf("GetWorkflowQuestionResolution(missing): ok=%v err=%v, want ok=false no error", ok, err)
	}
}

func TestGetCurrentResolutionForQuestion_FollowsPointer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wqr-2")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wqr-2", "wqr-run-2", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	questionID := seedWorkflowQuestionResolutionFixture(t, s, "wqr-run-2", now)

	// Before any resolving_run_id is set, there is no current resolution.
	if _, ok, err := s.GetCurrentResolutionForQuestion(ctx, string(questionID)); err != nil || ok {
		t.Fatalf("GetCurrentResolutionForQuestion (no pointer set): ok=%v err=%v, want ok=false", ok, err)
	}

	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-current-1",
		WorkflowQuestionID: questionID,
		WorkflowRunID:      "wqr-run-2",
		ResolverHarness:    domain.AgentHarness("codex"),
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("InsertWorkflowQuestionResolution: %v", err)
	}

	runID := "res-current-1"
	ok, err := s.SetWorkflowQuestionResolvingRunID(ctx, string(questionID), &runID)
	if err != nil || !ok {
		t.Fatalf("SetWorkflowQuestionResolvingRunID: ok=%v err=%v", ok, err)
	}

	current, ok, err := s.GetCurrentResolutionForQuestion(ctx, string(questionID))
	if err != nil || !ok {
		t.Fatalf("GetCurrentResolutionForQuestion: ok=%v err=%v", ok, err)
	}
	if current.ID != "res-current-1" {
		t.Fatalf("current.ID = %v, want res-current-1", current.ID)
	}

	// Clearing the pointer (nil) removes the "current" answer again.
	ok, err = s.SetWorkflowQuestionResolvingRunID(ctx, string(questionID), nil)
	if err != nil || !ok {
		t.Fatalf("SetWorkflowQuestionResolvingRunID (clear): ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.GetCurrentResolutionForQuestion(ctx, string(questionID)); err != nil || ok {
		t.Fatalf("GetCurrentResolutionForQuestion (after clear): ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestTransitionResolutionStatus_CASSucceedsAndFailsCleanly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wqr-3")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wqr-3", "wqr-run-3", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	questionID := seedWorkflowQuestionResolutionFixture(t, s, "wqr-run-3", now)

	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-cas-1",
		WorkflowQuestionID: questionID,
		WorkflowRunID:      "wqr-run-3",
		ResolverHarness:    domain.AgentHarness("codex"),
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("InsertWorkflowQuestionResolution: %v", err)
	}

	completedAt := now.Add(time.Minute)
	certainty := domain.QuestionCertaintyActual

	// A transition from the WRONG expected status is rejected cleanly: no
	// error, ok=false, row unchanged.
	ok, err := s.TransitionResolutionStatus(ctx, "res-cas-1", domain.ResolutionStatusPending, domain.ResolutionStatusComplete,
		"use pkg/foo.Bar", "found exactly one matching helper", []string{"pkg/foo/bar.go:5"}, &certainty, false, completedAt, &completedAt)
	if err != nil {
		t.Fatalf("TransitionResolutionStatus (wrong expected): unexpected error %v", err)
	}
	if ok {
		t.Fatalf("TransitionResolutionStatus (wrong expected) = true, want false (CAS should reject)")
	}
	unchanged, ok2, err := s.GetWorkflowQuestionResolution(ctx, "res-cas-1")
	if err != nil || !ok2 {
		t.Fatalf("GetWorkflowQuestionResolution: ok=%v err=%v", ok2, err)
	}
	if unchanged.Status != domain.ResolutionStatusRunning {
		t.Fatalf("status after rejected CAS = %v, want still running", unchanged.Status)
	}

	// The correct expected status applies cleanly.
	ok, err = s.TransitionResolutionStatus(ctx, "res-cas-1", domain.ResolutionStatusRunning, domain.ResolutionStatusComplete,
		"use pkg/foo.Bar", "found exactly one matching helper", []string{"pkg/foo/bar.go:5"}, &certainty, false, completedAt, &completedAt)
	if err != nil {
		t.Fatalf("TransitionResolutionStatus: %v", err)
	}
	if !ok {
		t.Fatalf("TransitionResolutionStatus = false, want true")
	}

	got, ok, err := s.GetWorkflowQuestionResolution(ctx, "res-cas-1")
	if err != nil || !ok {
		t.Fatalf("GetWorkflowQuestionResolution: ok=%v err=%v", ok, err)
	}
	if got.Status != domain.ResolutionStatusComplete || got.Answer != "use pkg/foo.Bar" {
		t.Fatalf("got = %+v, want status=complete answer=use pkg/foo.Bar", got)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
		t.Fatalf("CompletedAt = %v, want %v", got.CompletedAt, completedAt)
	}

	// A second attempt to transition away from 'running' again is now
	// rejected (the row is already 'complete', not 'running'): the same
	// race-loses-cleanly behavior applies to a repeated/duplicate transition.
	ok, err = s.TransitionResolutionStatus(ctx, "res-cas-1", domain.ResolutionStatusRunning, domain.ResolutionStatusFailed,
		"", "", nil, nil, false, completedAt, &completedAt)
	if err != nil {
		t.Fatalf("TransitionResolutionStatus (repeat): unexpected error %v", err)
	}
	if ok {
		t.Fatalf("TransitionResolutionStatus (repeat) = true, want false (already terminal)")
	}
}

func TestPartialUniqueIndex_PreventsConcurrentRunningResolutions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wqr-4")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wqr-4", "wqr-run-4", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	questionID := seedWorkflowQuestionResolutionFixture(t, s, "wqr-run-4", now)

	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-race-1",
		WorkflowQuestionID: questionID,
		WorkflowRunID:      "wqr-run-4",
		ResolverHarness:    domain.AgentHarness("codex"),
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("insert first running resolution: %v", err)
	}

	// A second concurrent running attempt for the SAME question must be
	// rejected by the partial unique index on
	// workflow_question_resolutions(workflow_question_id) WHERE
	// status='running' (0105) — this is the SQL-layer guarantee that at
	// most one resolver session is ever in flight per question.
	_, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-race-2",
		WorkflowQuestionID: questionID,
		WorkflowRunID:      "wqr-run-4",
		ResolverHarness:    domain.AgentHarness("claude-code"),
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	if err == nil {
		t.Fatal("expected second concurrent running resolution to be rejected, got nil error")
	}

	// A pending (non-running) attempt for the same question is NOT blocked
	// by the partial index — only 'running' is exclusive.
	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-race-3",
		WorkflowQuestionID: questionID,
		WorkflowRunID:      "wqr-run-4",
		ResolverHarness:    domain.AgentHarness("claude-code"),
		Status:             domain.ResolutionStatusPending,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("insert second pending resolution: %v", err)
	}

	// Once the first attempt leaves 'running', a fresh running attempt for
	// the same question becomes insertable again.
	if _, err := s.TransitionResolutionStatus(ctx, "res-race-1", domain.ResolutionStatusRunning, domain.ResolutionStatusFailed,
		"", "", nil, nil, false, now.Add(time.Second), nil); err != nil {
		t.Fatalf("transition first attempt to failed: %v", err)
	}
	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-race-4",
		WorkflowQuestionID: questionID,
		WorkflowRunID:      "wqr-run-4",
		ResolverHarness:    domain.AgentHarness("claude-code"),
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("insert running resolution after prior one failed: %v", err)
	}
}

func TestCancelRunningResolutionsByQuestion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wqr-5")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wqr-5", "wqr-run-5", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	questionID := seedWorkflowQuestionResolutionFixture(t, s, "wqr-run-5", now)

	// No-op when nothing is running for this question.
	n, err := s.CancelRunningResolutionsByQuestion(ctx, string(questionID), now)
	if err != nil {
		t.Fatalf("CancelRunningResolutionsByQuestion (no-op): %v", err)
	}
	if n != 0 {
		t.Fatalf("cancelled %d rows, want 0 (nothing running)", n)
	}

	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-cancel-1",
		WorkflowQuestionID: questionID,
		WorkflowRunID:      "wqr-run-5",
		ResolverHarness:    domain.AgentHarness("codex"),
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("insert running resolution: %v", err)
	}

	cancelAt := now.Add(time.Minute)
	n, err = s.CancelRunningResolutionsByQuestion(ctx, string(questionID), cancelAt)
	if err != nil {
		t.Fatalf("CancelRunningResolutionsByQuestion: %v", err)
	}
	if n != 1 {
		t.Fatalf("cancelled %d rows, want 1", n)
	}

	got, ok, err := s.GetWorkflowQuestionResolution(ctx, "res-cancel-1")
	if err != nil || !ok {
		t.Fatalf("GetWorkflowQuestionResolution: ok=%v err=%v", ok, err)
	}
	if got.Status != domain.ResolutionStatusCancelled {
		t.Fatalf("status = %v, want cancelled", got.Status)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(cancelAt) {
		t.Fatalf("CompletedAt = %v, want %v", got.CompletedAt, cancelAt)
	}

	// A second call is a clean no-op: the row is no longer 'running'.
	n, err = s.CancelRunningResolutionsByQuestion(ctx, string(questionID), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("CancelRunningResolutionsByQuestion (second call): %v", err)
	}
	if n != 0 {
		t.Fatalf("second cancel affected %d rows, want 0", n)
	}
}

func TestListRunningResolutions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wqr-6")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wqr-6", "wqr-run-6", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	q1 := seedWorkflowQuestionResolutionFixture(t, s, "wqr-run-6", now)

	if list, err := s.ListRunningResolutions(ctx); err != nil || len(list) != 0 {
		t.Fatalf("ListRunningResolutions (empty): list=%+v err=%v", list, err)
	}

	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-list-1",
		WorkflowQuestionID: q1,
		WorkflowRunID:      "wqr-run-6",
		ResolverHarness:    domain.AgentHarness("codex"),
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("insert running resolution: %v", err)
	}
	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "res-list-2",
		WorkflowQuestionID: q1,
		WorkflowRunID:      "wqr-run-6",
		ResolverHarness:    domain.AgentHarness("codex"),
		Status:             domain.ResolutionStatusPending,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("insert pending resolution: %v", err)
	}

	list, err := s.ListRunningResolutions(ctx)
	if err != nil {
		t.Fatalf("ListRunningResolutions: %v", err)
	}
	if len(list) != 1 || list[0].ID != "res-list-1" {
		t.Fatalf("ListRunningResolutions = %+v, want exactly [res-list-1]", list)
	}
}

// TestListWorkflowQuestionResolutionsByRun exercises Checkpoint 8K-B pass
// 3's telemetry read source, including the sorted-by-CreatedAt-in-Go
// contract documented on the store method (see the query's own doc comment
// in workflow_question_resolutions.sql for why there is no SQL ORDER BY).
func TestListWorkflowQuestionResolutionsByRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wqr-7")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wqr-7", "wqr-run-7", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	q1 := seedWorkflowQuestionResolutionFixture(t, s, "wqr-run-7", now)

	later := now.Add(time.Minute)
	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID: "res-run7-b", WorkflowQuestionID: q1, WorkflowRunID: "wqr-run-7",
		ResolverHarness: domain.AgentHarness("codex"), Status: domain.ResolutionStatusFailed,
		CreatedAt: later, UpdatedAt: later,
	}); err != nil {
		t.Fatalf("insert second resolution: %v", err)
	}
	if _, err := s.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID: "res-run7-a", WorkflowQuestionID: q1, WorkflowRunID: "wqr-run-7",
		ResolverHarness: domain.AgentHarness("claude-code"), Status: domain.ResolutionStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert first resolution: %v", err)
	}

	list, err := s.ListWorkflowQuestionResolutionsByRun(ctx, "wqr-run-7")
	if err != nil {
		t.Fatalf("ListWorkflowQuestionResolutionsByRun: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 resolutions for wqr-run-7, got %d: %+v", len(list), list)
	}
	if list[0].ID != "res-run7-a" || list[1].ID != "res-run7-b" {
		t.Fatalf("expected sorted by CreatedAt ascending [res-run7-a, res-run7-b], got [%s, %s]", list[0].ID, list[1].ID)
	}

	empty, err := s.ListWorkflowQuestionResolutionsByRun(ctx, "no-such-run")
	if err != nil {
		t.Fatalf("ListWorkflowQuestionResolutionsByRun(no-such-run): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected empty result for unknown run, got %+v", empty)
	}
}

// TestListPendingWorkflowQuestions exercises Checkpoint 8K-B pass 3's
// global "Pending Decisions" inbox source: it must span ALL runs (no
// run-id filter) and honor the state filter/default.
func TestListPendingWorkflowQuestions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)

	seedProject(t, s, "proj-pending-1")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-pending-1", "pending-run-a", now), nil); err != nil {
		t.Fatalf("create workflow run a: %v", err)
	}
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-pending-1", "pending-run-b", now), nil); err != nil {
		t.Fatalf("create workflow run b: %v", err)
	}

	mustInsertQuestion := func(id, runID, fingerprint string, state domain.QuestionState) {
		t.Helper()
		if _, _, err := s.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
			ID: domain.WorkflowQuestionID(id), WorkflowRunID: domain.WorkflowRunID(runID),
			Fingerprint: fingerprint, QuestionText: "q", Certainty: domain.QuestionCertaintyInferred,
			Classification: domain.QuestionClassificationHumanRequired, State: state, CreatedAt: now,
		}); err != nil {
			t.Fatalf("insert question %s: %v", id, err)
		}
	}
	mustInsertQuestion("pend-q-1", "pending-run-a", "fp-pend-1", domain.QuestionStateHumanRequired)
	mustInsertQuestion("pend-q-2", "pending-run-b", "fp-pend-2", domain.QuestionStateResolving)
	mustInsertQuestion("pend-q-3", "pending-run-a", "fp-pend-3", domain.QuestionStateAnswered)
	mustInsertQuestion("pend-q-4", "pending-run-b", "fp-pend-4", domain.QuestionStateCancelled)

	// Default (no states passed): human_required + resolving across both runs.
	def, err := s.ListPendingWorkflowQuestions(ctx, nil)
	if err != nil {
		t.Fatalf("ListPendingWorkflowQuestions(default): %v", err)
	}
	if len(def) != 2 {
		t.Fatalf("expected 2 pending questions across both runs by default, got %+v", def)
	}
	seen := map[string]bool{}
	for _, q := range def {
		seen[string(q.ID)] = true
	}
	if !seen["pend-q-1"] || !seen["pend-q-2"] {
		t.Fatalf("expected pend-q-1 and pend-q-2 in default pending list, got %+v", def)
	}

	// Filtered to human_required only.
	humanOnly, err := s.ListPendingWorkflowQuestions(ctx, []string{"human_required"})
	if err != nil {
		t.Fatalf("ListPendingWorkflowQuestions(human_required): %v", err)
	}
	if len(humanOnly) != 1 || humanOnly[0].ID != "pend-q-1" {
		t.Fatalf("expected exactly [pend-q-1], got %+v", humanOnly)
	}
}
