package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// project_memory.go — the durable vocabulary of AO's project memory (P2-A).
//
// Project memory is a *derived semantic cache*: bounded, provenance-carrying
// facts about a project that let the Planner, Worker, Reviewer and the Repair
// Agents skip re-deriving what has not changed. It is deliberately NOT a
// source of truth. The repository on disk and AO's durable workflow rows are.
// When the two disagree the repository wins and the memory item is marked
// stale rather than served — see ProjectMemoryState and the "fail closed" rule
// in docs/project-memory.md.
//
// Three properties are carried by this vocabulary rather than by convention:
//
//   - Identity is derived, never supplied. ProjectMemoryKey names one fact
//     within one repository, and ProjectMemoryItemID hashes it. Two indexers
//     that discover the same fact address the same row, so re-indexing updates
//     instead of accumulating.
//   - Every item names its provenance: the commit it was derived at, the paths
//     it was derived from, and a digest over those paths' content. An item
//     whose provenance can no longer be demonstrated is not authoritative.
//   - Every item carries a Generation. Writes are generation-conditioned
//     (compare-and-set) so a slow indexer that wakes up after a newer pass has
//     already run cannot overwrite the newer pass's memory. This is the same
//     fence the capacity scheduler and the execution placements use.
//
// The vocabulary is provider-neutral on purpose. Nothing here knows about
// Claude, Codex, Grae or Graphify; adapters may reformat a pack, never change
// what it means.

// ProjectMemorySchemaVersion identifies the shape of a durable memory item. It
// participates in every derived hash, so an item written by an older schema
// can be recognised — and re-derived — rather than misread.
const ProjectMemorySchemaVersion = 1

// ErrProjectMemoryInvalid is the sentinel every project-memory validation
// failure wraps, so callers can branch on the cause rather than on message
// text.
var ErrProjectMemoryInvalid = errors.New("domain: invalid project memory")

// ProjectMemoryType is what kind of fact an item holds.
//
// The type is an open vocabulary: an item read back with a type this build
// does not know keeps its own name instead of collapsing into a neighbouring
// one. The constants below are the types AO writes today.
type ProjectMemoryType string

// Project memory item types.
const (
	// MemoryTypeProjectOverview is the one-per-project orientation fact: what
	// the project is, which repositories it spans, how it is laid out.
	MemoryTypeProjectOverview ProjectMemoryType = "project_overview"
	// MemoryTypeArchitecture is a durable architectural statement derived from
	// an architecture document or from the repository's own shape.
	MemoryTypeArchitecture ProjectMemoryType = "architecture"
	// MemoryTypeModule is one coherent unit of the repository — a Go package,
	// a frontend feature directory, a service.
	MemoryTypeModule ProjectMemoryType = "module"
	// MemoryTypeFileSummary is what one file is for. It is written only for
	// files the indexer's bounds admitted; it is not a summary of every byte.
	MemoryTypeFileSummary ProjectMemoryType = "file_summary"
	// MemoryTypeSymbolSummary is what one exported symbol is for.
	MemoryTypeSymbolSummary ProjectMemoryType = "symbol_summary"
	// MemoryTypeDependency is one declared external dependency of the project.
	MemoryTypeDependency ProjectMemoryType = "dependency"
	// MemoryTypeConvention is a rule the project expects code to follow.
	MemoryTypeConvention ProjectMemoryType = "convention"
	// MemoryTypeInstruction is an instruction file's standing content
	// (AGENTS.md, CLAUDE.md, CONTRIBUTING.md).
	MemoryTypeInstruction ProjectMemoryType = "instruction"
	// MemoryTypeBuildTest is how the project is built, linted and tested.
	MemoryTypeBuildTest ProjectMemoryType = "build_test"
	// MemoryTypeDecision is a decision AO or a person took that later work
	// must respect.
	MemoryTypeDecision ProjectMemoryType = "decision"
	// MemoryTypeTaskResult is the bounded outcome of one completed task: what
	// changed, why, and how it verified. It is a summary, never a transcript.
	MemoryTypeTaskResult ProjectMemoryType = "task_result"
	// MemoryTypeKnownRisk is a risk or follow-up a later task should know
	// about before touching the same area.
	MemoryTypeKnownRisk ProjectMemoryType = "known_risk"
	// MemoryTypeRepositoryRelationship describes how one repository in a
	// multi-repo project relates to another.
	MemoryTypeRepositoryRelationship ProjectMemoryType = "repository_relationship"

	// --- P4-H: the high-level durable categories -------------------------
	//
	// These are the facts a person asks a project about before touching it,
	// and the ones a Planner needs before it can plan. They are deliberately
	// ABOVE the symbol level: what runs, what stores, what authorises, what
	// the project talks to. The Code Graph already answers "which function" —
	// duplicating that here would make memory a second, worse graph, so
	// nothing below is per-symbol.

	// MemoryTypeEntryPoint is a file a process actually starts in: a main, a
	// server bootstrap, a CLI root, an app entry.
	MemoryTypeEntryPoint ProjectMemoryType = "entry_point"
	// MemoryTypeRuntimeSurface is how the project is reached at runtime — the
	// HTTP/RPC surface, where routes are registered, and what serves them.
	MemoryTypeRuntimeSurface ProjectMemoryType = "runtime_surface"
	// MemoryTypePersistence is the storage architecture: which engine, which
	// tables, where the schema and the queries against it live.
	MemoryTypePersistence ProjectMemoryType = "persistence"
	// MemoryTypeAuthModel is the authentication/authorization architecture:
	// where identity is established and where permission is decided. It never
	// carries a secret, a key or a credential — only where the decisions are.
	MemoryTypeAuthModel ProjectMemoryType = "auth_model"
	// MemoryTypeIntegration is an external system the project talks to, as
	// distinct from a build-time dependency (MemoryTypeDependency).
	MemoryTypeIntegration ProjectMemoryType = "integration"
	// MemoryTypeTestingSurface is how the project verifies itself: where the
	// tests live, how many there are, what they cover.
	MemoryTypeTestingSurface ProjectMemoryType = "testing_surface"
	// MemoryTypeConfigSurface is the configuration convention: which files
	// configure the project and which keys the code reads. Keys only, never a
	// value — a value read out of a checked-in config is exactly the class of
	// thing that turns out to be a credential.
	MemoryTypeConfigSurface ProjectMemoryType = "config_surface"
	// MemoryTypeDeployment is the runtime/deployment structure: containers,
	// orchestration, CI/CD pipelines, release manifests.
	MemoryTypeDeployment ProjectMemoryType = "deployment"
)

