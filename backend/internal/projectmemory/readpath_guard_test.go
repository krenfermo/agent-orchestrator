package projectmemory_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readpath_guard_test.go — Frente 3 / 3B: repository CONTENT is read only
// through repoaccess (eligibility, secret boundary, confined no-symlink read).
// A direct os.ReadFile / os.Open / WalkDir in the indexers would reopen the
// holes 3B closed, so every such call must be in a file on this allowlist, each
// of which reads AO's OWN state, never a project checkout.
var directReadAllowlist = map[string]string{
	"../codegraph/store.go": "JSON graph store under AO's data dir",
	"./store.go":            "legacy JSON memory store under AO's data dir",
	"./baseline.go":         "baseline evidence files under AO's data dir",
	"./stale.go":            "legacy file HASH only (Lstat-checked, no content persisted)",
}

var directRead = regexp.MustCompile(`\bos\.(ReadFile|Open|OpenFile)\(|filepath\.WalkDir\(`)

func TestRepositoryContentIsReadOnlyThroughRepoaccess(t *testing.T) {
	for _, dir := range []string{".", "../codegraph"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel := dir + "/" + name
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(src), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") || !directRead.MatchString(line) {
					continue
				}
				if _, ok := directReadAllowlist[rel]; !ok {
					t.Errorf("%s:%d reads the filesystem directly (%s); read repository files through repoaccess", rel, i+1, trimmed)
				}
			}
		}
	}
}
