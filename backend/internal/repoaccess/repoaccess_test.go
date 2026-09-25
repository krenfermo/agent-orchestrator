package repoaccess_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/repoaccess"
)

const maxBytes = 1 << 20

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// canonicalTemp returns a symlink-resolved temp dir (macOS /var -> /private/var),
// so the root handed to repoaccess is what a real project path looks like.
func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// --- symlinks: every shape fails closed ------------------------------------

func TestReadConfinedRefusesEverySymlinkShape(t *testing.T) {
	outside := canonicalTemp(t)
	write(t, outside, "credentials", "SECRET-OUTSIDE-CANARY")
	write(t, outside, "dir/file.go", "package outside // OUTSIDE-DIR-CANARY")

	root := canonicalTemp(t)
	write(t, root, "real/inside.go", "package inside")

	links := map[string]string{
		"file_abs_out.go": filepath.Join(outside, "credentials"),                      // file -> outside, absolute
		"file_rel_out.go": filepath.Join("..", filepath.Base(outside), "credentials"), // relative, escaping
		"dir_out":         filepath.Join(outside, "dir"),                              // directory -> outside
		"file_rel_in.go":  "real/inside.go",                                           // relative, INSIDE the root
		"dir_in":          "real",                                                     // directory INSIDE the root
		"chain_a.go":      "chain_b.go",                                               // chain a -> b -> outside
		"chain_b.go":      filepath.Join(outside, "credentials"),
		"broken.go":       "does/not/exist.go", // dangling
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}

	cases := []string{
		"file_abs_out.go", "file_rel_out.go", "dir_out/file.go", "file_rel_in.go",
		"dir_in/inside.go", "chain_a.go", "chain_b.go", "broken.go",
	}
	for _, rel := range cases {
		t.Run(rel, func(t *testing.T) {
			data, err := repoaccess.ReadConfined(root, rel, maxBytes)
			if !errors.Is(err, repoaccess.ErrSymlink) {
				t.Fatalf("ReadConfined(%s) err = %v, want ErrSymlink", rel, err)
			}
			if data != nil {
				t.Fatalf("ReadConfined(%s) returned %d bytes on refusal", rel, len(data))
			}
			if !repoaccess.IsRefusal(err) {
				t.Fatalf("a symlink refusal must be a skip outcome, got %v", err)
			}
		})
	}

	// The real file is still readable: refusing links must not refuse files.
	if data, err := repoaccess.ReadConfined(root, "real/inside.go", maxBytes); err != nil || string(data) != "package inside" {
		t.Fatalf("regular file: data=%q err=%v", data, err)
	}
}

func TestReadConfinedRefusesTraversalAndAbsolutePaths(t *testing.T) {
	root := canonicalTemp(t)
	write(t, root, "a.go", "package a")
	for _, rel := range []string{"../etc/passwd", "a/../../x", "/etc/passwd", "..", ".", "", "a.go\x00b"} {
		if _, err := repoaccess.ReadConfined(root, rel, maxBytes); !errors.Is(err, repoaccess.ErrEscapesRoot) {
			t.Errorf("ReadConfined(%q) err = %v, want ErrEscapesRoot", rel, err)
		}
	}
	// a/../a.go is harmless and cleans to a.go.
	if _, err := repoaccess.ReadConfined(root, "x/../a.go", maxBytes); err != nil {
		t.Errorf("x/../a.go should clean to a.go: %v", err)
	}
}

func TestReadConfinedRefusesSecretsWithoutOpeningThem(t *testing.T) {
	root := canonicalTemp(t)
	// A secret path that is not even readable: if ReadConfined opened it, the
	// error would be a permission error rather than ErrSecretPath.
	write(t, root, ".env", "API_KEY=CANARY")
	if err := os.Chmod(filepath.Join(root, ".env"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, ".env"), 0o644) })
	if _, err := repoaccess.ReadConfined(root, ".env", maxBytes); !errors.Is(err, repoaccess.ErrSecretPath) {
		t.Fatalf("ReadConfined(.env) err = %v, want ErrSecretPath decided before any open", err)
	}
}

