package projectmemory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// graphmemory.go — the code graph, as project memory sees it.
//
// Project memory already knew what a repository IS at the level of modules,
// documents, dependencies and conventions. What it could not say was anything
// below that: which function decides a permission, which route reaches it,
// which table it writes, which test covers it. Those facts existed in
// internal/codegraph and were never part of what a dispatch was told.
//
// This file is the join, and it is deliberately thin. Three rules shape it:
//
//   - The graph is a SECOND source category, never a rewrite of the first.
//     Project memory stays authoritative for what it already covers; graph
//     evidence is added beside it, rendered in its own section, and measured
//     separately (section 15 of the brief). Nothing that used to come from
//     project memory now comes from the graph instead.
//   - The graph is OPTIONAL at every point. A Service constructed without one
//     behaves exactly as it did before; a graph that fails, is unbuilt, or
//     times out degrades to the memory AO already had, with the reason stated.
//     That is the same contract Provision has always had, extended one layer.
//   - The graph is bounded independently. It has its own byte budget per role,
//     so adding structural evidence can never quietly eat the budget that was
//     paying for durable facts.

// CodeGraph is the structural half of memory, as this package needs it.
//
// It is declared here, at the consumer, and is satisfied by
// *codegraph.Index. Keeping it an interface is not ceremony: it is what lets
// every test in this package exercise the degraded paths -- graph absent, graph
// failing, graph empty -- which are the paths that decide whether the dispatch
// survives a bad day.
type CodeGraph interface {
	// Build runs a full, staged pass.
	Build(ctx context.Context, req codegraph.SyncRequest) (codegraph.SyncOutcome, error)
	// Apply applies a diff to the served graph, in place.
	Apply(ctx context.Context, req codegraph.SyncRequest, diff codegraph.Diff) (codegraph.SyncOutcome, error)
	// Retrieve answers a task's question with bounded graph evidence.
	Retrieve(ctx context.Context, projectID domain.ProjectID, repoID string, req codegraph.RetrieveRequest) (codegraph.Neighborhood, error)
	// Architecture returns the stored bounded structural summary.
	Architecture(ctx context.Context, projectID domain.ProjectID, repoID string) (string, codegraph.Architecture, bool, error)
	// Status reads one repository's durable graph state.
	Status(ctx context.Context, projectID domain.ProjectID, repoID string) (store.CodeGraphState, bool, error)
}

// GraphBudget bounds one role's graph evidence.
type GraphBudget struct {
	// MaxBytes caps the rendered graph section. Zero disables graph evidence
	// for the role entirely, which is a legitimate configuration and not an
	// error.
	MaxBytes int
	// MaxSymbols and MaxEdges bound the retrieval itself, before rendering.
	MaxSymbols int
	MaxEdges   int
	// Architecture asks for the project-level structural summary. It is on for
	// the Planner, whose whole job is splitting work along module boundaries,
	// and off for roles that already know which file they are looking at.
	Architecture bool
}

// Enabled reports whether this role receives graph evidence at all.
func (b GraphBudget) Enabled() bool { return b.MaxBytes > 0 }

// GraphBudgetSet is the per-role table.
type GraphBudgetSet map[PackRole]GraphBudget

// DefaultGraphBudgets are the bounds graph evidence is assembled with.
//
// They are small, and they differ by role for a reason each can be defended
// on:
//
//   - The Planner gets the architecture summary and few symbols. It is
//     deciding how to SPLIT work, so the module map is worth more to it than
//     any individual function.
//   - The Worker and the Repair Agent get symbols and no architecture. They
//     already know which area they are in; what they need is the neighbourhood
//     of the thing they are about to change.
//   - The Reviewer gets the most symbols. "What calls this, what tests cover
//     it, which boundary does it belong to" is exactly the question an
//     independent review asks, and it is the question that otherwise costs a
//     reviewer a repository-wide read (section 26).
func DefaultGraphBudgets() GraphBudgetSet {
	return GraphBudgetSet{
		RolePlanner:  {MaxBytes: 6 * 1024, MaxSymbols: 16, MaxEdges: 32, Architecture: true},
		RoleWorker:   {MaxBytes: 6 * 1024, MaxSymbols: 24, MaxEdges: 48},
		RoleReviewer: {MaxBytes: 8 * 1024, MaxSymbols: 32, MaxEdges: 64},
		RoleRepair:   {MaxBytes: 4 * 1024, MaxSymbols: 16, MaxEdges: 32},
	}
}

