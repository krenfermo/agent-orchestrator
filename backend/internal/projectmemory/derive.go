package projectmemory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// derive.go — what a bounded index actually extracts from a repository.
//
// The governing rule is the one the P2-A brief states twice: do NOT index
// every byte. This file therefore classifies each admitted path into a role,
// and only a small, named set of roles produces a durable fact. A source file
// contributes its *shape* — which module it belongs to, what it imports — and
// nothing else. Its contents remain the repository's business, which is the
// point: memory is a semantic cache over the repo, and the repo stays the
// source of truth.
//
// Every derivation here is mechanical: a heading, a manifest field, an import
// line, a file's own first sentence. Nothing is inferred by a model, and
// nothing is invented. That is what lets each item carry an honest Confidence
// — a verbatim excerpt of an observed file is worth more than a structural
// inference, and both are worth more than a guess, which is why there are no
// guesses.

// fileRole is what kind of thing an admitted path is, and therefore what — if
// anything — is derived from it.
type fileRole string

const (
	// roleInstruction is a standing instruction file an agent is expected to
	// obey (AGENTS.md, CLAUDE.md, CONTRIBUTING.md).
	roleInstruction fileRole = "instruction"
	// roleReadme is the repository's or a module's own introduction.
	roleReadme fileRole = "readme"
	// roleArchitecture is a document describing how the system is put
	// together.
	roleArchitecture fileRole = "architecture"
	// roleManifest is a dependency manifest.
	roleManifest fileRole = "manifest"
	// roleBuild is a build/test entry point (Makefile, Taskfile, CI workflow).
	roleBuild fileRole = "build"
	// roleConfig is a configuration file that changes how the project builds
	// or runs.
	roleConfig fileRole = "config"
	// roleEntrypoint is a program entry point.
	roleEntrypoint fileRole = "entrypoint"
	// roleSource is ordinary source. It contributes module membership and
	// imports, and produces no item of its own.
	roleSource fileRole = "source"
	// roleOther is admitted but derives nothing. It still enters the digest
	// ledger, so its deletion is still detectable.
	roleOther fileRole = "other"
)

// derivesItem reports whether a role produces a durable fact. This is the
// bound that keeps a 4000-file repository from becoming 4000 memory items.
func (r fileRole) derivesItem() bool {
	switch r {
	case roleInstruction, roleReadme, roleArchitecture, roleManifest, roleBuild, roleConfig, roleEntrypoint:
		return true
	default:
		return false
	}
}

var (
	instructionNames = map[string]struct{}{
		"agents.md": {}, "claude.md": {}, "contributing.md": {},
		"conventions.md": {}, ".cursorrules": {}, "context.md": {},
	}
	manifestNames = map[string]struct{}{
		"go.mod": {}, "package.json": {}, "cargo.toml": {}, "pyproject.toml": {},
		"requirements.txt": {}, "gemfile": {}, "pom.xml": {}, "build.gradle": {},
		"build.gradle.kts": {}, "composer.json": {}, "pubspec.yaml": {},
	}
	buildNames = map[string]struct{}{
		"makefile": {}, "taskfile.yml": {}, "taskfile.yaml": {}, "justfile": {},
		"dockerfile": {}, "docker-compose.yml": {}, "docker-compose.yaml": {},
	}
	configNames = map[string]struct{}{
		"tsconfig.json": {}, "sqlc.yaml": {}, "sqlc.json": {}, ".golangci.yml": {},
		".golangci.yaml": {}, "vite.config.ts": {}, "eslint.config.js": {},
		"flake.nix": {}, "pyproject.cfg": {}, "setup.cfg": {},
	}
	entrypointNames = map[string]struct{}{
		"main.go": {}, "main.ts": {}, "main.tsx": {}, "main.py": {}, "main.rs": {},
		"index.ts": {}, "index.tsx": {}, "index.js": {}, "app.tsx": {}, "server.ts": {},
	}
	sourceExts = map[string]struct{}{
		".go": {}, ".ts": {}, ".tsx": {}, ".js": {}, ".jsx": {}, ".py": {},
		".rs": {}, ".java": {}, ".kt": {}, ".rb": {}, ".c": {}, ".h": {},
		".cc": {}, ".cpp": {}, ".hpp": {}, ".cs": {}, ".swift": {}, ".sql": {},
	}
)

