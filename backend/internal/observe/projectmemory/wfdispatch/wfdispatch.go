// Package wfdispatch instruments AO's existing agent dispatch surfaces for the
// project-memory baseline (Phase 0) by wrapping them, never by editing them.
//
// Each decorator implements the exact workflow port it wraps, opens a
// projectmemory span around the call, records what that particular surface can
// honestly report, and delegates. The wrapped dispatcher's arguments, return
// values, and errors pass through untouched: an instrumented pipeline and an
// uninstrumented one execute the same code, which is what makes the numbers a
// baseline of the current pipeline rather than of a modified one.
//
// A nil recorder is not an error — every constructor returns the original
// dispatcher unchanged, so instrumentation costs nothing when it is off.
package wfdispatch

import (
	stdctx "context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// finish closes a span and reports a recording failure without ever turning it
// into a dispatch failure: the baseline observes the pipeline, it does not get
// to break it.
func finish(ctx stdctx.Context, log *slog.Logger, span *projectmemory.Span, dispatchErr error) {
	if _, _, err := span.Finish(ctx, dispatchErr); err != nil && log != nil {
		log.Warn("project-memory baseline: evidence not recorded", "record", span.RecordID(), "err", err)
	}
}

// spawner instruments worker dispatch (workflow.Spawner).
type spawner struct {
	next     workflowcore.Spawner
	recorder *projectmemory.Recorder
	log      *slog.Logger
}

// InstrumentSpawner wraps a worker-spawn path. AO hands the provider a prompt
// and the provider process then reads files on its own, so what is measurable
// here is the payload AO sent, not the reads the agent went on to make.
func InstrumentSpawner(next workflowcore.Spawner, recorder *projectmemory.Recorder, log *slog.Logger) workflowcore.Spawner {
	if next == nil || recorder == nil {
		return next
	}
	return &spawner{next: next, recorder: recorder, log: log}
}

func (s *spawner) Spawn(ctx stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	span := s.recorder.Begin(projectmemory.Dispatch{
		Role:           domain.WorkflowRoleWorker,
		WorkflowRunID:  cfg.WorkflowRunID,
		WorkflowStepID: workflowcore.WorkStepIDFromIssueID(cfg.IssueID),
		ProjectID:      string(cfg.ProjectID),
		Harness:        string(cfg.Harness),
		Observable:     projectmemory.Capabilities{ContextPayload: true},
	})
	span.ObserveContextSent(cfg.Prompt)
	span.ObserveContextSent(cfg.IssueContext)
	span.ObserveRoutingFromContext(ctx)
	span.Note("worker dispatch: AO measures the prompt it sends; the provider process makes its own file reads and tool calls, which this surface does not report")
	rec, sent, queued, err := s.next.Spawn(ctx, cfg)
	span.Identify(string(rec.ID), "", "")
	finish(ctx, s.log, span, err)
	return rec, sent, queued, err
}

// reviewerLauncher instruments reviewer dispatch
// (workflow.ReviewerLauncher).
type reviewerLauncher struct {
	next     workflowcore.ReviewerLauncher
	recorder *projectmemory.Recorder
	log      *slog.Logger
}

// InstrumentReviewerLauncher wraps a reviewer-launch path. Preflight is not
// instrumented: it starts no agent and consumes no context.
func InstrumentReviewerLauncher(next workflowcore.ReviewerLauncher, recorder *projectmemory.Recorder, log *slog.Logger) workflowcore.ReviewerLauncher {
	if next == nil || recorder == nil {
		return next
	}
	return &reviewerLauncher{next: next, recorder: recorder, log: log}
}

func (r *reviewerLauncher) Preflight(ctx stdctx.Context, harness domain.ReviewerHarness, workspacePath string) error {
	return r.next.Preflight(ctx, harness, workspacePath)
}

func (r *reviewerLauncher) Launch(ctx stdctx.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	span := r.recorder.Begin(projectmemory.Dispatch{
		Role:       domain.WorkflowRoleReviewer,
		ProjectID:  string(req.ProjectID),
		Harness:    string(req.Harness),
		Observable: projectmemory.Capabilities{ContextPayload: true},
	})
	span.ObserveContextSent(req.SystemPrompt)
	span.ObserveContextSent(req.Prompt)
	span.ObserveRoutingFromContext(ctx)
	span.LinkReviewRun(req.RunID)
	span.Note("reviewer dispatch: this surface carries no workflow run id, so the record is keyed by the reviewer session it created and linked to its review run")
	result, err := r.next.Launch(ctx, req)
	span.Identify(result.AgentSessionID, "", "")
	finish(ctx, r.log, span, err)
	return result, err
}

// messageSender instruments fix delivery (workflow.MessageSender): the fix
// prompt is sent into the worker's existing session rather than spawned, so
// the message itself is the context this dispatch adds.
type messageSender struct {
	next     workflowcore.MessageSender
	recorder *projectmemory.Recorder
	log      *slog.Logger
}

// reportingMessageSender preserves workflow.SubmissionReportingSender through
// the wrapper. Without it, fix_dispatch.go's type assertion for that optional
// capability would start failing the moment instrumentation was switched on,
// silently downgrading prompt-delivery reporting — an instrumentation layer
// must not change what the pipeline can prove about itself.
type reportingMessageSender struct {
	messageSender
	reporting workflowcore.SubmissionReportingSender
}

// InstrumentMessageSender wraps a fix-delivery path, preserving the optional
// SubmissionReportingSender capability when the wrapped sender has it.
func InstrumentMessageSender(next workflowcore.MessageSender, recorder *projectmemory.Recorder, log *slog.Logger) workflowcore.MessageSender {
	if next == nil || recorder == nil {
		return next
	}
	base := messageSender{next: next, recorder: recorder, log: log}
	if reporting, ok := next.(workflowcore.SubmissionReportingSender); ok {
		return &reportingMessageSender{messageSender: base, reporting: reporting}
	}
	return &base
}

func (m *messageSender) begin(sessionID domain.SessionID, message string) *projectmemory.Span {
	span := m.recorder.Begin(projectmemory.Dispatch{
		Role:       domain.WorkflowRoleFixWorker,
		SessionID:  string(sessionID),
		Observable: projectmemory.Capabilities{ContextPayload: true},
	})
	span.ObserveContextSent(message)
	span.Note("fix dispatch: delivered into the worker's existing session, so this record measures the added context only, not the session's accumulated history")
	return span
}

func (m *messageSender) Send(ctx stdctx.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) error {
	span := m.begin(id, message)
	span.ObserveRoutingFromContext(ctx)
	err := m.next.Send(ctx, id, message, attachment)
	finish(ctx, m.log, span, err)
	return err
}

func (r *reportingMessageSender) SendReportingSubmission(ctx stdctx.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) (ports.PromptSubmission, error) {
	span := r.begin(id, message)
	span.ObserveRoutingFromContext(ctx)
	submission, err := r.reporting.SendReportingSubmission(ctx, id, message, attachment)
	finish(ctx, r.log, span, err)
	return submission, err
}

// SubmitPending submits a draft already in the composer. It writes no new
// payload, so there is no context to measure and nothing to record.
func (r *reportingMessageSender) SubmitPending(ctx stdctx.Context, id domain.SessionID) (ports.PromptSubmission, error) {
	return r.reporting.SubmitPending(ctx, id)
}

// ComposerState reports what the composer holds and writes nothing.
func (r *reportingMessageSender) ComposerState(ctx stdctx.Context, id domain.SessionID) ports.PromptSubmission {
	return r.reporting.ComposerState(ctx, id)
}

// verifier instruments verification (workflow.VerifyRunner). Verify never
// invokes a provider, so its record carries no token metrics at all — only the
// command it ran, how long it took, and how it ended.
type verifier struct {
	next     workflowcore.VerifyRunner
	recorder *projectmemory.Recorder
	log      *slog.Logger
}

// InstrumentVerifier wraps a verify-command runner.
func InstrumentVerifier(next workflowcore.VerifyRunner, recorder *projectmemory.Recorder, log *slog.Logger) workflowcore.VerifyRunner {
	if next == nil || recorder == nil {
		return next
	}
	return &verifier{next: next, recorder: recorder, log: log}
}

func (v *verifier) Run(ctx stdctx.Context, req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
	span := v.recorder.Begin(projectmemory.Dispatch{
		Role:       domain.WorkflowRoleVerify,
		Observable: projectmemory.Capabilities{ToolCalls: true},
	})
	span.ObserveToolCall(req.Command)
	span.Note("verify dispatch: a deterministic command run, not a provider call, so every token metric here is genuinely inapplicable rather than merely missing")
	execution, err := v.next.Run(ctx, req)
	span.LinkVerifyOutcome(execution.ExitCode, execution.DurationMS)
	finish(ctx, v.log, span, err)
	return execution, err
}

// planner instruments plan generation (workflow.Planner). This is the one
// surface where AO itself assembles the context document, so the files that
// went into it are measurable rather than merely estimated.
type planner struct {
	next     workflowcore.Planner
	recorder *projectmemory.Recorder
	log      *slog.Logger
}

// describingPlanner preserves workflow.PlannerDescriptor through the wrapper,
// for the same reason reportingMessageSender preserves
// SubmissionReportingSender: master_coordinator.go asks the planner which
// provider and model it is, and an instrumentation layer that made that
// question start returning "unknown" would have changed what the pipeline
// records about itself.
type describingPlanner struct {
	planner
	descriptor workflowcore.PlannerDescriptor
}

// Descriptor reports the wrapped planner's provider and model.
func (d *describingPlanner) Descriptor() (provider, model string) {
	return d.descriptor.Descriptor()
}

// InstrumentPlanner wraps a planner, preserving the optional PlannerDescriptor
// capability when the wrapped planner has it.
func InstrumentPlanner(next workflowcore.Planner, recorder *projectmemory.Recorder, log *slog.Logger) workflowcore.Planner {
	if next == nil || recorder == nil {
		return next
	}
	base := planner{next: next, recorder: recorder, log: log}
	if descriptor, ok := next.(workflowcore.PlannerDescriptor); ok {
		return &describingPlanner{planner: base, descriptor: descriptor}
	}
	return &base
}

func (p *planner) Generate(ctx stdctx.Context, request workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	span := p.recorder.Begin(projectmemory.Dispatch{
		Role:       domain.WorkflowRolePlanner,
		ProjectID:  request.Project.ID,
		Observable: projectmemory.Capabilities{FileReads: true, ContextPayload: true},
	})
	RecordPlannerContext(span, request)
	span.ObserveRoutingFromContext(ctx)
	response, err := p.next.Generate(ctx, request)
	span.Identify("", response.Provider, response.Model)
	finish(ctx, p.log, span, err)
	return response, err
}

// RecordPlannerContext records the objective and every document AO put into a
// planner request as this dispatch's context. It is exported because the
// baseline harness assembles the same request through the same unmodified
// context builder and must record it identically.
func RecordPlannerContext(span *projectmemory.Span, request workflowcore.PlannerRequest) {
	span.ObserveContextSent(request.Objective)
	for _, doc := range request.Context.Documents {
		span.ObserveFileRead(doc.Path, int64(len(doc.Content)))
		span.ObserveContextSent(doc.Content)
	}
	span.Note("planner dispatch: AO assembles this context document itself, so its file reads are measured rather than inferred")
}

// Instrument wraps every agent dispatch surface in deps that the baseline can
// observe, leaving the rest of the wiring untouched. A nil recorder returns
// deps unchanged.
func Instrument(deps workflowcore.Deps, recorder *projectmemory.Recorder, log *slog.Logger) workflowcore.Deps {
	if recorder == nil {
		return deps
	}
	deps.Spawner = InstrumentSpawner(deps.Spawner, recorder, log)
	deps.ReviewerLauncher = InstrumentReviewerLauncher(deps.ReviewerLauncher, recorder, log)
	deps.MessageSender = InstrumentMessageSender(deps.MessageSender, recorder, log)
	deps.Verifier = InstrumentVerifier(deps.Verifier, recorder, log)
	deps.Planner = InstrumentPlanner(deps.Planner, recorder, log)
	return deps
}
