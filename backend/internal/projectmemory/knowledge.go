package projectmemory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// knowledge.go — the shared-task-knowledge lifecycle (P2-C).
//
// P2-A stored a task's outcome. P2-B called it from the lifecycle. What this
// file adds is the part that makes the result usable by a LATER task rather
// than merely durable:
//
//   - **Supersession.** A decision is never overwritten and never deleted. A
//     later decision on the same subject retires the earlier one and names it,
//     so current context carries one answer while an audit can still walk back
//     through every answer the project used to have.
//   - **Resolution.** A risk or follow-up a later task closes becomes resolved
//     and stops being carried as current, while the edge saying who closed it
//     survives.
//   - **Conflict.** When two facts assert incompatible things and AO cannot
//     prove which came later or which has more authority, it marks BOTH rather
//     than picking one. An unresolved contradiction is information about the
//     memory, not about the project, and it reaches a Planner as a stated
//     conflict and nobody else.
//   - **Scoped compaction.** Task knowledge is the only part of memory that
//     grows with time rather than with the repository, so it is the only part
//     with an explicit per-scope retention rule.
//
// Two rules run through all of it.
//
// **Authority is asymmetric.** A branch that has not been integrated may not
// retire the project's knowledge. A task-local or workflow-local decision that
// disagrees with a canonical one is recorded as a conflict, not as a
// replacement, and becomes a replacement only when an integration authority
// promotes it. This is the worktree rule applied to supersession, and it is
// what stops one unmerged opinion from silently rewriting the project's.
//
// **Nothing is invented.** Every fact written here comes from a structured
// field a caller supplied from a durable row. There is no summariser, no model
// call and no inference from prose; a task that recorded no decisions produces
// no decisions rather than guessed ones.

// TaskDecision is one choice a task made that later work must respect.
type TaskDecision struct {
	// Statement is the decision itself, in one or two sentences.
	Statement string
	// Rationale is bounded prose saying why. It is optional and is never
	// required to be present for the decision to be usable.
	Rationale string
	// Topic names WHAT the decision is about, independently of how it was
	// worded — "api-protocol", "storage-engine", "auth-scheme".
	//
	// It is the field supersession actually turns on, and it is caller-supplied
	// on purpose. AO can prove two statements are the same text; it cannot
	// prove two different texts mean the same thing. A decision with no topic
	// is about itself and can only ever be superseded by an explicit
	// Supersedes, which is the conservative reading: silence must never be
	// read as "replaces the last thing anyone said".
	Topic string
	// Scope and ScopeKey say how wide the decision's claim is. Empty defaults
	// to the repository.
	Scope    domain.ProjectMemoryScope
	ScopeKey string
	// Supersedes explicitly names the subject or the item id of a decision
	// this one replaces. It is how a re-worded decision retires its
	// predecessor when the topic alone cannot connect them.
	Supersedes string
	// Evidence are repo-relative paths that demonstrate the decision. They are
	// the decision's provenance and its relevance anchor.
	Evidence []string
}

// TaskRisk is a risk or a follow-up a later task should know about.
type TaskRisk struct {
	// Statement is the risk or the outstanding work, in one or two sentences.
	Statement string
	// Kind distinguishes a risk from a deliberately deferred piece of work.
	// Empty means a risk.
	Kind domain.KnowledgeKind
	// Topic names what the risk is about, and plays the same role in
	// resolution that a decision's topic plays in supersession.
	Topic string
	// Scope and ScopeKey say how wide it is.
	Scope    domain.ProjectMemoryScope
	ScopeKey string
	// Modules are the modules the risk concerns, for the graph edge.
	Modules []string
	// Evidence are repo-relative paths.
	Evidence []string
}

// Bounds on what one task outcome may contribute.
//
// They exist for the same reason every other bound in this package exists: a
// task that wants to record fifty decisions has not made fifty decisions, and
// an unbounded contribution would make one task able to fill every later
// task's context by itself.
const (
	// MaxTaskDecisions caps decisions from one outcome.
	MaxTaskDecisions = 10
	// MaxTaskRisks caps risks and follow-ups from one outcome.
	MaxTaskRisks = 10
	// MaxDecisionRationale caps a decision's prose.
	MaxDecisionRationale = 1024
)

