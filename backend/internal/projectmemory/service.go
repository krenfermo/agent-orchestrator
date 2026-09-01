package projectmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// service.go — the one entry point everything above the package uses.
//
// The CLI, the HTTP controllers, the daemon's wiring and the role integration
// all talk to Service. That is deliberate: it is the single place where the
// policies live that must not be re-decided per caller — which memory a task
// may see, when a task's memory becomes canonical, how much task history is
// kept, and what happens when memory is unavailable.
//
// The last of those is the load-bearing one. **A failure of the memory
// subsystem must never disable AO.** Every read method on Service is written
// so a caller can treat "no memory" and "memory is broken" the same way it
// treats "memory is switched off": ContextPack comes back empty with a stated
// reason rather than as an error, and the caller proceeds exactly as it did
// before P2-A.

// Service is the operational face of project memory.
type Service struct {
	repo     Repository
	indexer  *Indexer
	detector *Detector
	packs    *PackBuilder
	graph    MemoryGraph
	limits   IndexLimits
	now      func() time.Time
	log      *slog.Logger
}

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// WithServiceClock replaces the clock, for deterministic tests.
func WithServiceClock(now func() time.Time) ServiceOption {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithGraph attaches a graph backend. A nil graph, or no option at all, uses
// the local one — edges are canonical state and always have somewhere to go.
func WithGraph(g MemoryGraph) ServiceOption {
	return func(s *Service) {
		if g != nil {
			s.graph = g
		}
	}
}

// WithServiceLogger attaches a logger. Memory is best-effort by design, so
// the failures it swallows have to be visible somewhere; a nil logger simply
// swallows them silently, which is what a test wants.
func WithServiceLogger(log *slog.Logger) ServiceOption {
	return func(s *Service) { s.log = log }
}

// WithIndexerLimits sets the bounds passes run with unless a request overrides
// them.
func WithIndexerLimits(l IndexLimits) ServiceOption {
	return func(s *Service) { s.limits = l.Normalized() }
}

// NewService builds the project-memory service over a durable repository.
//
// Every collaborator is constructed AFTER the options have been applied, so
// the clock and the graph a caller supplied reach all of them. Building them
// inside an option would make the result depend on the order the options were
// written in, which is the kind of bug that only shows up in the one test that
// happens to pass its options the other way round.
func NewService(repo Repository, opts ...ServiceOption) *Service {
	s := &Service{
		repo:   repo,
		now:    func() time.Time { return time.Now().UTC() },
		graph:  NewLocalGraph(repo),
		limits: DefaultIndexLimits(),
	}
	for _, o := range opts {
		o(s)
	}
	s.indexer = NewIndexer(repo, s.graph, WithIndexLimits(s.limits), WithIndexClock(s.now))
	s.detector = NewDetector(repo)
	s.detector.now = s.now
	s.packs = NewPackBuilder(repo).WithPackClock(s.now)
	return s
}

// Graph exposes the wired graph backend, so an operator surface can report
// which one is actually in use.
func (s *Service) Graph() MemoryGraph { return s.graph }

// --- indexing -------------------------------------------------------------

// Index runs one bounded pass over a repository.
func (s *Service) Index(ctx context.Context, req IndexRequest) (IndexOutcome, error) {
	return s.indexer.Index(ctx, req)
}

// Update applies a change set. When the caller cannot enumerate one, Sync
// below derives it from git.
func (s *Service) Update(ctx context.Context, req UpdateRequest) (UpdateOutcome, error) {
	return s.indexer.UpdateChanged(ctx, req)
}

// Sync brings a repository's memory up to a commit by the cheapest route it
// can prove is correct.
//
// It asks git what changed since the commit the last completed pass indexed,
// and applies only that. Whenever it cannot get a trustworthy change set — the
// repository has never been indexed, the previous commit is unreachable after
// a force-push, the checkout is not a git repository — it falls back to a full
// pass and says why in the outcome.
//
// The fallback is not a defeat. Guessing at a change set produces memory with
// holes AO cannot detect, and an undetectable hole in memory is worse than a
// scan.
func (s *Service) Sync(ctx context.Context, projectID domain.ProjectID, repoPath, commit, branch string) (UpdateOutcome, error) {
	canonical, err := canonicalRepoPath(repoPath)
	if err != nil {
		return UpdateOutcome{}, err
	}
	repoID := domain.ProjectMemoryRepoID(canonical)
	state, found, err := s.repo.GetProjectMemoryIndexState(ctx, projectID, repoID)
	if err != nil {
		return UpdateOutcome{}, err
	}

	if found && state.IndexedCommit != "" && commit != "" {
		if state.IndexedCommit == commit && state.Phase == domain.IndexPhaseIdle {
			// Already at this commit. Nothing to do, and saying so costs one
			// row read — this is the cheap path a per-task hook wants.
			return UpdateOutcome{
				RepoID: repoID, Generation: state.Generation,
				IndexedCommit: state.IndexedCommit,
				Skipped:       true, SkipReason: "memory is already at this commit",
			}, nil
		}
		changes, cerr := ChangesSinceCommit(ctx, canonical, state.IndexedCommit, commit)
		if cerr == nil {
			return s.indexer.UpdateChanged(ctx, UpdateRequest{
				ProjectID: projectID, RepoPath: canonical,
				ToCommit: commit, Branch: branch, Changes: changes,
			})
		}
	}

	full, err := s.indexer.Index(ctx, IndexRequest{
		ProjectID: projectID, RepoPath: canonical, Commit: commit, Branch: branch,
	})
	out := UpdateOutcome{
		RepoID: full.RepoID, Generation: full.Generation,
		Skipped: full.Skipped, SkipReason: full.SkipReason,
		FellBackToFullIndex: true,
		FallbackReason:      "no trustworthy change set was available since the last indexed commit",
		ItemsWritten:        full.ItemsWritten,
		ItemsReconfirmed:    full.ItemsReconfirmed,
		ItemsInvalidated:    full.ItemsInvalidated,
		RelationsWritten:    full.RelationsWritten,
		IndexedCommit:       full.IndexedCommit,
		Duration:            full.Duration,
	}
	return out, err
}

// Rebuild re-derives a repository's memory from scratch.
//
// With purge false it forces re-derivation of every admitted file while
// keeping the existing rows, so identities and creation times survive. With
// purge true it deletes first, which is the operator's escape hatch for memory
// that is wrong in a way a re-derivation cannot fix — a schema change, a
// corrupted import.
func (s *Service) Rebuild(ctx context.Context, projectID domain.ProjectID, repoPath, commit, branch string, purge bool) (IndexOutcome, error) {
	canonical, err := canonicalRepoPath(repoPath)
	if err != nil {
		return IndexOutcome{}, err
	}
	if purge {
		repoID := domain.ProjectMemoryRepoID(canonical)
		if err := s.repo.PurgeProjectMemoryRepo(ctx, projectID, repoID); err != nil {
			return IndexOutcome{}, err
		}
	}
	return s.indexer.Index(ctx, IndexRequest{
		ProjectID: projectID, RepoPath: canonical, Commit: commit, Branch: branch, Force: true,
	})
}

// Verify runs drift detection. With apply false it is a dry run.
func (s *Service) Verify(ctx context.Context, projectID domain.ProjectID, repoPath, commit string, apply bool) (DriftReport, error) {
	return s.detector.Check(ctx, DriftRequest{
		ProjectID: projectID, RepoPath: repoPath, Commit: commit, Apply: apply,
	})
}

// Invalidate marks everything derived from the named paths as no longer
// authoritative.
func (s *Service) Invalidate(ctx context.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string) (int64, error) {
	return s.detector.InvalidatePaths(ctx, projectID, repoPath, paths, reason)
}

// --- reading --------------------------------------------------------------

// Status reports one repository's memory.
func (s *Service) Status(ctx context.Context, projectID domain.ProjectID, repoPath string) (domain.ProjectMemoryStatus, bool, error) {
	canonical, err := canonicalRepoPath(repoPath)
	if err != nil {
		return domain.ProjectMemoryStatus{}, false, err
	}
	return s.repo.GetProjectMemoryStatus(ctx, projectID, domain.ProjectMemoryRepoID(canonical))
}

// StatusAll reports every repository registered under one project.
func (s *Service) StatusAll(ctx context.Context, projectID domain.ProjectID) ([]domain.ProjectMemoryStatus, error) {
	states, err := s.repo.ListProjectMemoryIndexStates(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProjectMemoryStatus, 0, len(states))
	for _, st := range states {
		status, ok, err := s.repo.GetProjectMemoryStatus(ctx, projectID, st.RepoID)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, status)
		}
	}
	return out, nil
}

