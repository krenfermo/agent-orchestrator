// Package wfmemory attaches project memory to AO's agent dispatch surfaces by
// wrapping them, never by editing them.
//
// It is the third decorator in the same family as
// internal/observe/projectmemory/wfdispatch (which measures a dispatch) and
// internal/contextrouter/wfrouter (which budgets one). The shape is deliberate
// and identical: a decorator that implements the workflow port it wraps can be
// switched off by simply not being installed, and an uninstrumented pipeline
// then runs byte-for-byte the code it ran before. A disabled mode returns every
// dependency unchanged, which is what makes AO_MEMORY_MODE defaulting to off a
// real default rather than a configuration that merely looks disabled.
//
// This is where P2-B's "memory becomes part of the normal cycle" actually
// happens. Four boundaries are instrumented, and between them they cover every
// role P2-A built packs for:
//
//   - Planner — its assembled document set, deduped against the pack.
//   - Spawner — a worker's issue context, and therefore BOTH Repair Agents,
//     which dispatch through the same path (docs/p2-project-memory-audit.md §2.6).
//   - ReviewerLauncher — the standing system prompt P2-A gave a producer.
//   - Verifier — untouched: it carries a command to run, not context.
//
// Every wrapper obeys the same three rules:
//
//  1. **It never fails a dispatch.** Provision returns a degraded answer rather
//     than an error, and a wrapper with nothing to attach passes the original
//     request through unchanged.
//  2. **It never edits an instruction.** Prompts, objectives and acceptance
//     criteria are untouched; memory is added to the context channels beside
//     them.
//  3. **It records what it did.** Every wrapper puts its MemoryMetrics on the
//     context, where the baseline recorder picks it up — so "why did this role
//     receive this context" is answerable after the fact.
package wfmemory

