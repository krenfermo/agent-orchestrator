package codegraph

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// extract_go.go — the Go extractor.
//
// Go is the one language in this repository with a real parser in the standard
// library, so it is the one where the graph is built from an AST rather than
// from a scan, and it is where the richer relation kinds are proven:
// embedding, compile-time interface assertions, signature references, HTTP
// route registrations, configuration reads, and test coverage.
//
// Everything here is read out of the declaration itself. Nothing is inferred
// across files, because inference across files without a type checker is a
// guess, and a guessed edge is the one kind a Reviewer cannot audit.

// goExtractor reads Go source with the standard library parser, so functions,
// methods, types, package-level constants and variables, imports, and call
// sites come from the real AST rather than from pattern matching.
type goExtractor struct{}

// Language implements Extractor.
func (goExtractor) Language() string { return "go" }

// Extensions implements Extractor.
func (goExtractor) Extensions() []string { return []string{".go"} }

// maxSignatureRefs bounds how many type references one symbol contributes. A
// twelve-parameter constructor is real; a hundred-edge fan-out from one
// symbol is a graph nobody can traverse.
const maxSignatureRefs = 16

// Extract implements Extractor. A file with syntax errors is kept if the
// parser still produced a partial AST: half a file's symbols beat none while
// someone is mid-edit.
func (goExtractor) Extract(relPath string, src []byte) (Extraction, error) {
	fset := token.NewFileSet()
	// ParseComments, because the doc comment is the single most useful thing
	// a summary can carry and it is the author's own words rather than
	// anything AO invented.
	file, err := parser.ParseFile(fset, relPath, src, parser.ParseComments|parser.SkipObjectResolution)
	if file == nil {
		return Extraction{}, err
	}

	g := &goWalk{
		fset:     fset,
		src:      src,
		rel:      relPath,
		isTest:   ClassifyFile(relPath, src) == RoleTest,
		endpoint: map[string]bool{},
		config:   map[string]bool{},
	}

	for _, imp := range file.Imports {
		target, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil {
			target = strings.Trim(imp.Path.Value, `"`)
		}
		g.edge(Edge{Kind: EdgeImport, From: relPath, To: target, Line: g.line(imp.Pos())})
	}

	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			g.funcDecl(node)
		case *ast.GenDecl:
			g.genDecl(node)
		}
	}
	return Extraction{Symbols: g.symbols, Edges: g.edges}, nil
}

// goWalk accumulates one file's extraction. It is a struct rather than a pile
// of parameters because six of the eight relation kinds need the same four
// pieces of context, and threading them by hand is how one of them ends up
// recording the wrong line.
type goWalk struct {
	fset   *token.FileSet
	src    []byte
	rel    string
	isTest bool

	symbols []Symbol
	edges   []Edge
	// endpoint and config dedupe the synthesised symbols: a route registered
	// in a loop and an environment variable read in four functions are each
	// ONE surface, named once, with an edge from every site that touches it.
	endpoint map[string]bool
	config   map[string]bool
}

func (g *goWalk) line(pos token.Pos) int { return g.fset.Position(pos).Line }

func (g *goWalk) edge(e Edge) { g.edges = append(g.edges, e) }

func (g *goWalk) symbol(s Symbol) { g.symbols = append(g.symbols, s) }

// funcDecl records one function or method, everything its signature says, and
// everything its body does that the graph models.
func (g *goWalk) funcDecl(decl *ast.FuncDecl) {
	kind := SymbolFunction
	name := decl.Name.Name
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = SymbolMethod
		if recv := goTypeName(decl.Recv.List[0].Type); recv != "" {
			name = recv + "." + name
		}
	}
	if g.isTest && isGoTestName(decl.Name.Name) {
		kind = SymbolTest
	}

	sym := Symbol{
		ID:            symbolID(g.rel, kind, name),
		Name:          name,
		Kind:          kind,
		File:          g.rel,
		Line:          g.line(decl.Pos()),
		EndLine:       g.line(decl.End()),
		Signature:     clampRunes(collapseSpaces(g.text(decl.Type.Params.Pos(), decl.Type.End())), maxSignature),
		Doc:           firstSentence(docText(decl.Doc)),
		Exported:      ast.IsExported(decl.Name.Name),
		SummarySource: SummaryStatic,
		Hash:          hashBytes(g.bytes(decl.Pos(), decl.End())),
	}

	callees := g.bodyEdges(sym.ID, decl)
	sym.Summary = summarize(sym, staticEffects(callees))
	g.symbol(sym)

	for _, ref := range goSignatureRefs(decl.Type) {
		g.edge(Edge{Kind: EdgeReferences, From: sym.ID, To: ref, Line: sym.Line})
	}
	if kind == SymbolTest {
		for _, target := range goTestedNames(decl.Name.Name, callees) {
			g.edge(Edge{Kind: EdgeTests, From: sym.ID, To: target, Line: sym.Line})
		}
	}
}

