package projectmemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// pack.go — MemoryContextPack: what a role is actually handed.
//
// Four properties are non-negotiable, and every design choice below follows
// from one of them:
//
//   - **Bounded.** A pack has a byte budget and an item budget, both enforced
//     here. Never send the whole memory. A pack that does not fit degrades by
//     dropping the lowest-ranked facts and by falling back from body to
//     summary, and it records what it dropped.
//   - **Deterministic.** The same store, the same request and the same budget
//     produce the same bytes, and Digest proves it. Determinism is what makes
//     the pack auditable: two dispatches that were given the same memory can
//     be shown to have been.
//   - **Fail closed.** Only facts AO can currently vouch for are served. A
//     stale, invalidated or rebuilding fact never enters a pack, a repository
//     with no completed index produces an empty pack with a stated reason, and
//     a task's unintegrated memory reaches only that task.
//   - **Provider-neutral.** Nothing here knows about Claude, Codex or any
//     other provider. Render produces one canonical text; an adapter may
//     reformat it, and may not change what it says.
//
// Selection is relevance + scope + freshness + confidence, in that priority
// order, with the derived item id as the final tiebreak so ties can never be
// resolved by map iteration order.

// PackRole is the AO role a pack is assembled for. It is this package's own
// vocabulary rather than a workflow type, so nothing about memory selection
// depends on the workflow package's evolution.
type PackRole string

// Pack roles. They mirror the four roles the P2-A brief names.
const (
	RolePlanner  PackRole = "planner"
	RoleWorker   PackRole = "worker"
	RoleReviewer PackRole = "reviewer"
	RoleRepair   PackRole = "repair"
)

// Valid reports whether the role is one this build assembles packs for.
func (r PackRole) Valid() bool {
	switch r {
	case RolePlanner, RoleWorker, RoleReviewer, RoleRepair:
		return true
	default:
		return false
	}
}

// PackBudget bounds one pack.
type PackBudget struct {
	// MaxBytes caps the rendered pack. Zero means DefaultPackBytes.
	MaxBytes int
	// MaxItems caps how many facts it carries. Zero means DefaultPackItems.
	MaxItems int
}

// Pack bounds. They are small on purpose: a memory pack competes for the same
// context window as the task, the diff and the agent's own reads, and a pack
// that crowds those out has made the agent worse, not better.
const (
	// DefaultPackBytes is roughly six thousand tokens at the four-bytes-per-
	// token estimate the context router already uses.
	DefaultPackBytes = 24 * 1024
	// DefaultPackItems caps the fact count independently of size, so a pack
	// cannot become forty one-line facts an agent will not read.
	DefaultPackItems = 40
	// packBytesPerToken matches internal/contextrouter's estimator, so the two
	// budgeting surfaces report comparable numbers.
	packBytesPerToken = 4
)

// Normalized fills in the defaults.
func (b PackBudget) Normalized() PackBudget {
	if b.MaxBytes <= 0 {
		b.MaxBytes = DefaultPackBytes
	}
	if b.MaxItems <= 0 {
		b.MaxItems = DefaultPackItems
	}
	return b
}

// PackRequest asks for one role-scoped pack.
type PackRequest struct {
	ProjectID domain.ProjectID
	// RepoPath is the repository the work is in. It may be empty for a
	// Planner pack, which spans every repository of the project.
	RepoPath string
	// Role decides the section order and which types are eligible.
	Role PackRole
	// ChangedPaths are the repo-relative paths this piece of work touches, if
	// known. They are the strongest relevance signal there is: a Reviewer
	// looking at three files wants the facts about those three files.
	ChangedPaths []string
	// Modules narrows relevance to named modules when the caller knows them
	// and the changed paths are not yet available (a Worker at spawn time).
	Modules []string
	// Keywords are free-text terms from the objective or the findings. They
	// are the weakest signal and are used only to break ties among facts that
	// are otherwise equally relevant.
	Keywords []string
	// TaskRef, when set, admits that task's own unintegrated memory into the
	// pack — and only that task's. It is how a Repair Agent sees what the
	// task before it did without any other task's uncommitted view leaking in.
	TaskRef string
	// Budget bounds the result.
	Budget PackBudget
}

