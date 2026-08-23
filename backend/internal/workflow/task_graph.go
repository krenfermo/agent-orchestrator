package workflow

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TaskGraphPolicyVersion is the version of the classification policy below.
// It is stamped into every scope and lets a scope written today stay
// explainable after the policy changes, exactly like VerifyScopePolicyVersion
// and ReviewPolicyVersion.
const TaskGraphPolicyVersion = "v1"

// TaskRelationReason is a stable, machine-checkable code explaining why one
// task pair was classified the way it was, persisted alongside the decision so
// a plan classified today remains explainable later (mirrors ReviewReason and
// VerifyScopeReason).
type TaskRelationReason string

const (
	// TaskRelationReasonDirectDependency means one task lists the other in its
	// dependency edges.
	TaskRelationReasonDirectDependency TaskRelationReason = "direct_dependency_edge"
	// TaskRelationReasonTransitiveDependency means the DAG orders the pair through
	// one or more intermediate tasks.
	TaskRelationReasonTransitiveDependency TaskRelationReason = "transitive_dependency_edge"
	// TaskRelationReasonSharedFileWrite means no dependency edge, but both tasks are
	// estimated to write the same file.
	TaskRelationReasonSharedFileWrite TaskRelationReason = "shared_file_write"
	// TaskRelationReasonSharedPackageWrite means no dependency edge, and at least
	// one task's write scope is a directory that contains the other's, so the
	// specific file is unknown but the region is shared.
	TaskRelationReasonSharedPackageWrite TaskRelationReason = "shared_package_write"
	// TaskRelationReasonDisjointWriteSets means no dependency edge and no
	// overlapping write scope -- the pair is independent work.
	TaskRelationReasonDisjointWriteSets TaskRelationReason = "disjoint_write_sets"
	// TaskRelationReasonDeclaredSafeOverlap means the write sets DO overlap,
	// but the plan explicitly declared every overlapping path safe to share
	// with this specific sibling. The waiver's own reason is carried into the
	// stored relationship's detail.
	TaskRelationReasonDeclaredSafeOverlap TaskRelationReason = "declared_safe_write_overlap"
)

// TaskScopeInput is everything the classifier is allowed to know about one
// planned task. It is deliberately plain data: ClassifyTaskGraph does no IO,
// no model call, and no filesystem access, so the same plan always produces
// the same graph.
type TaskScopeInput struct {
	TaskID     string
	PlanStepID string
	Ordinal    int64
	Title      string
	// Objective-level context the task inherits; see TaskGraphInput.Objective.
	Description        string
	AcceptanceCriteria []string
	Verify             VerificationPlan
	// Dependencies are task ids (not plan step ids), matching
	// domain.WorkflowTask.Dependencies.
	Dependencies []string
	// DeclaredFiles / DeclaredPackages are what the plan named explicitly,
	// when it named anything. They are trusted over anything inferred from
	// prose.
	DeclaredFiles    []string
	DeclaredPackages []string
	// ObservedWritePaths are the paths this task's execution actually changed.
	// Empty until the task has run; when present they are the strongest
	// possible write-set evidence and flip the scope's source to "observed".
	ObservedWritePaths []string
	// SafeWriteOverlaps are this task's explicit waivers, already resolved to
	// task ids. Absent any waiver, an overlapping write set is a probable
	// conflict -- see ClassifyTaskRelations.
	SafeWriteOverlaps []domain.WorkflowTaskSafeOverlap
}

// TaskGraphInput is one whole plan, as handed to the classifier.
type TaskGraphInput struct {
	WorkflowRunID string
	Objective     string
	// RepoRoots is the repository-structure signal: the top-level directories
	// the project is actually made of (e.g. "backend", "frontend", "docs").
	// Optional -- with none, the classifier falls back to shape heuristics
	// alone and simply admits fewer directory-looking tokens.
	RepoRoots []string
	Tasks     []TaskScopeInput
}

// TaskGraph is the classifier's whole output: one scope per task plus one
// classification per unordered task pair.
type TaskGraph struct {
	// Scopes is keyed by TaskScopeInput.TaskID.
	Scopes map[string]domain.WorkflowTaskScope
	// Relationships is sorted by (TaskID, RelatedTaskID) and contains exactly
	// one entry per unordered pair.
	Relationships []domain.WorkflowTaskRelationship
}

