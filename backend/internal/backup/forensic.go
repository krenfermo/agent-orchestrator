package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	msqlite "modernc.org/sqlite"
)

// Rollback kinds a restore reports.
const (
	// RollbackVerifiedBackup is a VALID pre-restore backup, restorable as any other.
	RollbackVerifiedBackup = "verified_backup"
	// RollbackForensicCopy is a byte-for-byte copy of a damaged data dir's
	// managed entries: evidence, not a backup (see forensicCopy).
	RollbackForensicCopy = "forensic_copy"
)

// Forensic copies.
const (
	ForensicManifestName = "forensic.json"
	forensicFormat       = "ao.forensic-copy/v1"
	forensicPrefix       = "aof-"
)

type forensicFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type forensicManifest struct {
	Format    string         `json:"format"`
	ID        string         `json:"id"`
	RestoreID string         `json:"restoreId"`
	CreatedAt time.Time      `json:"createdAt"`
	Reason    string         `json:"reason"`
	Files     []forensicFile `json:"files"`
}

// isDatabaseDamage reports whether err is SQLite saying a file is damaged or is
// not a database at all (SQLITE_CORRUPT, SQLITE_NOTADB) -- as opposed to busy,
// out of space, or any other I/O problem.
func isDatabaseDamage(err error) bool {
	var se *msqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case 11, 26: // SQLITE_CORRUPT, SQLITE_NOTADB
			return true
		}
	}
	return false
}

// destinationDamaged reports whether a pre-restore backup failed because the
// current database is damaged, rather than for lack of space, an I/O error or
// cancellation -- which --preserve-broken-state must never paper over.
func destinationDamaged(err error) bool {
	e, ok := AsError(err)
	if !ok {
		return false
	}
	switch e.Code {
	case CodeIntegrityFailed, CodeForeignKeyViolations:
		return true
	case CodeSnapshotFailed, CodeDBUnreadable:
		return isDatabaseDamage(e.Err)
	}
	return false
}

// probeQuiet is ProbeExclusive for a database that may be damaged. SQLite
// cannot probe a file it cannot parse; for such a file "nobody has it open" is
// proven by the operating system instead, and the damage is returned for the
// caller to judge.
func probeQuiet(dbPath string, h *testHooks) (damage, err error) {
	perr := ProbeExclusive(dbPath)
	switch {
	case perr == nil:
		return nil, nil
	case !isDatabaseDamage(perr):
		return nil, perr
	}
	if herr := refuseOpenHolders(h, "the damaged database", dbFamily(filepath.Dir(dbPath))); herr != nil {
		return nil, herr
	}
	return perr, nil
}

// forensicCopy preserves a damaged data dir's managed entries -- ao.db and its
// sidecars as they are, the installation identity and the skill catalog --
// byte for byte in <root>/aof-<id>, so a restore over a database too damaged to
// produce a VALID pre-restore backup still destroys nothing.
//
// It is evidence, not a backup: nothing proves it is a working AO state, verify
// calls it INVALID, and retention never lists or deletes it. Every file is
// copied and fsynced, then both the copy and its source are hashed again and
// compared before the copy appears under its final name. Returns its path.
func forensicCopy(ctx context.Context, dataDir, root, restoreID string) (string, error) {
	id, err := newID(forensicPrefix, time.Now())
	if err != nil {
		return "", err
	}
	staging := filepath.Join(root, stagingPrefix+id)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return "", err
	}
	promoted := false
	defer func() {
		if !promoted {
			_ = os.RemoveAll(staging)
		}
	}()

	var files []forensicFile
	for _, name := range append([]string{DatabaseAsset, IdentityAsset}, sqliteSidecars...) {
		src := filepath.Join(dataDir, name)
		fi, ok, err := present(src)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file", name)
		}
		n, sum, err := copyFileHashed(ctx, src, filepath.Join(staging, name), 0o600)
		if err != nil {
			return "", fmt.Errorf("copy %s: %w", name, err)
		}
		files = append(files, forensicFile{Path: name, Size: n, SHA256: sum})
	}
	skills, err := copySkillCatalog(ctx, dataDir, staging)
	if err != nil {
		return "", fmt.Errorf("copy skill catalog: %w", err)
	}
	for _, a := range skills {
		files = append(files, forensicFile{Path: a.Path, Size: a.Size, SHA256: a.SHA256})
	}
	// The one claim this copy makes is that its bytes are the source's: read
	// both back.
	for _, f := range files {
		srcN, srcSum, err := hashFile(ctx, filepath.Join(dataDir, filepath.FromSlash(f.Path)))
		if err != nil {
			return "", err
		}
		n, sum, err := hashFile(ctx, filepath.Join(staging, filepath.FromSlash(f.Path)))
		if err != nil {
			return "", err
		}
		if n != f.Size || sum != f.SHA256 || srcN != n || srcSum != sum {
			return "", fmt.Errorf("the copy of %s does not match its source", f.Path)
		}
	}

	m := forensicManifest{Format: forensicFormat, ID: id, RestoreID: restoreID, CreatedAt: time.Now().UTC(),
		Reason: "the database could not produce a VALID pre-restore backup", Files: files}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(staging, ForensicManifestName), append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := syncTree(staging); err != nil {
		return "", err
	}
	final := filepath.Join(root, id)
	if err := os.Rename(staging, final); err != nil {
		return "", err
	}
	promoted = true
	if err := syncDir(root); err != nil {
		return "", fmt.Errorf("fsync backup root (the forensic copy at %s is complete but its entry may not be durable): %w", final, err)
	}
	return final, nil
}