// InspectRequest narrows an operator's read of the stored facts.
type InspectRequest struct {
	ProjectID domain.ProjectID
	RepoPath  string
	// State narrows to one state. Empty means every state, which is what an
	// operator asking "what went stale" needs.
	State domain.ProjectMemoryState
	// Type narrows to one item type.
	Type domain.ProjectMemoryType
	// PathPrefix narrows to facts about a subtree.
	PathPrefix string
	// Limit caps the result. Zero means DefaultInspectLimit.
	Limit int
}

// DefaultInspectLimit bounds an inspect read.
const DefaultInspectLimit = 200

// InspectResult is what an inspect read returned, and what it left out.
type InspectResult struct {
	RepoID    string
	Items     []domain.ProjectMemoryItem
	Total     int
	Truncated bool
}

// Inspect reads stored facts for an operator. Unlike a context pack it does
// not filter by authority: seeing the stale rows is the point.
func (s *Service) Inspect(ctx context.Context, req InspectRequest) (InspectResult, error) {
	canonical, err := canonicalRepoPath(req.RepoPath)
	if err != nil {
		return InspectResult{}, err
	}
	repoID := domain.ProjectMemoryRepoID(canonical)
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultInspectLimit
	}

	var items []domain.ProjectMemoryItem
	if req.State != "" {
		items, err = s.repo.ListProjectMemoryItemsByState(ctx, req.ProjectID, repoID, req.State)
	} else {
		items, err = s.repo.ListProjectMemoryItems(ctx, req.ProjectID, repoID)
	}
	if err != nil {
		return InspectResult{}, err
	}

	filtered := items[:0]
	for _, item := range items {
		if req.Type != "" && item.Key.Type != req.Type {
			continue
		}
		if req.PathPrefix != "" && !matchesPrefix(item, req.PathPrefix) {
			continue
		}
		filtered = append(filtered, item)
	}
	result := InspectResult{RepoID: repoID, Total: len(filtered)}
	if len(filtered) > limit {
		result.Truncated = true
		filtered = filtered[:limit]
	}
	result.Items = filtered
	return result, nil
}