// ClassifyTaskGraph estimates a write set for every task and labels every task
// pair. Pure and deterministic: no IO, no randomness, no model call.
//
// The two decisions it makes, and why they are made this way:
//
//   - A pair the DAG already orders is a functional dependency, full stop --
//     even when the two write the same file. The ordering is what removes the
//     collision, so reporting it a second time as a conflict would tell a
//     scheduler to serialize something already serialized.
//
//   - Otherwise the pair conflicts only when the two write sets actually
//     intersect: the same file, or a directory of one containing a path of the
//     other. Two tasks writing *different* files in the same package are NOT a
//     conflict -- that is the ordinary case in any codebase and calling it one
//     would serialize a whole plan for nothing. A directory only enters a
//     write set when the specific file is unknown, which is exactly when the
//     region really is shared.
func ClassifyTaskGraph(in TaskGraphInput) TaskGraph {
	roots := normalizeStrings(append(append([]string{}, in.RepoRoots...), discoverRepoRoots(in.Tasks)...))
	graph := TaskGraph{Scopes: make(map[string]domain.WorkflowTaskScope, len(in.Tasks)), Relationships: []domain.WorkflowTaskRelationship{}}

	tasks := append([]TaskScopeInput(nil), in.Tasks...)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].TaskID < tasks[j].TaskID })

	// Pass 1: per-task read/write estimate, before anything pairwise is known.
	pairs := make([]TaskRelationInput, 0, len(tasks))
	for _, t := range tasks {
		scope := estimateTaskScope(t, in.Objective, roots)
		graph.Scopes[t.TaskID] = scope
		pairs = append(pairs, TaskRelationInput{TaskID: t.TaskID, Ordinal: t.Ordinal, Dependencies: t.Dependencies, WritePaths: scope.WritePaths, SafeWriteOverlaps: scope.SafeWriteOverlaps})
	}

	// Passes 2 and 3: pair verdicts, then the scheduling facts that are only
	// knowable once every pair has one.
	rels, scheduling := ClassifyTaskRelations(in.WorkflowRunID, pairs)
	graph.Relationships = rels
	for id, sched := range scheduling {
		scope := graph.Scopes[id]
		scope.ExecutionStrategy = sched.ExecutionStrategy
		scope.IntegrationDependencies = sched.IntegrationDependencies
		graph.Scopes[id] = scope
	}
	return graph
}

// TaskRelationInput is the pairwise half of the classifier's input: everything
// needed to label a pair, with the write set already estimated. It is a
// separate entry point so a plan whose write sets have been REFRESHED -- with
// what a completed task actually wrote -- can be re-classified without
// re-deriving every task's scope from plan text that has not changed.
type TaskRelationInput struct {
	TaskID       string
	Ordinal      int64
	Dependencies []string
	WritePaths   []string
	// SafeWriteOverlaps are this task's waivers. They travel with the write
	// set so a re-classification from persisted scopes applies exactly the
	// same waivers the original classification did.
	SafeWriteOverlaps []domain.WorkflowTaskSafeOverlap
}

// TaskScheduling is what a pair classification implies for one task on its
// own: how it may be scheduled, and what must be integrated before it.
type TaskScheduling struct {
	ExecutionStrategy       domain.WorkflowTaskExecutionStrategy
	IntegrationDependencies []string
}

