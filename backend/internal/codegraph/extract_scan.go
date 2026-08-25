package codegraph

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
)

// scanSymbolPattern maps one declaration pattern to the kind it declares. The
// first capture group is the declared name.
type scanSymbolPattern struct {
	re   *regexp.Regexp
	kind SymbolKind
}

// scanPatterns is one language's declaration and import vocabulary.
type scanPatterns struct {
	// imports capture a module specifier in their first non-empty group.
	imports []*regexp.Regexp
	// symbols are tried in order; the first match wins for a line, so more
	// specific patterns must come first.
	symbols []scanSymbolPattern
}

// scanExtractor is a line-oriented declaration scanner for languages AO has
// no parser for in the standard library. It records symbols and import
// (dependency) edges only: call edges need real name resolution, and an edge
// guessed from a regex would be worse than an absent one. Go, where a real
// AST is available, gets call edges from goExtractor.
type scanExtractor struct {
	language   string
	extensions []string
	patterns   scanPatterns
}

func newScanExtractor(language string, extensions []string, patterns scanPatterns) scanExtractor {
	return scanExtractor{language: language, extensions: extensions, patterns: patterns}
}

// Language implements Extractor.
func (s scanExtractor) Language() string { return s.language }

// Extensions implements Extractor.
func (s scanExtractor) Extensions() []string { return s.extensions }

// Extract implements Extractor. A scanned symbol's hash covers its
// declaration line, not its body: it is enough to tell "this declaration
// changed" from "something else in the file changed", which is what the hash
// is for.
func (s scanExtractor) Extract(relPath string, src []byte) (Extraction, error) {
	out := Extraction{}
	seenImports := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 64*1024), maxScanLineBytes)

	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, re := range s.patterns.imports {
			for _, match := range re.FindAllStringSubmatch(line, -1) {
				target := firstGroup(match)
				if target == "" || seenImports[target] {
					continue
				}
				seenImports[target] = true
				out.Edges = append(out.Edges, Edge{Kind: EdgeImport, From: relPath, To: target})
			}
		}
		for _, pattern := range s.patterns.symbols {
			match := pattern.re.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			name := firstGroup(match)
			if name == "" {
				continue
			}
			out.Symbols = append(out.Symbols, Symbol{
				ID:   symbolID(relPath, pattern.kind, name),
				Name: name,
				Kind: pattern.kind,
				File: relPath,
				Line: lineNo,
				Hash: hashBytes([]byte(strings.TrimSpace(line))),
			})
			break
		}
	}
	// A line longer than the scan buffer (a bundled or minified file) ends the
	// scan early. Whatever was found before it still stands, so the partial
	// extraction is returned rather than discarded.
	_ = scanner.Err()
	return out, nil
}

// maxScanLineBytes caps one scanned line so a minified bundle cannot make the
// scanner allocate without bound.
const maxScanLineBytes = 1 << 20

func firstGroup(match []string) string {
	for _, group := range match[1:] {
		if trimmed := strings.TrimSpace(group); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// tsPatterns covers TypeScript and JavaScript. Ordering matters: an arrow
// function assigned to a const is a function, not a constant, so its pattern
// is tried before the plain const one.
var tsPatterns = scanPatterns{
	imports: []*regexp.Regexp{
		regexp.MustCompile(`^\s*(?:import|export)\s[^'"]*from\s*['"]([^'"]+)['"]`),
		regexp.MustCompile(`^\s*import\s*['"]([^'"]+)['"]`),
		regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`),
	},
	symbols: []scanSymbolPattern{
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`), SymbolFunction},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), SymbolType},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?(?:interface|type|enum)\s+([A-Za-z_$][\w$]*)`), SymbolType},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=>`), SymbolFunction},
		{regexp.MustCompile(`^\s*(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*[:=]`), SymbolConstant},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:let|var)\s+([A-Za-z_$][\w$]*)\s*[:=]`), SymbolVariable},
	},
}

// pyPatterns covers Python. Indentation is the only cue available for
// telling a method from a free function, and it is the right one: a def at
// column zero is module-level.
var pyPatterns = scanPatterns{
	imports: []*regexp.Regexp{
		regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\b`),
		regexp.MustCompile(`^\s*import\s+([\w.]+)`),
	},
	symbols: []scanSymbolPattern{
		{regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_]\w*)`), SymbolFunction},
		{regexp.MustCompile(`^\s+(?:async\s+)?def\s+([A-Za-z_]\w*)`), SymbolMethod},
		{regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`), SymbolType},
		{regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)\s*(?::[^=]+)?=`), SymbolConstant},
	},
}
