package proxybin

import (
	"errors"
	"strings"
	"testing"
)

// The tests in this file are the negative half of phase 8: every way selecting
// a proxy can go wrong, and the requirement that each one is a REFUSAL rather
// than a fallback. There is no case here that ends in "AO used something else".

// fixture builds a store over bytes a test controls, with a provenance that
// describes them honestly unless the test is about it not doing so.
func fixture(t *testing.T, files map[string][]byte) Store {
	t.Helper()
	p := Provenance{
		ArtifactVersion: "test", Source: "./cmd/ao-egress-proxy",
		Toolchain: "go1.26.6", BuildEnv: "CGO_ENABLED=0", BuildFlags: "-trimpath",
	}
	for _, arch := range []string{"amd64", "arm64"} {
		body, ok := files["linux-"+arch]
		if !ok {
			continue
		}
		p.Artifacts = append(p.Artifacts, Artifact{
			GOOS: "linux", GOARCH: arch, File: "ao-egress-proxy-linux-" + arch,
			SHA256: Digest(body), Bytes: int64(len(body)),
		})
	}
	return storeOver(p, files)
}

func storeOver(p Provenance, files map[string][]byte) Store {
	return newStore(p, func(file string) ([]byte, bool) {
		body, ok := files[strings.TrimPrefix(file, "ao-egress-proxy-")]
		return body, ok
	})
}

// Requirement: a build that packages nothing attests nothing. This is the state
// of every `go build ./...` in this repository, and it must be a clean refusal
// rather than a nil binary somebody dereferences later.
func TestSelect_ABuildWithNoProxyRefuses(t *testing.T) {
	empty := fixture(t, map[string][]byte{})
	// A provenance with no artifacts is itself unreadable: nothing could be
	// selected from it, so the failure comes even before "not packaged".
	if _, _, err := empty.Select("arm64"); !errors.Is(err, ErrProvenanceUnreadable) {
		t.Fatalf("err = %v, want ErrProvenanceUnreadable", err)
	}

	// The real shape: the provenance records both artifacts, and the build
	// embedded neither. That is a development daemon.
	described := fixture(t, map[string][]byte{"linux-amd64": []byte("amd64 bytes"), "linux-arm64": []byte("arm64 bytes")})
	unpackaged := newStore(described.Provenance(), func(string) ([]byte, bool) { return nil, false })
	if unpackaged.Packaged() {
		t.Fatal("a build with no embedded bytes reported itself packaged")
	}
	_, _, err := unpackaged.Select("arm64")
	if !errors.Is(err, ErrNotPackaged) {
		t.Fatalf("err = %v, want ErrNotPackaged", err)
	}
	// The refusal must say how to fix it, because the fix is a build flag
	// nobody guesses.
	if !strings.Contains(err.Error(), "ao_embed_egress_proxy") {
		t.Fatalf("the refusal should name the build tag: %v", err)
	}
}

// Requirement: the daemon selects ONLY the binary compatible with the Linux
// runtime the VM runs. An architecture AO does not package is a refusal, never
// the other one.
func TestSelect_RefusesAnArchitectureItDoesNotPackage(t *testing.T) {
	// A build that packages only arm64 must not hand an arm64 ELF to an amd64
	// runtime. Silently substituting is an exec-format error AFTER the control
	// has been attested, which is the worst moment to discover it.
	only := Provenance{
		ArtifactVersion: "test", Toolchain: "go1.26.6",
		Artifacts: []Artifact{{
			GOOS: "linux", GOARCH: "arm64", File: "ao-egress-proxy-linux-arm64",
			SHA256: Digest([]byte("arm64 bytes")), Bytes: int64(len("arm64 bytes")),
		}},
	}
	store := storeOver(only, map[string][]byte{"linux-arm64": []byte("arm64 bytes")})

	if _, _, err := store.Select("amd64"); !errors.Is(err, ErrArchUnsupported) {
		t.Fatalf("err = %v, want ErrArchUnsupported", err)
	}
	if _, _, err := store.Select("riscv64"); !errors.Is(err, ErrArchUnsupported) {
		t.Fatalf("err = %v, want ErrArchUnsupported", err)
	}
	// The one it does package still works, so the refusal above is selection
	// and not a broken store.
	a, body, err := store.Select("arm64")
	if err != nil {
		t.Fatalf("Select(arm64): %v", err)
	}
	if a.GOARCH != "arm64" || string(body) != "arm64 bytes" {
		t.Fatalf("selected %+v / %q", a, body)
	}
}

