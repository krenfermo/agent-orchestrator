package projectmemory

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// insight_test.go — the properties the high-level derivation must have, tested
// at the level it decides them.
//
// These are unit tests over pure functions on purpose. Whether the derivation
// runs at all is a lifecycle question the service tests own; whether what it
// derives is honest is decided entirely by the functions here, and testing it
// through a database and a git checkout would hide which of the two broke.

func testBase() itemBase {
	return itemBase{
		ProjectID: "p1", RepoID: "repo_1", Commit: "c1", Generation: 3,
		Origin: domain.OriginCanonical, ProvenanceKind: domain.ProvenanceRepoDerivation,
		Evidence: domain.EvidenceObserved,
	}
}

func signalsFor(paths ...string) *pathSignals {
	s := newPathSignals()
	for _, p := range paths {
		s.observe(p)
	}
	return s
}

func itemsByType(items []domain.ProjectMemoryItem) map[domain.ProjectMemoryType]domain.ProjectMemoryItem {
	out := map[domain.ProjectMemoryType]domain.ProjectMemoryItem{}
	for _, item := range items {
		out[item.Key.Type] = item
	}
	return out
}

// The whole point of the phase: a repository with an ordinary shape produces
// the high-level facts a person asks for before touching it.
func TestDerivationProducesTheHighLevelFacts(t *testing.T) {
	ev := insightEvidence{
		RepoPath: "/repo",
		Signals: signalsFor(
			"internal/auth/session.go", "internal/auth/token.go", "internal/oidc/login.go",
			"internal/storage/migrations/0001_init.sql", "internal/storage/queries/users.sql",
			"internal/httpd/router.go", "internal/httpd/controllers/users.go",
			"Dockerfile", ".github/workflows/ci.yml",
			"config/settings.yaml", "cmd/server/main.go",
		),
		GraphReady:      true,
		GraphGeneration: 7,
		GraphCommit:     "c1",
		Arch: codegraph.Architecture{
			Files: 120, Symbols: 900, Edges: 2400,
			Languages:    []codegraph.Count{{Name: "go", Count: 100}},
			Modules:      []codegraph.ModuleFacts{{Path: "internal/httpd", Files: 20}},
			TableCount:   14,
			Tables:       []string{"users", "sessions"},
			Endpoints:    31,
			Integrations: []codegraph.Count{{Name: "github.com/go-chi/chi", Count: 12}},
			TestFiles:    40, CoveredSymbols: 120,
			ConfigKeys: []string{"AO_DATA_DIR"},
		},
		FilesAdmitted: 120,
	}

	byType := itemsByType(deriveInsights(testBase(), ev, insightScope{All: true}))
	for _, want := range []domain.ProjectMemoryType{
		domain.MemoryTypeArchitecture, domain.MemoryTypeEntryPoint,
		domain.MemoryTypeRuntimeSurface, domain.MemoryTypePersistence,
		domain.MemoryTypeAuthModel, domain.MemoryTypeIntegration,
		domain.MemoryTypeTestingSurface, domain.MemoryTypeConfigSurface,
		domain.MemoryTypeDeployment,
	} {
		if _, ok := byType[want]; !ok {
			t.Errorf("no %s fact was derived from evidence that supports one", want)
		}
	}

	// Every fact must validate, or it cannot be stored at all.
	for typ, item := range byType {
		if err := item.Normalized().Validate(); err != nil {
			t.Errorf("%s does not validate: %v", typ, err)
		}
	}
}

