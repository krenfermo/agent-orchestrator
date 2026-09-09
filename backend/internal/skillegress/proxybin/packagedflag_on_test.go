//go:build ao_embed_egress_proxy

package proxybin

// packagedInThisBuild lets one test assert the right thing under both build
// tags, so `go test` and `go test -tags ao_embed_egress_proxy` both check the
// behaviour they actually have rather than one of them being skipped.
const packagedInThisBuild = true
