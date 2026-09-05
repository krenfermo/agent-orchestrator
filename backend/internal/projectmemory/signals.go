package projectmemory

import (
	"path"
	"sort"
	"strings"
)

// signals.go — the bounded path census the high-level facts are derived from
// (P4-H §2, §7).
//
// The indexing pass already visits every admitted path. This file is what it
// notices while it is there: which paths are evidence of authentication, of
// persistence, of a runtime surface, of a deployment. Nothing here reads a
// file. It is pure path classification, which is why it costs the walk
// nothing and why it is honest about what it is — a NAMING signal, recorded as
// such, and never promoted to a claim about behaviour.
//
// Two rules keep it from becoming a second file classifier:
//
//   - It never decides what is admitted. classifyPath (derive.go) does that,
//     and this runs only on paths that pass. A category here can therefore
//     never widen the walk.
//   - It never carries content. A signal is a path and a category. What the
//     path contains stays the repository's business, which is what makes
//     "memory holds no secrets" a structural property rather than a filter
//     somebody has to keep current.
//
// The exclusion list is the one place it does refuse a path outright, and it
// refuses the one class of file whose NAME alone is a liability to repeat: a
// live credential store. `.env.example` is a template and is kept; `.env` is
// not.

// signalKind is what a path is evidence of.
type signalKind string

const (
	// signalAuth is evidence of where identity is established or permission
	// is decided.
	signalAuth signalKind = "auth"
	// signalPersistence is evidence of the storage architecture: schema,
	// migrations, named queries, repositories.
	signalPersistence signalKind = "persistence"
	// signalRuntime is evidence of the served surface: routers, controllers,
	// handlers, the HTTP/RPC layer.
	signalRuntime signalKind = "runtime"
	// signalDeployment is evidence of how the project is packaged, shipped
	// and run: containers, orchestration, CI/CD, release manifests.
	signalDeployment signalKind = "deployment"
	// signalConfig is evidence of the configuration surface.
	signalConfig signalKind = "config"
	// signalEntry is evidence of a process entry point.
	signalEntry signalKind = "entry"
	// signalManifest is a dependency manifest, kept so the integrations fact
	// has something whose change can invalidate it.
	signalManifest signalKind = "manifest"
)

const (
	// maxSignalPaths caps how many paths one fact NAMES.
	//
	// It is a cap on what is rendered, not on what was counted: totals keep
	// rising after it binds, so a fact can honestly say "authorisation is
	// decided across 40 files, of these 12". Keeping all forty would put a
	// directory listing into a pack that is supposed to be a summary.
	maxSignalPaths = 12
	// maxSignalCandidates caps how many paths one category REMEMBERS while it
	// decides which twelve to name. It is larger because the selection is by
	// concentration (see rank), and concentration cannot be computed from a
	// sample taken in walk order — which is alphabetical, and therefore
	// picked whichever directory happened to sort first.
	//
	// The first operational run showed exactly that failure: AO's own auth
	// fact named backend/internal/adapters/agent/{agy,aider,amp} — three
	// adapters with one auth file each — while the packages that actually
	// decide permissions sorted later and never appeared.
	maxSignalCandidates = 400
)

// pathSignals is the census one pass accumulates.
//
// The zero value is NOT usable; construct with newPathSignals. Every method
// tolerates a nil receiver instead, which is what the degraded paths need.
type pathSignals struct {
	// candidates holds the paths seen in each category, up to
	// maxSignalCandidates, so the ranking below has something to rank.
	candidates map[signalKind][]string
	totals     map[signalKind]int
	// dirs counts matches per directory, UNBOUNDED by the candidate cap: the
	// count is a small integer and the concentration answer has to be right
	// even for a category with thousands of matches.
	dirs map[signalKind]map[string]int
}

func newPathSignals() *pathSignals {
	return &pathSignals{
		candidates: map[signalKind][]string{},
		totals:     map[signalKind]int{},
		dirs:       map[signalKind]map[string]int{},
	}
}

// observe classifies one admitted repo-relative path and records it.
//
// A path may be evidence of more than one thing — `internal/auth/store.go` is
// both auth and persistence — and both are recorded. Forcing a single category
// would make the fact that loses the tie-break invisible, and there is no
// tie-break that is right for every repository.
func (s *pathSignals) observe(rel string) {
	if s == nil || rel == "" {
		return
	}
	if excludedFromSignals(rel) {
		return
	}
	dir := path.Dir(rel)
	if dir == "" {
		dir = "."
	}
	for _, kind := range signalKindsOf(rel) {
		s.totals[kind]++
		if s.dirs[kind] == nil {
			s.dirs[kind] = map[string]int{}
		}
		s.dirs[kind][dir]++
		if len(s.candidates[kind]) < maxSignalCandidates {
			s.candidates[kind] = append(s.candidates[kind], rel)
		}
	}
}