// ProjectMemoryTypes returns every type this build writes, in a stable order.
// It is the vocabulary the CLI and the API validate against; an unknown type
// read from storage is still preserved.
func ProjectMemoryTypes() []ProjectMemoryType {
	return []ProjectMemoryType{
		MemoryTypeProjectOverview,
		MemoryTypeArchitecture,
		MemoryTypeModule,
		MemoryTypeFileSummary,
		MemoryTypeSymbolSummary,
		MemoryTypeDependency,
		MemoryTypeConvention,
		MemoryTypeInstruction,
		MemoryTypeBuildTest,
		MemoryTypeDecision,
		MemoryTypeTaskResult,
		MemoryTypeKnownRisk,
		MemoryTypeRepositoryRelationship,
		MemoryTypeEntryPoint,
		MemoryTypeRuntimeSurface,
		MemoryTypePersistence,
		MemoryTypeAuthModel,
		MemoryTypeIntegration,
		MemoryTypeTestingSurface,
		MemoryTypeConfigSurface,
		MemoryTypeDeployment,
	}
}

// ProjectMemoryScope says how wide a fact's claim is. It is what lets
// selection prefer the most specific fact that covers a changed area without
// having to parse the key.
type ProjectMemoryScope string

// Project memory scopes, narrowest last.
const (
	// MemoryScopeProject is a fact about the whole project, across every
	// repository in it.
	MemoryScopeProject ProjectMemoryScope = "project"
	// MemoryScopeRepository is a fact about one repository.
	MemoryScopeRepository ProjectMemoryScope = "repository"
	// MemoryScopeModule is a fact about one module/package/directory.
	MemoryScopeModule ProjectMemoryScope = "module"
	// MemoryScopeFile is a fact about one file.
	MemoryScopeFile ProjectMemoryScope = "file"
	// MemoryScopeSymbol is a fact about one symbol.
	MemoryScopeSymbol ProjectMemoryScope = "symbol"
	// MemoryScopeTask is a fact produced by, and about, one task.
	MemoryScopeTask ProjectMemoryScope = "task"
)

// Specificity ranks scopes from widest (0) to narrowest. Selection uses it to
// break ties deterministically: given equal relevance, the narrower fact about
// the area actually being touched is the more useful one.
func (s ProjectMemoryScope) Specificity() int {
	switch s {
	case MemoryScopeProject:
		return 0
	case MemoryScopeRepository:
		return 1
	case MemoryScopeModule:
		return 2
	case MemoryScopeFile:
		return 3
	case MemoryScopeSymbol:
		return 4
	case MemoryScopeTask:
		return 5
	default:
		// An unknown scope sorts between repository and module: wide enough
		// not to outrank a file fact about the changed area, specific enough
		// not to be mistaken for a project-wide claim.
		return 1
	}
}

// Valid reports whether the scope is one this build writes.
func (s ProjectMemoryScope) Valid() bool {
	switch s {
	case MemoryScopeProject, MemoryScopeRepository, MemoryScopeModule,
		MemoryScopeFile, MemoryScopeSymbol, MemoryScopeTask:
		return true
	default:
		return false
	}
}

// ProjectMemoryState is whether a fact may be served as authoritative context.
//
// The four states are the whole of the drift model, and the distinction
// between the last three is load-bearing:
//
//   - Valid: provenance was checked and still holds.
//   - Stale: provenance moved (the file changed, the commit is unreachable).
//     The row is KEPT, because re-deriving from a known previous answer is
//     cheaper than from nothing, but it is never served as authoritative.
//   - Invalidated: the fact's subject is gone (file deleted, module removed).
//     It cannot be refreshed, only replaced.
//   - Rebuilding: an indexer has claimed this fact and is re-deriving it. The
//     previous content is still readable, and still not authoritative.
//
// Anything that is not Valid fails closed: it is excluded from context packs
// unless the caller explicitly asks for the unfiltered set.
type ProjectMemoryState string

// Project memory states.
const (
	MemoryStateValid       ProjectMemoryState = "valid"
	MemoryStateStale       ProjectMemoryState = "stale"
	MemoryStateInvalidated ProjectMemoryState = "invalidated"
	MemoryStateRebuilding  ProjectMemoryState = "rebuilding"
)

// Authoritative reports whether a fact in this state may be handed to an agent
// as something AO vouches for. Only MemoryStateValid may.
func (s ProjectMemoryState) Authoritative() bool { return s == MemoryStateValid }

// Valid reports whether the state is one this build writes.
func (s ProjectMemoryState) Valid() bool {
	switch s {
	case MemoryStateValid, MemoryStateStale, MemoryStateInvalidated, MemoryStateRebuilding:
		return true
	default:
		return false
	}
}

// ProjectMemoryKey is the natural key of one fact: which repository it is
// about, what kind of fact it is, how wide its claim is, and what it names.
//
// It exists as a type because identity is the one thing two independent
// indexers must agree on. Deriving the row id from the key (rather than
// letting a caller supply one) is what makes re-indexing an update instead of
// an append.
type ProjectMemoryKey struct {
	// ProjectID is the registered project the fact belongs to. Every read and
	// write is scoped by it, so two projects can never observe each other's
	// memory.
	ProjectID ProjectID
	// RepoID is the repository identity within the project — see
	// ProjectMemoryRepoID. A single-repo project has exactly one.
	RepoID string
	// Type is what kind of fact this is.
	Type ProjectMemoryType
	// Scope is how wide its claim is.
	Scope ProjectMemoryScope
	// Key names the subject within the scope: a module path, a repo-relative
	// file path, a symbol id, a task id. It is empty only for a
	// project-scoped singleton such as MemoryTypeProjectOverview.
	Key string
}

