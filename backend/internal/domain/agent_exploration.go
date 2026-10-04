package domain

import (
	"path"
	"strings"
	"time"
)

// agent_exploration.go -- Frente 3 / 3C: what an agent LOOKED AT, beside what
// it cost.
//
// The usage ledger answers how many tokens a run spent and, since 0169, what
// kind of turn each provider call was. It cannot answer the question Project
// Memory exists to change: which files did the agent open, how many times,
// how many searches did it run before its first edit, and how much of the
// context it paid for was put there by AO, by the harness, or by the agent's
// own exploration. A tool observation is one answer to that, taken from the
// same provider transcript the usage parser already tails.
//
// WHAT IS KEPT. The operation (a closed vocabulary), the tool's name, where
// the target lies relative to the project (a closed vocabulary), the
// project-relative path when -- and only when -- it lies inside the project
// and passes the 3B repository boundary, and how many bytes the harness handed
// back to the model. WHAT IS NEVER KEPT: file content, tool arguments other
// than that one normalised path, search patterns, commands, prompts, results,
// and any path outside the project root.

// ToolObservationOrigin says whose decision put this material into the
// agent's context. It is the axis the 3D experiment compares: Project Memory
// moves bytes from AgentExploration into AOContext.
type ToolObservationOrigin string

// ToolObservationOrigin values.
const (
	// OriginAgentExploration -- the model chose to call a tool.
	OriginAgentExploration ToolObservationOrigin = "agent_exploration"
	// OriginAOContext -- a prompt delivered into the agent's conversation.
	// In an AO-owned pane that is AO's own dispatch or nudge; a human typing
	// into the same pane is indistinguishable in the transcript, which is why
	// every figure built on it is DERIVED and never OBSERVED.
	OriginAOContext ToolObservationOrigin = "ao_context"
	// OriginHarnessContext -- material the harness injected by itself:
	// instruction files, memory files, environment snapshots, system prompts.
	OriginHarnessContext ToolObservationOrigin = "harness_context"
	// OriginUnknown -- AO saw the material and cannot say who put it there.
	OriginUnknown ToolObservationOrigin = "unknown"
)

// Valid reports whether o is part of the closed vocabulary.
func (o ToolObservationOrigin) Valid() bool {
	switch o {
	case OriginAgentExploration, OriginAOContext, OriginHarnessContext, OriginUnknown:
		return true
	}
	return false
}

// ToolOp is what one observation DID. Closed, so every read model can count
// it without interpreting a free-text name.
type ToolOp string

// ToolOp values.
const (
	// ToolOpRead -- a structured file read (Claude Read / NotebookRead).
	ToolOpRead ToolOp = "read"
	// ToolOpSearch -- a structured content search (Claude Grep).
	ToolOpSearch ToolOp = "search"
	// ToolOpList -- a structured file listing (Claude Glob / LS).
	ToolOpList ToolOp = "list"
	// ToolOpCommandExplore -- a shell command whose leading program is a
	// read-only inspection tool (cat, rg, sed -n, git log ...). DERIVED from
	// the program NAME only; the command itself is never kept, and no path
	// is extracted from it.
	ToolOpCommandExplore ToolOp = "command_explore"
	// ToolOpCommandEdit -- a shell command that writes files (sed -i, an
	// output redirect, tee, patch, git apply). DERIVED from the program name
	// and redirect syntax only; no path is extracted.
	ToolOpCommandEdit ToolOp = "command_edit"
	// ToolOpCommand -- any other shell or code execution.
	ToolOpCommand ToolOp = "command"
	// ToolOpEdit -- the workspace was mutated through a structured tool.
	ToolOpEdit ToolOp = "edit"
	// ToolOpDelegate -- work was delegated into a separate context.
	ToolOpDelegate ToolOp = "delegate"
	// ToolOpWeb -- the network, not the repository, was consulted.
	ToolOpWeb ToolOp = "web"
	// ToolOpWait -- polling something already started.
	ToolOpWait ToolOp = "wait"
	// ToolOpPlan -- plan/task bookkeeping.
	ToolOpPlan ToolOp = "plan"
	// ToolOpOther -- a tool AO does not recognise (MCP tools, new harness
	// tools). Counted, never guessed into a class.
	ToolOpOther ToolOp = "other"
	// ToolOpPrompt -- a prompt entering the conversation (OriginAOContext).
	ToolOpPrompt ToolOp = "prompt"
	// ToolOpInjected -- harness-injected material (OriginHarnessContext).
	ToolOpInjected ToolOp = "injected"
)