// Retention bounds, per scope (P2-C §12).
//
// They are per-scope rather than global because the failure they prevent is
// per-scope: twenty small tasks in one module must not crowd out every other
// module's knowledge, and a global cap would let exactly that happen.
const (
	// MaxTaskResultsPerModule caps how many task outcomes stay active for one
	// module. Older ones are retired, not deleted.
	MaxTaskResultsPerModule = 8
	// MaxActiveDecisions caps active decisions per repository.
	MaxActiveDecisions = 60
	// MaxOpenRisks caps open risks and follow-ups per repository.
	MaxOpenRisks = 60
)

// knowledgeWriter applies one task outcome's knowledge lifecycle.
//
// It is a struct rather than a pile of Service methods so the whole write is
// one object with one clock and one repo id, and so the ordering below — read
// the existing knowledge ONCE, then decide everything against that snapshot —
// is structural rather than remembered. Deciding supersession against a
// snapshot means two facts in the same outcome cannot both think they retired
// the same predecessor.
type knowledgeWriter struct {
	svc      *Service
	base     itemBase
	out      TaskOutcome
	now      time.Time
	paths    []string
	existing []domain.ProjectMemoryItem
}

// share is the sharing scope every fact from this outcome carries.
func (w *knowledgeWriter) share() domain.KnowledgeShare {
	if w.out.Integrated {
		return domain.ShareCanonical
	}
	if s := w.out.Share; s.Valid() {
		return s
	}
	return domain.ShareTask
}

// meta is the lifecycle annotation every fact from this outcome starts with.
func (w *knowledgeWriter) meta(subject string, extra map[string]string) map[string]string {
	m := map[string]string{
		domain.MetaKnowledgeTask:    w.out.TaskRef,
		domain.MetaKnowledgeStatus:  string(domain.KnowledgeActive),
		domain.MetaKnowledgeShare:   string(w.share()),
		domain.MetaKnowledgeSubject: subject,
	}
	if run := strings.TrimSpace(w.out.WorkflowRunID); run != "" {
		m[domain.MetaKnowledgeRun] = run
	}
	for k, v := range extra {
		if strings.TrimSpace(v) != "" {
			m[k] = v
		}
	}
	return m
}

// decisionItems turns the outcome's decisions into durable facts.
//
// The item key is the SUBJECT rather than the task ref, which is the change
// that makes a decision outlive the task that made it: re-deciding the same
// topic addresses the same canonical row instead of accumulating one row per
// task. The task that made it is still recorded — in metadata, where it
// belongs — so provenance survives without identity depending on it.
func (w *knowledgeWriter) decisionItems() []domain.ProjectMemoryItem {
	items := make([]domain.ProjectMemoryItem, 0, len(w.out.Decisions))
	seen := map[string]struct{}{}
	for i, d := range w.out.Decisions {
		if i >= MaxTaskDecisions {
			break
		}
		statement := strings.TrimSpace(d.Statement)
		if statement == "" {
			continue
		}
		scope, scopeKey := normalizeKnowledgeScope(d.Scope, d.ScopeKey)
		subject := decisionSubject(scope, scopeKey, d)
		if _, dup := seen[subject]; dup {
			// Two statements about one topic in one outcome are one decision
			// stated twice, and the first wins. Letting both through would
			// make the outcome supersede itself.
			continue
		}
		seen[subject] = struct{}{}

		body := statement
		if rationale := strings.TrimSpace(d.Rationale); rationale != "" {
			body += "\n\nWhy: " + excerpt(rationale, MaxDecisionRationale)
		}
		evidence := knowledgeEvidence(d.Evidence, w.paths)
		extra := map[string]string{}
		if topic := strings.TrimSpace(d.Topic); topic != "" {
			extra["topic"] = topic
		}
		// The row key is the subject AND the statement, not the subject alone.
		//
		// That distinction is what makes supersession possible at all. Keyed by
		// subject alone, re-deciding a topic would address the SAME row and
		// overwrite its predecessor in place — the project would end up with
		// one decision and no history, which is precisely what P2-C §8
		// forbids. Keyed by both, a re-worded decision is a new row that
		// retires the old one by subject, while re-recording the IDENTICAL
		// decision addresses the same row and stays idempotent.
		items = append(items, w.base.item(
			domain.MemoryTypeDecision, scope, subject+"#"+statementFingerprint(statement),
			firstLine(statement, "decision"), body,
			evidence, "", confidenceVerbatim, w.meta(subject, extra),
		))
	}
	return items
}

