package projectmemory

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// insight.go — the high-level durable facts, derived from evidence AO already
// holds (P4-H §2, §3, §4, §7).
//
// THE PROBLEM THIS SOLVES. Before P4-H the indexer derived facts from
// documents: a README's first sentence, an AGENTS.md excerpt, a manifest's
// dependency list, a per-directory census. On a real repository that produced
// a great many `module` rows and almost nothing a person would call project
// knowledge — and on repositories with no such documents, nothing at all. The
// questions a Planner actually needs answered first ("what runs", "what
// stores", "what authorises", "how is this deployed") were answerable only by
// reading the repository again, which is the cost memory exists to remove.
//
// The evidence for those answers already existed in two places AO had not
// joined: the Code Graph's structural summary (codegraph.Architecture — entry
// points, routers, tables, integrations, coverage, config keys) and the path
// census the memory walk sees for free. This file joins them into a SMALL,
// bounded set of durable facts.
//
// FOUR RULES, and every function below obeys all four.
//
//   - **Not a second graph.** Nothing here is per-symbol or per-file. The Code
//     Graph answers "which function decides this"; memory answers "which
//     subsystem decides this, and where does it live". A fact that could be
//     replaced by a graph query does not belong here.
//   - **Deterministic.** No model is consulted. Every fact is a count, a name,
//     or a path AO read, so the same repository at the same commit derives
//     byte-identical facts — which is what makes the idempotent upsert
//     meaningful and what makes a change in memory attributable to a change in
//     the repository.
//   - **Labelled honestly.** Every item carries a MemoryEvidenceClass and a
//     confidence that match how it was actually obtained. A count off the
//     graph is `derived` at structural confidence; a subsystem located by
//     directory naming is `derived` at inferred confidence and says so in its
//     own body. Nothing here is `observed` unless AO is repeating something it
//     read verbatim.
//   - **Anchored.** Every fact names the paths it was derived from, so the
//     incremental pass can invalidate exactly the facts a change touches and
//     leave the rest alone (§5). A fact with no anchor would survive every
//     change forever, which is the failure mode staleness exists to prevent.
//
// WHAT IS DELIBERATELY ABSENT. No secrets, no configuration VALUES, no
// credentials, no model reasoning. Config keys are names the code reads; the
// signal census refuses live credential files by name (see
// excludedFromSignals). This is enforced at the two producers rather than by a
// scrub pass over the output, because a scrub pass is a thing that has to be
// kept current and a producer that never has the value cannot leak it.

// maxInsightItems bounds how many high-level facts one repository derives.
//
// It is small on purpose. These facts are the top of a pack, competing for
// budget with the work itself; a repository that produced thirty of them would
// be back to the problem the phase started with. Ten categories, at most one
// fact each, is the whole design.
const maxInsightItems = 12

// insightEvidence is everything the high-level derivation is allowed to look
// at. It is a value rather than a set of arguments so a test can construct the
// exact evidence shape it wants to assert on — including the degraded ones,
// which are the interesting ones: no graph, no signals, neither.
type insightEvidence struct {
	// RepoPath is the canonical repository root, used only for naming.
	RepoPath string
	// Signals is the path census the walk accumulated. Never nil in
	// production; a nil one degrades to "no signal evidence", which is a
	// supported state.
	Signals *pathSignals
	// Arch is the Code Graph's structural summary, and GraphReady says
	// whether it is real. A repository whose graph has not been built has
	// GraphReady false and derives only the signal-backed facts — the
	// degradation the brief requires in §10.
	Arch       codegraph.Architecture
	GraphReady bool
	// GraphGeneration and GraphCommit are the graph's provenance, recorded on
	// every fact derived from it so a reader can tell which build said this.
	GraphGeneration int64
	GraphCommit     string
	// TreeDigest anchors the facts that are about the repository as a whole
	// rather than about named files.
	TreeDigest string
	// FilesAdmitted is how many paths the walk admitted, reported in the
	// architecture fact so "AO looked at 812 files" is visible rather than
	// implied.
	FilesAdmitted int
}

