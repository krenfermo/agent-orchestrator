package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
)

// DefaultKeepLast is how many manual backups retention keeps by default.
const DefaultKeepLast = 10

// Prune actions.
const (
	ActionKeep             = "keep"
	ActionDelete           = "delete"
	ActionRemoveIncomplete = "remove_incomplete"
	ActionSkip             = "skip"
)

// PruneOptions configures retention. Dry run unless Apply.
type PruneOptions struct {
	DataDir string
	Root    string
	// KeepLast manual backups are always kept (default DefaultKeepLast, min 1).
	KeepLast int
	// MaxAge, when positive, additionally keeps every manual backup younger
	// than it: a backup is deleted only when it is BOTH outside the newest
	// KeepLast AND older than MaxAge.
	MaxAge time.Duration
	Apply  bool
	Now    func() time.Time
}

// PruneDecision is what retention decided about one entry and why.
type PruneDecision struct {
	Name      string     `json:"name"`
	BackupID  string     `json:"backupId"`
	Kind      Kind       `json:"kind,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	Action    string     `json:"action"`
	Reason    string     `json:"reason"`
	Done      bool       `json:"done"`
	Error     string     `json:"error,omitempty"`
}

// PruneReport lists every decision.
type PruneReport struct {
	Root       string          `json:"root"`
	DryRun     bool            `json:"dryRun"`
	KeepLast   int             `json:"keepLast"`
	MaxAge     string          `json:"maxAge,omitempty"`
	Decisions  []PruneDecision `json:"decisions"`
	Deleted    int             `json:"deleted"`
	LocksSwept int             `json:"locksSwept"`
	Errors     int             `json:"errors"`
}

// Prune applies conservative retention to the backup root.
//
// It only ever deletes finalized manual AO backups with a readable manifest,
// and incomplete leftovers whose owner is provably gone. It never deletes a
// pre-restore or pre-migration backup, the newest backup, a backup whose lock
// is held (in use by a restore), an entry whose manifest it cannot read, or
// anything that is not an AO backup. A deletion first renames the backup to
// .deleting-<id>, so an interrupted prune never leaves a partial backup that
// looks valid.
func Prune(ctx context.Context, opts PruneOptions) (*PruneReport, error) {
	if opts.KeepLast == 0 {
		opts.KeepLast = DefaultKeepLast
	}
	if opts.KeepLast < 1 {
		return nil, refusedf(CodeInvalidArgument, "--keep must be at least 1")
	}
	if opts.MaxAge < 0 {
		return nil, refusedf(CodeInvalidArgument, "--max-age must not be negative")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	root := opts.Root
	if root == "" {
		root = DefaultRoot(opts.DataDir)
	}
	resolved, err := resolveExisting(root)
	if err != nil {
		return nil, err
	}
	rep := &PruneReport{Root: resolved, DryRun: !opts.Apply, KeepLast: opts.KeepLast, Decisions: []PruneDecision{}}
	if opts.MaxAge > 0 {
		rep.MaxAge = opts.MaxAge.String()
	}
	dirents, err := os.ReadDir(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return nil, err
	}

	type finalized struct {
		name string
		m    *Manifest
	}
	var backups []finalized
	for _, d := range dirents {
		name := d.Name()
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		switch {
		case strings.HasPrefix(name, stagingPrefix) && ValidBackupID(strings.TrimPrefix(name, stagingPrefix)):
			id := strings.TrimPrefix(name, stagingPrefix)
			if lockHeld(resolved, id) {
				rep.Decisions = append(rep.Decisions, PruneDecision{Name: name, BackupID: id, Action: ActionSkip, Reason: "a create is still running"})
			} else {
				rep.Decisions = append(rep.Decisions, PruneDecision{Name: name, BackupID: id, Action: ActionRemoveIncomplete, Reason: "orphaned staging: its creator is gone"})
			}
		case strings.HasPrefix(name, deletingPrefix) && ValidBackupID(strings.TrimPrefix(name, deletingPrefix)):
			rep.Decisions = append(rep.Decisions, PruneDecision{Name: name, BackupID: strings.TrimPrefix(name, deletingPrefix),
				Action: ActionRemoveIncomplete, Reason: "an earlier prune was interrupted"})
		case ValidBackupID(name):
			m, err := readManifestForPrune(filepath.Join(resolved, name))
			if err != nil || m.BackupID != name {
				rep.Decisions = append(rep.Decisions, PruneDecision{Name: name, BackupID: name, Action: ActionKeep,
					Reason: "manifest missing, unreadable or mismatched: never pruned automatically"})
				continue
			}
			backups = append(backups, finalized{name: name, m: m})
		}
	}

	sort.Slice(backups, func(i, j int) bool {
		if !backups[i].m.CreatedAt.Equal(backups[j].m.CreatedAt) {
			return backups[i].m.CreatedAt.After(backups[j].m.CreatedAt)
		}
		return backups[i].name > backups[j].name
	})
	manualSeen := 0
	for i, b := range backups {
		created := b.m.CreatedAt
		dec := PruneDecision{Name: b.name, BackupID: b.name, Kind: b.m.Kind, CreatedAt: &created, Action: ActionKeep}
		switch {
		case b.m.Kind.Protected():
			dec.Reason = fmt.Sprintf("%s backups are never pruned", b.m.Kind)
		case i == 0:
			dec.Reason = "newest backup"
			manualSeen++
		case manualSeen < opts.KeepLast:
			dec.Reason = fmt.Sprintf("within the newest %d manual backups", opts.KeepLast)
			manualSeen++
		case opts.MaxAge > 0 && now().Sub(created) < opts.MaxAge:
			dec.Reason = "younger than --max-age"
			manualSeen++
		default:
			dec.Action = ActionDelete
			dec.Reason = fmt.Sprintf("outside the newest %d manual backups", opts.KeepLast)
			if opts.MaxAge > 0 {
				dec.Reason += " and older than --max-age"
			}
			manualSeen++
		}
		rep.Decisions = append(rep.Decisions, dec)
	}

	if !opts.Apply {
		return rep, nil
	}
	for i := range rep.Decisions {
		dec := &rep.Decisions[i]
		if dec.Action != ActionDelete && dec.Action != ActionRemoveIncomplete {
			continue
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		err := pruneOne(resolved, dec)
		switch {
		case errors.Is(err, daemonlock.ErrHeld):
			dec.Action, dec.Reason = ActionSkip, "in use by another operation"
		case err != nil:
			dec.Error = err.Error()
			rep.Errors++
		default:
			dec.Done = true
			if dec.Action == ActionDelete {
				rep.Deleted++
			}
		}
	}
	rep.LocksSwept = sweepOrphanLocks(resolved)
	appendOp(resolved, OpRecord{At: now(), Op: "prune", Result: fmt.Sprintf("deleted=%d errors=%d", rep.Deleted, rep.Errors)})
	return rep, nil
}

// sweepOrphanLocks removes lock files whose backup no longer exists in any form
// (finalized, staging or being deleted) and that nobody holds. Removing those is
// safe: no future operation can create a backup with the same random id.
func sweepOrphanLocks(root string) int {
	entries, err := os.ReadDir(filepath.Join(root, locksDir))
	if err != nil {
		return 0
	}
	swept := 0
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".lock")
		if !ok || !ValidBackupID(id) || !e.Type().IsRegular() {
			continue
		}
		exists := false
		for _, name := range []string{id, stagingPrefix + id, deletingPrefix + id} {
			if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
				exists = true
			}
		}
		if exists {
			continue
		}
		lock, err := daemonlock.Acquire(lockPath(root, id))
		if err != nil {
			continue
		}
		if os.Remove(lockPath(root, id)) == nil {
			swept++
		}
		_ = lock.Release()
	}
	return swept
}

func pruneOne(root string, dec *PruneDecision) error {
	target := filepath.Join(root, dec.Name)
	if strings.HasPrefix(dec.Name, deletingPrefix) {
		return os.RemoveAll(target)
	}
	if err := os.MkdirAll(filepath.Join(root, locksDir), 0o700); err != nil {
		return err
	}
	lock, err := daemonlock.Acquire(lockPath(root, dec.BackupID))
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(lockPath(root, dec.BackupID))
		_ = lock.Release()
	}()
	if dec.Action == ActionRemoveIncomplete {
		return os.RemoveAll(target)
	}
	deleting := filepath.Join(root, deletingPrefix+dec.BackupID)
	if err := os.Rename(target, deleting); err != nil {
		return err
	}
	if err := syncDir(root); err != nil {
		return err
	}
	return os.RemoveAll(deleting)
}

func readManifestForPrune(dir string) (*Manifest, error) {
	f, err := openRegular(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	m, status, findings := ParseManifest(data)
	if status != StatusValid {
		if len(findings) > 0 {
			return nil, errors.New(findings[0].Detail)
		}
		return nil, errors.New("invalid manifest")
	}
	return m, nil
}
