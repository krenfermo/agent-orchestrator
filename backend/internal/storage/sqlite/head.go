package sqlite

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// MigrationHead is the highest goose migration version embedded in this
// binary: the newest schema it understands. A database at a higher version was
// written by a newer AO, and this binary must neither open nor restore it (P10).
func MigrationHead() (int64, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var head int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			continue
		}
		head = max(head, v)
	}
	if head == 0 {
		return 0, errors.New("no embedded migrations")
	}
	return head, nil
}
