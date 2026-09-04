package codegraph

import (
	"fmt"
	"sort"
	"strings"
)

// retrieve.go — task-to-graph retrieval: sections 13 and 14 of the brief.
//
// The question this file answers is the one the whole phase exists for. Given
// an objective like "add export permissions to the Supervisor role", find the
// permission evaluator, the role definition, the handler, the authorization
// call and the tests that cover them -- and find them WITHOUT sending the
// repository.
//
// Two rules shape it.
//
// First, retrieval is hybrid and says so. Term matching over symbol names,
// paths and summaries is what finds the anchors; the graph is what expands an
// anchor into the neighbourhood that actually matters -- its callers, the
// tests that exercise it, the tables it writes, the route that reaches it.
// Neither half is sufficient. Term matching alone returns a keyword grep;
// graph traversal alone has nowhere to start. The brief's warning applies
// directly: this does not pretend graph retrieval solves semantic search.
//
// Second, everything is bounded before it is returned, and the counts of what
// was CONSIDERED versus what was SELECTED come back with the result. A
// retrieval that cannot say how much it looked at cannot be measured, and P3-E
// has to be able to measure it.

// Retrieval bounds.
const (
	// DefaultRetrievalSymbols is how many symbols a neighbourhood carries when
	// the caller does not say. It is small because these go into a context
	// pack alongside the task, the diff and the agent's own reads.
	DefaultRetrievalSymbols = 24
	// MaxRetrievalSymbols is the hard cap.
	MaxRetrievalSymbols = 200
	// DefaultRetrievalEdges bounds the relations returned.
	DefaultRetrievalEdges = 64
	// MaxRetrievalEdges is the hard cap on those.
	MaxRetrievalEdges = 512
	// maxTermsConsidered bounds how many task terms are honoured, so a pasted
	// paragraph cannot turn one retrieval into a full-text scan.
	maxTermsConsidered = 24
	// minTermLength is the shortest term worth matching. Below it a term
	// matches everything, which is the same as matching nothing.
	minTermLength = 3
)

// RetrieveRequest asks for the bounded graph evidence relevant to one piece of
// work.
type RetrieveRequest struct {
	// Files are project-relative paths already known to be involved -- the
	// diff, the task's stated targets. They are the strongest anchor there is.
	Files []string
	// Symbols are symbol names or IDs already known to be involved.
	Symbols []string
	// Terms are free-text words from the objective. They are the weakest
	// anchor and are used to find a starting point when Files and Symbols are
	// empty or thin.
	Terms []string
	// MaxSymbols and MaxEdges bound the result. Zero means the default.
	MaxSymbols int
	MaxEdges   int
	// IncludeGenerated admits generated files. It is off by default: a
	// generated client has one symbol per API operation, and admitting it
	// would spend the whole budget restating the schema.
	IncludeGenerated bool
}

// Normalized clamps a request to its bounds.
func (r RetrieveRequest) Normalized() RetrieveRequest {
	if r.MaxSymbols <= 0 {
		r.MaxSymbols = DefaultRetrievalSymbols
	}
	if r.MaxSymbols > MaxRetrievalSymbols {
		r.MaxSymbols = MaxRetrievalSymbols
	}
	if r.MaxEdges <= 0 {
		r.MaxEdges = DefaultRetrievalEdges
	}
	if r.MaxEdges > MaxRetrievalEdges {
		r.MaxEdges = MaxRetrievalEdges
	}
	if len(r.Terms) > maxTermsConsidered {
		r.Terms = r.Terms[:maxTermsConsidered]
	}
	return r
}

// ScoredSymbol is one symbol the retrieval selected, with why.
type ScoredSymbol struct {
	Symbol Symbol `json:"symbol"`
	// Score is the relevance score, in the order the retrieval ranked by.
	Score float64 `json:"score"`
	// Reason names the strongest signal that selected it, so an operator can
	// ask why a pack contains what it contains.
	Reason string `json:"reason"`
}

