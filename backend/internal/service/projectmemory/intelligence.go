package projectmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// intelligence.go -- P4-G's read surface: what AO knows about a project, made
// answerable from the frontend.
//
// Almost nothing here is a new fact. The durable rows already carry the
// architecture (rendered at build time), the counts, the commits, the sync
// measurements and the context manifests; P3-E already measured what a pack
// weighs. What was missing was a way to ASK, so this file is mostly
// projection, and the one genuinely new capability is bounded traversal.
//
// The traversal is new because P3 deliberately refused to build it: "there is
// deliberately no traversal or export endpoint ... an endpoint that returns
// the whole graph would be one nobody could use". That reasoning still holds,
// which is why what P4-G adds is not an export but a walk with a hard ceiling
// on both nodes and edges -- see Subgraph.

// GraphExplorer is the read surface bounded traversal and search need.
//
// Declared as a port rather than taking *store.Store so the traversal can be
// tested against a fake with a known shape, which is the only way to assert
// the bounds actually bind on a graph big enough to exceed them.
type GraphExplorer interface {
	ListCodeGraphEdgesFromKeys(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, keys []string) ([]store.CodeGraphEdgeRecord, error)
	ListCodeGraphEdgesToKeys(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, keys []string) ([]store.CodeGraphEdgeRecord, error)
	ListCodeGraphSymbolsByName(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, name string, limit int64) ([]store.CodeGraphSymbolRecord, error)
	SearchCodeGraphSymbolNames(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, term string, limit int64) ([]store.CodeGraphSymbolRecord, error)
	ListCodeGraphSymbolsForPath(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, path string) ([]store.CodeGraphSymbolRecord, error)
	ListCodeGraphSymbolsForPaths(ctx context.Context, projectID domain.ProjectID, repoID string, generation int64, paths []string) ([]store.CodeGraphSymbolRecord, error)
}

// WithExplorer attaches the traversal/search reads. Without it the subgraph
// and search routes report not-implemented, matching how the graph itself is
// optional.
func (s *Service) WithExplorer(e GraphExplorer) *Service {
	s.explorer = e
	return s
}

// Traversal ceilings. These are hard caps, not defaults a caller can raise:
// the whole point of the endpoint is that no request can ask for the whole
// graph, and a limit the caller chooses is not a limit.
const (
	// MaxSubgraphNodes bounds one traversal's node count.
	MaxSubgraphNodes = 300
	// MaxSubgraphEdges bounds one traversal's edge count.
	MaxSubgraphEdges = 900
	// MaxSubgraphDepth bounds how far a traversal walks. Two hops is what
	// "show me what this touches and what touches that" needs; three on a
	// densely-connected symbol is already tens of thousands of nodes.
	MaxSubgraphDepth = 2
	// MaxSearchResults bounds a search response.
	MaxSearchResults = 100
)

// Intelligence is the Overview tab's whole answer for one project.
type Intelligence struct {
	ProjectID string
	Repos     []RepoIntelligence
}

// RepoIntelligence is one repository's state, as a person reads it.
type RepoIntelligence struct {
	RepoID   string
	RepoPath string
	Backend  string
	// State is the derived vocabulary: pending, indexing, ready, stale, failed.
	State string
	// Drift explains a stale state in words, and is empty otherwise.
	Drift string
	// IndexedCommit is what the served graph describes; HeadCommit is where
	// the checkout actually is now. Both are shown, because "these differ" is
	// the whole of what stale means and a person should be able to see it
	// rather than be told it.
	IndexedCommit string
	HeadCommit    string
	Branch        string
	Generation    int64
	Files         int64
	Symbols       int64
	Edges         int64
	LastSyncKind  string
	FilesParsed   int64
	FilesReused   int64
	FilesRemoved  int64
	LastMillis    int64
	LastError     string
	UpdatedAt     string
	// MemoryItems is how many durable project-memory facts this repository
	// has, when memory is wired.
	MemoryItems int64
	MemoryState string
}

