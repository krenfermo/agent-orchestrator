package contextrouter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	memory "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// ErrRequest is the sentinel every rejected Select or Expand request wraps.
var ErrRequest = errors.New("contextrouter: invalid request")

// roleSectionOrder is the per-role packing order — the router's role-awareness
// in one table.
//
// A planner reads documents (AGENTS.md, briefs, conventions) before it reads a
// diff, because it is deciding what to do rather than continuing something. A
// worker and a reviewer start from the change itself and its impacted symbols.
// A verify dispatch runs a command, so a document it will not read must never
// crowd out the change list that tells it what to verify.
//
// The task section leads every role: a payload that dropped it would be
// smaller and useless.
var roleSectionOrder = map[Role][]SectionKind{
	RolePlanner:  {SectionTask, SectionDocument, SectionMemory, SectionGraph, SectionDiff},
	RoleWorker:   {SectionTask, SectionDiff, SectionGraph, SectionDocument, SectionMemory},
	RoleReviewer: {SectionTask, SectionDiff, SectionGraph, SectionMemory, SectionDocument},
	RoleFix:      {SectionTask, SectionDiff, SectionGraph, SectionMemory, SectionDocument},
	RoleVerify:   {SectionTask, SectionDiff, SectionDocument, SectionGraph, SectionMemory},
}

// priorityStride spaces the per-kind priorities so sections of one kind keep
// the order their source produced them in (documents in the order the caller
// supplied, changed files in diff order) without ever overtaking the next kind.
const priorityStride = 1000

// tierLimits is how much each retrieval tier asks its sources for. They are
// retrieval limits, not budget limits: the budget still packs whatever these
// produce, and the point of the compact tier is to not pay for retrieval whose
// result the budget would immediately drop.
type tierLimits struct {
	diffFiles           int
	graphFiles          int
	graphSymbolsPerFile int
	graphEdges          int
	memoryItems         int
	memoryItemTokens    int
	documentTokens      int
}

// compactLimits retrieves the outline: what changed, the symbols in the few
// most relevant changed files, a handful of memory facts, and the head of each
// document.
var compactLimits = tierLimits{
	diffFiles:           25,
	graphFiles:          5,
	graphSymbolsPerFile: 8,
	graphEdges:          0,
	memoryItems:         5,
	memoryItemTokens:    120,
	documentTokens:      500,
}

// expandedLimits retrieves the detail: the whole change set, symbols and edges
// for many more files, more memory, and whole documents.
var expandedLimits = tierLimits{
	diffFiles:           200,
	graphFiles:          25,
	graphSymbolsPerFile: 40,
	graphEdges:          40,
	memoryItems:         25,
	memoryItemTokens:    400,
	documentTokens:      0, // 0 means "no per-document cap"; the budget still bounds it
}

func limitsFor(tier Tier) tierLimits {
	if tier == TierExpanded {
		return expandedLimits
	}
	return compactLimits
}

// documentShare and memoryShare are the largest fraction of a tier's token
// limit that one document, respectively one memory item, may claim before it
// is capped.
//
// They are what makes retrieval itself role-aware rather than only the packing
// that follows it. Without them a small-budget role retrieves the same
// full-size document a planner does and then throws most of it away, which
// costs the same work and produces a payload whose size is decided by the
// retrieval constants rather than by the role's budget.
const (
	documentShare = 4
	memoryShare   = 8
)

// scaleLimits narrows a tier's per-item retrieval caps to what the role's
// token limit can actually hold. A tier cap of zero means "uncapped by the
// tier", and the role's share still applies: no single item may crowd out
// every other kind of evidence, whatever tier it was retrieved at.
func scaleLimits(limits tierLimits, limit int) tierLimits {
	limits.documentTokens = shareOf(limits.documentTokens, limit, documentShare)
	limits.memoryItemTokens = shareOf(limits.memoryItemTokens, limit, memoryShare)
	return limits
}

// shareOf returns the smaller of a tier cap and the role's share of the limit,
// never below minSectionTokens — a cap under which every item would be cut to
// a useless fragment is not a smaller payload, it is a payload of headers.
func shareOf(tierCap, limit, share int) int {
	roleCap := limit / share
	if roleCap < minSectionTokens {
		roleCap = minSectionTokens
	}
	if tierCap <= 0 || roleCap < tierCap {
		return roleCap
	}
	return tierCap
}

