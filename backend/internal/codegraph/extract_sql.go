package codegraph

import (
	"regexp"
	"sort"
	"strings"
)

// extract_sql.go — schema and named queries.
//
// SQL earns a place in the code graph because it is where the persistence half
// of an architecture is actually written down. An agent asked to "add export
// permissions to the Supervisor role" needs to reach the table the permission
// lands in and the query that writes it, and neither is reachable from Go
// source alone: in this repository the store layer calls a generated method
// whose SQL lives in a `.sql` file the Go extractor never sees.
//
// What is read here is deliberately shallow and provable:
//
//   - a `CREATE TABLE x` in a migration declares a table;
//   - a sqlc `-- name: X :one` block declares a named query;
//   - the table names a statement mentions after FROM / JOIN / INTO / UPDATE /
//     DELETE FROM decide whether that query reads or writes.
//
// There is no attempt at SQL semantic analysis beyond that, per section 31 of
// the brief. A CTE, a subquery alias and a view are all recorded as the table
// name they are spelled with, and a statement AO cannot classify contributes
// no edge rather than a guessed one.

// sqlExtractor reads .sql files: migrations and named-query files.
type sqlExtractor struct{}

// Language implements Extractor.
func (sqlExtractor) Language() string { return "sql" }

// Extensions implements Extractor.
func (sqlExtractor) Extensions() []string { return []string{".sql"} }

var (
	sqlNamedQuery = regexp.MustCompile(`(?i)^\s*--\s*name:\s*([A-Za-z_]\w*)\s*(:\w+)?`)
	sqlCreate     = regexp.MustCompile(`(?is)\bCREATE\s+(?:TEMP\s+|TEMPORARY\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + sqlIdent)
	sqlCreateIdx  = regexp.MustCompile(`(?is)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?` + sqlIdent + `\s+ON\s+` + sqlIdent)
	sqlAlter      = regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+` + sqlIdent)
	sqlFrom       = regexp.MustCompile(`(?is)\b(?:FROM|JOIN)\s+` + sqlIdent)
	sqlInsert     = regexp.MustCompile(`(?is)\bINSERT\s+(?:OR\s+\w+\s+)?INTO\s+` + sqlIdent)
	sqlUpdate     = regexp.MustCompile(`(?is)\bUPDATE\s+(?:OR\s+\w+\s+)?` + sqlIdent)
	sqlDelete     = regexp.MustCompile(`(?is)\bDELETE\s+FROM\s+` + sqlIdent)
)

// sqlIdent matches a table identifier: bare, quoted, or schema-qualified.
const sqlIdent = `["` + "`" + `\[]?([A-Za-z_][\w$]*(?:\.[A-Za-z_][\w$]*)?)["` + "`" + `\]]?`

// sqlNoise are words that follow FROM or UPDATE without naming a table.
var sqlNoise = map[string]bool{
	"select": true, "values": true, "set": true, "where": true, "dual": true,
}

