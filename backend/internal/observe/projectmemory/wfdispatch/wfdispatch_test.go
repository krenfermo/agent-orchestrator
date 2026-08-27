package wfdispatch

import (
	stdctx "context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type captureSink struct {
	records []projectmemory.EvidenceRecord
}

func (c *captureSink) Write(_ stdctx.Context, record projectmemory.EvidenceRecord) (string, error) {
	c.records = append(c.records, record)
	return "/dev/null/" + record.RecordID, nil
}

func newRecorder() (*projectmemory.Recorder, *captureSink) {
	sink := &captureSink{}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	return projectmemory.NewRecorder(sink, projectmemory.WithClock(func() time.Time { return now })), sink
}

func only(t *testing.T, sink *captureSink) projectmemory.EvidenceRecord {
	t.Helper()
	if len(sink.records) != 1 {
		t.Fatalf("recorded %d evidence records, want 1", len(sink.records))
	}
	return sink.records[0]
}

type fakeSpawner struct {
	got ports.SpawnConfig
	rec domain.SessionRecord
	err error
}

func (f *fakeSpawner) Spawn(_ stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	f.got = cfg
	return f.rec, 1, 2, f.err
}

func TestSpawnerRecordsWorkerContextAndPassesThrough(t *testing.T) {
	recorder, sink := newRecorder()
	next := &fakeSpawner{rec: domain.SessionRecord{ID: "sess-1"}}
	wrapped := InstrumentSpawner(next, recorder, nil)
	cfg := ports.SpawnConfig{
		ProjectID:     "proj-1",
		WorkflowRunID: "wf-1",
		IssueID:       domain.IssueID("workflow-step:step-7"),
		Harness:       domain.AgentHarness("codex"),
		Prompt:        "1234567890",
		IssueContext:  "12345",
	}
	rec, sent, queued, err := wrapped.Spawn(stdctx.Background(), cfg)
	if err != nil || rec.ID != "sess-1" || sent != 1 || queued != 2 {
		t.Fatalf("the wrapper altered the dispatch result: %v %v %d %d", rec, err, sent, queued)
	}
	if next.got.Prompt != cfg.Prompt || next.got.WorkflowRunID != cfg.WorkflowRunID {
		t.Fatalf("the wrapper altered the spawn config: %+v", next.got)
	}
	record := only(t, sink)
	if record.Role != domain.WorkflowRoleWorker {
		t.Fatalf("role = %q", record.Role)
	}
	if record.WorkflowRunID != "wf-1" || record.WorkflowStepID != "step-7" {
		t.Fatalf("record not keyed to its run/step: %+v", record)
	}
	if record.SessionID != "sess-1" {
		t.Fatalf("sessionId = %q", record.SessionID)
	}
	if got := *record.Context.ContextSentBytes.Value; got != 15 {
		t.Fatalf("contextSentBytes = %d, want prompt plus issue context", got)
	}
	if record.Context.FilesInspected.Basis != projectmemory.BasisUnavailable {
		t.Fatalf("a worker spawn cannot see the agent's reads; got %+v", record.Context.FilesInspected)
	}
}

func TestSpawnerRecordsAFailedLaunch(t *testing.T) {
	recorder, sink := newRecorder()
	wrapped := InstrumentSpawner(&fakeSpawner{err: errors.New("no capacity")}, recorder, nil)
	if _, _, _, err := wrapped.Spawn(stdctx.Background(), ports.SpawnConfig{}); err == nil {
		t.Fatal("the wrapper swallowed the dispatch error")
	}
	record := only(t, sink)
	if record.Dispatch.Succeeded || record.Dispatch.Error != "no capacity" {
		t.Fatalf("failed launch not recorded honestly: %+v", record.Dispatch)
	}
}

type fakeReviewer struct {
	preflighted bool
	got         workflowcore.ReviewerLaunchRequest
}

func (f *fakeReviewer) Preflight(_ stdctx.Context, _ domain.ReviewerHarness, _ string) error {
	f.preflighted = true
	return nil
}

