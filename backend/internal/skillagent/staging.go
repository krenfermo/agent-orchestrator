package skillagent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
)

// globSet matches repo-relative paths against a manifest's deny globs.
//
// The semantics are the conservative reading of a deny list: "**" crosses
// directories, "*" and "?" do not, and a pattern with no slash matches the
// file's name at ANY depth -- ".env" denies config/.env too. Reading a deny
// list narrowly is how a credential file one directory down reaches an agent.
type globSet []*regexp.Regexp

func compileGlobs(globs []string) globSet {
	var out globSet
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

func (s globSet) match(rel string) bool {
	for _, re := range s {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}

// makeReadOnly removes every write bit from the staged copy: files 0400,
// directories 0500. The agent has no tool that writes, and this makes the
// copy read-only to the OS as well, so an attempt that somehow found a way to
// write would fail rather than succeed and be caught afterwards.
func makeReadOnly(dir string) error {
	var files, dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		} else {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Staging never writes a symlink, and a chmod through one would change
	// its target. Each path is re-checked right before it is changed.
	for _, p := range files {
		if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("staged entry %q is not a regular file", p)
		}
		if err := os.Chmod(p, 0o400); err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if info, err := os.Lstat(dirs[i]); err != nil || !info.IsDir() {
			return fmt.Errorf("staged entry %q is not a directory", dirs[i])
		}
		// 0500: readable and traversable, never writable -- stricter than
		// the 0700 it was created with, and a directory without x cannot be
		// read through at all.
		if err := os.Chmod(dirs[i], 0o500); err != nil { //nolint:gosec // G302: see above; tighter than 0700.
			return err
		}
	}
	return nil
}

// removeStaging restores write permission on the per-run directory and removes
// it. It refuses a directory that is not under an AO staging root.
func removeStaging(s skillrunner.Staging) error {
	if s.Dir == "" {
		return nil
	}
	if !strings.Contains(s.Dir, ".ao-skill-staging") {
		return fmt.Errorf("skillagent: refusing to remove %q: not an AO staging directory", s.Dir)
	}
	_ = filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: an AO-owned staging dir being made removable.
		}
		return nil
	})
	return s.Cleanup()
}

// verifyStaging proves the staged copy is exactly what AO wrote: the same set
// of regular files, each with the same bytes, and nothing else.
func verifyStaging(s skillrunner.Staging) error {
	want := make(map[string]string, len(s.Inputs))
	for _, in := range s.Inputs {
		want[in.RelPath] = in.SHA256
	}
	var problems []string
	seen := 0
	err := filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.Dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			problems = append(problems, rel+" is not a regular file")
			return nil
		}
		sum, ok := want[rel]
		if !ok {
			problems = append(problems, rel+" was created")
			return nil
		}
		got, err := hashFile(p)
		if err != nil {
			return err
		}
		if got != sum {
			problems = append(problems, rel+" was modified")
		}
		seen++
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStagingTampered, err)
	}
	if seen != len(want) {
		problems = append(problems, fmt.Sprintf("%d of %d staged files remain", seen, len(want)))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%w: %s", ErrStagingTampered, strings.Join(firstN(problems, 10), "; "))
	}
	return nil
}

// verifySource proves the project files the copy came from still hold the
// bytes that were staged. The agent never had the checkout, so a difference is
// most likely a person or a worker editing it -- but AO cannot tell which, and
// a report about code that is no longer there is not one to store.
func verifySource(projectPath string, s skillrunner.Staging) error {
	var problems []string
	for _, in := range s.Inputs {
		got, err := hashFile(filepath.Join(projectPath, filepath.FromSlash(in.RelPath)))
		if err != nil {
			problems = append(problems, in.RelPath+" is gone or unreadable")
			continue
		}
		if got != in.SHA256 {
			problems = append(problems, in.RelPath+" changed")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%w: %s", ErrSourceChanged, strings.Join(firstN(problems, 10), "; "))
	}
	return nil
}

func hashFile(p string) (string, error) {
	b, err := os.ReadFile(p) //nolint:gosec // paths derived from AO's own staging record.
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], fmt.Sprintf("and %d more", len(s)-n))
}