// classifyPath assigns a role to a repo-relative, slash-separated path.
//
// Depth matters for the document roles: an AGENTS.md at the repository root is
// a standing instruction for the whole repository, while one buried eight
// directories down is a local note. Only the shallow ones are admitted as
// instructions, so a monorepo full of per-package READMEs does not drown the
// project's own conventions.
func classifyPath(rel string) fileRole {
	rel = strings.TrimPrefix(rel, "./")
	base := strings.ToLower(path.Base(rel))
	ext := strings.ToLower(path.Ext(rel))
	depth := strings.Count(rel, "/")

	if _, ok := instructionNames[base]; ok && depth <= 2 {
		return roleInstruction
	}
	if base == "readme.md" && depth <= 2 {
		return roleReadme
	}
	if isArchitectureDoc(rel, base) {
		return roleArchitecture
	}
	if _, ok := manifestNames[base]; ok && depth <= 3 {
		return roleManifest
	}
	if _, ok := buildNames[base]; ok && depth <= 2 {
		return roleBuild
	}
	if strings.HasPrefix(rel, ".github/workflows/") && (ext == ".yml" || ext == ".yaml") {
		return roleBuild
	}
	if _, ok := configNames[base]; ok && depth <= 3 {
		return roleConfig
	}
	if _, ok := entrypointNames[base]; ok {
		return roleEntrypoint
	}
	if _, ok := sourceExts[ext]; ok {
		return roleSource
	}
	return roleOther
}

func isArchitectureDoc(rel, base string) bool {
	if base == "architecture.md" || base == "design.md" || base == "stack.md" {
		return true
	}
	lower := strings.ToLower(rel)
	if strings.HasPrefix(lower, "docs/adr/") && strings.HasSuffix(lower, ".md") {
		return true
	}
	return strings.HasPrefix(lower, "docs/architecture") && strings.HasSuffix(lower, ".md")
}

// moduleOf returns the module a path belongs to: its directory, or "." at the
// repository root. Modules are directories rather than language-specific units
// because the memory has to work for a Go backend, a React frontend and a pile
// of shell scripts in the same project without knowing which is which.
func moduleOf(rel string) string {
	dir := path.Dir(rel)
	if dir == "" || dir == "/" {
		return "."
	}
	return dir
}

// derivation is what one admitted file contributed.
type derivation struct {
	Items     []domain.ProjectMemoryItem
	Relations []domain.ProjectMemoryRelation
	// Imports are repository-internal import targets this file named, left
	// unresolved: a Go import path or a relative specifier. They are resolved
	// to module edges at finalize, when the manifest's own module path is
	// known regardless of where in the walk it appeared.
	Imports []string
}

// deriveFile produces the durable facts of one admitted file.
func deriveFile(base itemBase, rel string, role fileRole, content []byte) derivation {
	var d derivation
	if role == roleSource || role == roleEntrypoint {
		d.Imports = extractImports(rel, content)
	}
	if !role.derivesItem() {
		return d
	}

	switch role {
	case roleInstruction:
		d.Items = append(d.Items, instructionItem(base, rel, content))
		d.Items = append(d.Items, conventionItems(base, rel, content)...)
	case roleReadme:
		d.Items = append(d.Items, readmeItem(base, rel, content))
	case roleArchitecture:
		d.Items = append(d.Items, architectureItem(base, rel, content))
	case roleManifest:
		d.Items = append(d.Items, dependencyItems(base, rel, content)...)
		if item, ok := manifestBuildItem(base, rel, content); ok {
			d.Items = append(d.Items, item)
		}
	case roleBuild:
		d.Items = append(d.Items, buildItem(base, rel, content))
	case roleConfig:
		d.Items = append(d.Items, configItem(base, rel, content))
	case roleEntrypoint:
		d.Items = append(d.Items, entrypointItem(base, rel, content))
	case roleSource, roleOther:
		// No item; membership and imports only.
	}

	// One membership edge per file that produced a fact, however many facts it
	// produced: the edge says where the file lives, and that is one statement.
	if len(d.Items) > 0 {
		d.Relations = append(d.Relations, base.relation(
			domain.ProjectMemoryNode{Kind: domain.NodeFile, Key: rel},
			domain.RelationImplements,
			domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: moduleOf(rel)},
			[]string{rel}, confidenceStructural,
		))
	}
	return d
}