// sample returns the evidence paths a fact should name: the ones in the
// directories where the category actually concentrates, most-concentrated
// directory first and alphabetical within it.
//
// Concentration rather than walk order is what makes the sample an ANSWER
// instead of a listing. "The auth code is in internal/oidc and internal/rbac"
// is what somebody asked; "here are the twelve alphabetically-first files whose
// name contains auth" is not, even though both are twelve true paths.
//
// The order is deterministic — count descending, then directory name, then
// path — so two passes over an unchanged repository derive byte-identical
// facts and the idempotent upsert reconfirms rather than rewrites.
func (s *pathSignals) sample(kind signalKind) []string {
	if s == nil || len(s.candidates[kind]) == 0 {
		return nil
	}
	counts := s.dirs[kind]
	rank := func(p string) int {
		dir := path.Dir(p)
		if dir == "" {
			dir = "."
		}
		return counts[dir]
	}
	out := append([]string(nil), s.candidates[kind]...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri > rj
		}
		di, dj := path.Dir(out[i]), path.Dir(out[j])
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	if len(out) > maxSignalPaths {
		out = out[:maxSignalPaths]
	}
	return out
}

// dirRanking returns the category's directories, most matches first. It is
// what a summary line names, and it is computed from every match rather than
// from the sample, so "concentrated in X" is true of the whole category.
func (s *pathSignals) dirRanking(kind signalKind) []string {
	if s == nil || len(s.dirs[kind]) == 0 {
		return nil
	}
	counts := s.dirs[kind]
	dirs := make([]string, 0, len(counts))
	for d := range counts {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if counts[dirs[i]] != counts[dirs[j]] {
			return counts[dirs[i]] > counts[dirs[j]]
		}
		return dirs[i] < dirs[j]
	})
	return dirs
}

// total reports how many paths a category matched, including those the cap
// dropped.
func (s *pathSignals) total(kind signalKind) int {
	if s == nil {
		return 0
	}
	return s.totals[kind]
}

// excludedFromSignals refuses the paths whose name alone should not be
// repeated into a durable fact, and the trees whose contents are not the
// project's own code.
//
// `.env` is the point of the list. A memory item that says "configuration
// lives in .env, .env.production and .env.staging" is a map of where this
// installation's secrets are, written into a store that is read back into
// agent prompts. The templates (`.env.example`, `.env.sample`) carry no values
// and are exactly what a newcomer should be pointed at, so they stay.
func excludedFromSignals(rel string) bool {
	lower := strings.ToLower(rel)
	base := path.Base(lower)

	switch {
	case base == ".env" || (strings.HasPrefix(base, ".env.") &&
		!strings.HasSuffix(base, ".example") && !strings.HasSuffix(base, ".sample") &&
		!strings.HasSuffix(base, ".template")):
		return true
	case strings.HasSuffix(base, ".pem"), strings.HasSuffix(base, ".key"),
		strings.HasSuffix(base, ".p12"), strings.HasSuffix(base, ".pfx"),
		strings.HasSuffix(base, ".keystore"), strings.HasSuffix(base, ".jks"):
		return true
	case strings.HasPrefix(base, "credentials"), strings.HasPrefix(base, "secrets"),
		base == "id_rsa", base == "id_ed25519", base == ".netrc", base == ".npmrc":
		return true
	}
	for _, seg := range strings.Split(lower, "/") {
		switch seg {
		// Not the project's own code: third-party trees, build output, and
		// caches.
		case "node_modules", "vendor", "dist", "build", "out", "target",
			".git", ".cache", ".next", ".nuxt", ".venv", "venv", "env",
			"__pycache__", "site-packages", "coverage", "testdata", "fixtures":
			return true
		// Agent scratch space. `.claude/worktrees` in particular holds whole
		// COPIES of the repository, and a fact derived from one names paths
		// that describe an agent's temporary checkout rather than the
		// project — the medusa run before this exclusion reported its entry
		// points as living under .claude/worktrees/roc-capacity-fe/.
		case ".claude", ".cursor", ".aider", ".ao", ".idea", ".vscode",
			".gradle", ".mvn", "nbproject":
			return true
		// Credential stores by DIRECTORY as well as by filename. The
		// basename rules above catch `config/credentials.json`; they do not
		// catch `secrets/tokens.yaml`, whose name says nothing and whose
		// directory says everything.
		case "secrets", ".secrets", "credentials", ".credentials", ".ssh", ".gnupg":
			return true
		}
	}
	return false
}

// subsystemExts are the file extensions a SUBSYSTEM signal is allowed to come
// from: auth, persistence, runtime and entry points are claims about code, and
// only code can be evidence for them.
//
// This is the fix for the other half of what the first real-project run
// showed. Without it a `docs/authorization.md` and an `artifacts/auth.json`
// counted as places authorisation is decided, and the resulting fact pointed a
// reader at documentation and build output instead of at the three files that
// actually decide a permission.
//
// The other categories keep their own rules: a Dockerfile has no extension, a
// config file is a config file whatever it is written in, and a manifest is
// named rather than typed.
var subsystemExts = map[string]struct{}{
	".go": {}, ".ts": {}, ".tsx": {}, ".js": {}, ".jsx": {}, ".mjs": {}, ".cjs": {},
	".py": {}, ".rs": {}, ".java": {}, ".kt": {}, ".rb": {}, ".php": {},
	".cs": {}, ".swift": {}, ".scala": {}, ".ex": {}, ".exs": {}, ".sql": {},
	".c": {}, ".cc": {}, ".cpp": {}, ".h": {}, ".hpp": {}, ".vue": {}, ".svelte": {},
}

