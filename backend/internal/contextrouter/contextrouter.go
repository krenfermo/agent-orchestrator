// Package contextrouter assembles the context AO hands to an agent, instead of
// handing it everything AO happens to have.
//
// Today every dispatch surface sends its whole assembled payload: the planner
// gets every context document, a worker gets the whole issue context. That is
// simple and it is also why the baseline evidence in
// internal/observe/projectmemory keeps recording payloads far larger than the
// part of them an agent actually reads.
//
// The router is the other half of that story. It answers one question —
// "given this role, this task, and this project, what is worth sending?" — and
// answers it under a budget:
//
//   - Role-aware. A planner is reasoning about a whole repository and gets the
//     largest budget with documents ranked first; a verify dispatch runs a
//     command and needs little more than the task and what changed. The
//     ordering of sections, not just their size, differs per role.
//   - Evidence-backed. Sections are built from facts AO already owns: the
//     current git diff, the impacted files and symbols from
//     internal/codegraph, and the durable facts in internal/projectmemory.
//     This package retrieves and ranks them; it does not re-derive them.
//   - Progressively expanded. Select does a compact retrieval first. Only when
//     that compact payload is not self-evidently sufficient does a caller
//     Expand it, and the expansion is bounded by the role's hard cap, which is
//     never exceeded — a section that does not fit is truncated or dropped and
//     said so, never silently allowed through.
//
// Nothing here is on by default. See Enabled and the wfrouter subpackage: with
// the flag off, dispatch keeps receiving exactly the payload it received
// before this package existed.
package contextrouter

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// bytesPerToken is the divisor behind every size figure this package reports.
// It deliberately matches the baseline harness's own heuristic
// (baseline.EstimateTokensFromBytes) so a routed payload and a measured
// baseline payload are counted the same way and can be compared directly;
// tokenEstimateMatchesBaseline in the tests holds the two together.
const bytesPerToken = 4

// minSectionTokens is the smallest slice of a section worth keeping. Below it
// a truncated section carries a header and a few words of its body, which
// costs budget and teaches an agent nothing, so the section is dropped and
// reported instead.
const minSectionTokens = 32

// truncationMarker is appended to every section the packer had to cut. It is
// counted against the budget like any other text, and it exists so the agent
// reading the payload knows it is reading a fragment.
const truncationMarker = "\n… [truncated by the AO context router to fit the role budget]"

// Role is the dispatch role a payload is being assembled for. It is the
// router's own vocabulary rather than domain.WorkflowRole because the router
// budgets fix delivery and worker spawn identically in kind but not in size,
// and because a caller outside the workflow (a CLI dry run, a test) should not
// have to reach for workflow vocabulary to ask for a planner payload.
type Role string

// The roles the router budgets for.
const (
	// RolePlanner assembles a plan for a whole objective and reads broadly.
	RolePlanner Role = "planner"
	// RoleWorker implements one task in one checkout.
	RoleWorker Role = "worker"
	// RoleReviewer judges a change and needs the change plus its surroundings.
	RoleReviewer Role = "reviewer"
	// RoleFix applies a specific correction into an existing session that
	// already holds the task's history, so it needs the correction's evidence
	// and little else.
	RoleFix Role = "fix"
	// RoleVerify runs a deterministic command. It needs the task and what
	// changed; it never reasons over the repository.
	RoleVerify Role = "verify"
)

// Roles returns every budgeted role, in a stable order.
func Roles() []Role {
	return []Role{RolePlanner, RoleWorker, RoleReviewer, RoleFix, RoleVerify}
}

// Valid reports whether r is a role the router budgets for.
func (r Role) Valid() bool {
	switch r {
	case RolePlanner, RoleWorker, RoleReviewer, RoleFix, RoleVerify:
		return true
	default:
		return false
	}
}

// RoleFromWorkflowRole maps AO's durable workflow vocabulary onto the router's
// and reports whether the role is one the router assembles context for.
// Decision resolution is deliberately absent: it answers a question about a
// run, not about a checkout, so there is no diff, graph, or memory evidence to
// route for it.
func RoleFromWorkflowRole(role domain.WorkflowRole) (Role, bool) {
	switch role {
	case domain.WorkflowRolePlanner:
		return RolePlanner, true
	case domain.WorkflowRoleWorker:
		return RoleWorker, true
	case domain.WorkflowRoleReviewer:
		return RoleReviewer, true
	case domain.WorkflowRoleFixWorker:
		return RoleFix, true
	case domain.WorkflowRoleVerify:
		return RoleVerify, true
	default:
		return "", false
	}
}

