package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// The supervisor endpoint is named after the daemon INSTANCE (P9's per-process
// identity), never after where the run-file happens to live: two daemons whose
// run-files share a directory, or two installations, must never share an
// endpoint, and a client that linked instance A must never reach instance B by
// connecting to "the same" name. The exact address is published in the
// run-file (supervisorAddress) by the same atomic write that publishes the
// instance, so clients read it instead of deriving it.

// endpointTokenHexLen keeps names short (sockaddr_un is 104 bytes on macOS)
// while leaving 64 bits between two random instance ids.
const endpointTokenHexLen = 16

// ErrNoInstanceIdentity: an endpoint without an instance identity cannot be
// proven to belong to anyone, so it is never created.
var ErrNoInstanceIdentity = errors.New("supervisor: no daemon instance identity for the endpoint")

// EndpointToken is the short, stable token derived from a daemon instance id.
func EndpointToken(instanceID string) (string, error) {
	id := strings.TrimSpace(instanceID)
	if id == "" {
		return "", ErrNoInstanceIdentity
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:endpointTokenHexLen], nil
}

// SocketName is the Unix socket file name for an instance (placed beside the run-file).
func SocketName(instanceID string) (string, error) {
	token, err := EndpointToken(instanceID)
	if err != nil {
		return "", err
	}
	return "supervise-" + token + ".sock", nil
}

// PipeName is the Windows named-pipe address for an instance.
func PipeName(instanceID string) (string, error) {
	token, err := EndpointToken(instanceID)
	if err != nil {
		return "", err
	}
	return `\\.\pipe\ao-supervise-` + token, nil
}

// Prior is the incarnation that last wrote this daemon's run-file, read while
// the new daemon holds the run-file's exclusive lock -- so that incarnation is
// no longer running. Its endpoint is the only one a starting daemon may clean up.
type Prior struct {
	InstanceID string
	Address    string
}