// riskItems turns the outcome's risks and follow-ups into durable facts.
func (w *knowledgeWriter) riskItems() []domain.ProjectMemoryItem {
	items := make([]domain.ProjectMemoryItem, 0, len(w.out.Risks))
	seen := map[string]struct{}{}
	for i, r := range w.out.Risks {
		if i >= MaxTaskRisks {
			break
		}
		statement := strings.TrimSpace(r.Statement)
		if statement == "" {
			continue
		}
		scope, scopeKey := normalizeKnowledgeScope(r.Scope, r.ScopeKey)
		subject := riskSubject(scope, scopeKey, r)
		if _, dup := seen[subject]; dup {
			continue
		}
		seen[subject] = struct{}{}

		kind := r.Kind
		if kind == "" {
			kind = domain.KnowledgeKindRisk
		}
		extra := map[string]string{domain.MetaKnowledgeKind: string(kind)}
		if topic := strings.TrimSpace(r.Topic); topic != "" {
			extra["topic"] = topic
		}
		items = append(items, w.base.item(
			domain.MemoryTypeKnownRisk, scope, subject,
			firstLine(statement, string(kind)), statement,
			knowledgeEvidence(r.Evidence, w.paths), "", confidenceVerbatim,
			w.meta(subject, extra),
		))
	}
	return items
}

// supersede retires the predecessors of everything this outcome just wrote,
// and records the conflicts it could not order.
//
// It runs AFTER the new facts are written, against the snapshot read before
// them, so a crash between the two leaves the new fact present and the old one
// still active — duplicated, never lost — and the next pass over the same
// outcome retires it. That ordering is the whole of the restart-safety story
// here: the failure mode is a redundant fact, not a missing one.
func (w *knowledgeWriter) supersede(
	ctx context.Context, written []domain.ProjectMemoryItem,
) ([]domain.ProjectMemoryRelation, error) {
	var rels []domain.ProjectMemoryRelation
	incomingShare := w.share()
	for _, fresh := range written {
		if fresh.Key.Type != domain.MemoryTypeDecision {
			continue
		}
		for _, old := range w.predecessorsOf(fresh) {
			switch {
			case incomingShare == domain.ShareCanonical ||
				domain.KnowledgeShareOf(old) != domain.ShareCanonical:
				// The incoming decision is the project's own, or the one it
				// replaces was never the project's. Ordinary supersession.
				if err := w.retire(ctx, old, domain.KnowledgeSuperseded,
					domain.MetaKnowledgeSupersededBy, fresh.ID); err != nil {
					return rels, err
				}
				rels = append(rels, w.base.relation(
					knowledgeNode(fresh.ID), domain.RelationSupersedes, knowledgeNode(old.ID),
					nil, confidenceVerbatim))
			default:
				// An unintegrated branch disagreeing with the project's own
				// decision. It may not retire it, and AO may not silently
				// prefer either: the branch has not landed, and the project's
				// decision has not been withdrawn. Both are marked, and the
				// Planner is the only role that will be told.
				if err := w.markConflict(ctx, old, fresh.ID); err != nil {
					return rels, err
				}
				if err := w.markConflict(ctx, fresh, old.ID); err != nil {
					return rels, err
				}
				rels = append(rels,
					w.base.relation(knowledgeNode(fresh.ID), domain.RelationConflictsWith,
						knowledgeNode(old.ID), nil, confidenceVerbatim),
					w.base.relation(knowledgeNode(old.ID), domain.RelationConflictsWith,
						knowledgeNode(fresh.ID), nil, confidenceVerbatim))
			}
		}
	}
	return rels, nil
}

// predecessorsOf finds the active decisions one new decision replaces.
//
// Two signals connect them, and only two. An explicit Supersedes names a
// subject or an item id outright. Otherwise the subjects must match, which for
// a topic-bearing decision means "the same topic in the same scope" and for
// one without a topic means "the same statement" — a decision that never named
// its topic can only supersede a verbatim restatement of itself, which is the
// conservative reading.
func (w *knowledgeWriter) predecessorsOf(fresh domain.ProjectMemoryItem) []domain.ProjectMemoryItem {
	subject := domain.KnowledgeSubjectOf(fresh)
	explicit := w.explicitSupersedes(fresh)
	var out []domain.ProjectMemoryItem
	for _, old := range w.existing {
		if old.Key.Type != domain.MemoryTypeDecision || old.ID == fresh.ID {
			continue
		}
		if !domain.KnowledgeStatusOf(old).Current() {
			continue
		}
		if domain.KnowledgeSubjectOf(old) == subject || old.ID == explicit ||
			(explicit != "" && domain.KnowledgeSubjectOf(old) == explicit) {
			out = append(out, old)
		}
	}
	return out
}

