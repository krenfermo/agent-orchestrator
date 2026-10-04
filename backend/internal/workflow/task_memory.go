package workflow

import (
	stdctx "context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// task_memory.go — feeding a finished task's outcome back into project memory
// (P2-B §11, §12, §13).
//
// P2-A built the durable task-memory API and deliberately left it with no
// caller, because *where* it is called decides two things that are not the
// memory subsystem's to decide: what counts as a task having produced a fact
// worth keeping, and which authority may promote that fact from one task's
// opinion to the project's knowledge.
//
// Both answers live here, at the boundary that already knows them:
//
//   - **Recording** happens when a run is verified and its work has been
//     committed — inside completeVerifiedRun, after autonomousLocalCommit
//     succeeded and while the branch lock is still held. That is the first
//     moment AO can say the work is both good and durable.
//   - **Promotion** is gated on the same proof, and on the placement. A task on
//     a direct branch has, by committing, put its work where the repository's
//     own history is, so its facts are canonical. A task in an isolated
//     worktree has not: its commit is on a branch nothing has integrated, so
//     its facts stay task-local until an integration authority says otherwise.
//   - **Discarding** happens when a run ends without that proof — cancelled,
//     failed, or stopped. Nothing it believed becomes project knowledge.
//
// Invalidation is separate and runs first: the moment AO knows which paths a
// task wrote, every memory item derived from those paths stops being served,
// whether or not anything is recorded. That closes the window in which a
// Reviewer launched seconds later would be handed a summary of a file as it
// was before the work.

// TaskMemory is the slice of the project-memory subsystem this package needs.
//
// It is declared here, at the consumer, and is deliberately four verbs wide:
// anything larger would let workflow logic drift into the memory package, and
// anything smaller would force this file to know how memory is stored.
//
// A nil TaskMemory disables all of it, which is what a daemon with memory
// switched off has — and every call site treats that as normal rather than as
// a degraded mode.
type TaskMemory interface {
	// InvalidatePaths retires memory derived from paths a task has changed.
	InvalidatePaths(ctx stdctx.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string) (int64, error)
	// RecordOutcome persists the bounded facts of a finished task.
	RecordOutcome(ctx stdctx.Context, outcome TaskOutcomeFacts) error
	// PromoteTask turns one task's facts into canonical project knowledge,
	// under a proof that the work is durably part of the repository.
	//
	// The proof travels rather than a bare commit because P2-D's whole
	// argument is that "a commit exists" is not "the project has this work".
	// A caller that cannot prove it passes a refusal carrying its reason, and
	// the facts are recorded as unprovable instead of being promoted -- which
	// is a withheld fact with an explanation, not a silent absence.
	PromoteTask(ctx stdctx.Context, projectID domain.ProjectID, taskRef string, proof domain.MemoryPromotionProof) (int, error)
	// DiscardTask drops one task's unintegrated facts.
	DiscardTask(ctx stdctx.Context, projectID domain.ProjectID, taskRef string) (int64, error)
}

// TaskOutcomeFacts is what workflow knows about a finished task that is worth
// remembering.
//
// There is no transcript field and nowhere to put one. Everything a later task
// needs is here — what the work was, what it touched, how it verified — and
// everything else is already durable in AO's own workflow rows, where it is
// queryable and bounded.
type TaskOutcomeFacts struct {
	ProjectID domain.ProjectID
	RepoPath  string
	// TaskRef is the durable identity later memory is filed under. It is the
	// planned task id when there is one, and the run id otherwise, so a run
	// with no plan still records its outcome under something stable.
	TaskRef string
	Title   string
	// WhatChanged and Why are the two sentences a later task actually reads.
	WhatChanged string
	Why         string
	// FilesChanged and Modules come from the task's durable scope, and are the
	// relevance anchors of the resulting fact.
	FilesChanged []string
	Modules      []string
	// Verification is how the work was proved, in one line.
	Verification string
	// Commit is where the work landed.
	Commit string
	// Integrated reports whether the work is part of the repository's
	// integrated state. Only integrated work produces canonical memory.
	//
	// P2-D: it is now the CONCLUSION of Promotion below rather than a reading
	// of the project's execution mode. A direct-branch task whose verified
	// head AO cannot locate in the branch's history is not integrated, however
	// the project is configured.
	Integrated bool
	// Promotion is what AO proved about where this work landed. It is carried
	// on the facts so that the record written for an UNPROVABLE task still
	// says why -- see recordTaskMemory.
	Promotion domain.MemoryPromotionProof
	// RepoIdentity is the durable identity of the repository these facts are
	// about, stamped onto every row so a different repository later checked
	// out at the same path cannot inherit them (P2-D section 9).
	RepoIdentity domain.RepoIdentity
	// VerifiedCommit is the head verification actually passed on. It is kept
	// apart from Commit because they answer different questions: Commit is
	// where the content was read, VerifiedCommit is what AO vouched for.
	VerifiedCommit string

	// --- shared knowledge (P2-C) -----------------------------------------

	// WorkflowRunID is the run this task belonged to. It scopes workflow-local
	// sharing, so a later run that names the same upstream task reads nothing.
	WorkflowRunID string
	// Share is how far these facts may travel while the work is not
	// integrated. See knowledgeShareFor: verified work may reach the tasks
	// that declared a dependency on it, and everything else reaches only
	// itself.
	Share domain.KnowledgeShare
	// DependsOnTasks are the tasks the PLAN says this one depends on. They are
	// copied from workflow_task_dependencies, never inferred — two tasks that
	// touched the same files are not related (P2-C §5).
	DependsOnTasks []string

	// Decisions and Risks are derived from the durable rows AO already holds —
	// the plan's amendment ledger, the question ledger, the Post-Run QA gate,
	// the review runs and their threads. See task_knowledge_sources.go for
	// which rows count and why the rest do not. A task whose sources hold
	// nothing contributes nothing, never a guess.
	Decisions []TaskDecisionFact
	Risks     []TaskRiskFact
	// ResolvesRisks names, by topic, the risks a durable source now reports as
	// closed. Emitting nothing would leave an earlier task's risk active
	// forever; this is what makes "it was dealt with" a fact rather than a
	// silence.
	ResolvesRisks []string
}

// recordTaskMemory is called from completeVerifiedRun, after the work has been
// committed and before the run goes terminal.
//
// It is best-effort by construction: every failure is logged and swallowed. A
// run that did its work, passed verification and committed must not be failed
// because a cache could not be updated — that would make an optimisation into
// a new way for AO to lose good work.
func (c *Coordinator) recordTaskMemory(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) {
	if c.taskMemory == nil {
		return
	}
	project, repoPath, ok := c.memoryProjectRoot(ctx, run)
	if !ok {
		return
	}
	facts := c.taskOutcomeFacts(ctx, run, step, repoPath)

	// Invalidate first, and unconditionally. Memory derived from a path this
	// task rewrote is wrong the moment the commit lands, whether or not the
	// outcome below is worth recording.
	if len(facts.FilesChanged) > 0 {
		if _, err := c.taskMemory.InvalidatePaths(ctx, project, repoPath, facts.FilesChanged,
			"changed by task "+facts.TaskRef+" at "+shortCommit(facts.Commit)); err != nil && c.log != nil {
			c.log.Warn("project memory: could not invalidate the paths a task changed",
				"run", run.ID, "err", err)
		}
	}

	if err := c.taskMemory.RecordOutcome(ctx, facts); err != nil && c.log != nil {
		c.log.Warn("project memory: could not record a task outcome", "run", run.ID, "err", err)
	}

	// Promotion, only where the placement proves the work is integrated. An
	// isolated worktree's commit is on a branch nothing has merged, so its
	// facts stay task-local and are promoted by whatever later authority
	// integrates them.
	if !facts.Integrated {
		return
	}
	if _, err := c.taskMemory.PromoteTask(ctx, project, facts.TaskRef, facts.Promotion); err != nil && c.log != nil {
		c.log.Warn("project memory: could not promote a verified task's memory", "run", run.ID, "err", err)
	}
}

// discardTaskMemory is called when a run ends without proving its work.
//
// A cancelled or failed task's beliefs are not project knowledge, and leaving
// them as task-local rows forever would be the "permanent parallel memory" the
// worktree rule exists to prevent.
func (c *Coordinator) discardTaskMemory(ctx stdctx.Context, run domain.WorkflowRun) {
	if c.taskMemory == nil {
		return
	}
	project, _, ok := c.memoryProjectRoot(ctx, run)
	if !ok {
		return
	}
	if _, err := c.taskMemory.DiscardTask(ctx, project, taskRefFor(run)); err != nil && c.log != nil {
		c.log.Warn("project memory: could not discard an unintegrated task's memory", "run", run.ID, "err", err)
	}
}

// taskOutcomeFacts assembles what is known, and nothing more.
//
// Every field comes from a durable row AO already holds. Nothing is inferred
// from prose and nothing is invented: a task whose scope was never observed
// contributes no file list rather than a guessed one, because a wrong
// relevance anchor is worse than a missing one.
func (c *Coordinator) taskOutcomeFacts(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, repoPath string,
) TaskOutcomeFacts {
	facts := TaskOutcomeFacts{
		ProjectID:    domain.ProjectID(run.ProjectID),
		RepoPath:     repoPath,
		TaskRef:      taskRefFor(run),
		WhatChanged:  run.Objective,
		Verification: verificationSummary(step),
	}
	facts.Title, facts.Why = titleAndReason(run.Objective)
	facts.Commit = c.completedRunCommit(ctx, run)
	facts.RepoIdentity = c.memoryRepoIdentity(ctx, repoPath)

	// P2-D: integration is now something AO proves, not something it reads off
	// the project's configuration. directBranchPromotionProof refuses when the
	// placement is not direct branch, so an isolated-worktree task lands on
	// exactly the same "not integrated yet" path it took before -- carrying,
	// now, the reason it is waiting.
	facts.Promotion = c.directBranchPromotionProof(ctx, run, facts.TaskRef, repoPath)
	facts.Integrated = facts.Promotion.Provable
	facts.VerifiedCommit = facts.Promotion.VerifiedCommit
	if facts.Integrated && facts.Commit == "" {
		// A proven promotion names the commit the knowledge is about, so a
		// checkpoint scan that found nothing does not leave the fact
		// unanchored.
		facts.Commit = facts.Promotion.IntegratedCommit
	}

	if scope, ok := c.taskScope(ctx, run); ok {
		facts.FilesChanged = boundedPaths(scope.WritePaths)
		facts.Modules = boundedPaths(scope.Packages)
	}

	// The sharing decision and the task's declared lineage. Both are read from
	// the same durable plan the dispatch side reads, through the same helper,
	// so "which tasks is this one related to" cannot get two answers.
	auth := c.taskAuthorityFor(ctx, run)
	facts.WorkflowRunID = auth.WorkflowRunID
	facts.DependsOnTasks = auth.UpstreamTaskRefs
	facts.Share = knowledgeShareFor(step, facts.Integrated)

	// The decisions and risks the durable sources prove. Assembling them here,
	// with everything else this task knows, is what keeps one recording one
	// snapshot: a second pass over the same rows produces the same facts, which
	// is what makes a duplicate completion callback and a restart idempotent.
	facts.Decisions = c.durableTaskDecisions(ctx, run)
	facts.Risks, facts.ResolvesRisks = c.durableTaskRisks(ctx, run)
	return facts
}

// taskScope reads the planned task's durable read/write footprint, through the
// resolver the placement path already uses. Reusing it is deliberate: two
// readers of one scope that disagreed about how to unmarshal it would be a
// silent source of divergent decisions.
func (c *Coordinator) taskScope(ctx stdctx.Context, run domain.WorkflowRun) (domain.WorkflowTaskScope, bool) {
	if run.PlannedTaskID == nil || strings.TrimSpace(*run.PlannedTaskID) == "" {
		return domain.WorkflowTaskScope{}, false
	}
	return c.taskScopeFor(ctx, run, *run.PlannedTaskID)
}

// memoryProjectRoot resolves the checkout project memory is keyed by.
//
// It is deliberately the PROJECT root, never a task worktree. A worktree is
// not a repository (see docs/project-memory.md §7), and indexing one would
// create exactly the parallel permanent memory the model forbids.
func (c *Coordinator) memoryProjectRoot(ctx stdctx.Context, run domain.WorkflowRun) (domain.ProjectID, string, bool) {
	if c.projects == nil || strings.TrimSpace(run.ProjectID) == "" {
		return "", "", false
	}
	project, found, err := c.projects.GetProject(ctx, run.ProjectID)
	if err != nil || !found || strings.TrimSpace(project.Path) == "" {
		return "", "", false
	}
	return domain.ProjectID(run.ProjectID), project.Path, true
}

// taskRefFor is the identity a task's memory is filed under.
func taskRefFor(run domain.WorkflowRun) string {
	if run.PlannedTaskID != nil && strings.TrimSpace(*run.PlannedTaskID) != "" {
		return *run.PlannedTaskID
	}
	return run.ID
}

// completedRunCommit reports the commit this run's work landed at, from the
// durable checkpoint evidence AO already recorded.
//
// An unknown commit is left EMPTY rather than guessed. A memory item with no
// commit is one drift detection treats as unprovable, which is the correct
// consequence; an item stamped with a commit its content did not come from
// would read as current forever.
func (c *Coordinator) completedRunCommit(ctx stdctx.Context, run domain.WorkflowRun) string {
	if c.store == nil {
		return ""
	}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return ""
	}
	for i := len(checkpoints) - 1; i >= 0; i-- {
		if sha := strings.TrimSpace(checkpoints[i].HeadSHA); sha != "" {
			return sha
		}
	}
	return ""
}

// verificationSummary renders how the work was proved, in one line.
func verificationSummary(step domain.WorkflowStep) string {
	if step.State == domain.WorkflowStepCompleted || step.State == domain.WorkflowStepRunning {
		return "verified by the workflow's own verification step"
	}
	return "verification state " + string(step.State)
}

// titleAndReason splits an objective into a short title and the rest.
//
// It is crude on purpose: the objective is the only prose AO has, and
// inventing a summary of it with a model would be exactly the unbounded,
// unverifiable derivation this memory model refuses.
func titleAndReason(objective string) (title, reason string) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return "task", ""
	}
	if idx := strings.IndexAny(objective, ".\n"); idx > 0 {
		return strings.TrimSpace(objective[:idx]), strings.TrimSpace(objective[idx+1:])
	}
	return objective, ""
}

// boundedPaths caps a path list so one task's outcome cannot carry an
// unbounded relevance anchor.
func boundedPaths(in []string) []string {
	const limit = 32
	out := make([]string, 0, min(len(in), limit))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func shortCommit(sha string) string {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "an unknown commit"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