// Options configure a Router. Every source is optional: a router with no code
// graph still routes on the diff, the documents, and memory, and says in its
// notes which source it did not have. Refusing to assemble anything because
// one source is absent would make routing strictly worse than the full-context
// path it replaces.
type Options struct {
	// Budgets is the per-role budget table. Nil uses DefaultBudgets.
	Budgets BudgetSet
	// Diff reports the current change set. Nil disables diff evidence.
	Diff DiffSource
	// Graph answers impacted-symbol queries. Nil disables graph evidence.
	Graph GraphQuerier
	// Memory reads durable project memory. Nil disables memory evidence.
	Memory MemorySource
	// Log receives source failures. Nil logs nothing.
	Log *slog.Logger
}

// Router assembles bounded, role-aware context payloads. It is stateless
// beyond its configuration and is safe for concurrent use as long as its
// sources are.
type Router struct {
	budgets BudgetSet
	diff    DiffSource
	graph   GraphQuerier
	memory  MemorySource
	log     *slog.Logger
}

// New validates the options and returns a router. An invalid budget table is
// an error: a router that silently fell back to defaults would make an
// operator's override look applied when it was not.
func New(opts Options) (*Router, error) {
	budgets := opts.Budgets
	if budgets == nil {
		budgets = DefaultBudgets()
	} else {
		budgets = budgets.Clone()
	}
	if err := budgets.Validate(); err != nil {
		return nil, err
	}
	return &Router{
		budgets: budgets,
		diff:    opts.Diff,
		graph:   opts.Graph,
		memory:  opts.Memory,
		log:     opts.Log,
	}, nil
}

// Budgets returns the table in force, as an independent copy.
func (r *Router) Budgets() BudgetSet { return r.budgets.Clone() }

// BudgetFor returns one role's budget.
func (r *Router) BudgetFor(role Role) Budget { return r.budgets.For(role) }

// Select performs the compact retrieval pass: it assembles the ordered context
// sections for a role, a task, and a project, packs them against that role's
// compact target, and reports the estimated size against the budget.
//
// It never exceeds the role's hard cap, and it never returns an unusable
// payload because a source failed — a failure becomes a note and the selection
// is marked insufficient, which is what a caller tests before calling Expand.
func (r *Router) Select(ctx context.Context, req Request) (Selection, error) {
	return r.assemble(ctx, req, TierCompact)
}

// Expand performs the second, deeper retrieval pass for a selection whose
// evidence was not sufficient.
//
// It is deliberately a separate call rather than something Select does on its
// own: expansion costs retrieval and budget, so the decision to pay for it
// belongs to the caller, and the condition that justifies it
// (Selection.EvidenceSufficient) is reported rather than guessed at.
//
// Three things it will not do. It will not expand an already-expanded
// selection. It will not expand a sufficient one unless Request.ForceExpand
// says so. And it will not exceed the role's hard cap — forcing expansion buys
// retrieval depth, never budget.
func (r *Router) Expand(ctx context.Context, req Request, prior Selection) (Selection, error) {
	if !req.Role.Valid() {
		return Selection{}, fmt.Errorf("%w: unknown role %q", ErrRequest, req.Role)
	}
	if prior.Role != "" && prior.Role != req.Role {
		return Selection{}, fmt.Errorf("%w: prior selection is for role %q, not %q", ErrRequest, prior.Role, req.Role)
	}
	if prior.Tier == TierExpanded {
		prior.Notes = append(prior.Notes, "expansion skipped: the selection is already expanded")
		prior.Expandable = false
		return prior, nil
	}
	if prior.EvidenceSufficient && !req.ForceExpand {
		prior.Notes = append(prior.Notes, "expansion skipped: the compact selection already carries the evidence its sources could offer")
		prior.Expandable = false
		return prior, nil
	}
	return r.assemble(ctx, req, TierExpanded)
}