// sourceDigest is the provenance hash of a single-source fact.
//
// It MUST be computed the same way the drift detector recomputes it
// (domain.MemorySourceDigest over path -> content hash), or every fact would
// look drifted the first time it was checked. Routing both sides through one
// domain function is what makes that impossible to get wrong in only one
// place.
func sourceDigest(rel string, content []byte) string {
	return domain.MemorySourceDigest(map[string]string{rel: hashBytes(content)})
}

// itemBase carries the provenance every derived fact shares, so a derivation
// function cannot forget to stamp one.
type itemBase struct {
	ProjectID  domain.ProjectID
	RepoID     string
	Commit     string
	Generation int64
	Origin     domain.ProjectMemoryOrigin
	OriginRef  string
}

func (b itemBase) item(
	t domain.ProjectMemoryType, scope domain.ProjectMemoryScope, key, summary, content string,
	paths []string, digest string, confidence float64, meta map[string]string,
) domain.ProjectMemoryItem {
	return domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: b.ProjectID, RepoID: b.RepoID, Type: t, Scope: scope, Key: key,
		},
		Origin:       b.Origin,
		OriginRef:    b.OriginRef,
		Summary:      clampSummary(summary),
		Content:      clampContent(content),
		SourcePaths:  paths,
		SourceCommit: b.Commit,
		SourceDigest: digest,
		Generation:   b.Generation,
		State:        domain.MemoryStateValid,
		Confidence:   confidence,
		Metadata:     meta,
	}
}

func (b itemBase) relation(
	from domain.ProjectMemoryNode, kind domain.ProjectMemoryRelationKind, to domain.ProjectMemoryNode,
	paths []string, confidence float64,
) domain.ProjectMemoryRelation {
	return domain.ProjectMemoryRelation{
		ProjectID: b.ProjectID, RepoID: b.RepoID,
		From: from, Kind: kind, To: to,
		Origin: b.Origin, OriginRef: b.OriginRef,
		SourcePaths: paths, SourceCommit: b.Commit,
		Generation: b.Generation, State: domain.MemoryStateValid,
		Confidence: confidence,
	}
}

// Confidence values, stated once so the scale means something.
//
// The scale is "how directly did AO observe this": a verbatim excerpt of a
// file it read is near-certain; a structural fact counted from the file system
// is nearly as good; a claim a prose document makes about the code is weaker,
// because a document can be out of date in ways the file system cannot.
const (
	confidenceVerbatim   = 0.95 // an excerpt of a file AO read this pass
	confidenceStructural = 0.85 // counted from the tree AO walked this pass
	confidenceManifest   = 0.90 // a field parsed out of a declared manifest
	confidenceProse      = 0.65 // what a document claims about the code
	confidenceInferred   = 0.55 // an inference from naming or layout
)

func instructionItem(b itemBase, rel string, content []byte) domain.ProjectMemoryItem {
	body := string(content)
	return b.item(
		domain.MemoryTypeInstruction, domain.MemoryScopeFile, rel,
		fmt.Sprintf("%s — standing instructions agents in this repository must follow", rel),
		excerpt(body, domain.MaxProjectMemoryContent),
		[]string{rel}, sourceDigest(rel, content), confidenceVerbatim,
		map[string]string{"role": string(roleInstruction)},
	)
}

var conventionHeading = regexp.MustCompile(`(?i)^#{2,4}\s+(.*(convention|hard rule|rule|guideline|style|boundar).*)$`)

// conventionItems lifts a small number of explicitly-labelled rule sections
// out of an instruction file, as separate facts.
//
// They are separate from the whole-file instruction item because selection
// works on them differently: a Reviewer looking at a changed area wants the
// two conventions that bear on it, not the whole of AGENTS.md, and a bounded
// pack can afford the former and not the latter.
func conventionItems(b itemBase, rel string, content []byte) []domain.ProjectMemoryItem {
	const maxConventions = 8
	sections := splitSections(string(content))
	var out []domain.ProjectMemoryItem
	for _, s := range sections {
		if len(out) >= maxConventions {
			break
		}
		m := conventionHeading.FindStringSubmatch(s.Heading)
		if m == nil {
			continue
		}
		title := strings.TrimSpace(m[1])
		out = append(out, b.item(
			domain.MemoryTypeConvention, domain.MemoryScopeRepository,
			rel+"#"+slug(title),
			fmt.Sprintf("%s: %s", rel, title),
			excerpt(s.Body, 2048),
			[]string{rel}, sourceDigest(rel, content), confidenceVerbatim,
			map[string]string{"section": title},
		))
	}
	return out
}

