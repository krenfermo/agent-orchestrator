//go:build unix

package skillrunner

import (
	"os"
	"syscall"
)

// hardLinkCount returns the number of names pointing at this inode. A file with
// more than one is refused as an artifact: its bytes can be changed through a
// name AO did not collect.
func hardLinkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// If the platform will not say, treat it as a single link rather than
		// rejecting every file — the other checks still apply.
		return 1
	}
	return uint64(stat.Nlink)
}