// insightScope says WHICH high-level facts a pass should re-derive (P4-H §5).
//
// This is the mechanism behind "do not regenerate all memory after every
// commit", and it has to be a filter on the DERIVATION rather than a check on
// the result. Every fact carries the commit it was derived at, so re-deriving
// an unchanged fact at a new commit is still a change to the row: the store
// updates it, updated_at moves, and the freshness signal a pack ranks on is
// destroyed across the whole high-level set by one unrelated commit. The
// incremental test caught precisely that — an auth-only change rewriting the
// deployment fact.
//
// So a pass declares what its change set could have moved, and nothing else is
// derived at all.
type insightScope struct {
	// All derives everything. A full pass sets it: it re-read the whole
	// repository, so every fact is being restated at the same commit.
	All bool
	// Kinds are the scan-backed evidence categories the change set touched.
	Kinds map[signalKind]bool
	// Graph reports that the code graph was rebuilt, which moves the counts
	// every graph-backed fact quotes.
	Graph bool
}

// includes reports whether one fact type is in scope.
func (s insightScope) includes(typ domain.ProjectMemoryType) bool {
	if s.All {
		return true
	}
	trigger, known := insightTriggers[typ]
	if !known {
		return false
	}
	if trigger.graph && s.Graph {
		return true
	}
	for _, kind := range trigger.kinds {
		if s.Kinds[kind] {
			return true
		}
	}
	return false
}

// insightTriggers is what can move each high-level fact: which scan-backed
// evidence categories it reads, and whether a code-graph rebuild changes it.
//
// It is the table §5 asks for in one place — "auth code changed → the auth
// fact may be stale; an unrelated test changed → deployment facts remain
// untouched" — rather than the same reasoning repeated at nine producers.
var insightTriggers = map[domain.ProjectMemoryType]struct {
	kinds []signalKind
	graph bool
}{
	// Pure graph summaries: no path naming feeds them.
	domain.MemoryTypeArchitecture:   {graph: true},
	domain.MemoryTypeTestingSurface: {graph: true},
	// Both: the graph answers them better when it has an answer, and the scan
	// answers them when it does not.
	domain.MemoryTypeEntryPoint:     {kinds: []signalKind{signalEntry}, graph: true},
	domain.MemoryTypeRuntimeSurface: {kinds: []signalKind{signalRuntime}, graph: true},
	domain.MemoryTypePersistence:    {kinds: []signalKind{signalPersistence}, graph: true},
	domain.MemoryTypeConfigSurface:  {kinds: []signalKind{signalConfig}, graph: true},
	domain.MemoryTypeIntegration:    {kinds: []signalKind{signalManifest}, graph: true},
	// Scan only. A graph rebuild tells us nothing new about either.
	domain.MemoryTypeAuthModel:  {kinds: []signalKind{signalAuth}},
	domain.MemoryTypeDeployment: {kinds: []signalKind{signalDeployment}},
}

// deriveInsights produces the high-level durable facts for one repository,
// restricted to what the scope says could have moved.
//
// It returns items only for the categories the evidence actually supports. A
// repository with no HTTP surface gets no runtime_surface fact — not an empty
// one saying so. An absent fact is the honest representation of "AO found no
// evidence"; a fact asserting absence would be a claim AO cannot support,
// since the evidence here is naming and naming can miss.
func deriveInsights(b itemBase, ev insightEvidence, scope insightScope) []domain.ProjectMemoryItem {
	out := make([]domain.ProjectMemoryItem, 0, maxInsightItems)
	add := func(typ domain.ProjectMemoryType, derive func(itemBase, insightEvidence) (domain.ProjectMemoryItem, bool)) {
		if !scope.includes(typ) || len(out) >= maxInsightItems {
			return
		}
		if item, ok := derive(b, ev); ok {
			out = append(out, item)
		}
	}

	add(domain.MemoryTypeArchitecture, architectureInsight)
	add(domain.MemoryTypeEntryPoint, entryPointInsight)
	add(domain.MemoryTypeRuntimeSurface, runtimeSurfaceInsight)
	add(domain.MemoryTypePersistence, persistenceInsight)
	add(domain.MemoryTypeAuthModel, authModelInsight)
	add(domain.MemoryTypeIntegration, integrationInsight)
	add(domain.MemoryTypeTestingSurface, testingSurfaceInsight)
	add(domain.MemoryTypeConfigSurface, configSurfaceInsight)
	add(domain.MemoryTypeDeployment, deploymentInsight)
	return out
}

