package codegraph

import (
	"fmt"
	"sort"
	"strings"
)

// architecture.go — the bounded, project-level structural summary.
//
// This is section 12 of the brief, and its constraint is the whole design: it
// is NOT a generated README. It is a few hundred bytes of structure a Planner
// can be handed at the top of a context pack so that it splits work along real
// module boundaries instead of guessing them from filenames.
//
// Everything in it is counted, not described. "backend/internal/projectmemory
// -- 41 files, 612 symbols, imported by 9 modules" is a fact; "the memory
// subsystem is responsible for durable knowledge" is a sentence somebody would
// have had to write, and the moment AO writes one of those it is claiming
// understanding it cannot defend.

// Architecture bounds. They are the reason this can go in every Planner pack.
const (
	// MaxArchitectureModules caps the named modules. Twelve is enough to
	// describe a layered backend plus a frontend, and few enough to read.
	MaxArchitectureModules = 12
	// MaxArchitectureEntryPoints caps the listed entry points.
	MaxArchitectureEntryPoints = 8
	// MaxArchitectureIntegrations caps the listed external dependencies.
	MaxArchitectureIntegrations = 10
	// MaxArchitectureBytes is the hard cap on the rendered summary. A pack
	// that spends more than this on structure has less for the work.
	MaxArchitectureBytes = 4096
)

// Architecture is the structural summary of one indexed project.
type Architecture struct {
	// ProjectRoot and IndexedCommit are the provenance: which checkout, at
	// which revision, this describes.
	ProjectRoot   string `json:"projectRoot"`
	IndexedCommit string `json:"indexedCommit,omitempty"`
	// Files, Symbols and Edges size the graph it was derived from.
	Files   int `json:"files"`
	Symbols int `json:"symbols"`
	Edges   int `json:"edges"`
	// Languages are the languages present, most files first.
	Languages []Count `json:"languages,omitempty"`
	// Modules are the largest and most depended-upon directories.
	Modules []ModuleFacts `json:"modules,omitempty"`
	// EntryPoints are the files a process actually starts in.
	EntryPoints []string `json:"entryPoints,omitempty"`
	// Persistence names the tables the schema declares and where the queries
	// against them live.
	Tables     []string `json:"tables,omitempty"`
	QueryFiles []string `json:"queryFiles,omitempty"`
	TableCount int      `json:"tableCount"`
	// Endpoints counts the HTTP surface, and Routers names where it is
	// registered.
	Endpoints int      `json:"endpoints"`
	Routers   []string `json:"routers,omitempty"`
	// Integrations are the external dependencies most files reach for.
	Integrations []Count `json:"integrations,omitempty"`
	// TestFiles and CoveredSymbols measure the verification surface.
	TestFiles      int `json:"testFiles"`
	CoveredSymbols int `json:"coveredSymbols"`
	// ConfigKeys are the configuration surfaces the code reads. Keys only --
	// never a value.
	ConfigKeys []string `json:"configKeys,omitempty"`
	// Truncated reports that a bound was hit, so a reader knows the summary is
	// the top of a list rather than the whole of one.
	Truncated bool `json:"truncated,omitempty"`
}

// Count is one label with an occurrence count.
type Count struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ModuleFacts is one module as the summary states it.
type ModuleFacts struct {
	Path         string   `json:"path"`
	Files        int      `json:"files"`
	Symbols      int      `json:"symbols"`
	Languages    []string `json:"languages,omitempty"`
	DependedOnBy int      `json:"dependedOnBy"`
	DependsOn    []string `json:"dependsOn,omitempty"`
	Endpoints    int      `json:"endpoints,omitempty"`
	Tables       int      `json:"tables,omitempty"`
	Queries      int      `json:"queries,omitempty"`
	Tests        int      `json:"tests,omitempty"`
}