func readmeItem(b itemBase, rel string, content []byte) domain.ProjectMemoryItem {
	body := string(content)
	return b.item(
		domain.MemoryTypeArchitecture, domain.MemoryScopeRepository, rel,
		fmt.Sprintf("%s — %s", rel, firstSentence(body, "repository introduction")),
		excerpt(body, 4096),
		[]string{rel}, sourceDigest(rel, content), confidenceProse,
		map[string]string{"role": string(roleReadme)},
	)
}

func architectureItem(b itemBase, rel string, content []byte) domain.ProjectMemoryItem {
	body := string(content)
	return b.item(
		domain.MemoryTypeArchitecture, domain.MemoryScopeRepository, rel,
		fmt.Sprintf("%s — %s", rel, firstSentence(body, "architecture document")),
		excerpt(body, domain.MaxProjectMemoryContent),
		[]string{rel}, sourceDigest(rel, content), confidenceProse,
		map[string]string{"role": string(roleArchitecture)},
	)
}

// dependencyItems parses a manifest into one fact per declared dependency set.
//
// It records the dependency *names*, bounded, rather than the manifest's text:
// what a later task needs to know is "this project uses gorilla/mux", and the
// exact version is a question the repository answers authoritatively at the
// moment it is asked.
func dependencyItems(b itemBase, rel string, content []byte) []domain.ProjectMemoryItem {
	names, ecosystem := parseManifestDependencies(rel, content)
	if len(names) == 0 {
		return nil
	}
	const maxListed = 80
	truncated := false
	if len(names) > maxListed {
		names = names[:maxListed]
		truncated = true
	}
	body := strings.Join(names, "\n")
	if truncated {
		body += "\n… (list truncated to the first " + fmt.Sprint(maxListed) + " declared dependencies)"
	}
	meta := map[string]string{"ecosystem": ecosystem}
	if truncated {
		meta["truncated"] = "true"
	}
	return []domain.ProjectMemoryItem{b.item(
		domain.MemoryTypeDependency, domain.MemoryScopeRepository, rel,
		fmt.Sprintf("%s declares %d %s dependencies", rel, len(names), ecosystem),
		body,
		[]string{rel}, sourceDigest(rel, content), confidenceManifest, meta,
	)}
}

