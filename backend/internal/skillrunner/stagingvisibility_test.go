package skillrunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The regression this file exists for is the first static-code pilot: the
// runtime was a VM that did not share the staging root, the bind mount produced
// an EMPTY directory, and the failure surfaced three layers later as "the run
// did not demonstrate filesystem_isolation" — a sentence that names a control
// and says nothing about a mount.
//
// Every test below is about failing earlier and saying the true thing.

func TestValidateStagingRoot_RefusesRootsAOMustNotStageInto(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "aodata")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("a relative root", func(t *testing.T) {
		if err := ValidateStagingRoot("relative/path", data); err == nil {
			t.Fatal("accepted a relative staging root")
		}
	})

	t.Run("a traversing root", func(t *testing.T) {
		// filepath.Clean cannot resolve a leading "..", so it survives and the
		// mount would land outside anything an operator reviewed.
		if err := ValidateStagingRoot("/var/../../etc", data); err == nil {
			t.Fatal("accepted a traversing staging root")
		}
	})

	// The one directory a run must never see: it holds AO's database and every
	// credential in it.
	t.Run("inside AO's data dir", func(t *testing.T) {
		err := ValidateStagingRoot(filepath.Join(data, "staging"), data)
		if err == nil {
			t.Fatal("accepted a staging root inside AO's data directory")
		}
		if !strings.Contains(err.Error(), "data directory") {
			t.Fatalf("the refusal must say why: %v", err)
		}
	})

	t.Run("the data dir itself", func(t *testing.T) {
		if err := ValidateStagingRoot(data, data); err == nil {
			t.Fatal("accepted AO's data directory as the staging root")
		}
	})

	// A symlinked parent would let the mount follow a link out of the reviewed
	// tree — the same class of problem Stage refuses symlinks for.
	t.Run("a symlinked parent", func(t *testing.T) {
		real := filepath.Join(base, "real")
		link := filepath.Join(base, "link")
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		err := ValidateStagingRoot(filepath.Join(link, "staging"), data)
		if err == nil {
			t.Fatal("accepted a staging root under a symlinked parent")
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("the refusal must name the symlink: %v", err)
		}
	})

	t.Run("the root itself is a symlink", func(t *testing.T) {
		real := filepath.Join(base, "real2")
		link := filepath.Join(base, "rootlink")
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := ValidateStagingRoot(link, data); err == nil {
			t.Fatal("accepted a staging root that is itself a symlink")
		}
	})

	// Anyone on the host could drop a file in between staging and launch, and
	// the container would scan it as if AO had put it there.
	t.Run("a world-writable parent", func(t *testing.T) {
		open := filepath.Join(base, "open")
		if err := os.MkdirAll(open, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, 0o777); err != nil {
			t.Fatal(err)
		}
		err := ValidateStagingRoot(filepath.Join(open, "staging"), data)
		if err == nil {
			t.Fatal("accepted a staging root under a world-writable parent")
		}
		if !strings.Contains(err.Error(), "world-writable") {
			t.Fatalf("the refusal must name the permissions: %v", err)
		}
	})

	t.Run("a parent that does not exist", func(t *testing.T) {
		if err := ValidateStagingRoot(filepath.Join(base, "nope", "staging"), data); err == nil {
			t.Fatal("accepted a staging root whose parent does not exist")
		}
	})

	// The shape a working host has: a real directory, sane permissions, outside
	// AO's data dir.
	t.Run("a usable root", func(t *testing.T) {
		good := filepath.Join(base, "projects")
		if err := os.MkdirAll(good, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStagingRoot(filepath.Join(good, ".ao-skill-staging"), data); err != nil {
			t.Fatalf("refused a perfectly usable staging root: %v", err)
		}
	})
}

// fakeCmd lets a test answer as the container runtime would, without one.
type fakeCmd struct {
	out []byte
	err error
	// seen records the argv, so a test can assert the probe is confined.
	seen []string
}

func (f *fakeCmd) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.seen = append([]string{name}, args...)
	return f.out, f.err
}

func probeRunner(cmd commandRunner) *Runner {
	return &Runner{runner: cmd, runtime: Runtime{Binary: "docker"}}
}

func TestVerifyStagingVisible_TheColimaShape(t *testing.T) {
	root := t.TempDir()
	img := ApprovedImage{Digest: "sha256:" + strings.Repeat("a", 64)}
	img.Contract.BaseImage = "alpine:3.19"

	// The incident, exactly: the runtime accepts the mount and the container
	// reads nothing back, because the VM never had the path.
	cmd := &fakeCmd{out: []byte("")}
	err := probeRunner(cmd).VerifyStagingVisible(context.Background(), root, img)
	if err == nil {
		t.Fatal("an invisible mount was accepted")
	}
	if !errors.Is(err, ErrStagingNotVisible) {
		t.Fatalf("wrong error kind: %v", err)
	}
	// The message has to send somebody to the runtime's share list, not to a
	// control name.
	for _, want := range []string{"EMPTY", "share", "AO_SKILL_STAGING_ROOT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must mention %q: %v", want, err)
		}
	}
	// And it must leave nothing behind in the operator's directory.
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("the probe left %d file(s) behind", len(entries))
	}
}

