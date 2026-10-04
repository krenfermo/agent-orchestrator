package domain

// usage_context.go — what AO ASSEMBLED, kept rigorously apart from what a
// provider REPORTED.
//
// THE DISTINCTION THAT MAKES THIS HONEST. AO can measure, exactly, the bytes it
// put into a prompt. It can estimate tokens from those bytes with a documented
// heuristic. It CANNOT see what a coding harness reads inside the worktree
// afterwards, nor how the provider tokenizes what it received — so
// AO-assembled context and provider input tokens are two different quantities
// that must never be added, compared as equals, or presented as one number.
// They live in separate structs for that reason, and the API keeps them in
// separate blocks.
//
// AND WHAT A "SAVING" IS ALLOWED TO MEAN. AO may only claim to have avoided
// context where it has a comparable baseline: a document project memory
// demonstrably REPLACED (dedupe), or candidate content the router assembled and
// then did not send. Both are measured in AO-assembled bytes, which is why the
// field is named AvoidedAssembledBytes and its token view is
// EstimatedAvoidedTokens. Neither is a claim that a provider billed fewer
// tokens: a smaller prompt very likely costs less, but AO did not measure that
// and does not say it did.

// ContextSourceKind names where a slice of AO-assembled context came from.
type ContextSourceKind string

// ContextSourceKind values. They partition what AO put into a prompt; anything
// AO could not attribute stays in ContextSourceOther rather than being spread
// across the others.
const (
	// ContextSourceTaskSpec is the objective, task text and acceptance
	// criteria — the part memory never replaces.
	ContextSourceTaskSpec ContextSourceKind = "task_spec"
	// ContextSourceProjectMemory is the durable project-memory pack.
	ContextSourceProjectMemory ContextSourceKind = "project_memory"
	// ContextSourceSharedKnowledge is task- and workflow-local knowledge
	// carried forward from earlier tasks. A subset of the memory pack's bytes,
	// reported separately because "did this task reuse a sibling's finding" is
	// its own question.
	ContextSourceSharedKnowledge ContextSourceKind = "shared_knowledge"
	// ContextSourceRepoContent is repository content read for this dispatch.
	ContextSourceRepoContent ContextSourceKind = "repo_content"
	// ContextSourceIndexReuse is content drawn from a store AO had already
	// built (code graph, memory index) rather than read fresh.
	ContextSourceIndexReuse ContextSourceKind = "index_reuse"
	// ContextSourceOther is measured context AO could not attribute further.
	ContextSourceOther ContextSourceKind = "other"
)

// ContextSourceLine is one source's contribution to a run's assembled context.
type ContextSourceLine struct {
	Source ContextSourceKind
	// Bytes is MEASURED. EstimatedTokens is derived from it by AO's own
	// bytes-per-token heuristic and is labeled estimated everywhere it is
	// shown — AO runs no provider tokenizer.
	Bytes           int64
	EstimatedTokens int64
}

// ContextMemoryView is what project memory did for a run.
type ContextMemoryView struct {
	// Mode is the rollout stage in force: "off", "assisted" or "preferred".
	// Empty when no dispatch recorded one.
	Mode string
	// Generation and IndexedCommit are the provenance of the memory served. An
	// empty commit means AO had memory it could not vouch for and did not use.
	Generation    int64
	IndexedCommit string

	// PackItems / PackBytes / EstimatedPackTokens are what was actually
	// attached, summed across dispatches.
	PackItems           int64
	PackBytes           int64
	EstimatedPackTokens int64
	// Candidates and RejectedByBudget are what selection chose from and what
	// the budget excluded — the pair that shows a budget is doing work rather
	// than never binding.
	Candidates       int64
	RejectedByBudget int64
	StaleExcluded    int64

	// CacheHits / CacheMisses count pack-cache outcomes across dispatches.
	CacheHits   int64
	CacheMisses int64

	// Syncs, FullSyncs, IncrementalSyncs and NoOpSyncs describe freshness work.
	// A warm project's normal path is a no-op, and that is what NoOpSyncs
	// proves.
	Syncs            int64
	FullSyncs        int64
	IncrementalSyncs int64
	NoOpSyncs        int64
	SyncFilesRead    int64

	// SharedCandidates / SharedSelected / SharedExcluded describe task-produced
	// knowledge reuse. Both halves matter: a task working next to an earlier
	// one should show candidates it took, an unrelated task candidates it
	// excluded, and neither claim can be made from one number.
	SharedCandidates int64
	SharedSelected   int64
	SharedExcluded   int64
	// TaskLocalItems / WorkflowLocalItems / CanonicalItems say which scope the
	// facts came from.
	TaskLocalItems     int64
	WorkflowLocalItems int64
	CanonicalItems     int64

	// FallbackReasons lists, in words, why memory contributed less than it
	// might have on some dispatch.
	FallbackReasons []string
}

// ContextCompositionView is a run's whole AO-assembled context story.
type ContextCompositionView struct {
	// Dispatches is how many dispatch records this view summarizes, and
	// Unmeasured how many carried a size AO could not measure. A nonzero
	// Unmeasured makes every total below a LOWER BOUND, and the API says so.
	Dispatches int64
	Unmeasured int64
	// SkippedRecords counts evidence files that could not be read at all.
	SkippedRecords int64

	// AssembledBytes is the measured total AO handed providers across those
	// dispatches; EstimatedAssembledTokens is that figure in tokens, ESTIMATED.
	AssembledBytes           int64
	EstimatedAssembledTokens int64

	// ByRole and BySource break the same total down. They are two views of one
	// number, never two numbers to add.
	ByRole   []ContextRoleLine
	BySource []ContextSourceLine

	// AvoidedAssembledBytes is context AO demonstrably did NOT assemble: memory
	// that replaced an equivalent document plus router candidates that were
	// assembled and not sent. EstimatedAvoidedTokens is its token view.
	//
	// AvoidedComparable is false when no baseline supports the claim at all,
	// and the API must then present NO saving rather than zero — "we avoided
	// nothing" and "nothing here can be compared" are different findings.
	AvoidedAssembledBytes  int64
	EstimatedAvoidedTokens int64
	AvoidedComparable      bool

	// Memory is what project memory did.
	Memory ContextMemoryView

	// EstimateMethod names the bytes-per-token heuristic behind every token
	// figure here, so a reader can see it is not a provider tokenizer.
	EstimateMethod string

	// Recorded is false when the run has no evidence at all. The UI must then
	// say "no context data recorded", never render zeroes.
	Recorded bool
}

// ContextRoleLine is one role's share of the assembled context.
type ContextRoleLine struct {
	Role                     WorkflowRole
	Dispatches               int64
	AssembledBytes           int64
	EstimatedAssembledTokens int64
}