// SelectedItem is one fact that made it into a pack, with the score that put
// it there. The score is carried so an operator can ask why a pack contains
// what it contains, which is the difference between a selector and a black box.
type SelectedItem struct {
	Item domain.ProjectMemoryItem
	// Score is the relevance score, in the order selection ranked by.
	Score float64
	// Reason names the strongest signal that selected this fact.
	Reason string
	// BodyIncluded reports whether the pack could afford the item's body. A
	// false value means the summary alone is present.
	BodyIncluded bool
}

// PackSection groups selected facts under a heading, in the role's order.
type PackSection struct {
	Title string
	Type  domain.ProjectMemoryType
	Items []SelectedItem
}

// PackStats is the measurement P2-B needs, and only what AO can actually
// observe.
//
// The honesty rule from the brief applies here: these numbers describe the
// AO-assembled memory pack. They say nothing about what the coding harness
// reads inside the worktree, because AO cannot see that
// (docs/p2-project-memory-audit.md §1), and no field here should ever be read
// as "reads avoided".
type PackStats struct {
	Role PackRole
	// CandidateItems and CandidateBytes describe everything selection was
	// allowed to choose from, before the budget.
	CandidateItems int
	CandidateBytes int
	// SelectedItems and SelectedBytes describe what the pack carries.
	SelectedItems int
	SelectedBytes int
	// SelectedTokens is SelectedBytes at the router's estimate. It is an
	// estimate and is named as one.
	SelectedTokens int
	// DroppedItems counts facts the budget excluded, and DroppedToSummary
	// counts facts kept without their body.
	DroppedItems     int
	DroppedToSummary int
	// StaleExcluded counts facts that existed and were withheld because AO
	// could not vouch for them. It is the fail-closed rule, measured.
	StaleExcluded int
	// SourcesReused names the repo-relative paths the pack's facts were
	// derived from. It is the honest form of "sources not re-read": these are
	// files whose *summarised* content AO supplied from memory rather than
	// re-deriving this dispatch. Whether the agent also opened them itself is
	// not observable here.
	SourcesReused []string
	// FallbackReason is set when the pack is empty or degraded, and says why.
	FallbackReason string
	// IndexedCommit and Generation are the provenance of the memory the pack
	// was built from.
	IndexedCommit string
	Generation    int64
}

// ContextPack is the bounded, role-scoped memory handed to one dispatch.
type ContextPack struct {
	Role      PackRole
	ProjectID domain.ProjectID
	RepoID    string
	BuiltAt   time.Time
	Sections  []PackSection
	Stats     PackStats
	// Digest is a hash over the rendered pack. Two dispatches with the same
	// digest were given the same memory; a changed digest is a changed
	// premise, and that is what makes a regression traceable.
	Digest string
}

// Empty reports whether the pack carries nothing. A caller that gets true must
// fall back to its pre-memory behaviour rather than sending a thinner context.
func (p ContextPack) Empty() bool { return p.Stats.SelectedItems == 0 }

// roleSections is the ordered set of types each role receives.
//
// The orders are not arbitrary. A Planner is told what the system IS before
// what any file does. A Worker is told the conventions it must obey and the
// modules it is about to touch. A Reviewer is told the rules first, because a
// review is an application of rules. A Repair Agent is told the known risks
// and the last task's outcome first, because it is repairing something that
// already went wrong.
var roleSections = map[PackRole][]domain.ProjectMemoryType{
	RolePlanner: {
		domain.MemoryTypeProjectOverview,
		domain.MemoryTypeArchitecture,
		domain.MemoryTypeRepositoryRelationship,
		domain.MemoryTypeModule,
		domain.MemoryTypeConvention,
		domain.MemoryTypeDecision,
		domain.MemoryTypeBuildTest,
	},
	RoleWorker: {
		domain.MemoryTypeConvention,
		domain.MemoryTypeInstruction,
		domain.MemoryTypeModule,
		domain.MemoryTypeFileSummary,
		domain.MemoryTypeDependency,
		domain.MemoryTypeBuildTest,
		domain.MemoryTypeDecision,
		domain.MemoryTypeKnownRisk,
	},
	RoleReviewer: {
		domain.MemoryTypeConvention,
		domain.MemoryTypeArchitecture,
		domain.MemoryTypeKnownRisk,
		domain.MemoryTypeDecision,
		domain.MemoryTypeModule,
		domain.MemoryTypeFileSummary,
		domain.MemoryTypeBuildTest,
	},
	RoleRepair: {
		domain.MemoryTypeKnownRisk,
		domain.MemoryTypeTaskResult,
		domain.MemoryTypeDecision,
		domain.MemoryTypeModule,
		domain.MemoryTypeFileSummary,
		domain.MemoryTypeConvention,
		domain.MemoryTypeBuildTest,
	},
}

