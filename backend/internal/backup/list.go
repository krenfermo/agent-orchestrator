package backup

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
)

// Entry states reported by List.
const (
	StateOK              = "ok"
	StateInProgress      = "in_progress"
	StateIncomplete      = "incomplete"
	StateDeleting        = "deleting"
	StateInvalidManifest = "invalid_manifest"
	StateUnsupported     = "unsupported"
)

// ListEntry is one backup-shaped entry of the root. It is read from the
// manifest alone: listing never hashes or opens a database.
type ListEntry struct {
	Name              string        `json:"name"`
	Path              string        `json:"path"`
	State             string        `json:"state"`
	BackupID          string        `json:"backupId,omitempty"`
	Kind              Kind          `json:"kind,omitempty"`
	CreatedAt         *time.Time    `json:"createdAt,omitempty"`
	SizeBytes         int64         `json:"sizeBytes,omitempty"`
	GooseVersion      int64         `json:"gooseVersion,omitempty"`
	Compatibility     Compatibility `json:"compatibility,omitempty"`
	IntegrityAtCreate string        `json:"integrityAtCreate,omitempty"`
	Note              string        `json:"note,omitempty"`
	Detail            string        `json:"detail,omitempty"`
}

// ListReport is the backup root's contents plus the operations log's latest.
type ListReport struct {
	Root        string      `json:"root"`
	BinaryHead  int64       `json:"binaryHead"`
	Entries     []ListEntry `json:"entries"`
	LastBackup  *OpRecord   `json:"lastBackup,omitempty"`
	LastRestore *OpRecord   `json:"lastRestore,omitempty"`
}

// List reports the AO backups under root (DefaultRoot(dataDir) when empty).
// Entries that are not AO backups -- such as hand-made copies -- are not
// listed and are never touched. A missing root is an empty list.
func List(dataDir, root string) (*ListReport, error) {
	if root == "" {
		root = DefaultRoot(dataDir)
	}
	resolved, err := resolveExisting(root)
	if err != nil {
		return nil, err
	}
	head, err := (*testHooks)(nil).head()
	if err != nil {
		return nil, err
	}
	rep := &ListReport{Root: resolved, BinaryHead: head, Entries: []ListEntry{}}
	dirents, err := os.ReadDir(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return nil, err
	}
	for _, d := range dirents {
		name := d.Name()
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		e := ListEntry{Name: name, Path: filepath.Join(resolved, name)}
		switch {
		case strings.HasPrefix(name, stagingPrefix) && ValidBackupID(strings.TrimPrefix(name, stagingPrefix)):
			e.BackupID = strings.TrimPrefix(name, stagingPrefix)
			e.State = StateIncomplete
			e.Detail = "a create did not finish; `ao backup prune --apply` removes it"
			if lockHeld(resolved, e.BackupID) {
				e.State, e.Detail = StateInProgress, "a create is running"
			}
		case strings.HasPrefix(name, deletingPrefix) && ValidBackupID(strings.TrimPrefix(name, deletingPrefix)):
			e.BackupID = strings.TrimPrefix(name, deletingPrefix)
			e.State, e.Detail = StateDeleting, "a prune did not finish; `ao backup prune --apply` removes it"
		case ValidBackupID(name):
			e.BackupID = name
			readListManifest(&e, head)
		default:
			continue
		}
		rep.Entries = append(rep.Entries, e)
	}
	sort.Slice(rep.Entries, func(i, j int) bool { return rep.Entries[i].BackupID > rep.Entries[j].BackupID })
	for _, op := range readOps(resolved) {
		switch {
		case op.Op == "create" && op.Result == "ok":
			rec := op
			rep.LastBackup = &rec
		case op.Op == "restore":
			rec := op
			rep.LastRestore = &rec
		}
	}
	return rep, nil
}

func readListManifest(e *ListEntry, head int64) {
	f, err := openRegular(filepath.Join(e.Path, ManifestName))
	if err != nil {
		e.State, e.Detail = StateInvalidManifest, "manifest missing or unreadable"
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	_ = f.Close()
	if err != nil {
		e.State, e.Detail = StateInvalidManifest, "manifest unreadable"
		return
	}
	m, status, findings := ParseManifest(data)
	switch {
	case status == StatusUnsupported:
		e.State = StateUnsupported
	case status != StatusValid || m.BackupID != e.Name:
		e.State = StateInvalidManifest
		if len(findings) > 0 {
			e.Detail = findings[0].Detail
		} else {
			e.Detail = "manifest backupId does not match the directory name"
		}
		return
	default:
		e.State = StateOK
	}
	if len(findings) > 0 && e.Detail == "" {
		e.Detail = findings[0].Detail
	}
	if m == nil {
		return
	}
	created := m.CreatedAt
	e.Kind, e.CreatedAt, e.GooseVersion, e.Note = m.Kind, &created, m.Schema.GooseVersion, m.Notes
	e.Compatibility = compatibility(m.Schema.GooseVersion, head)
	e.IntegrityAtCreate = m.Checks.IntegrityCheck
	for _, a := range m.Assets {
		e.SizeBytes += a.Size
	}
}

// lockHeld reports whether a live process holds a backup's lock. A missing lock
// file is not held: creators remove theirs only when they finish.
func lockHeld(root, id string) bool {
	p := lockPath(root, id)
	if _, err := os.Lstat(p); err != nil {
		return false
	}
	l, err := daemonlock.Acquire(p)
	if errors.Is(err, daemonlock.ErrHeld) {
		return true
	}
	if err != nil {
		// Unknown is treated as held: never delete what might be in use.
		return true
	}
	_ = l.Release()
	return false
}