// For returns a role's graph budget, falling back to the worker's for a role
// the table does not name -- the same modest-guess rule BudgetSet.For uses.
func (s GraphBudgetSet) For(role PackRole) GraphBudget {
	if b, ok := s[role]; ok {
		return b
	}
	if b, ok := s[RoleWorker]; ok {
		return b
	}
	return DefaultGraphBudgets()[RoleWorker]
}

// GraphEvidence is the `code_graph` source category: what the structural graph
// contributed to one pack, and what it cost.
//
// It is kept OUT of PackSection deliberately. A pack section holds durable
// project-memory items, each with its own provenance, confidence and
// authority; graph evidence is a different kind of fact with a different
// lifecycle, and mixing the two would make "how much of this context came from
// the graph" unanswerable -- which is precisely what P3-E has to be able to
// answer (section 15).
type GraphEvidence struct {
	// Backend names the implementation that produced the graph, so operator
	// output can never report LocalGraph under another name.
	Backend string
	// Generation and IndexedCommit are the graph's provenance.
	Generation    int64
	IndexedCommit string
	// Architecture is the bounded project structural summary, when the role's
	// budget asked for it.
	Architecture string
	// Symbols, Callers, Tests, Endpoints and Tables are the retrieved
	// neighbourhood, already bounded.
	Neighborhood codegraph.Neighborhood
	// ConsideredSymbols and ConsideredEdges are everything retrieval was
	// allowed to choose from; SelectedSymbols and SelectedEdges are what this
	// pack carries. The pairs are what make the graph's contribution
	// measurable rather than asserted.
	ConsideredSymbols int
	ConsideredEdges   int
	SelectedSymbols   int
	SelectedEdges     int
	// Bytes and EstimatedTokens are what the rendered section weighs.
	Bytes           int
	EstimatedTokens int
	// Truncated reports that the byte budget clipped the evidence.
	Truncated bool
	// Reason explains an empty contribution: no graph yet, the backend was
	// unreachable, the role's budget is zero, nothing matched.
	Reason string
}

// Empty reports whether the graph contributed nothing.
func (g GraphEvidence) Empty() bool { return g.Bytes == 0 }

// Render writes the graph's contribution as the text a pack carries.
//
// It is a separate, labelled section so a reader -- human or model -- can tell
// a structural fact derived from parsing the code from a durable summary
// derived from a document. They have different failure modes and deserve
// different trust: the graph is exact about where a symbol is and says nothing
// about why it exists.
func (g GraphEvidence) Render() string {
	var b strings.Builder
	if g.Architecture != "" {
		b.WriteString("### Project structure (code graph)\n\n")
		b.WriteString(g.Architecture)
		b.WriteString("\n")
	}
	if rendered := g.Neighborhood.Render(); rendered != "" {
		b.WriteString("### Code relevant to this work (code graph)\n\n")
		b.WriteString(rendered)
		b.WriteString("\n")
	}
	return b.String()
}

// GraphFreshness is what a graph sync did, reported beside the memory sync's
// own Freshness.
type GraphFreshness struct {
	// Attempted reports that a graph is configured and a sync was tried. A
	// false value means no graph is wired, which is the pre-phase behaviour
	// and not a degradation.
	Attempted bool
	// Kind is what the sync had to do: full, incremental, or noop.
	Kind store.CodeGraphSyncKind
	// Generation and IndexedCommit are the graph's provenance afterwards.
	Generation    int64
	IndexedCommit string
	// Files, Symbols and Edges size the graph after the sync.
	Files   int
	Symbols int
	Edges   int
	// FilesParsed against FilesReused is the measurement that shows an
	// incremental sync costing a file's work rather than a repository's.
	FilesParsed  int
	FilesReused  int
	FilesRemoved int
	Duration     time.Duration
	// Usable reports that a complete graph is being served.
	Usable bool
	// Reason explains a skip or a degradation.
	Reason string
}

