package codegraph

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"
	"time"
)

// graphVersion is the on-disk schema version. A persisted graph written by a
// different version is discarded rather than migrated: re-indexing is cheap
// and a half-understood graph is worse than none.
const graphVersion = 1

// SymbolKind classifies a declaration. The set is deliberately small and
// language-neutral so adapters for other tools can map onto it.
type SymbolKind string

// The symbol kinds the native indexer emits.
const (
	// SymbolFunction is a free function.
	SymbolFunction SymbolKind = "function"
	// SymbolMethod is a function bound to a receiver, class, or type.
	SymbolMethod SymbolKind = "method"
	// SymbolType is a type, struct, interface, class, enum, or type alias.
	SymbolType SymbolKind = "type"
	// SymbolConstant is a package- or module-level constant.
	SymbolConstant SymbolKind = "constant"
	// SymbolVariable is a package- or module-level variable.
	SymbolVariable SymbolKind = "variable"
)

// EdgeKind classifies a relation in the graph.
type EdgeKind string

// The edge kinds the native indexer emits.
const (
	// EdgeImport goes from a file path to an imported module/package path.
	EdgeImport EdgeKind = "import"
	// EdgeCall goes from a symbol ID to a callee expression as written at the
	// call site ("fmt.Println", "helper", "rcv.Method"). It is intentionally
	// unresolved: resolving a callee needs type information the native
	// indexer does not build. Callers resolve by name through Query.
	EdgeCall EdgeKind = "call"
)

// Symbol is one declaration found in a file.
type Symbol struct {
	// ID is "<file>#<kind>:<name>" and is stable for as long as the symbol
	// keeps its file, kind, and name.
	ID string `json:"id"`
	// Name is the declared name. Methods are recorded as "Receiver.Method".
	Name string `json:"name"`
	// Kind classifies the declaration.
	Kind SymbolKind `json:"kind"`
	// File is the project-relative, slash-separated path it was declared in.
	File string `json:"file"`
	// Line is the 1-based line of the declaration.
	Line int `json:"line"`
	// Hash is the content hash of the symbol's own source text, so a caller
	// can tell an edited symbol from an untouched one inside a changed file.
	Hash string `json:"hash"`
}

// Edge is one directed relation. Import edges start at a file path, call
// edges at a symbol ID.
type Edge struct {
	// Kind classifies the relation.
	Kind EdgeKind `json:"kind"`
	// From is the source: a file path for imports, a symbol ID for calls.
	From string `json:"from"`
	// To is the target: an imported module path, or a callee expression.
	To string `json:"to"`
}

// FileEntry is everything the graph knows about one file. Symbols and the
// edges that originate in the file are stored together so dropping a deleted
// file drops its edges with it — there is no second index to keep in sync.
type FileEntry struct {
	// Path is the project-relative, slash-separated path.
	Path string `json:"path"`
	// Hash is the content hash of the whole file. It is the gate that lets an
	// incremental update skip an unchanged file without parsing it.
	Hash string `json:"hash"`
	// Language names the extractor that produced this entry.
	Language string `json:"language"`
	// Size is the file's size in bytes at index time.
	Size int64 `json:"size"`
	// Symbols are the declarations found in the file.
	Symbols []Symbol `json:"symbols,omitempty"`
	// Edges are the relations originating in this file.
	Edges []Edge `json:"edges,omitempty"`
}

// Graph is one project's persisted code graph.
type Graph struct {
	// Version is the on-disk schema version.
	Version int `json:"version"`
	// ProjectRoot is the canonical absolute root this graph describes. It is
	// stored so a load can prove the file it read belongs to the project that
	// asked for it.
	ProjectRoot string `json:"projectRoot"`
	// ProjectKey is the storage key derived from ProjectRoot.
	ProjectKey string `json:"projectKey"`
	// IndexedAt is when the graph was last written.
	IndexedAt time.Time `json:"indexedAt"`
	// Files maps project-relative path to its entry.
	Files map[string]FileEntry `json:"files"`
}

// NewGraph returns an empty graph for a canonical project root.
func NewGraph(projectRoot string) *Graph {
	return &Graph{
		Version:     graphVersion,
		ProjectRoot: projectRoot,
		ProjectKey:  ProjectKey(projectRoot),
		Files:       map[string]FileEntry{},
	}
}

// Put stores (or replaces) one file entry.
func (g *Graph) Put(entry FileEntry) {
	if g.Files == nil {
		g.Files = map[string]FileEntry{}
	}
	g.Files[entry.Path] = entry
}

// Lookup returns the entry for a project-relative path.
func (g *Graph) Lookup(rel string) (FileEntry, bool) {
	entry, ok := g.Files[rel]
	return entry, ok
}

// Remove drops a file entry and reports whether it existed.
func (g *Graph) Remove(rel string) bool {
	if _, ok := g.Files[rel]; !ok {
		return false
	}
	delete(g.Files, rel)
	return true
}

