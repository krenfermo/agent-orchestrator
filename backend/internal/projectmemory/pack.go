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
	// WorkflowRunID is the run this dispatch belongs to. It scopes
	// workflow-local knowledge: a fact shared at ShareWorkflow reaches only
	// readers inside the run that produced it, and never a later, unrelated
	// run that happens to name the same upstream task.
	WorkflowRunID string
	// UpstreamTaskRefs are the tasks this one explicitly depends on, and whose
	// workflow-local knowledge it is therefore authorized to read.
	//
	// It is supplied by the workflow boundary from workflow_task_dependencies,
	// never derived here, and it is what makes sibling isolation real: two
	// parallel tasks that do not declare a dependency on each other name each
	// other in no list, so neither can see the other's unintegrated work no
	// matter how much their file sets overlap.
	UpstreamTaskRefs []string
	// CoverablePaths are legacy documents this dispatch is carrying that
	// memory would be ALLOWED to replace.
	//
	// It is the difference between memory that costs bytes and memory that
	// saves them. A fact summarising a document the dispatch is already
	// sending can pay for itself by removing that document; a fact about
	// something the dispatch was not going to mention can only add. So when
	// replacement is permitted, the coverable facts are ranked first — and
	// measurement on a real repository is what made this necessary rather than
	// clever: without it the planner's pack spent its whole budget on module
	// censuses and replaced exactly one of six documents.
	//
	// The caller sets it only in a mode that may replace. In an add-only mode
	// covering a document buys nothing, and boosting those facts would spend
	// the budget on the least useful ones.
	CoverablePaths []string
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
	// Freshness is how well this fact's provenance matches the memory's
	// current state (see freshnessRank). It is the first key selection ranks
	// on, so a fact AO re-confirmed at the commit in front of it outranks one
	// carried over from an older pass at equal relevance.
	Freshness int
}

// Freshness ranks, highest first. They are buckets rather than timestamps
// because within one indexing pass every fact shares an UpdatedAt, and a
// tie-break on raw time would be noise dressed as a signal.
const (
	// freshConfirmedAtCommit is a fact whose provenance names the commit the
	// memory is currently indexed at: AO checked this file, at this version,
	// during the pass that produced the state being served.
	freshConfirmedAtCommit = 2
	// freshCurrentGeneration is a fact the current pass re-confirmed but whose
	// commit AO cannot match (a checkout with no commit, an aggregate).
	freshCurrentGeneration = 1
	// freshCarriedOver is a valid fact from an earlier pass. Still servable —
	// validity is what authorises serving it — but it loses a tie.
	freshCarriedOver = 0
)