// explicitSupersedes reads the subject or id a decision declared it replaces.
func (w *knowledgeWriter) explicitSupersedes(fresh domain.ProjectMemoryItem) string {
	for _, d := range w.out.Decisions {
		scope, scopeKey := normalizeKnowledgeScope(d.Scope, d.ScopeKey)
		if decisionSubject(scope, scopeKey, d) == domain.KnowledgeSubjectOf(fresh) {
			return strings.TrimSpace(d.Supersedes)
		}
	}
	return ""
}

// retire moves one fact out of current knowledge without losing it.
//
// The row keeps MemoryStateValid — it is not stale, and its provenance has not
// moved; it is simply no longer the current answer. Using the drift state for
// this would conflate "AO cannot vouch for this" with "the project changed its
// mind", and only the first is a reason to stop trusting the row.
func (w *knowledgeWriter) retire(
	ctx context.Context, item domain.ProjectMemoryItem,
	status domain.KnowledgeStatus, refKey, refValue string,
) error {
	updated := domain.WithKnowledgeMetadata(item, domain.MetaKnowledgeStatus, string(status))
	updated = domain.WithKnowledgeMetadata(updated, refKey, refValue)
	updated = updated.Normalized()
	_, err := w.svc.repo.PutProjectMemoryItem(ctx, updated, w.now)
	return err
}

// markConflict records that one fact could not be ordered against another.
func (w *knowledgeWriter) markConflict(ctx context.Context, item domain.ProjectMemoryItem, other string) error {
	updated := domain.WithKnowledgeMetadata(item, domain.MetaKnowledgeStatus, string(domain.KnowledgeConflicting))
	updated = domain.WithKnowledgeMetadata(updated, domain.MetaKnowledgeConflictsWith, other)
	updated = updated.Normalized()
	_, err := w.svc.repo.PutProjectMemoryItem(ctx, updated, w.now)
	return err
}

// resolveRisks closes the risks this outcome says it fixed.
func (w *knowledgeWriter) resolveRisks(ctx context.Context) ([]domain.ProjectMemoryRelation, error) {
	if len(w.out.ResolvesRisks) == 0 {
		return nil, nil
	}
	wanted := make(map[string]struct{}, len(w.out.ResolvesRisks))
	for _, r := range w.out.ResolvesRisks {
		if r = strings.TrimSpace(r); r != "" {
			wanted[r] = struct{}{}
		}
	}
	var rels []domain.ProjectMemoryRelation
	for _, item := range w.existing {
		if item.Key.Type != domain.MemoryTypeKnownRisk || !domain.KnowledgeStatusOf(item).Current() {
			continue
		}
		_, byID := wanted[item.ID]
		_, bySubject := wanted[domain.KnowledgeSubjectOf(item)]
		if !byID && !bySubject {
			continue
		}
		if err := w.retire(ctx, item, domain.KnowledgeResolved,
			domain.MetaKnowledgeResolvedBy, w.out.TaskRef); err != nil {
			return rels, err
		}
		rels = append(rels, w.base.relation(
			knowledgeNode(item.ID), domain.RelationResolvedBy,
			domain.ProjectMemoryNode{Kind: domain.NodeTask, Key: w.out.TaskRef},
			nil, confidenceVerbatim))
	}
	return rels, nil
}