// Tier is how much retrieval a selection did. It is the mechanism behind
// progressive expansion: a caller starts compact and pays for more only when
// the compact payload could not carry the evidence.
type Tier string

// The retrieval tiers.
const (
	// TierCompact is the first pass: what changed, which symbols it touches,
	// the few memory facts that bear on it, and the head of each document.
	TierCompact Tier = "compact"
	// TierExpanded is the second pass: more files, symbol-level detail, graph
	// edges, more memory, and whole documents — still under the hard cap.
	TierExpanded Tier = "expanded"
)

// SectionKind classifies one block of an assembled payload.
type SectionKind string

// The section kinds the router emits.
const (
	// SectionTask states what is being asked. It is mandatory: a payload
	// without it is not a smaller payload, it is a useless one.
	SectionTask SectionKind = "task"
	// SectionDocument is one context document the caller already assembled
	// (an AGENTS.md, a task brief, a tracker issue body).
	SectionDocument SectionKind = "document"
	// SectionDiff is the current change set: which files moved and how.
	SectionDiff SectionKind = "diff"
	// SectionGraph is the impacted files and symbols from the code graph.
	SectionGraph SectionKind = "graph"
	// SectionMemory is durable project memory that bears on this task.
	SectionMemory SectionKind = "memory"
)

// Document is a context document the caller has already assembled and wants
// the router to budget alongside the evidence it retrieves itself.
type Document struct {
	// Path is what the document is, for the agent and for provenance.
	Path string
	// Content is the document text.
	Content string
}

// Task is what the dispatch is being asked to do, plus whatever the caller
// already knows about where the answer lives.
type Task struct {
	// ID is the task or step id, for provenance. Optional.
	ID string
	// Title is a one-line description. Optional.
	Title string
	// Objective is the full statement of the work. It anchors the mandatory
	// task section.
	Objective string
	// Paths are files the caller already believes are involved. They rank
	// memory and seed graph queries, on top of whatever the diff shows.
	Paths []string
	// Symbols are symbols the caller already believes are involved. They seed
	// graph queries the diff alone would not reach.
	Symbols []string
}

// Project identifies the checkout a payload is assembled against.
type Project struct {
	// ID is the AO project id. Project memory is keyed by it.
	ID string
	// Root is the absolute path of the checkout. The diff source and the code
	// graph are keyed by it.
	Root string
	// BaseRef is what the working tree is diffed against. Empty means HEAD,
	// i.e. "everything not yet committed".
	BaseRef string
}

// Request is one Select or Expand call: a role, a task, and a project, plus
// the documents the caller had already assembled.
type Request struct {
	// Role selects the budget and the section ordering.
	Role Role
	// Task is what is being asked.
	Task Task
	// Project is the checkout to assemble against.
	Project Project
	// Documents are context documents the caller already holds. They are
	// budgeted like any other section rather than being passed through, which
	// is the whole point of routing: today's dispatch sends all of them.
	Documents []Document
	// ForceExpand makes Expand assemble the expanded tier even when the
	// compact selection already carried sufficient evidence. The hard cap
	// still applies — forcing expansion buys retrieval depth, never budget.
	ForceExpand bool
}

// Section is one ordered block of an assembled payload.
type Section struct {
	// Kind classifies the block.
	Kind SectionKind `json:"kind"`
	// Title is the block's heading in the rendered payload.
	Title string `json:"title"`
	// Source names where the content came from, for provenance in logs and
	// in the rendered payload's header.
	Source string `json:"source,omitempty"`
	// Content is the block's text.
	Content string `json:"content"`
	// Priority is the packing order for this role; lower packs first.
	Priority int `json:"priority"`
	// Tier is the retrieval tier that produced the block.
	Tier Tier `json:"tier"`
	// Truncated reports that the packer had to cut the block to fit.
	Truncated bool `json:"truncated,omitempty"`
	// EstimatedTokens and Bytes size the block as packed.
	EstimatedTokens int `json:"estimatedTokens"`
	Bytes           int `json:"bytes"`
}