func TestVerifyStagingVisible_AVisiblePathPasses(t *testing.T) {
	root := t.TempDir()
	img := ApprovedImage{Digest: "sha256:" + strings.Repeat("b", 64)}
	img.Contract.BaseImage = "alpine:3.19"

	// A real runtime echoes back the sentinel AO just wrote. Read it off disk
	// so the test proves the round trip rather than a hardcoded string.
	reader := &sentinelEcho{root: root}
	r := probeRunner(reader)

	if err := r.VerifyStagingVisible(context.Background(), root, img); err != nil {
		t.Fatalf("a visible mount was refused: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("the probe left %d file(s) behind", len(entries))
	}

	// The probe must be as confined as a real run: no network, non-root,
	// read-only, no capabilities, and it must not pull.
	argv := strings.Join(reader.seen, " ")
	for _, want := range []string{
		"--network none", "--read-only", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--pull=never", "--user " + nobodyUser,
	} {
		if !strings.Contains(argv, want) {
			t.Fatalf("the probe container is missing %q: %s", want, argv)
		}
	}
}

// sentinelEcho answers with the sentinel file's real content, which is how a
// working runtime behaves.
type sentinelEcho struct {
	root string
	seen []string
}

func (s *sentinelEcho) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	s.seen = append([]string{name}, args...)
	body, err := os.ReadFile(filepath.Join(s.root, sentinelName))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func TestVerifyStagingVisible_WrongContentIsRefused(t *testing.T) {
	root := t.TempDir()
	img := ApprovedImage{Digest: "sha256:" + strings.Repeat("c", 64)}
	img.Contract.BaseImage = "alpine:3.19"

	// A stale VM cache serving an old sentinel: not empty, not right.
	cmd := &fakeCmd{out: []byte("some-other-token")}
	err := probeRunner(cmd).VerifyStagingVisible(context.Background(), root, img)
	if err == nil || !errors.Is(err, ErrStagingNotVisible) {
		t.Fatalf("a mount showing the wrong bytes was accepted: %v", err)
	}
}

func TestVerifyStagingVisible_NoRuntimeIsARefusal(t *testing.T) {
	r := &Runner{probeErr: errors.New("no container runtime on this host"), runner: &fakeCmd{}}
	img := ApprovedImage{Digest: "sha256:" + strings.Repeat("d", 64)}

	err := r.VerifyStagingVisible(context.Background(), t.TempDir(), img)
	if err == nil || !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("a host with no runtime did not refuse: %v", err)
	}
}

func TestVerifyStagedInputsDelivered(t *testing.T) {
	staged := Staging{
		Dir: "/host/.ao-skill-staging/run-1",
		Inputs: []StagedInput{
			{RelPath: "src/a.go", SHA256: strings.Repeat("1", 64), Bytes: 10},
			{RelPath: "src/b.go", SHA256: strings.Repeat("2", 64), Bytes: 20},
		},
	}
	want := stagedInputsDigest(staged.Inputs)

	t.Run("the tree AO staged", func(t *testing.T) {
		ev := BoundaryEvidence{InputFilesVisible: 2, InputDigest: want}
		if err := verifyStagedInputsDelivered(staged, ev); err != nil {
			t.Fatalf("a correct delivery was refused: %v", err)
		}
	})

	// The Colima shape again, this time caught after the run with a message
	// about the mount rather than about a control.
	t.Run("an empty mount", func(t *testing.T) {
		err := verifyStagedInputsDelivered(staged, BoundaryEvidence{InputFilesVisible: 0})
		if err == nil || !errors.Is(err, ErrStagedInputsMismatch) {
			t.Fatalf("an empty mount was accepted: %v", err)
		}
		if !strings.Contains(err.Error(), "saw none") {
			t.Fatalf("the refusal must say the container saw nothing: %v", err)
		}
	})

	// A count matches by accident; a digest does not. This is the case a
	// file-count check would wave through.
	t.Run("the right number of the wrong files", func(t *testing.T) {
		ev := BoundaryEvidence{InputFilesVisible: 2, InputDigest: strings.Repeat("9", 64)}
		err := verifyStagedInputsDelivered(staged, ev)
		if err == nil || !errors.Is(err, ErrStagedInputsMismatch) {
			t.Fatalf("a different tree with the same file count was accepted: %v", err)
		}
	})

	t.Run("no digest reported at all", func(t *testing.T) {
		ev := BoundaryEvidence{InputFilesVisible: 2}
		if err := verifyStagedInputsDelivered(staged, ev); err == nil {
			t.Fatal("a run that reported no digest was accepted")
		}
	})

	// An empty scope is legitimate — a project with nothing in the manifest's
	// read scope — and must not be confused with a broken mount.
	t.Run("nothing staged, nothing seen", func(t *testing.T) {
		empty := Staging{Dir: staged.Dir}
		ev := BoundaryEvidence{InputFilesVisible: 0, InputDigest: stagedInputsDigest(nil)}
		if err := verifyStagedInputsDelivered(empty, ev); err != nil {
			t.Fatalf("an honestly empty scope was refused: %v", err)
		}
	})
}

// The digest is order-independent and content-sensitive: those two properties
// are what make it a check rather than a formality.
func TestStagedInputsDigest_IsOrderIndependentAndContentSensitive(t *testing.T) {
	a := []StagedInput{
		{RelPath: "b.go", SHA256: strings.Repeat("2", 64), Bytes: 2},
		{RelPath: "a.go", SHA256: strings.Repeat("1", 64), Bytes: 1},
	}
	b := []StagedInput{
		{RelPath: "a.go", SHA256: strings.Repeat("1", 64), Bytes: 1},
		{RelPath: "b.go", SHA256: strings.Repeat("2", 64), Bytes: 2},
	}
	if stagedInputsDigest(a) != stagedInputsDigest(b) {
		t.Fatal("directory order changed the digest; it is not a fact about the tree")
	}

	changed := []StagedInput{
		{RelPath: "a.go", SHA256: strings.Repeat("1", 64), Bytes: 1},
		{RelPath: "b.go", SHA256: strings.Repeat("3", 64), Bytes: 2},
	}
	if stagedInputsDigest(b) == stagedInputsDigest(changed) {
		t.Fatal("different content produced the same digest")
	}
}