// insightMeta is the provenance annotation every high-level fact carries: what
// produced it, and — when the graph did — which build of the graph.
//
// It is not a second content channel. Three keys, all short, all machine
// readable, so a surface can group by producer without parsing prose.
func insightMeta(ev insightEvidence, producer string, extra map[string]string) map[string]string {
	meta := map[string]string{"derivedBy": producer, "phase": "p4h"}
	// The tree digest is deliberately NOT recorded here, and the reason is
	// section 5 rather than brevity.
	//
	// Metadata participates in the content hash (domain.contentHash), so a
	// value that moves on every commit makes every fact carrying it a CHANGED
	// fact on every commit — the store rewrites it, updated_at moves, and
	// "this fact has not changed since March" becomes false for the whole
	// high-level set. The first run of the incremental test caught exactly
	// that: an auth-only change rewrote the deployment fact.
	//
	// What is recorded instead is provenance that moves when the FACT'S OWN
	// evidence moves: which producer derived it, and for a graph-backed fact
	// which graph build it read. Those are the changes a reader should see.
	if producer == insightProducerGraph && ev.GraphReady {
		meta["graphGeneration"] = strconv.FormatInt(ev.GraphGeneration, 10)
		if ev.GraphCommit != "" {
			meta["graphCommit"] = ev.GraphCommit
		}
	}
	for k, v := range extra {
		if v != "" {
			meta[k] = v
		}
	}
	return meta
}

// The two producers a high-level fact can name. They are distinguished because
// they fail differently: a graph-derived fact is only as current as the graph
// build, while a scan-derived one is as current as the last memory pass.
const (
	insightProducerGraph = "code_graph"
	insightProducerScan  = "repository_scan"
)

// architectureInsight is the orientation fact: what this repository is made
// of, at the size a person reads before touching it.
//
// It is the one fact that requires the graph. Without a graph AO already has
// project_overview (derive.go), which says the same thing at the level of
// directories; adding a second, weaker version of it would be the duplication
// §2 forbids.
func architectureInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	if !ev.GraphReady || ev.Arch.Files == 0 {
		return domain.ProjectMemoryItem{}, false
	}
	arch := ev.Arch

	var body strings.Builder
	body.WriteString(arch.Render())

	langs := make([]string, 0, len(arch.Languages))
	for _, l := range arch.Languages {
		langs = append(langs, fmt.Sprintf("%s (%d)", l.Name, l.Count))
	}
	summary := fmt.Sprintf(
		"Architecture: %d files, %d symbols, %d relations across %d modules; primary languages %s",
		arch.Files, arch.Symbols, arch.Edges, len(arch.Modules), joinTop(langs, 3, "none identified"),
	)

	// The modules are the anchor. An architecture fact whose top modules all
	// still exist is one whose shape has not moved; one whose modules were
	// deleted or renamed should not survive the change.
	paths := make([]string, 0, len(arch.Modules))
	for _, m := range arch.Modules {
		paths = append(paths, m.Path)
	}

	return b.item(
		domain.MemoryTypeArchitecture, domain.MemoryScopeRepository, "graph-architecture",
		summary, body.String(),
		boundPaths(paths), "", confidenceStructural,
		insightMeta(ev, insightProducerGraph, map[string]string{
			"files":   strconv.Itoa(arch.Files),
			"symbols": strconv.Itoa(arch.Symbols),
		}),
	).WithEvidenceClass(domain.EvidenceDerived), true
}