// Dropped is one candidate section that did not make it into the payload. It
// is reported rather than discarded: "the budget could not hold the graph
// evidence" is the signal that decides whether to expand.
type Dropped struct {
	Kind            SectionKind `json:"kind"`
	Title           string      `json:"title"`
	Reason          string      `json:"reason"`
	EstimatedTokens int         `json:"estimatedTokens"`
}

// Selection is what the router decided to send.
type Selection struct {
	// Role and Project echo the request the selection answers.
	Role    Role   `json:"role"`
	Project string `json:"project"`
	// Tier is the retrieval depth that produced it.
	Tier Tier `json:"tier"`
	// Sections are the payload blocks in send order.
	Sections []Section `json:"sections"`
	// Dropped are the candidates that did not fit.
	Dropped []Dropped `json:"dropped,omitempty"`
	// EstimatedTokens and EstimatedBytes size the whole payload. The token
	// figure is what the budget is measured against.
	EstimatedTokens int `json:"estimatedTokens"`
	EstimatedBytes  int `json:"estimatedBytes"`
	// Budget is the role budget this selection was packed against.
	Budget Budget `json:"budget"`
	// Limit is the token target actually applied for this tier — the compact
	// target for a compact selection, the expanded target for an expanded one,
	// each already clamped to the hard cap.
	Limit int `json:"limit"`
	// EvidenceSufficient reports that the payload carries the evidence its
	// sources could offer: nothing was dropped, nothing was truncated, and no
	// source failed. It is the condition Expand tests before spending more
	// budget.
	EvidenceSufficient bool `json:"evidenceSufficient"`
	// Expandable reports that a bounded Expand call could add something. It is
	// false for an already-expanded selection and for a compact one that is
	// already sufficient.
	Expandable bool `json:"expandable"`
	// Notes explain what the assembly could not do — a source that failed, an
	// expansion that was skipped. They are operator-facing and are not part of
	// the payload sent to an agent.
	Notes []string `json:"notes,omitempty"`
}

// WithinBudget reports that the selection respects its role's hard cap. It is
// the invariant this package exists to keep, and it is asserted at the end of
// every assembly rather than only in tests.
func (s Selection) WithinBudget() bool {
	return s.EstimatedTokens <= s.Budget.HardCapTokens
}

// Render turns the selection into the payload text a dispatch surface sends.
// Sections are emitted in order under their titles, with the truncation marker
// already inside any section that carries one.
func (s Selection) Render() string {
	var b strings.Builder
	for i, section := range s.Sections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## ")
		b.WriteString(section.Title)
		if section.Source != "" {
			b.WriteString(" (")
			b.WriteString(section.Source)
			b.WriteString(")")
		}
		b.WriteString("\n")
		b.WriteString(section.Content)
	}
	return b.String()
}

// estimateTokens sizes a string the way the baseline harness sizes a payload.
func estimateTokens(s string) int {
	return int(baseline.EstimateTokensFromBytes(int64(len(s))))
}

// truncateToTokens cuts s so its estimate is at most tokens, appending the
// truncation marker when there is room for it, and reports whether it cut.
// The marker's own cost is subtracted first, so the returned string is within
// budget including the marker — a truncation that pushed a section back over
// the limit would defeat the cap it was performed to respect.
func truncateToTokens(s string, tokens int) (string, bool) {
	if tokens <= 0 {
		return "", true
	}
	if estimateTokens(s) <= tokens {
		return s, false
	}
	limit := tokens * bytesPerToken
	if limit <= len(truncationMarker) {
		return cutBytes(s, limit), true
	}
	return cutBytes(s, limit-len(truncationMarker)) + truncationMarker, true
}

// cutBytes truncates s to at most n bytes without splitting a rune.
func cutBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// sortSections orders candidates by packing priority, then by kind and title,
// so an assembly is deterministic for a given set of inputs.
func sortSections(sections []Section) {
	sort.SliceStable(sections, func(i, j int) bool {
		a, b := sections[i], sections[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Title < b.Title
	})
}
