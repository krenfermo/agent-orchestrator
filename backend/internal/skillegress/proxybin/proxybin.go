// Package proxybin packages ao-egress-proxy as an artifact the daemon carries,
// rather than as something compiled while a skill runs.
//
// # Why this package exists
//
// The egress allowlist is enforced by a Go binary that must run INSIDE a Linux
// container. Phase 7 proved the enforcement by cross-compiling the proxy in the
// test and bind-mounting the result. That is fine for a test and wrong for a
// product: it needs a Go toolchain on the operator's machine at run time, it
// compiles unreviewed bytes at the moment they are trusted, and it gives nobody
// a way to say which proxy actually ran.
//
// So the proxy is built ahead of time by scripts/build-egress-proxy.sh, pinned
// by SHA-256 in provenance.json, and embedded into the daemon under the build
// tag ao_embed_egress_proxy. At run time AO SELECTS -- it never compiles.
//
// # What "the right binary" means
//
// One thing, checked three ways:
//
//  1. It is packaged at all. A daemon built without the tag carries no proxy,
//     Packaged() is false, and every caller must refuse rather than fall back.
//  2. It matches the architecture the container runtime actually runs. On macOS
//     that is the VM's architecture, not the daemon's: an arm64 daemon driving
//     an amd64 VM needs the amd64 proxy, and the wrong one is an exec-format
//     error at the worst possible moment -- after AO has already told the
//     capability table the control is available.
//  3. Its bytes hash to the digest provenance.json records. That is checked
//     when the artifact is selected AND again after it is written to disk, so
//     the window between the two is not somewhere a swap can hide.
//
// Every failure here is a refusal. There is no "use whatever is on the host"
// path, because a proxy AO cannot identify is a proxy AO cannot attest.
package proxybin

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The provenance is embedded UNCONDITIONALLY, unlike the binaries. A daemon
// that carries no proxy can still say which one it would have carried, which
// turns "egress is unavailable" into a diagnosable statement.
//
//go:embed provenance.json
var provenanceJSON []byte

var (
	// ErrNotPackaged means this daemon build carries no proxy at all. It is the
	// expected state of a plain `go build ./...`, and the reason the runner
	// attests no allowlist on a development build.
	ErrNotPackaged = errors.New("skillegress/proxybin: this build carries no egress proxy")
	// ErrArchUnsupported means a proxy is packaged but not for the
	// architecture the container runtime runs.
	ErrArchUnsupported = errors.New("skillegress/proxybin: no egress proxy for this architecture")
	// ErrDigestMismatch means the bytes are not the ones provenance.json
	// records. It is deliberately not recoverable: there is no second attempt
	// and no fallback artifact.
	ErrDigestMismatch = errors.New("skillegress/proxybin: egress proxy digest does not match its provenance")
	// ErrProvenanceUnreadable means the embedded provenance is malformed, which
	// makes every digest comparison meaningless.
	ErrProvenanceUnreadable = errors.New("skillegress/proxybin: provenance.json is unreadable")
)

// Artifact is one built proxy binary, as recorded when it was built.
type Artifact struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	// File is the name under artifacts/, and the name written into staging.
	File string `json:"file"`
	// SHA256 is the digest the reproducible build produced. It is the identity
	// of this artifact; ArtifactVersion is only how a person refers to it.
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Provenance is what scripts/build-egress-proxy.sh recorded: which source, on
// which toolchain, with which flags, produced which bytes.
//
// It is committed and reviewable. The binaries are not committed -- they are
// 6 MiB each and would be reviewable by nobody -- so this file is the artifact
// a human actually approves, and the build script is what proves the bytes
// still match it.
type Provenance struct {
	ArtifactVersion string     `json:"artifactVersion"`
	Source          string     `json:"source"`
	Toolchain       string     `json:"toolchain"`
	BuildEnv        string     `json:"buildEnv"`
	BuildFlags      string     `json:"buildFlags"`
	Artifacts       []Artifact `json:"artifacts"`
}

// Validate refuses a provenance that could not decide anything.
func (p Provenance) Validate() error {
	if strings.TrimSpace(p.ArtifactVersion) == "" || strings.TrimSpace(p.Toolchain) == "" {
		return fmt.Errorf("%w: it names no artifact version or no toolchain", ErrProvenanceUnreadable)
	}
	if len(p.Artifacts) == 0 {
		return fmt.Errorf("%w: it records no artifacts", ErrProvenanceUnreadable)
	}
	seen := map[string]bool{}
	for _, a := range p.Artifacts {
		switch {
		case a.GOOS != "linux":
			return fmt.Errorf("%w: %s targets %q; a skill container is Linux",
				ErrProvenanceUnreadable, a.File, a.GOOS)
		case strings.TrimSpace(a.GOARCH) == "" || strings.TrimSpace(a.File) == "":
			return fmt.Errorf("%w: an artifact has no architecture or no file name", ErrProvenanceUnreadable)
		case len(a.SHA256) != 64 || strings.Trim(a.SHA256, "0123456789abcdef") != "":
			return fmt.Errorf("%w: %s records %q, which is not a sha256 digest",
				ErrProvenanceUnreadable, a.File, a.SHA256)
		case a.Bytes <= 0:
			return fmt.Errorf("%w: %s records %d bytes", ErrProvenanceUnreadable, a.File, a.Bytes)
		case seen[a.GOARCH]:
			return fmt.Errorf("%w: two artifacts claim linux/%s, so selection would be a coin flip",
				ErrProvenanceUnreadable, a.GOARCH)
		}
		seen[a.GOARCH] = true
	}
	return nil
}

