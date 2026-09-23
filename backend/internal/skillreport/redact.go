package skillreport

import (
	"crypto/md5"  //nolint:gosec // G501: used only to recognise a digest of a known secret, never as a hash of record.
	"crypto/sha1" //nolint:gosec // G505: same, recognition only.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// redact.go -- storage-layer redaction (roadmap Subfase 5: "redaction is a
// storage-layer control, not a prompt").
//
// The skill is TOLD never to put a secret in a report. This is what happens
// when it does anyway, or when content it read persuaded it to. Two nets:
//
//   - SHAPES: well-known credential formats (cloud keys, forge tokens, private
//     key blocks, JWTs, credentials in URLs, a credential-named assignment)
//     are replaced wherever they appear.
//   - LITERALS: before the agent runs AO harvests every credential-shaped value
//     from the staged files itself. Those exact values -- and any 8+ character
//     prefix of one, and its hex MD5/SHA-1/SHA-256 and base64 -- are replaced.
//     This catches the value no pattern would: a password that is just a word
//     and some digits, copied verbatim from the file that holds it.
//
// Harvested literals live only in memory, for the duration of one run.

// Marker replaces a redacted value.
const Marker = "[REDACTED]"

// minLiteral is the shortest harvested value AO will redact. Shorter values
// ("true", "admin") would rewrite ordinary prose all over a report.
const minLiteral = 6

// minPrefix is the shortest prefix of a known secret treated as the secret.
const minPrefix = 8

var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\b[rs]k_(?:live|test)_[0-9A-Za-z]{16,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
}

// credentialAssignment finds `name <op> value` where name is credential-shaped.
// Group 3 is the value; the name and operator are kept so the finding still
// says WHAT was assigned.
var credentialAssignment = regexp.MustCompile(
	`(?i)((?:password|passwd|pwd|secret(?:[-_]?key)?|api[-_]?(?:key|token)|access[-_]?key|` +
		`private[-_]?key|client[-_]?secret|auth[-_]?token|token)["']?\s*(?::=|=|:)\s*)` +
		`(["'])([^"'\r\n]{` + "6" + `,})["']`)

// credentialUnquoted is the env-file form: NAME=value with no quotes, where
// the name is an UPPER_SNAKE credential name.
var credentialUnquoted = regexp.MustCompile(
	`(?m)^\s*(?:export\s+)?[A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_KEY|APIKEY|ACCESS_KEY|PRIVATE_KEY)` +
		`[A-Z0-9_]*\s*=\s*([^\s"'#]{6,})`)

// urlCredential is the password half of scheme://user:password@host.
var urlCredential = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:)([^\s@/]+)(@)`)

// Redactor rewrites every string of a decoded report.
type Redactor struct {
	literals []string
}

// NewRedactor builds a redactor for one run. literals are the values AO
// harvested from the staged files (HarvestLiterals); duplicates and values
// shorter than the minimum are dropped.
func NewRedactor(literals []string) *Redactor {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if len(s) >= minLiteral && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, l := range literals {
		l = strings.TrimSpace(l)
		if len(l) < minLiteral {
			continue
		}
		add(l)
		// A digest or an encoding of the value discloses it just as well:
		// the digest is a lookup away, the encoding is the value.
		m := md5.Sum([]byte(l)) //nolint:gosec // recognition only.
		add(hex.EncodeToString(m[:]))
		s1 := sha1.Sum([]byte(l)) //nolint:gosec // recognition only.
		add(hex.EncodeToString(s1[:]))
		s2 := sha256.Sum256([]byte(l))
		add(hex.EncodeToString(s2[:]))
		add(strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(l)), "="))
	}
	// Longest first, so a value that contains another is replaced whole.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return &Redactor{literals: out}
}

// Redact rewrites every string value in v (a document from Decode) and returns
// the rewritten document and how many replacements were made. Object keys are
// not rewritten: the schema fixes them.
func (r *Redactor) Redact(v any) (any, int) {
	n := 0
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			s, c := r.String(t)
			n += c
			return s
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
			return t
		}
		return x
	}
	return walk(v), n
}

// String redacts one string.
func (r *Redactor) String(s string) (string, int) {
	n := 0
	for _, re := range secretShapes {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return Marker })
	}
	s = credentialAssignment.ReplaceAllStringFunc(s, func(m string) string {
		sub := credentialAssignment.FindStringSubmatch(m)
		if strings.Contains(sub[3], Marker) {
			return m
		}
		n++
		return sub[1] + sub[2] + Marker + sub[2]
	})
	s = urlCredential.ReplaceAllStringFunc(s, func(m string) string {
		sub := urlCredential.FindStringSubmatch(m)
		if sub[2] == Marker {
			return m
		}
		n++
		return sub[1] + Marker + sub[3]
	})
	for _, lit := range r.literals {
		var c int
		s, c = redactLiteral(s, lit)
		n += c
	}
	return s, n
}

// redactLiteral replaces lit and any run of at least minPrefix characters of
// it (or the whole of a shorter lit) that starts where lit starts.
func redactLiteral(s, lit string) (string, int) {
	head := lit
	if len(head) > minPrefix {
		head = lit[:minPrefix]
	}
	if !strings.Contains(s, head) {
		return s, 0
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], head)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		start := i + j
		end := start + len(head)
		for end < len(s) && end-start < len(lit) && s[end] == lit[end-start] {
			end++
		}
		b.WriteString(s[i:start])
		b.WriteString(Marker)
		n++
		i = end
	}
	return b.String(), n
}

// HarvestLiterals collects every credential-shaped VALUE in the files under
// dir: the values of credential-named assignments, unquoted credential env
// lines, URL passwords, and anything matching a known token shape. It reads
// only regular files, at most maxBytes each.
//
// The result is sensitive by construction. The caller keeps it in memory for
// one run and passes it to NewRedactor; it is never logged or stored.
func HarvestLiterals(dir string, maxBytes int64) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxBytes {
			return nil //nolint:nilerr // an unreadable or oversized file contributes no literals.
		}
		b, err := os.ReadFile(p) //nolint:gosec // path from the walked staging root.
		if err != nil {
			return nil //nolint:nilerr // same.
		}
		out = append(out, harvestText(string(b))...)
		return nil
	})
	return out, err
}

func harvestText(text string) []string {
	out := make([]string, 0, 8)
	for _, m := range credentialAssignment.FindAllStringSubmatch(text, -1) {
		out = append(out, m[3])
	}
	for _, m := range credentialUnquoted.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	for _, m := range urlCredential.FindAllStringSubmatch(text, -1) {
		out = append(out, m[2])
	}
	for _, re := range secretShapes {
		out = append(out, re.FindAllString(text, -1)...)
	}
	return out
}
