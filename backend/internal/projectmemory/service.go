package projectmemory

import (
	"context"
	"errors"
	"fmt"
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
	// fact, because a decision outlives the task that made it.
	Decisions []string
	// Risks are follow-ups a later task should know about before touching the
	// same area.
	Risks []string
	// Verification is how the work was checked ("go test ./... green,
	// reviewer approved"), in one line.
	Verification string
	// Commit is the commit the work landed at, when it landed.
	Commit string
	// Integrated says whether the work is part of the repository's integrated
	// state. Only integrated work produces canonical memory; everything else
	// is task-local and reaches nobody but the task itself.
	Integrated bool
}

// MaxRetainedTaskResults bounds how many task outcomes one repository keeps.
//
// Task memory is the one part of project memory that grows with time rather
// than with the repository, so it is the one part that needs an explicit
// retention rule. The oldest outcomes beyond the bound are retired, not
// deleted: an operator can still see that they existed and when they aged out.
const MaxRetainedTaskResults = 200

// RecordTaskOutcome persists what one task did.
//
// Facts from unintegrated work are stored as task-local and are visible only
// to that task (see PackBuilder.filterServable). Promotion to canonical is a
// separate, explicit act — PromoteTaskMemory — performed by whatever authority
// integrated the work. Nothing here promotes on its own, because a task's own
// account of itself is not evidence that it landed.
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

	decisions := decisionItems(base, out, paths)
	risks := riskItems(base, out, paths)
	items := make([]domain.ProjectMemoryItem, 0, 1+len(decisions)+len(risks))
	items = append(items, taskResultItem(base, out, paths))
	items = append(items, decisions...)
	items = append(items, risks...)
	if _, err := putItems(ctx, s.repo, now, items...); err != nil {
		return err
	}

	if err := s.writeTaskRelations(ctx, base, out, paths, now); err != nil {
		return err
	}
	return s.compactTaskMemory(ctx, out.ProjectID, repoID, now)
}

func taskResultItem(base itemBase, out TaskOutcome, paths []string) domain.ProjectMemoryItem {
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
	if len(paths) > 0 {
		fmt.Fprintf(&body, "Files changed: %s\n", strings.Join(paths, ", "))
	}
	if out.Verification != "" {
		fmt.Fprintf(&body, "Verified by: %s\n", out.Verification)
	}
	title := out.Title
	if title == "" {
		title = "task " + out.TaskRef
	}
	return base.item(
		domain.MemoryTypeTaskResult, domain.MemoryScopeTask, out.TaskRef,
		fmt.Sprintf("%s — %s", title, firstLine(out.WhatChanged, "completed")),
		body.String(), paths, "", confidenceVerbatim,
		map[string]string{"task": out.TaskRef, "integrated": fmt.Sprint(out.Integrated)},
	)
}

func decisionItems(base itemBase, out TaskOutcome, paths []string) []domain.ProjectMemoryItem {
	const maxDecisions = 10
	var items []domain.ProjectMemoryItem
	for i, d := range dedupeUnsorted(out.Decisions) {
		if i >= maxDecisions {
			break
		}
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		items = append(items, base.item(
			domain.MemoryTypeDecision, domain.MemoryScopeRepository,
			out.TaskRef+"#"+slug(firstLine(d, fmt.Sprint(i))),
			firstLine(d, "decision"), d, paths, "", confidenceVerbatim,
			map[string]string{"task": out.TaskRef},
		))
	}
	return items
}

func riskItems(base itemBase, out TaskOutcome, paths []string) []domain.ProjectMemoryItem {
	const maxRisks = 10
	var items []domain.ProjectMemoryItem
	for i, r := range dedupeUnsorted(out.Risks) {
		if i >= maxRisks {
			break
		}
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		items = append(items, base.item(
			domain.MemoryTypeKnownRisk, domain.MemoryScopeRepository,
			out.TaskRef+"#"+slug(firstLine(r, fmt.Sprint(i))),
			firstLine(r, "risk"), r, paths, "", confidenceVerbatim,
			map[string]string{"task": out.TaskRef},
		))
	}
	return items
}

// writeTaskRelations records what the task touched, as graph edges: task
// changed file, and decision affects module. They are what lets a later
// Reviewer ask "what else has been done to this area" without reading history.
func (s *Service) writeTaskRelations(ctx context.Context, base itemBase, out TaskOutcome, paths []string, now time.Time) error {
	var rels []domain.ProjectMemoryRelation
	task := domain.ProjectMemoryNode{Kind: domain.NodeTask, Key: out.TaskRef}
	for _, p := range paths {
		rels = append(rels, base.relation(task, domain.RelationChanged,
			domain.ProjectMemoryNode{Kind: domain.NodeFile, Key: p}, []string{p}, confidenceVerbatim))
	}
	for _, m := range dedupeUnsorted(out.Modules) {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		rels = append(rels, base.relation(task, domain.RelationAffects,
			domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: m}, nil, confidenceVerbatim))
	}
	if len(rels) == 0 {
		return nil
	}
	return s.graph.Upsert(ctx, now, rels...)
}

// compactTaskMemory enforces the retention bound.
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
func (s *Service) PromoteTaskMemory(ctx context.Context, projectID domain.ProjectID, taskRef, commit string) (int, error) {
	items, err := s.repo.ListProjectMemoryItemsForTask(ctx, projectID, taskRef)
	if err != nil {
		return 0, err
	}
	now := s.now()
	promoted := 0
	for _, item := range items {
		canonical := item
		canonical.Origin = domain.OriginCanonical
		canonical.OriginRef = ""
		canonical.ID = ""
		if commit != "" {
			canonical.SourceCommit = commit
		}
		if canonical.Metadata == nil {
			canonical.Metadata = map[string]string{}
		}
		canonical.Metadata["integrated"] = "true"
		canonical.Metadata["promotedFromTask"] = taskRef
		canonical = canonical.Normalized()
		if _, err := s.repo.PutProjectMemoryItem(ctx, canonical, now); err != nil {
			return promoted, err
		}
		promoted++
	}
	if promoted > 0 {
		if _, _, err := s.repo.DiscardProjectMemoryForTask(ctx, projectID, taskRef); err != nil {
			return promoted, err
		}
	}
	return promoted, nil
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
