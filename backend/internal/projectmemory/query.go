package projectmemory

import (
	"context"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// query.go — the inspection surface over shared task knowledge (P2-C §17).
//
// These reads exist so the questions an operator actually asks have one answer
// each, computed the same way the pack builder computes them:
//
//   - What did we learn from this task?
//   - Which decisions still govern, and which did they replace?
//   - Which risks are still open, and who closed the ones that are not?
//   - What did this execution actually know?
//
// They are read-only and they apply the SAME lifecycle rules retrieval does,
// which is the point: an operator who is told a decision is active must be
// looking at the same judgement the Worker's pack made, or the inspection is
// worse than nothing. The only difference is that inspection can be asked for
// the non-current facts too, because reconstructing what the project used to
// believe is exactly what supersession keeps them for.

// KnowledgeFilter narrows a knowledge query.
type KnowledgeFilter struct {
	ProjectID domain.ProjectID
	// RepoPath narrows to one repository. Empty spans the project.
	RepoPath string
	// Types narrows to particular item types. Empty means every
	// shared-knowledge type (see domain.SharedKnowledgeTypes).
	Types []domain.ProjectMemoryType
	// Statuses narrows by lifecycle status. Empty means active only, which is
	// the reading that matches what retrieval would serve.
	Statuses []domain.KnowledgeStatus
	// TaskRef narrows to the knowledge one task produced.
	TaskRef string
	// Limit bounds the result. Zero means DefaultInspectLimit.
	Limit int
}

// KnowledgeEntry is one fact, with its lifecycle rendered rather than left in
// metadata for every caller to decode again.
type KnowledgeEntry struct {
	Item domain.ProjectMemoryItem
	// Status is the lifecycle status the retrieval path would apply.
	Status domain.KnowledgeStatus
	// Kind distinguishes a risk from a follow-up; it is empty for other types.
	Kind domain.KnowledgeKind
	// Share is how far the fact may travel.
	Share domain.KnowledgeShare
	// Subject is the stable identity of what the fact is about.
	Subject string
	// SourceTask is the task that produced it, when one did.
	SourceTask string
	// SupersededBy, Supersedes, ResolvedBy and ConflictsWith are the lifecycle
	// links, empty when they do not apply.
	SupersededBy  string
	Supersedes    string
	ResolvedBy    string
	ConflictsWith string
}

// Knowledge reads shared task knowledge.
//
// The default — no types, no statuses — is "the shared knowledge that is
// currently true", which is what a person asking "what do we know" means. Ask
// for a status explicitly to see what has been retired; nothing is ever gone.
func (s *Service) Knowledge(ctx context.Context, f KnowledgeFilter) ([]KnowledgeEntry, error) {
	items, err := s.knowledgeItems(ctx, f)
	if err != nil {
		return nil, err
	}

	types := map[domain.ProjectMemoryType]struct{}{}
	for _, t := range f.Types {
		types[t] = struct{}{}
	}
	if len(types) == 0 {
		for _, t := range domain.SharedKnowledgeTypes() {
			types[t] = struct{}{}
		}
	}
	statuses := map[domain.KnowledgeStatus]struct{}{}
	for _, st := range f.Statuses {
		statuses[st] = struct{}{}
	}
	if len(statuses) == 0 {
		statuses[domain.KnowledgeActive] = struct{}{}
	}

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultInspectLimit
	}
	task := strings.TrimSpace(f.TaskRef)

	out := make([]KnowledgeEntry, 0, min(len(items), limit))
	for _, item := range items {
		if _, ok := types[item.Key.Type]; !ok {
			continue
		}
		status := domain.KnowledgeStatusOf(item)
		if _, ok := statuses[status]; !ok {
			continue
		}
		if task != "" && domain.KnowledgeTaskOf(item) != task {
			continue
		}
		out = append(out, knowledgeEntryOf(item, status))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// knowledgeItems reads the candidate rows for a filter.
//
// Every state is read, not just valid, because a knowledge query must be able
// to explain a fact that retrieval refuses — "it exists and it is stale" is an
// answer, and a query that could not give it would send an operator to the
// database by hand.
func (s *Service) knowledgeItems(ctx context.Context, f KnowledgeFilter) ([]domain.ProjectMemoryItem, error) {
	if strings.TrimSpace(f.RepoPath) == "" {
		return s.repo.ListProjectMemoryItemsForProject(ctx, f.ProjectID)
	}
	canonical, err := canonicalRepoPath(f.RepoPath)
	if err != nil {
		return nil, err
	}
	return s.repo.ListProjectMemoryItems(ctx, f.ProjectID, domain.ProjectMemoryRepoID(canonical))
}

func knowledgeEntryOf(item domain.ProjectMemoryItem, status domain.KnowledgeStatus) KnowledgeEntry {
	entry := KnowledgeEntry{
		Item:          item,
		Status:        status,
		Share:         domain.KnowledgeShareOf(item),
		Subject:       domain.KnowledgeSubjectOf(item),
		SourceTask:    domain.KnowledgeTaskOf(item),
		SupersededBy:  item.Metadata[domain.MetaKnowledgeSupersededBy],
		Supersedes:    item.Metadata[domain.MetaKnowledgeSupersedes],
		ResolvedBy:    item.Metadata[domain.MetaKnowledgeResolvedBy],
		ConflictsWith: item.Metadata[domain.MetaKnowledgeConflictsWith],
	}
	if item.Key.Type == domain.MemoryTypeKnownRisk {
		entry.Kind = domain.KnowledgeKindOf(item)
	}
	return entry
}

// Decisions reads the project's decisions.
//
// Active by default; ask for KnowledgeSuperseded to walk back through what the
// project used to believe. A superseded decision names its replacement, so the
// chain is followable in both directions without a graph query.
func (s *Service) Decisions(ctx context.Context, f KnowledgeFilter) ([]KnowledgeEntry, error) {
	f.Types = []domain.ProjectMemoryType{domain.MemoryTypeDecision}
	return s.Knowledge(ctx, f)
}

// Risks reads the project's risks and follow-ups.
//
// Open by default. A resolved risk keeps the task ref that closed it, so
// "who fixed this" survives the risk leaving normal retrieval.
func (s *Service) Risks(ctx context.Context, f KnowledgeFilter) ([]KnowledgeEntry, error) {
	f.Types = []domain.ProjectMemoryType{domain.MemoryTypeKnownRisk}
	return s.Knowledge(ctx, f)
}

// TaskKnowledge reads everything one task produced, in every status.
//
// Status is deliberately unfiltered here. "What did we learn from this task"
// includes the decision a later task has since replaced — that is a fact ABOUT
// the task, and hiding it would make the answer wrong rather than tidy.
func (s *Service) TaskKnowledge(ctx context.Context, projectID domain.ProjectID, taskRef string) ([]KnowledgeEntry, error) {
	return s.Knowledge(ctx, KnowledgeFilter{
		ProjectID: projectID,
		TaskRef:   taskRef,
		Types:     domain.SharedKnowledgeTypes(),
		Statuses: []domain.KnowledgeStatus{
			domain.KnowledgeActive, domain.KnowledgeSuperseded, domain.KnowledgeResolved,
			domain.KnowledgeObsolete, domain.KnowledgeConflicting,
		},
	})
}

// --- context manifests ------------------------------------------------------

// RecordContextManifest persists what one execution was told.
//
// It swallows its own errors on purpose. The manifest is an observation of a
// dispatch that has already been assembled; failing the dispatch because the
// observation could not be stored would make an audit aid into a new way to
// lose work, which is the same rule every other memory write on the dispatch
// path follows.
func (s *Service) RecordContextManifest(ctx context.Context, manifest domain.MemoryContextManifest) {
	if err := s.repo.PutProjectMemoryContextManifest(ctx, manifest, s.now()); err != nil && s.log != nil {
		s.log.Warn("project memory: could not record a context manifest",
			"task", manifest.TaskRef, "run", manifest.WorkflowRunID, "err", err)
	}
}

// ContextManifests reads what one execution was told.
//
// A task ref answers "what did this task know"; a run id answers "what did
// this run's executions know". Asking for both narrows to the task, because
// the task is the more specific question.
func (s *Service) ContextManifests(
	ctx context.Context, projectID domain.ProjectID, taskRef, runID string,
) ([]domain.MemoryContextManifest, error) {
	if taskRef = strings.TrimSpace(taskRef); taskRef != "" {
		return s.repo.ListProjectMemoryContextManifestsForTask(ctx, projectID, taskRef)
	}
	return s.repo.ListProjectMemoryContextManifestsForRun(ctx, projectID, strings.TrimSpace(runID))
}

// ManifestItems expands a manifest back into the facts it names.
//
// A fact the manifest names that no longer exists is reported as a MISSING
// entry rather than silently dropped, because "the Worker was told something
// AO has since discarded" is the most interesting thing a manifest can reveal
// and a quietly shorter list would hide it.
func (s *Service) ManifestItems(
	ctx context.Context, manifest domain.MemoryContextManifest,
) ([]KnowledgeEntry, []string, error) {
	entries := make([]KnowledgeEntry, 0, len(manifest.ItemIDs))
	var missing []string
	for _, id := range manifest.ItemIDs {
		item, found, err := s.repo.GetProjectMemoryItem(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			missing = append(missing, id)
			continue
		}
		entries = append(entries, knowledgeEntryOf(item, domain.KnowledgeStatusOf(item)))
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Item.Key.Type < entries[j].Item.Key.Type
	})
	return entries, missing, nil
}