// Section 4: a claim AO inferred must never read as one AO confirmed. The auth
// fact is the sharpest case — it is located by naming alone.
func TestInferredFactsSayTheyAreInferred(t *testing.T) {
	items := deriveInsights(testBase(), insightEvidence{
		Signals: signalsFor("internal/auth/session.go", "internal/rbac/permissions.go"),
	}, insightScope{All: true})
	auth, ok := itemsByType(items)[domain.MemoryTypeAuthModel]
	if !ok {
		t.Fatal("no auth fact was derived")
	}
	if auth.EvidenceClass != domain.EvidenceDerived {
		t.Errorf("evidence class = %q, want derived", auth.EvidenceClass)
	}
	if !auth.EvidenceClass.Inferred() {
		t.Error("the auth fact does not report itself as an inference")
	}
	if auth.Confidence > confidenceInferred {
		t.Errorf("confidence %v is above the inferred ceiling %v", auth.Confidence, confidenceInferred)
	}
	if !strings.Contains(auth.Content, "NOT verified") {
		t.Error("the auth fact's body does not state that AO has not verified it")
	}
	if len(auth.SourcePaths) == 0 {
		t.Error("the auth fact names no evidence, so nobody can check it")
	}
}

// Section 10: no graph is a degraded state, never a failure. The scan-backed
// facts must still be derived.
func TestDerivationDegradesWithoutTheGraph(t *testing.T) {
	items := deriveInsights(testBase(), insightEvidence{
		Signals: signalsFor(
			"internal/auth/session.go", "internal/httpd/router.go",
			"Dockerfile", "cmd/server/main.go", "config/settings.yaml",
		),
		GraphReady: false,
	}, insightScope{All: true})
	byType := itemsByType(items)
	for _, want := range []domain.ProjectMemoryType{
		domain.MemoryTypeAuthModel, domain.MemoryTypeRuntimeSurface,
		domain.MemoryTypeDeployment, domain.MemoryTypeEntryPoint,
		domain.MemoryTypeConfigSurface,
	} {
		if _, ok := byType[want]; !ok {
			t.Errorf("%s was not derived without a graph, but its evidence is scan-backed", want)
		}
	}
	// And the graph-only ones are absent rather than invented.
	for _, absent := range []domain.ProjectMemoryType{
		domain.MemoryTypeArchitecture, domain.MemoryTypeIntegration,
		domain.MemoryTypeTestingSurface,
	} {
		if _, ok := byType[absent]; ok {
			t.Errorf("%s was derived with no graph to derive it from", absent)
		}
	}
	// A scan-backed location is worth less than a parsed one, and must say so.
	if runtime := byType[domain.MemoryTypeRuntimeSurface]; runtime.Confidence != confidenceInferred {
		t.Errorf("scan-backed runtime confidence = %v, want %v", runtime.Confidence, confidenceInferred)
	}
}

// A category with no evidence produces NO fact, not an empty one. A fact
// asserting "this project has no authentication" is a claim naming cannot
// support.
func TestNoEvidenceProducesNoFact(t *testing.T) {
	items := deriveInsights(testBase(), insightEvidence{Signals: signalsFor("README.md", "docs/notes.md")}, insightScope{All: true})
	for _, item := range items {
		if item.Key.Type == domain.MemoryTypeAuthModel {
			t.Fatalf("an auth fact was derived from no auth evidence: %q", item.Summary)
		}
	}
}

// Determinism is what makes the idempotent upsert meaningful: the same
// repository at the same commit must derive byte-identical facts, or every
// pass would rewrite every row and updated_at would stop meaning anything.
func TestDerivationIsDeterministic(t *testing.T) {
	build := func() []domain.ProjectMemoryItem {
		return deriveInsights(testBase(), insightEvidence{
			Signals: signalsFor(
				"internal/auth/b.go", "internal/auth/a.go", "internal/rbac/x.go",
				"internal/httpd/router.go", "Dockerfile",
			),
		}, insightScope{All: true})
	}
	first, second := build(), build()
	if len(first) != len(second) {
		t.Fatalf("two derivations produced %d and %d facts", len(first), len(second))
	}
	for i := range first {
		a := first[i].Normalized()
		b := second[i].Normalized()
		if a.ID != b.ID || a.ContentHash != b.ContentHash {
			t.Errorf("fact %d is not deterministic: %s/%s vs %s/%s",
				i, a.ID, a.ContentHash, b.ID, b.ContentHash)
		}
	}
}

