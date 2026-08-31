// Package wfrouter wires the role-aware context router into AO's existing
// agent dispatch surfaces by wrapping them, never by editing them.
//
// It is the same shape as internal/observe/projectmemory/wfdispatch, and for
// the same reason: a decorator that implements the workflow port it wraps can
// be switched off by simply not being installed, and an uninstrumented
// pipeline then runs byte-for-byte the code it ran before. A nil router
// returns every dependency unchanged, which is what makes
// contextrouter.FlagEnv defaulting to off a real default rather than a
// configuration that merely looks disabled.
//
// Two surfaces are routed, and only two: the ones where AO itself assembles a
// context payload rather than composing an instruction.
//
//   - The planner's PlannerContext.Documents — today every document AO found
//     is sent in full.
//   - A worker spawn's SpawnConfig.IssueContext — today the whole pre-fetched
//     tracker context is sent in full.
//
// P2-A adds a third: the reviewer's SystemPrompt, which had no producer at all
// before that checkpoint, so filling it adds standing project knowledge where
// there was none rather than budgeting an existing instruction. See
// reviewer.go.
//
// The fix and verify surfaces carry prompts (the specific correction, the
// command to run), not assembled context. Budgeting a prompt would truncate
// instructions rather than evidence, so those surfaces are still left alone
// even though the router budgets their roles — a role's budget exists for
// callers that assemble context for it, and the Repair Agents are reached
// through the Spawner path above rather than through the fix delivery surface
// (docs/p2-project-memory-audit.md §2.6).
//
// Both routed surfaces need the checkout's absolute root: it is what the diff
// source runs git in and what the code graph is keyed by. The planner request
// carries it; a worker spawn carries only a project id, so the wrapper resolves
// the root through the same workflow.Projects port the coordinator already
// uses. A root that cannot be resolved means the two evidence sources that
// justify routing are both unavailable, so the wrapper sends the original
// payload rather than a shrunken one assembled from what was left.
package wfrouter

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// evidencePathPrefix labels the synthetic documents the router contributes, so
// a reader of a planner context can tell an assembled evidence block from a
// file that actually exists in the checkout.
const evidencePathPrefix = "ao://context-router/"

// Instrument wraps the dispatch surfaces the router can route. A nil router —
// which is what the feature flag being off produces — returns deps untouched.
func Instrument(deps workflowcore.Deps, router *contextrouter.Router, log *slog.Logger) workflowcore.Deps {
	if router == nil {
		return deps
	}
	deps.Planner = InstrumentPlanner(deps.Planner, router, log)
	deps.Spawner = InstrumentSpawner(deps.Spawner, router, deps.Projects, log)
	// P2-A: the reviewer's standing system prompt, which had no producer at
	// all before this checkpoint (see reviewer.go).
	deps.ReviewerLauncher = InstrumentReviewerLauncher(deps.ReviewerLauncher, router, deps.Projects, log)
	return deps
}

// planner routes the context AO assembles for plan generation.
type planner struct {
	next   workflowcore.Planner
	router *contextrouter.Router
	log    *slog.Logger
}

// describingPlanner preserves workflow.PlannerDescriptor through the wrapper.
// Without it, master_coordinator.go's type assertion for that optional
// capability would start failing the moment routing was switched on, silently
// downgrading what the pipeline records about which provider planned a run.
type describingPlanner struct {
	planner
	descriptor workflowcore.PlannerDescriptor
}

// Descriptor reports the wrapped planner's provider and model.
func (d *describingPlanner) Descriptor() (provider, model string) {
	return d.descriptor.Descriptor()
}

// InstrumentPlanner wraps a planner so its context documents are routed
// against the planner budget, preserving the optional PlannerDescriptor
// capability when the wrapped planner has it.
func InstrumentPlanner(next workflowcore.Planner, router *contextrouter.Router, log *slog.Logger) workflowcore.Planner {
	if next == nil || router == nil {
		return next
	}
	base := planner{next: next, router: router, log: log}
	if descriptor, ok := next.(workflowcore.PlannerDescriptor); ok {
		return &describingPlanner{planner: base, descriptor: descriptor}
	}
	return &base
}

func (p *planner) Generate(ctx stdctx.Context, request workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	ctx, request = p.route(ctx, request)
	return p.next.Generate(ctx, request)
}