// lineage records what this task's knowledge came from and what it follows.
//
// Every edge here is a fact AO already holds: a produced edge exists because
// the item exists, a changed edge because the durable scope named the path, a
// depends_on edge because the plan declared the dependency. Nothing is
// asserted because two tasks happened to touch the same repository — that
// would be the invented relationship P2-C §5 forbids.
func (w *knowledgeWriter) lineage(written []domain.ProjectMemoryItem) []domain.ProjectMemoryRelation {
	task := domain.ProjectMemoryNode{Kind: domain.NodeTask, Key: w.out.TaskRef}
	rels := make([]domain.ProjectMemoryRelation, 0, len(written)+len(w.paths)+len(w.out.Modules))

	for _, item := range written {
		rels = append(rels, w.base.relation(task, domain.RelationProduced,
			knowledgeNode(item.ID), item.SourcePaths, confidenceVerbatim))
	}
	for _, p := range w.paths {
		rels = append(rels, w.base.relation(task, domain.RelationChanged,
			domain.ProjectMemoryNode{Kind: domain.NodeFile, Key: p}, []string{p}, confidenceVerbatim))
	}
	for _, m := range dedupeUnsorted(w.out.Modules) {
		if m = strings.TrimSpace(m); m == "" {
			continue
		}
		rels = append(rels, w.base.relation(task, domain.RelationAffects,
			domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: m}, nil, confidenceVerbatim))
	}
	for _, item := range written {
		if item.Key.Type != domain.MemoryTypeKnownRisk {
			continue
		}
		for _, m := range riskModulesOf(w.out, item) {
			rels = append(rels, w.base.relation(knowledgeNode(item.ID), domain.RelationConcerns,
				domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: m}, nil, confidenceVerbatim))
		}
	}
	for _, dep := range dedupeUnsorted(w.out.DependsOnTasks) {
		if dep = strings.TrimSpace(dep); dep == "" || dep == w.out.TaskRef {
			continue
		}
		rels = append(rels, w.base.relation(task, domain.RelationDependsOn,
			domain.ProjectMemoryNode{Kind: domain.NodeTask, Key: dep}, nil, confidenceVerbatim))
	}
	for _, up := range dedupeUnsorted(w.out.FollowsUpTasks) {
		if up = strings.TrimSpace(up); up == "" || up == w.out.TaskRef {
			continue
		}
		rels = append(rels, w.base.relation(task, domain.RelationFollowsUp,
			domain.ProjectMemoryNode{Kind: domain.NodeTask, Key: up}, nil, confidenceVerbatim))
	}
	return rels
}

// riskModulesOf finds the modules the outcome declared for one written risk.
func riskModulesOf(out TaskOutcome, item domain.ProjectMemoryItem) []string {
	subject := domain.KnowledgeSubjectOf(item)
	for _, r := range out.Risks {
		scope, scopeKey := normalizeKnowledgeScope(r.Scope, r.ScopeKey)
		if riskSubject(scope, scopeKey, r) == subject {
			return dedupeUnsorted(r.Modules)
		}
	}
	return nil
}

// --- compaction ------------------------------------------------------------

// compactKnowledge enforces the per-scope retention bounds.
//
// It retires rather than deletes, and it retires the OLDEST first, so what
// survives is what a later task is most likely to need. Retired rows keep
// every field they had — provenance included — so "twenty tasks touched
// payments and we kept the last eight" is still fully reconstructible from
// storage even though only eight can appear in a context pack.
func (s *Service) compactKnowledge(
	ctx context.Context, projectID domain.ProjectID, repoID string, now time.Time,
) error {
	items, err := s.repo.ListProjectMemoryItemsByState(ctx, projectID, repoID, domain.MemoryStateValid)
	if err != nil {
		return err
	}
	byModule := map[string][]domain.ProjectMemoryItem{}
	var decisions, risks []domain.ProjectMemoryItem
	for _, it := range items {
		if !domain.KnowledgeStatusOf(it).Current() {
			continue
		}
		switch it.Key.Type {
		case domain.MemoryTypeTaskResult:
			byModule[compactionScopeOf(it)] = append(byModule[compactionScopeOf(it)], it)
		case domain.MemoryTypeDecision:
			decisions = append(decisions, it)
		case domain.MemoryTypeKnownRisk:
			risks = append(risks, it)
		}
	}
	for scope, group := range byModule {
		if err := s.retireOldest(ctx, group, MaxTaskResultsPerModule, now,
			fmt.Sprintf("compacted: beyond the %d most recent task outcomes for %s",
				MaxTaskResultsPerModule, scope)); err != nil {
			return err
		}
	}
	if err := s.retireOldest(ctx, decisions, MaxActiveDecisions, now,
		fmt.Sprintf("compacted: beyond the %d most recent active decisions", MaxActiveDecisions)); err != nil {
		return err
	}
	return s.retireOldest(ctx, risks, MaxOpenRisks, now,
		fmt.Sprintf("compacted: beyond the %d most recent open risks", MaxOpenRisks))
}

// compactionScopeOf is the bucket a task result is retained within.
//
// It is the first module the task named, falling back to the directory of its
// first evidence path and then to the repository. Bucketing by module is what
// stops one busy area from evicting every other area's history.
//
// The module is read from metadata rather than parsed back out of the rendered
// body. Re-parsing prose AO itself wrote would make a change to the rendering
// silently change the retention buckets, which is the kind of coupling that
// only shows up as "why did that module's history disappear".
func compactionScopeOf(item domain.ProjectMemoryItem) string {
	if module := strings.TrimSpace(item.Metadata[metaPrimaryModule]); module != "" {
		return module
	}
	if len(item.SourcePaths) > 0 {
		return moduleOf(item.SourcePaths[0])
	}
	return "the repository"
}

