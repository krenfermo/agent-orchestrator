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

// ErrNoInstanceIdentity is returned when no instance identity is available: an
// endpoint without one cannot be proven to belong to anyone, so it is never created.
var ErrNoInstanceIdentity = errors.New("supervisor: no daemon instance identity for the endpoint")

// EndpointToken is the short, stable token derived from a daemon instance id.
func EndpointToken(instanceID string) (string, error) {
	// No normalization: two spellings of an id must never share an endpoint.
	if instanceID == "" || strings.TrimSpace(instanceID) != instanceID {
		return "", ErrNoInstanceIdentity
	}
	sum := sha256.Sum256([]byte(instanceID))
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

// Prior is the incarnation that last wrote this daemon's run-file. Its endpoint
// is the only one a starting daemon may clean up, and only when Exited is true:
// the caller holds the run-file's exclusive lock AND the recorded process is not
// alive. Without that, the endpoint is never even probed -- a probe is itself a
// supervisor client and would arm a live daemon's watchdog.
type Prior struct {
	InstanceID string
	Address    string
	Exited     bool
}
