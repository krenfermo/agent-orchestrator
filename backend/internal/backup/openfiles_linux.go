//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// openHolders scans /proc for other processes' descriptors on any of paths,
// matched by device and inode. A process whose descriptors this user cannot
// list belongs to another user, who cannot open AO's 0600 database anyway.
func openHolders(paths []string) ([]int, error) {
	var targets []os.FileInfo
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			targets = append(targets, fi)
		}
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var pids []int
	for _, d := range procs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid == self {
			continue
		}
		fdDir := filepath.Join("/proc", d.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			fi, err := os.Stat(filepath.Join(fdDir, fd.Name()))
			if err == nil && slices.ContainsFunc(targets, func(t os.FileInfo) bool { return os.SameFile(t, fi) }) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}
