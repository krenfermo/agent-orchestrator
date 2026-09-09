package proxybin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stagingFixture is a store holding one recognisable arm64 artifact.
func stagingFixture(t *testing.T) (Store, []byte) {
	t.Helper()
	body := []byte("\x7fELF not really, but exactly these bytes")
	return fixture(t, map[string][]byte{"linux-arm64": body}), body
}

// root returns a usable staging root under the test's own temp directory,
// through the real StagingRootFor so the marker and the refusals are exercised.
func root(t *testing.T) string {
	t.Helper()
	r, err := StagingRootFor("", t.TempDir())
	if err != nil {
		t.Fatalf("StagingRootFor: %v", err)
	}
	return r
}

// The happy path, asserted in the terms the container cares about: two files,
// nothing else, no write bit anywhere, and the bytes on disk are the bytes the
// digest names.
func TestStage_WritesExactlyTheProxyAndItsPolicy(t *testing.T) {
	store, body := stagingFixture(t)
	staged, err := Stage(StageRequest{
		Store: store, Root: root(t), RunID: "run-1", RuntimeArch: "aarch64",
		Policy: []byte(`{"allow":[]}`),
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	t.Cleanup(func() { _ = staged.Cleanup() })

	entries, err := os.ReadDir(staged.Dir)
	if err != nil {
		t.Fatalf("read staged dir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the mount holds %v; it must hold exactly the binary and the policy", names)
	}

	onDisk, err := os.ReadFile(staged.BinaryPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if string(onDisk) != string(body) {
		t.Fatal("what was staged is not what was selected")
	}
	if staged.Artifact.SHA256 != Digest(body) {
		t.Fatalf("Staged records %s, bytes hash to %s", staged.Artifact.SHA256, Digest(body))
	}

	// The container runs as uid 65534 and owns nothing here, so it needs
	// other-execute on the binary and other-read on the policy. What must NOT
	// be there is a write bit — for anybody, including the owner.
	binMode := statMode(t, staged.BinaryPath)
	if binMode != 0o555 {
		t.Fatalf("binary mode = %04o, want 0555", binMode)
	}
	polMode := statMode(t, staged.PolicyPath)
	if polMode != 0o444 {
		t.Fatalf("policy mode = %04o, want 0444", polMode)
	}
	dirMode := statMode(t, staged.Dir)
	if dirMode != 0o711 {
		t.Fatalf("dir mode = %04o, want 0711 (traversable, not listable)", dirMode)
	}
	// The paths AO hands the runtime are fixed, so the entrypoint does not vary
	// with the host architecture.
	if staged.ContainerBinary() != "/aoproxy/ao-egress-proxy" ||
		staged.ContainerPolicy() != "/aoproxy/policy.json" {
		t.Fatalf("container paths = %q, %q", staged.ContainerBinary(), staged.ContainerPolicy())
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// Requirement 3, negatively: neither the home directory nor AO's data directory
// may become a mount source. Mounting AO's data dir to deliver one binary would
// put the database and every project's state inside a container.
func TestStagingRootFor_RefusesTheHomeDirectoryAndAOsData(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory on this host")
	}
	if _, err := StagingRootFor("", home); !errors.Is(err, ErrStagingUnusable) {
		t.Fatalf("staging in the home directory was allowed: %v", err)
	}
	// A project directly in the home directory would put the root there too.
	if _, err := StagingRootFor(filepath.Join(home, "someproject"), ""); !errors.Is(err, ErrStagingUnusable) {
		t.Fatalf("a project in the home directory staged into it: %v", err)
	}
	for _, under := range []string{
		filepath.Join(home, ".ao"),
		filepath.Join(home, ".ao", "data"),
		filepath.Join(home, ".ao", "data", "skills", "deep"),
	} {
		_, err := StagingRootFor("", under)
		if !errors.Is(err, ErrStagingUnusable) {
			t.Fatalf("staging under %q was allowed: %v", under, err)
		}
		if !strings.Contains(err.Error(), "state directory") {
			t.Fatalf("the refusal should say why: %v", err)
		}
	}
	// A sibling whose name merely starts the same way is NOT AO's data dir.
	// A prefix comparison that got this wrong would refuse real projects.
	sibling := filepath.Join(home, ".aoother")
	if _, err := StagingRootFor("", sibling); err != nil {
		t.Fatalf("%q is not under ~/.ao and was refused: %v", sibling, err)
	}
}

// The same refusal for an explicitly configured data dir, which is the shape a
// second AO profile or a sandboxed test run has.
func TestStagingRootFor_RefusesAnExplicitDataDir(t *testing.T) {
	data := t.TempDir()
	t.Setenv("AO_DATA_DIR", data)
	if _, err := StagingRootFor("", filepath.Join(data, "skills")); !errors.Is(err, ErrStagingUnusable) {
		t.Fatal("staging under AO_DATA_DIR was allowed")
	}
	t.Setenv("AO_DATA_DIR", "")
	t.Setenv("AO_RUN_FILE", filepath.Join(data, "running.json"))
	if _, err := StagingRootFor("", data); !errors.Is(err, ErrStagingUnusable) {
		t.Fatal("staging beside AO_RUN_FILE was allowed")
	}
}

// Stage re-checks the root itself, so a caller that built one by hand rather
// than through StagingRootFor cannot escape the refusals above.
func TestStage_RefusesARootItDidNotChoose(t *testing.T) {
	store, _ := stagingFixture(t)
	policy := []byte(`{"allow":[]}`)
	cases := []struct {
		name    string
		root    string
		wantSub string
	}{
		{"relative", "relative/path", "must be absolute"},
		{"absolute but unmarked", t.TempDir(), "not an AO proxy staging root"},
		{"empty", "", "must be absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Stage(StageRequest{Store: store, Root: tc.root, RunID: "r", RuntimeArch: "aarch64", Policy: policy})
			if !errors.Is(err, ErrStagingUnusable) || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want ErrStagingUnusable mentioning %q", err, tc.wantSub)
			}
		})
	}
}

// Requirement: an insecure extraction is refused. A run id becomes a path
// element, and a path element that can hold a separator or a dot segment is a
// directory traversal. AO refuses rather than sanitizes: a sanitized name is
// one whose meaning changed without anybody being told.
func TestStage_RefusesARunIDThatCouldEscapeTheRoot(t *testing.T) {
	store, _ := stagingFixture(t)
	base := root(t)
	policy := []byte(`{"allow":[]}`)
	for _, id := range []string{
		"..", ".", "../escape", "a/b", "a\\b", "/absolute", "run id", "run;rm -rf /",
		"run$(whoami)", "run\x00null", "", strings.Repeat("x", 65),
	} {
		t.Run(strings.ReplaceAll(id, "/", "_"), func(t *testing.T) {
			_, err := Stage(StageRequest{Store: store, Root: base, RunID: id, RuntimeArch: "aarch64", Policy: policy})
			if !errors.Is(err, ErrStagingUnusable) {
				t.Fatalf("run id %q was accepted: %v", id, err)
			}
		})
	}
	// Nothing was created outside the root by any of them.
	if entries, err := os.ReadDir(base); err == nil && len(entries) != 0 {
		t.Fatalf("a refused run id left %d entries behind", len(entries))
	}
}

// A leftover directory is somebody's evidence. Writing over it destroys that
// and makes "whose bytes are these" unanswerable, so a second attempt with the
// same id refuses rather than reuses.
func TestStage_RefusesToReuseALeftoverDirectory(t *testing.T) {
	store, _ := stagingFixture(t)
	base := root(t)
	policy := []byte(`{"allow":[]}`)
	first, err := Stage(StageRequest{Store: store, Root: base, RunID: "run-1", RuntimeArch: "aarch64", Policy: policy})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	t.Cleanup(func() { _ = first.Cleanup() })

	_, err = Stage(StageRequest{Store: store, Root: base, RunID: "run-1", RuntimeArch: "aarch64", Policy: policy})
	if !errors.Is(err, ErrStagingUnusable) || !strings.Contains(err.Error(), "never reused") {
		t.Fatalf("err = %v, want a refusal to reuse", err)
	}
	// The first attempt is intact: refusing must not damage what it protected.
	if _, err := os.Stat(first.BinaryPath); err != nil {
		t.Fatalf("the refused second attempt disturbed the first: %v", err)
	}
}

// Every failure after the directory exists must leave nothing behind. A
// half-staged mount starts a proxy with no policy, or no proxy at all.
func TestStage_LeavesNothingBehindWhenItRefuses(t *testing.T) {
	base := root(t)
	good := []byte("the proxy, exactly as built")
	honest := fixture(t, map[string][]byte{"linux-arm64": good})
	// Bytes that do not match the recorded digest: the corruption case, seen
	// from staging rather than from selection.
	corrupt := newStore(honest.Provenance(), func(string) ([]byte, bool) {
		return []byte("the proxy, exactly as bui1t"), true
	})
	_, err := Stage(StageRequest{Store: corrupt, Root: base, RunID: "run-1", RuntimeArch: "aarch64",
		Policy: []byte(`{"allow":[]}`)})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "run-1")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused stage left %q behind", filepath.Join(base, "run-1"))
	}

	// A wrong architecture must not create the root's contents at all: the
	// selection happens before anything is written.
	_, err = Stage(StageRequest{Store: honest, Root: base, RunID: "run-2", RuntimeArch: "x86_64",
		Policy: []byte(`{"allow":[]}`)})
	if !errors.Is(err, ErrArchUnsupported) {
		t.Fatalf("err = %v, want ErrArchUnsupported", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "run-2")); !os.IsNotExist(statErr) {
		t.Fatal("a wrong-architecture stage created a directory")
	}

	// An empty policy is refused before anything is created, too: a proxy with
	// no policy allows nothing and finding that out from an exit code is late.
	_, err = Stage(StageRequest{Store: honest, Root: base, RunID: "run-3", RuntimeArch: "aarch64"})
	if !errors.Is(err, ErrStagingUnusable) || !strings.Contains(err.Error(), "no policy") {
		t.Fatalf("err = %v, want a refusal for an empty policy", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "run-3")); !os.IsNotExist(statErr) {
		t.Fatal("an empty-policy stage created a directory")
	}
}