// signalKindsOf is the whole classifier: which categories a path is evidence
// of, by name.
//
// Every rule below is a NAMING convention, and the confidence the derived
// facts carry says so. A directory called `auth` is very likely where
// authentication lives and is occasionally a helper nobody uses; the fact AO
// writes from it is labelled derived and carries the inferred confidence, so a
// reader is never told AO proved something it guessed.
func signalKindsOf(rel string) []signalKind {
	lower := strings.ToLower(rel)
	base := path.Base(lower)
	ext := path.Ext(lower)
	segs := strings.Split(path.Dir(lower), "/")
	stem := strings.TrimSuffix(base, ext)

	_, isCode := subsystemExts[ext]

	var out []signalKind
	add := func(k signalKind) {
		for _, existing := range out {
			if existing == k {
				return
			}
		}
		out = append(out, k)
	}

	// Tests describe the verification surface, never the subsystem they
	// exercise. `auth_test.go` must not be counted as a place authorisation
	// is decided — the whole value of the auth fact is that it names the
	// files a reader should open first.
	if isTestPath(lower, base, stem) {
		return nil
	}

	hasSeg := func(names ...string) bool {
		for _, seg := range segs {
			for _, n := range names {
				if seg == n {
					return true
				}
			}
		}
		return false
	}
	nameHas := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(stem, sub) {
				return true
			}
		}
		return false
	}

	if isCode && (hasSeg("auth", "authn", "authz", "oidc", "oauth", "rbac", "iam", "identity",
		"permissions", "authorization", "authentication", "tenants", "tenancy") ||
		nameHas("auth", "oidc", "oauth", "rbac", "permission", "credential",
			"session_guard", "authorize", "authorization", "login", "jwt", "tenant")) {
		add(signalAuth)
	}
	if (isCode && (hasSeg("migrations", "migration", "queries", "schema", "models", "entities",
		"repositories", "storage", "persistence", "dao") ||
		nameHas("_store", "repository", "migration"))) ||
		base == "schema.sql" || base == "schema.prisma" || base == "sqlc.yaml" {
		add(signalPersistence)
	}
	if isCode && (hasSeg("routes", "router", "routers", "controllers", "handlers", "endpoints",
		"resolvers", "api", "httpd", "http", "server", "rpc", "grpc") ||
		nameHas("router", "routes", "controller", "handler", "endpoint", "server")) {
		add(signalRuntime)
	}
	if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") ||
		strings.HasPrefix(base, "docker-compose") ||
		strings.HasPrefix(lower, ".github/workflows/") ||
		strings.HasPrefix(lower, ".gitlab-ci") || base == ".gitlab-ci.yml" ||
		hasSeg("k8s", "kubernetes", "helm", "charts", "terraform", "deploy",
			"deployment", "infra", "infrastructure", "ansible", "pulumi") ||
		base == "procfile" || base == "fly.toml" || base == "vercel.json" ||
		base == "render.yaml" || base == "app.yaml" || base == "cloudbuild.yaml" ||
		base == "jenkinsfile" {
		add(signalDeployment)
	}
	if base == ".env.example" || base == ".env.sample" || base == ".env.template" ||
		base == "config.yaml" || base == "config.yml" || base == "config.toml" ||
		base == "config.json" || base == "settings.py" || base == "application.yml" ||
		base == "application.yaml" || base == "appsettings.json" ||
		hasSeg("config", "configs", "settings") ||
		strings.HasSuffix(stem, ".config") {
		add(signalConfig)
	}
	if _, ok := entrypointNames[base]; ok && isCode {
		add(signalEntry)
	}
	if hasSeg("cmd") && ext == ".go" {
		add(signalEntry)
	}
	if _, ok := manifestNames[base]; ok {
		add(signalManifest)
	}
	return out
}

// isTestPath recognises the test-file conventions of the languages the
// indexer admits. It mirrors internal/codegraph's classifier rather than
// importing it: this one runs over paths the MEMORY walk admitted, which is a
// wider set than the graph parses, and coupling the two would make a change to
// what the graph can parse silently change what memory believes about tests.
func isTestPath(lower, base, stem string) bool {
	switch {
	case strings.HasSuffix(base, "_test.go"), strings.HasSuffix(base, "_test.py"),
		strings.HasSuffix(base, "_test.rb"), strings.HasSuffix(base, "_spec.rb"):
		return true
	case strings.HasSuffix(stem, ".test"), strings.HasSuffix(stem, ".spec"):
		return true
	case strings.HasPrefix(base, "test_"):
		return true
	}
	for _, seg := range strings.Split(lower, "/") {
		switch seg {
		case "test", "tests", "__tests__", "spec", "e2e":
			return true
		}
	}
	return false
}