// ClassifyTaskRelations labels every unordered pair of a plan's tasks and
// derives each task's execution strategy and integration order from those
// labels. Pure and deterministic; the returned relationships are sorted by
// (TaskID, RelatedTaskID) and contain exactly one entry per pair.
func ClassifyTaskRelations(runID string, in []TaskRelationInput) ([]domain.WorkflowTaskRelationship, map[string]TaskScheduling) {
	tasks := append([]TaskRelationInput(nil), in...)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].TaskID < tasks[j].TaskID })
	known := map[string]bool{}
	for _, t := range tasks {
		known[t.TaskID] = true
	}
	reach := transitiveDependencies(tasks, known)

	rels := []domain.WorkflowTaskRelationship{}
	conflictPartners := map[string][]string{}
	for i := 0; i < len(tasks); i++ {
		for j := i + 1; j < len(tasks); j++ {
			a, b := tasks[i], tasks[j]
			rel := classifyTaskPair(a, b, reach)
			rel.WorkflowRunID = runID
			rels = append(rels, rel)
			if rel.Relation == domain.WorkflowTaskRelationWriteConflict {
				conflictPartners[a.TaskID] = append(conflictPartners[a.TaskID], b.TaskID)
				conflictPartners[b.TaskID] = append(conflictPartners[b.TaskID], a.TaskID)
			}
		}
	}

	ordinalOf := map[string]int64{}
	for _, t := range tasks {
		ordinalOf[t.TaskID] = t.Ordinal
	}
	scheduling := make(map[string]TaskScheduling, len(tasks))
	for _, t := range tasks {
		deps := normalizeStrings(t.Dependencies)
		partners := normalizeStrings(conflictPartners[t.TaskID])
		sched := TaskScheduling{ExecutionStrategy: domain.WorkflowTaskExecutionParallel}
		switch {
		case len(partners) > 0:
			sched.ExecutionStrategy = domain.WorkflowTaskExecutionSerialized
		case len(deps) > 0:
			sched.ExecutionStrategy = domain.WorkflowTaskExecutionSequential
		}
		integration := append([]string{}, deps...)
		for _, p := range partners {
			// Only an EARLIER conflict partner is an integration dependency.
			// Taking both directions would be a cycle, and the plan's own
			// ordinal is the one total order both tasks already agree on.
			if ordinalOf[p] < t.Ordinal || (ordinalOf[p] == t.Ordinal && p < t.TaskID) {
				integration = append(integration, p)
			}
		}
		sched.IntegrationDependencies = normalizeStrings(integration)
		scheduling[t.TaskID] = sched
	}
	return rels, scheduling
}

// discoverRepoRoots reads the repository's own shape back out of the plan: the
// first segment of every unambiguous (three-or-more-segment) path any task
// names. It is what lets a two-segment directory like "frontend/src" be
// recognized as a path once some other task has said
// "frontend/src/api/schema.ts". Callers may also supply roots they know
// independently; the two sets are merged.
func discoverRepoRoots(tasks []TaskScopeInput) []string {
	roots := map[string]bool{}
	consider := func(tok string) {
		tok = strings.Trim(strings.TrimSpace(tok), "`'\"()[]{}<>,;:")
		tok = strings.TrimPrefix(tok, "./")
		if strings.Contains(tok, "://") || strings.HasPrefix(tok, "/") {
			return
		}
		segments := strings.Split(tok, "/")
		if len(segments) < 3 {
			return
		}
		for _, seg := range segments {
			if seg == "" || englishSlashWords[strings.ToLower(seg)] {
				return
			}
		}
		roots[segments[0]] = true
	}
	for _, t := range tasks {
		for _, unit := range textUnits(t) {
			for _, tok := range pathTokenRe.FindAllString(unit, -1) {
				consider(tok)
			}
		}
		for _, f := range t.Verify.Files {
			consider(f.Path)
		}
		for _, f := range t.DeclaredFiles {
			consider(f)
		}
		for _, pkg := range t.DeclaredPackages {
			consider(pkg)
		}
		for _, o := range t.ObservedWritePaths {
			consider(o)
		}
	}
	return sortedKeys(roots)
}