// assemble is the one path both Select and Expand run through, so the cap is
// enforced in exactly one place.
func (r *Router) assemble(ctx context.Context, req Request, tier Tier) (Selection, error) {
	if !req.Role.Valid() {
		return Selection{}, fmt.Errorf("%w: unknown role %q", ErrRequest, req.Role)
	}
	if strings.TrimSpace(req.Task.Objective) == "" && strings.TrimSpace(req.Task.Title) == "" {
		return Selection{}, fmt.Errorf("%w: a task needs an objective or a title", ErrRequest)
	}
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}

	budget := r.budgets.For(req.Role)
	limit := budget.LimitFor(tier)
	limits := scaleLimits(limitsFor(tier), limit)
	order := sectionOrderFor(req.Role)

	candidates := []Section{taskSection(req, order, tier)}
	candidates = append(candidates, documentSections(req, order, tier, limits)...)

	var notes []string
	var failures int

	changes, diffNote, diffFailed := r.gatherDiff(ctx, req)
	if diffNote != "" {
		notes = append(notes, diffNote)
	}
	if diffFailed {
		failures++
	}
	if section, ok := diffSection(changes, order, tier, limits); ok {
		candidates = append(candidates, section)
	}

	graphSections, graphNote, graphFailed := r.gatherGraph(ctx, req, changes, order, tier, limits)
	if graphNote != "" {
		notes = append(notes, graphNote)
	}
	if graphFailed {
		failures++
	}
	candidates = append(candidates, graphSections...)

	memorySections, memoryNote, memoryFailed := r.gatherMemory(req, changes, order, tier, limits)
	if memoryNote != "" {
		notes = append(notes, memoryNote)
	}
	if memoryFailed {
		failures++
	}
	candidates = append(candidates, memorySections...)

	sortSections(candidates)
	packed := pack(candidates, limit)

	selection := Selection{
		Role:            req.Role,
		Project:         req.Project.ID,
		Tier:            tier,
		Sections:        packed.sections,
		Dropped:         packed.dropped,
		EstimatedTokens: packed.tokens,
		EstimatedBytes:  packed.bytes,
		Budget:          budget,
		Limit:           limit,
		Notes:           notes,
	}
	selection.EvidenceSufficient = failures == 0 && len(packed.dropped) == 0 && !packed.truncated
	selection.Expandable = tier == TierCompact && !selection.EvidenceSufficient

	if !selection.WithinBudget() {
		// Unreachable while pack honours its target, which is already clamped
		// to the cap by Budget.LimitFor. It is asserted anyway because "the
		// hard cap is never exceeded" is the guarantee this package sells, and
		// a guarantee that is only checked in tests is a guarantee that
		// eventually is not one.
		return Selection{}, fmt.Errorf("contextrouter: assembled %d tokens for role %q above its hard cap of %d", selection.EstimatedTokens, req.Role, budget.HardCapTokens)
	}
	return selection, nil
}

// sectionOrderFor returns the kind ordering for a role, falling back to the
// worker ordering for a role with no entry (which Role.Valid already prevents).
func sectionOrderFor(role Role) map[SectionKind]int {
	kinds, ok := roleSectionOrder[role]
	if !ok {
		kinds = roleSectionOrder[RoleWorker]
	}
	out := make(map[SectionKind]int, len(kinds))
	for i, kind := range kinds {
		out[kind] = i * priorityStride
	}
	return out
}

// taskSection is the mandatory statement of what is being asked.
func taskSection(req Request, order map[SectionKind]int, tier Tier) Section {
	var b strings.Builder
	if id := strings.TrimSpace(req.Task.ID); id != "" {
		fmt.Fprintf(&b, "task: %s\n", id)
	}
	if title := strings.TrimSpace(req.Task.Title); title != "" {
		fmt.Fprintf(&b, "title: %s\n", title)
	}
	if objective := strings.TrimSpace(req.Task.Objective); objective != "" {
		fmt.Fprintf(&b, "objective:\n%s\n", objective)
	}
	if len(req.Task.Paths) > 0 {
		fmt.Fprintf(&b, "paths of interest: %s\n", strings.Join(req.Task.Paths, ", "))
	}
	if len(req.Task.Symbols) > 0 {
		fmt.Fprintf(&b, "symbols of interest: %s\n", strings.Join(req.Task.Symbols, ", "))
	}
	return newSection(SectionTask, "Task", string(req.Role)+" dispatch", b.String(), order[SectionTask], tier)
}

// documentSections turn the caller's already-assembled documents into
// candidates. Today's dispatch sends all of them in full; here they are capped
// per document at the compact tier and then packed like everything else, so a
// large document competes with the evidence rather than crowding it out
// invisibly.
func documentSections(req Request, order map[SectionKind]int, tier Tier, limits tierLimits) []Section {
	out := make([]Section, 0, len(req.Documents))
	for i, doc := range req.Documents {
		content := doc.Content
		if strings.TrimSpace(content) == "" {
			continue
		}
		truncated := false
		if limits.documentTokens > 0 {
			content, truncated = truncateToTokens(content, limits.documentTokens)
		}
		title := strings.TrimSpace(doc.Path)
		if title == "" {
			title = fmt.Sprintf("document %d", i+1)
		}
		section := newSection(SectionDocument, title, "caller document", content, order[SectionDocument]+i, tier)
		section.Truncated = truncated
		out = append(out, section)
	}
	return out
}

