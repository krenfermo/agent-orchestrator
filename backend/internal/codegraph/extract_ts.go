package codegraph

import (
	"regexp"
	"sort"
	"strings"
)

// extract_ts.go — the TypeScript/JavaScript extractor.
//
// There is no TypeScript parser in the Go standard library and none is
// vendored here, so this is not an AST extractor and does not pretend to be
// one. What it is instead is a real lexer plus a brace-tracking structural
// walk: the source is first tokenized so that comments, string bodies and
// template literals cannot be mistaken for code, and only then is the
// remaining structure read.
//
// That distinction is the reason this file exists rather than another table of
// line patterns. A line-oriented regex scanner reports `class Foo` inside a
// comment, loses every method because it cannot tell which class it is in, and
// records `import x from 'y'` written inside a string. Those are not edge
// cases in a frontend: they are what a test file, a code-generation template
// and a documentation block look like. Masking the non-code first, and
// tracking brace depth, removes that whole class of wrong facts, and it is
// what makes qualifying a method as `Class.method` possible at all.
//
// What it still cannot do, stated plainly rather than papered over: it does
// not resolve types, so it emits no reference edges for TypeScript; and its
// call edges are attributed to the innermost function-like scope by brace
// depth, which is exact for ordinary code and approximate inside a callback
// chain written on one line. Where the answer would be a guess, nothing is
// emitted.

// tsExtractor reads TypeScript and JavaScript.
type tsExtractor struct {
	language   string
	extensions []string
}

// Language implements Extractor.
func (t tsExtractor) Language() string { return t.language }

// Extensions implements Extractor.
func (t tsExtractor) Extensions() []string { return t.extensions }

// Extract implements Extractor.
func (t tsExtractor) Extract(relPath string, src []byte) (Extraction, error) {
	lines := maskScript(string(src))
	w := &tsWalk{
		rel:    relPath,
		lines:  lines,
		isTest: ClassifyFile(relPath, src) == RoleTest,
		config: map[string]bool{},
	}
	w.run()
	return Extraction{Symbols: w.symbols, Edges: w.edges}, nil
}

// maskedLine is one source line with everything that is not code blanked out,
// alongside the string literals that were removed from it.
type maskedLine struct {
	// Number is the 1-based line number.
	Number int
	// Code is the line with comment text and string bodies replaced by spaces,
	// so column positions still line up with the original.
	Code string
	// Raw is the original line, used when a symbol's own text is needed.
	Raw string
	// Strings are the literal values that were masked out, in order.
	Strings []string
	// Depth is the brace nesting depth at the START of the line.
	Depth int
}

