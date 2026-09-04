package codegraph

import (
	"path/filepath"
	"strings"
)

// Extraction is what an extractor found in one file.
type Extraction struct {
	// Symbols are the declarations in the file.
	Symbols []Symbol
	// Edges are the relations originating in the file.
	Edges []Edge
}

// Extractor turns one file's bytes into symbols and edges. Adding a language
// means adding an Extractor and registering it — nothing in the indexer, the
// store, or the provider boundary changes.
type Extractor interface {
	// Language names the extractor, and is recorded on every entry it
	// produces.
	Language() string
	// Extensions are the lowercase file extensions (with the dot) this
	// extractor claims.
	Extensions() []string
	// Extract analyzes src, which is the full content of the file at the
	// project-relative path relPath. An extractor should return what it could
	// understand rather than failing on a file that does not fully parse:
	// a graph missing one malformed file is more useful than no graph.
	Extract(relPath string, src []byte) (Extraction, error)
}

// DefaultExtractors is the set the native indexer uses when none is supplied.
//
// The languages are the ones AO's own repositories are written in, which is
// the priority the brief sets: Go (backend, CLI, daemon), TypeScript and
// JavaScript (the Electron supervisor and its renderer), Python (the scripts
// and the tooling around them), and SQL (the schema and the named queries that
// are the persistence half of the architecture).
//
// Go is parsed with the standard library's AST. The others are read by the
// lexer-backed structural extractors in this package: no TypeScript or Python
// parser is available to this binary and none is vendored, so what those
// extractors do is tokenize first -- masking comments, strings and docstrings
// so they cannot be mistaken for code -- and then walk the remaining structure
// by brace depth or indentation. That is materially different from matching
// patterns against raw lines, and it is what makes `Class.method` qualification
// and scoped call attribution possible; it is also, honestly, less than an AST
// would give, and each extractor's doc comment says exactly where it stops.
func DefaultExtractors() []Extractor {
	return []Extractor{
		goExtractor{},
		tsExtractor{language: "typescript", extensions: []string{".ts", ".tsx", ".mts", ".cts"}},
		tsExtractor{language: "javascript", extensions: []string{".js", ".jsx", ".mjs", ".cjs"}},
		pyExtractor{},
		sqlExtractor{},
	}
}

// extractorSet indexes extractors by extension for O(1) dispatch.
type extractorSet struct {
	byExt map[string]Extractor
}

func newExtractorSet(extractors []Extractor) extractorSet {
	set := extractorSet{byExt: map[string]Extractor{}}
	for _, ex := range extractors {
		for _, ext := range ex.Extensions() {
			set.byExt[strings.ToLower(ext)] = ex
		}
	}
	return set
}

// find returns the extractor claiming a path's extension, if any.
func (s extractorSet) find(relPath string) (Extractor, bool) {
	ex, ok := s.byExt[strings.ToLower(filepath.Ext(relPath))]
	return ex, ok
}