// Section 2 forbids storing secrets. The census is where a path could leak
// one, so it is where the refusal is tested.
func TestSignalsRefuseCredentialFilesAndForeignTrees(t *testing.T) {
	refused := []string{
		".env", ".env.production", "backend/.env.local",
		"certs/server.pem", "certs/server.key", "config/credentials.json",
		"secrets/tokens.yaml", ".npmrc",
		"node_modules/express/lib/router.js",
		".claude/worktrees/wt-1/internal/auth/session.go",
		"vendor/github.com/x/auth/auth.go",
	}
	for _, path := range refused {
		if !excludedFromSignals(path) {
			t.Errorf("%q was admitted as evidence and must not be", path)
		}
	}
	// Templates carry no values and are exactly what a newcomer needs.
	for _, kept := range []string{".env.example", ".env.sample", "config/settings.yaml"} {
		if excludedFromSignals(kept) {
			t.Errorf("%q was refused, but it carries no secret", kept)
		}
	}
}

// A test file describes the verification surface, never the subsystem it
// exercises. Counting auth_test.go as a place authorisation is decided would
// point a reader at the test instead of at the code.
func TestTestFilesAreNotSubsystemEvidence(t *testing.T) {
	for _, path := range []string{
		"internal/auth/session_test.go", "src/auth/login.test.ts",
		"tests/test_permissions.py", "spec/rbac_spec.rb",
	} {
		if kinds := signalKindsOf(path); len(kinds) > 0 {
			t.Errorf("%q was classified as %v evidence", path, kinds)
		}
	}
}

// A subsystem claim is a claim about code, so only code may be evidence for
// it. Before this rule AO's auth fact pointed at documentation and build
// artefacts.
func TestOnlyCodeIsSubsystemEvidence(t *testing.T) {
	for _, path := range []string{
		"docs/authorization.md", "artifacts/auth.json", "notes/permissions.txt",
	} {
		for _, kind := range signalKindsOf(path) {
			switch kind {
			case signalAuth, signalPersistence, signalRuntime, signalEntry:
				t.Errorf("%q was classified as %s evidence", path, kind)
			}
		}
	}
}

// The evidence a fact names must be where the category actually concentrates,
// not wherever the walk happened to look first. Walk order is alphabetical, so
// without this the fact names whichever directory sorts earliest.
func TestEvidenceFollowsConcentrationNotWalkOrder(t *testing.T) {
	s := newPathSignals()
	// One auth file in an early-sorting directory...
	s.observe("aaa/adapters/auth.go")
	// ...and five in a late-sorting one.
	for _, f := range []string{"session.go", "token.go", "login.go", "oidc.go", "rbac.go"} {
		s.observe("zzz/identity/" + f)
	}
	dirs := s.dirRanking(signalAuth)
	if len(dirs) == 0 || dirs[0] != "zzz/identity" {
		t.Fatalf("dirRanking = %v, want zzz/identity first", dirs)
	}
	sample := s.sample(signalAuth)
	if len(sample) == 0 || !strings.HasPrefix(sample[0], "zzz/identity/") {
		t.Fatalf("sample = %v, want the concentrated directory first", sample)
	}
}

// The cap bounds what a fact NAMES, never what it counted — a fact that said
// "12 files" when it had seen 400 would be wrong, not merely brief.
func TestTheEvidenceCapBoundsNamingNotCounting(t *testing.T) {
	s := newPathSignals()
	for i := range 40 {
		s.observe("internal/auth/" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".go")
	}
	if got := len(s.sample(signalAuth)); got > maxSignalPaths {
		t.Errorf("sample named %d paths, over the %d cap", got, maxSignalPaths)
	}
	if got := s.total(signalAuth); got != 40 {
		t.Errorf("total = %d, want 40 — the cap must not lose the count", got)
	}
	item, ok := authModelInsight(testBase(), insightEvidence{Signals: s})
	if !ok {
		t.Fatal("no auth fact")
	}
	if !strings.Contains(item.Summary, "40 files") {
		t.Errorf("summary %q does not report the true count", item.Summary)
	}
}

