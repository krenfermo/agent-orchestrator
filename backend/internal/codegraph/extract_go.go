package codegraph

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// goExtractor reads Go source with the standard library parser, so functions,
// methods, types, package-level constants and variables, imports, and call
// sites come from the real AST rather than from pattern matching.
type goExtractor struct{}

// Language implements Extractor.
func (goExtractor) Language() string { return "go" }

// Extensions implements Extractor.
func (goExtractor) Extensions() []string { return []string{".go"} }

// Extract implements Extractor. A file with syntax errors is kept if the
// parser still produced a partial AST: half a file's symbols beat none while
// someone is mid-edit.
func (goExtractor) Extract(relPath string, src []byte) (Extraction, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relPath, src, parser.SkipObjectResolution)
	if file == nil {
		return Extraction{}, err
	}

	out := Extraction{}
	for _, imp := range file.Imports {
		target, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil {
			target = strings.Trim(imp.Path.Value, `"`)
		}
		out.Edges = append(out.Edges, Edge{Kind: EdgeImport, From: relPath, To: target})
	}

	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			sym, calls := goFuncSymbol(fset, relPath, src, node)
			out.Symbols = append(out.Symbols, sym)
			out.Edges = append(out.Edges, calls...)
		case *ast.GenDecl:
			out.Symbols = append(out.Symbols, goGenSymbols(fset, relPath, src, node)...)
		}
	}
	return out, nil
}

func goFuncSymbol(fset *token.FileSet, relPath string, src []byte, decl *ast.FuncDecl) (Symbol, []Edge) {
	kind := SymbolFunction
	name := decl.Name.Name
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = SymbolMethod
		if recv := goTypeName(decl.Recv.List[0].Type); recv != "" {
			name = recv + "." + name
		}
	}
	sym := Symbol{
		ID:   symbolID(relPath, kind, name),
		Name: name,
		Kind: kind,
		File: relPath,
		Line: fset.Position(decl.Pos()).Line,
		Hash: hashBytes(nodeSource(fset, src, decl)),
	}
	return sym, goCallEdges(sym.ID, decl)
}

func goGenSymbols(fset *token.FileSet, relPath string, src []byte, decl *ast.GenDecl) []Symbol {
	var kind SymbolKind
	switch decl.Tok {
	case token.TYPE:
		kind = SymbolType
	case token.CONST:
		kind = SymbolConstant
	case token.VAR:
		kind = SymbolVariable
	default:
		return nil
	}

	symbols := make([]Symbol, 0, len(decl.Specs))
	for _, spec := range decl.Specs {
		var names []*ast.Ident
		var node ast.Node = spec
		switch typed := spec.(type) {
		case *ast.TypeSpec:
			names = []*ast.Ident{typed.Name}
		case *ast.ValueSpec:
			names = typed.Names
		default:
			continue
		}
		hash := hashBytes(nodeSource(fset, src, node))
		for _, ident := range names {
			if ident == nil || ident.Name == "_" {
				continue
			}
			symbols = append(symbols, Symbol{
				ID:   symbolID(relPath, kind, ident.Name),
				Name: ident.Name,
				Kind: kind,
				File: relPath,
				Line: fset.Position(ident.Pos()).Line,
				Hash: hash,
			})
		}
	}
	return symbols
}

// goCallEdges records one edge per distinct callee expression in a function
// body. Callees stay as written ("fmt.Println", "helper", "rcv.Method"):
// resolving them to a declaration needs type information this indexer does
// not build, and a truthful unresolved name is more useful than a guess.
func goCallEdges(from string, decl *ast.FuncDecl) []Edge {
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
	edges := make([]Edge, 0, len(names))
	for _, name := range names {
		edges = append(edges, Edge{Kind: EdgeCall, From: from, To: name})
	}
	return edges
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

// goTypeName renders a receiver type as a bare name, dropping pointer and
// generic decoration so *Store and Store[T] both read as "Store".
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
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

// nodeSource returns the exact bytes a node spans, which is what a symbol's
// hash is taken over. A node whose offsets fall outside src (only possible
// from a badly damaged partial parse) degrades to an empty slice rather than
// panicking.
func nodeSource(fset *token.FileSet, src []byte, node ast.Node) []byte {
	start := fset.Position(node.Pos()).Offset
	end := fset.Position(node.End()).Offset
	if start < 0 || end > len(src) || start >= end {
		return nil
	}
	return src[start:end]
}