// intelligence reports every repository's state for the Overview tab.
func (s *Service) intelligence(ctx context.Context, projectID domain.ProjectID) (Intelligence, error) {
	out := Intelligence{ProjectID: string(projectID)}
	if s.graph == nil {
		return out, fmt.Errorf("no code graph is configured for this daemon")
	}
	states, err := s.graph.StatusAll(ctx, projectID)
	if err != nil {
		return Intelligence{}, err
	}
	if len(states) == 0 {
		// No row at all is the honest "pending": the project exists and
		// nothing has been indexed yet. Reporting an empty list instead would
		// render as "this project has no repositories", which is a different
		// and wrong claim.
		resolved, resolveErr := s.resolveRepo(ctx, projectID, "")
		if resolveErr == nil {
			head, branch := pm.HeadOf(ctx, resolved)
			out.Repos = append(out.Repos, RepoIntelligence{
				RepoID:     domain.ProjectMemoryRepoID(resolved),
				RepoPath:   resolved,
				State:      string(IntelligencePending),
				HeadCommit: head,
				Branch:     branch,
			})
		}
		return out, nil
	}
	memoryByRepo := s.memoryStatusByRepo(ctx, projectID)
	for _, state := range states {
		drift := graphDrift(ctx, state)
		head, branch := pm.HeadOf(ctx, state.RepoPath)
		repo := RepoIntelligence{
			RepoID: state.RepoID, RepoPath: state.RepoPath, Backend: state.Backend,
			State: string(intelligenceState(state, drift)), Drift: drift,
			IndexedCommit: state.IndexedCommit, HeadCommit: head, Branch: branch,
			Generation: state.ServedGeneration,
			Files:      state.FileCount, Symbols: state.SymbolCount, Edges: state.EdgeCount,
			LastSyncKind: string(state.LastSyncKind),
			FilesParsed:  state.LastFilesParsed, FilesReused: state.LastFilesReused,
			FilesRemoved: state.LastFilesRemoved,
			LastMillis:   state.LastDuration.Milliseconds(),
			LastError:    state.LastError,
		}
		if !state.UpdatedAt.IsZero() {
			repo.UpdatedAt = state.UpdatedAt.UTC().Format(time.RFC3339)
		}
		if m, ok := memoryByRepo[state.RepoID]; ok {
			repo.MemoryItems = m.Items
			repo.MemoryState = m.State
		}
		out.Repos = append(out.Repos, repo)
	}
	return out, nil
}

type memorySummary struct {
	Items int64
	State string
}

// memoryStatusByRepo reads project memory's own status alongside the graph's.
// Failures are swallowed into an absent entry: memory is optional, and a
// daemon without it must still render the graph half of the Overview.
func (s *Service) memoryStatusByRepo(ctx context.Context, projectID domain.ProjectID) map[string]memorySummary {
	out := map[string]memorySummary{}
	if s.memory == nil {
		return out
	}
	statuses, err := s.memory.StatusAll(ctx, projectID)
	if err != nil {
		return out
	}
	for _, st := range statuses {
		out[st.RepoID] = memorySummary{Items: int64(st.Counts.Valid), State: string(st.Index.Phase)}
	}
	return out
}

// Architecture serves the structural summary the build already derived.
//
// It is READ, never recomputed. The summary was rendered at build time from
// the graph the build already held in memory, which is the only moment it is
// cheap; recomputing it per request would walk every file of every repository
// to produce a value that cannot have changed since the generation it belongs
// to.
func (s *Service) Architecture(ctx context.Context, projectID domain.ProjectID, repoPath string) (map[string]any, string, error) {
	if s.graph == nil {
		return nil, "", fmt.Errorf("no code graph is configured for this daemon")
	}
	state, err := s.repoState(ctx, projectID, repoPath)
	if err != nil {
		return nil, "", err
	}
	if state.ArchitectureJSON == "" {
		// Nothing derived yet is not an error: it is the pending state, and
		// the Overview already says so. Returning an empty object lets the tab
		// render its own "not indexed yet" rather than an error toast.
		return map[string]any{}, state.Architecture, nil
	}
	var parsed map[string]any
	if decodeErr := json.Unmarshal([]byte(state.ArchitectureJSON), &parsed); decodeErr != nil {
		// A structured summary that will not decode is a bug worth fixing and
		// not worth failing the tab over: the rendered text form carries the
		// same facts, it is what a dispatch is actually given, and it is
		// readable. Mirrors codegraph.Index.Architecture's own choice.
		//
		//nolint:nilerr // degrading to the rendered form is the intended
		// answer here; surfacing the decode error would fail a tab that has
		// perfectly good facts to show.
		return map[string]any{}, state.Architecture, nil
	}
	return parsed, state.Architecture, nil
}