// Requirement: a corrupted or altered artifact is refused. This is the check
// that makes the embed a supply-chain boundary rather than a delivery
// mechanism: bytes that do not hash to the recorded digest are not run.
func TestSelect_RefusesBytesThatDoNotMatchTheDigest(t *testing.T) {
	good := []byte("the proxy, exactly as built")
	honest := fixture(t, map[string][]byte{"linux-arm64": good})

	cases := []struct {
		name string
		body []byte
	}{
		{"one flipped byte, same length", []byte("the proxy, exactly as bui1t")},
		{"truncated", good[:len(good)-3]},
		{"appended to", append(append([]byte(nil), good...), 'x')},
		{"empty", []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tampered := newStore(honest.Provenance(), func(string) ([]byte, bool) { return tc.body, true })
			_, _, err := tampered.Select("arm64")
			if !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("err = %v, want ErrDigestMismatch", err)
			}
			// A length change is caught by length and a same-length change by
			// the hash; both must name what was recorded so an operator can
			// compare it against provenance.json by eye.
			if !strings.Contains(err.Error(), honest.Provenance().Artifacts[0].SHA256[:16]) &&
				!strings.Contains(err.Error(), "provenance records") {
				t.Fatalf("the refusal should name the recorded digest: %v", err)
			}
		})
	}
}

// Requirement: a provenance that cannot decide anything is refused before it
// can be used to approve bytes. A digest field nobody validated is a digest
// field somebody can widen.
func TestProvenance_RefusesEveryUnusableRecord(t *testing.T) {
	body := []byte("bytes")
	valid := Artifact{GOOS: "linux", GOARCH: "arm64", File: "p", SHA256: Digest(body), Bytes: int64(len(body))}
	cases := []struct {
		name    string
		p       Provenance
		wantSub string
	}{
		{"no version", Provenance{Toolchain: "go1.26.6", Artifacts: []Artifact{valid}}, "no artifact version"},
		{"no toolchain", Provenance{ArtifactVersion: "1", Artifacts: []Artifact{valid}}, "no artifact version"},
		{"no artifacts", Provenance{ArtifactVersion: "1", Toolchain: "go1.26.6"}, "records no artifacts"},
		{
			"a non-Linux target", Provenance{ArtifactVersion: "1", Toolchain: "go1.26.6",
				Artifacts: []Artifact{{GOOS: "darwin", GOARCH: "arm64", File: "p", SHA256: Digest(body), Bytes: 5}}},
			"a skill container is Linux",
		},
		{
			"a digest that is not one", Provenance{ArtifactVersion: "1", Toolchain: "go1.26.6",
				Artifacts: []Artifact{{GOOS: "linux", GOARCH: "arm64", File: "p", SHA256: "deadbeef", Bytes: 5}}},
			"not a sha256 digest",
		},
		{
			"a digest with non-hex in it", Provenance{ArtifactVersion: "1", Toolchain: "go1.26.6",
				Artifacts: []Artifact{{GOOS: "linux", GOARCH: "arm64", File: "p",
					SHA256: strings.Repeat("z", 64), Bytes: 5}}},
			"not a sha256 digest",
		},
		{
			"two artifacts for one architecture", Provenance{ArtifactVersion: "1", Toolchain: "go1.26.6",
				Artifacts: []Artifact{valid, valid}},
			"coin flip",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if !errors.Is(err, ErrProvenanceUnreadable) {
				t.Fatalf("err = %v, want ErrProvenanceUnreadable", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error should say why: %v", err)
			}
		})
	}
}