// route replaces the planner's documents with the routed selection. A routing
// failure returns the request untouched: the router exists to send less, and
// sending nothing because it could not decide what to send would be a strictly
// worse outcome than the full-context behaviour it replaces.
//
// It returns a context carrying what the router decided, so the independent
// baseline recorder wrapped INSIDE this one records the routing alongside the
// payload it measures. Neither wrapper requires the other to be installed:
// without this one the context simply carries nothing, and the recorder says
// so (see baseline.WithRouting).
func (p *planner) route(ctx stdctx.Context, request workflowcore.PlannerRequest) (stdctx.Context, workflowcore.PlannerRequest) {
	root, ok := routableRoot(request.Project.Path)
	if !ok {
		if p.log != nil {
			p.log.Warn("context router: no checkout root, sending the unrouted planner context", "project", request.Project.ID, "path", request.Project.Path)
		}
		return ctx, request
	}
	docs := make([]contextrouter.Document, 0, len(request.Context.Documents))
	for _, doc := range request.Context.Documents {
		docs = append(docs, contextrouter.Document{Path: doc.Path, Content: doc.Content})
	}
	selection, selected := route(ctx, p.router, p.log, contextrouter.Request{
		Role: contextrouter.RolePlanner,
		Task: contextrouter.Task{
			ID:        request.Project.ID,
			Objective: request.Objective,
		},
		Project: contextrouter.Project{
			ID:   request.Project.ID,
			Root: root,
		},
		Documents: docs,
	})
	if !selected {
		return ctx, request
	}
	routed := make([]workflowcore.PlannerDocument, 0, len(selection.Sections))
	for _, section := range selection.Sections {
		if section.Kind == contextrouter.SectionTask {
			// The objective already travels in PlannerRequest.Objective; a
			// second copy would spend budget restating what the planner was
			// asked in the same message.
			continue
		}
		path := section.Title
		if section.Kind != contextrouter.SectionDocument {
			path = evidencePathPrefix + string(section.Kind) + "/" + section.Title
		}
		sum := sha256.Sum256([]byte(section.Content))
		routed = append(routed, workflowcore.PlannerDocument{
			Path:    path,
			SHA256:  hex.EncodeToString(sum[:]),
			Content: section.Content,
		})
	}
	request.Context.Documents = routed
	logSelection(p.log, "context router: planner context routed", selection)
	return baseline.WithRouting(ctx, selection.BaselineRouting()), request
}

// spawner routes the issue context AO pre-fetches for a worker spawn.
type spawner struct {
	next     workflowcore.Spawner
	router   *contextrouter.Router
	projects workflowcore.Projects
	log      *slog.Logger
}

// InstrumentSpawner wraps a worker-spawn path so its pre-fetched issue context
// is routed against the worker budget. The prompt is never touched: it carries
// the instruction, not the evidence.
//
// projects resolves the spawn's project id to its checkout root — the absolute
// path the diff source runs git in and the code graph is keyed by. A spawn
// config carries no path of its own, so without this port the two evidence
// sources that justify routing a worker payload would both be silently
// unavailable. A nil resolver therefore disables worker routing rather than
// producing a payload assembled from whatever was left.
func InstrumentSpawner(next workflowcore.Spawner, router *contextrouter.Router, projects workflowcore.Projects, log *slog.Logger) workflowcore.Spawner {
	if next == nil || router == nil {
		return next
	}
	return &spawner{next: next, router: router, projects: projects, log: log}
}

func (s *spawner) Spawn(ctx stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	ctx, cfg = s.route(ctx, cfg)
	return s.next.Spawn(ctx, cfg)
}

