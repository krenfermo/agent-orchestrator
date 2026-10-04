package codegraph

import (
	"path"
	"sort"
	"strings"
)

// module.go — the directory-level view of a graph, and how an import becomes a
// module dependency.
//
// Modules are directories. Not Go packages, not npm workspaces, not Python
// packages -- directories. The reason is the one the project-memory indexer
// already settled on: a project is a Go backend, a React renderer and a pile of
// SQL in one tree, and a module notion that needs to know which language it is
// looking at cannot describe all three. A directory can.

// Module is one directory's census.
type Module struct {
	// Path is the project-relative directory, or "." at the root.
	Path string
	// Files, Symbols and Tests count what the module contains.
	Files   int
	Symbols int
	Tests   int
	// Languages are the languages present, sorted.
	Languages []string
	// Exports counts symbols on the module's public surface.
	Exports int
	// DependsOn are the modules this one imports, sorted.
	DependsOn []string
	// DependedOnBy counts modules importing this one. A high value is the
	// signal that a module is load-bearing, which is what makes it worth
	// naming in a bounded architecture summary.
	DependedOnBy int
	// Endpoints, Tables and Queries count the architectural surfaces declared
	// here.
	Endpoints int
	Tables    int
	Queries   int
	// Generated reports that every file in the module is generated output.
	Generated bool
}

// moduleOf returns the directory a project-relative path belongs to.
func moduleOf(rel string) string {
	dir := path.Dir(rel)
	if dir == "" || dir == "." || dir == "/" {
		return "."
	}
	return dir
}

// Modules builds the directory census, with module-to-module dependencies
// resolved from the import edges.
//
// It is computed rather than stored. A census derived on demand from the file
// entries can never disagree with them, whereas a second persisted structure
// would need its own invalidation and would eventually drift -- and a drifted
// architecture summary is worse than none, because it reads authoritative.
func (g *Graph) Modules() map[string]*Module {
	modules := map[string]*Module{}
	get := func(dir string) *Module {
		m, ok := modules[dir]
		if !ok {
			m = &Module{Path: dir, Generated: true}
			modules[dir] = m
		}
		return m
	}

	languages := map[string]map[string]bool{}
	for _, rel := range g.Paths() {
		entry := g.Files[rel]
		dir := moduleOf(rel)
		m := get(dir)
		m.Files++
		m.Symbols += len(entry.Symbols)
		if entry.Role != RoleGenerated {
			m.Generated = false
		}
		if entry.Role == RoleTest {
			m.Tests++
		}
		if languages[dir] == nil {
			languages[dir] = map[string]bool{}
		}
		if entry.Language != "" {
			languages[dir][entry.Language] = true
		}
		for _, sym := range entry.Symbols {
			if sym.Exported {
				m.Exports++
			}
			switch sym.Kind {
			case SymbolEndpoint:
				m.Endpoints++
			case SymbolTable:
				m.Tables++
			case SymbolQuery:
				m.Queries++
			}
		}
	}
	for dir, set := range languages {
		out := make([]string, 0, len(set))
		for lang := range set {
			out = append(out, lang)
		}
		sort.Strings(out)
		modules[dir].Languages = out
	}

	g.linkModules(modules)
	return modules
}

// linkModules resolves every import edge onto a module-to-module dependency.
func (g *Graph) linkModules(modules map[string]*Module) {
	dirs := make([]string, 0, len(modules))
	for dir := range modules {
		dirs = append(dirs, dir)
	}
	// Longest first, so "backend/internal/store" wins over "backend" when an
	// import target could match either.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })

	deps := map[string]map[string]bool{}
	for _, rel := range g.Paths() {
		for _, edge := range g.Files[rel].Edges {
			if edge.Kind != EdgeImport {
				continue
			}
			from := moduleOf(rel)
			to, ok := resolveImport(rel, edge.To, dirs, modules)
			if !ok || to == from {
				continue
			}
			if deps[from] == nil {
				deps[from] = map[string]bool{}
			}
			deps[from][to] = true
		}
	}

	for from, targets := range deps {
		out := make([]string, 0, len(targets))
		for to := range targets {
			out = append(out, to)
			modules[to].DependedOnBy++
		}
		sort.Strings(out)
		modules[from].DependsOn = out
	}
}

// resolveImport maps one import specifier onto an indexed module, or reports
// that it names something outside the project.
//
// Two rules, and nothing else:
//
//   - A relative specifier ("./store", "../lib/x") is resolved against the
//     importing file's own directory. That is exact.
//   - An absolute specifier is internal when an indexed directory is a SUFFIX
//     of it: `github.com/aoagents/.../backend/internal/store` ends with
//     `backend/internal/store`. The longest match wins.
//
// Everything else is external, and external is recorded as such rather than
// guessed at. Asserting a module edge AO cannot demonstrate would produce a
// dependency graph that looks complete and is not, which is the failure the
// project-memory indexer already refuses to make.
func resolveImport(fromFile, target string, dirs []string, modules map[string]*Module) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") || target == "." || target == ".." {
		resolved := path.Clean(path.Join(moduleOf(fromFile), target))
		if resolved == "" || strings.HasPrefix(resolved, "..") {
			return "", false
		}
		// The specifier may name a file ("./store") or a directory ("./lib").
		if _, ok := modules[resolved]; ok {
			return resolved, true
		}
		if dir := moduleOf(resolved); dir != "" {
			if _, ok := modules[dir]; ok {
				return dir, true
			}
		}
		return "", false
	}
	normalized := strings.TrimSuffix(target, "/")
	for _, dir := range dirs {
		if dir == "." {
			continue
		}
		if normalized == dir || strings.HasSuffix(normalized, "/"+dir) {
			return dir, true
		}
	}
	return "", false
}

// ExternalDependencies names the import specifiers that resolve to nothing
// inside the project, with how many files reach each. They are the project's
// external integration surface, which is one of the things a Planner is asked
// to respect and cannot see from a file listing.
func (g *Graph) ExternalDependencies() map[string]int {
	modules := g.Modules()
	dirs := make([]string, 0, len(modules))
	for dir := range modules {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })

	out := map[string]int{}
	for _, rel := range g.Paths() {
		for _, edge := range g.Files[rel].Edges {
			if edge.Kind != EdgeImport {
				continue
			}
			if _, internal := resolveImport(rel, edge.To, dirs, modules); internal {
				continue
			}
			if isRelativeSpecifier(edge.To) {
				// A relative import that resolved to nothing is a broken or
				// unindexed path inside the project, not an integration.
				continue
			}
			out[externalRoot(edge.To)]++
		}
	}
	return out
}

func isRelativeSpecifier(target string) bool {
	return strings.HasPrefix(target, ".") || strings.HasPrefix(target, "/")
}

// externalRoot reduces a specifier to the dependency it names: the module path
// for a Go import, the package for a scoped or plain npm one, the top package
// for a Python one.
func externalRoot(target string) string {
	if strings.HasPrefix(target, "@") {
		parts := strings.SplitN(target, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return target
	}
	if strings.Contains(target, ".") && strings.Contains(target, "/") {
		// A Go-style path: keep host/org/repo.
		parts := strings.Split(target, "/")
		if len(parts) >= 3 {
			return strings.Join(parts[:3], "/")
		}
		return target
	}
	if idx := strings.IndexAny(target, "/."); idx > 0 {
		return target[:idx]
	}
	return target
}