func (f *fakeReviewer) Launch(_ stdctx.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	f.got = req
	return workflowcore.ReviewerLaunchResult{HandleID: "h-1", AgentSessionID: "rev-1"}, nil
}

func TestReviewerLauncherRecordsPromptAndLinksReviewRun(t *testing.T) {
	recorder, sink := newRecorder()
	next := &fakeReviewer{}
	wrapped := InstrumentReviewerLauncher(next, recorder, nil)
	if err := wrapped.Preflight(stdctx.Background(), domain.ReviewerHarness("claude-code"), "/tmp/ws"); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !next.preflighted {
		t.Fatal("Preflight did not reach the wrapped launcher")
	}
	if len(sink.records) != 0 {
		t.Fatal("preflight starts no agent and must record no evidence")
	}
	result, err := wrapped.Launch(stdctx.Background(), workflowcore.ReviewerLaunchRequest{
		Harness:      domain.ReviewerHarness("claude-code"),
		ProjectID:    "proj-1",
		RunID:        "rr-9",
		Prompt:       "1234",
		SystemPrompt: "123456",
	})
	if err != nil || result.AgentSessionID != "rev-1" {
		t.Fatalf("the wrapper altered the launch result: %+v %v", result, err)
	}
	record := only(t, sink)
	if record.Role != domain.WorkflowRoleReviewer {
		t.Fatalf("role = %q", record.Role)
	}
	if len(record.Outcomes.ReviewRunIDs) != 1 || record.Outcomes.ReviewRunIDs[0] != "rr-9" {
		t.Fatalf("review run not linked: %+v", record.Outcomes)
	}
	if got := *record.Context.ContextSentBytes.Value; got != 10 {
		t.Fatalf("contextSentBytes = %d, want prompt plus system prompt", got)
	}
	if record.RunKey() != "rev-1" {
		t.Fatalf("a reviewer record with no workflow run must be filed under its session, got %q", record.RunKey())
	}
}

type fakeSender struct{ got string }

func (f *fakeSender) Send(_ stdctx.Context, _ domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	f.got = message
	return nil
}

type fakeReportingSender struct {
	fakeSender
	submitted int
	composer  int
	reported  string
}

func (f *fakeReportingSender) SendReportingSubmission(_ stdctx.Context, _ domain.SessionID, message string, _ *ports.SpawnAttachment) (ports.PromptSubmission, error) {
	f.reported = message
	return ports.PromptSubmitted, nil
}

func (f *fakeReportingSender) SubmitPending(_ stdctx.Context, _ domain.SessionID) (ports.PromptSubmission, error) {
	f.submitted++
	return ports.PromptSubmitted, nil
}

func (f *fakeReportingSender) ComposerState(_ stdctx.Context, _ domain.SessionID) ports.PromptSubmission {
	f.composer++
	return ports.PromptSubmitted
}

