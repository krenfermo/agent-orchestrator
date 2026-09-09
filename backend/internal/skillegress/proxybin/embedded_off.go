//go:build !ao_embed_egress_proxy

package proxybin

// embedded_off.go — the default build, including every `go build ./...` and
// every `go test ./...` in this repository.
//
// It carries no proxy. That is not a degraded mode to work around: Select
// returns ErrNotPackaged, the runner attests no allowlist, and net.egress stays
// refused with the missing control named. A development daemon that quietly
// enforced nothing while reporting an allowlist would be worse than one that
// refuses.
func embeddedArtifact(string) ([]byte, bool) { return nil, false }