func matchesPrefix(item domain.ProjectMemoryItem, prefix string) bool {
	prefix = strings.TrimPrefix(strings.TrimSpace(prefix), "./")
	if strings.HasPrefix(item.Key.Key, prefix) {
		return true
	}
	for _, p := range item.SourcePaths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// Context assembles a role-scoped memory pack.
//
// It never returns an error the caller has to handle as a failure of its own
// work: a storage problem produces an empty pack with a stated fallback
// reason, exactly as an unindexed repository does. That is what makes the role
// integrations fallback-safe by construction rather than by each caller
// remembering to be careful.
func (s *Service) Context(ctx context.Context, req PackRequest) ContextPack {
	pack, err := s.packs.Build(ctx, req)
	if err != nil {
		role := req.Role
		if !role.Valid() {
			role = RoleWorker
		}
		degraded := ContextPack{
			Role: role, ProjectID: req.ProjectID, BuiltAt: s.now(),
			Stats: PackStats{
				Role:           role,
				FallbackReason: fmt.Sprintf("project memory could not be read (%v)", err),
			},
		}
		degraded.Digest = digestOf(degraded.Render())
		return degraded
	}
	return pack
}

// --- task and decision memory ---------------------------------------------

// TaskOutcome is what AO records when a piece of work finishes.
//
// It is a summary by construction. There is no field for a transcript, a
// session log or a diff body, and there is nowhere to put one: what a later
// task needs is what changed, why, how it verified and what is still risky.
// Everything else is already durable in AO's workflow rows, and a second copy
// of it in memory would be both redundant and unbounded.
type TaskOutcome struct {
	ProjectID domain.ProjectID
	// RepoPath is the repository the work happened in.
	RepoPath string
	// TaskRef identifies the task. It is the origin ref of every fact this
	// outcome produces.
	TaskRef string
	// WorkflowRunID is the run the task belonged to, when there was one. It is
	// what scopes workflow-local sharing, and it is empty for a task that ran
	// outside a workflow.
	WorkflowRunID string
	// Title is a short name for the work.
	Title string
	// WhatChanged and Why are the two sentences a later task actually needs.
	WhatChanged string
	Why         string
	// FilesChanged and Modules are what the work touched, and become the
	// relevance anchors of the resulting fact.
	FilesChanged []string
	Modules      []string
	// Decisions are choices later work must respect. Each becomes its own
	// fact, keyed by what it is ABOUT rather than by the task that made it, so
	// re-deciding a topic supersedes the previous answer instead of piling a
	// second one beside it.
	Decisions []TaskDecision
	// Risks are risks and follow-ups a later task should know about before
	// touching the same area.
	Risks []TaskRisk
	// ResolvesRisks names risks this task closed, by item id or by subject.
	// The named risks become resolved and stop being carried as current, while
	// the edge saying who closed them survives.
	ResolvesRisks []string
	// DependsOnTasks and FollowsUpTasks are the durable task relationships the
	// plan already recorded. They are copied, never inferred: two tasks that
	// merely touched the same repository are not related.
	DependsOnTasks []string
	FollowsUpTasks []string
	// Verification is how the work was checked ("go test ./... green,
	// reviewer approved"), in one line.
	Verification string
	// Commit is the commit the work landed at, when it landed.
	Commit string
	// Integrated says whether the work is part of the repository's integrated
	// state. Only integrated work produces canonical memory; everything else
	// is task-local and reaches nobody but the task itself.
	Integrated bool
	// Share is how far this outcome's facts may travel when the work is NOT
	// integrated. It is decided by the workflow boundary that knows the
	// placement and the verification state, never here: ShareWorkflow means a
	// verified task whose downstream dependents may read it, and ShareTask —
	// the default — means nobody but the task itself. Integrated work is
	// always canonical regardless of this field.
	Share domain.KnowledgeShare
}

// MaxRetainedTaskResults bounds how many task outcomes one repository keeps.
//
// Task memory is the one part of project memory that grows with time rather
// than with the repository, so it is the one part that needs an explicit
// retention rule. The oldest outcomes beyond the bound are retired, not
// deleted: an operator can still see that they existed and when they aged out.
// P2-C adds a second, per-scope bound on top of it (see compactKnowledge),
// because a global cap alone lets one busy module evict every other module's
// history.
const MaxRetainedTaskResults = 200

// RecordTaskOutcome persists what one task did, and applies the knowledge
// lifecycle that makes it reusable.
//
// Facts from unintegrated work are stored as task-local and reach only the
// readers their sharing scope admits (see PackBuilder.filterServable).
// Promotion to canonical is a separate, explicit act — PromoteTaskMemory —
// performed by whatever authority integrated the work. Nothing here promotes
// on its own, because a task's own account of itself is not evidence that it
// landed.
//
// It is idempotent. Every write addresses a derived identity, so recording the
// same outcome twice — which is exactly what a duplicate completion callback
// or a restart between the outcome and the promotion produces — updates the
// same rows rather than creating a second set.
func (s *Service) RecordTaskOutcome(ctx context.Context, out TaskOutcome) error {
	if strings.TrimSpace(out.TaskRef) == "" {
		return errors.New("projectmemory: a task outcome must name its task")
	}
	canonical, err := canonicalRepoPath(out.RepoPath)
	if err != nil {
		return err
	}
	repoID := domain.ProjectMemoryRepoID(canonical)
	now := s.now()
	if err := s.repo.EnsureProjectMemoryRepo(ctx, out.ProjectID, repoID, canonical, now); err != nil {
		return err
	}

	origin := domain.OriginTaskLocal
	originRef := out.TaskRef
	if out.Integrated {
		origin = domain.OriginCanonical
		originRef = ""
	}
	base := itemBase{
		ProjectID: out.ProjectID, RepoID: repoID, Commit: out.Commit,
		Origin: origin, OriginRef: originRef,
		// Task memory carries generation 0. It is not produced by an indexing
		// pass, so fencing it against one would be meaningless — and a pass
		// must never be able to retire it, which the retire sweep already
		// guarantees by ignoring task-local rows and by only sweeping facts a
		// full walk was responsible for.
		Generation: 0,
	}
	paths := domain.NormalizeMemorySourcePaths(out.FilesChanged)
	if len(paths) > domain.MaxProjectMemorySourcePaths {
		paths = paths[:domain.MaxProjectMemorySourcePaths]
	}

	// The snapshot every lifecycle decision below is made against. Reading it
	// once, before anything is written, is what stops two facts in one outcome
	// from both believing they retired the same predecessor.
	existing, err := s.repo.ListProjectMemoryItems(ctx, out.ProjectID, repoID)
	if err != nil {
		return err
	}
	w := &knowledgeWriter{svc: s, base: base, out: out, now: now, paths: paths, existing: existing}

	decisions := w.decisionItems()
	risks := w.riskItems()
	items := make([]domain.ProjectMemoryItem, 0, 1+len(decisions)+len(risks))
	items = append(items, w.taskResultItem())
	items = append(items, decisions...)
	items = append(items, risks...)
	// Normalise BEFORE anything reads an id off these. Supersession compares
	// identities and the lineage edges name them, and a derived id that has
	// not been filled in yet is the empty string — which makes every fact
	// look like every other fact and makes an outcome supersede itself.
	for i := range items {
		items[i] = items[i].Normalized()
	}
	if _, err := putItems(ctx, s.repo, now, items...); err != nil {
		return err
	}

	rels := w.lineage(items)
	superseded, err := w.supersede(ctx, items)
	if err != nil {
		return err
	}
	rels = append(rels, superseded...)
	resolved, err := w.resolveRisks(ctx)
	if err != nil {
		return err
	}
	rels = append(rels, resolved...)

	if len(rels) > 0 {
		// A graph the daemon cannot reach must not fail a recording: the items
		// above are the durable knowledge, and the edges are an index over
		// them that a later pass can rebuild.
		if err := s.graph.Upsert(ctx, now, rels...); err != nil && s.log != nil {
			s.log.Warn("project memory: could not mirror a task's knowledge into the graph",
				"task", out.TaskRef, "err", err)
		}
	}

	if err := s.compactTaskMemory(ctx, out.ProjectID, repoID, now); err != nil {
		return err
	}
	return s.compactKnowledge(ctx, out.ProjectID, repoID, now)
}

// taskResultItem renders one outcome as the bounded fact a later task reads.
//
// Every line is a field the caller supplied from a durable row. The "Verified
// by" line is the verification fact P2-C names as its own knowledge type; it
// lives on the task result rather than in a row of its own because it has no
// life independent of the outcome it verified.
func (w *knowledgeWriter) taskResultItem() domain.ProjectMemoryItem {
	out := w.out
	var body strings.Builder
	if out.WhatChanged != "" {
		fmt.Fprintf(&body, "What changed: %s\n", out.WhatChanged)
	}
	if out.Why != "" {
		fmt.Fprintf(&body, "Why: %s\n", out.Why)
	}
	if len(out.Modules) > 0 {
		fmt.Fprintf(&body, "Modules affected: %s\n", strings.Join(dedupeUnsorted(out.Modules), ", "))
	}
	if len(w.paths) > 0 {
		fmt.Fprintf(&body, "Files changed: %s\n", strings.Join(w.paths, ", "))
	}
	if out.Verification != "" {
		fmt.Fprintf(&body, "Verified by: %s\n", out.Verification)
	}
	title := out.Title
	if title == "" {
		title = "task " + out.TaskRef
	}
	return w.base.item(
		domain.MemoryTypeTaskResult, domain.MemoryScopeTask, out.TaskRef,
		fmt.Sprintf("%s — %s", title, firstLine(out.WhatChanged, "completed")),
		body.String(), w.paths, "", confidenceVerbatim,
		w.meta(out.TaskRef, map[string]string{
			domain.MetaKnowledgeIntegrated: fmt.Sprint(out.Integrated),
			// The retention bucket, recorded rather than re-derived from the
			// rendered body (see compactionScopeOf).
			metaPrimaryModule: primaryModuleOf(out),
		}),
	)
}

// compactTaskMemory enforces the global retention bound.
//
// It retires the oldest task-result facts beyond MaxRetainedTaskResults rather
// than deleting them, so "this repository's memory of task X aged out" stays
// readable. Without this, task memory is the one part of the system that grows
// without limit — which is exactly what the brief forbids.
func (s *Service) compactTaskMemory(ctx context.Context, projectID domain.ProjectID, repoID string, now time.Time) error {
	items, err := s.repo.ListProjectMemoryItemsByState(ctx, projectID, repoID, domain.MemoryStateValid)
	if err != nil {
		return err
	}
	results := make([]domain.ProjectMemoryItem, 0, len(items))
	for _, it := range items {
		if it.Key.Type == domain.MemoryTypeTaskResult {
			results = append(results, it)
		}
	}
	if len(results) <= MaxRetainedTaskResults {
		return nil
	}
	sort.Slice(results, func(i, j int) bool {
		if !results[i].UpdatedAt.Equal(results[j].UpdatedAt) {
			return results[i].UpdatedAt.After(results[j].UpdatedAt)
		}
		return results[i].ID < results[j].ID
	})
	for _, old := range results[MaxRetainedTaskResults:] {
		if _, err := s.repo.MarkProjectMemoryItemState(ctx, old.ID, old.Generation,
			domain.MemoryStateInvalidated,
			fmt.Sprintf("compacted: beyond the %d most recent task outcomes", MaxRetainedTaskResults),
			now); err != nil {
			return err
		}
	}
	return nil
}

// PromoteTaskMemory turns one task's unintegrated facts into canonical project
// memory.
//
// It is called by whatever authority integrated the work, and by nothing else.
// The separation matters: a task saying "I decided X" is a claim about a
// branch, and it becomes a fact about the project only when that branch is
// part of the project. Promotion re-keys each fact as canonical, which gives
// it a new identity — the same content under a different origin is a different
// row, and the task-local original is discarded.
//
// Promotion is also where a branch's DISAGREEMENT with the project is
// resolved. A decision recorded on an unintegrated branch that contradicted a
// canonical one was marked conflicting rather than allowed to supersede it
// (see knowledgeWriter.supersede); once the branch is integrated the
// contradiction has an answer, and this is the moment the promoted decision
// finally retires the one it replaced.
//
// It is idempotent and restart-safe. A crash between the promotion write and
// the discard leaves the canonical row present and the task-local original
// still there, and a second call promotes the same content to the same derived
// identity before discarding again — so a duplicate completion callback
// produces exactly one canonical fact, never two.
func (s *Service) PromoteTaskMemory(ctx context.Context, projectID domain.ProjectID, taskRef, commit string) (int, error) {
	items, err := s.repo.ListProjectMemoryItemsForTask(ctx, projectID, taskRef)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	now := s.now()
	repoID := items[0].Key.RepoID
	existing, err := s.repo.ListProjectMemoryItems(ctx, projectID, repoID)
	if err != nil {
		return 0, err
	}

	promoted := make([]domain.ProjectMemoryItem, 0, len(items))
	for _, item := range items {
		canonical := item
		canonical.Origin = domain.OriginCanonical
		canonical.OriginRef = ""
		canonical.ID = ""
		if commit != "" {
			canonical.SourceCommit = commit
		}
		canonical = domain.WithKnowledgeMetadata(canonical, domain.MetaKnowledgeIntegrated, "true")
		canonical = domain.WithKnowledgeMetadata(canonical, "promotedFromTask", taskRef)
		canonical = domain.WithKnowledgeMetadata(canonical, domain.MetaKnowledgeShare, string(domain.ShareCanonical))
		// A branch decision that was held as conflicting becomes the project's
		// answer the moment the branch lands. The conflict annotation is
		// cleared here and settled by the supersession pass below.
		if domain.KnowledgeStatusOf(canonical) == domain.KnowledgeConflicting {
			canonical = domain.WithKnowledgeMetadata(canonical, domain.MetaKnowledgeStatus, string(domain.KnowledgeActive))
			canonical = domain.WithKnowledgeMetadata(canonical, domain.MetaKnowledgeConflictsWith, "")
		}
		canonical = canonical.Normalized()
		if _, err := s.repo.PutProjectMemoryItem(ctx, canonical, now); err != nil {
			return len(promoted), err
		}
		promoted = append(promoted, canonical)
	}

	// The promoted facts now carry the project's own authority, so the
	// supersession they were refused on the branch is applied here.
	w := &knowledgeWriter{
		svc: s, now: now, existing: existing,
		base: itemBase{
			ProjectID: projectID, RepoID: repoID, Commit: commit,
			Origin: domain.OriginCanonical,
		},
		out: TaskOutcome{ProjectID: projectID, TaskRef: taskRef, Integrated: true},
	}
	rels, err := w.supersede(ctx, promoted)
	if err != nil {
		return len(promoted), err
	}
	for _, item := range promoted {
		rels = append(rels, w.base.relation(
			domain.ProjectMemoryNode{Kind: domain.NodeTask, Key: taskRef},
			domain.RelationProduced, knowledgeNode(item.ID), item.SourcePaths, confidenceVerbatim))
	}
	if len(rels) > 0 {
		if err := s.graph.Upsert(ctx, now, rels...); err != nil && s.log != nil {
			s.log.Warn("project memory: could not mirror a promotion into the graph",
				"task", taskRef, "err", err)
		}
	}

	if _, _, err := s.repo.DiscardProjectMemoryForTask(ctx, projectID, taskRef); err != nil {
		return len(promoted), err
	}
	return len(promoted), s.compactKnowledge(ctx, projectID, repoID, now)
}

// DiscardTaskMemory drops one task's unintegrated facts.
//
// It is what stops an isolated worktree from leaving a permanent parallel
// memory behind: a task that ends without integrating takes its own view with
// it.
func (s *Service) DiscardTaskMemory(ctx context.Context, projectID domain.ProjectID, taskRef string) (int64, error) {
	items, _, err := s.repo.DiscardProjectMemoryForTask(ctx, projectID, taskRef)
	return items, err
}

func firstLine(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if idx := strings.IndexByte(s, '\n'); idx > 0 {
		s = s[:idx]
	}
	return clampSummary(s)
}

// dedupeUnsorted removes duplicates while preserving the caller's order, which
// for a decision list is meaningful: the order they were made in.
func dedupeUnsorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
