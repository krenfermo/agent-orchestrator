package skillrunner

import (
	"path"
	"regexp"
	"sort"
)

// scantools.go — the closed set of scan tools (Frente 2 / 2D).
//
// There is ONE scanning engine: an AO-authored POSIX-shell script in the
// digest-approved alpine image, with no network, a read-only mount, the
// boundary evidence first and the coverage accounting last (staticscan.go).
// A tool is a row in this table -- which files it stages, which files it
// reads, which rules it runs, what extra AO-authored section it adds, and what
// it cannot know. Adding a row is a code change and a release, exactly as
// adding a tool always was; a manifest still contributes nothing to a command
// line.
//
// Every tool keeps the one property the engine is built around: a finding is a
// RULE and a LOCATION, never the matched text. That is what lets a secret
// scanner exist at all without becoming a second copy of the secrets.

const (
	// ToolSecretScan finds committed credentials by their shape, and reports
	// the presence -- never the contents -- of files the manifest denies to
	// every run (.env, private keys).
	ToolSecretScan Tool = "ao.secret-scan/v1"
	// ToolDependencyScan inventories declared dependencies and reports what can
	// be established from the manifests and lockfiles alone. It consults no
	// advisory database, because it has none, and says so.
	ToolDependencyScan Tool = "ao.dependency-scan/v1"
)

// scanTool is one row of the closed tool table.
type scanTool struct {
	tool          Tool
	schemaVersion string
	rules         []scanRule
	// extensions and names select which STAGED files the rules read: by
	// extension, or by exact basename (Dockerfile, .npmrc). scanAll reads every
	// staged file, for a tool that stages nothing it does not read.
	extensions []string
	names      []string
	scanAll    bool
	// stageOnly, when set, keeps every other file off the mount entirely. A
	// dependency scan has no business receiving source code, and a copy that
	// was never made is a copy that cannot leak.
	stageOnly func(rel string) bool
	// reportDenied turns the manifest-denied files seen during staging into
	// findings, by path only. Those files are never read.
	reportDenied bool
	// extraSection is additional AO-authored shell, run after the rules
	// section; it may append findings to /tmp/extra_findings in the engine's
	// line protocol and print an AO_SECTION=inventory block.
	extraSection string
	// unparsedManifests are manifest basenames this tool recognises but does
	// not parse; their presence is stated as a limitation, not ignored.
	unparsedManifests []string
	limitations       []string
	params            ToolParams
}

// scanTools is the table. ToolStaticScan is the pre-2D behaviour, unchanged
// except that the manifest's deny list is now applied to its staging too.
var scanTools = map[Tool]scanTool{
	ToolStaticScan: {
		tool:          ToolStaticScan,
		schemaVersion: "ao.static-scan/v1",
		rules:         staticScanRules,
		extensions:    scannedExtensions,
		limitations:   staticScanLimitations,
		params:        ToolParams{MaxFiles: 2000, MaxFileBytes: 512 << 10},
	},
	ToolSecretScan: {
		tool:          ToolSecretScan,
		schemaVersion: "ao.secret-scan/v1",
		rules:         secretScanRules,
		extensions: append(append([]string(nil), scannedExtensions...),
			"txt", "md", "xml", "properties", "cfg", "config", "sql", "html", "vue", "kt", "swift", "gradle", "tfvars"),
		names:        []string{"Dockerfile", ".npmrc", ".yarnrc", ".pypirc", ".netrc", ".dockercfg", ".git-credentials"},
		reportDenied: true,
		limitations:  secretScanLimitations,
		params:       ToolParams{MaxFiles: 5000, MaxFileBytes: 1 << 20},
	},
	ToolDependencyScan: {
		tool:          ToolDependencyScan,
		schemaVersion: "ao.dependency-scan/v1",
		rules:         dependencyScanRules,
		scanAll:       true,
		stageOnly:     isDependencyFile,
		extraSection:  dependencySection,
		unparsedManifests: []string{
			"Gemfile", "Gemfile.lock", "pyproject.toml", "Pipfile", "Pipfile.lock", "poetry.lock",
			"pom.xml", "build.gradle", "build.gradle.kts", "composer.json", "composer.lock",
			"packages.config", "Directory.Packages.props", "mix.exs", "pubspec.yaml",
		},
		limitations: dependencyScanLimitations,
		// Lockfiles are routinely megabytes; one skipped as too large would
		// read as "no lockfile" to a reader, so the per-file bound is raised
		// for this tool (still inside ToolParams.Validate's range).
		params: ToolParams{MaxFiles: 2000, MaxFileBytes: 32 << 20},
	},
}

