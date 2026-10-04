package webdastbin

import (
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress/proxybin"
)

// testsupport.go — a door for tests, closed in a shipped binary.
//
// The runner's live tests build the checker themselves and hand its bytes to
// RunActivePentest, exactly as the phase-7 egress tests built the proxy. That
// needs a Store over test-controlled bytes, which is the one capability nothing
// in production may have: a substitutable checker source is the digest control
// defeated. So these constructors panic unless the process is a test binary.

func mustBeATest(fn string) {
	if !testing.Testing() {
		panic("webdast/webdastbin: " + fn + " is a test-only constructor and was called in a real binary; " +
			"a substitutable checker source would defeat the digest check entirely")
	}
}

// StoreForTest builds a store that behaves like a release build carrying one
// checker artifact for goarch, whose bytes are body and whose provenance is
// honest about them.
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

func provenanceForTest(goarch string, body []byte) proxybin.Provenance {
	return proxybin.Provenance{
		ArtifactVersion: "test",
		Source:          "./cmd/ao-web-dast",
		Toolchain:       "go1.26.6",
		BuildEnv:        "CGO_ENABLED=0",
		BuildFlags:      "-trimpath -buildvcs=false -ldflags=-s -w -buildid=",
		Artifacts: []proxybin.Artifact{{
			GOOS: "linux", GOARCH: goarch,
			File:   fmt.Sprintf("ao-web-dast-linux-%s", goarch),
			SHA256: proxybin.Digest(body), Bytes: int64(len(body)),
		}},
	}
}