// bodyEdges walks one body once and emits every relation it carries: call
// edges, HTTP route registrations, and configuration reads. It returns the
// distinct callee names, which is what staticEffects reads and what test
// coverage is proven from.
//
// One walk rather than three because the AST is the expensive part: on this
// repository's 1800 Go files the difference is the whole cost of an index.
func (g *goWalk) bodyEdges(from string, decl *ast.FuncDecl) []string {
	if decl.Body == nil {
		return nil
	}
	seen := map[string]bool{}
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if callee := goCalleeName(call.Fun); callee != "" {
			seen[callee] = true
			g.routeEdge(from, callee, call)
			g.configEdge(from, callee, call)
		}
		return true
	})
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		g.edge(Edge{Kind: EdgeCall, From: from, To: name, Line: g.line(decl.Pos())})
	}
	return names
}

// genDecl records types, interfaces, constants and variables, plus the two
// relations a declaration list can prove: embedding, and a compile-time
// interface assertion.
func (g *goWalk) genDecl(decl *ast.GenDecl) {
	switch decl.Tok {
	case token.TYPE:
		for _, spec := range decl.Specs {
			typed, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			g.typeSpec(decl, typed)
		}
	case token.CONST, token.VAR:
		kind := SymbolConstant
		if decl.Tok == token.VAR {
			kind = SymbolVariable
		}
		for _, spec := range decl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			g.valueSpec(decl, value, kind)
		}
	}
}

func (g *goWalk) typeSpec(decl *ast.GenDecl, spec *ast.TypeSpec) {
	kind := SymbolType
	signature := clampRunes(collapseSpaces(g.text(spec.Type.Pos(), spec.Type.End())), maxSignature)
	switch underlying := spec.Type.(type) {
	case *ast.InterfaceType:
		kind = SymbolInterface
		// The kind already says "interface"; repeating it in the signature
		// would render as "interface Repository interface".
		signature = ""
		g.interfaceMembers(spec.Name.Name, underlying)
	case *ast.StructType:
		signature = "struct"
		g.structEmbeds(spec.Name.Name, underlying)
	}

	sym := Symbol{
		ID:            symbolID(g.rel, kind, spec.Name.Name),
		Name:          spec.Name.Name,
		Kind:          kind,
		File:          g.rel,
		Line:          g.line(spec.Name.Pos()),
		EndLine:       g.line(spec.End()),
		Signature:     signature,
		Doc:           firstSentence(docText(spec.Doc, decl.Doc)),
		Exported:      ast.IsExported(spec.Name.Name),
		SummarySource: SummaryStatic,
		Hash:          hashBytes(g.bytes(spec.Pos(), spec.End())),
	}
	sym.Summary = summarize(sym, nil)
	g.symbol(sym)
}