var sectionTitles = map[domain.ProjectMemoryType]string{
	domain.MemoryTypeProjectOverview:        "Project overview",
	domain.MemoryTypeArchitecture:           "Architecture",
	domain.MemoryTypeRepositoryRelationship: "Repository relationships",
	domain.MemoryTypeModule:                 "Modules",
	domain.MemoryTypeFileSummary:            "Files",
	domain.MemoryTypeSymbolSummary:          "Symbols",
	domain.MemoryTypeDependency:             "Dependencies",
	domain.MemoryTypeConvention:             "Conventions",
	domain.MemoryTypeInstruction:            "Standing instructions",
	domain.MemoryTypeBuildTest:              "Build and test",
	domain.MemoryTypeDecision:               "Decisions",
	domain.MemoryTypeTaskResult:             "Previous task outcomes",
	domain.MemoryTypeKnownRisk:              "Known risks",
}

// PackBuilder assembles memory context packs.
type PackBuilder struct {
	repo Repository
	now  func() time.Time
}

// NewPackBuilder builds a pack builder over a durable repository.
func NewPackBuilder(repo Repository) *PackBuilder {
	return &PackBuilder{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

// WithPackClock replaces the builder's clock, for deterministic tests.
func (b *PackBuilder) WithPackClock(now func() time.Time) *PackBuilder {
	if now != nil {
		b.now = now
	}
	return b
}

// Build assembles one pack.
//
// It never returns an error for "there is no memory": that is a normal state
// with a stated FallbackReason, and a caller must be able to treat it the same
// way it treats a memory subsystem that is switched off. Errors are reserved
// for a storage failure the caller genuinely cannot proceed past.
func (b *PackBuilder) Build(ctx context.Context, req PackRequest) (ContextPack, error) {
	role := req.Role
	if !role.Valid() {
		role = RoleWorker
	}
	budget := req.Budget.Normalized()
	pack := ContextPack{
		Role: role, ProjectID: req.ProjectID, BuiltAt: b.now(),
		Stats: PackStats{Role: role},
	}

	candidates, repoID, reason, err := b.candidates(ctx, req, &pack.Stats)
	if err != nil {
		return pack, err
	}
	pack.RepoID = repoID
	if reason != "" {
		pack.Stats.FallbackReason = reason
		pack.Digest = digestOf(pack.Render())
		return pack, nil
	}

	eligible := roleSections[role]
	allowed := make(map[domain.ProjectMemoryType]int, len(eligible))
	for i, t := range eligible {
		allowed[t] = i
	}

	scored := make([]SelectedItem, 0, len(candidates))
	for _, item := range candidates {
		if _, ok := allowed[item.Key.Type]; !ok {
			continue
		}
		score, why := scoreItem(item, req)
		scored = append(scored, SelectedItem{Item: item, Score: score, Reason: why})
		pack.Stats.CandidateItems++
		pack.Stats.CandidateBytes += item.Bytes()
	}
	sortSelected(scored, allowed)

	selected := b.fit(scored, budget, &pack.Stats)
	selected = trimToRenderedBudget(&pack, selected, eligible, budget, &pack.Stats)
	pack.Sections = groupSections(selected, eligible)
	pack.Stats.SelectedItems = len(selected)
	pack.Stats.SourcesReused = sourcesOf(selected)
	if len(selected) == 0 && pack.Stats.FallbackReason == "" {
		pack.Stats.FallbackReason = fmt.Sprintf(
			"no memory of a type the %s role consumes is currently valid for this repository", role)
	}

	rendered := pack.Render()
	pack.Stats.SelectedBytes = len(rendered)
	pack.Stats.SelectedTokens = (len(rendered) + packBytesPerToken - 1) / packBytesPerToken
	pack.Digest = digestOf(rendered)
	return pack, nil
}

// candidates reads the facts this request is allowed to see, and decides
// whether there is anything trustworthy to read at all.
//
// This is where fail-closed lives. A repository with no completed pass, or one
// whose last pass failed, yields nothing and says so — a caller must not be
// handed half a memory it cannot tell from a whole one.
func (b *PackBuilder) candidates(
	ctx context.Context, req PackRequest, stats *PackStats,
) (items []domain.ProjectMemoryItem, repoID, fallback string, err error) {
	if req.RepoPath == "" && req.Role == RolePlanner {
		// A Planner pack spans the project. Every repository's state is
		// checked, and a repository that is not indexed simply contributes
		// nothing rather than blocking the others.
		all, err := b.repo.ListProjectMemoryItemsForProject(ctx, req.ProjectID)
		if err != nil {
			return nil, "", "", err
		}
		kept := b.filterServable(all, req, stats)
		if len(kept) == 0 {
			return nil, "", "no repository of this project has a completed project-memory index", nil
		}
		return kept, "", "", nil
	}

	repoPath, err := canonicalRepoPath(req.RepoPath)
	if err != nil {
		// An unreachable repository path is a degradation, not a failure: the
		// caller's own work is still valid, it simply gets no memory. The
		// reason is carried in the pack's FallbackReason instead, where the
		// caller and an operator can both read it.
		return nil, "", "the repository path could not be resolved, so no memory was attached", nil //nolint:nilerr // an unresolvable root yields an empty pack with a stated reason, never a failed dispatch
	}
	repoID = domain.ProjectMemoryRepoID(repoPath)

	state, found, err := b.repo.GetProjectMemoryIndexState(ctx, req.ProjectID, repoID)
	if err != nil {
		return nil, repoID, "", err
	}
	switch {
	case !found || state.IndexedCommit == "":
		return nil, repoID, "this repository has not completed a project-memory index yet", nil
	case state.Phase == domain.IndexPhaseFailed:
		return nil, repoID, fmt.Sprintf(
			"the last project-memory index failed (%s), so its memory is not vouched for", state.LastError), nil
	}
	stats.IndexedCommit = state.IndexedCommit
	stats.Generation = state.Generation

	all, err := b.repo.ListProjectMemoryItems(ctx, req.ProjectID, repoID)
	if err != nil {
		return nil, repoID, "", err
	}
	return b.filterServable(all, req, stats), repoID, "", nil
}

// filterServable applies the two rules that decide whether a stored fact may
// be handed to an agent: it must be valid, and — if it is one task's
// unintegrated view — it must belong to the task asking.
func (b *PackBuilder) filterServable(
	all []domain.ProjectMemoryItem, req PackRequest, stats *PackStats,
) []domain.ProjectMemoryItem {
	kept := make([]domain.ProjectMemoryItem, 0, len(all))
	for _, item := range all {
		if !item.State.Authoritative() {
			stats.StaleExcluded++
			continue
		}
		if item.Origin == domain.OriginTaskLocal {
			// Task-local memory is visible only to its own task. This is the
			// whole of the worktree-isolation rule at the read side: one
			// task's uncommitted opinion can never become another's premise.
			if req.TaskRef == "" || item.OriginRef != req.TaskRef {
				continue
			}
		}
		kept = append(kept, item)
	}
	return kept
}

// scoreItem ranks one fact for one request.
//
// The signals are combined rather than compared so that a strong weak signal
// cannot outrank a direct hit: a changed-path match is worth more than any
// number of keyword matches, by construction.
func scoreItem(item domain.ProjectMemoryItem, req PackRequest) (float64, string) {
	score := item.Confidence
	reason := "general project knowledge"

	if len(req.ChangedPaths) > 0 && matchesPaths(item, req.ChangedPaths) {
		score += 10
		reason = "covers a path this work changes"
	} else if len(req.Modules) > 0 && matchesModules(item, req.Modules) {
		score += 6
		reason = "covers a module this work touches"
	}
	if item.Origin == domain.OriginTaskLocal && req.TaskRef != "" && item.OriginRef == req.TaskRef {
		score += 4
		reason = "this task's own recorded outcome"
	}
	if len(req.Keywords) > 0 && matchesKeywords(item, req.Keywords) {
		score += 1
		if reason == "general project knowledge" {
			reason = "matches a term from the objective"
		}
	}
	// A narrower fact about the same subject is the more useful one, but only
	// as a tiebreak: a project-wide convention still outranks an unrelated
	// file summary, because the file summary earned no relevance bonus.
	score += float64(item.Key.Scope.Specificity()) * 0.1
	return score, reason
}

func matchesPaths(item domain.ProjectMemoryItem, changed []string) bool {
	for _, c := range changed {
		c = strings.TrimSpace(strings.TrimPrefix(c, "./"))
		if c == "" {
			continue
		}
		if item.Key.Scope == domain.MemoryScopeFile && item.Key.Key == c {
			return true
		}
		if item.Key.Scope == domain.MemoryScopeModule && item.Key.Key == path.Dir(c) {
			return true
		}
		for _, src := range item.SourcePaths {
			if src == c {
				return true
			}
		}
	}
	return false
}

func matchesModules(item domain.ProjectMemoryItem, modules []string) bool {
	for _, m := range modules {
		m = strings.TrimSpace(strings.TrimSuffix(m, "/"))
		if m == "" {
			continue
		}
		if item.Key.Key == m {
			return true
		}
		if item.Key.Scope == domain.MemoryScopeFile && path.Dir(item.Key.Key) == m {
			return true
		}
	}
	return false
}

func matchesKeywords(item domain.ProjectMemoryItem, keywords []string) bool {
	hay := strings.ToLower(item.Summary + " " + item.Key.Key)
	for _, k := range keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if len(k) < 3 {
			// A one- or two-character term matches everything, which is the
			// same as matching nothing while looking like a signal.
			continue
		}
		if strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

// sortSelected imposes the deterministic ranking. Every comparison falls
// through to the derived item id, so no two orderings of the same set are
// possible.
func sortSelected(items []SelectedItem, sectionRank map[domain.ProjectMemoryType]int) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.Score != b.Score:
			return a.Score > b.Score
		case sectionRank[a.Item.Key.Type] != sectionRank[b.Item.Key.Type]:
			return sectionRank[a.Item.Key.Type] < sectionRank[b.Item.Key.Type]
		case a.Item.Confidence != b.Item.Confidence:
			return a.Item.Confidence > b.Item.Confidence
		case !a.Item.UpdatedAt.Equal(b.Item.UpdatedAt):
			return a.Item.UpdatedAt.After(b.Item.UpdatedAt)
		default:
			return a.Item.ID < b.Item.ID
		}
	})
}