// DefaultParamsFor is the params a tool runs with when the caller supplies none.
func DefaultParamsFor(tool Tool) ToolParams {
	if t, ok := scanTools[tool]; ok {
		return t.params
	}
	return DefaultToolParams()
}

// ---------------------------------------------------------------------------
// secret scan
// ---------------------------------------------------------------------------

// secretScanRules are credential SHAPES. The engine prints the rule id and the
// location of a match, never the matched text.
var secretScanRules = []scanRule{
	{ID: "SEC-001", Category: "secret", Severity: "critical", Confidence: "probable",
		Title:          "AWS access key id",
		Pattern:        `(AKIA|ASIA)[0-9A-Z]{16}`,
		Recommendation: "Deactivate the key in IAM first, then remove it from the tree and from history."},
	{ID: "SEC-002", Category: "secret", Severity: "critical", Confidence: "probable",
		Title:          "Private key block",
		Pattern:        `-----BEGIN ([A-Z0-9]+ )*PRIVATE KEY-----`,
		Recommendation: "Revoke and reissue the key pair, then remove the file from the tree and from history."},
	{ID: "SEC-003", Category: "secret", Severity: "critical", Confidence: "probable",
		Title:          "GitHub token",
		Pattern:        `(gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,})`,
		Recommendation: "Revoke the token on GitHub, then remove it from the tree and from history."},
	{ID: "SEC-004", Category: "secret", Severity: "critical", Confidence: "probable",
		Title:          "GitLab personal access token",
		Pattern:        `glpat-[A-Za-z0-9_-]{20}`,
		Recommendation: "Revoke the token in GitLab, then remove it from the tree and from history."},
	{ID: "SEC-005", Category: "secret", Severity: "high", Confidence: "probable",
		Title:          "Slack token",
		Pattern:        `xox[abprs]-[A-Za-z0-9-]{10,}`,
		Recommendation: "Revoke the token in the Slack app settings, then remove it from the tree and from history."},
	{ID: "SEC-006", Category: "secret", Severity: "critical", Confidence: "probable",
		Title:          "Stripe live secret or restricted key",
		Pattern:        `(sk|rk)_live_[0-9A-Za-z]{20,}`,
		Recommendation: "Roll the key in the Stripe dashboard, then remove it from the tree and from history."},
	{ID: "SEC-007", Category: "secret", Severity: "high", Confidence: "probable",
		Title:          "Google API key",
		Pattern:        `AIza[0-9A-Za-z_-]{35}`,
		Recommendation: "Delete or restrict the key in the Google Cloud console, then remove it from the tree."},
	{ID: "SEC-008", Category: "secret", Severity: "critical", Confidence: "probable",
		Title:          "AI provider API key",
		Pattern:        `sk-(ant-api[0-9]{2}-|proj-)[A-Za-z0-9_-]{20,}`,
		Recommendation: "Revoke the key in the provider console, then remove it from the tree and from history."},
	{ID: "SEC-009", Category: "secret", Severity: "medium",
		Title:          "JSON Web Token",
		Pattern:        `eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,
		Recommendation: "If it is a live token, rotate the signing key or revoke the session; keep test tokens obviously fake."},
	{ID: "SEC-010", Category: "secret", Severity: "high",
		Title:          "Credentials embedded in a URL",
		Pattern:        `[a-zA-Z][a-zA-Z0-9+.-]*://[^[:space:]/:@"]+:[^[:space:]/@"]{3,}@[^[:space:]/]`,
		Recommendation: "Rotate the password and read the URL (or its password) from a secret store at runtime."},
	{ID: "SEC-011", Category: "secret", Severity: "high",
		Title:          "Credential-shaped literal assigned in source",
		Pattern:        ruleIn(staticScanRules, "AOSS-006").Pattern, // the same shape, by construction
		Recommendation: "Rotate the credential first, then remove it from the tree AND from history."},
	{ID: "SEC-012", Category: "secret", Severity: "high",
		Title:          "Package registry auth token in an rc file",
		Pattern:        `_auth(Token)?[[:space:]]*=[[:space:]]*[^[:space:]$]`,
		Recommendation: "Revoke the token and reference it from the environment (${NPM_TOKEN}) instead of the file."},
	{ID: "SEC-013", Category: "secret", Severity: "medium",
		Title:          "Slack incoming webhook URL",
		Pattern:        `hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`,
		Recommendation: "Regenerate the webhook and keep its URL in a secret store."},
}

