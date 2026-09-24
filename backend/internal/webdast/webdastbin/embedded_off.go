//go:build !ao_embed_web_dast

package webdastbin

// embedded_off.go — the default build, including every `go build ./...` and
// every `go test ./...` in this repository.
//
// It carries no checker. That is not a degraded mode to work around: Select
// returns ErrNotPackaged, the runner refuses to run active-pentest, and the
// capability stays fail-closed. A development daemon that quietly ran an
// unverified checker would be worse than one that refuses.
func embeddedArtifact(string) ([]byte, bool) { return nil, false }