// Rekey moves an entry from one path to another without re-analyzing it,
// rewriting the path-derived symbol IDs and the edges that referenced them.
// It is what makes a pure rename (identical bytes at a new path) cost no
// parsing. It reports whether the old entry existed.
func (g *Graph) Rekey(oldRel, newRel string) bool {
	entry, ok := g.Files[oldRel]
	if !ok {
		return false
	}
	delete(g.Files, oldRel)

	rewrites := map[string]string{oldRel: newRel}
	symbols := make([]Symbol, 0, len(entry.Symbols))
	for _, sym := range entry.Symbols {
		moved := sym
		moved.File = newRel
		moved.ID = symbolID(newRel, sym.Kind, sym.Name)
		rewrites[sym.ID] = moved.ID
		symbols = append(symbols, moved)
	}
	edges := make([]Edge, 0, len(entry.Edges))
	for _, edge := range entry.Edges {
		moved := edge
		if to, ok := rewrites[edge.From]; ok {
			moved.From = to
		}
		if to, ok := rewrites[edge.To]; ok {
			moved.To = to
		}
		edges = append(edges, moved)
	}
	entry.Path = newRel
	entry.Symbols = symbols
	entry.Edges = edges
	g.Files[newRel] = entry
	return true
}

// Counts returns the number of symbols and edges across the whole graph.
func (g *Graph) Counts() (symbols, edges int) {
	for _, entry := range g.Files {
		symbols += len(entry.Symbols)
		edges += len(entry.Edges)
	}
	return symbols, edges
}

// Paths returns every indexed path, sorted.
func (g *Graph) Paths() []string {
	out := make([]string, 0, len(g.Files))
	for rel := range g.Files {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// QueryResult is what a provider returns for a QueryRequest.
type QueryResult struct {
	// ProjectRoot is the canonical root that was queried.
	ProjectRoot string `json:"projectRoot"`
	// Files are the matched file entries, sorted by path.
	Files []FileEntry `json:"files,omitempty"`
	// Symbols are the matched symbols, sorted by ID.
	Symbols []Symbol `json:"symbols,omitempty"`
	// Outgoing are edges leaving the matched files and symbols.
	Outgoing []Edge `json:"outgoing,omitempty"`
	// Incoming are edges pointing at a matched file path, symbol ID, or
	// symbol name. Call targets are unresolved names, so this is a
	// by-name match, not a type-resolved one.
	Incoming []Edge `json:"incoming,omitempty"`
}

// Query answers a request against an in-memory graph. It never mutates.
func (g *Graph) Query(req QueryRequest) (QueryResult, error) {
	symbolTerm := strings.TrimSpace(req.Symbol)
	fileTerm := strings.TrimSpace(req.File)
	if symbolTerm == "" && fileTerm == "" {
		return QueryResult{}, ErrEmptyQuery
	}

	result := QueryResult{ProjectRoot: g.ProjectRoot}
	sources := map[string]bool{}
	targets := map[string]bool{}

	for _, rel := range g.Paths() {
		entry := g.Files[rel]
		fileMatched := fileTerm != "" && matchesFile(rel, fileTerm)
		if fileMatched {
			result.Files = append(result.Files, entry)
			sources[rel] = true
			targets[rel] = true
		}
		if symbolTerm == "" {
			continue
		}
		for _, sym := range entry.Symbols {
			if !matchesSymbol(sym, symbolTerm) {
				continue
			}
			if fileTerm != "" && !fileMatched {
				continue
			}
			result.Symbols = append(result.Symbols, sym)
			sources[sym.ID] = true
			targets[sym.ID] = true
			targets[sym.Name] = true
			if _, method, ok := strings.Cut(sym.Name, "."); ok {
				targets[method] = true
			}
		}
	}

	for _, rel := range g.Paths() {
		for _, edge := range g.Files[rel].Edges {
			if sources[edge.From] {
				result.Outgoing = append(result.Outgoing, edge)
			}
			if targets[edge.To] {
				result.Incoming = append(result.Incoming, edge)
			}
		}
	}

	sortEdges(result.Outgoing)
	sortEdges(result.Incoming)
	sort.Slice(result.Symbols, func(i, j int) bool { return result.Symbols[i].ID < result.Symbols[j].ID })
	if req.Limit > 0 {
		if len(result.Files) > req.Limit {
			result.Files = result.Files[:req.Limit]
		}
		if len(result.Symbols) > req.Limit {
			result.Symbols = result.Symbols[:req.Limit]
		}
	}
	return result, nil
}

func matchesSymbol(sym Symbol, term string) bool {
	if sym.ID == term || sym.Name == term {
		return true
	}
	_, method, ok := strings.Cut(sym.Name, ".")
	return ok && method == term
}

func matchesFile(rel, term string) bool {
	term = strings.TrimPrefix(strings.ReplaceAll(term, "\\", "/"), "./")
	if rel == term {
		return true
	}
	if strings.HasSuffix(rel, "/"+term) {
		return true
	}
	return path.Base(rel) == term
}

func sortEdges(edges []Edge) {
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})
}

// symbolID builds the stable identifier for a declaration.
func symbolID(file string, kind SymbolKind, name string) string {
	return file + "#" + string(kind) + ":" + name
}

// hashBytes is the one content hash used for both files and symbols. It is
// truncated to 128 bits: long enough that a collision is not a practical
// concern for change detection, short enough to keep the persisted graph
// readable.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}
