//go:build windows

package backup

import "errors"

// syncDir is a no-op: Windows cannot fsync a directory handle. Backup/restore
// on Windows compiles but is not a supported, proven target (see the P10 doc).
func syncDir(string) error { return nil }

func freeSpace(string) (uint64, error) {
	return 0, errors.New("free space is not measured on windows")
}