// entryPointInsight names the files a process actually starts in.
//
// One census item rather than one item per entry point: "where does this thing
// start" is a single question, and four rows answering a quarter of it each is
// four rows a pack has to pay for to answer it once.
func entryPointInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	paths, producer := insightPaths(ev, ev.Arch.EntryPoints, signalEntry)
	if len(paths) == 0 {
		return domain.ProjectMemoryItem{}, false
	}
	body := "Processes in this repository start in:\n" + bulletList(paths)
	if producer == insightProducerScan {
		body += "\nIdentified by file naming during the repository scan; the code " +
			"graph had no entry-point analysis for this repository."
	}
	return b.item(
		domain.MemoryTypeEntryPoint, domain.MemoryScopeRepository, "entry-points",
		fmt.Sprintf("Entry points: %s", joinTop(paths, 4, "none")),
		body, boundPaths(paths), "", insightConfidence(producer),
		insightMeta(ev, producer, nil),
	).WithEvidenceClass(domain.EvidenceDerived), true
}

// runtimeSurfaceInsight describes how the project is reached at runtime.
func runtimeSurfaceInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	graphRouters := ev.Arch.Routers
	endpoints := ev.Arch.Endpoints
	paths, producer := insightPaths(ev, graphRouters, signalRuntime)
	if len(paths) == 0 && endpoints == 0 {
		return domain.ProjectMemoryItem{}, false
	}

	var summary string
	var body strings.Builder
	if ev.GraphReady && endpoints > 0 {
		summary = fmt.Sprintf("Runtime surface: %d HTTP endpoints registered in %d location(s)",
			endpoints, len(graphRouters))
		fmt.Fprintf(&body, "The code graph counted %d registered endpoints.\n", endpoints)
	} else {
		summary = fmt.Sprintf("Runtime surface: request handling lives in %s",
			joinTop(ev.Signals.dirRanking(signalRuntime), 3, "unidentified locations"))
	}
	if len(paths) > 0 {
		body.WriteString("Registered and served from:\n")
		body.WriteString(bulletList(paths))
	}
	if total := ev.Signals.total(signalRuntime); total > len(paths) {
		fmt.Fprintf(&body, "\n%d files in total match the runtime-surface naming convention; the above are a sample.\n", total)
	}
	if producer == insightProducerScan {
		body.WriteString("\nLocated by directory and file naming, not by parsing route registrations.")
	}

	return b.item(
		domain.MemoryTypeRuntimeSurface, domain.MemoryScopeRepository, "runtime-surface",
		summary, body.String(), boundPaths(paths), "",
		insightConfidence(producer), insightMeta(ev, producer, map[string]string{
			"endpoints": strconv.Itoa(endpoints),
		}),
	).WithEvidenceClass(domain.EvidenceDerived), true
}