// route replaces the spawn's pre-fetched issue context with the routed
// selection, and returns a context carrying what the router decided for the
// baseline recorder inside it — see planner.route.
func (s *spawner) route(ctx stdctx.Context, cfg ports.SpawnConfig) (stdctx.Context, ports.SpawnConfig) {
	if strings.TrimSpace(cfg.Prompt) == "" && strings.TrimSpace(cfg.IssueContext) == "" {
		return ctx, cfg
	}
	root, ok := s.projectRoot(ctx, cfg.ProjectID)
	if !ok {
		return ctx, cfg
	}
	var docs []contextrouter.Document
	if strings.TrimSpace(cfg.IssueContext) != "" {
		docs = append(docs, contextrouter.Document{Path: "issue context", Content: cfg.IssueContext})
	}
	selection, routed := route(ctx, s.router, s.log, contextrouter.Request{
		Role: contextrouter.RoleWorker,
		Task: contextrouter.Task{
			ID:        string(cfg.IssueID),
			Objective: cfg.Prompt,
		},
		Project: contextrouter.Project{
			ID:      string(cfg.ProjectID),
			Root:    root,
			BaseRef: cfg.BaseRef,
		},
		Documents: docs,
	})
	if !routed {
		return ctx, cfg
	}
	var b strings.Builder
	for _, section := range selection.Sections {
		if section.Kind == contextrouter.SectionTask {
			// The prompt already carries the task; the routed issue context
			// carries what supports it.
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
	cfg.IssueContext = b.String()
	logSelection(s.log, "context router: worker issue context routed", selection)
	return baseline.WithRouting(ctx, selection.BaselineRouting()), cfg
}

// projectRoot resolves a spawn's project id to its checkout root, and reports
// whether routing may proceed. Every reason it cannot — no resolver, a lookup
// failure, an unregistered project, a record with no usable path — leaves the
// spawn on its original full context, because a routed payload assembled
// without a root would be a smaller payload with the diff and graph evidence
// silently missing.
func (s *spawner) projectRoot(ctx stdctx.Context, id domain.ProjectID) (string, bool) {
	if s.projects == nil {
		s.warnNoRoot(id, "no project resolver is wired", nil)
		return "", false
	}
	record, found, err := s.projects.GetProject(ctx, string(id))
	if err != nil {
		s.warnNoRoot(id, "project lookup failed", err)
		return "", false
	}
	if !found {
		s.warnNoRoot(id, "project is not registered", nil)
		return "", false
	}
	root, ok := routableRoot(record.Path)
	if !ok {
		s.warnNoRoot(id, "project record carries no absolute checkout path", nil)
		return "", false
	}
	return root, true
}

func (s *spawner) warnNoRoot(id domain.ProjectID, reason string, err error) {
	if s.log == nil {
		return
	}
	s.log.Warn("context router: no checkout root, sending the unrouted issue context", "project", string(id), "reason", reason, "err", err)
}

// routableRoot returns the cleaned checkout root a routed request may use, and
// reports whether there is one.
//
// A relative path is refused rather than resolved against the daemon's working
// directory — the same rule contextrouter.GitDiffSource and the code graph
// apply to their own roots, restated here so an unusable root is caught at the
// dispatch boundary instead of becoming a per-source failure inside the router.
func routableRoot(path string) (string, bool) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || !filepath.IsAbs(trimmed) {
		return "", false
	}
	return filepath.Clean(trimmed), true
}

// route runs the compact pass and, only when that pass reports its evidence
// insufficient, the bounded expansion. It reports ok=false when the caller
// should keep its original payload.
func route(ctx stdctx.Context, router *contextrouter.Router, log *slog.Logger, req contextrouter.Request) (contextrouter.Selection, bool) {
	selection, err := router.Select(ctx, req)
	if err != nil {
		if log != nil {
			log.Warn("context router: selection failed, sending the unrouted context", "role", req.Role, "project", req.Project.ID, "err", err)
		}
		return contextrouter.Selection{}, false
	}
	if !selection.Expandable {
		return selection, true
	}
	expanded, err := router.Expand(ctx, req, selection)
	if err != nil {
		if log != nil {
			log.Warn("context router: expansion failed, sending the compact selection", "role", req.Role, "project", req.Project.ID, "err", err)
		}
		return selection, true
	}
	return expanded, true
}

func logSelection(log *slog.Logger, msg string, selection contextrouter.Selection) {
	if log == nil {
		return
	}
	log.Debug(msg,
		"role", selection.Role,
		"tier", selection.Tier,
		"sections", len(selection.Sections),
		"dropped", len(selection.Dropped),
		"tokens", selection.EstimatedTokens,
		"limit", selection.Limit,
		"cap", selection.Budget.HardCapTokens,
	)
}