// metaPrimaryModule names the module a task outcome is retained under.
const metaPrimaryModule = "module"

// primaryModuleOf is the module a task outcome is bucketed under: the first
// module the task named, or the directory of its first changed path.
func primaryModuleOf(out TaskOutcome) string {
	for _, m := range out.Modules {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	if paths := domain.NormalizeMemorySourcePaths(out.FilesChanged); len(paths) > 0 {
		return moduleOf(paths[0])
	}
	return ""
}

// retireOldest marks everything past the bound obsolete, newest kept.
func (s *Service) retireOldest(
	ctx context.Context, group []domain.ProjectMemoryItem, keep int, now time.Time, reason string,
) error {
	if len(group) <= keep {
		return nil
	}
	sort.Slice(group, func(i, j int) bool {
		if !group[i].UpdatedAt.Equal(group[j].UpdatedAt) {
			return group[i].UpdatedAt.After(group[j].UpdatedAt)
		}
		return group[i].ID < group[j].ID
	})
	for _, old := range group[keep:] {
		updated := domain.WithKnowledgeMetadata(old, domain.MetaKnowledgeStatus, string(domain.KnowledgeObsolete))
		updated = domain.WithKnowledgeMetadata(updated, domain.MetaKnowledgeAggregate, reason)
		if _, err := s.repo.PutProjectMemoryItem(ctx, updated.Normalized(), now); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

// normalizeKnowledgeScope defaults a knowledge fact to repository scope.
//
// A decision with no stated scope governs the repository, which is both the
// safe reading and the useful one: a decision that only applied to one file
// would have said so.
func normalizeKnowledgeScope(
	scope domain.ProjectMemoryScope, key string,
) (domain.ProjectMemoryScope, string) {
	key = strings.TrimSpace(key)
	if !scope.Valid() || scope == domain.MemoryScopeTask {
		scope = domain.MemoryScopeRepository
	}
	if scope == domain.MemoryScopeRepository || scope == domain.MemoryScopeProject {
		key = ""
	}
	return scope, key
}

// decisionSubject derives what a decision is about.
func decisionSubject(scope domain.ProjectMemoryScope, scopeKey string, d TaskDecision) string {
	if topic := strings.TrimSpace(d.Topic); topic != "" {
		return domain.KnowledgeSubject(scope, scopeKey, "topic:"+strings.ToLower(topic))
	}
	return domain.KnowledgeSubject(scope, scopeKey, d.Statement)
}

// riskSubject derives what a risk is about.
func riskSubject(scope domain.ProjectMemoryScope, scopeKey string, r TaskRisk) string {
	if topic := strings.TrimSpace(r.Topic); topic != "" {
		return domain.KnowledgeSubject(scope, scopeKey, "topic:"+strings.ToLower(topic))
	}
	return domain.KnowledgeSubject(scope, scopeKey, r.Statement)
}

// knowledgeEvidence bounds a fact's own evidence, falling back to the task's
// changed paths when the caller supplied none.
//
// The fallback matters for relevance, not for provenance: a decision with no
// evidence of its own is still about the area the task touched, and giving it
// that anchor is what lets a later task working in the same area find it.
func knowledgeEvidence(evidence, fallback []string) []string {
	paths := domain.NormalizeMemorySourcePaths(evidence)
	if len(paths) == 0 {
		paths = fallback
	}
	if len(paths) > domain.MaxProjectMemorySourcePaths {
		paths = paths[:domain.MaxProjectMemorySourcePaths]
	}
	return paths
}

// statementFingerprint is the short, stable hash that distinguishes one
// wording of a decision from another within the same subject.
//
// It hashes the statement's significant words rather than its bytes, so
// punctuation and casing do not fork a decision into two rows that then
// supersede each other forever.
func statementFingerprint(statement string) string {
	return strings.TrimPrefix(
		domain.KnowledgeSubject(domain.MemoryScopeRepository, "", statement), "sub_")[:10]
}

// knowledgeNode addresses one memory item as a graph endpoint.
func knowledgeNode(id string) domain.ProjectMemoryNode {
	return domain.ProjectMemoryNode{Kind: domain.NodeKnowledge, Key: id}
}
