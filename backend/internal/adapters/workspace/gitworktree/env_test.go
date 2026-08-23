package gitworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The pin must WIN over whatever the operator's environment already says, so
// the assertion is about the last occurrence, not merely the presence of one.
func TestGitEnvPinsMessageLocaleOverInheritedValues(t *testing.T) {
	t.Setenv("LC_ALL", "es_ES.UTF-8")
	t.Setenv("LANG", "es_ES.UTF-8")
	t.Setenv("LANGUAGE", "es")

	env := gitEnv("GIT_INDEX_FILE=/tmp/idx")
	last := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		last[key] = value
	}
	if last["LC_ALL"] != "C" {
		t.Fatalf("LC_ALL = %q, want C", last["LC_ALL"])
	}
	if last["LANGUAGE"] != "" {
		t.Fatalf("LANGUAGE = %q, want cleared", last["LANGUAGE"])
	}
	if last["GIT_INDEX_FILE"] != "/tmp/idx" {
		t.Fatalf("GIT_INDEX_FILE = %q, want the caller's value preserved", last["GIT_INDEX_FILE"])
	}
}

// Regression for the localized-git failure: every recovery in this adapter is
// selected by matching git's own prose ("is a missing but already registered
// worktree"), so on a machine whose git speaks anything but English the
// recovery simply did not run -- addNewBranchWorktree failed outright with
// "fatal: '<path>' es un árbol de trabajo faltante pero ya registrado".
//
// This drives the same stale-registration path as
// TestWorkspaceIntegrationAddNewBranchRecoversStaleRegistration with a
// non-English locale in the environment. On a machine with the Spanish
// translations installed it fails without the LC_ALL pin; on one without them
// git falls back to English and it passes either way, which is why the pin
// itself is asserted separately above.
func TestWorkspaceIntegrationRecoversStaleRegistrationUnderNonEnglishLocale(t *testing.T) {
	git := requireGit(t)
	t.Setenv("LC_ALL", "es_ES.UTF-8")
	t.Setenv("LANG", "es_ES.UTF-8")
	t.Setenv("LANGUAGE", "es")

	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)
	root := filepath.Join(tmp, "managed")
	ws, err := New(Options{Binary: git, ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	info, err := ws.Create(ctx, ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess", Branch: "child-old"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.RemoveAll(info.Path); err != nil {
		t.Fatalf("remove dir: %v", err)
	}

	if err := ws.addNewBranchWorktree(ctx, repo, "child-new", info.Path, "origin/main", false); err != nil {
		t.Fatalf("addNewBranchWorktree under a non-English locale: %v", err)
	}
	out, err := exec.Command(git, "-C", info.Path, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse recovered worktree: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "child-new" {
		t.Fatalf("recovered worktree branch = %q, want child-new", got)
	}
}
