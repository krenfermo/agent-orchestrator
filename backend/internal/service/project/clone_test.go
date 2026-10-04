package project_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// newManagerWithClone builds a Manager confined to root, with CloneFromGitHub
// backed by a fake `gh` runner instead of the real binary. The fake, when it
// reports success, actually creates a committed git repo at the requested
// destination so the subsequent auto-register (Add) has something real to
// validate — mirroring what a real `gh repo clone` would leave behind.
func newManagerWithClone(t *testing.T, root string, runner func(ctx context.Context, args ...string) ([]byte, error)) project.Manager {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return project.NewWithDeps(project.Deps{Store: store, AllowedRoots: []string{root}, CloneRunner: runner})
}

func successfulCloneRunner(t *testing.T) func(ctx context.Context, args ...string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, args ...string) ([]byte, error) {
		// args: "repo" "clone" "<slug>" "<destPath>"
		if len(args) != 4 || args[0] != "repo" || args[1] != "clone" {
			t.Fatalf("unexpected gh args: %v", args)
		}
		dest := args[3]
		gitRepoAt(t, dest)
		return []byte("Cloning into '" + dest + "'...\n"), nil
	}
}

func TestCloneFromGitHub_Success(t *testing.T) {
	root := t.TempDir()
	mgr := newManagerWithClone(t, root, successfulCloneRunner(t))

	p, err := mgr.CloneFromGitHub(context.Background(), project.CloneInput{Repo: "octocat/hello-world"})
	if err != nil {
		t.Fatalf("CloneFromGitHub: %v", err)
	}
	wantPath := filepath.Join(root, "hello-world")
	if p.Path != wantPath {
		t.Errorf("Path = %q, want %q", p.Path, wantPath)
	}

	// Registered: listing shows it.
	list, err := mgr.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %d entries, want 1", len(list))
	}
}

func TestCloneFromGitHub_AcceptsGitHubURL(t *testing.T) {
	root := t.TempDir()
	mgr := newManagerWithClone(t, root, successfulCloneRunner(t))

	p, err := mgr.CloneFromGitHub(context.Background(), project.CloneInput{Repo: "https://github.com/octocat/hello-world.git"})
	if err != nil {
		t.Fatalf("CloneFromGitHub: %v", err)
	}
	if filepath.Base(p.Path) != "hello-world" {
		t.Errorf("Path = %q, want basename hello-world", p.Path)
	}
}

func TestCloneFromGitHub_RejectsMalformedRepo(t *testing.T) {
	root := t.TempDir()
	mgr := newManagerWithClone(t, root, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("gh should not run for a malformed repo")
		return nil, nil
	})

	_, err := mgr.CloneFromGitHub(context.Background(), project.CloneInput{Repo: "not a repo; rm -rf /"})
	if err == nil {
		t.Fatal("want error for malformed repo, got nil")
	}
	wantCode(t, err, "INVALID_GITHUB_REPO")
}

func TestCloneFromGitHub_RejectsOverwrite(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "hello-world")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := newManagerWithClone(t, root, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("gh should not run when the destination already exists")
		return nil, nil
	})

	_, err := mgr.CloneFromGitHub(context.Background(), project.CloneInput{Repo: "octocat/hello-world"})
	if err == nil {
		t.Fatal("want error for existing destination, got nil")
	}
	wantCode(t, err, "DESTINATION_ALREADY_EXISTS")
}

func TestCloneFromGitHub_RequiresAllowedRoots(t *testing.T) {
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := project.NewWithDeps(project.Deps{Store: store})

	_, err = mgr.CloneFromGitHub(context.Background(), project.CloneInput{Repo: "octocat/hello-world"})
	if err == nil {
		t.Fatal("want error when no allowed roots configured, got nil")
	}
	wantCode(t, err, "NO_ALLOWED_ROOTS_CONFIGURED")
}

func TestCloneFromGitHub_SurfacesAuthFailure(t *testing.T) {
	root := t.TempDir()
	mgr := newManagerWithClone(t, root, func(context.Context, ...string) ([]byte, error) {
		return []byte("failed to clone: HTTP 401: authentication required"), errors.New("exit status 1")
	})

	_, err := mgr.CloneFromGitHub(context.Background(), project.CloneInput{Repo: "octocat/private-repo"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	wantCode(t, err, "GITHUB_NOT_AUTHENTICATED")
}

func TestCloneFromGitHub_SurfacesGenericFailure(t *testing.T) {
	root := t.TempDir()
	mgr := newManagerWithClone(t, root, func(context.Context, ...string) ([]byte, error) {
		return []byte("HTTP 404: Not Found"), errors.New("exit status 1")
	})

	_, err := mgr.CloneFromGitHub(context.Background(), project.CloneInput{Repo: "octocat/does-not-exist"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	wantCode(t, err, "GITHUB_CLONE_FAILED")
}

func TestCloneFromGitHub_SanitizesDestinationName(t *testing.T) {
	root := t.TempDir()
	mgr := newManagerWithClone(t, root, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("gh should not run for an unsafe destination name")
		return nil, nil
	})
	bad := "../escape"
	_, err := mgr.CloneFromGitHub(context.Background(), project.CloneInput{Repo: "octocat/hello-world", DestinationName: &bad})
	if err == nil {
		t.Fatal("want error for unsafe destination name, got nil")
	}
	wantCode(t, err, "INVALID_DESTINATION_NAME")
}