// freshnessRank buckets one fact against the memory state being served.
func freshnessRank(item domain.ProjectMemoryItem, indexedCommit string, generation int64) int {
	switch {
	case indexedCommit != "" && item.SourceCommit == indexedCommit:
		return freshConfirmedAtCommit
	case generation > 0 && item.Generation == generation:
		return freshCurrentGeneration
	default:
		return freshCarriedOver
	}
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

	// --- shared task knowledge (P2-C §18) --------------------------------
	//
	// These count the facts that came from a TASK rather than from the
	// repository, which is the population whose selection P2-C has to be able
	// to defend. The pair that matters is SharedCandidates against
	// SharedSelected: an unrelated task's pack should show candidates it
	// considered and did not take, not an empty population it never had.

	// SharedCandidates counts task-produced facts selection was allowed to
	// choose from, after the sharing gate and before the relevance gate.
	SharedCandidates int
	// SharedSelected counts task-produced facts the pack actually carries.
	SharedSelected int
	// SharedIrrelevantExcluded counts task-produced facts withheld because
	// they had no bearing on this work. It is the number that proves an
	// unrelated task received nothing rather than everything.
	SharedIrrelevantExcluded int
	// SharedUnauthorizedExcluded counts task-produced facts withheld because
	// this reader was not entitled to them — another task's unintegrated view,
	// or a sibling's workflow-local knowledge.
	SharedUnauthorizedExcluded int
	// SupersededExcluded counts facts withheld because they are no longer
	// current: a superseded decision, a resolved risk, an obsolete fact.
	SupersededExcluded int
	// ConflictingExcluded counts facts withheld because AO could not order
	// them against an incompatible peer.
	ConflictingExcluded int
	// DecisionsSelected and RisksSelected break the selected shared knowledge
	// down by what it is.
	DecisionsSelected int
	RisksSelected     int
	// TaskLocalSelected, WorkflowLocalSelected and CanonicalSelected report
	// which scope the pack's facts came from. They are what makes "did this
	// task read a sibling's unintegrated work" answerable after the fact.
	TaskLocalSelected     int
	WorkflowLocalSelected int
	CanonicalSelected     int
	// KnowledgeBytes is what the task-produced facts weigh inside the pack.
	KnowledgeBytes int
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
		domain.MemoryTypeKnownRisk,
		domain.MemoryTypeTaskResult,
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
		domain.MemoryTypeTaskResult,
	},
	RoleReviewer: {
		domain.MemoryTypeConvention,
		domain.MemoryTypeArchitecture,
		domain.MemoryTypeKnownRisk,
		domain.MemoryTypeDecision,
		domain.MemoryTypeTaskResult,
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
	coverable := make(map[string]struct{}, len(req.CoverablePaths))
	for _, p := range req.CoverablePaths {
		if p = normalizePath(p); p != "" {
			coverable[p] = struct{}{}
		}
	}

	upstream := make(map[string]struct{}, len(req.UpstreamTaskRefs))
	for _, ref := range req.UpstreamTaskRefs {
		if ref = strings.TrimSpace(ref); ref != "" {
			upstream[ref] = struct{}{}
		}
	}

	scored := make([]SelectedItem, 0, len(candidates))
	for _, item := range candidates {
		if _, ok := allowed[item.Key.Type]; !ok {
			continue
		}
		shared := domain.IsSharedKnowledgeType(item.Key.Type)
		if shared {
			pack.Stats.SharedCandidates++
		}
		// The relevance gate runs BEFORE scoring, not as a low score. A fact
		// with no bearing on this work must be absent from the candidate set
		// entirely: leaving it in with a poor score would still let it win a
		// pack that had nothing better, which is exactly how an unrelated task
		// ends up carrying another task's history.
		if !relevantSharedKnowledge(item, req, upstream) {
			pack.Stats.SharedIrrelevantExcluded++
			continue
		}
		score, why := scoreItem(item, req, coverable)
		scored = append(scored, SelectedItem{
			Item: item, Score: score, Reason: why,
			Freshness: freshnessRank(item, pack.Stats.IndexedCommit, pack.Stats.Generation),
		})
		pack.Stats.CandidateItems++
		pack.Stats.CandidateBytes += item.Bytes()
	}
	sortSelected(scored, allowed)

	selected := b.fit(scored, budget, &pack.Stats)
	selected = trimToRenderedBudget(&pack, selected, eligible, budget, &pack.Stats)
	pack.Sections = groupSections(selected, eligible)
	pack.Stats.SelectedItems = len(selected)
	pack.Stats.SourcesReused = sourcesOf(selected)
	countSharedKnowledge(selected, req, &pack.Stats)
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
	case !found || state.Generation == 0 || state.CompletedAt.IsZero():
		// No pass has finished, so there is nothing AO can vouch for. Note the
		// gate is a COMPLETED PASS, not a known commit: a repository whose
		// commit AO cannot read (a scratch directory, a checkout with no
		// history) still has memory worth serving — it simply cannot prove
		// that memory is current, which costs it the warm path rather than the
		// memory itself.
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

// filterServable applies the three rules that decide whether a stored fact may
// be handed to an agent: it must be valid, this reader must be entitled to it,
// and it must still be current.
//
// The three are separate on purpose, because they answer different questions
// and their answers have different consequences.
//
//  1. **Validity** is the P2-A drift rule: can AO still demonstrate this
//     fact's provenance? A fact whose sources moved is withheld regardless of
//     who is asking.
//  2. **Entitlement** is the P2-C sharing rule: may THIS reader see it? A
//     task-local fact reaches its own task; a workflow-local fact reaches the
//     tasks downstream of it inside its own run; a canonical fact reaches
//     everyone. This is the whole of sibling safety at the read side — two
//     parallel tasks name each other in no dependency list, so neither is
//     entitled to the other's unintegrated view no matter how much their file
//     sets overlap.
//  3. **Currency** is the P2-C lifecycle rule: is this still what the project
//     believes? A superseded decision, a resolved risk and an obsolete fact
//     are all kept forever and none of them is served as current.
//
// Each exclusion is counted under its own name, so "why did this task not
// receive that decision" has one answer rather than three candidates.
func (b *PackBuilder) filterServable(
	all []domain.ProjectMemoryItem, req PackRequest, stats *PackStats,
) []domain.ProjectMemoryItem {
	upstream := make(map[string]struct{}, len(req.UpstreamTaskRefs))
	for _, ref := range req.UpstreamTaskRefs {
		if ref = strings.TrimSpace(ref); ref != "" {
			upstream[ref] = struct{}{}
		}
	}

	kept := make([]domain.ProjectMemoryItem, 0, len(all))
	for _, item := range all {
		if !item.State.Authoritative() {
			stats.StaleExcluded++
			continue
		}
		if !b.entitled(item, req, upstream) {
			if domain.IsSharedKnowledgeType(item.Key.Type) {
				stats.SharedUnauthorizedExcluded++
			}
			continue
		}
		switch status := domain.KnowledgeStatusOf(item); {
		case status.Current():
		case status == domain.KnowledgeConflicting && req.Role == RolePlanner:
			// A contradiction AO could not order is information about the
			// memory rather than about the project, so it reaches the one role
			// whose job is to decide what to do about it — and reaches it
			// labelled as a conflict, never as a fact.
			item.Summary = "CONFLICTING — " + item.Summary
			kept = append(kept, item)
			continue
		case status == domain.KnowledgeConflicting:
			stats.ConflictingExcluded++
			continue
		default:
			stats.SupersededExcluded++
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// entitled reports whether this reader may see one fact.
func (b *PackBuilder) entitled(
	item domain.ProjectMemoryItem, req PackRequest, upstream map[string]struct{},
) bool {
	switch domain.KnowledgeShareOf(item) {
	case domain.ShareCanonical:
		// Canonical knowledge is the project's. A task-local ROW that somehow
		// claims canonical sharing is still refused below, because origin and
		// sharing must agree before anything is served project-wide.
		return item.Origin == domain.OriginCanonical
	case domain.ShareWorkflow:
		// Downstream of the producer, inside the producer's own run. Both
		// halves are required: the run scopes it, and the declared dependency
		// authorizes it.
		if req.WorkflowRunID == "" || domain.KnowledgeRunOf(item) != req.WorkflowRunID {
			return item.OriginRef != "" && item.OriginRef == req.TaskRef
		}
		if item.OriginRef == req.TaskRef {
			return true
		}
		_, ok := upstream[item.OriginRef]
		return ok
	default:
		// ShareTask, and anything unrecognised. Its own task and nobody else.
		return item.OriginRef != "" && item.OriginRef == req.TaskRef
	}
}

// relevantSharedKnowledge is the gate that keeps one task's learning away from
// work it has nothing to do with (P2-C §6, §19).
//
// It applies only to task-produced facts. A fact about the repository — a
// convention, a module summary — is relevant to anyone working in that
// repository, and gating it would be a regression. A fact about what some
// earlier task did is different: it is worth carrying when the two pieces of
// work overlap, and it is pure cost when they do not. Without this gate, every
// task would receive every prior task's outcome and the pack budget would be
// spent on history instead of on knowledge.
//
// Four things count as overlap, and nothing else does:
//
//   - The reader's OWN task produced it.
//   - The reader explicitly depends on the task that produced it.
//   - The fact's evidence names a path or a module this work touches.
//   - The fact is stated at PROJECT scope, or is a decision or risk that names
//     no evidence at all. Both are facts AO has nothing narrower to match on,
//     and withholding them would withhold the project's standing rules.
//
// "Both tasks are in the same project" is deliberately NOT overlap. That is
// the invented relationship P2-C §5 forbids, and admitting it would make the
// gate no gate at all.
//
// Repository scope is NOT a free pass either, and measuring it is what showed
// why. A decision recorded by a task inherits that task's changed paths as its
// evidence, and its scope defaults to the repository — so treating
// "repository-scoped" as universally relevant admitted every decision and
// every risk every task had ever recorded. On a store holding twelve prior
// tasks in one module, an unrelated task was handed 24 of their facts and 3.3
// KB of another module's history. The rule is therefore about EVIDENCE, not
// scope: a fact whose subject AO can locate must overlap the work; only a fact
// AO cannot locate is admitted on the repository's authority.
func relevantSharedKnowledge(
	item domain.ProjectMemoryItem, req PackRequest, upstream map[string]struct{},
) bool {
	if !domain.IsSharedKnowledgeType(item.Key.Type) {
		return true
	}
	task := domain.KnowledgeTaskOf(item)
	if task != "" && req.TaskRef != "" && task == req.TaskRef {
		return true
	}
	if _, ok := upstream[task]; ok && task != "" {
		return true
	}
	if len(req.ChangedPaths) > 0 && matchesPaths(item, req.ChangedPaths) {
		return true
	}
	if len(req.Modules) > 0 && matchesModules(item, req.Modules) {
		return true
	}
	// A task RESULT never qualifies on anything but overlap: it is a report
	// about one piece of work, and a reader with no overlap has no use for it.
	if item.Key.Type != domain.MemoryTypeDecision && item.Key.Type != domain.MemoryTypeKnownRisk {
		return false
	}
	// A decision or risk stated at project scope is a standing rule.
	if item.Key.Scope == domain.MemoryScopeProject {
		return true
	}
	// Otherwise it is admitted only when AO has nothing narrower to judge it
	// by. A fact with evidence has already failed the overlap test above.
	return len(item.SourcePaths) == 0
}

// scoreItem ranks one fact for one request.
//
// The signals are combined rather than compared so that a strong weak signal
// cannot outrank a direct hit: a changed-path match is worth more than any
// number of keyword matches, by construction.
func scoreItem(item domain.ProjectMemoryItem, req PackRequest, coverable map[string]struct{}) (float64, string) {
	score := item.Confidence
	reason := "general project knowledge"

	// A fact that can REPLACE a document the dispatch is already carrying
	// outranks everything else, because it is the only kind of fact that
	// reduces the payload rather than adding to it. It must be a single-source
	// fact: an aggregate summarises a combination and cannot stand in for any
	// one member of it (see coverageIndex).
	if len(coverable) > 0 && len(item.SourcePaths) == 1 {
		if _, ok := coverable[normalizePath(item.SourcePaths[0])]; ok {
			return score + 20, "replaces a document this dispatch was already sending"
		}
	}

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
	// Scope proximity is NOT folded into the score: it is a separate, later
	// ranking key (see sortSelected), so a narrow fact about an unrelated file
	// can never accumulate its way past a project-wide convention that the
	// work actually touches.
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

// sortSelected imposes the ranking the budget evicts from the bottom of.
//
// The order is the one P2-B specifies, and each key earns its position:
//
//  1. **Freshness.** A fact AO confirmed against the state in front of it
//     outranks one carried over from an earlier pass. This is first because
//     serving a less current fact in place of a more current one is the only
//     ordering mistake here that can mislead rather than merely disappoint.
//  2. **Relevance.** What this piece of work actually touches. Within one pass
//     every canonical fact shares a freshness bucket, so in practice this is
//     the key that decides most packs — which is the intent.
//  3. **Confidence.** How directly AO observed the fact.
//  4. **Scope proximity.** The narrower fact about the same subject.
//  5. **A deterministic tie-break.** The section order, then the derived id.
//
// Every comparison falls through to the id, so no two orderings of the same
// set are possible and a pack's digest is reproducible.
func sortSelected(items []SelectedItem, sectionRank map[domain.ProjectMemoryType]int) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.Freshness != b.Freshness:
			return a.Freshness > b.Freshness
		case a.Score != b.Score:
			return a.Score > b.Score
		case a.Item.Confidence != b.Item.Confidence:
			return a.Item.Confidence > b.Item.Confidence
		case a.Item.Key.Scope.Specificity() != b.Item.Key.Scope.Specificity():
			return a.Item.Key.Scope.Specificity() > b.Item.Key.Scope.Specificity()
		case sectionRank[a.Item.Key.Type] != sectionRank[b.Item.Key.Type]:
			return sectionRank[a.Item.Key.Type] < sectionRank[b.Item.Key.Type]
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

// countSharedKnowledge attributes the selected facts to the scope they came
// from and to what they are.
//
// It runs on the FINAL selection rather than on the candidates, because the
// question it answers is "what did this dispatch actually receive" — a fact
// the budget evicted was not shared with anyone, and counting it as shared
// would make the number describe an intention instead of an outcome.
func countSharedKnowledge(selected []SelectedItem, req PackRequest, stats *PackStats) {
	for _, sel := range selected {
		item := sel.Item
		switch domain.KnowledgeShareOf(item) {
		case domain.ShareCanonical:
			stats.CanonicalSelected++
		case domain.ShareWorkflow:
			if item.OriginRef == req.TaskRef {
				stats.TaskLocalSelected++
			} else {
				stats.WorkflowLocalSelected++
			}
		default:
			stats.TaskLocalSelected++
		}
		if !domain.IsSharedKnowledgeType(item.Key.Type) {
			continue
		}
		stats.SharedSelected++
		stats.KnowledgeBytes += item.Bytes()
		switch item.Key.Type {
		case domain.MemoryTypeDecision:
			stats.DecisionsSelected++
		case domain.MemoryTypeKnownRisk:
			stats.RisksSelected++
		}
	}
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
