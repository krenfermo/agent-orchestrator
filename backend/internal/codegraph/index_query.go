package codegraph

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// index_query.go — the read side of the project-scoped graph.
//
// Everything here runs on the dispatch path, which sets the rule it is written
// to: no query may be proportional to the size of the repository. A retrieval
// costs a bounded number of indexed lookups whether the graph holds a thousand
// symbols or half a million, and the architecture summary costs one row read
// because it was computed when the graph was last built.
//
// The scoring is deliberately the SAME function the in-memory Retrieve uses.
// One definition of "relevant" is what lets a test assert ranking against a
// small in-memory graph and have the answer mean something for the durable one.

// Retrieval read bounds.
const (
	// candidatesPerTerm caps how many symbols one task term contributes. The
	// cap is in SQL, not in Go, so a term like "service" in a large repository
	// never crosses the boundary in bulk.
	candidatesPerTerm = 120
	// candidatesPerAnchor caps the symbols one named symbol resolves to.
	candidatesPerAnchor = 32
	// edgeFanOut caps the relations pulled in around one selected symbol.
	edgeFanOut = 32
)

// Retrieve answers a task's question against the served graph.
//
// A repository with no complete graph yields an empty neighbourhood and no
// error: the caller must be able to treat "not indexed yet" exactly as it
// treats "memory is switched off", which is by carrying on.
func (ix *Index) Retrieve(
	ctx context.Context, projectID domain.ProjectID, repoID string, req RetrieveRequest,
) (Neighborhood, error) {
	req = req.Normalized()
	state, found, err := ix.repo.GetCodeGraphState(ctx, projectID, repoID)
	if err != nil || !found || !state.Indexed() {
		return Neighborhood{}, err
	}
	generation := state.ServedGeneration

	anchorSymbols := map[string]bool{}
	for _, name := range req.Symbols {
		if name = strings.TrimSpace(name); name != "" {
			anchorSymbols[name] = true
		}
	}
	anchorFiles := map[string]bool{}
	for _, path := range req.Files {
		if rel := normalizeRel(path); rel != "" {
			anchorFiles[rel] = true
		}
	}

	candidates := map[string]store.CodeGraphSymbolRecord{}
	fromAnchoredFile := map[string]bool{}
	out := Neighborhood{}

	for rel := range anchorFiles {
		records, err := ix.repo.ListCodeGraphSymbolsForPath(ctx, projectID, repoID, generation, rel)
		if err != nil {
			return Neighborhood{}, err
		}
		out.ConsideredSymbols += len(records)
		for _, record := range records {
			candidates[record.SymbolID] = record
			fromAnchoredFile[record.SymbolID] = true
		}
	}
	for name := range anchorSymbols {
		records, err := ix.repo.ListCodeGraphSymbolsByName(ctx, projectID, repoID, generation, name, candidatesPerAnchor)
		if err != nil {
			return Neighborhood{}, err
		}
		out.ConsideredSymbols += len(records)
		for _, record := range records {
			candidates[record.SymbolID] = record
		}
	}
	for _, term := range normalizeTerms(req.Terms) {
		records, err := ix.repo.SearchCodeGraphSymbols(ctx, projectID, repoID, generation, term, candidatesPerTerm)
		if err != nil {
			return Neighborhood{}, err
		}
		out.ConsideredSymbols += len(records)
		for _, record := range records {
			candidates[record.SymbolID] = record
		}
	}
	if len(candidates) == 0 {
		return out, nil
	}

	terms := normalizeTerms(req.Terms)
	scored := make([]ScoredSymbol, 0, len(candidates))
	for _, record := range candidates {
		if FileRole(roleOf(record)) == RoleGenerated && !req.IncludeGenerated {
			continue
		}
		sym := symbolFromRecord(record)
		score, reason := scoreSymbol(sym, fromAnchoredFile[record.SymbolID], anchorSymbols, terms,
			termScore(record.Path, terms)*pathTermWeight)
		if score <= 0 {
			continue
		}
		scored = append(scored, ScoredSymbol{Symbol: sym, Score: score, Reason: reason})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Symbol.ID < scored[j].Symbol.ID
	})
	if len(scored) > req.MaxSymbols {
		scored = scored[:req.MaxSymbols]
		out.Truncated = true
	}
	out.Symbols = scored

	if err := ix.expandDurable(ctx, projectID, repoID, generation, &out, req); err != nil {
		return Neighborhood{}, err
	}
	return out, nil
}

// roleOf reports a candidate's file role. The role lives on the file row, and
// carrying it on the symbol row would duplicate a fact; a generated file's
// symbols are recognised by their path instead, which is how ClassifyFile
// decided in the first place.
func roleOf(record store.CodeGraphSymbolRecord) string {
	return string(ClassifyFile(record.Path, nil))
}