// Cleanup must remove AO's own directory and refuse anything else. A bug in
// root selection must not be able to become a delete of somebody's files.
func TestCleanup_RemovesAOsDirectoryAndRefusesAnyOther(t *testing.T) {
	store, _ := stagingFixture(t)
	staged, err := Stage(StageRequest{Store: store, Root: root(t), RunID: "run-1", RuntimeArch: "aarch64",
		Policy: []byte(`{"allow":[]}`)})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := staged.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(staged.Dir); !os.IsNotExist(err) {
		t.Fatal("Cleanup left the run directory behind")
	}
	// The ROOT stays, deliberately: churning a path the container runtime's
	// shared mount is caching makes the next run's mount source fail to resolve
	// on virtiofs.
	if _, err := os.Stat(staged.Root); err != nil {
		t.Fatalf("Cleanup removed the staging root: %v", err)
	}
	// Removing twice is not an error, and the second call has nothing to do.
	if err := staged.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}

	// A path outside an AO staging root is refused, and the directory survives.
	elsewhere := t.TempDir()
	precious := filepath.Join(elsewhere, "someone-elses-work")
	if err := os.WriteFile(precious, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	stray := Staged{Dir: elsewhere}
	if err := stray.Cleanup(); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("Cleanup(%q) = %v; it must refuse a directory that is not AO's", elsewhere, err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatalf("the refused cleanup deleted anyway: %v", err)
	}
}