// Neighborhood is the bounded graph evidence for one piece of work.
type Neighborhood struct {
	// Symbols are the selected symbols, most relevant first.
	Symbols []ScoredSymbol `json:"symbols,omitempty"`
	// Callers are edges reaching INTO the selected symbols: who would be
	// affected by changing one.
	Callers []Edge `json:"callers,omitempty"`
	// Callees are edges leaving them: what they depend on.
	Callees []Edge `json:"callees,omitempty"`
	// Tests are the test symbols proven to exercise a selected symbol.
	Tests []Symbol `json:"tests,omitempty"`
	// Endpoints are HTTP routes that reach a selected symbol.
	Endpoints []Symbol `json:"endpoints,omitempty"`
	// Tables are the database tables a selected symbol reads or writes.
	Tables []string `json:"tables,omitempty"`
	// Files are the project-relative paths the evidence came from.
	Files []string `json:"files,omitempty"`
	// ConsideredSymbols and ConsideredEdges count everything the retrieval was
	// allowed to choose from, before the bounds. With the selected counts they
	// are what makes the graph's contribution measurable rather than asserted.
	ConsideredSymbols int `json:"consideredSymbols"`
	ConsideredEdges   int `json:"consideredEdges"`
	// Truncated reports that a bound was hit.
	Truncated bool `json:"truncated,omitempty"`
}

// Empty reports whether the retrieval found nothing.
func (n Neighborhood) Empty() bool { return len(n.Symbols) == 0 }

// SelectedSymbols and SelectedEdges are what the neighbourhood carries.
func (n Neighborhood) SelectedSymbols() int { return len(n.Symbols) }

// SelectedEdges counts every relation in the result.
func (n Neighborhood) SelectedEdges() int { return len(n.Callers) + len(n.Callees) }

// Retrieve answers a RetrieveRequest against the graph. It never mutates.
func (g *Graph) Retrieve(req RetrieveRequest) Neighborhood {
	req = req.Normalized()
	terms := normalizeTerms(req.Terms)
	anchorFiles := map[string]bool{}
	for _, f := range req.Files {
		if rel := normalizeRel(f); rel != "" {
			anchorFiles[rel] = true
		}
	}
	anchorSymbols := map[string]bool{}
	for _, s := range req.Symbols {
		if s = strings.TrimSpace(s); s != "" {
			anchorSymbols[s] = true
		}
	}

	out := Neighborhood{}
	scored := make([]ScoredSymbol, 0, 64)

	for _, rel := range g.Paths() {
		entry := g.Files[rel]
		if entry.Role == RoleGenerated && !req.IncludeGenerated {
			continue
		}
		fileAnchored := anchorFiles[rel] || matchesAnyFile(rel, anchorFiles)
		pathScore := termScore(rel, terms) * pathTermWeight
		for _, sym := range entry.Symbols {
			out.ConsideredSymbols++
			score, reason := scoreSymbol(sym, fileAnchored, anchorSymbols, terms, pathScore)
			if score <= 0 {
				continue
			}
			scored = append(scored, ScoredSymbol{Symbol: sym, Score: score, Reason: reason})
		}
		out.ConsideredEdges += len(entry.Edges)
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		// The symbol ID is the final tiebreak so a retrieval can never be
		// decided by map iteration order: an unstable pack digest would make
		// every dispatch look like a changed premise.
		return scored[i].Symbol.ID < scored[j].Symbol.ID
	})
	if len(scored) > req.MaxSymbols {
		scored = scored[:req.MaxSymbols]
		out.Truncated = true
	}
	out.Symbols = scored
	if len(scored) == 0 {
		return out
	}

	g.expand(&out, req)
	return out
}