// Normalized trims and lower-cases the parts of a key that must compare
// equal across producers. The subject Key keeps its case — file paths and
// symbol names are case-sensitive facts about the repository.
func (k ProjectMemoryKey) Normalized() ProjectMemoryKey {
	k.RepoID = strings.TrimSpace(k.RepoID)
	k.Type = ProjectMemoryType(strings.ToLower(strings.TrimSpace(string(k.Type))))
	k.Scope = ProjectMemoryScope(strings.ToLower(strings.TrimSpace(string(k.Scope))))
	k.Key = strings.TrimSpace(k.Key)
	return k
}

// ID is the deterministic row identity for the key. It is a hash rather than a
// concatenation so an arbitrarily long module path or symbol id still yields a
// fixed-width primary key, and so a key containing the separator cannot forge
// a different item's identity.
func (k ProjectMemoryKey) ID() string {
	n := k.Normalized()
	h := sha256.New()
	for _, part := range []string{
		fmt.Sprintf("v%d", ProjectMemorySchemaVersion),
		string(n.ProjectID),
		n.RepoID,
		string(n.Type),
		string(n.Scope),
		n.Key,
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// String renders the key for logs and operator output.
func (k ProjectMemoryKey) String() string {
	n := k.Normalized()
	if n.Key == "" {
		return fmt.Sprintf("%s/%s:%s", n.RepoID, n.Type, n.Scope)
	}
	return fmt.Sprintf("%s/%s:%s:%s", n.RepoID, n.Type, n.Scope, n.Key)
}

// Validate rejects a key that cannot address a row.
func (k ProjectMemoryKey) Validate() error {
	n := k.Normalized()
	if strings.TrimSpace(string(n.ProjectID)) == "" {
		return fmt.Errorf("%w: project id is required", ErrProjectMemoryInvalid)
	}
	if n.RepoID == "" {
		return fmt.Errorf("%w: repo id is required", ErrProjectMemoryInvalid)
	}
	if string(n.Type) == "" {
		return fmt.Errorf("%w: type is required", ErrProjectMemoryInvalid)
	}
	if !n.Scope.Valid() {
		return fmt.Errorf("%w: unknown scope %q", ErrProjectMemoryInvalid, n.Scope)
	}
	if n.Scope != MemoryScopeProject && n.Key == "" {
		return fmt.Errorf("%w: scope %q requires a key", ErrProjectMemoryInvalid, n.Scope)
	}
	return nil
}

// ProjectMemoryRepoID derives the stable repository identity used by RepoID
// from a repository's absolute path.
//
// It is a hash of the cleaned path rather than the path itself for two
// reasons. A path is not a safe primary-key component (length, separators,
// case), and — more importantly — an operator who moves a checkout must not
// silently inherit another repository's memory, which a truncated or
// sanitised path could allow. The path itself is stored alongside, in
// ProjectMemoryIndexState.RepoPath, so the identity stays explainable.
//
// A worktree is deliberately NOT its own repository: callers pass the
// canonical repository root, so an isolated worktree contributes task-scoped
// memory to the repository it belongs to instead of forking a parallel
// permanent memory of its own. See ProjectMemoryOrigin.
func ProjectMemoryRepoID(repoPath string) string {
	clean := strings.TrimRight(strings.TrimSpace(repoPath), "/")
	if clean == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(clean))
	return "repo_" + hex.EncodeToString(sum[:])[:24]
}

// ProjectMemoryOrigin says where a fact came from and therefore whether it may
// be treated as canonical repository knowledge.
//
// This is the multi-repo/worktree rule in one field. A fact derived inside an
// isolated task worktree describes changes that are not integrated anywhere;
// promoting it to canonical would let one task's unmerged opinion become every
// later task's premise. Such a fact is stored as OriginTaskLocal and is served
// only back to the task that produced it, until an integration authority
// promotes it.
type ProjectMemoryOrigin string

// Project memory origins.
const (
	// OriginCanonical is a fact derived from the repository's integrated
	// state, at a commit reachable from the repository's own history.
	OriginCanonical ProjectMemoryOrigin = "canonical"
	// OriginTaskLocal is a fact derived inside one task's worktree, from
	// changes that are not integrated. It never enters another task's context.
	OriginTaskLocal ProjectMemoryOrigin = "task_local"
)

// Valid reports whether the origin is one this build writes.
func (o ProjectMemoryOrigin) Valid() bool {
	return o == OriginCanonical || o == OriginTaskLocal
}

// ProjectMemoryItem is one durable, bounded fact about one project.
type ProjectMemoryItem struct {
	// ID is the derived identity (see ProjectMemoryKey.ID). A caller that
	// supplies it has it overwritten, so two writers cannot disagree about
	// which row they are addressing.
	ID string
	// Key is the natural key this item is addressed by.
	Key ProjectMemoryKey
	// Origin says whether the fact is canonical repository knowledge or one
	// task's unintegrated view (see ProjectMemoryOrigin).
	Origin ProjectMemoryOrigin
	// OriginRef names the task/worktree an OriginTaskLocal fact belongs to. It
	// is empty exactly when Origin is OriginCanonical.
	OriginRef string
	// Summary is the one-line form. Selection ranks and renders on it, and a
	// pack that cannot afford the body still carries the summary, so a bounded
	// pack degrades to less detail rather than to silence.
	Summary string
	// Content is the bounded body. It is text an agent can be handed as-is,
	// never a transcript and never a whole file — see MaxProjectMemoryContent.
	Content string
	// SourcePaths are the repo-relative paths the fact was derived from,
	// sorted and de-duplicated. They are the anchor for incremental
	// invalidation: a changed path invalidates exactly the items that name it.
	SourcePaths []string
	// SourceCommit is the commit the fact was derived at.
	SourceCommit string
	// SourceDigest is a hash over the content of SourcePaths as they stood at
	// SourceCommit. It is what lets drift detection answer "did my sources
	// actually move" without re-deriving the fact, and it is the reason a
	// branch that moves without touching the sources does not invalidate
	// anything.
	SourceDigest string
	// Generation is the indexing generation that last wrote this row. Every
	// update is conditional on it: a writer carrying an older generation is
	// refused, so a slow pass can never overwrite a newer one.
	Generation int64
	// State is whether the fact may be served (see ProjectMemoryState).
	State ProjectMemoryState
	// StateReason says why the item is not valid, and is empty exactly when
	// State is MemoryStateValid.
	StateReason string
	// Confidence is how much weight the fact deserves, in [0,1]. It is set
	// from what the producer could actually observe, never as a flat default
	// that would make a guess look like a measurement.
	Confidence float64
	// CreatedAt is when the row first appeared; it survives every later
	// update of the same identity.
	CreatedAt time.Time
	// UpdatedAt is when its content or provenance last actually changed. An
	// upsert that changes nothing does not move it.
	UpdatedAt time.Time
	// InvalidatedAt is when the item left MemoryStateValid, and is zero
	// exactly when State is MemoryStateValid.
	InvalidatedAt time.Time
	// Metadata is small, structured, provider-neutral annotation. It is not a
	// second content channel: see MaxProjectMemoryMetadata.
	Metadata map[string]string
	// ContentHash covers Summary, Content and the fields that say what the
	// content is about. It is what makes ingestion idempotent.
	ContentHash string

	// --- P2-D provenance and authority ----------------------------------
	//
	// State (above) says whether the fact's EVIDENCE still holds. The five
	// fields below say whether its LICENCE does, and they are what a fact has
	// to be able to answer before it is served as current:
	//
	//	what is my source of truth   -> ProvenanceKind
	//	which repository am I about  -> RepoIdentity
	//	what commit supports me      -> SourceCommit / VerifiedCommit / IntegratedCommit
	//	which generation made me      -> Generation
	//	what authorized my promotion -> PromotionAuthority
	//
	// A row that cannot answer the ones its ProvenanceKind requires is
	// Authority = AuthorityUnprovable and is withheld. See
	// project_memory_authority.go for why this is a second axis rather than
	// more values of State.

	// Authority is whether the fact's licence is still provable. Serving
	// requires it to be AuthorityAuthoritative, so an unrecognised or unset
	// value withholds rather than serves.
	Authority MemoryAuthority
	// AuthorityReason is the class-prefixed explanation (see
	// MemoryAuthorityReason), empty exactly when Authority is
	// AuthorityAuthoritative.
	AuthorityReason string
	// RepoIdentity is the durable repository identity the fact was derived
	// under. Empty means AO could not identify the checkout, which never
	// matches — including another empty identity.
	RepoIdentity RepoIdentity
	// ProvenanceKind says which proof applies to this row.
	ProvenanceKind MemoryProvenanceKind
	// PromotionAuthority is the workflow_mutation_provenance row that licensed
	// this fact's promotion to canonical, when one did. Empty for facts no
	// promotion produced, and for promotions AO could not prove — which are
	// also Authority = AuthorityUnprovable, so the two are never confused.
	PromotionAuthority string
	// VerifiedCommit is the commit verification actually passed on, and
	// IntegratedCommit the target-branch commit the work became part of. They
	// are separate from SourceCommit and from each other because they license
	// different things: a worktree result has a verified commit and no
	// integrated one until an integration is proven (P2-D §7, §13).
	VerifiedCommit   string
	IntegratedCommit string

	// EvidenceClass is how strong the claim is: something AO copied, something
	// it concluded, something a person stated, or something a verified
	// workflow established (see MemoryEvidenceClass). It is a THIRD axis, not
	// a refinement of the two above: a repo derivation whose authority is
	// intact can still be an inference, and a reader has to be told which.
	//
	// Empty means the row does not say. Every row written before P4-H is in
	// that state and nothing backfills it.
	EvidenceClass MemoryEvidenceClass
}

// Servable reports whether this fact may be handed to a role as current.
//
// It is the whole fail-closed rule, in one predicate that every reader goes
// through: the evidence must still hold AND the licence must still be
// provable. Neither alone is enough, and a value of either field that this
// build does not recognise fails both tests rather than passing by default.
func (i ProjectMemoryItem) Servable() bool {
	return i.State.Authoritative() && i.Authority.Provable()
}

// Bounds on one item. They are enforced in Validate rather than left to the
// producers, because "bounded and pragmatic" has to be a property of the
// store, not an intention of whoever last wrote an indexer.
const (
	// MaxProjectMemorySummary caps the one-line form.
	MaxProjectMemorySummary = 400
	// MaxProjectMemoryContent caps one item's body. It is deliberately small:
	// project memory holds what a file is FOR, not what it says. A producer
	// that needs more is summarising too little.
	MaxProjectMemoryContent = 8 * 1024
	// MaxProjectMemorySourcePaths caps how many paths one fact may claim to
	// be derived from. A fact derived from a hundred files is a fact about
	// the module, and should be written at module scope.
	MaxProjectMemorySourcePaths = 64
	// MaxProjectMemoryMetadata caps the annotation map, by entries and by the
	// length of one value.
	MaxProjectMemoryMetadata      = 16
	MaxProjectMemoryMetadataValue = 256
)

// Normalized returns the item with its free-text fields trimmed, its derived
// fields filled in, and its state annotations made self-consistent. It is
// applied before every write and after every read, so an item in memory and
// the same item on disk always agree.
func (i ProjectMemoryItem) Normalized() ProjectMemoryItem {
	i.Key = i.Key.Normalized()
	i.Summary = strings.TrimSpace(i.Summary)
	i.Content = strings.TrimSpace(i.Content)
	i.SourceCommit = strings.TrimSpace(i.SourceCommit)
	i.SourceDigest = strings.TrimSpace(i.SourceDigest)
	i.OriginRef = strings.TrimSpace(i.OriginRef)
	i.StateReason = strings.TrimSpace(i.StateReason)
	if i.Origin == "" {
		i.Origin = OriginCanonical
	}
	if i.Origin == OriginCanonical {
		i.OriginRef = ""
	}
	if i.State == "" {
		i.State = MemoryStateValid
	}
	if i.State == MemoryStateValid {
		i.StateReason = ""
		i.InvalidatedAt = time.Time{}
	}
	// An item that names no authority is authoritative: every writer that
	// predates P2-D, and every test fixture, means "nothing is wrong with
	// this". What withholds a fact is a validation pass that could not prove
	// it, not a field somebody forgot to set — and the legacy classification
	// (projectmemory.ClassifyLegacy) is the one place that turns "no
	// provenance recorded" into AuthorityLegacyUnprovable, deliberately, once.
	if i.Authority == "" {
		i.Authority = AuthorityAuthoritative
	}
	i.AuthorityReason = strings.TrimSpace(i.AuthorityReason)
	if i.Authority == AuthorityAuthoritative {
		i.AuthorityReason = ""
	}
	i.RepoIdentity = RepoIdentity(strings.TrimSpace(string(i.RepoIdentity)))
	i.PromotionAuthority = strings.TrimSpace(i.PromotionAuthority)
	i.VerifiedCommit = strings.TrimSpace(i.VerifiedCommit)
	i.IntegratedCommit = strings.TrimSpace(i.IntegratedCommit)
	// The evidence class is deliberately NOT defaulted. A producer that did
	// not say how strong its claim is has not made a weak claim, it has made
	// an unlabelled one, and picking a value here would put words in its
	// mouth — which is the whole failure mode the axis exists to prevent.
	i.EvidenceClass = MemoryEvidenceClass(strings.TrimSpace(string(i.EvidenceClass)))
	i.SourcePaths = NormalizeMemorySourcePaths(i.SourcePaths)
	i.Metadata = normalizeMemoryMetadata(i.Metadata)
	i.ContentHash = i.contentHash()
	i.ID = i.Identity()
	return i
}

// Identity is the row this item addresses.
//
// For canonical memory it is exactly the key's id, so a lookup by key finds
// the project's own fact. A task-local item is a DIFFERENT row: the same key
// derived inside one task's worktree is a claim about that task's branch, not
// about the project, and the two must be able to coexist. Folding the origin
// ref into the identity is what makes promotion (see PromoteTaskMemory) create
// the canonical fact rather than silently rewrite the task-local one in place —
// a bug that would then lose both when the task's memory was discarded.
func (i ProjectMemoryItem) Identity() string {
	base := i.Key.ID()
	if i.Origin != OriginTaskLocal || strings.TrimSpace(i.OriginRef) == "" {
		return base
	}
	h := sha256.New()
	h.Write([]byte(base))
	h.Write([]byte{0})
	h.Write([]byte(string(OriginTaskLocal)))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(i.OriginRef)))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// NormalizeMemorySourcePaths trims, drops empties, de-duplicates and sorts a
