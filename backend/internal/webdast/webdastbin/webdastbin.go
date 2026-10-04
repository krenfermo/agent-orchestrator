// Package webdastbin packages the ao-web-dast checker as an artifact the daemon
// carries, exactly as proxybin packages the egress proxy: built ahead of time
// by scripts/build-web-dast.sh, pinned by SHA-256 in provenance.json, and
// embedded under the build tag ao_embed_web_dast. At run time AO SELECTS the
// right binary for the container runtime's architecture and verifies its
// digest; it never compiles the checker while a pentest runs.
//
// The proxy pattern is reused deliberately, including its errors: a checker AO
// cannot identify by digest is one it will not run, so a default `go build` (no
// tag) carries no checker, Packaged() is false, and RunActivePentest refuses
// with the missing artifact named. Fail-closed, like everything in this area.
//
// The pure helpers (Digest, ArchFromRuntime) and the provenance shape are
// shared with proxybin rather than copied, so "the digest" and "which arch"
// mean the same thing for both binaries.
package webdastbin

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress/proxybin"
)

//go:embed provenance.json
var provenanceJSON []byte

var (
	// ErrNotPackaged means this daemon build carries no checker. It is the
	// expected state of a plain `go build ./...`, and the reason active-pentest
	// refuses on a development build.
	ErrNotPackaged = errors.New("webdast/webdastbin: this build carries no ao-web-dast checker")
	// ErrArchUnsupported means a checker is packaged but not for the runtime's
	// architecture.
	ErrArchUnsupported = errors.New("webdast/webdastbin: no ao-web-dast checker for this architecture")
	// ErrDigestMismatch means the bytes are not the ones provenance records.
	ErrDigestMismatch = errors.New("webdast/webdastbin: ao-web-dast digest does not match its provenance")
)

// Store is a source of checker artifacts. Embedded() is the one production
// source; the type exists so the runner's tests can inject a checker they built
// themselves (StoreForTest) without a global.
type Store struct {
	provenance proxybin.Provenance
	read       func(file string) ([]byte, bool)
}

// Embedded is the store this daemon build carries.
func Embedded() Store {
	return Store{provenance: embeddedProvenance(), read: embeddedArtifact}
}

// newStore builds a store over arbitrary bytes. Unexported: nothing outside
// this package (and its test-only door) may substitute what the daemon runs.
func newStore(p proxybin.Provenance, read func(string) ([]byte, bool)) Store {
	return Store{provenance: p, read: read}
}

// Provenance returns what was recorded at build time.
func (s Store) Provenance() proxybin.Provenance { return s.provenance }

// Packaged reports whether any checker binary is actually carried.
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

// Select returns the artifact for one GOARCH and its verified bytes. The digest
// is checked here, before the bytes are handed to anything that writes them.
func (s Store) Select(goarch string) (proxybin.Artifact, []byte, error) {
	if err := s.provenance.Validate(); err != nil {
		return proxybin.Artifact{}, nil, err
	}
	if !s.Packaged() {
		return proxybin.Artifact{}, nil, fmt.Errorf(
			"%w: build the daemon with -tags ao_embed_web_dast after running "+
				"scripts/build-web-dast.sh (recorded artifact version %s for linux/%s)",
			ErrNotPackaged, s.provenance.ArtifactVersion, strings.Join(s.provenance.Architectures(), ", linux/"))
	}
	var want proxybin.Artifact
	for _, a := range s.provenance.Artifacts {
		if a.GOARCH == goarch {
			want = a
			break
		}
	}
	if want.File == "" {
		return proxybin.Artifact{}, nil, fmt.Errorf("%w: the runtime runs linux/%s and this build packages linux/%s",
			ErrArchUnsupported, goarch, strings.Join(s.provenance.Architectures(), ", linux/"))
	}
	body, ok := s.read(want.File)
	if !ok {
		return proxybin.Artifact{}, nil, fmt.Errorf("%w: linux/%s is recorded as %s but no bytes are embedded for it",
			ErrNotPackaged, goarch, want.File)
	}
	if int64(len(body)) != want.Bytes {
		return proxybin.Artifact{}, nil, fmt.Errorf("%w: %s is %d bytes, provenance records %d",
			ErrDigestMismatch, want.File, len(body), want.Bytes)
	}
	if got := proxybin.Digest(body); got != want.SHA256 {
		return proxybin.Artifact{}, nil, fmt.Errorf("%w: %s hashes to %s, provenance records %s",
			ErrDigestMismatch, want.File, got, want.SHA256)
	}
	return want, body, nil
}

// ArchFromRuntime maps a container runtime's reported architecture to a GOARCH,
// reusing proxybin's closed map so both binaries answer the question the same
// way.
func ArchFromRuntime(reported string) (string, error) { return proxybin.ArchFromRuntime(reported) }

func embeddedProvenance() proxybin.Provenance {
	var p proxybin.Provenance
	if err := json.Unmarshal(provenanceJSON, &p); err != nil {
		return proxybin.Provenance{}
	}
	return p
}
