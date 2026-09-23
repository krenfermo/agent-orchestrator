//go:build !unix

package skillagent

import (
	"context"
	"errors"
	"os/exec"
)

func supportedOS() bool { return false }

func runGroup(context.Context, *exec.Cmd, string) ([]byte, error) {
	return nil, errors.New("skillagent: host agent execution is not implemented on this OS")
}

func reapPID(string, string) bool { return false }