// interfaceMembers records an interface's method set as symbols, and its
// embedded interfaces as edges.
//
// The method set is worth storing because an agent asked to change a contract
// needs to see the contract, and `Repository` in this repository is forty
// methods long: naming them costs forty short rows and saves reading the file.
func (g *goWalk) interfaceMembers(owner string, node *ast.InterfaceType) {
	if node.Methods == nil {
		return
	}
	from := symbolID(g.rel, SymbolInterface, owner)
	for _, field := range node.Methods.List {
		fn, ok := field.Type.(*ast.FuncType)
		if !ok {
			// No names and not a func type: an embedded interface or a type
			// constraint element. Either way it is an embedding relation.
			if embedded := goTypeName(field.Type); embedded != "" {
				g.edge(Edge{Kind: EdgeEmbeds, From: from, To: embedded, Line: g.line(field.Pos())})
			}
			continue
		}
		for _, ident := range field.Names {
			name := owner + "." + ident.Name
			sym := Symbol{
				ID:            symbolID(g.rel, SymbolMethod, name),
				Name:          name,
				Kind:          SymbolMethod,
				File:          g.rel,
				Line:          g.line(ident.Pos()),
				EndLine:       g.line(field.End()),
				Signature:     clampRunes(collapseSpaces(g.text(fn.Params.Pos(), fn.End())), maxSignature),
				Doc:           firstSentence(docText(field.Doc)),
				Exported:      ast.IsExported(ident.Name),
				SummarySource: SummaryStatic,
				Hash:          hashBytes(g.bytes(field.Pos(), field.End())),
			}
			sym.Summary = summarize(sym, nil)
			g.symbol(sym)
			for _, ref := range goSignatureRefs(fn) {
				g.edge(Edge{Kind: EdgeReferences, From: sym.ID, To: ref, Line: sym.Line})
			}
		}
	}
}

// structEmbeds records a struct's embedded types. An embedded field is a field
// with a type and no name, and it is the closest thing Go has to inheritance,
// so it is the edge that answers "where do this type's other methods come
// from".
func (g *goWalk) structEmbeds(owner string, node *ast.StructType) {
	if node.Fields == nil {
		return
	}
	from := symbolID(g.rel, SymbolType, owner)
	for _, field := range node.Fields.List {
		if len(field.Names) > 0 {
			continue
		}
		if embedded := goTypeName(field.Type); embedded != "" {
			g.edge(Edge{Kind: EdgeEmbeds, From: from, To: embedded, Line: g.line(field.Pos())})
		}
	}
}

// valueSpec records constants and variables, and recognises the one Go idiom
// that PROVES an implementation relationship:
//
//	var _ Iface = (*T)(nil)
//
// The compiler checks that assertion, so recording it is recording a fact the
// build already enforces. Structural satisfaction that nobody asserted is not
// recorded: proving it needs a type checker, and this indexer does not build
// one.
func (g *goWalk) valueSpec(decl *ast.GenDecl, spec *ast.ValueSpec, kind SymbolKind) {
	if kind == SymbolVariable {
		if iface, impl, ok := goImplementsAssertion(spec); ok {
			g.edge(Edge{
				Kind: EdgeImplements,
				From: g.implementsSource(impl),
				To:   iface,
				Line: g.line(spec.Pos()),
			})
		}
	}
	hash := hashBytes(g.bytes(spec.Pos(), spec.End()))
	doc := firstSentence(docText(spec.Doc, decl.Doc))
	for _, ident := range spec.Names {
		if ident == nil || ident.Name == "_" {
			continue
		}
		sym := Symbol{
			ID:            symbolID(g.rel, kind, ident.Name),
			Name:          ident.Name,
			Kind:          kind,
			File:          g.rel,
			Line:          g.line(ident.Pos()),
			EndLine:       g.line(spec.End()),
			Doc:           doc,
			Exported:      ast.IsExported(ident.Name),
			SummarySource: SummaryStatic,
			Hash:          hash,
		}
		if spec.Type != nil {
			sym.Signature = " " + clampRunes(collapseSpaces(g.text(spec.Type.Pos(), spec.Type.End())), maxSignature)
		}
		sym.Summary = summarize(sym, nil)
		g.symbol(sym)
	}
}

// implementsSource addresses the implementing side of an assertion.
//
// A type declared in this file is addressed by its symbol ID, which is exact.
// A type from another package (`var _ Repository = (*store.Store)(nil)`, the
// commonest form) has no ID here, so the edge carries the name as written and
// Query resolves it the same way it resolves a callee.
func (g *goWalk) implementsSource(impl string) string {
	local := symbolID(g.rel, SymbolType, impl)
	for _, sym := range g.symbols {
		if sym.ID == local {
			return local
		}
	}
	if strings.Contains(impl, ".") {
		return impl
	}
	// The declaration may still be ahead of the assertion in the file; the
	// local ID is the better address when the name is unqualified.
	return local
}

