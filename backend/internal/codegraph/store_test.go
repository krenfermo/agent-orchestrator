package codegraph

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataDirPrefersAODataDirOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AO_DATA_DIR", dir)

	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if got != dir {
		t.Fatalf("DataDir = %q, want %q", got, dir)
	}

	root, err := StoreRoot()
	if err != nil {
		t.Fatalf("StoreRoot: %v", err)
	}
	if want := filepath.Join(dir, "codegraph"); root != want {
		t.Fatalf("StoreRoot = %q, want %q", root, want)
	}
}

func TestStoreRootDefaultsUnderAOHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AO_DATA_DIR", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	root, err := StoreRoot()
	if err != nil {
		t.Fatalf("StoreRoot: %v", err)
	}
	want := filepath.Join(home, ".ao", "data", "codegraph")
	if root != want {
		t.Fatalf("StoreRoot = %q, want %q", root, want)
	}
	for _, forbidden := range []string{"Application Support", "AppData"} {
		if strings.Contains(root, forbidden) {
			t.Fatalf("StoreRoot %q resolved into an OS app-data location", root)
		}
	}
}

func TestValidateStoreDirRejectsUnusableLocations(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"relative":    filepath.Join("relative", "codegraph"),
		"macAppData":  filepath.Join(string(filepath.Separator), "Users", "someone", "Library", "Application Support", "ao"),
		"winAppData":  filepath.Join(string(filepath.Separator), "Users", "someone", "AppData", "Roaming", "ao"),
		"winAppLocal": filepath.Join(string(filepath.Separator), "Users", "someone", "AppData", "Local", "ao"),
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateStoreDir(dir); !errors.Is(err, ErrStorePath) {
				t.Fatalf("ValidateStoreDir(%q) = %v, want ErrStorePath", dir, err)
			}
			if _, err := NewStore(dir); !errors.Is(err, ErrStorePath) {
				t.Fatalf("NewStore(%q) = %v, want ErrStorePath", dir, err)
			}
		})
	}
}

func TestCanonicalRootRequiresAnAbsolutePath(t *testing.T) {
	for name, root := range map[string]string{
		"empty":   "",
		"blank":   "   ",
		"dot":     ".",
		"nested":  filepath.Join("some", "checkout"),
		"parent":  filepath.Join("..", "checkout"),
		"homeish": "~/projects/api",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalRoot(root); !errors.Is(err, ErrProjectRoot) {
				t.Fatalf("CanonicalRoot(%q) = %v, want ErrProjectRoot", root, err)
			}
		})
	}

	dir := t.TempDir()
	canonical, err := CanonicalRoot(dir)
	if err != nil {
		t.Fatalf("CanonicalRoot(%q): %v", dir, err)
	}
	if !filepath.IsAbs(canonical) {
		t.Fatalf("CanonicalRoot returned a relative path %q", canonical)
	}
	// A redundant spelling of the same absolute checkout must collapse onto
	// the same key, symlinks resolved.
	sep := string(filepath.Separator)
	noisy := dir + sep + "child" + sep + ".."
	if got, err := CanonicalRoot(noisy); err != nil || got != canonical {
		t.Fatalf("CanonicalRoot(%q) = %q, %v; want %q", noisy, got, err, canonical)
	}
}

func TestProjectKeySeparatesSameNamedProjects(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "orgA", "api")
	second := filepath.Join(base, "orgB", "api")

	if ProjectKey(first) == ProjectKey(second) {
		t.Fatalf("two checkouts named %q collapsed onto one key %q", "api", ProjectKey(first))
	}
	if !strings.HasPrefix(ProjectKey(first), "api-") {
		t.Fatalf("ProjectKey = %q, want a readable %q prefix", ProjectKey(first), "api-")
	}

	store, err := NewStore(filepath.Join(base, "store"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store.PathFor(first) == store.PathFor(second) {
		t.Fatalf("same-named projects share a graph file %q", store.PathFor(first))
	}
}

func TestStoreLoadRejectsAGraphFromAnotherProject(t *testing.T) {
	base := t.TempDir()
	store, err := NewStore(filepath.Join(base, "store"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	mine := filepath.Join(base, "mine")
	theirs := filepath.Join(base, "theirs")

	graph := NewGraph(theirs)
	graph.Put(FileEntry{Path: "secret.go", Hash: "deadbeef", Language: "go"})
	if _, err := store.Save(graph); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Plant the other project's graph at this project's location: a load must
	// refuse it rather than hand back entries that belong elsewhere.
	planted := store.PathFor(mine)
	if err := os.MkdirAll(filepath.Dir(planted), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw, err := os.ReadFile(store.PathFor(theirs))
	if err != nil {
		t.Fatalf("read planted graph: %v", err)
	}
	if err := os.WriteFile(planted, raw, 0o600); err != nil {
		t.Fatalf("write planted graph: %v", err)
	}

	loaded, found, err := store.Load(mine)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Fatal("Load reported a graph found for a project it does not belong to")
	}
	if len(loaded.Files) != 0 {
		t.Fatalf("Load leaked %d entries from another project", len(loaded.Files))
	}
}

func TestStoreSaveRoundTrips(t *testing.T) {
	base := t.TempDir()
	store, err := NewStore(filepath.Join(base, "store"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	root := filepath.Join(base, "project")

	graph := NewGraph(root)
	graph.Put(FileEntry{
		Path:     "a.go",
		Hash:     "hash-a",
		Language: "go",
		Symbols:  []Symbol{{ID: symbolID("a.go", SymbolFunction, "A"), Name: "A", Kind: SymbolFunction, File: "a.go", Line: 3}},
		Edges:    []Edge{{Kind: EdgeImport, From: "a.go", To: "fmt"}},
	})
	path, err := store.Save(graph)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.HasPrefix(path, store.Root()) {
		t.Fatalf("graph written to %q, outside store root %q", path, store.Root())
	}

	loaded, found, err := store.Load(root)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	entry, ok := loaded.Lookup("a.go")
	if !ok || entry.Hash != "hash-a" || len(entry.Symbols) != 1 || len(entry.Edges) != 1 {
		t.Fatalf("round-tripped entry = %+v", entry)
	}
}