// Requirement: an architecture AO cannot map is a refusal, not a guess. The
// wrong guess writes an ELF the kernel cannot exec, and it would be discovered
// after the capability table had already been told the control was available.
func TestArchFromRuntime_RefusesWhatItCannotMap(t *testing.T) {
	for reported, want := range map[string]string{
		"x86_64": "amd64", "amd64": "amd64", "X86_64": "amd64",
		"aarch64": "arm64", "arm64": "arm64", " aarch64 ": "arm64",
	} {
		got, err := ArchFromRuntime(reported)
		if err != nil || got != want {
			t.Fatalf("ArchFromRuntime(%q) = %q, %v; want %q", reported, got, err, want)
		}
	}
	for _, reported := range []string{"", "  ", "armv7l", "s390x", "riscv64", "i386"} {
		if _, err := ArchFromRuntime(reported); !errors.Is(err, ErrArchUnsupported) {
			t.Fatalf("ArchFromRuntime(%q) err = %v, want ErrArchUnsupported", reported, err)
		}
	}
}

// The committed provenance is the artifact a human reviews. It must be
// well-formed in every build, including the default one that embeds no
// binaries — otherwise the release build's digest comparison is meaningless.
func TestEmbeddedProvenance_IsWellFormedAndCoversBothArchitectures(t *testing.T) {
	p := Embedded().Provenance()
	if err := p.Validate(); err != nil {
		t.Fatalf("the committed provenance.json is unusable: %v", err)
	}
	archs := strings.Join(p.Architectures(), ",")
	if archs != "amd64,arm64" {
		t.Fatalf("provenance covers %q; a skill container runs on amd64 and arm64", archs)
	}
	if !strings.HasPrefix(p.Toolchain, "go1.") {
		t.Fatalf("toolchain = %q, want a recorded Go version", p.Toolchain)
	}
	// The flags are what makes the build reproducible. If they stop being
	// recorded, `--record` produced a provenance nobody can reproduce from.
	for _, want := range []string{"-trimpath", "-buildvcs=false", "-buildid="} {
		if !strings.Contains(p.BuildFlags, want) {
			t.Fatalf("buildFlags = %q, missing %q", p.BuildFlags, want)
		}
	}
	if !strings.Contains(p.BuildEnv, "CGO_ENABLED=0") {
		t.Fatalf("buildEnv = %q, want CGO_ENABLED=0", p.BuildEnv)
	}
}

// The default build carries no binaries, on purpose. This test asserts the
// fail-closed default rather than the release behaviour: it is what makes
// `go test ./...` on a developer's machine mean the same thing as CI.
func TestEmbedded_DefaultBuildPackagesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("not a boundary test")
	}
	store := Embedded()
	if store.Packaged() != packagedInThisBuild {
		t.Fatalf("Packaged() = %v, want %v for this build", store.Packaged(), packagedInThisBuild)
	}
	if packagedInThisBuild {
		// A release build must be able to select each recorded architecture,
		// and the digest check must pass against the real embedded bytes.
		for _, a := range store.Provenance().Artifacts {
			got, body, err := store.Select(a.GOARCH)
			if err != nil {
				t.Fatalf("Select(%s): %v", a.GOARCH, err)
			}
			if Digest(body) != a.SHA256 || got.File != a.File {
				t.Fatalf("linux/%s selected %s", a.GOARCH, got.File)
			}
		}
		return
	}
	if _, _, err := store.Select("arm64"); !errors.Is(err, ErrNotPackaged) {
		t.Fatalf("err = %v, want ErrNotPackaged on a build with no embed tag", err)
	}
}