// A symlink planted inside the staged directory must not turn cleanup into a
// delete of what it points at. RemoveAll unlinks the link itself; this asserts
// that rather than assuming it.
func TestCleanup_DoesNotFollowASymlinkOutOfTheStagedDirectory(t *testing.T) {
	store, _ := stagingFixture(t)
	staged, err := Stage(StageRequest{Store: store, Root: root(t), RunID: "run-1", RuntimeArch: "aarch64",
		Policy: []byte(`{"allow":[]}`)})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "keep-me")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(staged.Dir, "escape")); err != nil {
		t.Skipf("this host does not allow symlinks: %v", err)
	}
	if err := staged.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("cleanup followed the symlink and deleted through it: %v", err)
	}
}

// Requirement 2, at the staging layer: a caller cannot ask for an architecture
// the container runtime did not report. Stage takes what the runtime SAID and
// maps it itself, so "select only the compatible binary" is a property of the
// API rather than a rule callers are asked to follow.
func TestStage_MapsTheRuntimesOwnArchitectureAndRefusesAnythingElse(t *testing.T) {
	body := []byte("the arm64 proxy")
	store := fixture(t, map[string][]byte{"linux-arm64": body})
	base := root(t)
	policy := []byte(`{"allow":[]}`)

	// What the runtime reports is what gets staged.
	staged, err := Stage(StageRequest{Store: store, Root: base, RunID: "native",
		RuntimeArch: "aarch64", Policy: policy})
	if err != nil {
		t.Fatalf("Stage(aarch64): %v", err)
	}
	t.Cleanup(func() { _ = staged.Cleanup() })
	if staged.Artifact.GOARCH != "arm64" {
		t.Fatalf("staged linux/%s for a runtime reporting aarch64", staged.Artifact.GOARCH)
	}

	// A runtime reporting something this build does not package is a refusal,
	// never the other artifact.
	if _, err := Stage(StageRequest{Store: store, Root: base, RunID: "foreign",
		RuntimeArch: "x86_64", Policy: policy}); !errors.Is(err, ErrArchUnsupported) {
		t.Fatalf("err = %v, want ErrArchUnsupported", err)
	}
	// And an answer AO cannot map at all is a refusal, not a default.
	for _, reported := range []string{"", "s390x", "armv7l"} {
		if _, err := Stage(StageRequest{Store: store, Root: base, RunID: "unmapped",
			RuntimeArch: reported, Policy: policy}); !errors.Is(err, ErrArchUnsupported) {
			t.Fatalf("runtime arch %q gave err = %v, want ErrArchUnsupported", reported, err)
		}
	}
	// None of the refusals created anything.
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the root holds %d entries; only the staged one should exist", len(entries))
	}
}
