package codegraph

import (
	"regexp"
	"sort"
	"strings"
)

// extract_py.go — the Python extractor.
//
// Like TypeScript, Python has no parser available to this binary, so this is a
// lexer plus a structural walk rather than an AST reader. Python's structure is
// carried by indentation, which makes the walk both simpler and stricter than
// the brace-based one: a `def` at column zero is a module function, a `def`
// indented inside a `class` block is that class's method, and there is no
// ambiguity to resolve.
//
// The lexing that matters here is the triple-quoted string. A docstring
// containing `def parse(...)` is documentation, and a line scanner that has not
// tracked the quote state will happily record it as a declaration -- which is
// precisely how a repository of well-documented modules ends up with a graph
// full of symbols that do not exist. Masking first is what prevents that, and
// it is also what lets the first masked literal after a `def` be recognised as
// that function's docstring.

// pyExtractor reads Python.
type pyExtractor struct{}

// Language implements Extractor.
func (pyExtractor) Language() string { return "python" }

// Extensions implements Extractor.
func (pyExtractor) Extensions() []string { return []string{".py", ".pyi"} }

// Extract implements Extractor.
func (pyExtractor) Extract(relPath string, src []byte) (Extraction, error) {
	w := &pyWalk{
		rel:      relPath,
		lines:    maskPython(string(src)),
		isTest:   ClassifyFile(relPath, src) == RoleTest,
		config:   map[string]bool{},
		callsFor: map[string][]string{},
	}
	w.run()
	return Extraction{Symbols: w.symbols, Edges: w.edges}, nil
}

// pyLine is one masked Python line.
type pyLine struct {
	Number int
	// Code has string bodies and comments blanked, columns preserved.
	Code string
	// Raw is the original line.
	Raw string
	// Strings are the literal values masked out of it, in order.
	Strings []string
	// Indent is the count of leading spaces (a tab counts as one level of
	// eight, matching the interpreter's own tabstop).
	Indent int
	// Blank reports a line with no code on it, which never closes a block.
	Blank bool
}

// maskPython tokenizes a module into masked lines.
func maskPython(src string) []pyLine {
	var out []pyLine
	var code, raw, literal strings.Builder
	var strs []string
	lineNo := 1
	// triple is the open triple-quote delimiter, empty when not inside one.
	triple := ""
	quote := byte(0)
	inComment := false

	flush := func() {
		text := code.String()
		out = append(out, pyLine{
			Number: lineNo, Code: text, Raw: raw.String(), Strings: strs,
			Indent: pyIndent(raw.String()), Blank: strings.TrimSpace(text) == "",
		})
		code.Reset()
		raw.Reset()
		strs = nil
		lineNo++
		inComment = false
	}

	blank := func(n int) {
		for i := 0; i < n; i++ {
			code.WriteByte(' ')
		}
	}

	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			if triple != "" {
				literal.WriteByte('\n')
			}
			flush()
			continue
		}
		raw.WriteByte(c)

		switch {
		case inComment:
			blank(1)
		case triple != "":
			if strings.HasPrefix(src[i:], triple) {
				strs = append(strs, literal.String())
				literal.Reset()
				blank(len(triple))
				for j := 1; j < len(triple); j++ {
					raw.WriteByte(src[i+j])
				}
				i += len(triple) - 1
				triple = ""
				continue
			}
			literal.WriteByte(c)
			blank(1)
		case quote != 0:
			switch {
			case c == '\\' && i+1 < len(src) && src[i+1] != '\n':
				literal.WriteByte(src[i+1])
				raw.WriteByte(src[i+1])
				blank(2)
				i++
			case c == quote:
				quote = 0
				strs = append(strs, literal.String())
				literal.Reset()
				blank(1)
			default:
				literal.WriteByte(c)
				blank(1)
			}
		case c == '#':
			inComment = true
			blank(1)
		case c == '"' || c == '\'':
			if strings.HasPrefix(src[i:], strings.Repeat(string(c), 3)) {
				triple = strings.Repeat(string(c), 3)
				literal.Reset()
				blank(3)
				raw.WriteByte(src[i+1])
				raw.WriteByte(src[i+2])
				i += 2
				continue
			}
			quote = c
			literal.Reset()
			blank(1)
		default:
			code.WriteByte(c)
		}
	}
	if raw.Len() > 0 || code.Len() > 0 || len(strs) > 0 {
		flush()
	}
	return out
}

// pyIndent measures a line's indentation, counting a tab as eight columns the
// way CPython's own tokenizer does.
func pyIndent(raw string) int {
	n := 0
	for _, r := range raw {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 8
		default:
			return n
		}
	}
	return n
}

// pyWalk accumulates one module's extraction.
type pyWalk struct {
	rel    string
	lines  []pyLine
	isTest bool

	symbols  []Symbol
	edges    []Edge
	config   map[string]bool
	callsFor map[string][]string

	scopes []pyScope
	// awaiting is the symbol whose docstring the next literal line belongs to.
	awaiting string
	awaitIdx int
}