// Valid reports whether op is part of the closed vocabulary.
func (op ToolOp) Valid() bool {
	switch op {
	case ToolOpRead, ToolOpSearch, ToolOpList, ToolOpCommandExplore, ToolOpCommand,
		ToolOpCommandEdit, ToolOpEdit, ToolOpDelegate, ToolOpWeb, ToolOpWait, ToolOpPlan, ToolOpOther,
		ToolOpPrompt, ToolOpInjected:
		return true
	}
	return false
}

// IsExploration reports whether op inspects the repository without changing
// it -- the operations Project Memory is meant to make unnecessary.
func (op ToolOp) IsExploration() bool {
	switch op {
	case ToolOpRead, ToolOpSearch, ToolOpList, ToolOpCommandExplore:
		return true
	}
	return false
}

// ToolPathScope says where an observation's target lies. Only
// ToolPathProject ever carries a path.
type ToolPathScope string

// ToolPathScope values.
const (
	// ToolPathNone -- the operation names no file (a shell command, a prompt).
	ToolPathNone ToolPathScope = "none"
	// ToolPathProject -- inside the project root and eligible under the 3B
	// boundary. The only scope that stores a path.
	ToolPathProject ToolPathScope = "project"
	// ToolPathSecret -- inside the project, and a secret by the shared 3B
	// policy (.env, keys, credential stores). Counted; never named.
	ToolPathSecret ToolPathScope = "secret"
	// ToolPathExcluded -- inside the project under a directory AO never
	// indexes (.git, node_modules, agent scratch). Counted; never named.
	ToolPathExcluded ToolPathScope = "excluded"
	// ToolPathOutsideHarness -- outside the project, inside the harness's own
	// configuration home (the directory the transcript itself lives under).
	ToolPathOutsideHarness ToolPathScope = "outside_harness"
	// ToolPathOutside -- anywhere else outside the project. Counted; never
	// named.
	ToolPathOutside ToolPathScope = "outside"
	// ToolPathUnresolved -- a path was named but AO could not place it: no
	// known project root, a relative path with no trustworthy base, or a
	// malformed value.
	ToolPathUnresolved ToolPathScope = "unresolved"
)

// Valid reports whether s is part of the closed vocabulary.
func (s ToolPathScope) Valid() bool {
	switch s {
	case ToolPathNone, ToolPathProject, ToolPathSecret, ToolPathExcluded,
		ToolPathOutsideHarness, ToolPathOutside, ToolPathUnresolved:
		return true
	}
	return false
}

// MaxToolObservationPathBytes bounds a stored project-relative path. A longer
// value is recorded as unresolved rather than truncated into a different path.
const MaxToolObservationPathBytes = 512

// MaxToolNameBytes bounds a stored tool name.
const MaxToolNameBytes = 96

// AgentToolObservation is one thing that entered, or was fetched into, an
// agent's context: a tool call the model made, a prompt it was given, or
// material its harness injected.
type AgentToolObservation struct {
	// Key is the exactly-once identity within the binding, derived from the
	// artifact (a tool-call id, a record uuid) and never from a clock.
	Key string
	// EventKey is the SourceEventKey of the billed message that issued the
	// call, when the transcript links them. Empty means "not linkable" --
	// every Codex observation, and every non-tool observation.
	EventKey string
	// Ordinal orders observations within one transcript: the byte offset of
	// the record that carried it.
	Ordinal    int64
	ObservedAt *time.Time
	Origin     ToolObservationOrigin
	Op         ToolOp
	// ToolName is the harness's own name for the tool (or the injected
	// material's type). Validated to a conservative charset and length.
	ToolName  string
	PathScope ToolPathScope
	// Path is project-relative and slash-separated, set only for
	// ToolPathProject.
	Path string
	// ResultBytes is the size of what the harness handed back to the model
	// (or, for a prompt or injection, the size of the material itself). Nil
	// means AO did not observe it -- never zero.
	ResultBytes *int64
}

// AgentToolResult completes an observation once its result record arrives,
// which for a tool call is always a LATER transcript record, and possibly a
// later ingestion chunk.
type AgentToolResult struct {
	Key         string
	ResultBytes int64
	// ResultItems is the harness's own count of what came back (lines for a
	// read, files for a search or listing). Nil when the harness did not say.
	ResultItems *int64
	IsError     bool
}