// fit applies the budget.
//
// The degradation is two-stage on purpose. A fact that will not fit whole is
// first tried as its summary alone, because "there is a convention about this
// area, and here is its one-line form" is far more useful than silence; only
// when even the summary will not fit is the fact dropped. Both outcomes are
// counted, so a pack never quietly under-delivers.
func (b *PackBuilder) fit(ranked []SelectedItem, budget PackBudget, stats *PackStats) []SelectedItem {
	const perItemOverhead = 8 // the rendered "- " prefix, newlines and indent
	out := make([]SelectedItem, 0, len(ranked))
	used := 0

	for _, cand := range ranked {
		if len(out) >= budget.MaxItems {
			stats.DroppedItems++
			continue
		}
		full := len(cand.Item.Summary) + len(cand.Item.Content) + perItemOverhead
		summaryOnly := len(cand.Item.Summary) + perItemOverhead
		switch {
		case used+full <= budget.MaxBytes:
			cand.BodyIncluded = true
			used += full
		case used+summaryOnly <= budget.MaxBytes:
			cand.BodyIncluded = false
			used += summaryOnly
			stats.DroppedToSummary++
		default:
			stats.DroppedItems++
			continue
		}
		out = append(out, cand)
	}
	return out
}

// trimToRenderedBudget enforces MaxBytes against the bytes the agent will
// actually receive.
//
// fit() budgets against each fact's own size, which is the right basis for
// ranking but is not the rendered size: rendering adds a header, a heading per
// section, a list marker per fact and two spaces of indent per body line. On a
// real repository that gap was measured at roughly 8% — enough for a pack to
// exceed the budget it reported honouring.
//
// So the budget is enforced where it is promised. The lowest-ranked fact is
// demoted to its summary first and dropped only if that is still not enough,
// which keeps the degradation the same shape as fit()'s: less detail before
// less coverage.
func trimToRenderedBudget(
	pack *ContextPack, selected []SelectedItem, order []domain.ProjectMemoryType,
	budget PackBudget, stats *PackStats,
) []SelectedItem {
	for len(selected) > 0 {
		pack.Sections = groupSections(selected, order)
		if len(pack.Render()) <= budget.MaxBytes {
			return selected
		}
		last := len(selected) - 1
		if selected[last].BodyIncluded {
			selected[last].BodyIncluded = false
			stats.DroppedToSummary++
			continue
		}
		selected = selected[:last]
		stats.DroppedItems++
		stats.DroppedToSummary--
	}
	pack.Sections = nil
	return selected
}