// estimateTaskScope derives one task's read/write footprint from its
// acceptance criteria, its prose, its verification checks, whatever the plan
// declared explicitly, and -- when the task has already run -- what it
// actually wrote.
func estimateTaskScope(t TaskScopeInput, objective string, roots []string) domain.WorkflowTaskScope {
	scope := domain.WorkflowTaskScope{Version: TaskGraphPolicyVersion, Source: domain.WorkflowTaskScopeEstimated}
	writes := map[string]bool{}
	reads := map[string]bool{}

	// Prose. A path mentioned in a unit of text that also carries a write verb
	// is a write; anything else the task merely names is a read.
	for _, unit := range textUnits(t) {
		target := reads
		if mentionsWriteIntent(unit) {
			target = writes
		}
		for _, p := range extractPaths(unit, roots) {
			target[p] = true
		}
	}
	// The objective is shared by every task, so it can never discriminate
	// between them: it contributes read context only.
	for _, p := range extractPaths(objective, roots) {
		if !writes[p] {
			reads[p] = true
		}
	}

	// Verification. A step's file checks describe the artifacts that step is
	// responsible for, whether it must create them or must have removed them,
	// so both polarities are writes. A command's working directory and package
	// arguments are read context only -- running a test somewhere says nothing
	// about editing it.
	//
	// Both halves are resolved through the spec's own VerifyPathContext rather
	// than read literally, for exactly the reason that type exists: a plan that
	// says workingDirectory "backend" and file check "internal/workflow/plan.go"
	// means backend/internal/workflow/plan.go, and a write set that recorded the
	// unqualified path would never match the other task that names the real one.
	dirs := make([]string, 0, len(t.Verify.Commands))
	for _, c := range t.Verify.Commands {
		dirs = append(dirs, c.WorkingDirectory)
	}
	pathCtx := verifyPathContextFor(dirs)
	for _, f := range t.Verify.Files {
		if p, ok := normalizePath(pathCtx.ResolvePath(f.Path), roots); ok {
			writes[p] = true
		}
	}
	for _, c := range t.Verify.Commands {
		if p, ok := normalizePath(normalizeRel(c.WorkingDirectory), roots); ok {
			reads[p] = true
		}
		for _, a := range c.Args {
			cand := strings.TrimSuffix(strings.TrimPrefix(a, "./"), "/...")
			// Only an argument that already looks like a path is resolved
			// against the spec's namespace. Resolving a flag would turn
			// "-race" into "backend/-race", which normalizePath would then
			// happily accept as a path under a known repository root.
			if strings.HasPrefix(cand, "-") || (!strings.Contains(cand, "/") && !isFilePath(cand)) {
				continue
			}
			if p, ok := normalizePath(pathCtx.ResolvePath(cand), roots); ok {
				reads[p] = true
			}
		}
	}

	// Anything the plan named explicitly outranks anything inferred above.
	for _, f := range t.DeclaredFiles {
		if p, ok := normalizePath(f, roots); ok {
			writes[p] = true
		}
	}
	for _, pkg := range t.DeclaredPackages {
		if p, ok := normalizePath(pkg, roots); ok {
			writes[p] = true
		}
	}

	// And what actually happened outranks everything.
	observed := []string{}
	for _, o := range t.ObservedWritePaths {
		if p, ok := normalizePath(o, roots); ok {
			writes[p] = true
			delete(reads, p)
			observed = append(observed, p)
		}
	}
	scope.ObservedWritePaths = normalizeStrings(observed)
	if len(scope.ObservedWritePaths) > 0 {
		scope.Source = domain.WorkflowTaskScopeObserved
	}

	// A path is never both: writing something implies reading it, and listing
	// it twice would make every conflict check ambiguous.
	for w := range writes {
		delete(reads, w)
	}
	scope.WritePaths = sortedKeys(writes)
	scope.ReadPaths = sortedKeys(reads)

	facetPaths := append(append([]string{}, scope.WritePaths...), scope.ReadPaths...)
	for _, pkg := range t.DeclaredPackages {
		if p, ok := normalizePath(pkg, roots); ok {
			facetPaths = append(facetPaths, p)
		}
	}
	scope.Files, scope.Packages, scope.Components = derivePathFacets(facetPaths, roots)

	scope.Symbols = extractSymbols(t)
	scope.SafeWriteOverlaps = normalizeSafeOverlaps(t.SafeWriteOverlaps)
	if scope.IntegrationDependencies == nil {
		scope.IntegrationDependencies = []string{}
	}
	return scope
}