// expandDurable pulls the bounded relation neighbourhood around the selected
// symbols: what they reach, what reaches them, which tests cover them, which
// routes arrive at them, which tables they touch.
//
// It does so in three round trips, whatever the neighbourhood's size: the
// edges leaving the selection, the edges arriving at it, and one read of the
// files those edges live in. The first version did one query per symbol per
// direction, and measurement on a synthetic thousand-file repository found
// exactly what that costs -- retrieval scaling with the size of the graph,
// which is the one thing a dispatch-path read may not do.
func (ix *Index) expandDurable(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
	out *Neighborhood, req RetrieveRequest,
) error {
	fromKeys := make([]string, 0, len(out.Symbols))
	toKeys := make([]string, 0, len(out.Symbols)*3)
	files := map[string]bool{}
	seenKey := map[string]bool{}
	for _, selected := range out.Symbols {
		sym := selected.Symbol
		files[sym.File] = true
		fromKeys = append(fromKeys, sym.ID)
		for _, key := range incomingKeys(sym) {
			if key == "" || seenKey[key] {
				continue
			}
			seenKey[key] = true
			toKeys = append(toKeys, key)
		}
	}

	outgoing, err := ix.repo.ListCodeGraphEdgesFromKeys(ctx, projectID, repoID, generation, fromKeys)
	if err != nil {
		return err
	}
	incoming, err := ix.repo.ListCodeGraphEdgesToKeys(ctx, projectID, repoID, generation, toKeys)
	if err != nil {
		return err
	}
	out.ConsideredEdges += len(outgoing) + len(incoming)

	tables := map[string]bool{}
	perSymbol := map[string]int{}
	for _, edge := range outgoing {
		if perSymbol[edge.FromKey] >= edgeFanOut {
			// A symbol that reaches four hundred things does not need four
			// hundred of them named. The cap is on the ANSWER; the edges are
			// all still in the graph and all still counted above.
			out.Truncated = true
			continue
		}
		perSymbol[edge.FromKey]++
		out.Callees = append(out.Callees, Edge{
			Kind: EdgeKind(edge.Kind), From: edge.FromKey, To: edge.ToKey, Line: int(edge.Line),
		})
		if edge.Kind == string(EdgeReadsFrom) || edge.Kind == string(EdgeWritesTo) {
			tables[edge.ToKey] = true
		}
	}

	// Incoming edges are matched by NAME as well as by id: a caller writes
	// `s.Delete`, not the callee's identity, so resolving by name at read time
	// is the only honest way to find it.
	perTarget := map[string]int{}
	var testEdges, routeEdges []store.CodeGraphEdgeRecord
	for _, edge := range incoming {
		if perTarget[edge.ToKey] >= edgeFanOut {
			out.Truncated = true
			continue
		}
		perTarget[edge.ToKey]++
		out.Callers = append(out.Callers, Edge{
			Kind: EdgeKind(edge.Kind), From: edge.FromKey, To: edge.ToKey, Line: int(edge.Line),
		})
		files[edge.Path] = true
		switch edge.Kind {
		case string(EdgeTests):
			testEdges = append(testEdges, edge)
		case string(EdgeRoutesTo):
			routeEdges = append(routeEdges, edge)
		}
	}

	// One read for every file those edges live in, then resolve from memory.
	resolved, err := ix.resolveSources(ctx, projectID, repoID, generation, testEdges, routeEdges)
	if err != nil {
		return err
	}
	out.Tests = pickSources(testEdges, resolved)
	out.Endpoints = pickSources(routeEdges, resolved)

	sortEdges(out.Callers)
	sortEdges(out.Callees)
	if len(out.Callers) > req.MaxEdges {
		out.Callers = out.Callers[:req.MaxEdges]
		out.Truncated = true
	}
	if len(out.Callees) > req.MaxEdges {
		out.Callees = out.Callees[:req.MaxEdges]
		out.Truncated = true
	}
	sortSymbols(out.Tests)
	sortSymbols(out.Endpoints)
	out.Tables = sortedKeys(tables)
	out.Files = sortedKeys(files)
	return nil
}

// resolveSources reads the files a set of edges originate in, once each, and
// returns their declarations by symbol id.
func (ix *Index) resolveSources(
	ctx context.Context, projectID domain.ProjectID, repoID string, generation int64,
	groups ...[]store.CodeGraphEdgeRecord,
) (map[string]Symbol, error) {
	seen := map[string]bool{}
	var paths []string
	for _, group := range groups {
		for _, edge := range group {
			if edge.Path == "" || seen[edge.Path] {
				continue
			}
			seen[edge.Path] = true
			paths = append(paths, edge.Path)
			if len(paths) >= maxResolvedPaths {
				break
			}
		}
	}
	records, err := ix.repo.ListCodeGraphSymbolsForPaths(ctx, projectID, repoID, generation, paths)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Symbol, len(records))
	for _, record := range records {
		out[record.SymbolID] = symbolFromRecord(record)
	}
	return out, nil
}