// routeEdge records an HTTP route registered at a call site.
//
// The shapes recognised are the ones that carry a literal method and a literal
// pattern, which is what makes them provable: chi/gorilla/gin style
// `r.Get("/pattern", handler)`, `r.Method("GET", "/pattern", handler)`, and
// `mux.HandleFunc("/pattern", handler)`. A route whose pattern is computed is
// not recorded, because AO would be inventing the string.
func (g *goWalk) routeEdge(from, callee string, call *ast.CallExpr) {
	method, pattern, handler, ok := goRouteCall(callee, call)
	if !ok {
		return
	}
	name := method + " " + pattern
	id := symbolID(g.rel, SymbolEndpoint, name)
	if !g.endpoint[name] {
		g.endpoint[name] = true
		sym := Symbol{
			ID:            id,
			Name:          name,
			Kind:          SymbolEndpoint,
			File:          g.rel,
			Line:          g.line(call.Pos()),
			EndLine:       g.line(call.End()),
			Exported:      true,
			SummarySource: SummaryStatic,
			Hash:          hashBytes([]byte(name + "\x00" + handler)),
		}
		sym.Summary = summarize(sym, nil)
		g.symbol(sym)
	}
	if handler != "" {
		g.edge(Edge{Kind: EdgeRoutesTo, From: id, To: handler, Line: g.line(call.Pos())})
	}
	// The registering function reaches the endpoint too, so a query that
	// starts at the router setup can walk to the routes it installs.
	g.edge(Edge{Kind: EdgeReferences, From: from, To: name, Line: g.line(call.Pos())})
}

// configEdge records that code reads a configuration key. The KEY is stored;
// the value is never read, let alone stored -- see DeniedPath and section 28
// of the brief.
func (g *goWalk) configEdge(from, callee string, call *ast.CallExpr) {
	switch callee {
	case "os.Getenv", "os.LookupEnv", "os.Setenv":
	default:
		return
	}
	if len(call.Args) == 0 {
		return
	}
	key, ok := goStringLiteral(call.Args[0])
	if !ok || key == "" {
		return
	}
	id := symbolID(g.rel, SymbolConfig, key)
	if !g.config[key] {
		g.config[key] = true
		sym := Symbol{
			ID:            id,
			Name:          key,
			Kind:          SymbolConfig,
			File:          g.rel,
			Line:          g.line(call.Pos()),
			Exported:      true,
			SummarySource: SummaryStatic,
			Hash:          hashBytes([]byte(key)),
		}
		sym.Summary = summarize(sym, nil)
		g.symbol(sym)
	}
	g.edge(Edge{Kind: EdgeConfigures, From: from, To: key, Line: g.line(call.Pos())})
}

// text returns the source between two positions as a collapsed string.
func (g *goWalk) text(from, to token.Pos) string { return string(g.bytes(from, to)) }

// bytes returns the exact source bytes a range spans. A range whose offsets
// fall outside src (only possible from a badly damaged partial parse) degrades
// to an empty slice rather than panicking.
func (g *goWalk) bytes(from, to token.Pos) []byte {
	start := g.fset.Position(from).Offset
	end := g.fset.Position(to).Offset
	if start < 0 || end > len(g.src) || start >= end {
		return nil
	}
	return g.src[start:end]
}

// httpMethods are the router method names a literal route registration can use.
var httpMethods = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH",
	"Delete": "DELETE", "Head": "HEAD", "Options": "OPTIONS", "Connect": "CONNECT",
	"Trace": "TRACE",
}