// SubgraphRequest is one bounded traversal.
type SubgraphRequest struct {
	ProjectID domain.ProjectID
	RepoPath  string
	// Symbol or Path seeds the walk. A request naming neither is refused:
	// "start anywhere" is the whole-graph export this endpoint exists to not
	// be.
	Symbol string
	Path   string
	Depth  int
	// NodeKinds and EdgeKinds filter, and empty means no filter.
	NodeKinds []string
	EdgeKinds []string
	MaxNodes  int
	MaxEdges  int
}

// SubgraphNode is one node of a bounded traversal.
type SubgraphNode struct {
	Key       string
	Name      string
	Kind      string
	Path      string
	Language  string
	Line      int64
	Signature string
	Summary   string
	Exported  bool
	// Depth is how many hops from the seed this node was reached at, so the
	// renderer can lay the walk out without recomputing it.
	Depth int
}

// SubgraphEdge is one relation of a bounded traversal.
type SubgraphEdge struct {
	Kind string
	From string
	To   string
	Path string
	Line int64
}

// Subgraph is a bounded neighbourhood.
type Subgraph struct {
	Seeds []string
	Nodes []SubgraphNode
	Edges []SubgraphEdge
	// Truncated reports that the walk hit a ceiling and the neighbourhood is
	// incomplete. It is surfaced rather than hidden: a graph view that
	// silently drops half the edges teaches people something false about the
	// codebase.
	Truncated bool
	// Generation and IndexedCommit are the provenance of everything above.
	Generation    int64
	IndexedCommit string
}

