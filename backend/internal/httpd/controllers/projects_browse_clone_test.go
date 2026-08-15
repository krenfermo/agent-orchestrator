package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func newTestServerWithRoots(t *testing.T, roots []string, cloneRunner func(ctx context.Context, args ...string) ([]byte, error)) *httptest.Server {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := projectsvc.NewWithDeps(projectsvc.Deps{Store: store, AllowedRoots: roots, CloneRunner: cloneRunner})
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		Projects: mgr,
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

func gitRepoFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "-c", "user.email=ao@example.com", "-c", "user.name=AO Test", "commit", "--allow-empty", "-m", "initial").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func TestProjectsAPI_Browse(t *testing.T) {
	root := t.TempDir()
	gitRepoFixture(t, filepath.Join(root, "repo-a"))
	srv := newTestServerWithRoots(t, []string{root}, nil)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/projects/browse", "")
	if status != http.StatusOK {
		t.Fatalf("GET browse = %d, want 200; body=%s", status, body)
	}
	assertJSON(t, headers)
	var got projectsvc.BrowseResult
	mustJSON(t, body, &got)
	if len(got.Entries) != 1 || got.Entries[0].Name != "repo-a" || !got.Entries[0].IsGitRepo {
		t.Fatalf("Entries = %+v", got.Entries)
	}
}

func TestProjectsAPI_Browse_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithRoots(t, []string{root}, nil)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/projects/browse?path=..%2F..%2Fetc", "")
	if status == http.StatusOK {
		t.Fatalf("GET browse traversal = 200, want error; body=%s", body)
	}
}

func TestProjectsAPI_Clone_Success(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithRoots(t, []string{root}, func(_ context.Context, args ...string) ([]byte, error) {
		gitRepoFixture(t, args[3])
		return []byte("Cloning...\n"), nil
	})

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/projects/clone", `{"repo":"octocat/hello-world"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST clone = %d, want 201; body=%s", status, body)
	}
	assertJSON(t, headers)
	var got struct {
		Project projectsvc.Project `json:"project"`
	}
	mustJSON(t, body, &got)
	if filepath.Base(got.Project.Path) != "hello-world" {
		t.Errorf("Path = %q", got.Project.Path)
	}
}

func TestProjectsAPI_Clone_RejectsOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "hello-world"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newTestServerWithRoots(t, []string{root}, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("gh should not run when destination exists")
		return nil, nil
	})

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/clone", `{"repo":"octocat/hello-world"}`)
	assertErrorCode(t, body, status, http.StatusConflict, "DESTINATION_ALREADY_EXISTS")
}

func TestProjectsAPI_Clone_NoRootsConfigured(t *testing.T) {
	srv := newTestServerWithRoots(t, nil, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("gh should not run without allowed roots")
		return nil, nil
	})

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/clone", `{"repo":"octocat/hello-world"}`)
	assertErrorCode(t, body, status, http.StatusBadRequest, "NO_ALLOWED_ROOTS_CONFIGURED")
}