// persistenceInsight describes the storage architecture.
//
// Table names are the one thing here that is quoted rather than concluded:
// they are identifiers the schema declares, and the graph read them out of it.
// The fact as a whole is still `derived`, because "this is the storage
// architecture" is AO's summary — but the body says which half is which, so a
// reader can tell the quoted names from the framing around them.
func persistenceInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	arch := ev.Arch
	paths, producer := insightPaths(ev, arch.QueryFiles, signalPersistence)
	if len(paths) == 0 && arch.TableCount == 0 {
		return domain.ProjectMemoryItem{}, false
	}

	var summary string
	var body strings.Builder
	if ev.GraphReady && arch.TableCount > 0 {
		summary = fmt.Sprintf("Persistence: %d tables declared by the schema", arch.TableCount)
		fmt.Fprintf(&body, "The schema declares %d tables.", arch.TableCount)
		if len(arch.Tables) > 0 {
			fmt.Fprintf(&body, " Named in the schema: %s.", joinTop(arch.Tables, 20, ""))
		}
		body.WriteString("\n")
	} else {
		summary = fmt.Sprintf("Persistence: storage code lives in %s",
			joinTop(ev.Signals.dirRanking(signalPersistence), 3, "unidentified locations"))
	}
	if len(paths) > 0 {
		body.WriteString("Schema, migrations and queries live in:\n")
		body.WriteString(bulletList(paths))
	}
	if total := ev.Signals.total(signalPersistence); total > len(paths) {
		fmt.Fprintf(&body, "\n%d files in total match the persistence naming convention; the above are a sample.\n", total)
	}

	return b.item(
		domain.MemoryTypePersistence, domain.MemoryScopeRepository, "persistence",
		summary, body.String(), boundPaths(paths), "",
		insightConfidence(producer), insightMeta(ev, producer, map[string]string{
			"tables": strconv.Itoa(arch.TableCount),
		}),
	).WithEvidenceClass(domain.EvidenceDerived), true
}

// authModelInsight names where identity is established and permission decided.
//
// This is the weakest fact this file produces and the one most worth having.
// It is located by naming alone — there is no static analysis here that proves
// a function decides a permission — so it carries the inferred confidence, it
// is labelled `derived`, and its own body states the method. A reader who acts
// on it is being pointed at the right files to open, which is the honest form
// of this fact and the useful one; a reader must never come away believing AO
// verified an authorisation model.
func authModelInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	paths := ev.Signals.sample(signalAuth)
	if len(paths) == 0 {
		return domain.ProjectMemoryItem{}, false
	}
	total := ev.Signals.total(signalAuth)
	dirs := ev.Signals.dirRanking(signalAuth)

	var body strings.Builder
	fmt.Fprintf(&body,
		"Authentication/authorization code was located in %d file(s), concentrated in %s.\n",
		total, joinTop(dirs, 4, "no single directory"))
	body.WriteString("Files to read first:\n")
	body.WriteString(bulletList(paths))
	body.WriteString("\nHow this was determined: directory and file naming during the " +
		"repository scan. AO has NOT verified that these files implement the " +
		"authorization model, only that they are where it would be. Confirm " +
		"against the code before relying on it.")

	return b.item(
		domain.MemoryTypeAuthModel, domain.MemoryScopeRepository, "auth-model",
		fmt.Sprintf("Auth model: identity/permission code concentrated in %s (%d files)",
			joinTop(dirs, 3, "unidentified locations"), total),
		body.String(), boundPaths(paths), "",
		confidenceInferred, insightMeta(ev, insightProducerScan, map[string]string{
			"files": strconv.Itoa(total),
		}),
	).WithEvidenceClass(domain.EvidenceDerived), true
}

// integrationInsight names the external systems the project reaches for.
//
// It is anchored on the dependency manifests rather than on the importing
// files: a manifest change is what actually adds or removes an integration,
// and anchoring on every importer would invalidate the fact whenever anybody
// touched a file that happened to import one.
func integrationInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	if !ev.GraphReady || len(ev.Arch.Integrations) == 0 {
		return domain.ProjectMemoryItem{}, false
	}
	names := make([]string, 0, len(ev.Arch.Integrations))
	lines := make([]string, 0, len(ev.Arch.Integrations))
	for _, in := range ev.Arch.Integrations {
		names = append(names, in.Name)
		lines = append(lines, fmt.Sprintf("%s — reached from %d file(s)", in.Name, in.Count))
	}
	body := "External packages this repository's code imports most:\n" + bulletList(lines) +
		"\nThese are the integrations visible to the code graph's import analysis. " +
		"A service reached only over HTTP with no client library will not appear here."

	return b.item(
		domain.MemoryTypeIntegration, domain.MemoryScopeRepository, "integrations",
		fmt.Sprintf("External integrations: %s", joinTop(names, 5, "none identified")),
		body, boundPaths(ev.Signals.sample(signalManifest)), "",
		confidenceStructural, insightMeta(ev, insightProducerGraph, nil),
	).WithEvidenceClass(domain.EvidenceDerived), true
}

