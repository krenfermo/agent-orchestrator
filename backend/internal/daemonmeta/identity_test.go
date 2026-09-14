package daemonmeta

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// P9 review: N daemons racing on a fresh data dir mint exactly ONE identity,
// every one of them reads that same identity, and no name ever points at a
// partial file.
func TestLoadOrCreateInstallationIDIsExclusiveUnderRace(t *testing.T) {
	dir := t.TempDir()
	const n = 32
	ids := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = LoadOrCreateInstallationID(dir)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] || !ValidInstallationID(ids[i]) {
			t.Fatalf("racer %d got %q, racer 0 got %q", i, ids[i], ids[0])
		}
	}
	info, err := os.Stat(filepath.Join(dir, InstallationIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("installation identity perms = %o, want 600", perm)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != InstallationIDFile {
			t.Fatalf("staging file left behind: %s", e.Name())
		}
	}
	again, err := LoadOrCreateInstallationID(dir)
	if err != nil || again != ids[0] {
		t.Fatalf("re-read = %q, %v; want the minted identity", again, err)
	}
}

// A malformed, empty or partial identity is an ERROR and is never replaced: a
// new identity would silently disown every runtime the old one stamped.
func TestMalformedInstallationIDFailsClosedAndIsUntouched(t *testing.T) {
	for name, content := range map[string]string{
		"empty":     "",
		"partial":   "aoi-1234",
		"garbage":   "not an id\n",
		"no prefix": "00000000-0000-4000-8000-000000000001\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, InstallationIDFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if id, err := LoadOrCreateInstallationID(dir); err == nil {
				t.Fatalf("malformed identity accepted as %q", id)
			}
			b, _ := os.ReadFile(path)
			if string(b) != content {
				t.Fatalf("malformed identity was rewritten to %q", b)
			}
		})
	}
}

func TestInstallationIDRequiresADataDir(t *testing.T) {
	if _, err := LoadOrCreateInstallationID("  "); err == nil {
		t.Fatal("an empty data dir minted an identity")
	}
}