// pyScope is one open indented block.
type pyScope struct {
	kind     string // "class" or "function"
	name     string
	symbolID string
	indent   int
}

var (
	pyFromImport = regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\b`)
	pyImport     = regexp.MustCompile(`^\s*import\s+([\w.]+)`)
	pyDef        = regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	pyClass      = regexp.MustCompile(`^(\s*)class\s+([A-Za-z_]\w*)\s*(?:\(([^)]*)\))?`)
	pyConst      = regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)\s*(?::[^=]+)?=`)
	pyModuleVar  = regexp.MustCompile(`^([a-z_]\w*)\s*(?::[^=]+)?=[^=]`)
	pyCall       = regexp.MustCompile(`([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)\s*\(`)
	pyEnvCall    = regexp.MustCompile(`\bos\.(?:getenv|environ\.get)\s*\(`)
	pyEnvIndex   = regexp.MustCompile(`\bos\.environ\s*\[`)
	pyParams     = regexp.MustCompile(`\((.*)`)
	pyKeywords   = map[string]bool{
		"if": true, "for": true, "while": true, "with": true, "print": false,
		"return": true, "elif": true, "except": true, "assert": true, "lambda": true,
	}
)

func (w *pyWalk) run() {
	for _, line := range w.lines {
		if line.Blank {
			w.docstring(line)
			continue
		}
		w.closeScopes(line.Indent)
		w.lineEdges(line)
		if !w.declaration(line) {
			w.docstring(line)
		}
	}
	w.finish()
}

func (w *pyWalk) closeScopes(indent int) {
	for len(w.scopes) > 0 && indent <= w.scopes[len(w.scopes)-1].indent {
		w.scopes = w.scopes[:len(w.scopes)-1]
	}
}

func (w *pyWalk) currentClass() (pyScope, bool) {
	for i := len(w.scopes) - 1; i >= 0; i-- {
		if w.scopes[i].kind == "class" {
			return w.scopes[i], true
		}
	}
	return pyScope{}, false
}

func (w *pyWalk) currentSymbol() (pyScope, bool) {
	for i := len(w.scopes) - 1; i >= 0; i-- {
		if w.scopes[i].kind == "function" {
			return w.scopes[i], true
		}
	}
	return pyScope{}, false
}

func (w *pyWalk) lineEdges(line pyLine) {
	if m := pyFromImport.FindStringSubmatch(line.Code); m != nil {
		w.edges = append(w.edges, Edge{Kind: EdgeImport, From: w.rel, To: m[1], Line: line.Number})
	} else if m := pyImport.FindStringSubmatch(line.Code); m != nil {
		w.edges = append(w.edges, Edge{Kind: EdgeImport, From: w.rel, To: m[1], Line: line.Number})
	}

	if (pyEnvCall.MatchString(line.Code) || pyEnvIndex.MatchString(line.Code)) && len(line.Strings) > 0 {
		w.configSymbol(strings.TrimSpace(line.Strings[0]), line.Number)
	}

	if owner, ok := w.currentSymbol(); ok {
		for _, match := range pyCall.FindAllStringSubmatch(line.Code, -1) {
			if pyKeywords[match[1]] {
				continue
			}
			w.callsFor[owner.symbolID] = append(w.callsFor[owner.symbolID], match[1])
		}
	}
}

func (w *pyWalk) configSymbol(key string, line int) {
	if key == "" {
		return
	}
	id := symbolID(w.rel, SymbolConfig, key)
	if !w.config[key] {
		w.config[key] = true
		sym := Symbol{
			ID: id, Name: key, Kind: SymbolConfig, File: w.rel, Line: line,
			Exported: true, SummarySource: SummaryStatic, Hash: hashBytes([]byte(key)),
		}
		sym.Summary = summarize(sym, nil)
		w.symbols = append(w.symbols, sym)
	}
	from := w.rel
	if owner, ok := w.currentSymbol(); ok {
		from = owner.symbolID
	}
	w.edges = append(w.edges, Edge{Kind: EdgeConfigures, From: from, To: key, Line: line})
}