// Architecture derives the summary from the graph.
func (g *Graph) Architecture() Architecture {
	symbols, edges := g.Counts()
	arch := Architecture{
		ProjectRoot:   g.ProjectRoot,
		IndexedCommit: g.IndexedCommit,
		Files:         len(g.Files),
		Symbols:       symbols,
		Edges:         edges,
	}

	languages := map[string]int{}
	tables := map[string]bool{}
	queryFiles := map[string]bool{}
	routers := map[string]bool{}
	configKeys := map[string]bool{}
	covered := map[string]bool{}

	for _, rel := range g.Paths() {
		entry := g.Files[rel]
		if entry.Language != "" {
			languages[entry.Language]++
		}
		if entry.Role == RoleTest {
			arch.TestFiles++
		}
		for _, sym := range entry.Symbols {
			switch sym.Kind {
			case SymbolTable:
				tables[sym.Name] = true
			case SymbolQuery:
				queryFiles[rel] = true
			case SymbolEndpoint:
				arch.Endpoints++
				routers[rel] = true
			case SymbolConfig:
				configKeys[sym.Name] = true
			}
		}
		for _, edge := range entry.Edges {
			if edge.Kind == EdgeTests {
				covered[edge.To] = true
			}
		}
		if isEntryPoint(rel, entry) && len(arch.EntryPoints) < MaxArchitectureEntryPoints {
			arch.EntryPoints = append(arch.EntryPoints, rel)
		}
	}

	arch.Languages = topCounts(languages, len(languages))
	arch.Tables = sortedKeys(tables)
	arch.TableCount = len(arch.Tables)
	if len(arch.Tables) > MaxArchitectureModules {
		arch.Tables = arch.Tables[:MaxArchitectureModules]
		arch.Truncated = true
	}
	arch.QueryFiles = sortedKeys(queryFiles)
	if len(arch.QueryFiles) > 4 {
		arch.QueryFiles = arch.QueryFiles[:4]
		arch.Truncated = true
	}
	arch.Routers = sortedKeys(routers)
	if len(arch.Routers) > 4 {
		arch.Routers = arch.Routers[:4]
		arch.Truncated = true
	}
	arch.ConfigKeys = sortedKeys(configKeys)
	if len(arch.ConfigKeys) > MaxArchitectureModules {
		arch.ConfigKeys = arch.ConfigKeys[:MaxArchitectureModules]
		arch.Truncated = true
	}
	arch.CoveredSymbols = len(covered)

	arch.Modules, arch.Truncated = topModules(g.Modules(), arch.Truncated)
	arch.Integrations = topCounts(g.ExternalDependencies(), MaxArchitectureIntegrations)
	return arch
}

// isEntryPoint reports whether a file is where a process starts.
//
// The test is the declaration itself -- a Go `main`, a Python `__main__`
// guard, a `main`/`index` module under a source root -- rather than a path
// convention alone, because "cmd/" is a convention this repository happens to
// follow and the summary has to be right for one that does not.
func isEntryPoint(rel string, entry FileEntry) bool {
	base := strings.ToLower(rel[strings.LastIndex(rel, "/")+1:])
	switch entry.Language {
	case "go":
		if base != "main.go" {
			return false
		}
		for _, sym := range entry.Symbols {
			if sym.Kind == SymbolFunction && sym.Name == "main" {
				return true
			}
		}
		return false
	case "python":
		return base == "__main__.py" || base == "main.py"
	case "typescript", "javascript":
		return base == "main.ts" || base == "main.tsx" || base == "index.ts" && strings.Count(rel, "/") <= 2
	default:
		return false
	}
}

// topModules ranks the modules worth naming.
//
// The ranking is "how much of the project hangs off this", which is
// dependants first and size second. A twelve-file module that nine other
// modules import is more of the architecture than a forty-file leaf, and a
// Planner splitting work needs the former.
func topModules(modules map[string]*Module, truncated bool) ([]ModuleFacts, bool) {
	ranked := make([]*Module, 0, len(modules))
	for _, m := range modules {
		if m.Files == 0 || m.Path == "." || m.Generated {
			continue
		}
		ranked = append(ranked, m)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.DependedOnBy != b.DependedOnBy {
			return a.DependedOnBy > b.DependedOnBy
		}
		if a.Symbols != b.Symbols {
			return a.Symbols > b.Symbols
		}
		return a.Path < b.Path
	})
	if len(ranked) > MaxArchitectureModules {
		ranked = ranked[:MaxArchitectureModules]
		truncated = true
	}

	out := make([]ModuleFacts, 0, len(ranked))
	for _, m := range ranked {
		facts := ModuleFacts{
			Path: m.Path, Files: m.Files, Symbols: m.Symbols,
			Languages: m.Languages, DependedOnBy: m.DependedOnBy,
			Endpoints: m.Endpoints, Tables: m.Tables, Queries: m.Queries, Tests: m.Tests,
		}
		if len(m.DependsOn) > 4 {
			facts.DependsOn = append([]string(nil), m.DependsOn[:4]...)
			truncated = true
		} else {
			facts.DependsOn = m.DependsOn
		}
		out = append(out, facts)
	}
	return out, truncated
}

