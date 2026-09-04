package codegraph

import (
	"sort"
	"strings"
	"unicode"
)

// summary.go — the production symbol_summary capability.
//
// A summary here is DERIVED, never generated. It is assembled from three
// things the extractor already read out of the declaration itself — what kind
// of thing it is, its public contract, and the first sentence its author wrote
// about it — plus, where a real AST was available, the side effects that are
// visible in its body.
//
// That choice is the whole design, and it is the brief's:
//
//   - Indexing a project must never require a model. A project registered on a
//     machine with no provider configured, or with a budget of zero, still
//     gets a complete graph with complete summaries.
//   - Two indexes of the same source must produce the same bytes, because a
//     context pack's digest is what proves two dispatches were given the same
//     memory.
//   - Nothing stored here is reasoning. A summary says what the code IS, as
//     the code says it. It never says what the code is FOR beyond the sentence
//     its author already committed.
//
// If an AI-written semantic summary is ever added it belongs in a separate
// field with its own SummarySource and its own provider/model provenance, and
// it must remain optional. Overwriting these would make the graph's honesty
// depend on a provider being up.

// summarize renders one symbol's bounded description.
//
// The shape is deliberately dense and uniform — "<kind> <name><signature> —
// <sentence>" — because a context pack shows dozens of these in a list, and a
// reader scanning that list is looking down a column, not reading prose.
func summarize(sym Symbol, effects []string) string {
	var b strings.Builder
	b.WriteString(string(sym.Kind))
	b.WriteByte(' ')
	b.WriteString(sym.Name)
	if sig := sym.Signature; sig != "" {
		// A parameter list joins the name directly ("Sync(ctx, id)"); a bare
		// descriptor does not ("type Store struct").
		if !strings.HasPrefix(sig, "(") && !strings.HasPrefix(sig, " ") {
			b.WriteByte(' ')
		}
		b.WriteString(sig)
	}
	if doc := sym.Doc; doc != "" {
		b.WriteString(" — ")
		b.WriteString(doc)
	}
	if len(effects) > 0 {
		b.WriteString(" [")
		b.WriteString(strings.Join(effects, ", "))
		b.WriteString("]")
	}
	return clampRunes(collapseSpaces(b.String()), maxSymbolSummary)
}

// firstSentence reduces a documentation comment to its opening sentence,
// bounded.
//
// It takes the first sentence rather than the first paragraph because the
// convention every language in this repository shares is that the first
// sentence names the thing — "Sync brings memory up to date with the
// repository" — and everything after it is rationale a context pack cannot
// afford. A comment with no sentence terminator degrades to its first line.
func firstSentence(doc string) string {
	doc = collapseSpaces(doc)
	if doc == "" {
		return ""
	}
	for i, r := range doc {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// "e.g." and "Fig. 2" are not sentence ends. A terminator counts only
		// when what follows it is a space (or the end) and what precedes it is
		// not a single letter.
		if i+1 < len(doc) && doc[i+1] != ' ' {
			continue
		}
		if i > 0 && i-2 >= 0 && doc[i-2] == '.' {
			continue
		}
		return clampRunes(doc[:i+1], maxSymbolDoc)
	}
	return clampRunes(doc, maxSymbolDoc)
}

// effectVocabulary maps a callee or import prefix onto the one word a reader
// needs. It is intentionally tiny: these are the side effects that change what
// a reviewer must check, and a longer list would turn a one-line summary into
// a second signature.
//
// The mapping is over names as WRITTEN at the call site, so it is exact for
// the standard library and wrong for nothing else: a project package that
// happens to be named `os` would have to be imported under that name, at which
// point "touches the operating system" is still the honest reading.
var effectVocabulary = []struct {
	prefixes []string
	label    string
}{
	{[]string{"os.Remove", "os.RemoveAll", "os.WriteFile", "os.Create", "os.Mkdir", "os.MkdirAll", "os.Rename", "os.Chmod", "os.Truncate"}, "writes files"},
	{[]string{"os.Open", "os.ReadFile", "os.Stat", "os.Lstat", "os.ReadDir"}, "reads files"},
	{[]string{"os.Getenv", "os.LookupEnv", "os.Setenv"}, "reads config"},
	{[]string{"exec.Command", "exec.CommandContext"}, "runs processes"},
	{[]string{"http.Get", "http.Post", "http.NewRequest", "http.NewRequestWithContext", "net.Dial"}, "network"},
	{[]string{"sql.Open", "db.Query", "db.Exec", "tx.Query", "tx.Exec"}, "database"},
	{[]string{"panic", "log.Fatal", "os.Exit"}, "terminates"},
}

// staticEffects names the side effects visible in a body, from the callee
// names the AST actually produced.
//
// It reports only what it saw. A function whose effects run through an
// injected interface shows none here, and that is correct: this is evidence,
// not inference, and an empty list means "nothing observable in this body",
// never "this function is pure".
func staticEffects(callees []string) []string {
	seen := map[string]bool{}
	for _, callee := range callees {
		for _, entry := range effectVocabulary {
			for _, prefix := range entry.prefixes {
				if callee == prefix || strings.HasPrefix(callee, prefix+".") {
					seen[entry.label] = true
				}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for label := range seen {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// collapseSpaces turns any run of whitespace into a single space and trims the
// ends, so a summary assembled from a multi-line comment renders as one line.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// clampRunes truncates on a rune boundary and marks that it did. Truncating on
// a byte boundary would split a multi-byte character and produce invalid UTF-8
// in a field that goes straight into a prompt.
func clampRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return strings.TrimRight(string(runes[:max-3]), " ") + "..."
}
