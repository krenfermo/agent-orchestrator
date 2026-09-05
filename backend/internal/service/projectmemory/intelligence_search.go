package projectmemory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// intelligence_search.go -- the Search and Context tabs.
//
// Search deliberately does NOT build a retrieval engine. It asks the two
// authorities AO already has -- durable project memory and the code graph --
// and merges their answers, because a third index would be a third thing that
// can disagree with the repository and a third thing to keep current. What it
// adds over the existing per-subsystem reads is one question and one answer
// list with provenance on every row, which is what makes a result actionable
// rather than merely present.

// SearchRequest is one Project Intelligence question.
type SearchRequest struct {
	ProjectID domain.ProjectID
	RepoPath  string
	Query     string
	Limit     int
}

// SearchHit is one answer, from whichever authority produced it.
type SearchHit struct {
	// Kind is "memory" or "symbol": which authority this came from. It is on
	// every row because a durable fact somebody wrote and a symbol AO parsed
	// are different KINDS of claim, and collapsing them would let one borrow
	// the other's credibility.
	Kind string
	// Title is the symbol name or the fact's summary.
	Title string
	// Detail is the signature or the fact's body.
	Detail string
	Path   string
	Line   int64
	// SymbolKind is set for a symbol hit ("func", "type", ...).
	SymbolKind string
	// MemoryType is set for a memory hit ("convention", "module", ...).
	MemoryType string
	// State is the memory item's state, so a stale fact is visibly stale
	// rather than quietly presented as current.
	State string
	// SourceCommit is the provenance of a memory hit.
	SourceCommit string
	// Score is the rank this row was ordered by. Reported so an operator can
	// see WHY one row came above another rather than having to trust it.
	Score int
}

// SearchResult is one search's whole answer.
type SearchResult struct {
	Query string
	Hits  []SearchHit
	// MemoryHits and SymbolHits are the per-authority counts, so "the graph
	// found nothing but memory did" is visible.
	MemoryHits int
	SymbolHits int
	Truncated  bool
	// Generation and IndexedCommit are the graph provenance the symbol half
	// was answered from.
	Generation    int64
	IndexedCommit string
}

// Search answers a Project Intelligence question from memory and the graph.
func (s *Service) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return SearchResult{}, fmt.Errorf("a search needs a question")
	}
	limit := clampLimit(req.Limit, MaxSearchResults)
	out := SearchResult{Query: query}

	terms := searchTerms(query)

	// Memory half.
	if s.memory != nil {
		resolved, err := s.resolveRepo(ctx, req.ProjectID, req.RepoPath)
		if err == nil {
			res, err := s.memory.Inspect(ctx, pm.InspectRequest{
				ProjectID: req.ProjectID, RepoPath: resolved, Limit: pm.DefaultInspectLimit,
			})
			if err == nil {
				for _, item := range res.Items {
					score := scoreMemoryItem(item, terms)
					if score == 0 {
						continue
					}
					out.Hits = append(out.Hits, SearchHit{
						Kind: "memory", Title: item.Summary, Detail: item.Content,
						Path:  firstPath(item.SourcePaths),
						State: string(item.State), MemoryType: string(item.Key.Type),
						SourceCommit: item.SourceCommit, Score: score,
					})
					out.MemoryHits++
				}
			}
		}
	}

	// Graph half.
	if s.graph != nil && s.explorer != nil {
		state, err := s.repoState(ctx, req.ProjectID, req.RepoPath)
		if err == nil && state.Indexed() {
			out.Generation = state.ServedGeneration
			out.IndexedCommit = state.IndexedCommit
			seen := map[string]bool{}
			for _, term := range terms {
				symbols, err := s.explorer.SearchCodeGraphSymbolNames(ctx, req.ProjectID, state.RepoID, state.ServedGeneration, term, int64(limit))
				if err != nil {
					break
				}
				for _, sym := range symbols {
					if seen[sym.SymbolID] {
						continue
					}
					seen[sym.SymbolID] = true
					out.Hits = append(out.Hits, SearchHit{
						Kind: "symbol", Title: sym.Name, Detail: symbolDetail(sym.Signature, sym.Summary),
						Path: sym.Path, Line: sym.Line, SymbolKind: sym.Kind,
						Score: scoreSymbol(sym.Name, sym.Path, terms),
					})
					out.SymbolHits++
				}
			}
		}
	}

	sort.SliceStable(out.Hits, func(i, j int) bool {
		if out.Hits[i].Score != out.Hits[j].Score {
			return out.Hits[i].Score > out.Hits[j].Score
		}
		return out.Hits[i].Title < out.Hits[j].Title
	})
	if len(out.Hits) > limit {
		out.Hits = out.Hits[:limit]
		out.Truncated = true
	}
	return out, nil
}

// searchTerms splits a question into the words worth matching.
//
// Short words are dropped because they match everything: a question in Spanish
// or English is mostly articles and prepositions, and letting "de" or "the"
// into the term list turns every symbol in the repository into a hit.
func searchTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		// Split on everything that is not part of an identifier or a word.
		// Accented letters are kept: the question may well be in Spanish.
		return r != '_' && r != '.' && r != '/' &&
			(r < 'a' || r > 'z') && (r < '0' || r > '9') && r < 0x00C0
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		if len([]rune(f)) < 3 || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// scoreMemoryItem ranks a durable fact against the question's terms. A term in
// the summary counts for more than one in the body, because the summary is
// what the fact claims to be about.
func scoreMemoryItem(item domain.ProjectMemoryItem, terms []string) int {
	summary := strings.ToLower(item.Summary)
	content := strings.ToLower(item.Content)
	paths := strings.ToLower(strings.Join(item.SourcePaths, " "))
	score := 0
	for _, term := range terms {
		switch {
		case strings.Contains(summary, term):
			score += 4
		case strings.Contains(paths, term):
			score += 2
		case strings.Contains(content, term):
			score++
		}
	}
	if score > 0 && item.State != domain.MemoryStateValid {
		// A stale or invalidated fact still answers the question, but it must
		// not outrank a current one that answers it equally well.
		score--
	}
	return score
}

func scoreSymbol(name, path string, terms []string) int {
	lowerName := strings.ToLower(name)
	lowerPath := strings.ToLower(path)
	score := 0
	for _, term := range terms {
		switch {
		case lowerName == term:
			score += 6
		case strings.Contains(lowerName, term):
			score += 3
		case strings.Contains(lowerPath, term):
			score++
		}
	}
	return score
}

func symbolDetail(signature, summary string) string {
	if signature != "" {
		return signature
	}
	return summary
}

func firstPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}
