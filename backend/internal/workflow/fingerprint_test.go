package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Regression: the most common real fix cycle is Codex editing the SAME
// untracked file it just created (status stays "??" both times). Path+status
// alone would report no change even though the content genuinely changed —
// exactly the case that would make Checkpoint 8D's fix-completion detection
// blind to its single most common scenario. WorkspaceFingerprint must hash
// each changed path's current content, not just its git status code.
func TestWorkspaceFingerprintDetectsSamePathStatusContentChange(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "output.txt")

	if err := os.WriteFile(file, []byte("first version"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	obsBefore := ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Untracked: true,
		Changes: []ports.WorkspaceChange{{Path: "output.txt", Status: "??"}},
	}
	fpBefore := WorkspaceFingerprint(obsBefore)

	if err := os.WriteFile(file, []byte("second version, actually different"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	obsAfter := ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Untracked: true,
		Changes: []ports.WorkspaceChange{{Path: "output.txt", Status: "??"}},
	}
	fpAfter := WorkspaceFingerprint(obsAfter)

	if fpBefore == fpAfter {
		t.Fatalf("fingerprint unchanged despite different file content at the same path+status: %s", fpBefore)
	}
}

func TestWorkspaceFingerprintStableForIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(file, []byte("stable content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	obs := ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Untracked: true,
		Changes: []ports.WorkspaceChange{{Path: "output.txt", Status: "??"}},
	}
	fp1 := WorkspaceFingerprint(obs)
	fp2 := WorkspaceFingerprint(obs)
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable across two observations of identical state: %s vs %s", fp1, fp2)
	}
}

func TestIsEphemeralArtifactPath(t *testing.T) {
	cases := map[string]bool{
		"__pycache__/module.cpython-312.pyc": true,
		"pkg/__pycache__/foo.pyc":            true,
		"foo.pyc":                            true,
		"foo.pyo":                            true,
		".pytest_cache/v/cache/lastfailed":   true,
		".coverage":                          true,
		".coverage.12345.Xabc":               true,
		"htmlcov/index.html":                 true,
		".mypy_cache/3.12/module.data.json":  true,
		".ruff_cache/0.1.0/cache.bin":        true,
		".pyright/module.json":               true,
		"src/main.py":                        false,
		"mypycache_report.py":                false,
		"README.md":                          false,
		"pkg/pycache_helper.go":              false,
	}
	for path, want := range cases {
		if got := IsEphemeralArtifactPath(path); got != want {
			t.Errorf("IsEphemeralArtifactPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// Regression: a Python fixture whose reviewer/verify commands import code and
// therefore generate __pycache__/.pytest_cache must NOT change the workspace
// fingerprint — those artifacts carry no signal about the task's real work
// (Checkpoint 8M.1, E2E C).
func TestWorkspaceFingerprintStableForCacheOnlyChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "__pycache__"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "__pycache__", "mod.cpython-312.pyc"), []byte("bytecode-v1"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	obsBefore := ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Untracked: true,
		Changes: []ports.WorkspaceChange{{Path: "__pycache__/mod.cpython-312.pyc", Status: "??"}},
	}
	fpBefore := WorkspaceFingerprint(obsBefore)

	if err := os.WriteFile(filepath.Join(dir, "__pycache__", "mod.cpython-312.pyc"), []byte("bytecode-v2-different"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".coverage"), []byte("coverage-data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	obsAfter := ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Untracked: true,
		Changes: []ports.WorkspaceChange{
			{Path: "__pycache__/mod.cpython-312.pyc", Status: "??"},
			{Path: ".coverage", Status: "??"},
		},
	}
	fpAfter := WorkspaceFingerprint(obsAfter)

	if fpBefore != fpAfter {
		t.Fatalf("fingerprint changed for cache-only artifacts: before=%s after=%s", fpBefore, fpAfter)
	}
}

// Regression: hygiene must not weaken the guard — a real source file change
// alongside cache noise still changes the fingerprint (Checkpoint 8M.1, E2E D).
func TestWorkspaceFingerprintDetectsRealChangeAmongCacheNoise(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("v1"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	obsBefore := ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Dirty: true,
		Changes: []ports.WorkspaceChange{
			{Path: "main.py", Status: "M"},
			{Path: "__pycache__/mod.pyc", Status: "??"},
		},
	}
	fpBefore := WorkspaceFingerprint(obsBefore)

	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("v2-real-change"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	obsAfter := ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Dirty: true,
		Changes: []ports.WorkspaceChange{
			{Path: "main.py", Status: "M"},
			{Path: "__pycache__/mod.pyc", Status: "??"},
		},
	}
	fpAfter := WorkspaceFingerprint(obsAfter)

	if fpBefore == fpAfter {
		t.Fatal("expected fingerprint to change when a real tracked source file changes")
	}
}

func TestWorkspaceFingerprintDetectsDeletion(t *testing.T) {
	dir := t.TempDir()
	obsExisting := ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "gone.txt", Status: "D"}},
	}
	fp := WorkspaceFingerprint(obsExisting)
	if fp == "" {
		t.Fatal("expected a non-empty fingerprint for a deleted-file observation")
	}
}
