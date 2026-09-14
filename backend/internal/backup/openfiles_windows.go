//go:build windows

package backup

// openHolders has nothing to find on Windows: SQLite opens its files without
// FILE_SHARE_DELETE, so renaming a database another process has open fails and
// the swap rolls back on its own.
func openHolders([]string) ([]int, error) { return nil, nil }