// pickSources turns edges into the declarations they start at, bounded and
// deduplicated.
func pickSources(edges []store.CodeGraphEdgeRecord, resolved map[string]Symbol) []Symbol {
	seen := map[string]bool{}
	var out []Symbol
	for _, edge := range edges {
		if seen[edge.FromKey] || len(out) >= maxResolvedSymbols {
			continue
		}
		seen[edge.FromKey] = true
		if sym, ok := resolved[edge.FromKey]; ok {
			out = append(out, sym)
		}
	}
	return out
}

// Answer bounds. A symbol exercised by three hundred tests does not need three
// hundred of them named, and reading three hundred files to find that out is
// the cost this cap exists to refuse.
const (
	maxResolvedSymbols = 24
	maxResolvedPaths   = 64
)

// incomingKeys are the names an edge could use to reach a symbol: its id, its
// qualified name, and -- for a method -- its bare name.
func incomingKeys(sym Symbol) []string {
	keys := []string{sym.ID, sym.Name}
	if _, member, ok := strings.Cut(sym.Name, "."); ok && member != "" {
		keys = append(keys, member)
	}
	return keys
}

// Architecture returns the stored structural summary for a repository.
//
// It is a single row read because the summary was computed when the graph was
// last built. Recomputing it per dispatch would put a census of every file on
// the path of every task, which is exactly the cost this phase exists to
// remove.
func (ix *Index) Architecture(
	ctx context.Context, projectID domain.ProjectID, repoID string,
) (rendered string, arch Architecture, ok bool, err error) {
	state, found, err := ix.repo.GetCodeGraphState(ctx, projectID, repoID)
	if err != nil || !found || !state.Indexed() || state.Architecture == "" {
		return "", Architecture{}, false, err
	}
	if state.ArchitectureJSON != "" {
		if decodeErr := json.Unmarshal([]byte(state.ArchitectureJSON), &arch); decodeErr != nil {
			// The rendered text is what a pack carries and it is still good;
			// a structured form that will not decode is a bug worth reporting
			// and not worth failing a dispatch over.
			if ix.log != nil {
				ix.log.Warn("code graph: stored architecture summary would not decode",
					"project", projectID, "repo", repoID, "err", decodeErr)
			}
		}
	}
	return state.Architecture, arch, true, nil
}

// AnalyzeChanged answers section 25 of the brief: what a TASK's own
// provisional changes look like, without any of it becoming canonical.
//
// A task worktree holds files that are not in the repository yet. Indexing
// them into the canonical graph would be exactly the worktree contamination
// P2-E closed. Indexing them into a second, task-scoped graph would be a
// parallel store with its own lifecycle, its own staleness and its own way to
// leak into a sibling's context.
//
// So neither happens. The changed files are extracted ON DEMAND, in memory,
// bounded by the same limits a pass uses, and the result is handed to the one
// caller that asked. Nothing is written. When the task is abandoned the
// analysis simply never existed; when it integrates, the next canonical
// incremental sync absorbs the same files through the ordinary path.
func (ix *Index) AnalyzeChanged(ctx context.Context, worktreeRoot string, paths []string, req RetrieveRequest) (Neighborhood, error) {
	root, err := CanonicalRoot(worktreeRoot)
	if err != nil {
		return Neighborhood{}, err
	}
	graph := NewGraph(root)
	scanned := 0
	for _, raw := range paths {
		if err := ctx.Err(); err != nil {
			return Neighborhood{}, err
		}
		if scanned >= ix.limits.MaxFiles {
			break
		}
		rel := normalizeRel(raw)
		if rel == "" {
			continue
		}
		extractor, ok := ix.scanner.indexable(rel)
		if !ok {
			continue
		}
		data, ok, err := ix.scanner.readCandidate(root, rel)
		if err != nil {
			return Neighborhood{}, err
		}
		if !ok {
			continue
		}
		scanned++
		extraction, err := extractor.Extract(rel, data)
		if err != nil {
			return Neighborhood{}, err
		}
		graph.Put(FileEntry{
			Path: rel, Hash: hashBytes(data), Language: extractor.Language(),
			Role: ClassifyFile(rel, data), Size: int64(len(data)),
			Symbols: dedupeSymbols(extraction.Symbols), Edges: dedupeEdges(extraction.Edges),
		})
	}
	if len(graph.Files) == 0 {
		return Neighborhood{}, nil
	}
	if len(req.Files) == 0 && len(req.Symbols) == 0 && len(req.Terms) == 0 {
		// With no question asked, the question is "what did this task touch".
		req.Files = graph.Paths()
	}
	return graph.Retrieve(req), nil
}