// source path list. Order is normalised rather than preserved because the list
// is a set: two producers that discovered the same sources in different orders
// have discovered the same provenance, and must hash identically.
func NormalizeMemorySourcePaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func normalizeMemoryMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// contentHash covers exactly what "the same fact" means: what it is about
// (the key), and what it says (summary, content, metadata).
//
// Provenance is deliberately excluded. A fact re-derived at a newer commit is
// the SAME fact, and an ingestion has to be able to see that its content did
// not move while still refreshing the commit and digest it was last confirmed
// at. Excluding provenance is what makes "re-index an unchanged file" cost one
// timestamp-free comparison instead of a rewrite.
func (i ProjectMemoryItem) contentHash() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	n := i.Key.Normalized()
	write(
		fmt.Sprintf("v%d", ProjectMemorySchemaVersion),
		string(n.ProjectID), n.RepoID, string(n.Type), string(n.Scope), n.Key,
		string(i.Origin), i.OriginRef,
		i.Summary, i.Content,
	)
	keys := make([]string, 0, len(i.Metadata))
	for k := range i.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, i.Metadata[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SameFactAs reports whether other carries the same content AND the same
// provenance as i — the test an upsert uses to decide that a write would
// change nothing at all. Timestamps and the derived id are excluded; the state
// annotation is excluded because it is decided by drift detection rather than
// supplied by an ingestion.
func (i ProjectMemoryItem) SameFactAs(other ProjectMemoryItem) bool {
	if i.ContentHash != other.ContentHash ||
		i.SourceCommit != other.SourceCommit ||
		i.SourceDigest != other.SourceDigest ||
		i.Confidence != other.Confidence ||
		len(i.SourcePaths) != len(other.SourcePaths) {
		return false
	}
	for n := range i.SourcePaths {
		if i.SourcePaths[n] != other.SourcePaths[n] {
			return false
		}
	}
	return true
}

// Validate checks everything an item cannot be stored without, including its
// bounds. It runs before every write, so an item that is in the database is
// one that names a project and a repository, says what kind of fact it is,
// carries a summary, states its confidence honestly, and fits.
func (i ProjectMemoryItem) Validate() error {
	if err := i.Key.Validate(); err != nil {
		return err
	}
	if !i.Origin.Valid() {
		return fmt.Errorf("%w: unknown origin %q", ErrProjectMemoryInvalid, i.Origin)
	}
	if i.Origin == OriginTaskLocal && strings.TrimSpace(i.OriginRef) == "" {
		return fmt.Errorf("%w: a task-local item must name the task it belongs to", ErrProjectMemoryInvalid)
	}
	if strings.TrimSpace(i.Summary) == "" {
		return fmt.Errorf("%w: summary is required", ErrProjectMemoryInvalid)
	}
	if len(i.Summary) > MaxProjectMemorySummary {
		return fmt.Errorf("%w: summary is %d bytes, over the %d cap",
			ErrProjectMemoryInvalid, len(i.Summary), MaxProjectMemorySummary)
	}
	if len(i.Content) > MaxProjectMemoryContent {
		return fmt.Errorf("%w: content is %d bytes, over the %d cap",
			ErrProjectMemoryInvalid, len(i.Content), MaxProjectMemoryContent)
	}
	if len(i.SourcePaths) > MaxProjectMemorySourcePaths {
		return fmt.Errorf("%w: %d source paths, over the %d cap",
			ErrProjectMemoryInvalid, len(i.SourcePaths), MaxProjectMemorySourcePaths)
	}
	if len(i.Metadata) > MaxProjectMemoryMetadata {
		return fmt.Errorf("%w: %d metadata entries, over the %d cap",
			ErrProjectMemoryInvalid, len(i.Metadata), MaxProjectMemoryMetadata)
	}
	for k, v := range i.Metadata {
		if len(v) > MaxProjectMemoryMetadataValue {
			return fmt.Errorf("%w: metadata %q is %d bytes, over the %d cap",
				ErrProjectMemoryInvalid, k, len(v), MaxProjectMemoryMetadataValue)
		}
	}
	if i.Confidence < 0 || i.Confidence > 1 {
		return fmt.Errorf("%w: confidence %v is outside [0,1]", ErrProjectMemoryInvalid, i.Confidence)
	}
	if !i.State.Valid() {
		return fmt.Errorf("%w: unknown state %q", ErrProjectMemoryInvalid, i.State)
	}
	// Empty is legal (a pre-P4-H row, or a producer that makes no claim about
	// its own strength). A value that is neither empty nor recognised is not:
	// it would render as a class a reader could misjudge.
	if i.EvidenceClass != "" && !i.EvidenceClass.Valid() {
		return fmt.Errorf("%w: unknown evidence class %q", ErrProjectMemoryInvalid, i.EvidenceClass)
	}
	if i.State != MemoryStateValid && strings.TrimSpace(i.StateReason) == "" {
		return fmt.Errorf("%w: a %s item must say why", ErrProjectMemoryInvalid, i.State)
	}
	if i.Generation < 0 {
		return fmt.Errorf("%w: generation %d is negative", ErrProjectMemoryInvalid, i.Generation)
	}
	return nil
}

// Bytes reports the item's on-the-wire cost as a context pack would render it.
// Selection budgets against this, so the budget it enforces is the budget the
// agent actually receives.
func (i ProjectMemoryItem) Bytes() int {
	return len(i.Summary) + len(i.Content) + len(i.Key.Key) + len(i.Key.Type)
}

// ProjectMemoryRelationKind names an edge in the memory graph.
//
// The vocabulary is chosen to be expressible in any property-graph backend —
// Grae, Graphify, or the in-tree relational default — so an adapter maps
// these names onto its own edge labels without AO learning that backend's
// model. Like the item type, it is open.
type ProjectMemoryRelationKind string

// Project memory relation kinds.
const (
	// RelationDependsOn is module -> module, or repository -> repository.
	RelationDependsOn ProjectMemoryRelationKind = "depends_on"
	// RelationImplements is file -> module.
	RelationImplements ProjectMemoryRelationKind = "implements"
	// RelationDefinedIn is symbol -> file.
	RelationDefinedIn ProjectMemoryRelationKind = "defined_in"
	// RelationChanged is task -> file.
	RelationChanged ProjectMemoryRelationKind = "changed"
	// RelationAffects is decision -> module.
	RelationAffects ProjectMemoryRelationKind = "affects"
	// RelationContains is the structural edge: repository -> module, module ->
	// file.
	RelationContains ProjectMemoryRelationKind = "contains"
	// RelationDerivedFrom links a memory item to the fact it was summarised
	// from, so a rebuild can find what to re-derive.
	RelationDerivedFrom ProjectMemoryRelationKind = "derived_from"
)

// ProjectMemoryNodeKind is what sort of thing an edge endpoint is. Endpoints
// are addressed by kind + key rather than by memory-item id, so a relation can
// name a module that has no summary item yet — the graph and the item set are
// allowed to be at different completeness, and neither blocks the other.
type ProjectMemoryNodeKind string

// Project memory node kinds.
const (
	NodeRepository ProjectMemoryNodeKind = "repository"
	NodeModule     ProjectMemoryNodeKind = "module"
	NodeFile       ProjectMemoryNodeKind = "file"
	NodeSymbol     ProjectMemoryNodeKind = "symbol"
	NodeTask       ProjectMemoryNodeKind = "task"
	NodeDecision   ProjectMemoryNodeKind = "decision"
)

// Valid reports whether the node kind is one this build writes.
func (k ProjectMemoryNodeKind) Valid() bool {
	switch k {
	case NodeRepository, NodeModule, NodeFile, NodeSymbol, NodeTask, NodeDecision,
		NodeKnowledge:
		return true
	default:
		return false
	}
}

// ProjectMemoryNode is one endpoint of a relation.
type ProjectMemoryNode struct {
	// Kind is what sort of thing this is.
	Kind ProjectMemoryNodeKind
	// Key names it within its kind and repository: a module path, a
	// repo-relative file path, a symbol id, a task id.
	Key string
}

// Normalized trims the node's parts.
func (n ProjectMemoryNode) Normalized() ProjectMemoryNode {
	n.Kind = ProjectMemoryNodeKind(strings.ToLower(strings.TrimSpace(string(n.Kind))))
	n.Key = strings.TrimSpace(n.Key)
	return n
}

// String renders the node for logs and operator output.
func (n ProjectMemoryNode) String() string {
	nn := n.Normalized()
	return string(nn.Kind) + ":" + nn.Key
}

// Validate rejects an endpoint that cannot be addressed.
func (n ProjectMemoryNode) Validate() error {
	nn := n.Normalized()
	if !nn.Kind.Valid() {
		return fmt.Errorf("%w: unknown node kind %q", ErrProjectMemoryInvalid, nn.Kind)
	}
	if nn.Key == "" {
		return fmt.Errorf("%w: node key is required", ErrProjectMemoryInvalid)
	}
	return nil
}

// ProjectMemoryRelation is one durable edge between two nodes.
//
// Relations carry the same provenance and the same generation fence as items,
// for the same reason: an edge asserted from a commit that no longer describes
// the repository is not evidence, and a slow indexer must not resurrect an
// edge a newer pass deleted.
type ProjectMemoryRelation struct {
	// ID is the derived identity (see RelationID).
	ID string
	// ProjectID and RepoID scope the edge, exactly as they scope an item. An
	// edge between two repositories is stored under the repository it is
	// asserted FROM.
	ProjectID ProjectID
	RepoID    string
	// From, Kind, To are the edge itself.
	From ProjectMemoryNode
	Kind ProjectMemoryRelationKind
	To   ProjectMemoryNode
	// Origin and OriginRef follow the item rule: an edge discovered inside an
	// isolated worktree is task-local until an authority promotes it.
	Origin    ProjectMemoryOrigin
	OriginRef string
	// SourcePaths, SourceCommit and SourceDigest are the edge's provenance.
	SourcePaths  []string
	SourceCommit string
	SourceDigest string
	// Generation is the CAS fence.
	Generation int64
	// State and StateReason follow ProjectMemoryState.
	State       ProjectMemoryState
	StateReason string
	// Confidence is how much weight the edge deserves, in [0,1].
	Confidence float64
	// CreatedAt, UpdatedAt, InvalidatedAt mirror the item timestamps.
	CreatedAt     time.Time
	UpdatedAt     time.Time
	InvalidatedAt time.Time
	// Metadata is small, structured annotation.
	Metadata map[string]string

	// Authority, AuthorityReason and RepoIdentity mirror the item fields, for
	// the reason P2-D §23 gives: an edge derived from a node whose licence has
	// gone must stop being traversed as current. It is retired, never deleted
	// — the record that two facts were once related is what an operator reads
	// when asking why a decision was made.
	Authority       MemoryAuthority
	AuthorityReason string
	RepoIdentity    RepoIdentity
}

// Traversable reports whether this edge may be followed as current. Same
// composition as ProjectMemoryItem.Servable, for the same reason.
func (r ProjectMemoryRelation) Traversable() bool {
	return r.State.Authoritative() && r.Authority.Provable()
}

// RelationID derives an edge's row identity from everything that makes it the
// same edge: the project, the repository, both endpoints, the kind, and the
// origin. Origin participates so a task-local assertion of an edge cannot
// overwrite the canonical one.
func RelationID(
	projectID ProjectID, repoID string,
	from ProjectMemoryNode, kind ProjectMemoryRelationKind, to ProjectMemoryNode,
	origin ProjectMemoryOrigin, originRef string,
) string {
	h := sha256.New()
	for _, part := range []string{
		fmt.Sprintf("v%d", ProjectMemorySchemaVersion),
		string(projectID),
		strings.TrimSpace(repoID),
		from.Normalized().String(),
		strings.ToLower(strings.TrimSpace(string(kind))),
		to.Normalized().String(),
		string(origin),
		strings.TrimSpace(originRef),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Normalized fills in the relation's derived and defaulted fields.
func (r ProjectMemoryRelation) Normalized() ProjectMemoryRelation {
	r.RepoID = strings.TrimSpace(r.RepoID)
	r.From = r.From.Normalized()
	r.To = r.To.Normalized()
	r.Kind = ProjectMemoryRelationKind(strings.ToLower(strings.TrimSpace(string(r.Kind))))
	r.SourceCommit = strings.TrimSpace(r.SourceCommit)
	r.SourceDigest = strings.TrimSpace(r.SourceDigest)
	r.OriginRef = strings.TrimSpace(r.OriginRef)
	r.StateReason = strings.TrimSpace(r.StateReason)
	if r.Origin == "" {
		r.Origin = OriginCanonical
	}
	if r.Origin == OriginCanonical {
		r.OriginRef = ""
	}
	if r.State == "" {
		r.State = MemoryStateValid
	}
	if r.State == MemoryStateValid {
		r.StateReason = ""
		r.InvalidatedAt = time.Time{}
	}
	if r.Authority == "" {
		r.Authority = AuthorityAuthoritative
	}
	r.AuthorityReason = strings.TrimSpace(r.AuthorityReason)
	if r.Authority == AuthorityAuthoritative {
		r.AuthorityReason = ""
	}
	r.RepoIdentity = RepoIdentity(strings.TrimSpace(string(r.RepoIdentity)))
	r.SourcePaths = NormalizeMemorySourcePaths(r.SourcePaths)
	r.Metadata = normalizeMemoryMetadata(r.Metadata)
	r.ID = RelationID(r.ProjectID, r.RepoID, r.From, r.Kind, r.To, r.Origin, r.OriginRef)
	return r
}

// Validate rejects an edge that cannot be stored.
func (r ProjectMemoryRelation) Validate() error {
	if strings.TrimSpace(string(r.ProjectID)) == "" {
		return fmt.Errorf("%w: project id is required", ErrProjectMemoryInvalid)
	}
	if strings.TrimSpace(r.RepoID) == "" {
		return fmt.Errorf("%w: repo id is required", ErrProjectMemoryInvalid)
	}
	if err := r.From.Validate(); err != nil {
		return fmt.Errorf("relation from: %w", err)
	}
	if err := r.To.Validate(); err != nil {
		return fmt.Errorf("relation to: %w", err)
	}
	if strings.TrimSpace(string(r.Kind)) == "" {
		return fmt.Errorf("%w: relation kind is required", ErrProjectMemoryInvalid)
	}
	if !r.Origin.Valid() {
		return fmt.Errorf("%w: unknown origin %q", ErrProjectMemoryInvalid, r.Origin)
	}
	if r.Origin == OriginTaskLocal && strings.TrimSpace(r.OriginRef) == "" {
		return fmt.Errorf("%w: a task-local relation must name the task it belongs to", ErrProjectMemoryInvalid)
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf("%w: confidence %v is outside [0,1]", ErrProjectMemoryInvalid, r.Confidence)
	}
	if !r.State.Valid() {
		return fmt.Errorf("%w: unknown state %q", ErrProjectMemoryInvalid, r.State)
	}
	if r.State != MemoryStateValid && strings.TrimSpace(r.StateReason) == "" {
		return fmt.Errorf("%w: a %s relation must say why", ErrProjectMemoryInvalid, r.State)
	}
	if len(r.SourcePaths) > MaxProjectMemorySourcePaths {
		return fmt.Errorf("%w: %d source paths, over the %d cap",
			ErrProjectMemoryInvalid, len(r.SourcePaths), MaxProjectMemorySourcePaths)
	}
	if r.Generation < 0 {
		return fmt.Errorf("%w: generation %d is negative", ErrProjectMemoryInvalid, r.Generation)
	}
	return nil
}

// ProjectMemoryIndexPhase is where an indexing pass has got to.
//
// The phase is durable, and it is what makes indexing restart-safe: a pass
// that dies mid-scan leaves a row saying which phase it was in and which path
// it had reached, so a restart resumes from there instead of starting over.
type ProjectMemoryIndexPhase string

// Project memory index phases.
const (
	// IndexPhaseIdle is a repository with no pass in flight. It is the phase
	// a completed pass returns to.
	IndexPhaseIdle ProjectMemoryIndexPhase = "idle"
	// IndexPhaseScanning is walking the tree and admitting files.
	IndexPhaseScanning ProjectMemoryIndexPhase = "scanning"
	// IndexPhaseSummarizing is deriving items from the admitted files.
	IndexPhaseSummarizing ProjectMemoryIndexPhase = "summarizing"
	// IndexPhaseLinking is writing the relations.
	IndexPhaseLinking ProjectMemoryIndexPhase = "linking"
	// IndexPhaseFinalizing is retiring what the pass superseded.
	IndexPhaseFinalizing ProjectMemoryIndexPhase = "finalizing"
	// IndexPhaseFailed is a pass that stopped on an error. The generation is
	// kept so the failure is diagnosable, and a later pass takes a new one.
	IndexPhaseFailed ProjectMemoryIndexPhase = "failed"
)

// Terminal reports whether the phase is one no pass is running in.
func (p ProjectMemoryIndexPhase) Terminal() bool {
	return p == IndexPhaseIdle || p == IndexPhaseFailed
}

// Valid reports whether the phase is one this build writes.
func (p ProjectMemoryIndexPhase) Valid() bool {
	switch p {
	case IndexPhaseIdle, IndexPhaseScanning, IndexPhaseSummarizing,
		IndexPhaseLinking, IndexPhaseFinalizing, IndexPhaseFailed:
		return true
	default:
		return false
	}
}

// ProjectMemoryIndexState is the durable record of one repository's indexing.
//
// There is exactly one row per (project, repo), and it is both the resume
// point and the generation allocator: taking the next generation and claiming
// the pass is a single conditional write, which is what stops two daemons — or
// a daemon and a CLI — from indexing the same repository concurrently.
type ProjectMemoryIndexState struct {
	ProjectID ProjectID
	RepoID    string
	// RepoPath is the absolute repository root the RepoID was derived from.
	// It is stored so the hashed identity stays explainable, and so a moved
	// checkout is detectable rather than silently mismatched.
	RepoPath string
	// Generation is the newest generation allocated for this repository. It
	// only ever increases.
	Generation int64
	// Phase is where the current (or last) pass got to.
	Phase ProjectMemoryIndexPhase
	// IndexedCommit is the commit the last COMPLETED pass indexed at. It is
	// what incremental update diffs from, and it is not advanced by a pass
	// that failed.
	IndexedCommit string
	// PendingCommit is the commit the in-flight pass is indexing at. It
	// becomes IndexedCommit when the pass completes.
	PendingCommit string
	// Branch is the branch the last pass observed, recorded so a branch move
	// is visible in `ao memory status` even when it changed no sources.
	Branch string
	// Cursor is the resume point within the current phase — the last path the
	// scan admitted. A restart continues after it.
	Cursor string
	// FilesSeen, FilesIndexed and FilesSkipped are the pass's audit trail:
	// how many paths the walk visited, how many were summarised, and how many
	// were left alone because their digest still matched.
	FilesSeen    int
	FilesIndexed int
	FilesSkipped int
	// ItemsWritten and RelationsWritten count what the pass produced.
	ItemsWritten     int
	RelationsWritten int
	// LastError is the failure that ended a IndexPhaseFailed pass.
	LastError string
	// StartedAt is when the current or last pass began; CompletedAt when it
	// finished, zero while one is in flight.
	StartedAt   time.Time
	CompletedAt time.Time
	UpdatedAt   time.Time
}

// Running reports whether a pass is in flight for this repository.
func (s ProjectMemoryIndexState) Running() bool { return !s.Phase.Terminal() }

// ProjectMemoryCounts is the per-state item census the status surfaces report.
type ProjectMemoryCounts struct {
	Total       int
	Valid       int
	Stale       int
	Invalidated int
	Rebuilding  int
	// TaskLocal counts items whose Origin is OriginTaskLocal, across every
	// state. They are reported separately because they are deliberately not
	// part of the canonical memory a new task would see.
	TaskLocal int
	Relations int
}

// ProjectMemoryStatus is the operator-facing view of one repository's memory:
// what generation it is at, what commit it was indexed from, how many facts it
// holds and in what condition, and whether a pass is running.
type ProjectMemoryStatus struct {
	ProjectID ProjectID
	RepoID    string
	RepoPath  string
	Index     ProjectMemoryIndexState
	Counts    ProjectMemoryCounts
	// ByType is the item census per type, so an operator can see at a glance
	// that (say) conventions were indexed and modules were not.
	ByType map[ProjectMemoryType]int
	// LastIndexedAt is the completion time of the newest successful pass.
	LastIndexedAt time.Time
	// LastUpdatedAt is when any item or relation last changed.
	LastUpdatedAt time.Time
}

// Healthy reports whether the memory for this repository may be relied on as a
// whole: a pass has completed at least once, none is currently failed, and at
// least one valid item exists. A caller that gets false should fall back to its
// pre-memory behaviour rather than send a thinner context.
func (s ProjectMemoryStatus) Healthy() bool {
	return s.Index.Phase != IndexPhaseFailed &&
		s.Index.IndexedCommit != "" &&
		s.Counts.Valid > 0
}

// MemorySourceDigest hashes a set of (path, content-hash) pairs into the single
// SourceDigest an item or relation carries.
//
// It is a function on the domain rather than on an indexer because drift
// detection and indexing must compute it identically: the whole point of the
// digest is that a later pass can recompute it from the repository and compare,
// and a second implementation is a second chance to disagree.
//
// The input is sorted by path before hashing, so producers that discovered the
// same sources in different orders agree.
func MemorySourceDigest(pairs map[string]string) string {
	if len(pairs) == 0 {
		return ""
	}
	paths := make([]string, 0, len(pairs))
	for p := range pairs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	// hash.Hash.Write never returns an error; the discard is the idiom used
	// throughout this file rather than a swallowed failure.
	_, _ = fmt.Fprintf(h, "v%d\x00", ProjectMemorySchemaVersion)
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(pairs[p]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