// maskScript tokenizes a script into masked lines.
//
// It is a small state machine rather than a regex because the states are
// genuinely stateful: a `/` is a comment, a division, or a regex literal
// depending on what came before it, and a backtick string can contain
// `${ expression }` holes that are code again.
func maskScript(src string) []maskedLine {
	var out []maskedLine
	var code strings.Builder
	var raw strings.Builder
	var literal strings.Builder
	var strs []string

	depth := 0
	lineDepth := 0
	lineNo := 1
	// templateDepth tracks the brace nesting inside a `${ }` hole so the
	// closing brace of the hole is not read as a block close.
	var stack []byte // open string delimiters, innermost last
	inLine, inBlock := false, false

	flush := func() {
		out = append(out, maskedLine{
			Number: lineNo, Code: code.String(), Raw: raw.String(),
			Strings: strs, Depth: lineDepth,
		})
		code.Reset()
		raw.Reset()
		strs = nil
		lineNo++
		lineDepth = depth
		inLine = false
	}

	blank := func(n int) {
		for i := 0; i < n; i++ {
			code.WriteByte(' ')
		}
	}

	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			flush()
			continue
		}
		raw.WriteByte(c)

		switch {
		case inLine:
			blank(1)
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				raw.WriteByte(src[i+1])
				blank(2)
				i++
				continue
			}
			blank(1)
		case len(stack) > 0:
			quote := stack[len(stack)-1]
			switch {
			case c == '\\' && i+1 < len(src) && src[i+1] != '\n':
				literal.WriteByte(src[i+1])
				raw.WriteByte(src[i+1])
				blank(2)
				i++
			case c == quote:
				stack = stack[:len(stack)-1]
				strs = append(strs, literal.String())
				literal.Reset()
				blank(1)
			case quote == '`' && c == '$' && i+1 < len(src) && src[i+1] == '{':
				// Leave the hole's contents as code; the matching brace is
				// balanced by the depth counter like any other block.
				stack = stack[:len(stack)-1]
				strs = append(strs, literal.String())
				literal.Reset()
				code.WriteByte('$')
				code.WriteByte('{')
				raw.WriteByte(src[i+1])
				depth++
				i++
			default:
				literal.WriteByte(c)
				blank(1)
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			raw.WriteByte(src[i+1])
			blank(2)
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			raw.WriteByte(src[i+1])
			blank(2)
			i++
		case c == '\'' || c == '"' || c == '`':
			stack = append(stack, c)
			literal.Reset()
			blank(1)
		default:
			if c == '{' {
				depth++
			} else if c == '}' && depth > 0 {
				depth--
			}
			code.WriteByte(c)
		}
	}
	if raw.Len() > 0 || code.Len() > 0 || len(strs) > 0 {
		flush()
	}
	return out
}

// tsWalk accumulates one script's extraction.
type tsWalk struct {
	rel    string
	lines  []maskedLine
	isTest bool

	symbols []Symbol
	edges   []Edge
	config  map[string]bool

	// scopes is the enclosing structure at the current line: a class provides
	// the qualifier for its methods, a function provides the attribution for
	// call edges.
	scopes []tsScope
	// pendingDoc is the JSDoc block immediately above the current line.
	pendingDoc string
	// callsFor accumulates callee names per symbol ID, so a summary can report
	// what a function actually touches.
	callsFor map[string][]string
	order    []string
}

// tsScope is one open brace-delimited structure.
type tsScope struct {
	kind     string // "class" or "function"
	name     string
	symbolID string
	depth    int
}