// deniedFileRule is the host-side finding for a file the manifest keeps out of
// every run. The file is NOT read -- its presence is the finding.
var deniedFileRule = scanRule{
	ID: "SEC-100", Category: "secret", Severity: "medium",
	Title: "Credential-bearing file present in the checkout (not read)",
	Recommendation: "Confirm the file is not committed (check git history, not only .gitignore); " +
		"if it ever was, rotate what it holds.",
}

var secretScanLimitations = []string{
	"This scan matches credential SHAPES. A secret with no recognisable shape (a plain password in an " +
		"unusual variable) is only found when it is assigned to a credential-named identifier.",
	"Findings carry the rule and the location, never the matched value. Confirming one means opening the file.",
	"Files the manifest denies (.env, private keys) are never read; their presence is reported as SEC-100.",
	"Only the working tree is scanned. A secret removed from the tree but still in git history is not found.",
	"An empty findings list means these rules matched nothing in the files listed as scanned. " +
		"It is not evidence that the project holds no secrets.",
}

// ---------------------------------------------------------------------------
// dependency scan
// ---------------------------------------------------------------------------

// dependencyScanRules describe the findings the dependency section emits. They
// have no grep pattern -- the AO-authored awk program decides them -- and each
// is a fact read off a manifest, hence "confirmed". None of them is a claim
// about a vulnerability.
var dependencyScanRules = []scanRule{
	{ID: "DEP-001", Category: "supply-chain", Severity: "high", Confidence: "confirmed",
		Title: "Dependency installed from a git or URL source instead of a registry",
		Recommendation: "Consume it from the registry at a pinned version, or pin the source to an immutable " +
			"commit and review it like first-party code."},
	{ID: "DEP-002", Category: "dependency", Severity: "medium", Confidence: "confirmed",
		Title:          "Dependency version is not pinned (wildcard, latest, or none)",
		Recommendation: "Pin an explicit version or range and commit a lockfile."},
	{ID: "DEP-003", Category: "dependency", Severity: "medium", Confidence: "confirmed",
		Title:          "Manifest declares dependencies but no lockfile is present",
		Recommendation: "Generate and commit the lockfile so every install resolves the same versions."},
	{ID: "DEP-004", Category: "supply-chain", Severity: "high", Confidence: "confirmed",
		Title:          "Package source or registry fetched over plain HTTP",
		Recommendation: "Use the registry over HTTPS; remove --trusted-host and http:// index URLs."},
}

var dependencyScanLimitations = []string{
	"No vulnerability database is consulted: this scan has no advisory data, so it reports no known " +
		"vulnerabilities and must not be read as saying there are none.",
	"Only npm (package.json and its lockfiles), Go (go.mod/go.sum), pip (requirements*.txt) and " +
		"Cargo (Cargo.toml) manifests are parsed; other manifests present are listed as unparsed.",
	"Manifests are read line by line. A package.json whose dependency list sits on a single line " +
		"(minified) is not inventoried.",
	"Only manifests and lockfiles are staged; the dependency scan never receives source code.",
}

// dependencyFileNames are staged by the dependency scan. requirements*.txt is
// matched by pattern in isDependencyFile.
var dependencyFileNames = map[string]bool{
	"package.json": true, "package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true,
	"pnpm-lock.yaml": true, ".npmrc": true, ".yarnrc": true,
	"go.mod": true, "go.sum": true, "Cargo.toml": true, "Cargo.lock": true,
}

var requirementsRe = regexp.MustCompile(`^requirements.*\.txt$`)

func isDependencyFile(rel string) bool {
	base := path.Base(rel)
	return dependencyFileNames[base] || requirementsRe.MatchString(base)
}

// lockfilesFor is which lockfiles satisfy a manifest ecosystem, by basename.
var lockfilesFor = map[string][]string{
	"npm": {"package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml"},
	"go":  {"go.sum"},
}