// Every high-level fact must be anchored on paths, or an incremental pass can
// never invalidate it and it survives every change forever. The two aggregates
// that legitimately have no per-path anchor are the exceptions and are named.
func TestHighLevelFactsAreAnchoredOnEvidence(t *testing.T) {
	items := deriveInsights(testBase(), insightEvidence{
		Signals: signalsFor(
			"internal/auth/session.go", "internal/httpd/router.go",
			"internal/storage/queries/users.sql", "Dockerfile",
			"config/settings.yaml", "cmd/server/main.go", "go.mod",
		),
		GraphReady: true,
		Arch: codegraph.Architecture{
			Files: 10, Symbols: 20, Edges: 30, TableCount: 2, TestFiles: 3,
			Modules:      []codegraph.ModuleFacts{{Path: "internal", Files: 5}},
			Integrations: []codegraph.Count{{Name: "chi", Count: 2}},
		},
	}, insightScope{All: true})
	unanchored := map[domain.ProjectMemoryType]bool{
		// A testing-surface fact is two counts over the whole repository;
		// there is no subset of paths it is about.
		domain.MemoryTypeTestingSurface: true,
	}
	for _, item := range items {
		if unanchored[item.Key.Type] {
			continue
		}
		if len(item.SourcePaths) == 0 {
			t.Errorf("%s names no source paths, so nothing can ever invalidate it", item.Key.Type)
		}
	}
}

// Section 2: no configuration VALUES. The config fact carries names only.
func TestConfigFactCarriesKeysNotValues(t *testing.T) {
	item, ok := configSurfaceInsight(testBase(), insightEvidence{
		Signals:    signalsFor("config/settings.yaml"),
		GraphReady: true,
		Arch:       codegraph.Architecture{ConfigKeys: []string{"DATABASE_URL", "API_TOKEN"}},
	})
	if !ok {
		t.Fatal("no config fact")
	}
	if !strings.Contains(item.Content, "never records a configuration value") {
		t.Error("the config fact does not state that it holds no values")
	}
	// It is `observed` rather than `derived`: both halves are names AO read.
	if item.EvidenceClass != domain.EvidenceObserved {
		t.Errorf("evidence class = %q, want observed", item.EvidenceClass)
	}
}

// The per-repository bound must actually bind, or "bounded" is an intention.
func TestDerivationIsBounded(t *testing.T) {
	items := deriveInsights(testBase(), insightEvidence{
		Signals: signalsFor(
			"internal/auth/a.go", "internal/db/queries/x.sql", "internal/httpd/router.go",
			"Dockerfile", "config/x.yaml", "cmd/a/main.go", "go.mod",
		),
		GraphReady: true,
		Arch: codegraph.Architecture{
			Files: 1, Symbols: 1, Edges: 1, TableCount: 1, TestFiles: 1,
			Modules:      []codegraph.ModuleFacts{{Path: "internal"}},
			Integrations: []codegraph.Count{{Name: "x", Count: 1}},
			ConfigKeys:   []string{"K"},
		},
	}, insightScope{All: true})
	if len(items) > maxInsightItems {
		t.Fatalf("derived %d facts, over the %d cap", len(items), maxInsightItems)
	}
	for _, item := range items {
		if len(item.Content) > domain.MaxProjectMemoryContent {
			t.Errorf("%s body is %d bytes, over the cap", item.Key.Type, len(item.Content))
		}
	}
}