// groupSections lays the selected facts out in the role's section order,
// preserving the ranking within each section.
func groupSections(selected []SelectedItem, order []domain.ProjectMemoryType) []PackSection {
	byType := map[domain.ProjectMemoryType][]SelectedItem{}
	for _, s := range selected {
		byType[s.Item.Key.Type] = append(byType[s.Item.Key.Type], s)
	}
	sections := make([]PackSection, 0, len(order))
	for _, t := range order {
		items := byType[t]
		if len(items) == 0 {
			continue
		}
		title := sectionTitles[t]
		if title == "" {
			title = string(t)
		}
		sections = append(sections, PackSection{Title: title, Type: t, Items: items})
	}
	return sections
}

func sourcesOf(selected []SelectedItem) []string {
	var all []string
	for _, s := range selected {
		all = append(all, s.Item.SourcePaths...)
	}
	return domain.NormalizeMemorySourcePaths(all)
}

// Render produces the pack's canonical text.
//
// This is the one representation. A provider adapter may re-wrap it, and may
// not add, drop or reword a fact — a pack whose meaning depends on which
// provider rendered it is not a shared premise.
//
// The header states the provenance and the trust boundary in the same breath,
// because an agent has to know both: what commit this knowledge is from, and
// that it is a cache over the repository rather than the repository itself.
func (p ContextPack) Render() string {
	var b strings.Builder
	b.WriteString("## AO project memory\n\n")
	b.WriteString("These are durable facts AO has recorded about this project. ")
	b.WriteString("They are a summary derived from the repository, not the repository itself: ")
	b.WriteString("where this and the working tree disagree, the working tree is correct and this is out of date.\n\n")

	if p.Stats.IndexedCommit != "" {
		fmt.Fprintf(&b, "Derived at commit %s (memory generation %d).\n\n",
			p.Stats.IndexedCommit, p.Stats.Generation)
	}
	if p.Stats.FallbackReason != "" {
		fmt.Fprintf(&b, "No project memory is attached: %s.\n", p.Stats.FallbackReason)
		return b.String()
	}

	for _, section := range p.Sections {
		fmt.Fprintf(&b, "### %s\n\n", section.Title)
		for _, sel := range section.Items {
			fmt.Fprintf(&b, "- %s\n", sel.Item.Summary)
			if sel.BodyIncluded && sel.Item.Content != "" {
				for _, line := range strings.Split(sel.Item.Content, "\n") {
					b.WriteString("  " + line + "\n")
				}
			}
		}
		b.WriteString("\n")
	}
	if p.Stats.DroppedItems > 0 || p.Stats.DroppedToSummary > 0 {
		fmt.Fprintf(&b, "(%d further facts were omitted and %d reduced to their summary to stay within this pack's budget.)\n",
			p.Stats.DroppedItems, p.Stats.DroppedToSummary)
	}
	return b.String()
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