// dependencySection parses the staged manifests with an AO-authored awk
// program. It prints the inventory (INV lines) and the manifests that declare
// dependencies (MAN lines) to stdout, and appends DEP findings to
// /tmp/extra_findings in the engine's own line protocol.
//
// Everything it prints about a dependency passes through clean(): control
// characters become spaces, the userinfo of a URL is replaced -- a git URL can
// carry a token -- and the value is bounded. AO redacts again on the host.
const dependencySection = `
echo "AO_SECTION=inventory"
cat <<'AO_DEPS_AWK_EOF' > /tmp/deps.awk
function rel(p) { sub(/^\/work\//, "", p); return p }
function clean(v) {
  gsub(/[[:cntrl:]]/, " ", v)
  gsub(/:\/\/[^@\/ ]*@/, "://[REDACTED]@", v)
  if (length(v) > 200) v = substr(v, 1, 200)
  return v
}
function inv(eco, name, ver, kind) {
  printf "INV\037%s\037%s\037%s\037%s:%d\037%s\n", eco, clean(name), clean(ver), rel(FILENAME), FNR, kind
}
function man(eco,   key) {
  key = eco ":" FILENAME
  if (!(key in mans)) { mans[key] = 1; printf "MAN\037%s\037%s:%d\n", eco, rel(FILENAME), FNR }
}
function find(rid) { printf "%s\037%s:%d\n", rid, FILENAME, FNR >> "/tmp/extra_findings" }
FNR == 1 { b = FILENAME; sub(/^.*\//, "", b); sec = ""; inreq = 0 }

b == "package.json" {
  line = $0
  if (line ~ /"(dependencies|devDependencies|optionalDependencies|peerDependencies)"[ \t]*:[ \t]*\{/) {
    if (line ~ /\{[ \t]*\}/) next
    sec = line; sub(/^[^"]*"/, "", sec); sub(/".*$/, "", sec)
    next
  }
  if (sec != "") {
    if (line ~ /^[ \t]*\}/) { sec = ""; next }
    if (line ~ /^[ \t]*"[^"]+"[ \t]*:[ \t]*"[^"]*"/) {
      name = line; sub(/^[ \t]*"/, "", name); sub(/".*$/, "", name)
      spec = line; sub(/^[ \t]*"[^"]+"[ \t]*:[ \t]*"/, "", spec); sub(/".*$/, "", spec)
      kind = "direct"; if (sec != "dependencies") kind = sec
      man("npm")
      inv("npm", name, spec, kind)
      if (spec ~ /^(git\+|git:|github:|gitlab:|bitbucket:|https?:)/ || spec ~ /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+(#.*)?$/) find("DEP-001")
      if (spec == "*" || spec == "latest" || spec == "" || spec == "x") find("DEP-002")
    }
  }
  next
}

b == "go.mod" {
  line = $0; sub(/\/\/.*$/, "", line)
  ind = ($0 ~ /\/\/[ \t]*indirect/)
  if (line ~ /^[ \t]*require[ \t]*\([ \t]*$/) { inreq = 1; next }
  if (inreq && line ~ /^[ \t]*\)/) { inreq = 0; next }
  if (line ~ /^[ \t]*require[ \t]+[^ \t(]/) { sub(/^[ \t]*require[ \t]+/, "", line); gomod(line, ind); next }
  if (inreq && line ~ /[^ \t]/) { gomod(line, ind) }
  next
}
function gomod(l, ind,   n, parts, kind) {
  gsub(/^[ \t]+|[ \t]+$/, "", l)
  n = split(l, parts, /[ \t]+/)
  if (n < 2) return
  kind = "direct"; if (ind) kind = "indirect"
  man("go")
  inv("go", parts[1], parts[2], kind)
}

b ~ /^requirements.*\.txt$/ {
  line = $0; sub(/(^|[ \t])#.*$/, "", line); gsub(/^[ \t]+|[ \t]+$/, "", line)
  if (line == "") next
  if (line ~ /^--?(index-url|extra-index-url|i)([ =]|$)/) { if (line ~ /http:\/\//) find("DEP-004"); next }
  if (line ~ /^--trusted-host/) { find("DEP-004"); next }
  if (line ~ /(git\+|https?:\/\/)/) { inv("pypi", line, "(url)", "direct"); find("DEP-001"); if (line ~ /http:\/\//) find("DEP-004"); next }
  if (line ~ /^-/) next
  name = line; sub(/[ \t]*[<>=!~;\[@].*$/, "", name)
  ver = ""
  if (match(line, /==[^ ;,]+/)) ver = substr(line, RSTART + 2, RLENGTH - 2)
  if (ver == "") { inv("pypi", name, "(unpinned)", "direct"); find("DEP-002") } else inv("pypi", name, ver, "direct")
  next
}

b == "Cargo.toml" {
  line = $0; sub(/#.*$/, "", line)
  if (line ~ /^[ \t]*\[/) { sec = ""; if (line ~ /dependencies\][ \t]*$/) sec = "deps"; next }
  if (sec != "" && line ~ /^[ \t]*[A-Za-z0-9_-]+[ \t]*=/) {
    name = line; sub(/^[ \t]*/, "", name); sub(/[ \t]*=.*$/, "", name)
    spec = line; sub(/^[^=]*=[ \t]*/, "", spec)
    ver = "(unversioned)"
    if (spec ~ /^"/) { ver = spec; gsub(/"/, "", ver) }
    else if (match(spec, /version[ \t]*=[ \t]*"[^"]*"/)) { ver = substr(spec, RSTART, RLENGTH); sub(/^[^"]*"/, "", ver); sub(/"$/, "", ver) }
    inv("cargo", name, ver, "direct")
    if (spec ~ /git[ \t]*=/) find("DEP-001")
    if (ver == "*") find("DEP-002")
  }
  next
}

b == "package-lock.json" || b == "npm-shrinkwrap.json" { if ($0 ~ /"resolved"[ \t]*:[ \t]*"http:\/\//) find("DEP-004"); next }
b == "yarn.lock" { if ($0 ~ /resolved[ \t]+"?http:\/\//) find("DEP-004"); next }
b == "pnpm-lock.yaml" { if ($0 ~ /tarball:[ \t]*http:\/\//) find("DEP-004"); next }
b == ".npmrc" || b == ".yarnrc" { if ($0 ~ /registry[ \t]*[= ][ \t]*"?http:\/\//) find("DEP-004"); next }
AO_DEPS_AWK_EOF
if [ "$scanned" -gt 0 ]; then
  # shellcheck disable=SC2046
  awk -f /tmp/deps.awk $(cat /tmp/scan_files | tr '\n' ' ') 2>/dev/null || echo "ao_dependency_parse_failed=1"
fi
`

