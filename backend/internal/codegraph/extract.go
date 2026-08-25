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

// DefaultExtractors is the set the native indexer uses when none is supplied:
// a real Go AST parse, plus a declaration scanner covering the languages the
// AO repo itself contains beyond Go.
func DefaultExtractors() []Extractor {
	return []Extractor{
		goExtractor{},
		newScanExtractor("typescript", []string{".ts", ".tsx", ".mts", ".cts"}, tsPatterns),
		newScanExtractor("javascript", []string{".js", ".jsx", ".mjs", ".cjs"}, tsPatterns),
		newScanExtractor("python", []string{".py", ".pyi"}, pyPatterns),
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