// testingSurfaceInsight reports how the project verifies itself.
func testingSurfaceInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	if !ev.GraphReady || ev.Arch.TestFiles == 0 {
		return domain.ProjectMemoryItem{}, false
	}
	arch := ev.Arch
	summary := fmt.Sprintf("Testing surface: %d test files covering %d symbols",
		arch.TestFiles, arch.CoveredSymbols)
	body := fmt.Sprintf(
		"The code graph found %d test files, which reference %d symbols defined elsewhere "+
			"in the repository.\n\nCoverage here means \"a test file references this symbol\", "+
			"which is not the same as line coverage and must not be reported as it.",
		arch.TestFiles, arch.CoveredSymbols)

	return b.item(
		domain.MemoryTypeTestingSurface, domain.MemoryScopeRepository, "testing-surface",
		summary, body, nil, "", confidenceStructural,
		insightMeta(ev, insightProducerGraph, map[string]string{
			"testFiles": strconv.Itoa(arch.TestFiles),
		}),
	).WithEvidenceClass(domain.EvidenceDerived), true
}

// configSurfaceInsight names the configuration files and the keys the code
// reads. Keys only — a value read out of a checked-in config is exactly the
// class of thing that turns out to be a credential.
func configSurfaceInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	files := ev.Signals.sample(signalConfig)
	keys := ev.Arch.ConfigKeys
	if len(files) == 0 && len(keys) == 0 {
		return domain.ProjectMemoryItem{}, false
	}

	var body strings.Builder
	if len(files) > 0 {
		body.WriteString("Configuration files:\n")
		body.WriteString(bulletList(files))
	}
	if len(keys) > 0 {
		fmt.Fprintf(&body, "\nConfiguration keys the code reads (%d):\n%s", len(keys), bulletList(keys))
		body.WriteString("\nKey names only. AO never records a configuration value.")
	}

	producer := insightProducerScan
	if len(keys) > 0 {
		producer = insightProducerGraph
	}
	summary := fmt.Sprintf("Configuration: %d file(s), %d key(s) read by the code", len(files), len(keys))

	return b.item(
		domain.MemoryTypeConfigSurface, domain.MemoryScopeRepository, "config-surface",
		summary, body.String(), boundPaths(files), "",
		confidenceStructural, insightMeta(ev, producer, nil),
		// `observed` rather than `derived`: both halves are names AO read —
		// the files off the filesystem, the keys out of the source. The only
		// derived part is the count, which the reader can recount.
	).WithEvidenceClass(domain.EvidenceObserved), true
}

// deploymentInsight names how the project is packaged, shipped and run.
func deploymentInsight(b itemBase, ev insightEvidence) (domain.ProjectMemoryItem, bool) {
	paths := ev.Signals.sample(signalDeployment)
	if len(paths) == 0 {
		return domain.ProjectMemoryItem{}, false
	}
	total := ev.Signals.total(signalDeployment)
	kinds := deploymentKinds(paths)

	var body strings.Builder
	fmt.Fprintf(&body, "Packaging and deployment artefacts found (%d file(s)): %s.\n",
		total, joinTop(kinds, 6, "unclassified"))
	body.WriteString(bulletList(paths))
	body.WriteString("\nIdentified by filename convention. AO has not run or validated " +
		"any of these; they are where to look, not a description of what runs in production.")

	return b.item(
		domain.MemoryTypeDeployment, domain.MemoryScopeRepository, "deployment",
		fmt.Sprintf("Deployment: %s", joinTop(kinds, 4, "artefacts present")),
		body.String(), boundPaths(paths), "",
		confidenceInferred, insightMeta(ev, insightProducerScan, map[string]string{
			"files": strconv.Itoa(total),
		}),
	).WithEvidenceClass(domain.EvidenceDerived), true
}

