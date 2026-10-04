package skillrunner

import (
	"regexp"
	"strings"
)

// DenyGlobs matches repo-relative paths against a manifest's
// scope.files.deny patterns.
//
// The semantics are the conservative reading of a deny list: "**" crosses
// directories, "*" and "?" do not, and a pattern with no slash matches the
// file's name at ANY depth -- ".env" denies config/.env too. Reading a deny
// list narrowly is how a credential file one directory down reaches a run.
//
// Both execution paths use it: the container runner (tool modes) and the host
// agent (ADR 0010). A denied file is never staged, so nothing inside either
// boundary can read it; it is recorded in coverage as skipped.
type DenyGlobs []*regexp.Regexp

// CompileDenyGlobs compiles a manifest's deny list.
func CompileDenyGlobs(globs []string) DenyGlobs {
	var out DenyGlobs
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if !strings.Contains(g, "/") {
			g = "**/" + g
		}
		out = append(out, regexp.MustCompile("^"+globToRegexp(g)+"$"))
	}
	return out
}

func globToRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '*' && i+1 < len(g) && g[i+1] == '*':
			i++
			if i+1 < len(g) && g[i+1] == '/' {
				i++
				b.WriteString(`(?:.*/)?`)
			} else {
				b.WriteString(`.*`)
			}
		case c == '*':
			b.WriteString(`[^/]*`)
		case c == '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// Match reports whether rel (slash-separated, repo-relative) is denied.
func (s DenyGlobs) Match(rel string) bool {
	for _, re := range s {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}

// SkipReasonDenied is a file the manifest's deny list keeps out of every run.
const SkipReasonDenied = "denied-by-manifest"