// Render writes the summary as the compact text a context pack carries.
//
// It is deterministic and byte-bounded: the same graph renders the same bytes,
// which is what lets a pack digest prove two dispatches were given the same
// architecture.
func (a Architecture) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project structure (%d files, %d symbols", a.Files, a.Symbols)
	if a.IndexedCommit != "" {
		fmt.Fprintf(&b, ", at %s", shortCommit(a.IndexedCommit))
	}
	b.WriteString(")\n")

	if len(a.Languages) > 0 {
		parts := make([]string, 0, len(a.Languages))
		for _, l := range a.Languages {
			parts = append(parts, fmt.Sprintf("%s %d", l.Name, l.Count))
		}
		fmt.Fprintf(&b, "Languages: %s\n", strings.Join(parts, ", "))
	}
	if len(a.EntryPoints) > 0 {
		fmt.Fprintf(&b, "Entry points: %s\n", strings.Join(a.EntryPoints, ", "))
	}
	if len(a.Modules) > 0 {
		b.WriteString("Modules (most depended-upon first):\n")
		for _, m := range a.Modules {
			fmt.Fprintf(&b, "  %s — %d files, %d symbols", m.Path, m.Files, m.Symbols)
			if m.DependedOnBy > 0 {
				fmt.Fprintf(&b, ", imported by %d", m.DependedOnBy)
			}
			for _, extra := range []struct {
				n     int
				label string
			}{{m.Endpoints, "endpoints"}, {m.Tables, "tables"}, {m.Queries, "queries"}, {m.Tests, "test files"}} {
				if extra.n > 0 {
					fmt.Fprintf(&b, ", %d %s", extra.n, extra.label)
				}
			}
			if len(m.DependsOn) > 0 {
				fmt.Fprintf(&b, "\n    depends on: %s", strings.Join(m.DependsOn, ", "))
			}
			b.WriteString("\n")
		}
	}
	if a.Endpoints > 0 {
		fmt.Fprintf(&b, "HTTP surface: %d routes registered in %s\n", a.Endpoints, strings.Join(a.Routers, ", "))
	}
	if a.TableCount > 0 {
		fmt.Fprintf(&b, "Persistence: %d tables (%s)", a.TableCount, strings.Join(a.Tables, ", "))
		if len(a.QueryFiles) > 0 {
			fmt.Fprintf(&b, "; queries in %s", strings.Join(a.QueryFiles, ", "))
		}
		b.WriteString("\n")
	}
	if len(a.Integrations) > 0 {
		parts := make([]string, 0, len(a.Integrations))
		for _, i := range a.Integrations {
			parts = append(parts, i.Name)
		}
		fmt.Fprintf(&b, "External dependencies: %s\n", strings.Join(parts, ", "))
	}
	if a.TestFiles > 0 {
		fmt.Fprintf(&b, "Tests: %d files covering %d named symbols\n", a.TestFiles, a.CoveredSymbols)
	}
	if len(a.ConfigKeys) > 0 {
		fmt.Fprintf(&b, "Configuration keys read: %s\n", strings.Join(a.ConfigKeys, ", "))
	}
	if a.Truncated {
		b.WriteString("(largest items only; the graph holds more)\n")
	}
	return clampBytes(b.String(), MaxArchitectureBytes)
}

// clampBytes truncates to a byte budget on a line boundary, so a clipped
// summary never ends mid-fact.
func clampBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := strings.LastIndex(s[:limit], "\n")
	if cut <= 0 {
		return s[:limit]
	}
	return s[:cut+1] + "(truncated)\n"
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func topCounts(counts map[string]int, limit int) []Count {
	out := make([]Count, 0, len(counts))
	for name, n := range counts {
		out = append(out, Count{Name: name, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
