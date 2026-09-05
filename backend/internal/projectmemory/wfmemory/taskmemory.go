package wfmemory

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// taskmemory.go — the adapter between workflow's task boundaries and the
// memory subsystem (P2-B §11).
//
// It exists so neither side has to know the other's vocabulary: workflow
// speaks in runs, tasks and placements, project memory speaks in origins,
// generations and provenance. The translation is small and lives in exactly
// one place, which is what stops "is this work integrated" from being decided
// twice with two different answers.
//
// The adapter itself makes no policy. Whether a task's memory may become
// canonical is decided by the workflow boundary that knows the placement (see
// workflow/task_memory.go); this type only carries that decision across.

// TaskMemoryAdapter satisfies workflow.TaskMemory over the memory service.
type TaskMemoryAdapter struct {
	svc *projectmemory.Service
	cfg projectmemory.Config
}

// NewTaskMemory builds the adapter. A nil service yields nil — and workflow
// treats a nil TaskMemory as the ordinary "memory is off" state rather than as
// a failure.
//
// P4-H: THE ROLLOUT MODE NO LONGER GATES THIS, and the distinction it draws is
// the one the phase turns on.
//
// AO_MEMORY_MODE governs CONSUMPTION: whether an agent is handed a memory pack,
// and whether that pack may stand in for a legacy document. Those change what a
// model is told and deserve a staged rollout. Everything this adapter does is
// RECORDING: what a finished task changed, which paths its changes invalidate,
// and — under a proof the workflow boundary produced — which of its facts have
// become the project's knowledge. None of it alters a single dispatch.
//
// Gating recording behind the consumption rollout meant that on a default
// installation a workflow could run, verify, integrate and be forgotten: §11's
// workflow-learned memory had the same shape of bug as the derivation itself,
// where the machinery existed and nothing ever reached it. The provisioner
// (memoryProvisioner) still honours the mode, so a cautious operator gets a
// memory that learns and dispatches that are byte-for-byte what they were.
func NewTaskMemory(svc *projectmemory.Service, cfg projectmemory.Config) *TaskMemoryAdapter {
	if svc == nil {
		return nil
	}
	return &TaskMemoryAdapter{svc: svc, cfg: cfg}
}

// Compile-time proof that the adapter satisfies the port workflow declares.
var _ workflowcore.TaskMemory = (*TaskMemoryAdapter)(nil)

// InvalidatePaths retires memory derived from paths a task changed.
func (a *TaskMemoryAdapter) InvalidatePaths(
	ctx stdctx.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string,
) (int64, error) {
	if a == nil {
		return 0, nil
	}
	return a.svc.Invalidate(ctx, projectID, repoPath, paths, reason)
}

// RecordOutcome persists the bounded facts of a finished task.
//
// Origin follows Integrated, and nothing else: work the placement proved is in
// the repository's own history produces canonical memory, and everything else
// produces task-local memory visible only to the task that made it. The memory
// package enforces the rest of that rule at read time.
func (a *TaskMemoryAdapter) RecordOutcome(ctx stdctx.Context, facts workflowcore.TaskOutcomeFacts) error {
	if a == nil {
		return nil
	}
	return a.svc.RecordTaskOutcome(ctx, projectmemory.TaskOutcome{
		ProjectID:      facts.ProjectID,
		RepoPath:       facts.RepoPath,
		TaskRef:        facts.TaskRef,
		Title:          facts.Title,
		WhatChanged:    facts.WhatChanged,
		Why:            facts.Why,
		FilesChanged:   facts.FilesChanged,
		Modules:        facts.Modules,
		Verification:   facts.Verification,
		Commit:         facts.Commit,
		Integrated:     facts.Integrated,
		RepoIdentity:   facts.RepoIdentity,
		VerifiedCommit: facts.VerifiedCommit,
		Promotion:      facts.Promotion,
		WorkflowRunID:  facts.WorkflowRunID,
		Share:          facts.Share,
		DependsOnTasks: facts.DependsOnTasks,
		Decisions:      taskDecisions(facts.Decisions),
		Risks:          taskRisks(facts.Risks),
		ResolvesRisks:  facts.ResolvesRisks,
	})
}

// PromoteTask turns one task's facts into canonical project knowledge, under
// the proof the calling boundary produced.
//
// The adapter still makes no policy: it does not inspect the proof, does not
// second-guess a refusal, and does not construct one of its own. Whether the
// work is durably in the repository is a workflow question answered by
// workflow rows, and the memory service's job is to record the answer.
func (a *TaskMemoryAdapter) PromoteTask(
	ctx stdctx.Context, projectID domain.ProjectID, taskRef string, proof domain.MemoryPromotionProof,
) (int, error) {
	if a == nil {
		return 0, nil
	}
	return a.svc.PromoteTaskMemory(ctx, projectID, taskRef, proof)
}

// DiscardTask drops one task's unintegrated facts.
func (a *TaskMemoryAdapter) DiscardTask(
	ctx stdctx.Context, projectID domain.ProjectID, taskRef string,
) (int64, error) {
	if a == nil {
		return 0, nil
	}
	return a.svc.DiscardTaskMemory(ctx, projectID, taskRef)
}

// taskDecisions and taskRisks translate workflow's vocabulary for a derived
// fact into memory's.
//
// The translation is a copy and nothing else. Scope is left unset on purpose,
// so normalizeKnowledgeScope resolves it to the repository — a decision derived
// from a plan amendment or a risk derived from a reviewer thread is a claim
// about this repository's work, and narrowing it to a module here would be a
// judgement the source row does not support. Evidence stays empty unless the
// source PROVED a path, in which case memory anchors on it; otherwise memory
// falls back to the task's own changed paths.
func taskDecisions(in []workflowcore.TaskDecisionFact) []projectmemory.TaskDecision {
	if len(in) == 0 {
		return nil
	}
	out := make([]projectmemory.TaskDecision, 0, len(in))
	for _, d := range in {
		out = append(out, projectmemory.TaskDecision{
			Statement: d.Statement,
			Rationale: d.Rationale,
			Topic:     d.Topic,
			Evidence:  d.Evidence,
		})
	}
	return out
}

func taskRisks(in []workflowcore.TaskRiskFact) []projectmemory.TaskRisk {
	if len(in) == 0 {
		return nil
	}
	out := make([]projectmemory.TaskRisk, 0, len(in))
	for _, r := range in {
		out = append(out, projectmemory.TaskRisk{
			Statement: r.Statement,
			Kind:      r.Kind,
			Topic:     r.Topic,
			Evidence:  r.Evidence,
		})
	}
	return out
}