// expand fills in the relations around the selected symbols.
func (g *Graph) expand(out *Neighborhood, req RetrieveRequest) {
	selectedIDs := make(map[string]bool, len(out.Symbols))
	selectedNames := make(map[string]bool, len(out.Symbols)*2)
	files := map[string]bool{}
	for _, s := range out.Symbols {
		selectedIDs[s.Symbol.ID] = true
		selectedNames[s.Symbol.Name] = true
		if _, member, ok := strings.Cut(s.Symbol.Name, "."); ok {
			selectedNames[member] = true
		}
		files[s.Symbol.File] = true
	}

	tables := map[string]bool{}
	testIDs := map[string]bool{}
	endpointIDs := map[string]bool{}
	var tests, endpoints []Symbol

	for _, rel := range g.Paths() {
		entry := g.Files[rel]
		for _, edge := range entry.Edges {
			outgoing := selectedIDs[edge.From]
			incoming := selectedNames[edge.To] || selectedIDs[edge.To]
			if !outgoing && !incoming {
				continue
			}
			switch {
			case outgoing:
				out.Callees = append(out.Callees, edge)
				if edge.Kind == EdgeReadsFrom || edge.Kind == EdgeWritesTo {
					tables[edge.To] = true
				}
			default:
				out.Callers = append(out.Callers, edge)
				files[rel] = true
			}
			if !incoming {
				continue
			}
			switch edge.Kind {
			case EdgeTests:
				if sym, ok := symbolByID(entry, edge.From); ok && !testIDs[sym.ID] {
					testIDs[sym.ID] = true
					tests = append(tests, sym)
				}
			case EdgeRoutesTo:
				if sym, ok := symbolByID(entry, edge.From); ok && !endpointIDs[sym.ID] {
					endpointIDs[sym.ID] = true
					endpoints = append(endpoints, sym)
				}
			}
		}
	}

	sortEdges(out.Callers)
	sortEdges(out.Callees)
	if len(out.Callers) > req.MaxEdges {
		out.Callers = out.Callers[:req.MaxEdges]
		out.Truncated = true
	}
	if len(out.Callees) > req.MaxEdges {
		out.Callees = out.Callees[:req.MaxEdges]
		out.Truncated = true
	}
	sortSymbols(tests)
	sortSymbols(endpoints)
	out.Tests = tests
	out.Endpoints = endpoints
	out.Tables = sortedKeys(tables)
	out.Files = sortedKeys(files)
}

// Scoring weights. They are constants rather than tuning knobs because the
// order they impose is the retrieval's contract: an anchored file beats a name
// match, a name match beats a summary match, and nothing beats being named
// outright.
const (
	weightNamedSymbol  = 100.0
	weightAnchoredFile = 40.0
	weightNameTerm     = 12.0
	weightSummaryTerm  = 3.0
	weightExported     = 1.5
	pathTermWeight     = 6.0
	// weightArchitectural lifts the symbols that ARE the architecture --
	// endpoints, tables, queries, interfaces -- when they match at all. They
	// are the nodes an agent most needs and the ones a plain name match is
	// least likely to surface, because their names are patterns and table
	// names rather than identifiers.
	weightArchitectural = 8.0
)

// scoreSymbol ranks one symbol against the request.
func scoreSymbol(sym Symbol, fileAnchored bool, anchorSymbols map[string]bool, terms []string, pathScore float64) (float64, string) {
	if anchorSymbols[sym.ID] || anchorSymbols[sym.Name] {
		return weightNamedSymbol, "named by the task"
	}
	score := 0.0
	reason := ""
	if fileAnchored {
		score += weightAnchoredFile
		reason = "declared in a file this work touches"
	}
	if nameScore := termScore(sym.Name, terms); nameScore > 0 {
		score += nameScore * weightNameTerm
		if reason == "" {
			reason = "name matches the objective"
		}
	}
	if docScore := termScore(sym.Summary, terms); docScore > 0 {
		score += docScore * weightSummaryTerm
		if reason == "" {
			reason = "description matches the objective"
		}
	}
	if pathScore > 0 {
		score += pathScore
		if reason == "" {
			reason = "path matches the objective"
		}
	}
	if score <= 0 {
		return 0, ""
	}
	switch sym.Kind {
	case SymbolEndpoint, SymbolTable, SymbolQuery, SymbolInterface:
		score += weightArchitectural
	}
	if sym.Exported {
		score += weightExported
	}
	return score, reason
}