// AgentToolFacts is the tool half of one ingestion chunk, written in the same
// transaction as the chunk's usage events and cursor.
type AgentToolFacts struct {
	Observations []AgentToolObservation
	Results      []AgentToolResult
	// ExtractorVersion names the classifier that produced these facts. The
	// store records it per source (agent_tool_coverage) together with the byte
	// range parsed, so a read model can tell "the extractor saw nothing" from
	// "the extractor never ran here". Zero means the facts claim no coverage.
	ExtractorVersion int64
}

// Empty reports whether there is nothing to write.
func (f AgentToolFacts) Empty() bool { return len(f.Observations) == 0 && len(f.Results) == 0 }

// canonicalToolNames is the CLOSED vocabulary of agent_tool_observations.
// tool_name: the harness tools AO classifies, the Claude Code attachment types
// it recognises, the generic "mcp" and "attachment", and the names of AO's own
// synthetic observations. The transcript parser maps every other name to ""
// before it gets here; the store refusing anything else is what makes the
// column closed at the persistence boundary too, not only in today's parser.
var canonicalToolNames = map[string]bool{
	"Agent":                       true,
	"Bash":                        true,
	"BashOutput":                  true,
	"Edit":                        true,
	"EnterPlanMode":               true,
	"ExitPlanMode":                true,
	"Glob":                        true,
	"Grep":                        true,
	"KillBash":                    true,
	"KillShell":                   true,
	"LS":                          true,
	"MultiEdit":                   true,
	"NotebookEdit":                true,
	"NotebookRead":                true,
	"Read":                        true,
	"Task":                        true,
	"TodoWrite":                   true,
	"WebFetch":                    true,
	"WebSearch":                   true,
	"Write":                       true,
	"agent_listing_delta":         true,
	"apply_patch":                 true,
	"attachment":                  true,
	"auto_mode":                   true,
	"base_instructions":           true,
	"codex_file_change":           true,
	"codex_parsed_cmd":            true,
	"compact_file_reference":      true,
	"compact_summary":             true,
	"container.exec":              true,
	"credential_org":              true,
	"date":                        true,
	"deferred_tools_delta":        true,
	"deferred_tools_record":       true,
	"developer_message":           true,
	"edited_text_file":            true,
	"environment":                 true,
	"exec":                        true,
	"exec_command":                true,
	"file":                        true,
	"hook_additional_context":     true,
	"hook_success":                true,
	"image_generation":            true,
	"instructions":                true,
	"js":                          true,
	"local_shell":                 true,
	"local_shell_call":            true,
	"mcp":                         true,
	"mcp_instructions_delta":      true,
	"mcp_tool_call_end":           true,
	"meta_message":                true,
	"model":                       true,
	"nested_memory":               true,
	"patch_apply_end":             true,
	"prompt_snapshot":             true,
	"queued_command":              true,
	"remote_session_change":       true,
	"session_context":             true,
	"shell":                       true,
	"silent_turn_reminder":        true,
	"skill_listing":               true,
	"spawn_agent":                 true,
	"str_replace_based_edit_tool": true,
	"str_replace_editor":          true,
	"task_status":                 true,
	"thinking_drop":               true,
	"todo_reminder":               true,
	"total_tokens_reminder":       true,
	"update_plan":                 true,
	"user_message":                true,
	"view_image":                  true,
	"wait":                        true,
	"web_search":                  true,
	"web_search_call":             true,
	"write_stdin":                 true,
}

// ValidToolName reports whether name may be stored: it must belong to the
// closed vocabulary (the empty name, meaning "other", is stored as "").
func ValidToolName(name string) bool {
	return len(name) <= MaxToolNameBytes && canonicalToolNames[name]
}

// Valid reports whether an observation may be persisted as is. The write path
// refuses anything else rather than repairing it, so a parser bug cannot put
// a path into a row whose scope says it has none.
func (o AgentToolObservation) Valid() bool {
	if strings.TrimSpace(o.Key) == "" || !o.Origin.Valid() || !o.Op.Valid() || !o.PathScope.Valid() {
		return false
	}
	if o.ToolName != "" && !ValidToolName(o.ToolName) {
		return false
	}
	if o.ResultBytes != nil && *o.ResultBytes < 0 {
		return false
	}
	if o.PathScope != ToolPathProject {
		return o.Path == ""
	}
	return ValidProjectRelativePath(o.Path)
}