// syncGraph brings the code graph up to the same commit the memory sync just
// reached.
//
// It runs AFTER the memory sync and never in place of it, and it can only
// degrade: every failure path here returns a GraphFreshness with a reason and
// leaves the dispatch exactly as well off as it was before the graph existed.
// That is the contract the whole subsystem inherits from Provision -- there is
// no failure of memory that should stop a dispatch -- and a structural index is
// the last thing that should be allowed to break it.
func (s *Syncer) syncGraph(
	ctx context.Context, projectID domain.ProjectID, repoPath, repoID, commit, branch string,
) GraphFreshness {
	if s.svc == nil || s.svc.codeGraph == nil {
		return GraphFreshness{}
	}
	fresh := GraphFreshness{Attempted: true}

	state, found, err := s.svc.codeGraph.Status(ctx, projectID, repoID)
	if err != nil {
		return s.graphDegraded(fresh, "reading the code graph's state failed: "+err.Error())
	}

	// The warm path, and the one that has to be free: the graph already
	// describes this commit, so there is nothing to diff and nothing to read.
	if found && state.Indexed() && commit != "" && state.IndexedCommit == commit && state.Phase == store.CodeGraphIdle {
		return GraphFreshness{
			Attempted: true, Kind: store.CodeGraphSyncNoop, Usable: true,
			Generation: state.ServedGeneration, IndexedCommit: state.IndexedCommit,
			Files: int(state.FileCount), Symbols: int(state.SymbolCount), Edges: int(state.EdgeCount),
			Reason: "the code graph is already at this commit",
		}
	}

	req := codegraph.SyncRequest{
		ProjectID: projectID, RepoID: repoID, RepoPath: repoPath,
		Commit: commit, Branch: branch,
		RepoIdentity: RepoIdentityOf(ctx, repoPath).String(),
	}

	outcome, err := s.graphPass(ctx, state, found, req, commit)
	if err != nil {
		return s.graphDegraded(fresh, "the code graph sync failed: "+err.Error())
	}

	fresh.Kind = outcome.Kind
	fresh.Generation = outcome.Generation
	fresh.IndexedCommit = commit
	fresh.Files, fresh.Symbols, fresh.Edges = outcome.Files, outcome.Symbols, outcome.Edges
	fresh.FilesParsed, fresh.FilesReused, fresh.FilesRemoved = outcome.FilesParsed, outcome.FilesReused, outcome.FilesRemoved
	fresh.Duration = outcome.Duration
	fresh.Reason = outcome.Reason
	if after, ok, err := s.svc.codeGraph.Status(ctx, projectID, repoID); err == nil && ok {
		fresh.Usable = after.Indexed()
		fresh.Generation = after.ServedGeneration
		fresh.IndexedCommit = after.IndexedCommit
	}
	return fresh
}

// graphPass chooses between an incremental update and a full build.
//
// Incremental whenever AO can prove a change set: a graph that is already
// indexed, at a commit git can diff from, to the commit in front of it.
// Anything else -- a first index, an unreachable previous commit after a
// force-push, a checkout with no commit at all -- is a full build, because an
// incremental update on an unprovable change set would leave holes AO cannot
// detect, and an undetectable hole is worse than a scan. That is the same rule
// Service.Sync applies to project memory itself.
func (s *Syncer) graphPass(
	ctx context.Context, state store.CodeGraphState, found bool, req codegraph.SyncRequest, commit string,
) (codegraph.SyncOutcome, error) {
	if !found || !state.Indexed() || state.IndexedCommit == "" || commit == "" || state.IndexedCommit == commit {
		return s.svc.codeGraph.Build(ctx, req)
	}
	changes, err := ChangesSinceCommit(ctx, req.RepoPath, state.IndexedCommit, commit)
	if err != nil {
		if s.log != nil {
			s.log.Debug("code graph: no provable change set, falling back to a full build",
				"repo", req.RepoPath, "from", state.IndexedCommit, "to", commit, "err", err)
		}
		return s.svc.codeGraph.Build(ctx, req)
	}
	return s.svc.codeGraph.Apply(ctx, req, GraphDiff(changes))
}

// graphDegraded records a graph failure as a degradation and never as an error.
func (s *Syncer) graphDegraded(fresh GraphFreshness, reason string) GraphFreshness {
	if s.log != nil {
		s.log.Warn("code graph unavailable for this dispatch; falling back to project memory alone", "reason", reason)
	}
	fresh.Reason = reason
	return fresh
}

// GraphDiff converts project memory's change set into the graph's.
//
// The two vocabularies are both git's, which is why this is a mapping rather
// than a translation: one change set is computed, once, and both halves of
// memory are updated from it. It is exported because the operator-facing
// service performs the same sync by hand, and two mappings of one vocabulary
// is how the hand path and the dispatch path drift apart.
func GraphDiff(changes []PathChange) codegraph.Diff {
	diff := codegraph.Diff{Changes: make([]codegraph.FileChange, 0, len(changes))}
	for _, change := range changes {
		var status codegraph.ChangeStatus
		switch change.Kind {
		case ChangeAdded:
			status = codegraph.ChangeAdded
		case ChangeModified:
			status = codegraph.ChangeModified
		case ChangeDeleted:
			status = codegraph.ChangeDeleted
		case ChangeRenamed:
			status = codegraph.ChangeRenamed
		default:
			continue
		}
		diff.Changes = append(diff.Changes, codegraph.FileChange{
			Status: status, Path: change.Path, OldPath: change.PreviousPath,
		})
	}
	return diff
}