// gatherDiff asks the diff source what changed. A missing source and a failing
// source are different answers: the first is a configuration the operator
// chose, the second is evidence the router expected and did not get, so only
// the second counts as a failure that makes the selection insufficient.
func (r *Router) gatherDiff(ctx context.Context, req Request) (changes []codegraph.FileChange, note string, failed bool) {
	if r.diff == nil {
		return nil, "diff evidence unavailable: no diff source is configured", false
	}
	if strings.TrimSpace(req.Project.Root) == "" {
		// The production diff source runs git in the checkout and refuses an
		// empty root. Saying so here — rather than letting the source reject
		// the call — keeps a request that simply has no checkout from looking
		// like a retrieval that failed, which would mark the selection
		// insufficient and buy an expansion that cannot help.
		return nil, "diff evidence unavailable: the request carries no project root", false
	}
	diff, err := r.diff.Changes(ctx, req.Project)
	if err != nil {
		r.warn("contextrouter: diff source failed", "project", req.Project.ID, "err", err)
		return nil, "diff evidence unavailable: " + err.Error(), true
	}
	out := append([]codegraph.FileChange(nil), diff.Changes...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, "", false
}

// diffSection renders the change set. It is one section rather than one per
// file because the list is the anchor a routed payload is built around: an
// agent that gets half a change list does not know it got half.
func diffSection(changes []codegraph.FileChange, order map[SectionKind]int, tier Tier, limits tierLimits) (Section, bool) {
	if len(changes) == 0 {
		return Section{}, false
	}
	shown := changes
	omitted := 0
	if limits.diffFiles > 0 && len(shown) > limits.diffFiles {
		omitted = len(shown) - limits.diffFiles
		shown = shown[:limits.diffFiles]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d changed file(s) against the diff base.\n", len(changes))
	for _, change := range shown {
		if change.Status == codegraph.ChangeRenamed && change.OldPath != "" {
			fmt.Fprintf(&b, "- %s: %s -> %s\n", change.Status, change.OldPath, change.Path)
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", change.Status, change.Path)
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "- … %d more file(s) not listed at the %s retrieval tier\n", omitted, tier)
	}
	section := newSection(SectionDiff, "Changed files", "git diff", b.String(), order[SectionDiff], tier)
	section.Truncated = omitted > 0
	return section, true
}

// gatherGraph queries the code graph for the symbols the change touches, one
// section per file so the packer can shed the least relevant files first
// rather than the whole graph at once.
func (r *Router) gatherGraph(ctx context.Context, req Request, changes []codegraph.FileChange, order map[SectionKind]int, tier Tier, limits tierLimits) (sections []Section, note string, failed bool) {
	if r.graph == nil {
		return nil, "graph evidence unavailable: no code graph is configured", false
	}
	root := strings.TrimSpace(req.Project.Root)
	if root == "" {
		return nil, "graph evidence unavailable: the request carries no project root", false
	}

	files := impactedFiles(changes, req.Task.Paths, limits.graphFiles)
	var errs []string
	rank := 0
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return sections, "graph evidence truncated: " + err.Error(), true
		}
		result, err := r.graph.Query(ctx, codegraph.QueryRequest{ProjectRoot: root, File: rel, Limit: limits.graphSymbolsPerFile})
		if err != nil {
			if errors.Is(err, codegraph.ErrNotIndexed) {
				return sections, "graph evidence unavailable: the project has not been indexed", false
			}
			errs = append(errs, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		if content := renderGraphResult(result, limits); content != "" {
			sections = append(sections, newSection(SectionGraph, "Impacted symbols — "+rel, "code graph", content, order[SectionGraph]+rank, tier))
			rank++
		}
	}
	for _, symbol := range dedupeStrings(req.Task.Symbols) {
		if err := ctx.Err(); err != nil {
			return sections, "graph evidence truncated: " + err.Error(), true
		}
		result, err := r.graph.Query(ctx, codegraph.QueryRequest{ProjectRoot: root, Symbol: symbol, Limit: limits.graphSymbolsPerFile})
		if err != nil {
			if errors.Is(err, codegraph.ErrNotIndexed) {
				continue
			}
			errs = append(errs, fmt.Sprintf("symbol %s: %v", symbol, err))
			continue
		}
		if content := renderGraphResult(result, limits); content != "" {
			sections = append(sections, newSection(SectionGraph, "Symbol — "+symbol, "code graph", content, order[SectionGraph]+rank, tier))
			rank++
		}
	}
	if len(errs) > 0 {
		r.warn("contextrouter: code graph query failed", "project", req.Project.ID, "err", strings.Join(errs, "; "))
		return sections, "graph evidence partial: " + strings.Join(errs, "; "), true
	}
	return sections, "", false
}

// impactedFiles is the query list for the graph: the changed paths first, in
// diff order, then any path the caller nominated that the diff did not already
// name.
func impactedFiles(changes []codegraph.FileChange, taskPaths []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(changes)+len(taskPaths))
	add := func(rel string) {
		rel = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(rel)), "./")
		if rel == "" || seen[rel] {
			return
		}
		seen[rel] = true
		out = append(out, rel)
	}
	for _, change := range changes {
		if change.Status == codegraph.ChangeDeleted {
			// A deleted file has no symbols left to impact; its disappearance
			// is already reported by the diff section.
			continue
		}
		add(change.Path)
	}
	for _, rel := range taskPaths {
		add(rel)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// renderGraphResult writes the symbols a query matched, and — at the expanded
// tier only — the edges around them. Edges are the expensive half: they are
// what an agent needs to understand blast radius, and what a compact pass can
// almost always do without.
func renderGraphResult(result codegraph.QueryResult, limits tierLimits) string {
	var b strings.Builder
	for _, sym := range result.Symbols {
		fmt.Fprintf(&b, "- %s %s (%s:%d)\n", sym.Kind, sym.Name, sym.File, sym.Line)
	}
	if limits.graphEdges > 0 {
		written := 0
		for _, edge := range result.Outgoing {
			if written >= limits.graphEdges {
				break
			}
			fmt.Fprintf(&b, "- %s -> %s [%s]\n", edge.From, edge.To, edge.Kind)
			written++
		}
		for _, edge := range result.Incoming {
			if written >= limits.graphEdges {
				break
			}
			fmt.Fprintf(&b, "- %s <- %s [%s]\n", edge.To, edge.From, edge.Kind)
			written++
		}
	}
	return b.String()
}

// gatherMemory ranks the project's durable memory against this task and turns
// the top items into sections, one per item so the packer sheds the least
// relevant facts first.
func (r *Router) gatherMemory(req Request, changes []codegraph.FileChange, order map[SectionKind]int, tier Tier, limits tierLimits) (sections []Section, note string, failed bool) {
	if r.memory == nil {
		return nil, "memory evidence unavailable: no project memory is configured", false
	}
	project := strings.TrimSpace(req.Project.ID)
	if project == "" {
		return nil, "memory evidence unavailable: the request carries no project id", false
	}
	items, err := r.memory.List(project)
	if err != nil {
		r.warn("contextrouter: project memory read failed", "project", project, "err", err)
		return nil, "memory evidence unavailable: " + err.Error(), true
	}

	touched := map[string]bool{}
	for _, change := range changes {
		touched[change.Path] = true
		if change.OldPath != "" {
			touched[change.OldPath] = true
		}
	}
	for _, rel := range req.Task.Paths {
		touched[strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(rel)), "./")] = true
	}

	ranked := make([]rankedItem, 0, len(items))
	stale := 0
	for _, item := range items {
		if item.Stale {
			// A stale item's provenance no longer holds; serving it as current
			// context is exactly what projectmemory marks it to prevent.
			stale++
			continue
		}
		ranked = append(ranked, rankedItem{item: item, score: memoryScore(item, touched, req.Role)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.item.Confidence != b.item.Confidence {
			return a.item.Confidence > b.item.Confidence
		}
		if !a.item.UpdatedAt.Equal(b.item.UpdatedAt) {
			return a.item.UpdatedAt.After(b.item.UpdatedAt)
		}
		return a.item.ID < b.item.ID
	})

	omitted := 0
	if limits.memoryItems > 0 && len(ranked) > limits.memoryItems {
		omitted = len(ranked) - limits.memoryItems
		ranked = ranked[:limits.memoryItems]
	}
	for i, entry := range ranked {
		content := entry.item.Content
		truncated := false
		if limits.memoryItemTokens > 0 {
			content, truncated = truncateToTokens(content, limits.memoryItemTokens)
		}
		title := string(entry.item.Type)
		if scope := strings.TrimSpace(entry.item.Scope); scope != "" {
			title += " — " + scope
		}
		source := fmt.Sprintf("project memory %s (confidence %.2f)", entry.item.Source.Kind, entry.item.Confidence)
		section := newSection(SectionMemory, title, source, content, order[SectionMemory]+i, tier)
		section.Truncated = truncated
		sections = append(sections, section)
	}

	var notes []string
	if stale > 0 {
		notes = append(notes, fmt.Sprintf("%d stale memory item(s) withheld", stale))
	}
	if omitted > 0 {
		notes = append(notes, fmt.Sprintf("%d memory item(s) beyond the %s retrieval tier", omitted, tier))
	}
	return sections, strings.Join(notes, "; "), false
}