func TestMessageSenderRecordsFixContext(t *testing.T) {
	recorder, sink := newRecorder()
	next := &fakeSender{}
	wrapped := InstrumentMessageSender(next, recorder, nil)
	if err := wrapped.Send(stdctx.Background(), domain.SessionID("sess-3"), "12345678", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if next.got != "12345678" {
		t.Fatalf("the wrapper altered the message: %q", next.got)
	}
	record := only(t, sink)
	if record.Role != domain.WorkflowRoleFixWorker || record.SessionID != "sess-3" {
		t.Fatalf("record = %+v", record)
	}
	if got := *record.Context.ContextSentBytes.Value; got != 8 {
		t.Fatalf("contextSentBytes = %d", got)
	}
}

// The optional capabilities workflow type-asserts for must survive wrapping.
// Losing SubmissionReportingSender would silently downgrade what the fix path
// can prove about prompt delivery.
func TestMessageSenderPreservesSubmissionReporting(t *testing.T) {
	recorder, sink := newRecorder()
	next := &fakeReportingSender{}
	wrapped := InstrumentMessageSender(next, recorder, nil)
	reporting, ok := wrapped.(workflowcore.SubmissionReportingSender)
	if !ok {
		t.Fatal("wrapping lost the SubmissionReportingSender capability")
	}
	if _, err := reporting.SendReportingSubmission(stdctx.Background(), domain.SessionID("s"), "1234", nil); err != nil {
		t.Fatalf("SendReportingSubmission: %v", err)
	}
	if next.reported != "1234" {
		t.Fatalf("message did not reach the wrapped sender: %q", next.reported)
	}
	if _, err := reporting.SubmitPending(stdctx.Background(), domain.SessionID("s")); err != nil {
		t.Fatalf("SubmitPending: %v", err)
	}
	reporting.ComposerState(stdctx.Background(), domain.SessionID("s"))
	if next.submitted != 1 || next.composer != 1 {
		t.Fatalf("pass-through methods did not reach the wrapped sender: %+v", next)
	}
	// Only the call that delivered a new payload is evidence; re-submitting a
	// draft adds no context and must not inflate the baseline.
	if len(sink.records) != 1 {
		t.Fatalf("recorded %d records, want only the payload-bearing call", len(sink.records))
	}
}

type fakeVerifier struct {
	got workflowcore.VerifyCommandRequest
}

func (f *fakeVerifier) Run(_ stdctx.Context, req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
	f.got = req
	return workflowcore.VerifyCommandExecution{ExitCode: 0, DurationMS: 1234}, nil
}

func TestVerifierRecordsCommandAndOutcome(t *testing.T) {
	recorder, sink := newRecorder()
	next := &fakeVerifier{}
	wrapped := InstrumentVerifier(next, recorder, nil)
	execution, err := wrapped.Run(stdctx.Background(), workflowcore.VerifyCommandRequest{Command: "go", Args: []string{"build", "./..."}})
	if err != nil || execution.DurationMS != 1234 {
		t.Fatalf("the wrapper altered the execution result: %+v %v", execution, err)
	}
	record := only(t, sink)
	if record.Role != domain.WorkflowRoleVerify {
		t.Fatalf("role = %q", record.Role)
	}
	if record.Tools.ByName["go"] != 1 {
		t.Fatalf("tools.byName = %v", record.Tools.ByName)
	}
	if record.Outcomes.VerifyPassed == nil || !*record.Outcomes.VerifyPassed {
		t.Fatalf("verify outcome not linked: %+v", record.Outcomes)
	}
	if record.ProviderTokens.Total.Value != nil {
		t.Fatal("verify invokes no provider; token counts must stay null")
	}
}

type fakePlanner struct{ got workflowcore.PlannerRequest }

func (f *fakePlanner) Generate(_ stdctx.Context, req workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	f.got = req
	return workflowcore.PlannerResponse{Provider: "anthropic", Model: "sonnet"}, nil
}

type describingFakePlanner struct{ fakePlanner }

func (*describingFakePlanner) Descriptor() (provider, model string) { return "anthropic", "sonnet" }

func TestPlannerRecordsItsAssembledContext(t *testing.T) {
	recorder, sink := newRecorder()
	next := &fakePlanner{}
	wrapped := InstrumentPlanner(next, recorder, nil)
	request := workflowcore.PlannerRequest{
		Objective: "12345",
		Project:   domain.ProjectRecord{ID: "proj-1"},
		Context: workflowcore.PlannerContext{Documents: []workflowcore.PlannerDocument{
			{Path: "AGENTS.md", Content: "1234567890"},
			{Path: "README.md", Content: "12345"},
		}},
	}
	if _, err := wrapped.Generate(stdctx.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	record := only(t, sink)
	if record.Role != domain.WorkflowRolePlanner {
		t.Fatalf("role = %q", record.Role)
	}
	if got := *record.Context.FilesInspected.Value; got != 2 {
		t.Fatalf("filesInspected = %d", got)
	}
	if got := *record.Context.ContextSentBytes.Value; got != 20 {
		t.Fatalf("contextSentBytes = %d, want objective plus both documents", got)
	}
	if record.Provider != "anthropic" || record.Model != "sonnet" {
		t.Fatalf("planner identity not recorded: %+v", record)
	}
	if len(record.Context.Files) != 2 || record.Context.Files[0].Path != "AGENTS.md" {
		t.Fatalf("per-file detail missing: %+v", record.Context.Files)
	}
}

// master_coordinator.go asks the planner to describe itself; wrapping must not
// make that question unanswerable.
func TestPlannerPreservesDescriptor(t *testing.T) {
	recorder, _ := newRecorder()
	wrapped := InstrumentPlanner(&describingFakePlanner{}, recorder, nil)
	descriptor, ok := wrapped.(workflowcore.PlannerDescriptor)
	if !ok {
		t.Fatal("wrapping lost the PlannerDescriptor capability")
	}
	if provider, model := descriptor.Descriptor(); provider != "anthropic" || model != "sonnet" {
		t.Fatalf("Descriptor() = %q, %q", provider, model)
	}
}

// Instrumentation is opt-in: without a recorder the dependencies must come
// back exactly as they went in, so an uninstrumented daemon runs the same code
// it always did.
func TestInstrumentWithoutRecorderLeavesDepsUntouched(t *testing.T) {
	spawner := &fakeSpawner{}
	deps := workflowcore.Deps{Spawner: spawner, Verifier: &fakeVerifier{}}
	got := Instrument(deps, nil, nil)
	if got.Spawner != workflowcore.Spawner(spawner) {
		t.Fatal("a nil recorder must leave the spawner untouched")
	}
}

func TestInstrumentWrapsEveryDispatchSurface(t *testing.T) {
	recorder, sink := newRecorder()
	deps := workflowcore.Deps{
		Spawner:          &fakeSpawner{},
		ReviewerLauncher: &fakeReviewer{},
		MessageSender:    &fakeSender{},
		Verifier:         &fakeVerifier{},
		Planner:          &fakePlanner{},
	}
	got := Instrument(deps, recorder, nil)
	ctx := stdctx.Background()
	if _, _, _, err := got.Spawner.Spawn(ctx, ports.SpawnConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := got.ReviewerLauncher.Launch(ctx, workflowcore.ReviewerLaunchRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := got.MessageSender.Send(ctx, domain.SessionID("s"), "m", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := got.Verifier.Run(ctx, workflowcore.VerifyCommandRequest{Command: "go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := got.Planner.Generate(ctx, workflowcore.PlannerRequest{}); err != nil {
		t.Fatal(err)
	}
	roles := map[domain.WorkflowRole]bool{}
	for _, record := range sink.records {
		roles[record.Role] = true
	}
	for _, want := range []domain.WorkflowRole{
		domain.WorkflowRoleWorker, domain.WorkflowRoleReviewer,
		domain.WorkflowRoleFixWorker, domain.WorkflowRoleVerify, domain.WorkflowRolePlanner,
	} {
		if !roles[want] {
			t.Fatalf("no evidence recorded for role %q (got %v)", want, roles)
		}
	}
}

// Nil dependencies must stay nil: workflow treats a nil dispatcher as "this
// step is a no-op", and handing it a non-nil wrapper around nothing would turn
// that into a panic.
func TestInstrumentLeavesNilDependenciesNil(t *testing.T) {
	recorder, _ := newRecorder()
	got := Instrument(workflowcore.Deps{}, recorder, nil)
	if got.Spawner != nil || got.ReviewerLauncher != nil || got.MessageSender != nil || got.Verifier != nil || got.Planner != nil {
		t.Fatalf("a nil dependency was replaced by a wrapper: %+v", got)
	}
}

func (f *fakeReviewer) ReviewerIdentity(req workflowcore.ReviewerLaunchRequest) string {
	return "workflow-review-" + req.RunID
}

func (f *fakeReviewer) ProbeReviewer(stdctx.Context, workflowcore.ReviewerRef) (workflowcore.ReviewerObservation, error) {
	return workflowcore.ReviewerObservation{Presence: workflowcore.ReviewerPresenceAbsent}, nil
}

func (f *fakeReviewer) CancelReviewer(stdctx.Context, workflowcore.ReviewerRef) error { return nil }