// deploymentKinds names what KIND of deployment artefact each path is, so the
// summary line says "Docker, GitHub Actions, Terraform" rather than listing
// eleven file paths nobody can scan.
func deploymentKinds(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(kind string) {
		if !seen[kind] {
			seen[kind] = true
			out = append(out, kind)
		}
	}
	for _, p := range paths {
		lower := strings.ToLower(p)
		base := lower
		if i := strings.LastIndex(lower, "/"); i >= 0 {
			base = lower[i+1:]
		}
		switch {
		case strings.HasPrefix(base, "docker-compose"):
			add("Docker Compose")
		case base == "dockerfile" || strings.HasPrefix(base, "dockerfile."):
			add("Docker")
		case strings.HasPrefix(lower, ".github/workflows/"):
			add("GitHub Actions")
		case strings.Contains(lower, "gitlab-ci"):
			add("GitLab CI")
		case base == "jenkinsfile":
			add("Jenkins")
		case strings.Contains(lower, "/terraform/") || strings.HasPrefix(lower, "terraform/"):
			add("Terraform")
		case strings.Contains(lower, "/helm/") || strings.Contains(lower, "/charts/"):
			add("Helm")
		case strings.Contains(lower, "k8s") || strings.Contains(lower, "kubernetes"):
			add("Kubernetes")
		case base == "fly.toml":
			add("Fly.io")
		case base == "vercel.json":
			add("Vercel")
		case base == "procfile":
			add("Procfile")
		default:
			add("deployment config")
		}
	}
	sort.Strings(out)
	return out
}

// insightPaths picks the better of two evidence sources for one fact, and says
// which it used.
//
// The graph's answer wins when it has one: it comes from parsing the code
// rather than from reading names, and the confidence the fact carries reflects
// that. Falling back to the signal census is what makes these facts available
// on a repository whose graph has not been built yet, which §10 requires.
func insightPaths(ev insightEvidence, graphPaths []string, kind signalKind) ([]string, string) {
	if ev.GraphReady {
		// The graph's admitted set is its own and is wider than the memory
		// walk's: it indexes trees the memory indexer skips. So paths coming
		// back from it go through the SAME exclusion the census applies, or a
		// fact would name an agent's scratch worktree purely because the
		// graph could see it.
		out := make([]string, 0, len(graphPaths))
		for _, p := range graphPaths {
			if !excludedFromSignals(p) {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			sort.Strings(out)
			return out, insightProducerGraph
		}
	}
	return ev.Signals.sample(kind), insightProducerScan
}

// insightConfidence is how much a fact is worth given which evidence produced
// it. A graph-derived location was parsed; a scan-derived one was guessed from
// a name, and the scale has to say so or the whole confidence field is
// decoration.
func insightConfidence(producer string) float64 {
	if producer == insightProducerGraph {
		return confidenceStructural
	}
	return confidenceInferred
}

// boundPaths trims a source-path list to the domain cap. The cap is enforced
// here rather than relied on from Validate so a fact is never REJECTED for
// having found too much evidence — it is anchored on the first N, which is
// still a correct anchor.
func boundPaths(paths []string) []string {
	if len(paths) <= domain.MaxProjectMemorySourcePaths {
		return paths
	}
	return paths[:domain.MaxProjectMemorySourcePaths]
}

// joinTop renders at most n items, saying how many more there were. The
// fallback is what an empty list renders as, so a summary never reads
// "Entry points: ".
func joinTop(items []string, n int, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:n], ", "), len(items)-n)
}

func bulletList(items []string) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString("  - ")
		b.WriteString(item)
		b.WriteString("\n")
	}
	return b.String()
}