// rankedItem pairs a memory item with its relevance score for this task.
type rankedItem struct {
	item  memory.MemoryItem
	score int
}

// memoryScore ranks a memory item against the task at hand. It is a small,
// explicit heuristic rather than a similarity model: a fact about a file this
// change touches outranks a fact about the role, which outranks a fact about
// the project in general.
func memoryScore(item memory.MemoryItem, touched map[string]bool, role Role) int {
	score := 0
	if path := strings.TrimSpace(item.Source.Path); path != "" && touched[path] {
		score += 4
	}
	if scope := strings.TrimSpace(item.Scope); scope != "" {
		if touched[scope] {
			score += 2
		}
		if strings.EqualFold(scope, string(role)) {
			score++
		}
	}
	if item.SourceCommit != "" {
		// Provenance strong enough to invalidate is provenance strong enough
		// to prefer: an item that names the commit it was derived at can be
		// re-checked, and one that cannot is a claim with no way back to its
		// evidence.
		score++
	}
	return score
}

// newSection builds a section with its size already measured.
func newSection(kind SectionKind, title, source, content string, priority int, tier Tier) Section {
	content = strings.TrimRight(content, "\n")
	return Section{
		Kind:            kind,
		Title:           title,
		Source:          source,
		Content:         content,
		Priority:        priority,
		Tier:            tier,
		EstimatedTokens: estimateTokens(content),
		Bytes:           len(content),
	}
}

