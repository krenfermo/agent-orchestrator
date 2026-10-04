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
//
// Version 2 added the structural vocabulary this package needs to be useful to
// an agent rather than only to a file browser: symbol signatures, doc-derived
// summaries, and the relation kinds below.
const graphVersion = 2

// SymbolKind classifies a declaration. The set is deliberately small and
// language-neutral so adapters for other tools can map onto it.
type SymbolKind string

// The symbol kinds the native indexer emits.
//
// The set stays small and language-neutral. Kinds beyond the five declaration
// kinds exist because an agent asked to "add export permissions to Supervisor"
// needs to reach an endpoint, a table or a configuration key by name, and a
// graph that models those only as "a line inside some function" cannot be
// asked about them.
const (
	// SymbolFunction is a free function.
	SymbolFunction SymbolKind = "function"
	// SymbolMethod is a function bound to a receiver, class, or type.
	SymbolMethod SymbolKind = "method"
	// SymbolType is a type, struct, class, enum, or type alias.
	SymbolType SymbolKind = "type"
	// SymbolInterface is an interface or protocol: a contract other types
	// satisfy. It is split out from SymbolType because "what implements this"
	// is a question only interfaces answer.
	SymbolInterface SymbolKind = "interface"
	// SymbolConstant is a package- or module-level constant.
	SymbolConstant SymbolKind = "constant"
	// SymbolVariable is a package- or module-level variable.
	SymbolVariable SymbolKind = "variable"
	// SymbolTest is a test function. Tests are their own kind because they are
	// the cheapest evidence an agent has that a change is correct, and
	// "which tests cover this" has to be answerable without pattern-matching
	// names at query time.
	SymbolTest SymbolKind = "test"
	// SymbolEndpoint is an HTTP route as registered in source: its name is
	// "METHOD /pattern".
	SymbolEndpoint SymbolKind = "endpoint"
	// SymbolTable is a database table declared by a migration.
	SymbolTable SymbolKind = "table"
	// SymbolQuery is a named SQL statement (a sqlc `-- name:` block).
	SymbolQuery SymbolKind = "query"
	// SymbolConfig is a configuration surface: an environment variable the
	// code reads. Only the KEY is ever recorded; see extract_config.go.
	SymbolConfig SymbolKind = "config"
)

// EdgeKind classifies a relation in the graph.
type EdgeKind string

// The edge kinds the native indexer emits.
//
// Every one of them is statically provable from a single file's own text. The
// rule this package will not break is the brief's: an edge AO cannot
// demonstrate is worse than an absent edge, because a Reviewer will believe
// it. So there is no "probably calls", no type inference across files, and no
// heuristic that guesses an implementation from a name alone.
const (
	// EdgeImport goes from a file path to an imported module/package path.
	EdgeImport EdgeKind = "import"
	// EdgeCall goes from a symbol ID to a callee expression as written at the
	// call site ("fmt.Println", "helper", "rcv.Method"). It is intentionally
	// unresolved: resolving a callee needs type information the native
	// indexer does not build. Callers resolve by name through Query.
	EdgeCall EdgeKind = "call"
	// EdgeEmbeds goes from a type's symbol ID to an embedded or extended type
	// name: a Go struct/interface embedding, a TypeScript `extends`, a Python
	// base class. It is the "extends/embeds" relation, and it is exact --
	// the name is written in the declaration.
	EdgeEmbeds EdgeKind = "embeds"
	// EdgeImplements goes from a type's symbol ID to an interface name it is
	// PROVEN to satisfy: in Go, by a compile-time assertion
	// (`var _ Iface = (*T)(nil)`), in TypeScript by an `implements` clause.
	// Structural satisfaction that the compiler infers is NOT recorded --
	// proving it needs a type checker this indexer does not build.
	EdgeImplements EdgeKind = "implements"
	// EdgeReferences goes from a symbol ID to a type name that appears in its
	// public signature. It is what makes "what does this function touch"
	// answerable without reading the body.
	EdgeReferences EdgeKind = "references"
	// EdgeTests goes from a test symbol's ID to the name it exercises. It is
	// recorded only from evidence: a Go `TestFoo` that actually calls `Foo`,
	// a `describe("Foo")` in a spec file beside `Foo`'s module.
	EdgeTests EdgeKind = "tests"
	// EdgeRoutesTo goes from an endpoint symbol's ID to the handler
	// expression registered for it at the call site.
	EdgeRoutesTo EdgeKind = "routes_to"
	// EdgeReadsFrom goes from a query symbol's ID to a table it selects from.
	EdgeReadsFrom EdgeKind = "reads_from"
	// EdgeWritesTo goes from a query symbol's ID to a table it inserts into,
	// updates, or deletes from.
	EdgeWritesTo EdgeKind = "writes_to"
	// EdgeConfigures goes from a symbol ID to a configuration key it reads.
	// The key is recorded; the value never is.
	EdgeConfigures EdgeKind = "configures"
	// EdgeDefines goes from a file path to a symbol ID declared in it. It is
	// DERIVED at query time from the file entry rather than stored, so a
	// deleted file can never leave a dangling defines edge behind.
	EdgeDefines EdgeKind = "defines"
)

