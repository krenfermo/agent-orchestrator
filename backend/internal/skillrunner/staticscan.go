package skillrunner

import (
	"fmt"
	"strings"
)

// staticscan.go — the ao.static-scan/v1 tool.
//
// It is a POSIX-shell pattern scanner AO authors, running in a minimal image
// with no network and a read-only mount. What it is not, and what the report
// it emits says plainly, is a SAST engine: it matches patterns, it does not
// parse, it cannot follow a value to its sink, and it therefore proves nothing
// about the absence of a vulnerability.
//
// That honesty is the feature. A scanner that reports "no vulnerabilities" for
// a language it never had a rule for is worse than one that reports what it
// looked at, so every finding carries the rule that produced it and the report
// carries the rules that ran and the files that were skipped.
//
// The script takes NO value from a manifest. Two integers arrive from AO after
// range validation, and they are the only variable part of the command line.

// scanRule is one pattern and what a match means. The rules are deliberately
// few and high-signal: a scanner with a hundred noisy rules produces a report
// nobody reads, which is the same as no report.
type scanRule struct {
	ID string
	// Category matches the security-audit findings schema.
	Category string
	Severity string
	Title    string
	// Pattern is an extended regular expression for grep -E.
	Pattern string
	// Recommendation is one concrete change.
	Recommendation string
}

// staticScanRules is the closed rule set. Each is a shape that is nearly always
// worth a human look, and each says what it cannot know.
var staticScanRules = []scanRule{
	{
		ID: "AOSS-001", Category: "injection", Severity: "high",
		Title:   "SQL built by string concatenation or interpolation",
		Pattern: `(SELECT|INSERT|UPDATE|DELETE)[[:space:]].*(\+[[:space:]]*[a-zA-Z_]|\$\{|%s|\|\|[[:space:]]*[a-zA-Z_])`,
		Recommendation: "Use a parameterized query or a prepared statement; never concatenate " +
			"request data into SQL.",
	},
	{
		ID: "AOSS-002", Category: "injection", Severity: "high",
		Title:   "Shell command built from a variable",
		Pattern: `(exec\.Command|os\.system|subprocess\.(call|run|Popen)|child_process\.(exec|execSync))[[:space:]]*\(.*(\+|\$\{|%s|f")`,
		Recommendation: "Pass arguments as a list to the process API instead of building a " +
			"command string, and never interpolate request data into a shell.",
	},
	{
		ID: "AOSS-003", Category: "injection", Severity: "high",
		Title:          "Dynamic code evaluation",
		Pattern:        `(^|[^a-zA-Z_.])(eval|exec)[[:space:]]*\(|new[[:space:]]+Function[[:space:]]*\(`,
		Recommendation: "Remove the dynamic evaluation, or replace it with an explicit dispatch table.",
	},
	{
		ID: "AOSS-004", Category: "crypto", Severity: "medium",
		Title:          "Weak hash used where a strong one is expected",
		Pattern:        `(md5|sha1)\.(New|Sum)|hashlib\.(md5|sha1)|createHash\(['\"](md5|sha1)['\"]\)`,
		Recommendation: "Use SHA-256 or better for integrity, and bcrypt/scrypt/argon2 for passwords.",
	},
	{
		ID: "AOSS-005", Category: "config", Severity: "high",
		Title:          "TLS certificate verification disabled",
		Pattern:        `InsecureSkipVerify[[:space:]]*:[[:space:]]*true|verify[[:space:]]*=[[:space:]]*False|rejectUnauthorized[[:space:]]*:[[:space:]]*false|NODE_TLS_REJECT_UNAUTHORIZED`,
		Recommendation: "Re-enable verification and trust the correct CA instead of disabling the check.",
	},
	{
		ID: "AOSS-006", Category: "secret", Severity: "critical",
		Title: "Credential-shaped literal assigned in source",
		// Deliberately anchored on an assignment to a credential-named
		// identifier with a long literal. The report cites the location only;
		// the value never leaves the container.
		Pattern: `(password|passwd|secret|api_?key|token|private_?key)[[:space:]]*[:=][[:space:]]*['\"][^'\"]{12,}['\"]`,
		Recommendation: "Rotate the credential first, then remove it from the tree AND from history — " +
			"deleting it from the working tree does not un-leak it.",
	},
	{
		ID: "AOSS-007", Category: "config", Severity: "medium",
		Title:          "Permissive CORS origin",
		Pattern:        `Access-Control-Allow-Origin[[:space:]]*[:=][[:space:]]*['\"]?\*|cors\(\{[[:space:]]*origin[[:space:]]*:[[:space:]]*['\"]?\*`,
		Recommendation: "Name the allowed origins explicitly; a wildcard with credentials is unsafe.",
	},
	{
		ID: "AOSS-008", Category: "disclosure", Severity: "medium",
		Title:          "Debug mode enabled in committed configuration",
		Pattern:        `DEBUG[[:space:]]*[:=][[:space:]]*(True|true|1)[[:space:]]*$|app\.debug[[:space:]]*=[[:space:]]*True`,
		Recommendation: "Drive debug from the environment and default it off, so a committed file cannot enable it.",
	},
}

