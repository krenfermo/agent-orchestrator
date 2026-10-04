package projectmemory

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

// intelligence_api.go -- the projection onto the controller's wire types.
//
// Kept apart from the logic beside it for the reason the rest of this package
// keeps vocabularies apart: the service reasons in its own types, and the HTTP
// layer's shapes are a contract with the frontend that must be free to change
// independently. This file is the only place the two meet.

var _ controllers.ProjectIntelligenceService = (*Service)(nil)

// Intelligence implements the controller's overview read.
func (s *Service) Intelligence(ctx context.Context, projectID domain.ProjectID) (controllers.ProjectIntelligenceOverview, error) {
	in, err := s.intelligence(ctx, projectID)
	if err != nil {
		return controllers.ProjectIntelligenceOverview{}, err
	}
	out := controllers.ProjectIntelligenceOverview{
		ProjectID: in.ProjectID,
		Repos:     make([]controllers.ProjectIntelligenceRepoStatus, 0, len(in.Repos)),
	}
	for _, repo := range in.Repos {
		out.Repos = append(out.Repos, controllers.ProjectIntelligenceRepoStatus(repo))
	}
	return out, nil
}

// IntelligenceArchitecture implements the controller's architecture read.
func (s *Service) IntelligenceArchitecture(
	ctx context.Context, projectID domain.ProjectID, repoPath string,
) (controllers.ProjectIntelligenceArchitecture, error) {
	parsed, rendered, err := s.Architecture(ctx, projectID, repoPath)
	if err != nil {
		return controllers.ProjectIntelligenceArchitecture{}, err
	}
	return controllers.ProjectIntelligenceArchitecture{Architecture: parsed, Rendered: rendered}, nil
}

// IntelligenceSubgraph implements the controller's bounded traversal.
func (s *Service) IntelligenceSubgraph(
	ctx context.Context, req controllers.ProjectIntelligenceSubgraphQuery,
) (controllers.ProjectIntelligenceSubgraph, error) {
	sub, err := s.Subgraph(ctx, SubgraphRequest{
		ProjectID: req.ProjectID, RepoPath: req.RepoPath,
		Symbol: req.Symbol, Path: req.Path, Depth: req.Depth,
		NodeKinds: req.NodeKinds, EdgeKinds: req.EdgeKinds,
		MaxNodes: req.MaxNodes, MaxEdges: req.MaxEdges,
	})
	if err != nil {
		return controllers.ProjectIntelligenceSubgraph{}, err
	}
	out := controllers.ProjectIntelligenceSubgraph{
		Seeds: sub.Seeds, Truncated: sub.Truncated,
		Generation: sub.Generation, IndexedCommit: sub.IndexedCommit,
		Nodes: make([]controllers.ProjectIntelligenceSubgraphNode, 0, len(sub.Nodes)),
		Edges: make([]controllers.ProjectIntelligenceSubgraphEdge, 0, len(sub.Edges)),
	}
	for _, n := range sub.Nodes {
		out.Nodes = append(out.Nodes, controllers.ProjectIntelligenceSubgraphNode(n))
	}
	for _, e := range sub.Edges {
		out.Edges = append(out.Edges, controllers.ProjectIntelligenceSubgraphEdge(e))
	}
	if out.Seeds == nil {
		out.Seeds = []string{}
	}
	return out, nil
}

// IntelligenceSearch implements the controller's search.
func (s *Service) IntelligenceSearch(
	ctx context.Context, req controllers.ProjectIntelligenceSearchQuery,
) (controllers.ProjectIntelligenceSearchResult, error) {
	res, err := s.Search(ctx, SearchRequest{
		ProjectID: req.ProjectID, RepoPath: req.RepoPath,
		Query: req.Query, Limit: req.Limit,
	})
	if err != nil {
		return controllers.ProjectIntelligenceSearchResult{}, err
	}
	out := controllers.ProjectIntelligenceSearchResult{
		Query: res.Query, MemoryHits: res.MemoryHits, SymbolHits: res.SymbolHits,
		Truncated: res.Truncated, Generation: res.Generation, IndexedCommit: res.IndexedCommit,
		Hits: make([]controllers.ProjectIntelligenceSearchHit, 0, len(res.Hits)),
	}
	for _, hit := range res.Hits {
		out.Hits = append(out.Hits, controllers.ProjectIntelligenceSearchHit(hit))
	}
	return out, nil
}

// IntelligenceContext implements the controller's context-pack preview.
func (s *Service) IntelligenceContext(
	ctx context.Context, req controllers.ProjectIntelligenceContextQuery,
) (controllers.ProjectIntelligenceContextPreview, error) {
	preview, err := s.ContextPreview(ctx, ContextPreviewRequest{
		ProjectID: req.ProjectID, RepoPath: req.RepoPath, Role: req.Role,
		ChangedPaths: req.ChangedPaths, Keywords: req.Keywords,
	})
	if err != nil {
		return controllers.ProjectIntelligenceContextPreview{}, err
	}
	out := controllers.ProjectIntelligenceContextPreview{
		Role: preview.Role, ProjectID: preview.ProjectID, RepoID: preview.RepoID,
		Graph:          controllers.ProjectIntelligenceContextGraph(preview.Graph),
		CandidateItems: preview.CandidateItems, CandidateBytes: preview.CandidateBytes,
		SelectedItems: preview.SelectedItems, SelectedBytes: preview.SelectedBytes,
		EstimatedTokens: preview.EstimatedTokens,
		DroppedItems:    preview.DroppedItems, DroppedToSummary: preview.DroppedToSummary,
		StaleExcluded: preview.StaleExcluded, SourcesReused: preview.SourcesReused,
		FallbackReason: preview.FallbackReason, IndexedCommit: preview.IndexedCommit,
		Generation: preview.Generation, Digest: preview.Digest, Empty: preview.Empty,
		Sections: make([]controllers.ProjectIntelligenceContextSection, 0, len(preview.Sections)),
	}
	for _, section := range preview.Sections {
		s := controllers.ProjectIntelligenceContextSection{
			Title: section.Title, Type: section.Type,
			Items: make([]controllers.ProjectIntelligenceContextItem, 0, len(section.Items)),
		}
		for _, item := range section.Items {
			s.Items = append(s.Items, controllers.ProjectIntelligenceContextItem(item))
		}
		out.Sections = append(out.Sections, s)
	}
	return out, nil
}