// Subgraph walks outward from a seed, bounded in every direction.
//
// The bounds are the design. Existing projects have tens of thousands of
// symbols and hundreds of thousands of relations, and the only way a graph
// view stays usable on one is to never ask for more than a screen's worth. So:
// a seed is required, depth is capped at two hops, and both nodes and edges
// have hard ceilings that a caller cannot raise. Hitting one sets Truncated
// rather than silently returning a partial answer as if it were whole.
func (s *Service) Subgraph(ctx context.Context, req SubgraphRequest) (Subgraph, error) {
	if s.graph == nil || s.explorer == nil {
		return Subgraph{}, fmt.Errorf("no code graph is configured for this daemon")
	}
	if strings.TrimSpace(req.Symbol) == "" && strings.TrimSpace(req.Path) == "" {
		return Subgraph{}, apierr.Invalid("GRAPH_SEED_REQUIRED",
			"name a symbol or a file to start from", nil)
	}
	state, err := s.repoState(ctx, req.ProjectID, req.RepoPath)
	if err != nil {
		return Subgraph{}, err
	}
	if !state.Indexed() {
		return Subgraph{}, apierr.NotFound("GRAPH_NOT_INDEXED",
			"this repository has not been indexed yet")
	}

	depth := req.Depth
	if depth <= 0 {
		depth = 1
	}
	if depth > MaxSubgraphDepth {
		depth = MaxSubgraphDepth
	}
	maxNodes := clampLimit(req.MaxNodes, MaxSubgraphNodes)
	maxEdges := clampLimit(req.MaxEdges, MaxSubgraphEdges)

	seeds, err := s.seedSymbols(ctx, state, req)
	if err != nil {
		return Subgraph{}, err
	}
	out := Subgraph{Generation: state.ServedGeneration, IndexedCommit: state.IndexedCommit}
	if len(seeds) == 0 {
		return out, nil
	}

	nodeKinds := lowerSet(req.NodeKinds)
	edgeKinds := lowerSet(req.EdgeKinds)

	nodes := map[string]SubgraphNode{}
	edgeSeen := map[string]bool{}
	// frontier holds the symbols to expand from next. Both key sets are kept
	// because the two directions are asked in different vocabularies -- see
	// expandKeys.
	frontier := make([]store.CodeGraphSymbolRecord, 0, len(seeds))
	for _, sym := range seeds {
		if len(nodes) >= maxNodes {
			out.Truncated = true
			break
		}
		if !keepNode(sym, nodeKinds) {
			continue
		}
		nodes[sym.SymbolID] = subgraphNode(sym, 0)
		frontier = append(frontier, sym)
		out.Seeds = append(out.Seeds, sym.SymbolID)
	}

	for hop := 1; hop <= depth && len(frontier) > 0; hop++ {
		if len(nodes) >= maxNodes || len(out.Edges) >= maxEdges {
			out.Truncated = true
			break
		}
		fromKeys, toKeys := expandKeys(frontier)
		outgoing, err := s.explorer.ListCodeGraphEdgesFromKeys(ctx, req.ProjectID, state.RepoID, state.ServedGeneration, fromKeys)
		if err != nil {
			return Subgraph{}, err
		}
		incoming, err := s.explorer.ListCodeGraphEdgesToKeys(ctx, req.ProjectID, state.RepoID, state.ServedGeneration, toKeys)
		if err != nil {
			return Subgraph{}, err
		}
		next := map[string]bool{}
		for _, edge := range append(outgoing, incoming...) {
			if len(edgeKinds) > 0 && !edgeKinds[strings.ToLower(edge.Kind)] {
				continue
			}
			id := edge.Kind + "\x00" + edge.FromKey + "\x00" + edge.ToKey
			if edgeSeen[id] {
				continue
			}
			if len(out.Edges) >= maxEdges {
				out.Truncated = true
				break
			}
			edgeSeen[id] = true
			out.Edges = append(out.Edges, SubgraphEdge{
				Kind: edge.Kind, From: edge.FromKey, To: edge.ToKey,
				Path: edge.Path, Line: edge.Line,
			})
			for _, key := range []string{edge.FromKey, edge.ToKey} {
				if _, known := nodes[key]; !known && key != "" {
					next[key] = true
				}
			}
		}
		if len(next) == 0 {
			break
		}
		pending := make([]string, 0, len(next))
		for key := range next {
			if len(nodes)+len(pending) >= maxNodes {
				out.Truncated = true
				break
			}
			pending = append(pending, key)
		}
		sort.Strings(pending)
		resolved, err := s.resolveEndpoints(ctx, req.ProjectID, state, pending)
		if err != nil {
			return Subgraph{}, err
		}
		frontier = frontier[:0]
		for _, key := range pending {
			sym, ok := resolved[key]
			if !ok {
				// An endpoint with no declaration in this generation: a call
				// into a dependency AO does not index (fmt.Println, a
				// node_modules export). The relation is real and is kept, and
				// the node is drawn as external rather than invented -- the
				// graph should show that the code reaches outside itself, not
				// pretend the call went nowhere.
				if len(nodeKinds) == 0 {
					nodes[key] = SubgraphNode{Key: key, Name: key, Kind: "external", Depth: hop}
				}
				continue
			}
			if !keepNode(sym, nodeKinds) {
				continue
			}
			nodes[key] = subgraphNode(sym, hop)
			frontier = append(frontier, sym)
		}
	}

	out.Nodes = make([]SubgraphNode, 0, len(nodes))
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		if out.Nodes[i].Depth != out.Nodes[j].Depth {
			return out.Nodes[i].Depth < out.Nodes[j].Depth
		}
		return out.Nodes[i].Key < out.Nodes[j].Key
	})
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].From != out.Edges[j].From {
			return out.Edges[i].From < out.Edges[j].From
		}
		if out.Edges[i].To != out.Edges[j].To {
			return out.Edges[i].To < out.Edges[j].To
		}
		return out.Edges[i].Kind < out.Edges[j].Kind
	})
	return out, nil
}

// seedSymbols resolves the traversal's starting points.
func (s *Service) seedSymbols(ctx context.Context, state store.CodeGraphState, req SubgraphRequest) ([]store.CodeGraphSymbolRecord, error) {
	if sym := strings.TrimSpace(req.Symbol); sym != "" {
		found, err := s.explorer.ListCodeGraphSymbolsByName(ctx, req.ProjectID, state.RepoID, state.ServedGeneration, sym, int64(MaxSubgraphNodes))
		if err != nil {
			return nil, err
		}
		if len(found) > 0 {
			return found, nil
		}
		// Fall back to a prefix search so a half-remembered name still starts
		// somewhere useful rather than returning nothing.
		return s.explorer.SearchCodeGraphSymbolNames(ctx, req.ProjectID, state.RepoID, state.ServedGeneration, sym, int64(MaxSubgraphNodes))
	}
	return s.explorer.ListCodeGraphSymbolsForPath(ctx, req.ProjectID, state.RepoID, state.ServedGeneration, strings.TrimSpace(req.Path))
}

