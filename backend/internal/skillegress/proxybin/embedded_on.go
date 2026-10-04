//go:build ao_embed_egress_proxy

package proxybin

import "embed"

// embedded_on.go — the release build. It carries every artifact
// scripts/build-egress-proxy.sh produces, and the embed directive FAILS THE
// BUILD when one is missing: a release that would have shipped without the
// amd64 proxy does not compile, rather than shipping and refusing at run time
// on exactly the machines that needed it.
//
// The proxy command itself must never be built with this tag. It does not
// import this package, so it cannot embed itself.
//
//go:embed artifacts/ao-egress-proxy-linux-amd64 artifacts/ao-egress-proxy-linux-arm64
var binaries embed.FS

func embeddedArtifact(file string) ([]byte, bool) {
	body, err := binaries.ReadFile("artifacts/" + file)
	if err != nil {
		return nil, false
	}
	return body, true
}