// Section 5, at the level that decides it: a change set narrows the derivation
// to the categories its paths are evidence of, and nothing else is derived at
// all.
//
// "Not derived" rather than "derived and found unchanged" is the requirement.
// Every fact carries the commit it was derived at, so restating an untouched
// fact after a commit still rewrites its row — the only way to leave the
// deployment fact alone when somebody edits auth code is not to derive it.
func TestScopeNarrowsDerivationToTheTouchedCategories(t *testing.T) {
	authOnly := insightScope{Kinds: map[signalKind]bool{signalAuth: true}}

	if !authOnly.includes(domain.MemoryTypeAuthModel) {
		t.Error("an auth change does not reach the auth fact")
	}
	for _, untouched := range []domain.ProjectMemoryType{
		domain.MemoryTypeDeployment, domain.MemoryTypePersistence,
		domain.MemoryTypeRuntimeSurface, domain.MemoryTypeArchitecture,
		domain.MemoryTypeTestingSurface, domain.MemoryTypeConfigSurface,
		domain.MemoryTypeIntegration, domain.MemoryTypeEntryPoint,
	} {
		if authOnly.includes(untouched) {
			t.Errorf("an auth change reaches the %s fact", untouched)
		}
	}

	// A graph rebuild moves the counts the graph-backed facts quote — and
	// nothing else. The auth and deployment facts are scan-backed and must sit
	// out a rebuild entirely.
	graphOnly := insightScope{Graph: true}
	for _, moved := range []domain.ProjectMemoryType{
		domain.MemoryTypeArchitecture, domain.MemoryTypeTestingSurface,
		domain.MemoryTypePersistence, domain.MemoryTypeRuntimeSurface,
	} {
		if !graphOnly.includes(moved) {
			t.Errorf("a graph rebuild does not reach the %s fact", moved)
		}
	}
	for _, still := range []domain.ProjectMemoryType{
		domain.MemoryTypeAuthModel, domain.MemoryTypeDeployment,
	} {
		if graphOnly.includes(still) {
			t.Errorf("a graph rebuild reaches the scan-backed %s fact", still)
		}
	}

	// A full pass restates everything, because it re-read everything.
	all := insightScope{All: true}
	for typ := range insightTriggers {
		if !all.includes(typ) {
			t.Errorf("a full pass skipped %s", typ)
		}
	}

	// And the derivation honours the narrowing, not just the predicate.
	items := deriveInsights(testBase(), insightEvidence{
		Signals: signalsFor("internal/auth/session.go", "Dockerfile", "internal/httpd/router.go"),
	}, authOnly)
	for _, item := range items {
		if item.Key.Type != domain.MemoryTypeAuthModel {
			t.Errorf("a scoped derivation produced a %s fact", item.Key.Type)
		}
	}
	if len(items) == 0 {
		t.Error("a scoped derivation produced nothing at all")
	}
}

// Every fact type this file produces must be in the trigger table, or an
// incremental pass can never refresh it and it goes permanently stale after
// the first full pass.
func TestEveryHighLevelTypeHasARefreshTrigger(t *testing.T) {
	produced := map[domain.ProjectMemoryType]bool{}
	for _, item := range deriveInsights(testBase(), insightEvidence{
		Signals: signalsFor(
			"internal/auth/a.go", "internal/db/queries/x.go", "internal/httpd/router.go",
			"Dockerfile", "config/x.yaml", "cmd/a/main.go", "go.mod",
		),
		GraphReady: true,
		Arch: codegraph.Architecture{
			Files: 1, Symbols: 1, Edges: 1, TableCount: 1, TestFiles: 1,
			Modules:      []codegraph.ModuleFacts{{Path: "internal"}},
			Integrations: []codegraph.Count{{Name: "x", Count: 1}},
			ConfigKeys:   []string{"K"},
		},
	}, insightScope{All: true}) {
		produced[item.Key.Type] = true
	}
	for typ := range produced {
		if _, ok := insightTriggers[typ]; !ok {
			t.Errorf("%s is derived but has no refresh trigger, so it can never be updated", typ)
		}
	}
}