// Architectures lists what this provenance covers, in a stable order.
func (p Provenance) Architectures() []string {
	out := make([]string, 0, len(p.Artifacts))
	for _, a := range p.Artifacts {
		out = append(out, a.GOARCH)
	}
	sort.Strings(out)
	return out
}

// Store is a source of proxy artifacts. There is exactly one in production --
// Embedded() -- and the type exists so this package's negative tests can hand
// Select a corrupt, truncated or absent artifact without touching a global.
type Store struct {
	provenance Provenance
	// read returns the bytes for one recorded file name. A build with no
	// binaries embedded has a read that answers nothing, which is what makes
	// ErrNotPackaged the default rather than a special case.
	read func(file string) ([]byte, bool)
}

// Embedded is the store this daemon build carries.
func Embedded() Store {
	return Store{provenance: embeddedProvenance(), read: embeddedArtifact}
}

// newStore builds a store over arbitrary bytes. Unexported on purpose: nothing
// outside this package may substitute what the daemon runs.
func newStore(p Provenance, read func(string) ([]byte, bool)) Store {
	return Store{provenance: p, read: read}
}

// Provenance returns what was recorded at build time.
func (s Store) Provenance() Provenance { return s.provenance }

// Packaged reports whether any proxy binary is actually carried. A daemon built
// without -tags ao_embed_egress_proxy answers false, and that answer is the
// reason egress stays unattested on a development build.
func (s Store) Packaged() bool {
	if s.read == nil {
		return false
	}
	for _, a := range s.provenance.Artifacts {
		if _, ok := s.read(a.File); ok {
			return true
		}
	}
	return false
}

// Select returns the artifact for one GOARCH and its verified bytes.
//
// The digest is checked HERE, before the bytes are handed to anything that
// would write them. A caller that skips the check cannot: there is no accessor
// that returns unverified bytes.
func (s Store) Select(goarch string) (Artifact, []byte, error) {
	if err := s.provenance.Validate(); err != nil {
		return Artifact{}, nil, err
	}
	if !s.Packaged() {
		return Artifact{}, nil, fmt.Errorf(
			"%w: build the daemon with -tags ao_embed_egress_proxy after running "+
				"scripts/build-egress-proxy.sh (recorded artifact version %s for linux/%s)",
			ErrNotPackaged, s.provenance.ArtifactVersion, strings.Join(s.provenance.Architectures(), ", linux/"))
	}
	var want Artifact
	for _, a := range s.provenance.Artifacts {
		if a.GOARCH == goarch {
			want = a
			break
		}
	}
	if want.File == "" {
		return Artifact{}, nil, fmt.Errorf("%w: the runtime runs linux/%s and this build packages linux/%s",
			ErrArchUnsupported, goarch, strings.Join(s.provenance.Architectures(), ", linux/"))
	}
	body, ok := s.read(want.File)
	if !ok {
		return Artifact{}, nil, fmt.Errorf("%w: linux/%s is recorded as %s but no bytes are embedded for it",
			ErrNotPackaged, goarch, want.File)
	}
	if int64(len(body)) != want.Bytes {
		return Artifact{}, nil, fmt.Errorf("%w: %s is %d bytes, provenance records %d",
			ErrDigestMismatch, want.File, len(body), want.Bytes)
	}
	if got := Digest(body); got != want.SHA256 {
		return Artifact{}, nil, fmt.Errorf("%w: %s hashes to %s, provenance records %s",
			ErrDigestMismatch, want.File, got, want.SHA256)
	}
	return want, body, nil
}

// Digest is the one hash function this package uses, so "the digest" means the
// same thing in provenance.json, in Select and in staging.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ArchFromRuntime maps what a container runtime reports to a GOARCH.
//
// It is deliberately a closed map. `docker info` answers x86_64 or aarch64 on
// the platforms AO supports, and an unrecognised answer is a refusal rather
// than a guess: guessing wrong packages an ELF the kernel cannot exec, and the
// failure would land after the control had already been attested.
func ArchFromRuntime(reported string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	case "":
		return "", fmt.Errorf("%w: the container runtime reported no architecture", ErrArchUnsupported)
	default:
		return "", fmt.Errorf("%w: the container runtime reports %q, which AO does not package a proxy for",
			ErrArchUnsupported, reported)
	}
}

// embeddedProvenance parses the committed record once per call. It returns a
// zero Provenance on a malformed file rather than panicking at init: a daemon
// that cannot read its provenance must refuse egress, not fail to start.
func embeddedProvenance() Provenance {
	var p Provenance
	if err := json.Unmarshal(provenanceJSON, &p); err != nil {
		return Provenance{}
	}
	return p
}