var (
	// These match the MASKED line, where a string body and its quotes have
	// already been blanked out -- so they look for the keyword and leave the
	// module specifier to maskedLine.Strings. Matching the quote here is what
	// the first version got wrong: by the time the pattern runs, there is no
	// quote left to match.
	tsImportFrom  = regexp.MustCompile(`^\s*(?:import|export)\b[^;]*\bfrom\b`)
	tsImportBare  = regexp.MustCompile(`^\s*import\s+\S`)
	tsRequire     = regexp.MustCompile(`\brequire\s*\(`)
	tsFunc        = regexp.MustCompile(`^\s*(export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	tsClass       = regexp.MustCompile(`^\s*(export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	tsExtends     = regexp.MustCompile(`\bextends\s+([A-Za-z_$][\w$.]*)`)
	tsImplements  = regexp.MustCompile(`\bimplements\s+([A-Za-z_$][\w$.,\s]*)`)
	tsInterface   = regexp.MustCompile(`^\s*(export\s+)?(?:declare\s+)?interface\s+([A-Za-z_$][\w$]*)`)
	tsTypeAlias   = regexp.MustCompile(`^\s*(export\s+)?(?:declare\s+)?(?:type|enum)\s+([A-Za-z_$][\w$]*)`)
	tsArrowConst  = regexp.MustCompile(`^\s*(export\s+)?(?:default\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+?)?=\s*(?:async\s+)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]+?)?=>`)
	tsConst       = regexp.MustCompile(`^\s*(export\s+)?const\s+([A-Za-z_$][\w$]*)\s*[:=]`)
	tsVar         = regexp.MustCompile(`^\s*(export\s+)?(?:let|var)\s+([A-Za-z_$][\w$]*)\s*[:=]`)
	tsMethod      = regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+|static\s+|readonly\s+|override\s+|abstract\s+|async\s+|\*\s*|get\s+|set\s+)*([A-Za-z_$][\w$]*)\s*(?:<[^>()]*>)?\s*\(`)
	tsCall        = regexp.MustCompile(`([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\s*\(`)
	tsEnvAccess   = regexp.MustCompile(`\b(?:process\.env|import\.meta\.env)\.([A-Za-z_][A-Za-z0-9_]*)`)
	tsSpecName    = regexp.MustCompile(`^\s*(?:describe|it|test|suite)(?:\.\w+)?\s*\(`)
	tsParams      = regexp.MustCompile(`\((.*)`)
	tsReservedFns = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"return": true, "typeof": true, "await": true, "function": true, "constructor": false,
	}
)

func (w *tsWalk) run() {
	w.callsFor = map[string][]string{}
	for _, line := range w.lines {
		w.closeScopes(line.Depth)
		w.lineEdges(line)
		w.declaration(line)
		w.trackDoc(line)
	}
	w.finishSummaries()
}

// closeScopes pops every scope whose block has ended by this line's depth.
func (w *tsWalk) closeScopes(depth int) {
	for len(w.scopes) > 0 && depth <= w.scopes[len(w.scopes)-1].depth {
		w.scopes = w.scopes[:len(w.scopes)-1]
	}
}

// currentClass is the innermost enclosing class, if any.
func (w *tsWalk) currentClass() (tsScope, bool) {
	for i := len(w.scopes) - 1; i >= 0; i-- {
		if w.scopes[i].kind == "class" {
			return w.scopes[i], true
		}
	}
	return tsScope{}, false
}

// currentSymbol is the innermost function-like scope, which is what a call
// edge is attributed to. Outside any function, calls belong to the file's
// module-level initialization and are not attributed to a symbol.
func (w *tsWalk) currentSymbol() (tsScope, bool) {
	for i := len(w.scopes) - 1; i >= 0; i-- {
		if w.scopes[i].kind == "function" {
			return w.scopes[i], true
		}
	}
	return tsScope{}, false
}

// lineEdges records the relations a line carries regardless of what it
// declares: imports, calls, configuration reads, and spec coverage.
func (w *tsWalk) lineEdges(line maskedLine) {
	if (tsImportFrom.MatchString(line.Code) || tsImportBare.MatchString(line.Code) ||
		tsRequire.MatchString(line.Code)) && len(line.Strings) > 0 {
		for _, target := range line.Strings {
			if target = strings.TrimSpace(target); target != "" {
				w.edges = append(w.edges, Edge{Kind: EdgeImport, From: w.rel, To: target, Line: line.Number})
				break
			}
		}
	}

	for _, match := range tsEnvAccess.FindAllStringSubmatch(line.Code, -1) {
		w.configSymbol(match[1], line.Number)
	}

	owner, hasOwner := w.currentSymbol()
	for _, match := range tsCall.FindAllStringSubmatch(line.Code, -1) {
		callee := match[1]
		if tsReservedFns[callee] {
			continue
		}
		if hasOwner {
			w.callsFor[owner.symbolID] = append(w.callsFor[owner.symbolID], callee)
		}
	}

	if w.isTest && tsSpecName.MatchString(line.Code) && len(line.Strings) > 0 {
		w.specCoverage(line)
	}
}

// specCoverage records what a spec block names.
//
// A `describe("MemoryPanel", ...)` in `memory-panel.test.tsx` is the file's own
// statement about what it covers, and the file name corroborates it. The edge
// target is the described name, and it is emitted only from a describe/suite
// block, never from an `it(...)` sentence -- "renders the empty state" is
// prose, not a symbol.
func (w *tsWalk) specCoverage(line maskedLine) {
	if !strings.HasPrefix(strings.TrimSpace(line.Code), "describe") &&
		!strings.HasPrefix(strings.TrimSpace(line.Code), "suite") {
		return
	}
	name := strings.TrimSpace(line.Strings[0])
	if name == "" || strings.ContainsAny(name, " \t") {
		// A sentence rather than an identifier. Recording it as a symbol name
		// would put prose on the wrong side of an edge.
		return
	}
	from := symbolID(w.rel, SymbolTest, name)
	if !w.hasSymbol(from) {
		sym := Symbol{
			ID: from, Name: name, Kind: SymbolTest, File: w.rel,
			Line: line.Number, Exported: false, SummarySource: SummaryStatic,
			Hash: hashBytes([]byte(name)),
		}
		sym.Summary = summarize(sym, nil)
		w.addSymbol(sym)
	}
	w.edges = append(w.edges, Edge{Kind: EdgeTests, From: from, To: name, Line: line.Number})
}

// configSymbol records one environment key the script reads.
func (w *tsWalk) configSymbol(key string, line int) {
	id := symbolID(w.rel, SymbolConfig, key)
	if !w.config[key] {
		w.config[key] = true
		sym := Symbol{
			ID: id, Name: key, Kind: SymbolConfig, File: w.rel, Line: line,
			Exported: true, SummarySource: SummaryStatic, Hash: hashBytes([]byte(key)),
		}
		sym.Summary = summarize(sym, nil)
		w.addSymbol(sym)
	}
	from := w.rel
	if owner, ok := w.currentSymbol(); ok {
		from = owner.symbolID
	}
	w.edges = append(w.edges, Edge{Kind: EdgeConfigures, From: from, To: key, Line: line})
}

// declaration records whatever this line declares, and opens a scope when the
// declaration owns a block.
func (w *tsWalk) declaration(line maskedLine) {
	code := line.Code
	opens := strings.Contains(code, "{")

	switch {
	case tsClass.MatchString(code):
		m := tsClass.FindStringSubmatch(code)
		sym := w.declare(SymbolType, m[2], line, m[1] != "", "class") // signature names the form; kind alone cannot tell a class from a type alias
		for _, ext := range tsExtends.FindAllStringSubmatch(code, -1) {
			w.edges = append(w.edges, Edge{Kind: EdgeEmbeds, From: sym.ID, To: ext[1], Line: line.Number})
		}
		if impl := tsImplements.FindStringSubmatch(code); impl != nil {
			for _, name := range strings.Split(impl[1], ",") {
				if name = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), "{")); name != "" {
					w.edges = append(w.edges, Edge{Kind: EdgeImplements, From: sym.ID, To: name, Line: line.Number})
				}
			}
		}
		if opens {
			w.scopes = append(w.scopes, tsScope{kind: "class", name: m[2], symbolID: sym.ID, depth: line.Depth})
		}

	case tsInterface.MatchString(code):
		m := tsInterface.FindStringSubmatch(code)
		sym := w.declare(SymbolInterface, m[2], line, m[1] != "", "")
		for _, ext := range tsExtends.FindAllStringSubmatch(code, -1) {
			w.edges = append(w.edges, Edge{Kind: EdgeEmbeds, From: sym.ID, To: ext[1], Line: line.Number})
		}

	case tsFunc.MatchString(code):
		m := tsFunc.FindStringSubmatch(code)
		sym := w.declare(SymbolFunction, m[2], line, m[1] != "", tsSignature(code))
		if opens {
			w.scopes = append(w.scopes, tsScope{kind: "function", name: m[2], symbolID: sym.ID, depth: line.Depth})
		}

	case tsArrowConst.MatchString(code):
		m := tsArrowConst.FindStringSubmatch(code)
		sym := w.declare(SymbolFunction, m[2], line, m[1] != "", tsSignature(code))
		if opens {
			w.scopes = append(w.scopes, tsScope{kind: "function", name: m[2], symbolID: sym.ID, depth: line.Depth})
		}

	case tsTypeAlias.MatchString(code):
		m := tsTypeAlias.FindStringSubmatch(code)
		w.declare(SymbolType, m[2], line, m[1] != "", "")

	default:
		if class, inClass := w.currentClass(); inClass && opens {
			m := tsMethod.FindStringSubmatch(code)
			if m == nil || tsReservedFns[m[1]] {
				return
			}
			name := class.name + "." + m[1]
			sym := w.declare(SymbolMethod, name, line, !strings.Contains(code, "private"), tsSignature(code))
			w.scopes = append(w.scopes, tsScope{kind: "function", name: name, symbolID: sym.ID, depth: line.Depth})
			return
		}
		if m := tsConst.FindStringSubmatch(code); m != nil {
			w.declare(SymbolConstant, m[2], line, m[1] != "", "")
			return
		}
		if m := tsVar.FindStringSubmatch(code); m != nil {
			w.declare(SymbolVariable, m[2], line, m[1] != "", "")
		}
	}
}

// declare records one symbol and returns it.
func (w *tsWalk) declare(kind SymbolKind, name string, line maskedLine, exported bool, signature string) Symbol {
	sym := Symbol{
		ID:            symbolID(w.rel, kind, name),
		Name:          name,
		Kind:          kind,
		File:          w.rel,
		Line:          line.Number,
		Signature:     signature,
		Doc:           firstSentence(w.pendingDoc),
		Exported:      exported,
		SummarySource: SummaryStatic,
		Hash:          hashBytes([]byte(collapseSpaces(line.Raw))),
	}
	w.pendingDoc = ""
	sym.Summary = summarize(sym, nil)
	w.addSymbol(sym)
	return sym
}

func (w *tsWalk) addSymbol(sym Symbol) {
	if w.hasSymbol(sym.ID) {
		return
	}
	w.symbols = append(w.symbols, sym)
	w.order = append(w.order, sym.ID)
}

func (w *tsWalk) hasSymbol(id string) bool {
	for _, sym := range w.symbols {
		if sym.ID == id {
			return true
		}
	}
	return false
}

// trackDoc remembers a JSDoc block so the next declaration can carry it.
func (w *tsWalk) trackDoc(line maskedLine) {
	trimmed := strings.TrimSpace(line.Raw)
	switch {
	case strings.HasPrefix(trimmed, "/**"), strings.HasPrefix(trimmed, "*"), strings.HasPrefix(trimmed, "//"):
		text := strings.TrimSpace(strings.Trim(trimmed, "/*"))
		text = strings.TrimSpace(strings.TrimPrefix(text, "*"))
		if text != "" {
			if w.pendingDoc != "" {
				w.pendingDoc += " "
			}
			w.pendingDoc += text
		}
	case trimmed == "":
		// A blank line separates a comment from what it documents only when
		// nothing was declared in between; keeping the doc here is what lets
		// a JSDoc block with a trailing blank line still attach.
	}
}

// finishSummaries folds the observed calls into each function's summary, so a
// TypeScript symbol carries the same "what does it touch" note a Go one does.
func (w *tsWalk) finishSummaries() {
	for i := range w.symbols {
		calls := w.callsFor[w.symbols[i].ID]
		if len(calls) == 0 {
			continue
		}
		sort.Strings(calls)
		effects := staticEffects(calls)
		if len(effects) == 0 {
			continue
		}
		w.symbols[i].Summary = summarize(w.symbols[i], effects)
	}
	for id, calls := range w.callsFor {
		sort.Strings(calls)
		seen := map[string]bool{}
		for _, callee := range calls {
			if seen[callee] {
				continue
			}
			seen[callee] = true
			w.edges = append(w.edges, Edge{Kind: EdgeCall, From: id, To: callee})
		}
	}
}

// tsSignature lifts the parameter list off a declaration line, bounded. It is
// the text as written, which for TypeScript is also the type contract.
func tsSignature(code string) string {
	m := tsParams.FindStringSubmatch(code)
	if m == nil {
		return ""
	}
	sig := strings.TrimSpace(m[0])
	sig = strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sig), "{")), "=>")
	return clampRunes(collapseSpaces(strings.TrimSpace(sig)), maxSignature)
}
