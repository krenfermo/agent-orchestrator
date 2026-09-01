package workflow

import (
	stdctx "context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// task_knowledge.go — deciding what one task may be told about the tasks
// before it (P2-C §5, §14, §15).
//
// The rule this file implements is short, and every line of it is load-bearing:
//
//	A task may read the unintegrated knowledge of exactly the tasks the PLAN
//	says it depends on, within the run it belongs to, and of nobody else.
//
// It lives in workflow because both halves are workflow facts. The dependency
// edges are in workflow_task_dependencies, put there by the planner; the run
// boundary is the parent workflow. Project memory cannot derive either without
// re-implementing the plan, and if it tried it would have to guess — and the
// only guess available is "these two tasks touched the same files", which is
// precisely the invented relationship P2-C §5 forbids and precisely the hole
// P2-C §15 exists to close. Two parallel tasks editing the same package are
// the case sibling safety is ABOUT.
//
// Nothing here decides what memory does with the entitlement. It hands the
// entitlement over on the context and stops.

// taskAuthorityFor assembles what one run's dispatches are entitled to read.
//
// It fails open toward LESS knowledge, always. A plan store that cannot be
// read, a run with no planned task, a task whose dependencies were never
// recorded — each yields an authority naming fewer upstream tasks, never more.
// There is no failure of this function that can widen what a dispatch sees.
func (c *Coordinator) taskAuthorityFor(ctx stdctx.Context, run domain.WorkflowRun) projectmemory.TaskAuthority {
	auth := projectmemory.TaskAuthority{
		TaskRef:       taskRefFor(run),
		WorkflowRunID: knowledgeRunIDFor(run),
	}
	if run.PlannedTaskID == nil || strings.TrimSpace(*run.PlannedTaskID) == "" || c.planStore == nil {
		return auth.Normalized()
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, auth.WorkflowRunID)
	if err != nil {
		return auth.Normalized()
	}
	for _, task := range tasks {
		if task.ID != *run.PlannedTaskID {
			continue
		}
		auth.UpstreamTaskRefs = append(auth.UpstreamTaskRefs, task.Dependencies...)
		break
	}
	return auth.Normalized()
}

// knowledgeRunIDFor is the run that scopes workflow-local knowledge.
//
// It is the PARENT run when there is one. A child run is one task's execution;
// the workflow whose tasks may share with each other is the parent, and
// scoping by the child would make every task its own workflow and share
// nothing — the feature would be present and inert.
func knowledgeRunIDFor(run domain.WorkflowRun) string {
	if run.ParentWorkflowID != nil && strings.TrimSpace(*run.ParentWorkflowID) != "" {
		return *run.ParentWorkflowID
	}
	return run.ID
}

// withTaskAuthority stamps a dispatch's entitlement onto its context.
//
// Every boundary that assembles context for a task goes through it, and the
// stamp is the ONLY channel by which unintegrated knowledge can be shared. A
// dispatch that never passes through here provisions with a zero authority and
// receives canonical knowledge alone, which is why forgetting to call it can
// make a task less informed and can never make it less safe.
func (c *Coordinator) withTaskAuthority(ctx stdctx.Context, run domain.WorkflowRun) stdctx.Context {
	if c.taskMemory == nil {
		// Memory is switched off. Stamping an entitlement nobody will read
		// would cost a plan read on every dispatch for nothing.
		return ctx
	}
	auth := c.taskAuthorityFor(ctx, run)
	if auth.Empty() {
		return ctx
	}
	return projectmemory.WithTaskAuthority(ctx, auth)
}

// knowledgeShareFor decides how far a finished task's facts may travel when
// the work is not integrated.
//
// There are only two answers, and the boundary between them is verification:
//
//   - A task that VERIFIED its work has produced something AO can vouch for on
//     its own branch. Its facts may reach the tasks that explicitly depend on
//     it, inside its own run, which is what lets Task 2 build on Task 1
//     without waiting for the whole parent workflow to integrate.
//   - Anything else reaches only itself.
//
// Neither answer is canonical. Canonical is decided by integration and by
// nothing else — see promoteIntegratedTaskMemory — because a verified branch
// is proof the work is GOOD, and only integration is proof the project HAS it.
func knowledgeShareFor(step domain.WorkflowStep, integrated bool) domain.KnowledgeShare {
	if integrated {
		return domain.ShareCanonical
	}
	if step.State == domain.WorkflowStepCompleted {
		return domain.ShareWorkflow
	}
	return domain.ShareTask
}

// promoteIntegratedTaskMemory turns an isolated task's facts into project
// knowledge, at the moment integration makes that true (P2-C §4).
//
// It is called from finishTaskWorktree, and that placement is the whole point.
// finishTaskWorktree runs only AFTER the promotion is a durable fact — the
// integration checkpoint is written, the target ref has moved — which is the
// first instant at which "this task's work is part of the repository" is
// something AO can prove rather than something it hopes. Promoting anywhere
// earlier would let a crash leave canonical memory describing work the
// repository does not have.
//
// It inherits finishTaskWorktree's two properties for free, and needs both:
//
//   - **Idempotent.** PromoteTaskMemory addresses derived identities and
//     discards the task-local originals only after the canonical rows are
//     written, so a second call — which a duplicate completion callback or a
//     restart mid-cleanup produces — promotes the same content to the same
//     rows and then finds nothing left to promote.
//   - **Best-effort.** The work is integrated. A memory cache that could not
//     be updated is untidy; failing an integration over it would be a new way
//     to lose work, which is the one thing this subsystem may never become.
//
// A task that never integrates never reaches here, and its facts are discarded
// when the run ends. That is the leak P2-C's completion bar names explicitly,
// and it is closed by this function being the only promotion path an isolated
// worktree has.
func (c *Coordinator) promoteIntegratedTaskMemory(
	ctx stdctx.Context, parent domain.WorkflowRun, taskID, integratedSHA string,
) {
	if c.taskMemory == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(integratedSHA) == "" {
		return
	}
	project, _, ok := c.memoryProjectRoot(ctx, parent)
	if !ok {
		return
	}
	if _, err := c.taskMemory.PromoteTask(ctx, project, taskID, integratedSHA); err != nil && c.log != nil {
		c.log.Warn("project memory: could not promote an integrated task's memory",
			"run", parent.ID, "task", taskID, "sha", shortSHA(integratedSHA), "err", err)
	}
}