// expandKeys builds the two key sets one hop needs.
//
// The two directions are asked in DIFFERENT vocabularies, and getting this
// wrong is why an early version of this traversal returned isolated nodes on a
// graph with a hundred and seventy thousand relations. An edge's from_key is a
// symbol id; its to_key is an unresolved NAME -- a callee expression, a type
// name, an imported module -- because the native indexer records what it
// parsed without resolving it to a declaration. So outgoing edges are found by
// symbol id and incoming edges by name, exactly as the retrieval path's own
// incomingKeys does. This mirrors that rule rather than inventing a second
// one.
func expandKeys(frontier []store.CodeGraphSymbolRecord) (fromKeys, toKeys []string) {
	seenFrom := map[string]bool{}
	seenTo := map[string]bool{}
	add := func(set map[string]bool, keys *[]string, key string) {
		if key == "" || set[key] {
			return
		}
		set[key] = true
		*keys = append(*keys, key)
	}
	for _, sym := range frontier {
		add(seenFrom, &fromKeys, sym.SymbolID)
		add(seenTo, &toKeys, sym.SymbolID)
		add(seenTo, &toKeys, sym.Name)
		// A method recorded as "Receiver.Method" is also called by its bare
		// name, which is how most call sites in the graph refer to it.
		if _, member, ok := strings.Cut(sym.Name, "."); ok && member != "" {
			add(seenTo, &toKeys, member)
		}
	}
	return fromKeys, toKeys
}

// resolveEndpoints turns edge endpoints back into declarations, in a bounded
// number of reads rather than one per endpoint.
//
// Endpoints arrive in both vocabularies, so both are tried: a symbol id is
// resolved through its file (one read per distinct file), and a bare name
// through the name index. An endpoint that resolves to neither is external to
// the repository, and the caller draws it as such.
func (s *Service) resolveEndpoints(
	ctx context.Context, projectID domain.ProjectID, state store.CodeGraphState, keys []string,
) (map[string]store.CodeGraphSymbolRecord, error) {
	out := make(map[string]store.CodeGraphSymbolRecord, len(keys))
	paths := map[string]bool{}
	var names []string
	for _, key := range keys {
		if i := strings.IndexByte(key, '#'); i > 0 {
			paths[key[:i]] = true
			continue
		}
		names = append(names, key)
	}
	if len(paths) > 0 {
		list := make([]string, 0, len(paths))
		for p := range paths {
			list = append(list, p)
		}
		sort.Strings(list)
		symbols, err := s.explorer.ListCodeGraphSymbolsForPaths(ctx, projectID, state.RepoID, state.ServedGeneration, list)
		if err != nil {
			return nil, err
		}
		for _, sym := range symbols {
			out[sym.SymbolID] = sym
		}
	}
	for _, name := range names {
		symbols, err := s.explorer.ListCodeGraphSymbolsByName(ctx, projectID, state.RepoID, state.ServedGeneration, name, 1)
		if err != nil {
			return nil, err
		}
		if len(symbols) > 0 {
			out[name] = symbols[0]
		}
	}
	return out, nil
}

func subgraphNode(sym store.CodeGraphSymbolRecord, depth int) SubgraphNode {
	return SubgraphNode{
		Key: sym.SymbolID, Name: sym.Name, Kind: sym.Kind, Path: sym.Path,
		Language: sym.Language, Line: sym.Line, Signature: sym.Signature,
		Summary: sym.Summary, Exported: sym.Exported, Depth: depth,
	}
}

func keepNode(sym store.CodeGraphSymbolRecord, kinds map[string]bool) bool {
	return len(kinds) == 0 || kinds[strings.ToLower(sym.Kind)]
}

func lowerSet(in []string) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for _, v := range in {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out[v] = true
		}
	}
	return out
}

func clampLimit(requested, ceiling int) int {
	if requested <= 0 || requested > ceiling {
		return ceiling
	}
	return requested
}

// repoState resolves a project (and optional repo path) to its graph row.
func (s *Service) repoState(ctx context.Context, projectID domain.ProjectID, repoPath string) (store.CodeGraphState, error) {
	resolved, err := s.resolveRepo(ctx, projectID, repoPath)
	if err != nil {
		return store.CodeGraphState{}, err
	}
	state, found, err := s.graph.Status(ctx, projectID, domain.ProjectMemoryRepoID(resolved))
	if err != nil {
		return store.CodeGraphState{}, err
	}
	if !found {
		return store.CodeGraphState{}, apierr.NotFound("GRAPH_NOT_INDEXED",
			"this repository has not been indexed yet")
	}
	return state, nil
}