// manifestBuildItem records how the project is built and tested, when the
// manifest says so. Only package.json does today; a Go module's commands live
// in the repository's instruction files, which are captured separately.
func manifestBuildItem(b itemBase, rel string, content []byte) (domain.ProjectMemoryItem, bool) {
	if strings.ToLower(path.Base(rel)) != "package.json" {
		return domain.ProjectMemoryItem{}, false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(content, &pkg); err != nil || len(pkg.Scripts) == 0 {
		return domain.ProjectMemoryItem{}, false
	}
	names := make([]string, 0, len(pkg.Scripts))
	for k := range pkg.Scripts {
		names = append(names, k)
	}
	sort.Strings(names)
	var body strings.Builder
	for _, n := range names {
		fmt.Fprintf(&body, "npm run %s → %s\n", n, pkg.Scripts[n])
	}
	return b.item(
		domain.MemoryTypeBuildTest, domain.MemoryScopeRepository, rel,
		fmt.Sprintf("%s defines %d npm scripts", rel, len(names)),
		excerpt(body.String(), 2048),
		[]string{rel}, sourceDigest(rel, content), confidenceManifest,
		map[string]string{"runner": "npm"},
	), true
}

func buildItem(b itemBase, rel string, content []byte) domain.ProjectMemoryItem {
	return b.item(
		domain.MemoryTypeBuildTest, domain.MemoryScopeRepository, rel,
		fmt.Sprintf("%s — build/test entry point", rel),
		excerpt(string(content), 2048),
		[]string{rel}, sourceDigest(rel, content), confidenceVerbatim,
		map[string]string{"role": string(roleBuild)},
	)
}

func configItem(b itemBase, rel string, content []byte) domain.ProjectMemoryItem {
	return b.item(
		domain.MemoryTypeFileSummary, domain.MemoryScopeFile, rel,
		fmt.Sprintf("%s — configuration governing how this repository builds or runs", rel),
		excerpt(string(content), 2048),
		[]string{rel}, sourceDigest(rel, content), confidenceVerbatim,
		map[string]string{"role": string(roleConfig)},
	)
}

func entrypointItem(b itemBase, rel string, content []byte) domain.ProjectMemoryItem {
	return b.item(
		domain.MemoryTypeFileSummary, domain.MemoryScopeFile, rel,
		fmt.Sprintf("%s — entry point of %s", rel, moduleOf(rel)),
		excerpt(leadingComment(string(content)), 1024),
		[]string{rel}, sourceDigest(rel, content), confidenceStructural,
		map[string]string{"role": string(roleEntrypoint), "module": moduleOf(rel)},
	)
}

// moduleFacts is the per-directory census the walk accumulates in the durable
// file ledger and finalize reads back.
type moduleFacts struct {
	Path      string
	Files     int
	Bytes     int64
	Languages map[string]int
	Notable   []string
}

// moduleItem states what a directory is, from what was actually observed in
// it. There is no prose here on purpose: a structural statement AO counted is
// worth more than a sentence it guessed.
func moduleItem(b itemBase, f moduleFacts, digest string) domain.ProjectMemoryItem {
	langs := make([]string, 0, len(f.Languages))
	for ext := range f.Languages {
		langs = append(langs, ext)
	}
	sort.Slice(langs, func(i, j int) bool {
		if f.Languages[langs[i]] != f.Languages[langs[j]] {
			return f.Languages[langs[i]] > f.Languages[langs[j]]
		}
		return langs[i] < langs[j]
	})
	if len(langs) > 5 {
		langs = langs[:5]
	}

	var body strings.Builder
	fmt.Fprintf(&body, "%d files, %s.\n", f.Files, humanBytes(f.Bytes))
	if len(langs) > 0 {
		body.WriteString("Predominant file types: " + strings.Join(langs, ", ") + ".\n")
	}
	if len(f.Notable) > 0 {
		body.WriteString("Notable files:\n")
		for _, n := range f.Notable {
			body.WriteString("  " + n + "\n")
		}
	}
	summary := fmt.Sprintf("%s — %d files", f.Path, f.Files)
	if len(langs) > 0 {
		summary += " (" + strings.Join(langs, ", ") + ")"
	}
	// A module census is an AGGREGATE: it is derived from the whole directory
	// as the walk saw it, not from the handful of notable paths it names. So
	// it carries no SourceDigest, and drift detection reports it as
	// unverifiable rather than pretending to check it — recomputing a tree
	// digest is a full pass, which is precisely what drift detection is meant
	// to be cheaper than.
	//
	// The notable paths are still recorded as SourcePaths. They are what makes
	// "this file changed, so the module census may have moved" an indexed
	// lookup, and they are what relevance scoring matches a changed path
	// against. The tree digest is kept as metadata so an operator can still
	// see which tree state produced this census.
	return b.item(
		domain.MemoryTypeModule, domain.MemoryScopeModule, f.Path,
		summary, body.String(),
		f.Notable, "", confidenceStructural,
		map[string]string{"files": fmt.Sprint(f.Files), "treeDigest": digest},
	)
}

// overviewItem is the one-per-repository orientation fact.
func overviewItem(b itemBase, repoPath string, modules []moduleFacts, files int, digest string) domain.ProjectMemoryItem {
	var body strings.Builder
	fmt.Fprintf(&body, "Repository root: %s\n", repoPath)
	fmt.Fprintf(&body, "Indexed at commit: %s\n", orNone(b.Commit))
	fmt.Fprintf(&body, "%d files admitted by the index bounds across %d modules.\n\n", files, len(modules))
	body.WriteString("Largest modules:\n")
	for i, m := range modules {
		if i >= 15 {
			body.WriteString("  … (further modules omitted from the overview; ask for them by name)\n")
			break
		}
		fmt.Fprintf(&body, "  %s (%d files)\n", m.Path, m.Files)
	}
	// Like the module census, the overview is an aggregate over the whole
	// tree and carries no per-path provenance to check.
	return b.item(
		domain.MemoryTypeProjectOverview, domain.MemoryScopeProject, "",
		fmt.Sprintf("%s — %d indexed files across %d modules", path.Base(repoPath), files, len(modules)),
		body.String(),
		nil, "", confidenceStructural,
		map[string]string{"repoPath": repoPath, "treeDigest": digest},
	)
}

// --- import extraction ------------------------------------------------------

var (
	goImportLine = regexp.MustCompile(`^\s*(?:[\w.]+\s+)?"([^"]+)"`)
	jsImport     = regexp.MustCompile(`(?:from|require\()\s*['"]([^'"]+)['"]`)
	pyImport     = regexp.MustCompile(`^\s*(?:from\s+([\w.]+)\s+import|import\s+([\w.]+))`)
)

// extractImports pulls the import targets out of a source file's head.
//
// It reads the head only — imports live at the top of every language this
// understands — and it is deliberately lenient: a target it cannot resolve to
// a module inside the repository is dropped at finalize rather than guessed
// at here. An unresolved import is not a dependency edge, and inventing one
// would put a false claim in the graph.
func extractImports(rel string, content []byte) []string {
	const headBytes = 8 * 1024
	if len(content) > headBytes {
		content = content[:headBytes]
	}
	ext := strings.ToLower(path.Ext(rel))
	var out []string
	seen := map[string]struct{}{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	inGoBlock := false
	for sc.Scan() {
		line := sc.Text()
		switch ext {
		case ".go":
			trimmed := strings.TrimSpace(line)
			switch {
			case trimmed == "import (":
				inGoBlock = true
			case inGoBlock && trimmed == ")":
				inGoBlock = false
			case inGoBlock:
				if m := goImportLine.FindStringSubmatch(line); m != nil {
					add(m[1])
				}
			case strings.HasPrefix(trimmed, "import "):
				if m := goImportLine.FindStringSubmatch(strings.TrimPrefix(trimmed, "import")); m != nil {
					add(m[1])
				}
			}
		case ".ts", ".tsx", ".js", ".jsx":
			for _, m := range jsImport.FindAllStringSubmatch(line, -1) {
				add(m[1])
			}
		case ".py":
			if m := pyImport.FindStringSubmatch(line); m != nil {
				add(strings.TrimSpace(m[1] + m[2]))
			}
		}
		if len(out) >= 64 {
			break
		}
	}
	return out
}

// resolveImport maps one import target onto a module inside this repository,
// or reports that it points outside.
//
// The two forms it understands are the two that can be resolved without
// guessing: a Go import path under the module's own path, and a relative
// specifier. A bare package name from any other ecosystem is an external
// dependency, and is left to the manifest to record.
func resolveImport(fromModule, target, goModulePath string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if strings.HasPrefix(target, ".") {
		resolved := path.Clean(path.Join(fromModule, target))
		if resolved == "." || strings.HasPrefix(resolved, "..") {
			return "", false
		}
		return resolved, true
	}
	if goModulePath != "" && strings.HasPrefix(target, goModulePath+"/") {
		return strings.TrimPrefix(target, goModulePath+"/"), true
	}
	return "", false
}

// parseGoModulePath reads the `module` line out of a go.mod, which is what
// makes a Go import path resolvable to a directory in this repository.
func parseGoModulePath(content []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// parseManifestDependencies extracts declared dependency names from the
// manifests AO understands, and names the ecosystem so the fact says which
// vocabulary its names belong to.
func parseManifestDependencies(rel string, content []byte) (names []string, ecosystem string) {
	switch strings.ToLower(path.Base(rel)) {
	case "go.mod":
		return parseGoModRequires(content), "go"
	case "package.json":
		return parsePackageJSONDeps(content), "npm"
	case "cargo.toml", "pyproject.toml":
		return parseTOMLDeps(content), tomlEcosystem(rel)
	case "requirements.txt":
		return parseRequirementsTxt(content), "pypi"
	default:
		return nil, "unknown"
	}
}

func tomlEcosystem(rel string) string {
	if strings.EqualFold(path.Base(rel), "cargo.toml") {
		return "cargo"
	}
	return "pypi"
}

func parseGoModRequires(content []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(content))
	inBlock := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "" && !strings.HasPrefix(line, "//"):
			out = append(out, strings.Fields(line)[0])
		case strings.HasPrefix(line, "require "):
			fields := strings.Fields(strings.TrimPrefix(line, "require "))
			if len(fields) > 0 {
				out = append(out, fields[0])
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

func parsePackageJSONDeps(content []byte) []string {
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(content, &pkg); err != nil {
		return nil
	}
	out := make([]string, 0, len(pkg.Dependencies)+len(pkg.DevDependencies))
	for k := range pkg.Dependencies {
		out = append(out, k)
	}
	for k := range pkg.DevDependencies {
		out = append(out, k+" (dev)")
	}
	sort.Strings(out)
	return out
}

// parseTOMLDeps reads the names under a [dependencies]-style table without a
// TOML parser. It is a name extractor, not a parser: it is used only to list
// what a project declares, and a line it cannot read is skipped rather than
// guessed at.
func parseTOMLDeps(content []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(content))
	inDeps := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inDeps = strings.Contains(line, "dependencies")
			continue
		}
		if !inDeps || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, _, ok := strings.Cut(line, "="); ok {
			out = append(out, strings.TrimSpace(name))
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

func parseRequirementsTxt(content []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		name := strings.FieldsFunc(line, func(r rune) bool {
			return r == '=' || r == '>' || r == '<' || r == '~' || r == '!' || r == '['
		})
		if len(name) > 0 {
			out = append(out, strings.TrimSpace(name[0]))
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

// --- small text helpers -----------------------------------------------------

type section struct {
	Heading string
	Body    string
}

// splitSections cuts a Markdown document at its headings. It is used to pull
// named rule sections out of an instruction file.
func splitSections(doc string) []section {
	lines := strings.Split(doc, "\n")
	var out []section
	cur := section{Heading: ""}
	var body strings.Builder
	flush := func() {
		if cur.Heading != "" || body.Len() > 0 {
			cur.Body = strings.TrimSpace(body.String())
			out = append(out, cur)
		}
		body.Reset()
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			flush()
			cur = section{Heading: line}
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return out
}

// firstSentence returns a document's opening sentence, skipping its headings
// and badges, or fallback when it has none.
func firstSentence(doc, fallback string) string {
	sc := bufio.NewScanner(strings.NewReader(doc))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[!") ||
			strings.HasPrefix(line, "![") || strings.HasPrefix(line, "<") {
			continue
		}
		if idx := strings.Index(line, ". "); idx > 0 {
			line = line[:idx+1]
		}
		return clampSummary(strings.TrimSpace(line))
	}
	return fallback
}

// leadingComment returns a source file's opening comment block, which is where
// every language this understands puts the file's own explanation of itself.
func leadingComment(src string) string {
	var out strings.Builder
	sc := bufio.NewScanner(strings.NewReader(src))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			if out.Len() > 0 {
				return strings.TrimSpace(out.String())
			}
		case strings.HasPrefix(line, "//"), strings.HasPrefix(line, "#"),
			strings.HasPrefix(line, "*"), strings.HasPrefix(line, "/*"):
			out.WriteString(strings.TrimLeft(line, "/*# "))
			out.WriteString("\n")
		default:
			if out.Len() > 0 {
				return strings.TrimSpace(out.String())
			}
		}
	}
	return strings.TrimSpace(out.String())
}

// excerpt cuts text to a byte budget on a line boundary, and says so.
//
// Cutting on a line boundary rather than mid-character keeps the excerpt valid
// UTF-8 and readable; saying so keeps the fact honest, because an agent handed
// a silently-truncated rule would believe it had the whole rule.
func excerpt(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	const marker = "\n… [truncated by the AO project-memory indexer]"
	cut := limit - len(marker)
	if cut <= 0 {
		return marker
	}
	trimmed := s[:cut]
	if nl := strings.LastIndexByte(trimmed, '\n'); nl > cut/2 {
		trimmed = trimmed[:nl]
	} else {
		trimmed = strings.ToValidUTF8(trimmed, "")
	}
	return trimmed + marker
}

func clampSummary(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= domain.MaxProjectMemorySummary {
		return s
	}
	return strings.ToValidUTF8(s[:domain.MaxProjectMemorySummary-1], "") + "…"
}

func clampContent(s string) string { return excerpt(s, domain.MaxProjectMemoryContent) }

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0]
	var last string
	for i, s := range in {
		if i > 0 && s == last {
			continue
		}
		last = s
		out = append(out, s)
	}
	return out
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none — the checkout reported no commit)"
	}
	return s
}