func TestReadConfinedSizeAndKind(t *testing.T) {
	root := canonicalTemp(t)
	write(t, root, "big.go", strings.Repeat("x", 100))
	if _, err := repoaccess.ReadConfined(root, "big.go", 10); !errors.Is(err, repoaccess.ErrTooLarge) {
		t.Fatalf("over cap: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := repoaccess.ReadConfined(root, "adir", maxBytes); !errors.Is(err, repoaccess.ErrNotRegular) {
		t.Fatalf("directory: %v", err)
	}
	if _, err := repoaccess.ReadConfined(root, "missing.go", maxBytes); !errors.Is(err, repoaccess.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := repoaccess.ReadConfined(root, "node_modules/x/index.js", maxBytes); !errors.Is(err, repoaccess.ErrExcluded) {
		t.Fatalf("excluded dir: %v", err)
	}
}

// --- secret boundary --------------------------------------------------------

func TestSecretPathPolicy(t *testing.T) {
	secret := []string{
		".env", "config/.env", ".env.local", ".env.production", ".env.example", "prod.env",
		"certs/server.pem", "tls/server.key", "keystore.jks", "store.p12", "vault.kdbx",
		"deploy/id_rsa", "id_ed25519", ".ssh/config", "home/.aws/credentials", ".aws/config",
		"credentials.json", "secrets.yaml", "config/secrets/prod.yaml", "secrets/anything.txt",
		"infra/terraform.tfstate", "infra/prod.tfvars", ".npmrc", ".netrc", ".git-credentials",
		".docker/config.json", "gcp/application_default_credentials.json", "k8s/kubeconfig",
		"service-account.json", ".pgpass",
	}
	for _, rel := range secret {
		if !repoaccess.IsSecretPath(rel) {
			t.Errorf("IsSecretPath(%q) = false, want true", rel)
		}
	}
	notSecret := []string{
		"internal/secrets.go", "src/credentials.ts", "auth/token_store.py", "README.md",
		"id_rsa.pub", "config/app.yaml", "docs/environment.md", "venv_setup.sh",
		"backend/internal/secretbox/box.go", "pkg/envconfig/config.go",
	}
	for _, rel := range notSecret {
		if repoaccess.IsSecretPath(rel) {
			t.Errorf("IsSecretPath(%q) = true, want false (code that handles secrets must stay indexable)", rel)
		}
	}
	if !repoaccess.IsSecretTemplate(".env.example") || repoaccess.IsSecretTemplate(".env.production") {
		t.Fatal("env template classification wrong")
	}
}

// --- eligibility -----------------------------------------------------------

// The MEDUSA pattern, reproduced: a project repo with a real linked git
// worktree checked out under .claude/worktrees/. Only the project's own tracked
// files are eligible.
func TestListEligibleExcludesLinkedWorktreesUntrackedAndSecrets(t *testing.T) {
	root := canonicalTemp(t)
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "src/app.ts", "export const app = 1")
	write(t, root, "src/auth.ts", "export function login() {}")
	write(t, root, ".gitignore", ".claude/\n.env\n")
	write(t, root, ".env.example", "API_KEY=")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "init")

	// A linked worktree inside the project, exactly as an agent creates one.
	git(t, root, "worktree", "add", "-q", ".claude/worktrees/foo", "-b", "agent/foo")
	write(t, root, ".claude/worktrees/foo/src/extra.ts", "export const leaked = 1")
	// Untracked scratch and a secret that is not even ignored.
	write(t, root, "scratch.ts", "export const untracked = 1")
	write(t, root, ".env", "API_KEY=CANARY")

	got, err := repoaccess.ListEligible(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != repoaccess.ModeGitTracked {
		t.Fatalf("mode = %s, want git-tracked", got.Mode)
	}
	want := []string{".gitignore", "src/app.ts", "src/auth.ts"}
	if !slices.Equal(got.Paths, want) {
		t.Fatalf("eligible = %v, want %v", got.Paths, want)
	}
	for _, p := range got.Paths {
		if strings.Contains(p, ".claude") || strings.Contains(p, "scratch") || strings.Contains(p, ".env") {
			t.Fatalf("ineligible path listed: %s", p)
		}
	}
	if !slices.Contains(got.SecretTemplates, ".env.example") {
		t.Fatalf("tracked env template should be reported as existing (not read): %v", got.SecretTemplates)
	}
}

// Running from inside the auxiliary worktree lists THAT checkout's files, and
// running from the project lists the project's -- the two are never confused.
func TestListEligibleDistinguishesCurrentFromAuxiliaryWorktree(t *testing.T) {
	root := canonicalTemp(t)
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "main.go", "package main")
	write(t, root, ".gitignore", ".claude/\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "init")
	git(t, root, "worktree", "add", "-q", ".claude/worktrees/aux", "-b", "aux")
	aux := filepath.Join(root, ".claude", "worktrees", "aux")
	write(t, aux, "only_in_aux.go", "package main")
	git(t, aux, "add", ".")
	git(t, aux, "commit", "-q", "-m", "aux")

	project, err := repoaccess.ListEligible(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(project.Paths, "only_in_aux.go") {
		t.Fatal("the project's listing contains a file that exists only in an auxiliary worktree")
	}
	auxList, err := repoaccess.ListEligible(context.Background(), aux)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(auxList.Paths, "only_in_aux.go") || !slices.Contains(auxList.Paths, "main.go") {
		t.Fatalf("aux worktree listing = %v, want its own tracked files", auxList.Paths)
	}
}

func TestListEligibleFilesystemFallbackRefusesNestedCheckouts(t *testing.T) {
	root := canonicalTemp(t)
	write(t, root, "src/app.py", "print(1)")
	write(t, root, "node_modules/dep/index.js", "x")
	write(t, root, ".claude/worktrees/w/src/app.py", "print(2)")
	write(t, root, "nested/.git", "gitdir: /somewhere/else") // linked-worktree style .git FILE
	write(t, root, "nested/code.py", "print(3)")
	write(t, root, "config/.env", "K=CANARY")
	if err := os.Symlink("/etc", filepath.Join(root, "etc_link")); err != nil {
		t.Fatal(err)
	}

	got, err := repoaccess.ListEligible(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != repoaccess.ModeFilesystem {
		t.Fatalf("mode = %s, want filesystem", got.Mode)
	}
	if !slices.Equal(got.Paths, []string{"src/app.py"}) {
		t.Fatalf("eligible = %v, want only src/app.py", got.Paths)
	}
	for _, reason := range []string{"nested-checkout", "excluded-dir", "secret", "symlink"} {
		if got.Skipped[reason] == 0 {
			t.Errorf("skip reason %q not counted: %v", reason, got.Skipped)
		}
	}
}

// A tracked symlink is listed by git but refused by the read.
func TestTrackedSymlinkIsListedButNeverRead(t *testing.T) {
	outside := canonicalTemp(t)
	write(t, outside, "secret.txt", "OUTSIDE-CANARY")
	root := canonicalTemp(t)
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "a.go", "package a")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "init")

	got, err := repoaccess.ListEligible(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Paths, "CLAUDE.md") {
		t.Fatalf("git lists tracked symlinks; listing = %v", got.Paths)
	}
	if _, err := repoaccess.ReadConfined(root, "CLAUDE.md", maxBytes); !errors.Is(err, repoaccess.ErrSymlink) {
		t.Fatalf("tracked symlink read err = %v, want ErrSymlink", err)
	}
}
