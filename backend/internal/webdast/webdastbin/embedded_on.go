//go:build ao_embed_web_dast

package webdastbin

import "embed"

// embedded_on.go — the release build. It carries every artifact
// scripts/build-web-dast.sh produces, and the embed directive FAILS THE BUILD
// when one is missing: a release that would have shipped without the arm64
// checker does not compile, rather than shipping and refusing at run time on
// exactly the machines that needed it.
//
// The ao-web-dast command itself must never be built with this tag: it does not
// import this package, so it cannot embed itself.
//
//go:embed artifacts/ao-web-dast-linux-amd64 artifacts/ao-web-dast-linux-arm64
var binaries embed.FS

func embeddedArtifact(file string) ([]byte, bool) {
	body, err := binaries.ReadFile("artifacts/" + file)
	if err != nil {
		return nil, false
	}
	return body, true
}