// termScore counts how many distinct terms a haystack contains, normalised by
// the number of terms, so a two-term match out of two ranks above a two-term
// match out of eight.
func termScore(haystack string, terms []string) float64 {
	if len(terms) == 0 || haystack == "" {
		return 0
	}
	lowered := strings.ToLower(haystack)
	hits := 0
	for _, term := range terms {
		if strings.Contains(lowered, term) {
			hits++
		}
	}
	if hits == 0 {
		return 0
	}
	return float64(hits) / float64(len(terms))
}

// normalizeTerms lowercases, de-duplicates and drops the terms too short or
// too common to discriminate.
func normalizeTerms(raw []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, term := range raw {
		for _, word := range strings.FieldsFunc(strings.ToLower(term), func(r rune) bool {
			return !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		}) {
			if len(word) < minTermLength || stopWords[word] || seen[word] {
				continue
			}
			seen[word] = true
			out = append(out, word)
		}
	}
	return out
}

// stopWords are the words an objective is written with rather than about.
// Leaving them in would score every symbol in the repository equally, which is
// the same as not scoring at all.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true,
	"this": true, "into": true, "when": true, "then": true, "should": true, "must": true,
	"add": true, "fix": true, "make": true, "use": true, "using": true, "new": true,
	"not": true, "but": true, "all": true, "any": true, "our": true, "its": true,
	"code": true, "file": true, "files": true, "please": true, "also": true,
}

func matchesAnyFile(rel string, anchors map[string]bool) bool {
	for anchor := range anchors {
		if matchesFile(rel, anchor) {
			return true
		}
	}
	return false
}

func symbolByID(entry FileEntry, id string) (Symbol, bool) {
	for _, sym := range entry.Symbols {
		if sym.ID == id {
			return sym, true
		}
	}
	return Symbol{}, false
}

func sortSymbols(symbols []Symbol) {
	sort.Slice(symbols, func(i, j int) bool { return symbols[i].ID < symbols[j].ID })
}

// Render writes the neighbourhood as the compact text a context pack carries.
//
// The shape is a list of facts, one per line, each naming where it came from.
// It is not prose: an agent reading this is looking for a path and a name, and
// a paragraph would bury both.
func (n Neighborhood) Render() string {
	if n.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant code (from the project's code graph):\n")
	for _, s := range n.Symbols {
		fmt.Fprintf(&b, "  %s:%d %s\n", s.Symbol.File, s.Symbol.Line, s.Symbol.Summary)
	}
	if len(n.Callers) > 0 {
		b.WriteString("Reached by:\n")
		for _, edge := range n.Callers {
			fmt.Fprintf(&b, "  %s %s %s\n", edge.From, edge.Kind, edge.To)
		}
	}
	if len(n.Tests) > 0 {
		b.WriteString("Covered by:\n")
		for _, test := range n.Tests {
			fmt.Fprintf(&b, "  %s:%d %s\n", test.File, test.Line, test.Name)
		}
	}
	if len(n.Endpoints) > 0 {
		b.WriteString("Reached from HTTP:\n")
		for _, ep := range n.Endpoints {
			fmt.Fprintf(&b, "  %s (%s)\n", ep.Name, ep.File)
		}
	}
	if len(n.Tables) > 0 {
		fmt.Fprintf(&b, "Tables touched: %s\n", strings.Join(n.Tables, ", "))
	}
	if n.Truncated {
		b.WriteString("(bounded; the graph holds more)\n")
	}
	return b.String()
}
