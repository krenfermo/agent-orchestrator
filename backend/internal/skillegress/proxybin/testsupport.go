package proxybin

import (
	"fmt"
	"testing"
)

// testsupport.go — a door for tests that cannot be opened in a shipped binary.
//
// skillrunner has to prove that its attestation fails closed when the proxy is
// absent, is for the wrong architecture, or does not hash to its provenance.
// Doing that from another package needs a way to build a Store over bytes the
// test controls, and that is exactly the capability nothing in production may
// have: a substitutable proxy source is the whole control, defeated.
//
// So the constructors below panic unless the process is a test binary.
// testing.Testing() is false in `ao`, in the daemon and in the proxy itself, so
// a call that ever reached production would abort there rather than quietly
// hand somebody a proxy AO did not build.

func mustBeATest(fn string) {
	if !testing.Testing() {
		panic("skillegress/proxybin: " + fn + " is a test-only constructor and was called in a real binary; " +
			"a substitutable proxy source would defeat the digest check entirely")
	}
}

// StoreForTest builds a store that behaves like a release build carrying one
// artifact for goarch, whose bytes are body and whose provenance is honest
// about them.
func StoreForTest(goarch string, body []byte) Store {
	mustBeATest("StoreForTest")
	return newStore(provenanceForTest(goarch, body), func(string) ([]byte, bool) { return body, true })
}

// UnpackagedStoreForTest builds a store whose provenance records the artifact
// and whose build embedded no bytes for it — a plain `go build ./...` daemon.
func UnpackagedStoreForTest(goarch string, body []byte) Store {
	mustBeATest("UnpackagedStoreForTest")
	return newStore(provenanceForTest(goarch, body), func(string) ([]byte, bool) { return nil, false })
}

// CorruptStoreForTest builds a store whose embedded bytes are not the ones its
// provenance records.
func CorruptStoreForTest(goarch string, body []byte) Store {
	mustBeATest("CorruptStoreForTest")
	tampered := append(append([]byte(nil), body...), " tampered"...)
	return newStore(provenanceForTest(goarch, body), func(string) ([]byte, bool) { return tampered, true })
}

func provenanceForTest(goarch string, body []byte) Provenance {
	return Provenance{
		ArtifactVersion: "test",
		Source:          "./internal/skillegress/cmd/ao-egress-proxy",
		Toolchain:       "go1.26.6",
		BuildEnv:        "CGO_ENABLED=0",
		BuildFlags:      "-trimpath -buildvcs=false -ldflags=-s -w -buildid=",
		Artifacts: []Artifact{{
			GOOS: "linux", GOARCH: goarch,
			File:   fmt.Sprintf("ao-egress-proxy-linux-%s", goarch),
			SHA256: Digest(body), Bytes: int64(len(body)),
		}},
	}
}
