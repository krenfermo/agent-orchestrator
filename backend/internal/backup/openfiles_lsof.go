//go:build !linux && !windows

package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// openHolders asks lsof which other processes have any of paths open. lsof
// matches files by device and inode, so a descriptor on a file that has since
// been renamed is still found under its new name.
func openHolders(paths []string) ([]int, error) {
	bin, err := exec.LookPath("lsof")
	if err != nil {
		for _, c := range []string{"/usr/sbin/lsof", "/usr/bin/lsof"} {
			if _, serr := os.Stat(c); serr == nil {
				bin, err = c, nil
				break
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("lsof is needed to see open database files: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, append([]string{"-t", "-w", "--"}, paths...)...) //nolint:gosec // lsof from PATH or a fixed system path; paths are arguments, never a shell
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	pids, perr := parsePIDs(stdout.String())
	var exit *exec.ExitError
	switch {
	case perr != nil:
		return nil, fmt.Errorf("lsof: %w", perr)
	case runErr == nil:
		return pids, nil
	case errors.As(runErr, &exit) && exit.ExitCode() == 1 && stdout.Len() == 0 && strings.TrimSpace(stderr.String()) == "":
		// lsof exits 1, silently, when no process has any of the files open.
		return nil, nil
	default:
		return nil, fmt.Errorf("lsof: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
}

// parsePIDs reads lsof -t output, one pid per line, without this process.
func parsePIDs(out string) ([]int, error) {
	self := os.Getpid()
	var pids []int
	for _, field := range strings.Fields(out) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("unexpected output %q", field)
		}
		if pid != self && !slices.Contains(pids, pid) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
