package backup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
)

const (
	stagingPrefix     = ".staging-"
	deletingPrefix    = ".deleting-"
	locksDir          = ".locks"
	opsLogName        = "operations.jsonl"
	journalName       = ".ao-restore-journal.json"
	restoreWorkPrefix = ".ao-restore-"
	freeSpaceMargin   = 64 << 20
	maxOpsLogTail     = 1 << 20
)

// DefaultRoot is the backup root for a data dir: <parent>/backups, which is
// ~/.ao/backups for the default ~/.ao/data. It stays under the AO home and
// outside the data dir it protects.
func DefaultRoot(dataDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(dataDir)), "backups")
}

// resolveExisting canonicalizes p through its nearest existing ancestor, so a
// path can be checked for containment before anything is created.
func resolveExisting(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var rest []string
	cur := abs
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// within reports whether child is parent or lives under it.
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// sqlitePathSafe rejects paths SQLite's URI parser would misread.
func sqlitePathSafe(p string) error {
	if strings.ContainsAny(p, "?#%") {
		return &Error{Code: CodeUnsafePath, Class: ClassRefused,
			Msg: fmt.Sprintf("%s contains ?, # or %%, which SQLite would read as URI syntax", p)}
	}
	return nil
}

// resolveDataDir canonicalizes an existing data dir.
func resolveDataDir(dataDir string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", refusedf(CodeInvalidArgument, "no data dir given")
	}
	dd, err := resolveExisting(dataDir)
	if err != nil {
		return "", err
	}
	if err := sqlitePathSafe(dd); err != nil {
		return "", err
	}
	return dd, nil
}

// resolveRoot canonicalizes the backup root, refusing one that is, or lives
// inside, the data dir (a restore replaces the data dir) or that is a file,
// and creates it 0700 only after those checks.
func resolveRoot(dataDir, root string) (string, error) {
	if root == "" {
		root = DefaultRoot(dataDir)
	}
	candidate, err := resolveExisting(root)
	if err != nil {
		return "", err
	}
	if within(candidate, dataDir) {
		return "", refusedf(CodeInvalidBackupRoot, "backup root %s is inside the data dir %s it would protect", candidate, dataDir)
	}
	if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
		return "", refusedf(CodeInvalidBackupRoot, "backup root %s is not a directory", candidate)
	}
	if err := sqlitePathSafe(candidate); err != nil {
		return "", err
	}
	if err := os.MkdirAll(candidate, 0o700); err != nil {
		return "", failedf(CodeIO, err, "create backup root %s", candidate)
	}
	return filepath.EvalSymlinks(candidate)
}

func lockPath(root, id string) string { return filepath.Join(root, locksDir, id+".lock") }

// lockBackup takes a backup's lock in its own root, creating the lock file if
// needed. A holder never removes its lock file: unlinking it while another
// process has it open would let two processes both "hold" the same backup.
// Only prune removes lock files, and only for backups that no longer exist.
func lockBackup(root, id string) (*daemonlock.Lock, error) {
	if err := os.MkdirAll(filepath.Join(root, locksDir), 0o700); err != nil {
		return nil, err
	}
	return daemonlock.Acquire(lockPath(root, id))
}

// OpRecord is one line of <root>/operations.jsonl: metadata only, never paths
// beyond ids, never secrets.
type OpRecord struct {
	At               time.Time `json:"at"`
	Op               string    `json:"op"`
	BackupID         string    `json:"backupId,omitempty"`
	Kind             Kind      `json:"kind,omitempty"`
	Result           string    `json:"result"`
	Reason           Code      `json:"reason,omitempty"`
	DurationMs       int64     `json:"durationMs"`
	SizeBytes        int64     `json:"sizeBytes,omitempty"`
	GooseVersion     int64     `json:"gooseVersion,omitempty"`
	RollbackBackupID string    `json:"rollbackBackupId,omitempty"`
}

// appendOp records an operation. Best effort: the log is a convenience for
// `ao backup list`, never an input to a safety decision.
func appendOp(root string, rec OpRecord) {
	if root == "" {
		return
	}
	p := filepath.Join(root, opsLogName)
	if fi, err := os.Lstat(p); err == nil && !fi.Mode().IsRegular() {
		return
	}
	rec.At = rec.At.UTC()
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// readOps returns the recorded operations from the log's tail, oldest first.
func readOps(root string) []OpRecord {
	f, err := openRegular(filepath.Join(root, opsLogName))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > maxOpsLogTail {
		if _, err := f.Seek(-maxOpsLogTail, io.SeekEnd); err != nil {
			return nil
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	var out []OpRecord
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxOpsLogTail)
	for sc.Scan() {
		var rec OpRecord
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Op != "" {
			out = append(out, rec)
		}
	}
	return out
}
