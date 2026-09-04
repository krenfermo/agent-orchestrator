package projectmemory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// graph.go — the code graph's read/repair service.
//
// Same job as the file beside it: turn a project id into the repository the
// graph is keyed by, and keep the two vocabularies apart. Same policy too --
// a request that names no repository operates on the project's own root.
//
// The one thing this file adds that the memory service does not have is DRIFT.
// A graph carries the commit and the repository identity it was derived under,
// and both can stop being true without anything writing to the graph: a
// checkout moves on, a repository is re-cloned from a different remote. Status
// therefore compares what the graph claims against what the checkout says NOW,
// and reports a mismatch rather than serving facts it cannot vouch for.

// GraphIndex is the slice of the code graph this service needs. Declaring it
// here rather than depending on *codegraph.Index keeps the service testable
// against the degraded states -- absent, unbuilt, failing -- which are the ones
// an operator surface most needs to render correctly.
type GraphIndex interface {
	Build(ctx context.Context, req codegraph.SyncRequest) (codegraph.SyncOutcome, error)
	Apply(ctx context.Context, req codegraph.SyncRequest, diff codegraph.Diff) (codegraph.SyncOutcome, error)
	Retrieve(ctx context.Context, projectID domain.ProjectID, repoID string, req codegraph.RetrieveRequest) (codegraph.Neighborhood, error)
	Status(ctx context.Context, projectID domain.ProjectID, repoID string) (store.CodeGraphState, bool, error)
	StatusAll(ctx context.Context, projectID domain.ProjectID) ([]store.CodeGraphState, error)
}

// WithGraph attaches the code graph. Without it the graph routes report
// not-implemented, which is the honest answer for a build that has none.
func (s *Service) WithGraph(g GraphIndex) *Service {
	s.graph = g
	return s
}

// GraphStatus reports every registered repository's code-graph state.
func (s *Service) GraphStatus(ctx context.Context, projectID domain.ProjectID) ([]controllers.ProjectMemoryGraphStatus, error) {
	if s.graph == nil {
		return nil, fmt.Errorf("no code graph is configured for this daemon")
	}
	states, err := s.graph.StatusAll(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]controllers.ProjectMemoryGraphStatus, 0, len(states))
	for _, state := range states {
		out = append(out, s.graphStatus(ctx, state))
	}
	return out, nil
}

// graphStatus renders one state, with the drift check applied.
func (s *Service) graphStatus(ctx context.Context, state store.CodeGraphState) controllers.ProjectMemoryGraphStatus {
	status := controllers.ProjectMemoryGraphStatus{
		RepoID: state.RepoID, RepoPath: state.RepoPath, Backend: state.Backend,
		Generation: state.ServedGeneration, Phase: string(state.Phase),
		IndexedCommit: state.IndexedCommit, RepoIdentity: state.RepoIdentity,
		Files: state.FileCount, Symbols: state.SymbolCount, Edges: state.EdgeCount,
		LastSyncKind: string(state.LastSyncKind),
		FilesParsed:  state.LastFilesParsed, FilesReused: state.LastFilesReused,
		FilesRemoved: state.LastFilesRemoved,
		LastMillis:   state.LastDuration.Milliseconds(),
		LastError:    state.LastError,
		Architecture: state.Architecture,
		Healthy:      state.Indexed() && state.Phase == store.CodeGraphIdle,
	}
	if !state.UpdatedAt.IsZero() {
		status.UpdatedAt = state.UpdatedAt.UTC().Format(time.RFC3339)
	}
	status.Drift = graphDrift(ctx, state)
	if status.Drift != "" {
		// Fail closed: a graph whose provenance AO cannot confirm is not
		// healthy, whatever its row says. The facts may well still be right;
		// what is missing is the proof, and serving unprovable structure as
		// current is exactly the failure the memory subsystem refuses to make.
		status.Healthy = false
	}
	return status
}

