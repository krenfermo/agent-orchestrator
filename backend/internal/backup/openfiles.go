package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// dbFamily names a database file in dir and the sidecars SQLite keeps beside it.
func dbFamily(dir string) []string {
	out := []string{filepath.Join(dir, DatabaseAsset)}
	for _, s := range sqliteSidecars {
		out = append(out, filepath.Join(dir, s))
	}
	return out
}

// refuseOpenHolders refuses while any other process has one of paths open.
//
// SQLite's exclusive probe only sees connections that hold a lock, and a
// connection holds none until its first read: a sqlite3 shell opened and left
// idle passes it. That process keeps a descriptor on the file it opened but
// derives its -wal and -shm from the file's NAME, so once the swap has put the
// restored database under that name, its first write lands in a WAL that
// attaches to the restored database and corrupts it. The operating system knows
// about the descriptor; this asks it. Not being able to ask refuses.
func refuseOpenHolders(h *testHooks, what string, paths []string) error {
	var existing []string
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	pids, err := h.holders(existing)
	if err != nil {
		return &Error{Code: CodeDBInUse, Class: ClassRefused,
			Msg: fmt.Sprintf("cannot prove that no process has %s open", what), Err: err}
	}
	if len(pids) > 0 {
		ids := make([]string, len(pids))
		for i, pid := range pids {
			ids[i] = strconv.Itoa(pid)
		}
		return refusedf(CodeDBInUse, "process %s has %s open (a sqlite3 shell or a database browser?); close it before restoring",
			strings.Join(ids, ", "), what)
	}
	return nil
}
