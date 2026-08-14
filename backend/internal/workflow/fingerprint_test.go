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