// Extract implements Extractor.
func (sqlExtractor) Extract(relPath string, src []byte) (Extraction, error) {
	text := string(src)
	migration := ClassifyFile(relPath, src) == RoleMigration
	out := Extraction{}

	// Tables first, so a migration that both creates and reads a table has the
	// declaration on record before the edges that mention it.
	seenTable := map[string]bool{}
	declare := func(name string, line int) {
		name = strings.ToLower(name)
		if name == "" || seenTable[name] {
			return
		}
		seenTable[name] = true
		sym := Symbol{
			ID: symbolID(relPath, SymbolTable, name), Name: name, Kind: SymbolTable,
			File: relPath, Line: line, Exported: true, SummarySource: SummaryStatic,
			Hash: hashBytes([]byte(name)),
		}
		sym.Summary = summarize(sym, nil)
		out.Symbols = append(out.Symbols, sym)
	}
	for _, m := range sqlCreate.FindAllStringSubmatchIndex(text, -1) {
		declare(text[m[2]:m[3]], lineAt(text, m[0]))
	}
	for _, m := range sqlAlter.FindAllStringSubmatchIndex(text, -1) {
		declare(text[m[2]:m[3]], lineAt(text, m[0]))
	}

	// A migration is one statement block that belongs to the file itself: its
	// edges start at the file, because there is no named query to hang them
	// on. A query file is a sequence of named blocks, and each block's edges
	// start at its own symbol.
	if migration {
		out.Edges = append(out.Edges, sqlTableEdges(relPath, text, 0)...)
		for _, m := range sqlCreateIdx.FindAllStringSubmatchIndex(text, -1) {
			out.Edges = append(out.Edges, Edge{
				Kind: EdgeWritesTo, From: relPath,
				To: strings.ToLower(text[m[4]:m[5]]), Line: lineAt(text, m[0]),
			})
		}
		return dedupeExtraction(out), nil
	}

	for _, block := range sqlBlocks(text) {
		sym := Symbol{
			ID:            symbolID(relPath, SymbolQuery, block.name),
			Name:          block.name,
			Kind:          SymbolQuery,
			File:          relPath,
			Line:          block.line,
			EndLine:       block.endLine,
			Signature:     block.annotation,
			Doc:           firstSentence(block.doc),
			Exported:      true,
			SummarySource: SummaryStatic,
			Hash:          hashBytes([]byte(block.body)),
		}
		sym.Summary = summarize(sym, nil)
		out.Symbols = append(out.Symbols, sym)
		out.Edges = append(out.Edges, sqlTableEdges(sym.ID, block.body, block.line-1)...)
	}
	return dedupeExtraction(out), nil
}

// sqlBlock is one named-query block.
type sqlBlock struct {
	name       string
	annotation string
	doc        string
	body       string
	line       int
	endLine    int
}

// sqlBlocks splits a query file on its `-- name:` markers.
func sqlBlocks(text string) []sqlBlock {
	lines := strings.Split(text, "\n")
	var blocks []sqlBlock
	current := -1
	for i, line := range lines {
		m := sqlNamedQuery.FindStringSubmatch(line)
		if m != nil {
			blocks = append(blocks, sqlBlock{
				name:       m[1],
				annotation: strings.TrimSpace(m[2]),
				line:       i + 1,
				endLine:    i + 1,
			})
			current = len(blocks) - 1
			continue
		}
		if current < 0 {
			continue
		}
		blocks[current].endLine = i + 1
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			if blocks[current].body == "" {
				text := strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))
				if blocks[current].doc != "" {
					blocks[current].doc += " "
				}
				blocks[current].doc += text
			}
			continue
		}
		blocks[current].body += line + "\n"
	}
	return blocks
}

// sqlTableEdges classifies one statement body's table references.
func sqlTableEdges(from, body string, lineOffset int) []Edge {
	var edges []Edge
	add := func(kind EdgeKind, matches [][]int) {
		for _, m := range matches {
			name := strings.ToLower(body[m[2]:m[3]])
			if name == "" || sqlNoise[name] {
				continue
			}
			edges = append(edges, Edge{Kind: kind, From: from, To: name, Line: lineOffset + lineAt(body, m[0])})
		}
	}
	add(EdgeReadsFrom, sqlFrom.FindAllStringSubmatchIndex(body, -1))
	add(EdgeWritesTo, sqlInsert.FindAllStringSubmatchIndex(body, -1))
	add(EdgeWritesTo, sqlUpdate.FindAllStringSubmatchIndex(body, -1))
	add(EdgeWritesTo, sqlDelete.FindAllStringSubmatchIndex(body, -1))
	add(EdgeWritesTo, sqlCreate.FindAllStringSubmatchIndex(body, -1))
	return edges
}

// lineAt returns the 1-based line an offset falls on.
func lineAt(text string, offset int) int {
	if offset < 0 || offset > len(text) {
		return 0
	}
	return strings.Count(text[:offset], "\n") + 1
}

// dedupeExtraction collapses repeats and imposes a deterministic order. A
// statement mentioning one table three times is one edge.
func dedupeExtraction(in Extraction) Extraction {
	in.Symbols = dedupeSymbols(in.Symbols)
	seen := map[string]bool{}
	edges := in.Edges[:0:0]
	for _, edge := range in.Edges {
		key := string(edge.Kind) + "\x00" + edge.From + "\x00" + edge.To
		if seen[key] {
			continue
		}
		seen[key] = true
		edges = append(edges, edge)
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		return edges[i].To < edges[j].To
	})
	in.Edges = edges
	return in
}
