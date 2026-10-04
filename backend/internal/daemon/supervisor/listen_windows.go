//go:build windows

package supervisor

import (
	"net"

	"github.com/Microsoft/go-winio"
)

// Listen creates this daemon instance's supervisor named pipe,
// \\.\pipe\ao-supervise-<token(instanceID)>. A named pipe disappears with its
// last server handle, so there is nothing stale to clean up; runFilePath and
// prior are accepted for signature parity with Unix. A pipe name that already
// exists fails the listen (fail closed) -- it is never shared.
func Listen(runFilePath, instanceID string, prior *Prior) (net.Listener, string, error) {
	_, _ = runFilePath, prior
	name, err := PipeName(instanceID)
	if err != nil {
		return nil, "", err
	}
	ln, err := winio.ListenPipe(name, nil)
	if err != nil {
		return nil, "", err
	}
	return ln, name, nil
}