// graphDrift compares what a graph claims against what the checkout says now.
//
// Three questions, in the order an operator would ask them: is the checkout
// still there, is it still the same repository, and is the graph still at its
// commit. Each is answered from evidence or not at all -- a checkout with no
// commit yields no drift finding, because there is nothing to disagree with.
func graphDrift(ctx context.Context, state store.CodeGraphState) string {
	if !state.Indexed() {
		return ""
	}
	if state.Phase == store.CodeGraphBuilding {
		// Not drift: a build in flight is a known, temporary state, and the
		// previous complete generation is still what readers are being served.
		return ""
	}
	observed := pm.RepoIdentityOf(ctx, state.RepoPath)
	if identity := observed.String(); identity != "" && state.RepoIdentity != "" && identity != state.RepoIdentity {
		return fmt.Sprintf("the checkout at %s now identifies as %s, and this graph was derived under %s",
			state.RepoPath, identity, state.RepoIdentity)
	}
	commit, _ := pm.HeadOf(ctx, state.RepoPath)
	if commit != "" && state.IndexedCommit != "" && commit != state.IndexedCommit {
		return fmt.Sprintf("the checkout is at %s and the graph was indexed at %s; run a sync",
			shortSHA(commit), shortSHA(state.IndexedCommit))
	}
	return ""
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// GraphSync brings one repository's graph up to date.
//
// It chooses between incremental and full exactly as a dispatch would, and for
// the same reason: an operator running a sync by hand should be exercising the
// production path, not a second one that could diverge from it. `full` forces a
// rebuild, which is the repair for a graph an operator has reason to distrust.
func (s *Service) GraphSync(
	ctx context.Context, projectID domain.ProjectID, repoPath string, full bool,
) (controllers.ProjectMemoryGraphSyncResult, error) {
	if s.graph == nil {
		return controllers.ProjectMemoryGraphSyncResult{}, fmt.Errorf("no code graph is configured for this daemon")
	}
	resolved, err := s.resolveRepo(ctx, projectID, repoPath)
	if err != nil {
		return controllers.ProjectMemoryGraphSyncResult{}, err
	}
	repoID := domain.ProjectMemoryRepoID(resolved)
	commit, branch := pm.HeadOf(ctx, resolved)
	req := codegraph.SyncRequest{
		ProjectID: projectID, RepoID: repoID, RepoPath: resolved,
		Commit: commit, Branch: branch,
		RepoIdentity: pm.RepoIdentityOf(ctx, resolved).String(),
	}

	outcome, err := s.graphPass(ctx, req, full, commit)
	if err != nil {
		return controllers.ProjectMemoryGraphSyncResult{}, err
	}
	return controllers.ProjectMemoryGraphSyncResult{
		RepoID: repoID, RepoPath: resolved, Kind: string(outcome.Kind),
		Generation: outcome.Generation, IndexedCommit: commit,
		FilesScanned: outcome.FilesScanned, FilesParsed: outcome.FilesParsed,
		FilesReused: outcome.FilesReused, FilesRemoved: outcome.FilesRemoved,
		SymbolsAdded: outcome.SymbolsAdded, SymbolsRemoved: outcome.SymbolsRemoved,
		EdgesAdded: outcome.EdgesAdded, EdgesRemoved: outcome.EdgesRemoved,
		Files: outcome.Files, Symbols: outcome.Symbols, Edges: outcome.Edges,
		Millis: outcome.Duration.Milliseconds(), Truncated: outcome.Truncated,
		Reason: outcome.Reason,
	}, nil
}

// graphPass picks the pass, mirroring the syncer's own rule.
func (s *Service) graphPass(
	ctx context.Context, req codegraph.SyncRequest, full bool, commit string,
) (codegraph.SyncOutcome, error) {
	if full {
		return s.graph.Build(ctx, req)
	}
	state, found, err := s.graph.Status(ctx, req.ProjectID, req.RepoID)
	if err != nil {
		return codegraph.SyncOutcome{}, err
	}
	if !found || !state.Indexed() || state.IndexedCommit == "" || commit == "" || state.IndexedCommit == commit {
		return s.graph.Build(ctx, req)
	}
	changes, err := pm.ChangesSinceCommit(ctx, req.RepoPath, state.IndexedCommit, commit)
	if err != nil {
		// No provable change set -- a force-push, a shallow clone, a rewritten
		// history. A full build is the only honest answer, because an
		// incremental one would leave holes AO cannot detect.
		return s.graph.Build(ctx, req)
	}
	return s.graph.Apply(ctx, req, pm.GraphDiff(changes))
}

// GraphQuery answers one bounded question, and is the diagnostic that shows an
// operator exactly what a dispatch would be told.
func (s *Service) GraphQuery(
	ctx context.Context, q controllers.ProjectMemoryGraphQuery,
) (controllers.ProjectMemoryGraphAnswer, error) {
	if s.graph == nil {
		return controllers.ProjectMemoryGraphAnswer{}, fmt.Errorf("no code graph is configured for this daemon")
	}
	resolved, err := s.resolveRepo(ctx, q.ProjectID, q.RepoPath)
	if err != nil {
		return controllers.ProjectMemoryGraphAnswer{}, err
	}
	repoID := domain.ProjectMemoryRepoID(resolved)

	state, found, err := s.graph.Status(ctx, q.ProjectID, repoID)
	if err != nil {
		return controllers.ProjectMemoryGraphAnswer{}, err
	}
	if !found || !state.Indexed() {
		return controllers.ProjectMemoryGraphAnswer{
			RepoID: repoID,
			Reason: "this repository's code graph has not been built yet; run `ao memory graph sync`",
		}, nil
	}

	var symbols []string
	if symbol := strings.TrimSpace(q.Symbol); symbol != "" {
		symbols = []string{symbol}
	}
	var files []string
	if path := strings.TrimSpace(q.Path); path != "" {
		files = []string{path}
	}
	neighborhood, err := s.graph.Retrieve(ctx, q.ProjectID, repoID, codegraph.RetrieveRequest{
		Symbols: symbols, Files: files, Terms: q.Terms, MaxSymbols: q.Limit,
	})
	if err != nil {
		return controllers.ProjectMemoryGraphAnswer{}, err
	}

	answer := controllers.ProjectMemoryGraphAnswer{
		RepoID: repoID, Generation: state.ServedGeneration, IndexedCommit: state.IndexedCommit,
		Tables: neighborhood.Tables, Files: neighborhood.Files,
		ConsideredSymbols: neighborhood.ConsideredSymbols,
		ConsideredEdges:   neighborhood.ConsideredEdges,
		Truncated:         neighborhood.Truncated,
	}
	for _, scored := range neighborhood.Symbols {
		answer.Symbols = append(answer.Symbols, graphSymbol(scored.Symbol, scored.Score, scored.Reason))
	}
	for _, sym := range neighborhood.Tests {
		answer.Tests = append(answer.Tests, graphSymbol(sym, 0, ""))
	}
	for _, sym := range neighborhood.Endpoints {
		answer.Endpoints = append(answer.Endpoints, graphSymbol(sym, 0, ""))
	}
	for _, edge := range neighborhood.Callers {
		answer.Callers = append(answer.Callers, graphEdge(edge))
	}
	for _, edge := range neighborhood.Callees {
		answer.Callees = append(answer.Callees, graphEdge(edge))
	}
	if len(answer.Symbols) == 0 {
		answer.Reason = "the code graph held nothing matching that question"
	}
	return answer, nil
}

func graphSymbol(sym codegraph.Symbol, score float64, reason string) controllers.ProjectMemoryGraphSymbol {
	return controllers.ProjectMemoryGraphSymbol{
		ID: sym.ID, Name: sym.Name, Kind: string(sym.Kind), Path: sym.File,
		Line: sym.Line, Signature: sym.Signature, Summary: sym.Summary,
		Exported: sym.Exported, Score: score, Reason: reason,
	}
}

func graphEdge(edge codegraph.Edge) controllers.ProjectMemoryGraphEdge {
	return controllers.ProjectMemoryGraphEdge{
		Kind: string(edge.Kind), From: edge.From, To: edge.To, Line: edge.Line,
	}
}

// Compile-time proof that this service satisfies the graph contract too.
var _ controllers.ProjectMemoryGraphService = (*Service)(nil)