// normalizeSafeOverlaps canonicalizes a task's waivers so the persisted scope
// is byte-stable, and drops the ones that could never waive anything: a waiver
// naming no sibling, or stating no reason. Dropping them here rather than at
// use keeps the stored record honest about which declarations actually count.
func normalizeSafeOverlaps(in []domain.WorkflowTaskSafeOverlap) []domain.WorkflowTaskSafeOverlap {
	out := []domain.WorkflowTaskSafeOverlap{}
	for _, w := range in {
		with := strings.TrimSpace(w.WithTaskID)
		reason := strings.TrimSpace(w.Reason)
		if with == "" || reason == "" {
			continue
		}
		paths := map[string]bool{}
		for _, p := range w.Paths {
			p = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
			if p != "" {
				paths[p] = true
			}
		}
		out = append(out, domain.WorkflowTaskSafeOverlap{WithTaskID: with, Paths: sortedKeys(paths), Reason: reason})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WithTaskID != out[j].WithTaskID {
			return out[i].WithTaskID < out[j].WithTaskID
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

func classifyTaskPair(a, b TaskRelationInput, reach map[string]map[string]bool) domain.WorkflowTaskRelationship {
	rel := domain.WorkflowTaskRelationship{TaskID: a.TaskID, RelatedTaskID: b.TaskID, Overlap: []string{}}
	if rel.TaskID > rel.RelatedTaskID {
		rel.TaskID, rel.RelatedTaskID = rel.RelatedTaskID, rel.TaskID
	}
	switch {
	case reach[a.TaskID][b.TaskID]:
		rel.Relation = domain.WorkflowTaskRelationDependency
		rel.Reason, rel.Detail = dependencyReason(a, b)
		return rel
	case reach[b.TaskID][a.TaskID]:
		rel.Relation = domain.WorkflowTaskRelationDependency
		rel.Reason, rel.Detail = dependencyReason(b, a)
		return rel
	}
	overlap, exactFiles := writeOverlap(a.WritePaths, b.WritePaths)
	if len(overlap) == 0 {
		rel.Relation = domain.WorkflowTaskRelationIndependent
		rel.Reason = string(TaskRelationReasonDisjointWriteSets)
		rel.Detail = fmt.Sprintf("%s and %s have no dependency edge and no overlapping write scope", rel.TaskID, rel.RelatedTaskID)
		return rel
	}
	// An overlap is a conflict by default. Only an explicit, path-scoped,
	// reasoned waiver naming the other task can clear one, and only the paths
	// it actually names: whatever it does not cover still conflicts, so a
	// waiver can never quietly widen into a blanket exemption.
	remaining, waived, waiverReason := applySafeOverlaps(overlap, a, b)
	if len(remaining) == 0 {
		rel.Relation = domain.WorkflowTaskRelationIndependent
		rel.Reason = string(TaskRelationReasonDeclaredSafeOverlap)
		rel.Overlap = waived
		rel.Detail = fmt.Sprintf("%s and %s both write %s, declared safe: %s",
			rel.TaskID, rel.RelatedTaskID, strings.Join(waived, ", "), waiverReason)
		return rel
	}
	rel.Relation = domain.WorkflowTaskRelationWriteConflict
	rel.Overlap = remaining
	waivedNote := ""
	if len(waived) > 0 {
		waivedNote = fmt.Sprintf(" (%s declared safe: %s)", strings.Join(waived, ", "), waiverReason)
	}
	if namesSharedFile(remaining, exactFiles) {
		rel.Reason = string(TaskRelationReasonSharedFileWrite)
		rel.Detail = fmt.Sprintf("%s and %s are both estimated to write %s%s", rel.TaskID, rel.RelatedTaskID, strings.Join(remaining, ", "), waivedNote)
		return rel
	}
	rel.Reason = string(TaskRelationReasonSharedPackageWrite)
	rel.Detail = fmt.Sprintf("%s and %s are both estimated to write inside %s%s", rel.TaskID, rel.RelatedTaskID, strings.Join(remaining, ", "), waivedNote)
	return rel
}

// applySafeOverlaps removes from an overlap every path a waiver on either side
// explicitly covers, and returns what is left, what was waived, and the
// combined reason text the waivers gave.
//
// A waiver from ONE side is enough. The plan authors both tasks, so requiring
// both to repeat the same declaration would buy no extra safety and only make
// the waiver easy to get subtly wrong; the detail names which task declared it
// so the claim stays attributable.
func applySafeOverlaps(overlap []string, a, b TaskRelationInput) (remaining, waived []string, reason string) {
	type waiverNote struct{ by, text string }
	notes := []waiverNote{}
	waivedSet := map[string]bool{}
	consider := func(self, other TaskRelationInput) {
		for _, w := range self.SafeWriteOverlaps {
			if strings.TrimSpace(w.WithTaskID) != other.TaskID || strings.TrimSpace(w.Reason) == "" {
				// A waiver that names nobody, names the wrong sibling, or
				// states no reason waives nothing. Silently ignoring it is
				// the safe direction: the pair simply stays a conflict.
				continue
			}
			covered := false
			for _, p := range overlap {
				if !waiverCovers(w.Paths, p) {
					continue
				}
				waivedSet[p] = true
				covered = true
			}
			if covered {
				notes = append(notes, waiverNote{by: self.TaskID, text: strings.TrimSpace(w.Reason)})
			}
		}
	}
	consider(a, b)
	consider(b, a)
	if len(waivedSet) == 0 {
		return overlap, []string{}, ""
	}
	for _, p := range overlap {
		if !waivedSet[p] {
			remaining = append(remaining, p)
		}
	}
	sort.Slice(notes, func(i, j int) bool {
		if notes[i].by != notes[j].by {
			return notes[i].by < notes[j].by
		}
		return notes[i].text < notes[j].text
	})
	texts := make([]string, 0, len(notes))
	seen := map[string]bool{}
	for _, n := range notes {
		line := fmt.Sprintf("%s: %s", n.by, n.text)
		if seen[line] {
			continue
		}
		seen[line] = true
		texts = append(texts, line)
	}
	return remaining, sortedKeys(waivedSet), strings.Join(texts, "; ")
}

// waiverCovers reports whether a waiver's path list covers one overlapping
// path. An empty list covers the whole overlap with that sibling; a directory
// covers everything under it.
func waiverCovers(paths []string, candidate string) bool {
	if len(paths) == 0 {
		return true
	}
	for _, p := range paths {
		p = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
		if p == "" {
			continue
		}
		if p == candidate || pathWithin(candidate, p) {
			return true
		}
	}
	return false
}

func dependencyReason(downstream, upstream TaskRelationInput) (reason, detail string) {
	for _, d := range downstream.Dependencies {
		if d == upstream.TaskID {
			return string(TaskRelationReasonDirectDependency),
				fmt.Sprintf("%s depends directly on %s", downstream.TaskID, upstream.TaskID)
		}
	}
	return string(TaskRelationReasonTransitiveDependency),
		fmt.Sprintf("%s depends on %s through the plan's dependency graph", downstream.TaskID, upstream.TaskID)
}

// writeOverlap returns the specific paths that make two write sets intersect,
// and which of them both sets name EXACTLY. A directory in one set that
// contains a path in the other also counts as an intersection -- the directory
// is only ever in a write set because the exact file was unknown -- but it is
// not an exact match, so it stays a shared-package finding rather than
// claiming the two tasks named the same file.
func writeOverlap(a, b []string) (overlap []string, exact map[string]bool) {
	out := map[string]bool{}
	exact = map[string]bool{}
	for _, pa := range a {
		for _, pb := range b {
			switch {
			case pa == pb:
				out[pa] = true
				if isFilePath(pa) {
					exact[pa] = true
				}
			case !isFilePath(pa) && pathWithin(pb, pa):
				out[pb] = true
			case !isFilePath(pb) && pathWithin(pa, pb):
				out[pa] = true
			}
		}
	}
	return sortedKeys(out), exact
}

// namesSharedFile reports whether any surviving overlap path is one both write
// sets named exactly, which is what separates a shared-file conflict from a
// shared-package one.
func namesSharedFile(paths []string, exact map[string]bool) bool {
	for _, p := range paths {
		if exact[p] {
			return true
		}
	}
	return false
}

// pathWithin reports whether child sits under dir. Both are slash-separated
// and already normalized.
func pathWithin(child, dir string) bool {
	return dir != "" && child != dir && strings.HasPrefix(child, dir+"/")
}

// transitiveDependencies closes each task's dependency edges so a pair ordered
// through intermediate tasks is still recognized as ordered.
func transitiveDependencies(tasks []TaskRelationInput, known map[string]bool) map[string]map[string]bool {
	direct := map[string][]string{}
	for _, t := range tasks {
		for _, d := range t.Dependencies {
			if known[d] && d != t.TaskID {
				direct[t.TaskID] = append(direct[t.TaskID], d)
			}
		}
	}
	reach := map[string]map[string]bool{}
	var visit func(id string, seen map[string]bool) map[string]bool
	visit = func(id string, seen map[string]bool) map[string]bool {
		if got, ok := reach[id]; ok {
			return got
		}
		if seen[id] {
			// A cycle cannot reach this far -- NormalizeAndValidatePlan
			// rejects cyclic plans before any task row exists -- but the
			// classifier must terminate on any input it is handed.
			return map[string]bool{}
		}
		seen[id] = true
		out := map[string]bool{}
		for _, d := range direct[id] {
			out[d] = true
			for k := range visit(d, seen) {
				out[k] = true
			}
		}
		delete(seen, id)
		reach[id] = out
		return out
	}
	for _, t := range tasks {
		visit(t.TaskID, map[string]bool{})
	}
	return reach
}

// textUnits splits a task into the smallest chunks over which a write verb
// still binds to the paths named beside it. Splitting finer than a sentence
// would lose the verb; coarser would let one "add" in a long description turn
// every path the task merely reads into a write.
func textUnits(t TaskScopeInput) []string {
	units := make([]string, 0, 2+len(t.AcceptanceCriteria))
	units = append(units, t.Title)
	units = append(units, splitSentences(t.Description)...)
	for _, c := range t.AcceptanceCriteria {
		units = append(units, splitSentences(c)...)
	}
	return units
}

var sentenceSplitRe = regexp.MustCompile(`(?:\.\s+|;\s*|\n+)`)

func splitSentences(s string) []string {
	out := []string{}
	for _, part := range sentenceSplitRe.Split(s, -1) {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}

// writeVerbs are the verbs that turn a path mentioned nearby into a write.
// Deliberately a closed list rather than "any verb": the cost of a false
// write is a plan serialized for no reason.
var writeVerbs = map[string]bool{
	"add": true, "adds": true, "adding": true,
	"create": true, "creates": true, "creating": true,
	"extend": true, "extends": true, "extending": true,
	"implement": true, "implements": true, "implementing": true,
	"introduce": true, "introduces": true, "introducing": true,
	"modify": true, "modifies": true, "modifying": true,
	"update": true, "updates": true, "updating": true,
	"change": true, "changes": true, "changing": true,
	"write": true, "writes": true, "writing": true,
	"persist": true, "persists": true, "persisting": true,
	"store": true, "stores": true, "storing": true,
	"rename": true, "renames": true, "renaming": true,
	"delete": true, "deletes": true, "deleting": true,
	"remove": true, "removes": true, "removing": true,
	"refactor": true, "refactors": true, "refactoring": true,
	"replace": true, "replaces": true, "replacing": true,
	"wire": true, "wires": true, "wiring": true,
	"register": true, "registers": true, "registering": true,
	"migrate": true, "migrates": true, "migrating": true,
	"generate": true, "generates": true, "generating": true,
	"emit": true, "emits": true, "emitting": true,
	"record": true, "records": true, "recording": true,
	"insert": true, "inserts": true, "inserting": true,
	"rewrite": true, "rewrites": true, "rewriting": true,
	"expose": true, "exposes": true, "exposing": true,
	"define": true, "defines": true, "defining": true,
}

var wordRe = regexp.MustCompile(`[A-Za-z]+`)

func mentionsWriteIntent(unit string) bool {
	for _, w := range wordRe.FindAllString(strings.ToLower(unit), -1) {
		if writeVerbs[w] {
			return true
		}
	}
	return false
}

// codeExtensions is what makes a token a file rather than a directory or an
// English word with a period stuck to it.
var codeExtensions = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".sql": true, ".md": true, ".json": true, ".yaml": true, ".yml": true,
	".sh": true, ".css": true, ".html": true, ".proto": true, ".toml": true,
	".mod": true, ".sum": true, ".tmpl": true, ".txt": true,
}

// englishSlashWords are the segments that make a slashed token a phrase
// ("read/write", "and/or") rather than a path.
var englishSlashWords = map[string]bool{
	"and": true, "or": true, "not": true, "read": true, "write": true,
	"in": true, "out": true, "on": true, "off": true, "yes": true, "no": true,
	"input": true, "output": true, "true": true, "false": true, "he": true,
	"she": true, "they": true, "his": true, "her": true, "them": true,
	"pr": true, "mr": true, "ci": true, "cd": true, "a": true, "b": true,
}

var pathTokenRe = regexp.MustCompile(`[A-Za-z0-9_.+-]+(?:/[A-Za-z0-9_.+-]+)+/?|[A-Za-z0-9_+-]+\.[A-Za-z0-9]+`)

// extractPaths pulls the repository paths out of one unit of text. Everything
// it is unsure about it drops: an over-eager path is a phantom write, and a
// phantom write is a phantom conflict.
func extractPaths(text string, roots []string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, tok := range pathTokenRe.FindAllString(text, -1) {
		if p, ok := normalizePath(tok, roots); ok && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// normalizePath canonicalizes one candidate token and decides whether it is a
// repository path at all.
func normalizePath(tok string, roots []string) (string, bool) {
	tok = strings.TrimSpace(tok)
	tok = strings.Trim(tok, "`'\"()[]{}<>,;:")
	tok = strings.TrimSuffix(tok, ".")
	tok = strings.TrimPrefix(tok, "./")
	tok = strings.TrimSuffix(tok, "/")
	if tok == "" || tok == "." || strings.Contains(tok, "://") || strings.HasPrefix(tok, "/") || strings.Contains(tok, "..") {
		return "", false
	}
	segments := strings.Split(tok, "/")
	for _, s := range segments {
		if s == "" {
			return "", false
		}
	}
	if isFilePath(tok) {
		// A bare filename with a known code extension is a path even without a
		// directory ("go.mod", "AGENTS.md").
		return tok, true
	}
	if len(segments) < 2 {
		// A single-segment directory ("backend") is a component, not an
		// actionable scope: admitting it would make every task in the repo
		// conflict with every other.
		return "", false
	}
	for _, s := range segments {
		if englishSlashWords[strings.ToLower(s)] {
			return "", false
		}
	}
	if len(segments) >= 3 || matchesRoot(tok, roots) {
		return tok, true
	}
	return "", false
}

func matchesRoot(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

func isFilePath(p string) bool {
	return codeExtensions[strings.ToLower(path.Ext(p))]
}

// componentFor is the coarse component a path belongs to: the longest known
// repository root that contains it, or its own first segment when no root
// matches.
func componentFor(p string, roots []string) string {
	best := ""
	for _, r := range roots {
		if (p == r || strings.HasPrefix(p, r+"/")) && len(r) > len(best) {
			best = r
		}
	}
	if best != "" {
		return best
	}
	if i := strings.Index(p, "/"); i > 0 {
		return p[:i]
	}
	if isFilePath(p) {
		// A repository-root file ("AGENTS.md") belongs to no component.
		return ""
	}
	return p
}

var camelSymbolRe = regexp.MustCompile(`\b[A-Z][a-z0-9]+(?:[A-Z][a-z0-9]*)+\b`)
var callSymbolRe = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\(\)`)

// extractSymbols pulls the code identifiers a task names explicitly. Only two
// unambiguous shapes are admitted -- CamelCase identifiers and call syntax --
// because a symbol list that fills up with ordinary prose is worse than an
// empty one.
func extractSymbols(t TaskScopeInput) []string {
	out := map[string]bool{}
	for _, unit := range textUnits(t) {
		for _, m := range callSymbolRe.FindAllString(unit, -1) {
			out[strings.TrimSuffix(m, "()")] = true
		}
		for _, m := range camelSymbolRe.FindAllString(unit, -1) {
			if isFilePath(m) {
				continue
			}
			out[m] = true
		}
	}
	return sortedKeys(out)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func normalizeStrings(in []string) []string {
	seen := map[string]bool{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			seen[s] = true
		}
	}
	return sortedKeys(seen)
}

// MarshalTaskScope serializes a scope for workflow_tasks.scope_json. Every
// slice is non-nil first, so an empty scope round-trips as "[]" rather than
// "null" and two identical estimates produce identical JSON.
func MarshalTaskScope(scope domain.WorkflowTaskScope) (string, error) {
	if scope.Version == "" {
		scope.Version = TaskGraphPolicyVersion
	}
	if scope.Source == "" {
		scope.Source = domain.WorkflowTaskScopeEstimated
	}
	for _, p := range []*[]string{
		&scope.ReadPaths, &scope.WritePaths, &scope.Packages, &scope.Components,
		&scope.Files, &scope.Symbols, &scope.IntegrationDependencies, &scope.ObservedWritePaths,
	} {
		if *p == nil {
			*p = []string{}
		}
	}
	if scope.SafeWriteOverlaps == nil {
		scope.SafeWriteOverlaps = []domain.WorkflowTaskSafeOverlap{}
	}
	for i := range scope.SafeWriteOverlaps {
		if scope.SafeWriteOverlaps[i].Paths == nil {
			scope.SafeWriteOverlaps[i].Paths = []string{}
		}
	}
	b, err := json.Marshal(scope)
	if err != nil {
		return "", fmt.Errorf("marshal task scope: %w", err)
	}
	return string(b), nil
}

// UnmarshalTaskScope parses a task's scope_json column. An empty column or
// "{}" yields the zero value with no error -- a task planned before this model
// existed simply has no scope, which every reader must tolerate.
func UnmarshalTaskScope(raw string) (domain.WorkflowTaskScope, error) {
	var scope domain.WorkflowTaskScope
	if strings.TrimSpace(raw) == "" {
		return scope, nil
	}
	if err := json.Unmarshal([]byte(raw), &scope); err != nil {
		return domain.WorkflowTaskScope{}, fmt.Errorf("unmarshal task scope: %w", err)
	}
	return scope, nil
}
