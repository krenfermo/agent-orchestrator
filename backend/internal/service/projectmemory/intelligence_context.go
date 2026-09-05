package projectmemory

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// intelligence_context.go -- the Context tab: what AO would actually hand an
// agent, before it hands it.
//
// This is INPUT observability and nothing else. It shows the assembled context
// pack -- the durable facts selected, the graph evidence retrieved, and what
// each weighs. It does not show, and must never show, anything a model
// produced: no reasoning, no draft, no hidden plan. The pack is AO's own
// construction from AO's own durable rows, which is exactly why it is safe to
// display.
//
// It is built by calling the SAME function a dispatch calls. A preview
// assembled by a second code path would eventually disagree with the real one,
// and a preview that disagrees is worse than none: it would be trusted.

// ContextPreviewRequest asks what one role would be given.
type ContextPreviewRequest struct {
	ProjectID domain.ProjectID
	RepoPath  string
	Role      string
	// ChangedPaths and Keywords let a person preview the context for a
	// specific piece of work rather than for the project in the abstract,
	// which is the difference between "what does AO know" and "what would AO
	// say about THIS".
	ChangedPaths []string
	Keywords     []string
}

// ContextPreviewSection is one group of durable facts in the pack.
type ContextPreviewSection struct {
	Title string
	Type  string
	Items []ContextPreviewItem
}

// ContextPreviewItem is one durable fact the pack carries.
type ContextPreviewItem struct {
	Summary string
	// Content is the bounded body, present only when the pack could afford it.
	Content string
	// BodyIncluded distinguishes "this fact has no body" from "the budget
	// could not afford it", which are different things to know when a pack
	// looks thin.
	BodyIncluded bool
	Type         string
	State        string
	SourcePaths  []string
	SourceCommit string
	Score        float64
	// Reason names the strongest signal that selected this fact, so a person
	// can see WHY it is in the pack.
	Reason string
}

// ContextPreviewGraph is the code-graph half of the pack.
type ContextPreviewGraph struct {
	Backend       string
	Generation    int64
	IndexedCommit string
	Architecture  string
	Symbols       []string
	Files         []string
	Endpoints     []string
	Tables        []string
	Tests         []string
	// The considered/selected pairs are what make the graph's contribution
	// measurable rather than asserted.
	ConsideredSymbols int
	ConsideredEdges   int
	SelectedSymbols   int
	SelectedEdges     int
	Bytes             int
	EstimatedTokens   int
}

// ContextPreview is the whole answer for one role.
type ContextPreview struct {
	Role      string
	ProjectID string
	RepoID    string
	Sections  []ContextPreviewSection
	Graph     ContextPreviewGraph

	// --- what the pack weighs (P4-G section 12) ---
	//
	// The vocabulary here is deliberately "selected" and "avoided", never
	// "saved". AO cannot see what the coding harness reads inside the
	// worktree, so it cannot know what its context prevented anybody from
	// reading; claiming a savings number would be inventing a baseline. What
	// AO CAN observe is what it considered, what it selected, and what it left
	// out -- and those are what these fields report.
	CandidateItems int
	CandidateBytes int
	SelectedItems  int
	SelectedBytes  int
	// EstimatedTokens is SelectedBytes at the router's estimate, and is named
	// as an estimate everywhere it is shown.
	EstimatedTokens int
	// DroppedItems and DroppedToSummary are the context AVOIDED: facts the
	// budget excluded outright, and facts kept without their body.
	DroppedItems     int
	DroppedToSummary int
	// StaleExcluded counts facts withheld because AO could not vouch for them.
	// It is the fail-closed rule, measured.
	StaleExcluded int
	// SourcesReused names the paths whose SUMMARISED content AO supplied from
	// memory rather than re-deriving. It is not a claim that anybody avoided
	// opening those files.
	SourcesReused  []string
	FallbackReason string
	IndexedCommit  string
	Generation     int64
	// Digest identifies this exact pack, so two previews that differ can be
	// told apart from two that only look different.
	Digest string
	// Empty reports that the pack carries nothing and a dispatch would fall
	// back to its pre-memory behaviour.
	Empty bool
}