// declaration records a def or class, and reports whether the line was one.
func (w *pyWalk) declaration(line pyLine) bool {
	if m := pyClass.FindStringSubmatch(line.Code); m != nil {
		sym := w.declare(SymbolType, m[2], line, "class")
		for _, base := range strings.Split(m[3], ",") {
			base = strings.TrimSpace(base)
			if base == "" || strings.Contains(base, "=") {
				continue
			}
			w.edges = append(w.edges, Edge{Kind: EdgeEmbeds, From: sym.ID, To: base, Line: line.Number})
		}
		w.scopes = append(w.scopes, pyScope{kind: "class", name: m[2], symbolID: sym.ID, indent: line.Indent})
		w.awaiting, w.awaitIdx = sym.ID, len(w.symbols)-1
		return true
	}

	if m := pyDef.FindStringSubmatch(line.Code); m != nil {
		kind := SymbolFunction
		name := m[2]
		if class, inClass := w.currentClass(); inClass {
			kind = SymbolMethod
			name = class.name + "." + m[2]
		}
		if w.isTest && strings.HasPrefix(m[2], "test") {
			kind = SymbolTest
		}
		sym := w.declare(kind, name, line, pySignature(line.Code))
		w.scopes = append(w.scopes, pyScope{kind: "function", name: name, symbolID: sym.ID, indent: line.Indent})
		w.awaiting, w.awaitIdx = sym.ID, len(w.symbols)-1
		return true
	}

	if line.Indent > 0 {
		return false
	}
	if m := pyConst.FindStringSubmatch(line.Code); m != nil {
		w.declare(SymbolConstant, m[1], line, "")
		return true
	}
	if m := pyModuleVar.FindStringSubmatch(line.Code); m != nil {
		w.declare(SymbolVariable, m[1], line, "")
		return true
	}
	return false
}

// docstring attaches the first literal that follows a declaration.
func (w *pyWalk) docstring(line pyLine) {
	if w.awaiting == "" {
		return
	}
	if len(line.Strings) == 0 {
		if !line.Blank {
			w.awaiting = ""
		}
		return
	}
	if strings.TrimSpace(line.Code) != "" {
		// A literal used as a value rather than as a docstring.
		w.awaiting = ""
		return
	}
	sym := &w.symbols[w.awaitIdx]
	sym.Doc = firstSentence(line.Strings[0])
	sym.Summary = summarize(*sym, nil)
	w.awaiting = ""
}

func (w *pyWalk) declare(kind SymbolKind, name string, line pyLine, signature string) Symbol {
	sym := Symbol{
		ID:            symbolID(w.rel, kind, name),
		Name:          name,
		Kind:          kind,
		File:          w.rel,
		Line:          line.Number,
		Signature:     signature,
		Exported:      !strings.HasPrefix(name[strings.LastIndex(name, ".")+1:], "_"),
		SummarySource: SummaryStatic,
		Hash:          hashBytes([]byte(collapseSpaces(line.Raw))),
	}
	sym.Summary = summarize(sym, nil)
	w.symbols = append(w.symbols, sym)
	return sym
}

// finish emits the accumulated call edges, the test coverage they prove, and
// folds observed effects into each summary.
func (w *pyWalk) finish() {
	byID := map[string]int{}
	for i, sym := range w.symbols {
		byID[sym.ID] = i
	}
	ids := make([]string, 0, len(w.callsFor))
	for id := range w.callsFor {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		calls := w.callsFor[id]
		sort.Strings(calls)
		seen := map[string]bool{}
		distinct := calls[:0:0]
		for _, callee := range calls {
			if seen[callee] {
				continue
			}
			seen[callee] = true
			distinct = append(distinct, callee)
			w.edges = append(w.edges, Edge{Kind: EdgeCall, From: id, To: callee})
		}
		idx, known := byID[id]
		if !known {
			continue
		}
		if effects := staticEffects(distinct); len(effects) > 0 {
			w.symbols[idx].Summary = summarize(w.symbols[idx], effects)
		}
		if w.symbols[idx].Kind != SymbolTest {
			continue
		}
		for _, target := range pyTestedNames(w.symbols[idx].Name, distinct) {
			w.edges = append(w.edges, Edge{Kind: EdgeTests, From: id, To: target, Line: w.symbols[idx].Line})
		}
	}
}

// pyTestedNames resolves what `test_parse_header` covers, from evidence: the
// candidates come from the name, and only the ones the body actually calls are
// emitted.
func pyTestedNames(testName string, callees []string) []string {
	base := strings.TrimPrefix(testName[strings.LastIndex(testName, ".")+1:], "test")
	base = strings.TrimPrefix(base, "_")
	if base == "" {
		return nil
	}
	called := make(map[string]bool, len(callees)*2)
	for _, callee := range callees {
		called[callee] = true
		if _, member, ok := strings.Cut(callee, "."); ok {
			called[member] = true
		}
	}
	candidates := map[string]bool{base: true}
	for parts := strings.Split(base, "_"); len(parts) > 1; parts = parts[:len(parts)-1] {
		candidates[strings.Join(parts[:len(parts)-1], "_")] = true
	}
	var out []string
	for candidate := range candidates {
		if candidate != "" && called[candidate] {
			out = append(out, candidate)
		}
	}
	sort.Strings(out)
	return out
}

// pySignature lifts the parameter list off a def line.
func pySignature(code string) string {
	m := pyParams.FindStringSubmatch(code)
	if m == nil {
		return ""
	}
	sig := strings.TrimSpace(m[0])
	sig = strings.TrimSuffix(strings.TrimSpace(sig), ":")
	return clampRunes(collapseSpaces(sig), maxSignature)
}