// packResult is what the packer produced.
type packResult struct {
	sections  []Section
	dropped   []Dropped
	tokens    int
	bytes     int
	truncated bool
}

// pack fits candidates into a token target, in priority order.
//
// The rules, in the order they apply:
//
//   - The task section is mandatory. It is packed first and, if the target is
//     somehow smaller than it, truncated to the target rather than dropped.
//   - A section that fits whole goes in whole.
//   - A section that does not fit is truncated into the remaining room, but
//     only if that room is worth using (minSectionTokens). Below that it is
//     dropped, because a section reduced to its header teaches nothing and
//     still costs budget.
//   - Everything dropped is reported, with the size it would have needed.
//
// The target is never exceeded. Callers get it already clamped to the role's
// hard cap by Budget.LimitFor, which is why this function needs no cap of its
// own.
func pack(candidates []Section, target int) packResult {
	result := packResult{}
	used := 0
	for _, section := range candidates {
		mandatory := section.Kind == SectionTask
		remaining := target - used
		switch {
		case section.EstimatedTokens <= remaining:
			// fits whole
		case mandatory || remaining >= minSectionTokens:
			content, cut := truncateToTokens(section.Content, remaining)
			if strings.TrimSpace(content) == "" {
				result.dropped = append(result.dropped, Dropped{
					Kind:            section.Kind,
					Title:           section.Title,
					Reason:          fmt.Sprintf("no budget left: needed %d token(s), %d remained", section.EstimatedTokens, remaining),
					EstimatedTokens: section.EstimatedTokens,
				})
				continue
			}
			section.Content = content
			section.Truncated = section.Truncated || cut
			section.EstimatedTokens = estimateTokens(content)
			section.Bytes = len(content)
		default:
			result.dropped = append(result.dropped, Dropped{
				Kind:            section.Kind,
				Title:           section.Title,
				Reason:          fmt.Sprintf("over budget: needed %d token(s), %d remained", section.EstimatedTokens, remaining),
				EstimatedTokens: section.EstimatedTokens,
			})
			continue
		}
		used += section.EstimatedTokens
		result.bytes += section.Bytes
		result.truncated = result.truncated || section.Truncated
		result.sections = append(result.sections, section)
	}
	result.tokens = used
	return result
}

// dedupeStrings trims, drops empties, and removes duplicates while preserving
// order.
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		value := strings.TrimSpace(raw)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (r *Router) warn(msg string, args ...any) {
	if r.log != nil {
		r.log.Warn(msg, args...)
	}
}