// Symbol is one declaration found in a file.
//
// Identity is (file, kind, qualified name) and nothing else -- see ID. Line
// and EndLine are deliberately NOT part of it: they are observations of where
// the symbol happened to sit at index time, and treating a moved declaration
// as a new one would make every reformat look like a rewrite of the module.
type Symbol struct {
	// ID is "<file>#<kind>:<name>" and is stable for as long as the symbol
	// keeps its file, kind, and qualified name. The repository and the
	// language are not encoded in it because a Graph belongs to exactly one
	// repository and a path belongs to exactly one extractor: adding them
	// would repeat, on every symbol, what the containing graph already proves.
	ID string `json:"id"`
	// Name is the declared name, qualified by its owner where it has one:
	// "Receiver.Method" in Go, "Class.method" in TypeScript and Python. The
	// qualification is what keeps two `Get` methods on different types from
	// colliding on one identity.
	Name string `json:"name"`
	// Kind classifies the declaration.
	Kind SymbolKind `json:"kind"`
	// File is the project-relative, slash-separated path it was declared in.
	File string `json:"file"`
	// Line is the 1-based line of the declaration.
	Line int `json:"line"`
	// EndLine is the 1-based last line the declaration spans, or 0 when the
	// extractor cannot determine it. With Line it is the span a caller can
	// read to see the symbol itself instead of the whole file.
	EndLine int `json:"endLine,omitempty"`
	// Signature is the symbol's public contract as written: the parameter and
	// result list of a function, the underlying form of a type. It is the
	// single most useful thing an agent can be told about a symbol it has not
	// read, and it is verbatim rather than summarised.
	Signature string `json:"signature,omitempty"`
	// Doc is the first sentence of the declaration's documentation comment,
	// bounded. It is copied, never generated: see Summary.
	Doc string `json:"doc,omitempty"`
	// Summary is the bounded, deterministic description a context pack shows.
	// It is assembled from Kind, Signature and Doc by summarize() -- there is
	// no model in the loop, so indexing a project never needs a provider, and
	// two indexes of the same source produce the same bytes.
	Summary string `json:"summary,omitempty"`
	// SummarySource names what produced Summary, so a reader can tell a
	// mechanical description from a generated one. The native indexer only
	// ever writes SummaryStatic.
	SummarySource SummarySource `json:"summarySource,omitempty"`
	// Exported reports whether the symbol is part of its module's public
	// surface (a capitalised Go identifier, an exported TS declaration, a
	// Python name that does not start with an underscore).
	Exported bool `json:"exported,omitempty"`
	// Hash is the content hash of the symbol's own source text, so a caller
	// can tell an edited symbol from an untouched one inside a changed file.
	Hash string `json:"hash"`
}

// SummarySource names what produced a symbol summary. It exists so an
// AI-generated summary, if one is ever added, can never be mistaken for an
// observed fact -- the brief's rule that provider provenance stays explicit.
type SummarySource string

// Summary sources.
const (
	// SummaryStatic is a summary derived mechanically from the declaration.
	// It is the only kind the native indexer produces.
	SummaryStatic SummarySource = "static"
)

// maxSymbolSummary bounds one symbol's summary. A context pack may carry
// dozens of them, so the cap is what keeps "add graph evidence" from meaning
// "spend the budget on prose".
const maxSymbolSummary = 240

// maxSymbolDoc bounds the copied documentation sentence.
const maxSymbolDoc = 200

// maxSignature bounds a rendered signature.
const maxSignature = 200

// Edge is one directed relation. Import edges start at a file path, every
// other kind starts at a symbol ID.
//
// The target is deliberately a NAME rather than a resolved identity. Resolving
// "service.Delete" to a declaration needs type information across files that
// this indexer does not build, and a resolved-looking edge that guessed wrong
// is the one failure mode a Reviewer cannot detect. Query resolves by name at
// read time, where the ambiguity is visible.
type Edge struct {
	// Kind classifies the relation.
	Kind EdgeKind `json:"kind"`
	// From is the source: a file path for imports, a symbol ID otherwise.
	From string `json:"from"`
	// To is the target: an imported module path, a callee expression, a type
	// name, a table name, or a configuration key.
	To string `json:"to"`
	// Line is the 1-based line the relation was observed on, which is this
	// edge's provenance: the file it belongs to is the entry that holds it,
	// and the commit is the graph's. Zero means the extractor could not place
	// it more precisely than the file.
	Line int `json:"line,omitempty"`
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
	// Role is what kind of file this is -- source, test, migration, query, or
	// generated. It is what lets a retrieval ask for "the tests covering this"
	// or exclude generated noise without re-deciding the classification at
	// every call site.
	Role FileRole `json:"role,omitempty"`
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
	// IndexedCommit is the commit the graph was last brought up to date at,
	// empty when the caller did not supply one. Together with each entry's
	// content hash it is the graph's provenance: every fact in it was observed
	// in this checkout, at this revision.
	IndexedCommit string `json:"indexedCommit,omitempty"`
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
			// A symbol is addressable by its ID and by its name in BOTH
			// directions. Most edges start at an ID, but the ones this
			// indexer can only prove by name -- a compile-time interface
			// assertion about a type from another package -- start at the
			// name as written. Resolving names on the source side too is what
			// keeps those edges reachable instead of orphaned.
			sources[sym.ID] = true
			sources[sym.Name] = true
			targets[sym.ID] = true
			targets[sym.Name] = true
			if _, method, ok := strings.Cut(sym.Name, "."); ok {
				sources[method] = true
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