// goRouteCall decodes a route registration, or reports that the call is not
// one. It never guesses: a non-literal pattern means no route.
func goRouteCall(callee string, call *ast.CallExpr) (method, pattern, handler string, ok bool) {
	_, fn, hasSelector := strings.Cut(callee, ".")
	if !hasSelector {
		fn = callee
	}
	switch fn {
	case "HandleFunc", "Handle":
		if len(call.Args) < 2 {
			return "", "", "", false
		}
		pattern, ok = goStringLiteral(call.Args[0])
		if !ok {
			return "", "", "", false
		}
		return "ANY", pattern, goCalleeName(call.Args[1]), true
	case "Method", "MethodFunc":
		if len(call.Args) < 3 {
			return "", "", "", false
		}
		method, ok = goStringLiteral(call.Args[0])
		if !ok {
			return "", "", "", false
		}
		pattern, ok = goStringLiteral(call.Args[1])
		if !ok {
			return "", "", "", false
		}
		return strings.ToUpper(method), pattern, goCalleeName(call.Args[2]), true
	default:
		verb, known := httpMethods[fn]
		if !known || len(call.Args) < 2 {
			return "", "", "", false
		}
		pattern, ok = goStringLiteral(call.Args[0])
		if !ok || !strings.HasPrefix(pattern, "/") {
			// A path that does not start with "/" is far more likely to be
			// some other API's `Get(key)` than a route.
			return "", "", "", false
		}
		return verb, pattern, goCalleeName(call.Args[1]), true
	}
}

// goStringLiteral unwraps a basic string literal, reporting false for anything
// computed.
func goStringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// goImplementsAssertion decodes `var _ Iface = (*T)(nil)` and its variants
// (`var _ Iface = T{}`, `var _ Iface = someT`), returning the interface and
// the implementing type name.
func goImplementsAssertion(spec *ast.ValueSpec) (iface, impl string, ok bool) {
	if spec.Type == nil || len(spec.Names) != 1 || spec.Names[0].Name != "_" || len(spec.Values) != 1 {
		return "", "", false
	}
	iface = goTypeName(spec.Type)
	if iface == "" {
		return "", "", false
	}
	impl = goAssertedTypeName(spec.Values[0])
	if impl == "" {
		return "", "", false
	}
	return iface, impl, true
}

// goAssertedTypeName reads the implementing type out of the right-hand side of
// an assertion.
func goAssertedTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.CallExpr: // (*T)(nil)
		return goTypeName(typed.Fun)
	case *ast.CompositeLit: // T{}
		return goTypeName(typed.Type)
	case *ast.UnaryExpr: // &T{}
		return goAssertedTypeName(typed.X)
	case *ast.ParenExpr:
		return goAssertedTypeName(typed.X)
	case *ast.Ident, *ast.SelectorExpr, *ast.StarExpr:
		return goTypeName(expr)
	default:
		return ""
	}
}

// goSignatureRefs names the types that appear in a function's parameters and
// results: its public contract's dependencies.
//
// Builtins are excluded because "this function takes a string" is not a
// relationship, and the list is bounded and sorted so the edge set is
// deterministic.
func goSignatureRefs(fn *ast.FuncType) []string {
	if fn == nil {
		return nil
	}
	seen := map[string]bool{}
	collect := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			ast.Inspect(field.Type, func(n ast.Node) bool {
				switch typed := n.(type) {
				case *ast.SelectorExpr:
					if base, ok := typed.X.(*ast.Ident); ok {
						seen[base.Name+"."+typed.Sel.Name] = true
					}
					return false
				case *ast.Ident:
					if !goBuiltinTypes[typed.Name] {
						seen[typed.Name] = true
					}
				}
				return true
			})
		}
	}
	collect(fn.Params)
	collect(fn.Results)
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) > maxSignatureRefs {
		out = out[:maxSignatureRefs]
	}
	return out
}

// goBuiltinTypes are the predeclared names a reference edge would only add
// noise for.
var goBuiltinTypes = map[string]bool{
	"any": true, "bool": true, "byte": true, "comparable": true, "complex64": true,
	"complex128": true, "error": true, "float32": true, "float64": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"rune": true, "string": true, "uint": true, "uint8": true, "uint16": true,
	"uint32": true, "uint64": true, "uintptr": true,
}

// isGoTestName reports whether a function name is one `go test` will run.
func isGoTestName(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
			continue
		}
		next := rune(name[len(prefix)])
		// `TestMain` is a runner, not a test of something called `Main`, but
		// it is still a test function. The uppercase/underscore check is what
		// `go test` itself applies.
		if next == '_' || (next >= 'A' && next <= 'Z') {
			return true
		}
	}
	return false
}