// scannedExtensions is what the tool claims to understand. A file outside this
// set is reported as SKIPPED rather than silently ignored, because "we did not
// look" and "we looked and found nothing" are different results and only one of
// them is evidence.
var scannedExtensions = []string{
	"go", "py", "js", "jsx", "ts", "tsx", "rb", "php", "java", "cs", "rs",
	"sh", "yaml", "yml", "json", "toml", "ini", "env", "conf", "tf",
}

// skippedDirs are never worth scanning and would exhaust the file budget.
var skippedDirs = []string{".git", "node_modules", "vendor", "dist", "build", ".venv", "__pycache__", "target"}

// staticScanArgv builds the command. Only the two validated integers vary; the
// rules, the extensions and the structure are fixed here.
func staticScanArgv(p ToolParams) []string {
	var rules strings.Builder
	for _, rule := range staticScanRules {
		// Single-quoted, and no rule contains a single quote — asserted by a
		// test, so a future rule that does fails the build rather than
		// breaking out of the quoting.
		fmt.Fprintf(&rules, "%s\x1f%s\x1f%s\x1f%s\x1f%s\n",
			rule.ID, rule.Severity, rule.Category, rule.Title, rule.Pattern)
	}

	script := `
set -u
MAX_FILES=` + fmt.Sprint(p.MaxFiles) + `
MAX_BYTES=` + fmt.Sprint(p.MaxFileBytes) + `
EXTS="` + strings.Join(scannedExtensions, " ") + `"
SKIP_DIRS="` + strings.Join(skippedDirs, " ") + `"

# Boundary evidence first: a report from a run that cannot show it was
# confined is not evidence of anything.
echo "ao_uid=$(id -u)"
echo "ao_memory_max=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || echo unknown)"
echo "ao_pids_max=$(cat /sys/fs/cgroup/pids.max 2>/dev/null || echo unknown)"
echo "ao_cpu_max=$(cat /sys/fs/cgroup/cpu.max 2>/dev/null | tr ' ' '/' || echo unknown)"
if echo probe 2>/dev/null > /ao-write-probe; then
  echo "ao_rootfs_readonly=false"; rm -f /ao-write-probe 2>/dev/null
else
  echo "ao_rootfs_readonly=true"
fi
echo "ao_daemon_env_leaked=$(env | grep -Ec '^[^=]*(TOKEN|SECRET|PASSWORD|CREDENTIAL|API_KEY)[^=]*=' || true)"
# The NAMES of delivered secrets, never their contents. AO checks these against
# what it delivered; a file the container has that AO did not write is worse
# than a missing one and must not read as success.
` + SecretEvidenceScript + `
if wget -T 2 -q -O- http://1.1.1.1/ >/dev/null 2>&1; then
  echo "ao_network_reachable=true"
else
  echo "ao_network_reachable=false"
fi

prune=""
for d in $SKIP_DIRS; do prune="$prune -name $d -prune -o"; done

# One pass to enumerate, so the counts in the report describe the same set the
# rules ran over.
# shellcheck disable=SC2086
find /work $prune -type f -print 2>/dev/null | sort > /tmp/all_files
echo "ao_input_files=$(wc -l < /tmp/all_files | tr -d ' ')"

# The fingerprint of what the container ACTUALLY sees on the mount, in exactly
# the form AO computed for what it staged: sorted "relpath\0size\0sha256"
# lines, hashed once. A count can match by accident -- an empty mount and an
# empty project both report zero -- and a path list can match a tree carrying
# different bytes. This cannot.
: > /tmp/input_rows
while IFS= read -r f; do
  rel="${f#/work/}"
  sz=$(wc -c < "$f" 2>/dev/null || echo 0)
  hs=$(sha256sum "$f" 2>/dev/null | cut -d" " -f1)
  printf '%s\000%s\000%s\n' "$rel" "$sz" "$hs" >> /tmp/input_rows
done < /tmp/all_files
echo "ao_input_digest=$(sha256sum < /tmp/input_rows | cut -d' ' -f1)"

: > /tmp/scan_files
: > /tmp/skipped
scanned=0
while IFS= read -r f; do
  ext="${f##*.}"
  case " $EXTS " in
    *" $ext "*) ;;
    *) printf '%s\037%s\n' "$f" "unsupported_extension" >> /tmp/skipped; continue ;;
  esac
  size=$(wc -c < "$f" 2>/dev/null || echo 0)
  if [ "$size" -gt "$MAX_BYTES" ]; then
    printf '%s\037%s\n' "$f" "too_large" >> /tmp/skipped; continue
  fi
  if [ "$scanned" -ge "$MAX_FILES" ]; then
    printf '%s\037%s\n' "$f" "file_budget_exhausted" >> /tmp/skipped; continue
  fi
  echo "$f" >> /tmp/scan_files
  scanned=$((scanned+1))
done < /tmp/all_files

echo "ao_scanned_files=$scanned"
echo "ao_skipped_files=$(wc -l < /tmp/skipped | tr -d ' ')"

echo "AO_SECTION=skipped"
cat /tmp/skipped 2>/dev/null || true

echo "AO_SECTION=rules"
cat <<'AO_RULES_EOF'
` + rules.String() + `AO_RULES_EOF

echo "AO_SECTION=findings"
if [ "$scanned" -gt 0 ]; then
  cat <<'AO_PATTERNS_EOF' > /tmp/patterns
` + rules.String() + `AO_PATTERNS_EOF
  while IFS= read -r line; do
    rid=$(printf '%s' "$line" | cut -d"$(printf '\37')" -f1)
    pat=$(printf '%s' "$line" | cut -d"$(printf '\37')" -f5)
    [ -z "$pat" ] && continue
    # -H is load-bearing: with a SINGLE file argument grep omits the filename
    # prefix, so a one-file scope (the narrowest, most deliberate scan somebody
    # can ask for) would parse as pathless and yield zero findings -- a clean
    # report of a file that does match. -I skips binaries, -n gives the line,
    # and the MATCH ITSELF is never printed: a secret rule that echoed its
    # match would put the credential in the report it exists to keep out.
    grep -HInE -- "$pat" $(cat /tmp/scan_files | tr '\n' ' ') 2>/dev/null |
      cut -d: -f1,2 |
      while IFS= read -r hit; do
        printf '%s\037%s\n' "$rid" "$hit"
      done
  done < /tmp/patterns
fi
echo "AO_SECTION=end"
`
	return []string{"sh", "-c", script}
}