// ---------------------------------------------------------------------------
// what a tool saw on the host while staging
// ---------------------------------------------------------------------------

// stageObservation is what the staging walk saw for one run, recorded by the
// Exclude hook. It is how a tool can report the presence of a file it was
// never given (a denied .env) or check a lockfile that was too large to stage.
type stageObservation struct {
	seen   map[string]bool
	denied []string
}

func (o *stageObservation) exclude(tool scanTool, deny DenyGlobs) func(rel string) string {
	return func(rel string) string {
		o.seen[rel] = true
		if deny.Match(rel) {
			o.denied = append(o.denied, rel)
			return SkipReasonDenied
		}
		if tool.stageOnly != nil && !tool.stageOnly(rel) {
			return SkipReasonOutOfToolScope
		}
		return ""
	}
}

// SkipReasonOutOfToolScope is a file the tool does not stage at all (the
// dependency scan stages only manifests and lockfiles).
const SkipReasonOutOfToolScope = "not_used_by_this_tool"

// maxDeniedFindings bounds the SEC-100 findings one run can produce.
const maxDeniedFindings = 200

// hostFindings are the findings a tool derives from what staging SAW rather
// than from what the container read.
func (o *stageObservation) hostFindings(tool scanTool, manifests []manifestRef) []ScanFinding {
	var out []ScanFinding
	if tool.reportDenied {
		denied := append([]string(nil), o.denied...)
		sort.Strings(denied)
		for i, rel := range denied {
			if i == maxDeniedFindings {
				break
			}
			out = append(out, findingFor(deniedFileRule, rel, 0))
		}
	}
	for _, m := range manifests {
		locks, ok := lockfilesFor[m.ecosystem]
		if !ok {
			continue
		}
		dir := path.Dir(m.path)
		found := false
		for _, lock := range locks {
			if o.seen[path.Join(dir, lock)] {
				found = true
				break
			}
		}
		if !found {
			out = append(out, findingFor(ruleIn(dependencyScanRules, "DEP-003"), m.path, m.line))
		}
	}
	return out
}

// unparsedPresent lists the recognised-but-unparsed manifests staging saw.
func (o *stageObservation) unparsedPresent(tool scanTool) []string {
	if len(tool.unparsedManifests) == 0 {
		return nil
	}
	names := map[string]bool{}
	for _, n := range tool.unparsedManifests {
		names[n] = true
	}
	var out []string
	for rel := range o.seen {
		if names[path.Base(rel)] {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

type manifestRef struct {
	ecosystem string
	path      string
	line      int
}

func findingFor(rule scanRule, rel string, line int) ScanFinding {
	return ScanFinding{
		RuleID: rule.ID, Severity: rule.Severity, Category: rule.Category, Title: rule.Title,
		Path: rel, Line: line, Recommendation: rule.Recommendation, Confidence: rule.confidence(),
	}
}

func ruleIn(rules []scanRule, id string) scanRule {
	for _, r := range rules {
		if r.ID == id {
			return r
		}
	}
	return scanRule{ID: id}
}