// graphEvidence assembles the code_graph contribution for one pack.
//
// Everything about it fails soft. No graph configured, no completed build, a
// zero budget, a failing backend, nothing matched: each returns evidence with a
// stated reason and no content, and the pack is exactly what it would have been
// without this phase.
func (s *Service) graphEvidence(ctx context.Context, req PackRequest, repoID string, budget GraphBudget) GraphEvidence {
	if s.codeGraph == nil {
		return GraphEvidence{Reason: "no code graph is configured"}
	}
	if !budget.Enabled() {
		return GraphEvidence{Reason: fmt.Sprintf("the %s role's code-graph budget is zero", req.Role)}
	}
	if strings.TrimSpace(repoID) == "" {
		return GraphEvidence{Reason: "a code-graph lookup needs one repository, and this pack spans the project"}
	}

	state, found, err := s.codeGraph.Status(ctx, req.ProjectID, repoID)
	if err != nil {
		s.warn("code graph: reading state failed", err)
		return GraphEvidence{Reason: "the code graph could not be read: " + err.Error()}
	}
	if !found || !state.Indexed() {
		return GraphEvidence{Reason: "this repository's code graph has not been built yet"}
	}

	evidence := GraphEvidence{
		Backend: state.Backend, Generation: state.ServedGeneration, IndexedCommit: state.IndexedCommit,
	}
	if budget.Architecture {
		rendered, _, ok, archErr := s.codeGraph.Architecture(ctx, req.ProjectID, repoID)
		if archErr != nil {
			s.warn("code graph: reading the architecture summary failed", archErr)
		} else if ok {
			evidence.Architecture = rendered
		}
	}

	neighborhood, err := s.codeGraph.Retrieve(ctx, req.ProjectID, repoID, codegraph.RetrieveRequest{
		Files:      graphAnchorPaths(req),
		Terms:      req.Keywords,
		MaxSymbols: budget.MaxSymbols,
		MaxEdges:   budget.MaxEdges,
	})
	if err != nil {
		s.warn("code graph: retrieval failed", err)
		if evidence.Architecture == "" {
			return GraphEvidence{Reason: "the code graph could not be queried: " + err.Error()}
		}
	} else {
		evidence.Neighborhood = neighborhood
		evidence.ConsideredSymbols = neighborhood.ConsideredSymbols
		evidence.ConsideredEdges = neighborhood.ConsideredEdges
		evidence.SelectedSymbols = neighborhood.SelectedSymbols()
		evidence.SelectedEdges = neighborhood.SelectedEdges()
		evidence.Truncated = neighborhood.Truncated
	}

	rendered := evidence.Render()
	if len(rendered) > budget.MaxBytes {
		// Clipping drops the neighbourhood before the architecture: the map is
		// what a role cannot get anywhere else cheaply, and an over-budget
		// neighbourhood is a list whose tail was the least relevant part
		// anyway.
		evidence.Truncated = true
		evidence.Neighborhood = codegraph.Neighborhood{}
		evidence.SelectedSymbols, evidence.SelectedEdges = 0, 0
		rendered = evidence.Render()
		if len(rendered) > budget.MaxBytes {
			return GraphEvidence{
				Backend: evidence.Backend, Generation: evidence.Generation,
				IndexedCommit: evidence.IndexedCommit, Truncated: true,
				Reason: fmt.Sprintf("the code graph's evidence did not fit the %s role's %d-byte budget", req.Role, budget.MaxBytes),
			}
		}
	}
	evidence.Bytes = len(rendered)
	evidence.EstimatedTokens = EstimateTokens(evidence.Bytes)
	if evidence.Bytes == 0 && evidence.Reason == "" {
		evidence.Reason = "the code graph held nothing relevant to this work"
	}
	return evidence
}

// graphAnchorPaths are the paths a retrieval anchors on: what the work is
// known to touch.
//
// A task's own rewritten paths are INCLUDED here, unlike in item selection
// where they are an exclusion. The reason the two differ is what each fact
// says: a canonical summary of a file the task has rewritten describes a
// version the reader is not looking at, whereas the graph's answer to "what
// else touches this file" is about the rest of the repository, which the task
// has not rewritten.
func graphAnchorPaths(req PackRequest) []string {
	paths := append([]string(nil), req.ChangedPaths...)
	paths = append(paths, req.TaskChangedPaths...)
	return domain.NormalizeMemorySourcePaths(paths)
}

// warn reports a swallowed degradation, when a logger was supplied.
func (s *Service) warn(msg string, err error) {
	if s.log != nil {
		s.log.Warn(msg, "err", err)
	}
}