// ContextPreview assembles the pack one role would receive, without dispatching.
func (s *Service) ContextPreview(ctx context.Context, req ContextPreviewRequest) (ContextPreview, error) {
	if s.memory == nil {
		return ContextPreview{}, fmt.Errorf("project memory is not configured for this daemon")
	}
	role, err := packRole(req.Role)
	if err != nil {
		return ContextPreview{}, err
	}
	// A Planner spans every repository, so an unresolvable repo is not fatal
	// for it; every other role is about work in one checkout.
	resolved, resolveErr := s.resolveRepo(ctx, req.ProjectID, req.RepoPath)
	if resolveErr != nil && role != pm.RolePlanner {
		return ContextPreview{}, resolveErr
	}

	pack := s.memory.Context(ctx, pm.PackRequest{
		ProjectID:    req.ProjectID,
		RepoPath:     resolved,
		Role:         role,
		ChangedPaths: req.ChangedPaths,
		Keywords:     req.Keywords,
	})

	out := ContextPreview{
		Role: string(pack.Role), ProjectID: string(pack.ProjectID), RepoID: pack.RepoID,
		CandidateItems: pack.Stats.CandidateItems, CandidateBytes: pack.Stats.CandidateBytes,
		SelectedItems: pack.Stats.SelectedItems, SelectedBytes: pack.Stats.SelectedBytes,
		EstimatedTokens: pack.Stats.SelectedTokens,
		DroppedItems:    pack.Stats.DroppedItems, DroppedToSummary: pack.Stats.DroppedToSummary,
		StaleExcluded: pack.Stats.StaleExcluded, SourcesReused: pack.Stats.SourcesReused,
		FallbackReason: pack.Stats.FallbackReason,
		IndexedCommit:  pack.Stats.IndexedCommit, Generation: pack.Stats.Generation,
		Digest: pack.Digest, Empty: pack.Empty(),
	}
	for _, section := range pack.Sections {
		s := ContextPreviewSection{Title: section.Title, Type: string(section.Type)}
		for _, sel := range section.Items {
			item := ContextPreviewItem{
				Summary: sel.Item.Summary, Type: string(sel.Item.Key.Type),
				State: string(sel.Item.State), SourcePaths: sel.Item.SourcePaths,
				SourceCommit: sel.Item.SourceCommit, Score: sel.Score,
				Reason: sel.Reason, BodyIncluded: sel.BodyIncluded,
			}
			if sel.BodyIncluded {
				item.Content = sel.Item.Content
			}
			s.Items = append(s.Items, item)
		}
		out.Sections = append(out.Sections, s)
	}

	g := pack.Graph
	out.Graph = ContextPreviewGraph{
		Backend: g.Backend, Generation: g.Generation, IndexedCommit: g.IndexedCommit,
		Architecture:      g.Architecture,
		ConsideredSymbols: g.ConsideredSymbols, ConsideredEdges: g.ConsideredEdges,
		SelectedSymbols: g.SelectedSymbols, SelectedEdges: g.SelectedEdges,
		Bytes: g.Bytes, EstimatedTokens: g.EstimatedTokens,
		Files: g.Neighborhood.Files, Tables: g.Neighborhood.Tables,
	}
	for _, sym := range g.Neighborhood.Symbols {
		out.Graph.Symbols = append(out.Graph.Symbols, sym.Symbol.ID)
	}
	for _, sym := range g.Neighborhood.Endpoints {
		out.Graph.Endpoints = append(out.Graph.Endpoints, sym.ID)
	}
	for _, sym := range g.Neighborhood.Tests {
		out.Graph.Tests = append(out.Graph.Tests, sym.ID)
	}
	return out, nil
}

// packRole maps the wire role onto the pack vocabulary. An unknown role is
// refused rather than defaulted: silently previewing a Planner pack for a
// misspelled "wroker" would be a confidently wrong answer.
func packRole(raw string) (pm.PackRole, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(pm.RolePlanner):
		return pm.RolePlanner, nil
	case string(pm.RoleWorker):
		return pm.RoleWorker, nil
	case string(pm.RoleReviewer):
		return pm.RoleReviewer, nil
	case string(pm.RoleRepair):
		return pm.RoleRepair, nil
	default:
		return "", fmt.Errorf("unknown role %q: expected planner, worker, reviewer or repair", raw)
	}
}
