package wfrouter

import (
	stdctx "context"
	"log/slog"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// reviewer.go — P2-A: reaching the Reviewer, the one role the audit found no
// path to.
//
// The audit's finding (docs/p2-project-memory-audit.md §2.3b, §5) was precise:
// the 8C worktree reviewer is told *where* to look and never *what is already
// known* about the project, and `ReviewerLaunchRequest.SystemPrompt` exists
// with no producer anywhere in the repository. So the reviewer needs no new
// dispatch surface and no prompt-builder change either — it needs that field
// filled.
//
// This wrapper fills it, and nothing else. Three rules keep it safe:
//
//   - **It only ever ADDS.** A `SystemPrompt` the caller already set is left
//     exactly as it is. The wrapper is not permitted to edit an instruction,
//     only to supply standing knowledge where there was none.
//   - **It never touches the prompt.** `Prompt` carries the review's task —
//     the objective, the criteria, the branch. Budgeting an instruction would
//     truncate what the reviewer was asked to do, which is why the package
//     doc excludes prompt-carrying surfaces from routing. The system prompt is
//     a different thing: it is assembled context, and it is empty today.
//   - **Every failure sends the original request.** No root, no routing, no
//     sections — each one returns the launch untouched, on the reasoning
//     InstrumentSpawner already established: a payload assembled without its
//     evidence sources is a thinner payload, not a safer one.
//
// Combined with the already-routed Spawner path — which is also how both
// Repair Agents are dispatched — this completes role coverage for P2-A:
// Planner, Worker, Reviewer and Repair.

// reviewerLauncher routes standing project knowledge into a reviewer launch.
type reviewerLauncher struct {
	next     workflowcore.ReviewerLauncher
	router   *contextrouter.Router
	projects workflowcore.Projects
	log      *slog.Logger
}

// InstrumentReviewerLauncher wraps a reviewer-launch path so the reviewer is
// told what AO already knows about the project it is reviewing.
//
// A nil router (the flag being off) or a nil project resolver returns the
// launcher untouched, which is the pre-P2-A behaviour byte for byte.
func InstrumentReviewerLauncher(
	next workflowcore.ReviewerLauncher, router *contextrouter.Router,
	projects workflowcore.Projects, log *slog.Logger,
) workflowcore.ReviewerLauncher {
	if next == nil || router == nil || projects == nil {
		return next
	}
	return &reviewerLauncher{next: next, router: router, projects: projects, log: log}
}

// Preflight passes straight through. It starts nothing and assembles nothing,
// so there is no context for the router to have an opinion about.
func (r *reviewerLauncher) Preflight(ctx stdctx.Context, harness domain.ReviewerHarness, workspacePath string) error {
	return r.next.Preflight(ctx, harness, workspacePath)
}

// Launch routes the standing reviewer context, then launches.
func (r *reviewerLauncher) Launch(ctx stdctx.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	ctx, req = r.route(ctx, req)
	return r.next.Launch(ctx, req)
}

// The three ReviewerEnsurer methods pass straight through.
//
// They are the recovery half of the launch boundary: identify what a request
// would launch under, classify what is at that identity, and cancel it. None
// of them assembles context, and ReviewerIdentity in particular MUST stay pure
// and byte-stable across restarts — routing inside it would make a reviewer's
// identity depend on what the project's memory happened to contain, which is
// the one property the whole recovery path rests on.
func (r *reviewerLauncher) ReviewerIdentity(req workflowcore.ReviewerLaunchRequest) string {
	return r.next.ReviewerIdentity(req)
}

func (r *reviewerLauncher) ProbeReviewer(ctx stdctx.Context, ref workflowcore.ReviewerRef) (workflowcore.ReviewerObservation, error) {
	return r.next.ProbeReviewer(ctx, ref)
}

func (r *reviewerLauncher) CancelReviewer(ctx stdctx.Context, ref workflowcore.ReviewerRef) error {
	return r.next.CancelReviewer(ctx, ref)
}

func (r *reviewerLauncher) route(
	ctx stdctx.Context, req workflowcore.ReviewerLaunchRequest,
) (stdctx.Context, workflowcore.ReviewerLaunchRequest) {
	if strings.TrimSpace(req.SystemPrompt) != "" {
		// Somebody already set standing instructions for this reviewer.
		// Replacing them with routed evidence would be editing an instruction,
		// which this wrapper is not allowed to do.
		return ctx, req
	}
	root, ok := routableRoot(req.WorkspacePath)
	if !ok {
		root, ok = r.projectRoot(ctx, req.ProjectID)
		if !ok {
			return ctx, req
		}
	}

	selection, routed := route(ctx, r.router, r.log, contextrouter.Request{
		Role: contextrouter.RoleReviewer,
		Task: contextrouter.Task{
			ID:        req.RunID,
			Objective: req.Prompt,
		},
		Project: contextrouter.Project{
			ID:   string(req.ProjectID),
			Root: root,
		},
	})
	if !routed {
		return ctx, req
	}

	var b strings.Builder
	for _, section := range selection.Sections {
		if section.Kind == contextrouter.SectionTask {
			// The review prompt already carries the task. Repeating it in the
			// system prompt would spend the reviewer's budget on something it
			// has already been told.
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## ")
		b.WriteString(section.Title)
		b.WriteString("\n")
		b.WriteString(section.Content)
	}
	if b.Len() == 0 {
		// Nothing survived the budget, or there was nothing to say. An empty
		// system prompt is what the reviewer got before P2-A, and setting it
		// to an empty string with a heading would be worse than leaving it.
		return ctx, req
	}

	req.SystemPrompt = "The following is what AO already knows about this project. " +
		"It is a summary derived from the repository at an earlier commit, not the repository itself: " +
		"where it and the worktree in front of you disagree, the worktree is correct.\n\n" + b.String()
	logSelection(r.log, "context router: reviewer standing context routed", selection)
	return baseline.WithRouting(ctx, selection.BaselineRouting()), req
}

// projectRoot resolves a launch's project id to its checkout root, for the
// case where the workspace path is not usable as one.
//
// A reviewer's WorkspacePath is normally the worktree it is reviewing, which
// IS an absolute checkout root and is the more accurate one to route against —
// it is where the diff actually is. The project root is the fallback for a
// launch whose workspace path is relative or empty.
func (r *reviewerLauncher) projectRoot(ctx stdctx.Context, id domain.ProjectID) (string, bool) {
	record, found, err := r.projects.GetProject(ctx, string(id))
	if err != nil || !found {
		if r.log != nil {
			r.log.Warn("context router: no checkout root, launching the reviewer with no standing context",
				"project", string(id), "found", found, "err", err)
		}
		return "", false
	}
	return routableRoot(record.Path)
}
