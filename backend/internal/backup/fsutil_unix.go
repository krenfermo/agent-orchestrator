//go:build !windows

package backup

import (
	"os"
	"syscall"
)

// syncDir makes new, renamed or removed directory entries durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return serr
	}
	return cerr
}

// freeSpace reports the bytes available to an unprivileged writer on the
// filesystem holding path.
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // a filesystem block size is never negative
}
