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

	// match is the selection working, kept unexported because it is how the
	// gate is applied and not part of what a caller is told.
	match matchProfile
}

// keepEligible applies the coordination gate to a scored set.
func keepEligible(scored []ScoredSymbol, best int) []ScoredSymbol {
	out := scored[:0]
	for _, s := range scored {
		if eligible(s.match, best) {
			out = append(out, s)
		}
	}
	return out
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
	terms := AllSpecific(normalizeTerms(req.Terms))
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
	best := 0

	for _, rel := range g.Paths() {
		entry := g.Files[rel]
		if entry.Role == RoleGenerated && !req.IncludeGenerated {
			continue
		}
		fileAnchored := anchorFiles[rel] || matchesAnyFile(rel, anchorFiles)
		pathScore := termScore(rel, terms.Specific) * pathTermWeight
		for _, sym := range entry.Symbols {
			out.ConsideredSymbols++
			score, reason, m := scoreSymbol(sym, fileAnchored, anchorSymbols, terms, pathScore)
			if score <= 0 {
				continue
			}
			if m.coverage() > best {
				best = m.coverage()
			}
			scored = append(scored, ScoredSymbol{Symbol: sym, Score: score, Reason: reason, match: m})
		}
		out.ConsideredEdges += len(entry.Edges)
	}

	scored = keepEligible(scored, best)
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

// TermSet splits a task's terms by how much they discriminate.
//
// Common terms are not discarded -- they are genuinely informative -- but they
// contribute a fraction of a specific term's weight, because a word that
// matches a tenth of the repository mostly says what kind of repository this
// is.
type TermSet struct {
	// Specific are the terms whose candidate read did not fill its cap.
	Specific []string
	// Common are the terms that did.
	Common []string
}

// Empty reports a request with nothing to match on.
func (t TermSet) Empty() bool { return len(t.Specific) == 0 && len(t.Common) == 0 }

// All returns every term, specific first.
func (t TermSet) All() []string { return append(append([]string(nil), t.Specific...), t.Common...) }

// AllSpecific is the set to use when discrimination cannot be measured -- an
// in-memory graph of a handful of files, where every term matches almost
// everything and the ratio would say nothing.
func AllSpecific(terms []string) TermSet { return TermSet{Specific: terms} }

// commonTermDiscount is how much a saturated term is worth against a specific
// one.
const commonTermDiscount = 0.35

// matchProfile is how well one symbol matches a task's terms.
//
// The field that carries the ranking is NameHits: how many DISTINCT terms
// appear in the symbol's name or its path. Measurement on AO's own checkout is
// what put it there. An objective like "the authorization path for cancelling a
// workflow" has six usable terms, and in a forty-thousand-symbol repository
// each of them individually matches hundreds of things -- `TestContextCancellation`
// matches "cancel", `auth_test.go` matches "authoriz", every third file matches
// "path". What almost nothing matches is TWO of them at once, and the handful
// that do are the answer.
//
// So the score is superlinear in NameHits, and the eligibility gate below asks
// for the best coverage available rather than for any coverage at all. That is
// coordination matching, and it is the cheapest thing that works without
// corpus statistics AO would have to scan the whole graph to compute.
type matchProfile struct {
	// NameHits counts distinct terms found in the name or the path.
	NameHits int
	// SummaryHits counts distinct terms found in the summary. Prose is a tie
	// break, never a selector: on a real repository a summary is a true
	// English sentence that happens to contain the word, and hundreds of them
	// do.
	SummaryHits int
	// CommonNameHits and CommonSummaryHits are the same counts for the terms
	// that did not discriminate.
	CommonNameHits    int
	CommonSummaryHits int
	// Terms is how many specific terms there were to match.
	Terms int
	// Anchored reports that the task named this symbol or the file it is
	// declared in. It bypasses the coordination gate entirely: being in a file
	// this work touches is a stronger claim than any word.
	Anchored bool
}

// profile measures one symbol against a term set.
func profile(sym Symbol, terms TermSet) matchProfile {
	haystack := sym.Name + " " + sym.File
	return matchProfile{
		NameHits:          termHits(haystack, terms.Specific),
		SummaryHits:       termHits(sym.Summary, terms.Specific),
		CommonNameHits:    termHits(haystack, terms.Common),
		CommonSummaryHits: termHits(sym.Summary, terms.Common),
		Terms:             len(terms.Specific),
	}
}

// coverage is the total number of distinct terms the name or path matched,
// specific and common alike. It is what the eligibility gate ranks on: a
// symbol whose name carries two of the objective's words is a different kind
// of match from one that carries a single common one.
func (m matchProfile) coverage() int { return m.NameHits + m.CommonNameHits }

// score renders the profile as a number.
func (m matchProfile) score() float64 {
	total := float64(m.Terms + 1)
	// Squared, so two terms in a name are worth four times one and not twice.
	specific := float64(m.NameHits*m.NameHits)*weightNameTerm/total +
		float64(m.SummaryHits)*weightSummaryTerm/total
	common := (float64(m.CommonNameHits*m.CommonNameHits)*weightNameTerm/total +
		float64(m.CommonSummaryHits)*weightSummaryTerm/total) * commonTermDiscount
	return specific + common
}

// scoreSymbol ranks one symbol against the request.
func scoreSymbol(
	sym Symbol, fileAnchored bool, anchorSymbols map[string]bool, terms TermSet, pathScore float64,
) (score float64, reason string, m matchProfile) {
	if anchorSymbols[sym.ID] || anchorSymbols[sym.Name] {
		return weightNamedSymbol, "named by the task", matchProfile{Anchored: true}
	}
	m = profile(sym, terms)
	m.Anchored = fileAnchored
	score = m.score() + pathScore
	if fileAnchored {
		score += weightAnchoredFile
	}

	switch {
	case fileAnchored:
		reason = "declared in a file this work touches"
	case m.NameHits > 1 || m.coverage() > 1:
		reason = fmt.Sprintf("its name carries %d of the objective's terms", m.coverage())
	case m.NameHits > 0:
		reason = "name matches the objective"
	case m.SummaryHits > 0:
		reason = "description matches the objective"
	case pathScore > 0:
		reason = "path matches the objective"
	default:
		reason = "loosely matches the objective"
	}

	if score <= 0 {
		return 0, "", m
	}
	switch sym.Kind {
	case SymbolEndpoint, SymbolTable, SymbolQuery, SymbolInterface:
		score += weightArchitectural
	}
	if sym.Exported {
		score += weightExported
	}
	return score, reason, m
}

// eligible applies the coordination gate: when some candidate's name carries
// several of the objective's terms, a candidate whose name carries one is not
// competing for the same budget.
//
// It is capped at two rather than at the best available, because three
// co-occurring terms is usually one file and a pack of one symbol is not
// useful. Anchored symbols bypass it entirely -- being in a file this work
// touches is a stronger claim than any word.
func eligible(m matchProfile, bestCoverage int) bool {
	if m.Anchored {
		return true
	}
	required := bestCoverage
	if required > 2 {
		required = 2
	}
	if required < 1 {
		required = 1
	}
	if m.coverage() >= required {
		return true
	}
	// Prose gets one way in: two specific terms in the same summary is a
	// coincidence worth taking seriously, one is not.
	return m.SummaryHits >= 2
}

// termScore counts how many distinct terms a haystack contains, normalised by
// the number of terms, so a two-term match out of two ranks above a two-term
// match out of eight.
func termScore(haystack string, terms []string) float64 {
	hits := termHits(haystack, terms)
	if hits == 0 {
		return 0
	}
	return float64(hits) / float64(len(terms))
}

// termHits counts the distinct terms a haystack contains.
func termHits(haystack string, terms []string) int {
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
	return hits
}

// normalizeTerms lowercases, de-duplicates, drops the words an objective is
// written WITH rather than about, and reduces the rest to a stem.
//
// Stemming is not decoration here; it is the difference between finding the
// code and not. An objective says "cancelling a workflow" and the code says
// `Cancel`; a substring match on the inflected word finds nothing at all,
// which is a silent, total failure rather than a degraded answer. Measurement
// on AO's own checkout is what made that concrete.
func normalizeTerms(raw []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, term := range raw {
		for _, word := range strings.FieldsFunc(strings.ToLower(term), func(r rune) bool {
			return r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}) {
			if len(word) < minTermLength || stopWords[word] {
				continue
			}
			stemmed := stem(word)
			if len(stemmed) < minTermLength || seen[stemmed] {
				continue
			}
			seen[stemmed] = true
			out = append(out, stemmed)
		}
	}
	return out
}

// stemSuffixes are stripped longest-first. The set is deliberately small and
// English-only: it covers the inflections an objective actually uses about
// code ("cancelling", "authorization", "queries", "permissions") and stops
// well short of a real stemmer, because an over-eager one turns "user" into
// "us" and matches everything.
var stemSuffixes = []string{
	"ations", "ation", "ements", "ement", "ings", "ing", "ions", "ion",
	"ies", "ied", "ers", "ed", "es", "s",
}

// minStemLength is the shortest a stem may be. Below it the suffix is kept:
// what is left would match too much to mean anything.
const minStemLength = 4

// stem reduces one word to the prefix its inflections share.
//
// The doubled-consonant collapse is what makes the common case work:
// "cancelling" loses "ing" to become "cancell", which is a substring of
// nothing, and collapsing the doubled `l` gives "cancel", which is a substring
// of Cancel, Cancelled and CancelRun alike.
func stem(word string) string {
	if len(word) <= minStemLength {
		return word
	}
	for _, suffix := range stemSuffixes {
		if !strings.HasSuffix(word, suffix) || len(word)-len(suffix) < minStemLength {
			continue
		}
		trimmed := word[:len(word)-len(suffix)]
		if n := len(trimmed); n >= 2 && trimmed[n-1] == trimmed[n-2] && !isVowel(trimmed[n-1]) {
			trimmed = trimmed[:n-1]
		}
		return trimmed
	}
	return word
}

func isVowel(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	default:
		return false
	}
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
	// The verbs and pronouns an instruction is phrased with. They are here
	// rather than left to the specificity gate below because they are noise in
	// EVERY repository, and a term that is noise everywhere should not have to
	// be discovered to be noise each time.
	"identify": true, "find": true, "show": true, "list": true, "which": true,
	"what": true, "where": true, "how": true, "why": true, "does": true, "need": true,
	"want": true, "respect": true, "respecting": true, "existing": true, "given": true,
	"without": true, "about": true, "over": true, "under": true, "only": true,
	"still": true, "than": true, "more": true, "most": true, "some": true, "each": true,
	"every": true, "both": true, "same": true, "other": true, "such": true, "very": true,
	"just": true, "here": true, "there": true, "they": true, "them": true, "their": true,
	"your": true, "you": true, "are": true, "was": true, "were": true, "been": true,
	"being": true, "have": true, "has": true, "had": true, "will": true, "would": true,
	"can": true, "could": true, "may": true, "might": true, "ensure": true, "keep": true,
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