// ValidProjectRelativePath reports whether p may be stored as a project path:
// slash-separated, relative, already clean (no `..` segment, no `//`), and
// free of anything that could make it name a place outside the project on
// some platform -- a backslash, a drive letter, a control character.
func ValidProjectRelativePath(p string) bool {
	if p == "" || len(p) > MaxToolObservationPathBytes || strings.HasPrefix(p, "/") {
		return false
	}
	if p != "." && path.Clean(p) != p {
		return false
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) {
		return false
	}
	for _, r := range p {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ExplorationBasis says how an exploration figure came to be known. It is
// the 3C wording of the rule every usage read model follows (a number AO
// could not measure is never presented as measured); Certainty maps it onto
// the shared MetricCertainty vocabulary so the two never drift apart.
type ExplorationBasis string

// ExplorationBasis values.
const (
	// ExplorationObserved -- reported by the provider transcript, or counted
	// exactly from facts it reported (a distinct-path count, a sum of
	// reported lengths). No heuristic involved.
	ExplorationObserved ExplorationBasis = "observed"
	// ExplorationDerived -- computed through an inference Method names: a
	// program-name classification, a time ordering across two sources, an
	// origin assumption, a bytes-to-tokens heuristic.
	ExplorationDerived ExplorationBasis = "derived"
	// ExplorationUnavailable -- the provider or harness does not expose it.
	// Value is nil; never zero.
	ExplorationUnavailable ExplorationBasis = "unavailable"
)

// Certainty maps the basis onto domain.MetricCertainty.
func (b ExplorationBasis) Certainty() MetricCertainty {
	switch b {
	case ExplorationObserved:
		return MetricActual
	case ExplorationDerived:
		return MetricInferred
	default:
		return MetricUnknown
	}
}

// ExplorationMetric is one count plus how it was obtained. Value is nil
// exactly when Basis is unavailable.
type ExplorationMetric struct {
	Value  *int64
	Basis  ExplorationBasis
	Method string
	// LowerBound is set when the value is a floor, not a count: activity AO
	// saw but could not attribute (a shell command whose files are not named,
	// a shell write with no path) may add to it. A lower bound is never
	// compared as if it were exact.
	LowerBound bool
}

// ExplorationRatio is one share in [0,1] plus how it was obtained.
type ExplorationRatio struct {
	Value  *float64
	Basis  ExplorationBasis
	Method string
}

// AgentExploration is what one agent -- one usage subject in one role and
// cycle of a run -- looked at and spent.
type AgentExploration struct {
	Role    WorkflowRole
	Cycle   int64
	Subject UsageSubject
	Harness string
	Models  []string

	ModelCalls  ExplorationMetric
	InputTokens ExplorationMetric
	// UncachedInputTokens is input the provider billed as neither a cache
	// read nor a cache write. On Claude nearly all new input is written to the
	// cache, so this is tiny there; M1u is FreshInputTokens.
	UncachedInputTokens ExplorationMetric
	// FreshInputTokens is M1u: input NOT served from the cache (input minus
	// cache reads = uncached + cache writes). It is the input the provider
	// actually had to process anew, on either harness.
	FreshInputTokens  ExplorationMetric
	OutputTokens      ExplorationMetric
	CachedInputTokens ExplorationMetric
	CacheWriteTokens  ExplorationMetric
	FirstCallInput    ExplorationMetric
	// HarnessTokensFirstCall estimates how much of the first call's input
	// the harness contributed (system prompt, tool schemas, injected
	// instructions): first-call input minus the AO prompt at ~4 bytes/token.
	HarnessTokensFirstCall ExplorationMetric

	ToolCalls       ExplorationMetric
	FileReads       ExplorationMetric
	UniqueFilesRead ExplorationMetric
	RepeatedReads   ExplorationMetric
	Searches        ExplorationMetric
	Listings        ExplorationMetric
	Commands        ExplorationMetric
	ExploreCommands ExplorationMetric
	ExplorationOps  ExplorationMetric
	// ExplorationOpsAll is the 3D exploration figure (M3): every exploration
	// operation AO can classify, by ONE method per harness. Claude: structured
	// Read/Grep/Glob/LS calls plus shell commands classified as inspection by
	// their program name (derived). Codex: the provider's own parsed_cmd
	// read/search/list items; its shell calls are not added again, because
	// parsed_cmd already describes them. Commands AO cannot classify are NOT
	// in it -- they are UnattributedCommands, a sensitivity figure.
	ExplorationOpsAll ExplorationMetric
	// UnattributedCommands counts executed commands whose inspected files AO
	// cannot name: every Claude shell call, and every command Codex's own
	// parser marked unknown. File counts are a lower bound whenever it is > 0.
	UnattributedCommands ExplorationMetric
	Edits                ExplorationMetric
	ShellEdits           ExplorationMetric
	UniqueFilesEdited    ExplorationMetric
	OpsBeforeFirstEdit   ExplorationMetric
	CallsBeforeFirstEdit ExplorationMetric

	RepoBytesObserved      ExplorationMetric
	ExplorationResultBytes ExplorationMetric
	AOContextBytes         ExplorationMetric
	HarnessContextBytes    ExplorationMetric
	UnobservedResults      ExplorationMetric

	ExplorationRatio    ExplorationRatio
	AOContextRatio      ExplorationRatio
	HarnessContextRatio ExplorationRatio

	ActiveSpanMS ExplorationMetric

	// PathScopes counts path-naming observations by where their target lay.
	// Only the project scope ever named a path; the others are counts.
	PathScopes map[ToolPathScope]int64
	// TopFiles are the most-read project-relative paths, most read first.
	TopFiles []ExplorationFileCount
	// ApproximateAttribution counts observations placed in a role window by
	// the subject-earliest fallback rather than by their own time.
	ApproximateAttribution int64
	// Sources is how many transcript sources the agent's observations came
	// from. Byte orders of different transcripts are not comparable, so a
	// sequence figure over more than one is unavailable.
	Sources int64
	// ToolCoverage says whether the 3C extractor parsed every transcript of
	// this agent from its first byte. When it did not, every tool figure is
	// unavailable: an unparsed transcript is not zero exploration.
	ToolCoverage ExplorationCoverage
	// TurnMix is how the agent's provider calls split by turn class, and how
	// that was known.
	TurnMix ExplorationTurnMix
}

// ExplorationCoverage is the extractor's coverage of one agent's transcripts.
type ExplorationCoverage struct {
	Complete bool
	// Reason is empty when Complete.
	Reason string
	// ExtractorVersions are the classifier versions that produced the rows.
	ExtractorVersions []int64
}

// ExplorationTurnMix counts provider calls per turn class.
type ExplorationTurnMix struct {
	Basis  ExplorationBasis
	Method string
	Counts map[TurnClass]int64
}

// RunMemoryPack is what one dispatch of a run was handed by project memory: a
// summary of its context manifest. It is how 3D proves which memory a
// treatment run actually received -- identities and sizes only, no facts.
type RunMemoryPack struct {
	Role            string
	TaskRef         string
	PackDigest      string
	PolicyVersion   int
	Generation      int64
	IndexedCommit   string
	ItemCount       int
	SelectedBytes   int
	EstimatedTokens int
	CreatedAt       time.Time
}

// ExplorationFileCount is one project-relative path and how often it was read.
type ExplorationFileCount struct {
	Path  string
	Reads int64
}

// RunQualitySignals are the outcome facts 3D needs to show that less
// exploration did not buy a worse result. All are read from durable run
// state AO already keeps; none is new telemetry.
type RunQualitySignals struct {
	FinalState         WorkflowRunState
	Completed          bool
	DurationMS         ExplorationMetric
	Attempts           ExplorationMetric
	FailedAttempts     ExplorationMetric
	Retries            ExplorationMetric
	ProviderFailovers  ExplorationMetric
	VerifyRuns         ExplorationMetric
	VerifyPassed       *bool
	ChecksPassed       ExplorationMetric
	ChecksFailed       ExplorationMetric
	ReviewRuns         ExplorationMetric
	FinalReviewVerdict ReviewVerdict
	FixCycles          ExplorationMetric
}

// RunExploration is the 3C read model of one workflow run.
type RunExploration struct {
	RunID     string
	ProjectID string
	// Recorded is false when the run has no tool observation and no usage
	// event at all: nothing was recorded, which is not zero exploration.
	Recorded bool
	Agents   []AgentExploration
	Totals   AgentExploration
	Quality  RunQualitySignals
	// ContextSources is the context-decorator state frozen into the run's
	// policy_snapshot at creation (zero value: not recorded -- a run created
	// before 3C, or by a daemon that did not stamp it).
	ContextSources ContextSourcesSnapshot
	// MemoryPacks are the run's project-memory context manifests, oldest
	// first. Empty with memory off; with memory on, an empty list means no
	// dispatch was handed a pack.
	MemoryPacks []RunMemoryPack
}