import (
	stdctx "context"
	"log/slog"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// memoryDocumentPath labels the synthetic document a planner receives, so a
// reader of a planner context can tell assembled memory from a file that
// actually exists in the checkout.
const memoryDocumentPath = "ao://project-memory/pack"

// Provisioner is the slice of the memory subsystem these wrappers need.
// Declaring it here rather than importing the concrete type keeps the wrappers
// testable with a fake that has no database behind it.
type Provisioner interface {
	Provision(ctx stdctx.Context, req projectmemory.ProvisionRequest) projectmemory.Provisioned
}

// Instrument wraps the dispatch surfaces memory can contribute to.
//
// A nil provisioner — which is what AO_MEMORY_MODE=off produces — returns deps
// untouched.
func Instrument(deps workflowcore.Deps, prov Provisioner, log *slog.Logger) workflowcore.Deps {
	if prov == nil {
		return deps
	}
	deps.Planner = InstrumentPlanner(deps.Planner, prov, log)
	deps.Spawner = InstrumentSpawner(deps.Spawner, prov, deps.Projects, log)
	deps.ReviewerLauncher = InstrumentReviewerLauncher(deps.ReviewerLauncher, prov, deps.Projects, log)
	return deps
}

// --- planner ---------------------------------------------------------------

type planner struct {
	next workflowcore.Planner
	prov Provisioner
	log  *slog.Logger
}

// describingPlanner preserves the optional PlannerDescriptor capability
// through the wrapper. Without it a wrapped planner would silently lose its
// provider/model identity, and every plan would be recorded as "unknown".
type describingPlanner struct {
	planner
	inner workflowcore.PlannerDescriptor
}

func (d *describingPlanner) Descriptor() (provider, model string) { return d.inner.Descriptor() }

// InstrumentPlanner attaches project memory to the planner's document set.
func InstrumentPlanner(next workflowcore.Planner, prov Provisioner, log *slog.Logger) workflowcore.Planner {
	if next == nil || prov == nil {
		return next
	}
	base := planner{next: next, prov: prov, log: log}
	if desc, ok := next.(workflowcore.PlannerDescriptor); ok {
		return &describingPlanner{planner: base, inner: desc}
	}
	return &base
}

func (p *planner) Generate(ctx stdctx.Context, request workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	ctx, request = p.attach(ctx, request)
	return p.next.Generate(ctx, request)
}

// attach provisions the planner's memory and folds it into the document set.
//
// The planner is the one role whose pack spans every repository of a project,
// so RepoPath is the project root and the pack builder decides how far to
// range. Its documents are also the one legacy source AO can prove equivalence
// for — they carry their own SHA-256 — which is why the planner is where
// dedupe actually pays.
func (p *planner) attach(
	ctx stdctx.Context, request workflowcore.PlannerRequest,
) (stdctx.Context, workflowcore.PlannerRequest) {
	legacy := make([]projectmemory.LegacyDocument, 0, len(request.Context.Documents))
	for _, doc := range request.Context.Documents {
		legacy = append(legacy, projectmemory.LegacyDocument{
			Path: doc.Path, SHA256: doc.SHA256, Content: doc.Content,
		})
	}

	auth := projectmemory.TaskAuthorityFrom(ctx)
	provisioned := p.prov.Provision(ctx, projectmemory.ProvisionRequest{
		ProjectID:        domain.ProjectID(request.Project.ID),
		RepoPath:         request.Project.Path,
		Role:             projectmemory.RolePlanner,
		Keywords:         keywordsFrom(request.Objective),
		Legacy:           legacy,
		TaskBytes:        len(request.Objective),
		TaskRef:          auth.TaskRef,
		WorkflowRunID:    auth.WorkflowRunID,
		UpstreamTaskRefs: auth.UpstreamTaskRefs,
	})
	if !provisioned.Attached() {
		p.note("planner", provisioned)
		return provisioned.WithMetrics(ctx), request
	}

	kept := make([]workflowcore.PlannerDocument, 0, len(provisioned.Legacy)+1)
	surviving := map[string]bool{}
	for _, doc := range provisioned.Legacy {
		surviving[doc.Path] = true
	}
	for _, doc := range request.Context.Documents {
		if surviving[doc.Path] {
			kept = append(kept, doc)
		}
	}
	kept = append(kept, workflowcore.PlannerDocument{
		Path:    memoryDocumentPath,
		SHA256:  provisioned.Pack.Digest,
		Content: provisioned.Render(),
	})
	request.Context.Documents = kept

	p.note("planner", provisioned)
	return provisioned.WithMetrics(ctx), request
}

func (p *planner) note(role string, provisioned projectmemory.Provisioned) {
	logProvision(p.log, role, provisioned)
}

// --- spawner (worker, and therefore both repair agents) --------------------

type spawner struct {
	next     workflowcore.Spawner
	prov     Provisioner
	projects workflowcore.Projects
	log      *slog.Logger
}

// InstrumentSpawner attaches project memory to a worker spawn's issue context.
//
// The spawn config carries a project id but no checkout path, so the project
// resolver supplies the root — the same reason wfrouter's spawner needs one.
// A root that cannot be resolved leaves the spawn on its original context.
func InstrumentSpawner(
	next workflowcore.Spawner, prov Provisioner, projects workflowcore.Projects, log *slog.Logger,
) workflowcore.Spawner {
	if next == nil || prov == nil {
		return next
	}
	return &spawner{next: next, prov: prov, projects: projects, log: log}
}

func (s *spawner) Spawn(ctx stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	ctx, cfg = s.attach(ctx, cfg)
	return s.next.Spawn(ctx, cfg)
}

func (s *spawner) attach(ctx stdctx.Context, cfg ports.SpawnConfig) (stdctx.Context, ports.SpawnConfig) {
	root, ok := s.projectRoot(ctx, cfg.ProjectID)
	if !ok {
		return ctx, cfg
	}

	var legacy []projectmemory.LegacyDocument
	if strings.TrimSpace(cfg.IssueContext) != "" {
		// A pre-fetched tracker body is a synthetic source with no digest, so
		// it is never deduped — it is counted so the metrics report the whole
		// AO-assembled context honestly.
		legacy = append(legacy, projectmemory.LegacyDocument{
			Path: "issue context", Content: cfg.IssueContext,
		})
	}

	// The spawn config names the run; the entitlement on the context names the
	// task and what it may read. The run is taken from the config when the
	// context did not carry one, so a spawn dispatched outside the coordinator
	// still scopes workflow-local knowledge correctly rather than widening it.
	auth := projectmemory.TaskAuthorityFrom(ctx)
	runID := auth.WorkflowRunID
	if runID == "" {
		runID = cfg.WorkflowRunID
	}
	provisioned := s.prov.Provision(ctx, projectmemory.ProvisionRequest{
		ProjectID:        cfg.ProjectID,
		RepoPath:         root,
		Role:             projectmemory.RoleWorker,
		Keywords:         keywordsFrom(cfg.Prompt),
		Legacy:           legacy,
		TaskBytes:        len(cfg.Prompt),
		TaskRef:          auth.TaskRef,
		WorkflowRunID:    runID,
		UpstreamTaskRefs: auth.UpstreamTaskRefs,
	})
	if !provisioned.Attached() {
		logProvision(s.log, "worker", provisioned)
		return provisioned.WithMetrics(ctx), cfg
	}

	var b strings.Builder
	for _, doc := range provisioned.Legacy {
		b.WriteString(doc.Content)
		b.WriteString("\n\n")
	}
	b.WriteString(provisioned.Render())
	cfg.IssueContext = strings.TrimSpace(b.String())

	logProvision(s.log, "worker", provisioned)
	return provisioned.WithMetrics(ctx), cfg
}

func (s *spawner) projectRoot(ctx stdctx.Context, id domain.ProjectID) (string, bool) {
	if s.projects == nil {
		return "", false
	}
	record, found, err := s.projects.GetProject(ctx, string(id))
	if err != nil || !found || strings.TrimSpace(record.Path) == "" {
		return "", false
	}
	return record.Path, true
}

// --- reviewer --------------------------------------------------------------

type reviewerLauncher struct {
	next     workflowcore.ReviewerLauncher
	prov     Provisioner
	projects workflowcore.Projects
	log      *slog.Logger
}

// InstrumentReviewerLauncher attaches project memory to the reviewer's standing
// system prompt.
func InstrumentReviewerLauncher(
	next workflowcore.ReviewerLauncher, prov Provisioner, projects workflowcore.Projects, log *slog.Logger,
) workflowcore.ReviewerLauncher {
	if next == nil || prov == nil {
		return next
	}
	return &reviewerLauncher{next: next, prov: prov, projects: projects, log: log}
}

func (r *reviewerLauncher) Preflight(ctx stdctx.Context, harness domain.ReviewerHarness, workspacePath string) error {
	return r.next.Preflight(ctx, harness, workspacePath)
}

func (r *reviewerLauncher) Launch(ctx stdctx.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	ctx, req = r.attach(ctx, req)
	return r.next.Launch(ctx, req)
}

// The ReviewerEnsurer half passes straight through: it identifies, probes and
// cancels, and none of that assembles context. ReviewerIdentity in particular
// must stay pure and byte-stable across restarts, so nothing that depends on
// what memory happens to contain may enter it.
func (r *reviewerLauncher) ReviewerIdentity(req workflowcore.ReviewerLaunchRequest) string {
	return r.next.ReviewerIdentity(req)
}

func (r *reviewerLauncher) ProbeReviewer(ctx stdctx.Context, ref workflowcore.ReviewerRef) (workflowcore.ReviewerObservation, error) {
	return r.next.ProbeReviewer(ctx, ref)
}

func (r *reviewerLauncher) CancelReviewer(ctx stdctx.Context, ref workflowcore.ReviewerRef) error {
	return r.next.CancelReviewer(ctx, ref)
}

func (r *reviewerLauncher) attach(
	ctx stdctx.Context, req workflowcore.ReviewerLaunchRequest,
) (stdctx.Context, workflowcore.ReviewerLaunchRequest) {
	if strings.TrimSpace(req.SystemPrompt) != "" {
		// Somebody already set standing instructions. Replacing them would be
		// editing an instruction, which these wrappers may not do.
		return ctx, req
	}
	root := strings.TrimSpace(req.WorkspacePath)
	if root == "" {
		var ok bool
		if root, ok = r.projectRoot(ctx, req.ProjectID); !ok {
			return ctx, req
		}
	}

	// A reviewer reviews a change, so the changed area is the relevance
	// evidence that matters. When the launch does not carry one the pack falls
	// back to the repository's standing knowledge, which is still better than
	// the empty system prompt this field held before P2-A.
	auth := projectmemory.TaskAuthorityFrom(ctx)
	provisioned := r.prov.Provision(ctx, projectmemory.ProvisionRequest{
		ProjectID:        req.ProjectID,
		RepoPath:         root,
		Role:             projectmemory.RoleReviewer,
		Keywords:         keywordsFrom(req.Prompt),
		TaskBytes:        len(req.Prompt),
		TaskRef:          auth.TaskRef,
		WorkflowRunID:    auth.WorkflowRunID,
		UpstreamTaskRefs: auth.UpstreamTaskRefs,
	})
	if !provisioned.Attached() {
		logProvision(r.log, "reviewer", provisioned)
		return provisioned.WithMetrics(ctx), req
	}

	req.SystemPrompt = provisioned.Render()
	logProvision(r.log, "reviewer", provisioned)
	return provisioned.WithMetrics(ctx), req
}

func (r *reviewerLauncher) projectRoot(ctx stdctx.Context, id domain.ProjectID) (string, bool) {
	if r.projects == nil {
		return "", false
	}
	record, found, err := r.projects.GetProject(ctx, string(id))
	if err != nil || !found || strings.TrimSpace(record.Path) == "" {
		return "", false
	}
	return record.Path, true
}

// --- shared ----------------------------------------------------------------

func logProvision(log *slog.Logger, role string, provisioned projectmemory.Provisioned) {
	if log == nil {
		return
	}
	log.Debug("project memory: context provisioned", "role", role,
		"summary", provisioned.Metrics.Summary())
}

// keywordsFrom lifts the distinctive words out of an objective, for the
// weakest of the pack's relevance signals.
//
// It is deliberately crude: short words are dropped because a two-letter token
// matches everything, and the result is capped because a long objective would
// otherwise make every fact in the store "relevant" and defeat the ranking it
// was meant to inform. Keywords only ever break ties among facts that are
// already equally relevant.
func keywordsFrom(text string) []string {
	const (
		minWord  = 4
		maxWords = 12
	)
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r != '_' && r != '-' && r != '/' && r != '.' &&
			(r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, maxWords)
	for _, f := range fields {
		f = strings.ToLower(strings.Trim(f, "-_./"))
		if len(f) < minWord {
			continue
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
		if len(out) >= maxWords {
			break
		}
	}
	return out
}
