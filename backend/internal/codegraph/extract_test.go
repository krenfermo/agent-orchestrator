package codegraph

import (
	"testing"
)

func findSymbol(symbols []Symbol, name string) (Symbol, bool) {
	for _, sym := range symbols {
		if sym.Name == name {
			return sym, true
		}
	}
	return Symbol{}, false
}

func TestGoExtractorRecordsSymbolsImportsAndCalls(t *testing.T) {
	extraction, err := goExtractor{}.Extract("main.go", []byte(mainGo))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	greet, ok := findSymbol(extraction.Symbols, "Greeter.Greet")
	if !ok {
		t.Fatalf("method symbol missing from %+v", extraction.Symbols)
	}
	if greet.Kind != SymbolMethod || greet.File != "main.go" || greet.Line == 0 || greet.Hash == "" {
		t.Fatalf("method symbol = %+v", greet)
	}
	if _, ok := findSymbol(extraction.Symbols, "Greeter"); !ok {
		t.Fatal("type symbol missing")
	}
	if _, ok := findSymbol(extraction.Symbols, "DefaultPrefix"); !ok {
		t.Fatal("constant symbol missing")
	}

	for _, want := range []Edge{
		{Kind: EdgeImport, From: "main.go", To: "fmt"},
		{Kind: EdgeImport, From: "main.go", To: "strings"},
		{Kind: EdgeCall, From: "main.go#method:Greeter.Greet", To: "fmt.Sprintf"},
		{Kind: EdgeCall, From: "main.go#function:Run", To: "g.Greet"},
	} {
		if !hasEdge(extraction.Edges, want) {
			t.Fatalf("edge %+v missing from %+v", want, extraction.Edges)
		}
	}
}

func TestGoExtractorHashesSymbolsIndependently(t *testing.T) {
	before, err := goExtractor{}.Extract("helper.go", []byte("package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	after, err := goExtractor{}.Extract("helper.go", []byte("package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 3 }\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	beforeA, _ := findSymbol(before.Symbols, "A")
	afterA, _ := findSymbol(after.Symbols, "A")
	beforeB, _ := findSymbol(before.Symbols, "B")
	afterB, _ := findSymbol(after.Symbols, "B")

	if beforeA.Hash != afterA.Hash {
		t.Fatal("untouched symbol's hash changed")
	}
	if beforeB.Hash == afterB.Hash {
		t.Fatal("edited symbol's hash did not change")
	}
}

func TestGoExtractorKeepsWhatItCanFromABrokenFile(t *testing.T) {
	extraction, err := goExtractor{}.Extract("broken.go", []byte("package a\n\nimport \"fmt\"\n\nfunc Good() { fmt.Println(1) }\n\nfunc Bad( {\n"))
	if err != nil {
		t.Fatalf("Extract on a partial parse returned an error: %v", err)
	}
	if _, ok := findSymbol(extraction.Symbols, "Good"); !ok {
		t.Fatalf("symbols before the syntax error were dropped: %+v", extraction.Symbols)
	}
}

func TestScanExtractorReadsTypeScript(t *testing.T) {
	extractor := newScanExtractor("typescript", []string{".ts"}, tsPatterns)
	extraction, err := extractor.Extract("web/app.ts", []byte(appTS))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	widget, ok := findSymbol(extraction.Symbols, "Widget")
	if !ok || widget.Kind != SymbolFunction {
		t.Fatalf("arrow-function const not recorded as a function: %+v", extraction.Symbols)
	}
	props, ok := findSymbol(extraction.Symbols, "Props")
	if !ok || props.Kind != SymbolType {
		t.Fatalf("interface not recorded as a type: %+v", extraction.Symbols)
	}
	for _, want := range []Edge{
		{Kind: EdgeImport, From: "web/app.ts", To: "react"},
		{Kind: EdgeImport, From: "web/app.ts", To: "./helper"},
	} {
		if !hasEdge(extraction.Edges, want) {
			t.Fatalf("edge %+v missing from %+v", want, extraction.Edges)
		}
	}
}

func TestScanExtractorReadsJavaScriptRequires(t *testing.T) {
	extractor := newScanExtractor("javascript", []string{".js"}, tsPatterns)
	extraction, err := extractor.Extract("lib.js", []byte("const path = require(\"node:path\");\n\nexport function join(a, b) { return path.join(a, b); }\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !hasEdge(extraction.Edges, Edge{Kind: EdgeImport, From: "lib.js", To: "node:path"}) {
		t.Fatalf("require() edge missing from %+v", extraction.Edges)
	}
	if _, ok := findSymbol(extraction.Symbols, "join"); !ok {
		t.Fatalf("exported function missing from %+v", extraction.Symbols)
	}
}

func TestScanExtractorReadsPython(t *testing.T) {
	extractor := newScanExtractor("python", []string{".py"}, pyPatterns)
	src := "from os import path\nimport json\n\nMAX_SIZE = 10\n\nclass Store:\n    def save(self, item):\n        return json.dumps(item)\n\ndef load(raw):\n    return path.abspath(raw)\n"
	extraction, err := extractor.Extract("store.py", []byte(src))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if sym, ok := findSymbol(extraction.Symbols, "save"); !ok || sym.Kind != SymbolMethod {
		t.Fatalf("indented def not recorded as a method: %+v", extraction.Symbols)
	}
	if sym, ok := findSymbol(extraction.Symbols, "load"); !ok || sym.Kind != SymbolFunction {
		t.Fatalf("module-level def not recorded as a function: %+v", extraction.Symbols)
	}
	if sym, ok := findSymbol(extraction.Symbols, "Store"); !ok || sym.Kind != SymbolType {
		t.Fatalf("class not recorded as a type: %+v", extraction.Symbols)
	}
	if sym, ok := findSymbol(extraction.Symbols, "MAX_SIZE"); !ok || sym.Kind != SymbolConstant {
		t.Fatalf("module constant missing: %+v", extraction.Symbols)
	}
	for _, want := range []Edge{
		{Kind: EdgeImport, From: "store.py", To: "os"},
		{Kind: EdgeImport, From: "store.py", To: "json"},
	} {
		if !hasEdge(extraction.Edges, want) {
			t.Fatalf("edge %+v missing from %+v", want, extraction.Edges)
		}
	}
}

func TestExtractorSetDispatchesByExtension(t *testing.T) {
	set := newExtractorSet(DefaultExtractors())
	cases := map[string]string{
		"a/b.go":   "go",
		"a/b.ts":   "typescript",
		"a/b.TSX":  "typescript",
		"a/b.mjs":  "javascript",
		"a/b.py":   "python",
		"a/b.rb":   "",
		"README":   "",
		"a/b.json": "",
	}
	for path, want := range cases {
		extractor, ok := set.find(path)
		if want == "" {
			if ok {
				t.Fatalf("find(%q) matched %q, want no extractor", path, extractor.Language())
			}
			continue
		}
		if !ok || extractor.Language() != want {
			t.Fatalf("find(%q) = %v/%v, want %q", path, extractor, ok, want)
		}
	}
}