// goTestedNames resolves what a test actually exercises, from evidence.
//
// Two steps, and the second is what makes it honest. First the test's own name
// is broken into the subjects it could plausibly be about:
// `TestServiceDelete` yields `ServiceDelete`, `Service`, `Delete` and
// `Service.Delete`; `TestSyncer_EnsureFresh` yields the same shapes around the
// underscore. Then every candidate that the body does not actually CALL is
// discarded.
//
// Without the second step a naming convention alone would happily assert that
// `TestNothingHappens` covers a function called `NothingHappens` that does not
// exist -- a coverage edge nobody would notice was fictional until they
// trusted it.
func goTestedNames(testName string, callees []string) []string {
	base := testName
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(base, prefix) {
			base = strings.TrimPrefix(base, prefix)
			break
		}
	}
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

	var out []string
	for _, candidate := range nameCandidates(base) {
		if called[candidate] {
			out = append(out, candidate)
		}
	}
	sort.Strings(out)
	return out
}

// maxNameTokens bounds how far a test name is decomposed. A name with more
// tokens than this is a sentence, and a sentence's every prefix is not a
// plausible symbol.
const maxNameTokens = 6

// nameCandidates enumerates the symbol names a test name could be about.
//
// It is deliberately generous, because the evidence gate above is what makes
// generosity safe: a candidate that nothing calls costs nothing, and a
// candidate that was never proposed can never be found.
func nameCandidates(base string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}

	add(base)
	segments := strings.Split(base, "_")
	for _, segment := range segments {
		add(segment)
	}
	if len(segments) == 2 && segments[0] != "" && segments[1] != "" {
		add(segments[0] + "." + segments[1])
	}

	tokens := splitCamel(segments[0])
	if len(tokens) > maxNameTokens {
		tokens = tokens[:maxNameTokens]
	}
	for i := 1; i < len(tokens); i++ {
		add(strings.Join(tokens[:i], ""))
		add(strings.Join(tokens[i:], ""))
		add(strings.Join(tokens[:i], "") + "." + strings.Join(tokens[i:], ""))
	}
	return out
}

// splitCamel breaks an identifier at its capitals, keeping runs of capitals
// (an initialism like `HTTPServer`) together with the word they introduce.
func splitCamel(name string) []string {
	var tokens []string
	start := 0
	runes := []rune(name)
	for i := 1; i < len(runes); i++ {
		upper := runes[i] >= 'A' && runes[i] <= 'Z'
		if !upper {
			continue
		}
		prevUpper := runes[i-1] >= 'A' && runes[i-1] <= 'Z'
		nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
		if prevUpper && !nextLower {
			continue
		}
		tokens = append(tokens, string(runes[start:i]))
		start = i
	}
	if start < len(runes) {
		tokens = append(tokens, string(runes[start:]))
	}
	return tokens
}

// docText joins the first non-empty comment group's text. Several are accepted
// because a spec inside a parenthesised declaration may carry its own doc, and
// may instead be documented by the declaration that contains it.
func docText(groups ...*ast.CommentGroup) string {
	for _, group := range groups {
		if group == nil {
			continue
		}
		if text := strings.TrimSpace(group.Text()); text != "" {
			return text
		}
	}
	return ""
}

func goCalleeName(expr ast.Expr) string {
	switch fun := expr.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if base, ok := fun.X.(*ast.Ident); ok {
			return base.Name + "." + fun.Sel.Name
		}
		return fun.Sel.Name
	case *ast.IndexExpr: // generic instantiation: f[T](...)
		return goCalleeName(fun.X)
	case *ast.IndexListExpr:
		return goCalleeName(fun.X)
	case *ast.ParenExpr:
		return goCalleeName(fun.X)
	default:
		return ""
	}
}

// goTypeName renders a receiver or type expression as a bare name, dropping
// pointer and generic decoration so *Store and Store[T] both read as "Store".
// A qualified name keeps its package selector, because `store.Store` and a
// local `Store` are different types and collapsing them would be a lie.
func goTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return goTypeName(typed.X)
	case *ast.IndexExpr:
		return goTypeName(typed.X)
	case *ast.IndexListExpr:
		return goTypeName(typed.X)
	case *ast.ParenExpr:
		return goTypeName(typed.X)
	case *ast.SelectorExpr:
		if base, ok := typed.X.(*ast.Ident); ok {
			return base.Name + "." + typed.Sel.Name
		}
		return typed.Sel.Name
	default:
		return ""
	}
}
