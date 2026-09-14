package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
)

// CreateOptions configures one backup.
type CreateOptions struct {
	DataDir string
	// Root is the backup root; empty means DefaultRoot(DataDir).
	Root string
	// Kind defaults to KindManual.
	Kind Kind
	Note string
	Tool ToolInfo
	// Progress, when set, is told each phase as it starts.
	Progress func(phase string)

	hooks *testHooks
}

// CreateResult describes a finalized backup.
type CreateResult struct {
	Path       string    `json:"path"`
	BackupID   string    `json:"backupId"`
	Kind       Kind      `json:"kind"`
	SizeBytes  int64     `json:"sizeBytes"`
	DurationMs int64     `json:"durationMs"`
	Manifest   *Manifest `json:"manifest"`
}

// Create takes a backup of dataDir into the backup root.
//
// It is safe with the daemon running: the database is captured by VACUUM INTO
// through a read-only connection. The backup appears under its final name only
// after every asset is fsynced, checked and listed in an fsynced manifest; until
// then it lives under .staging-<id>, which a failure or cancellation removes and
// a crash leaves behind visibly incomplete.
func Create(ctx context.Context, opts CreateOptions) (res *CreateResult, err error) {
	start := time.Now()
	if opts.Kind == "" {
		opts.Kind = KindManual
	}
	if !opts.Kind.valid() {
		return nil, refusedf(CodeInvalidArgument, "unknown backup kind %q", opts.Kind)
	}
	if strings.ContainsRune(opts.Note, 0) || len(opts.Note) > 1024 {
		return nil, refusedf(CodeInvalidArgument, "note must be at most 1024 bytes of text")
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}

	dataDir, err := resolveDataDir(opts.DataDir)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, DatabaseAsset)
	if fi, ok, err := present(dbPath); err != nil {
		return nil, err
	} else if !ok || !fi.Mode().IsRegular() {
		return nil, failedf(CodeAssetMissing, nil, "no database at %s", dbPath)
	}
	// A data dir an interrupted restore may have left mixed is not a state
	// anybody should be able to preserve as a "good" backup.
	if j, err := readJournal(dataDir); err != nil {
		return nil, refusedf(CodeRestoreInterrupted, "restore journal in %s cannot be read (%v); run `ao backup recover` first", dataDir, err)
	} else if j != nil && j.unsettled(dataDir) {
		return nil, refusedf(CodeRestoreInterrupted, "restore %s was interrupted in phase %s; run `ao backup recover` first", j.RestoreID, j.Phase)
	}
	root, err := resolveRoot(dataDir, opts.Root)
	if err != nil {
		return nil, err
	}
	head, err := opts.hooks.head()
	if err != nil {
		return nil, failedf(CodeIO, err, "read binary migration head")
	}
	id, err := NewBackupID(time.Now())
	if err != nil {
		return nil, failedf(CodeIO, err, "mint backup id")
	}

	defer func() {
		rec := OpRecord{At: time.Now(), Op: "create", BackupID: id, Kind: opts.Kind, DurationMs: time.Since(start).Milliseconds(), Result: "ok"}
		if err != nil {
			rec.Result = "failed"
			if e, ok := AsError(err); ok {
				rec.Reason = e.Code
			}
		} else {
			rec.SizeBytes, rec.GooseVersion = res.SizeBytes, res.Manifest.Schema.GooseVersion
		}
		appendOp(root, rec)
	}()

	lock, err := lockBackup(root, id)
	if err != nil {
		return nil, failedf(CodeIO, err, "lock backup %s", id)
	}
	staging := filepath.Join(root, stagingPrefix+id)
	if err := os.Mkdir(staging, 0o700); err != nil {
		_ = lock.Release()
		return nil, failedf(CodeIO, err, "create staging dir")
	}
	promoted := false
	defer func() {
		if !promoted {
			_ = os.RemoveAll(staging)
		}
		_ = lock.Release()
	}()
	fail := func(code Code, cause error, format string, args ...any) error {
		if ctx.Err() != nil {
			return &Error{Code: CodeCanceled, Class: ClassFailed, Msg: "backup canceled; nothing was kept", Err: ctx.Err()}
		}
		return failedf(code, cause, format, args...)
	}

	progress("snapshotting")
	stagedDB := filepath.Join(staging, DatabaseAsset)
	journalMode, err := snapshotDatabase(ctx, dbPath, stagedDB)
	if err != nil {
		return nil, fail(CodeSnapshotFailed, err, "snapshot %s", dbPath)
	}
	if err := os.Chmod(stagedDB, 0o600); err != nil {
		return nil, fail(CodeIO, err, "protect snapshot")
	}
	if err := syncFile(stagedDB); err != nil {
		return nil, fail(CodeIO, err, "fsync snapshot")
	}
	if h := opts.hooks; h != nil && h.afterSnapshot != nil {
		if err := h.afterSnapshot(staging); err != nil {
			return nil, fail(CodeIO, err, "after snapshot")
		}
	}

	progress("copying")
	var assets []Asset
	installationID := ""
	identSrc := filepath.Join(dataDir, IdentityAsset)
	if _, ok, err := present(identSrc); err != nil {
		return nil, fail(CodeUnsafePath, err, "inspect installation identity")
	} else if ok {
		identDst := filepath.Join(staging, IdentityAsset)
		n, sum, err := copyFileHashed(ctx, identSrc, identDst, 0o600)
		if err != nil {
			return nil, fail(CodeIO, err, "copy installation identity")
		}
		raw, err := os.ReadFile(identDst)
		if err != nil {
			return nil, fail(CodeIO, err, "read installation identity")
		}
		installationID = strings.TrimSpace(string(raw))
		if !daemonmeta.ValidInstallationID(installationID) {
			return nil, fail(CodeIO, nil, "installation identity in %s is malformed; refusing to back it up", dataDir)
		}
		assets = append(assets, Asset{Path: IdentityAsset, Role: RoleInstallationIdentity, Size: n, SHA256: sum, Mode: formatMode(0o600)})
	}
	skills, err := copySkillCatalog(ctx, dataDir, staging)
	if err != nil {
		return nil, fail(CodeIO, err, "copy skill catalog")
	}
	assets = append(assets, skills...)

	progress("hashing")
	dbSize, dbSum, err := hashFile(ctx, stagedDB)
	if err != nil {
		return nil, fail(CodeIO, err, "hash snapshot")
	}
	assets = append(assets, Asset{Path: DatabaseAsset, Role: RoleDatabase, Size: dbSize, SHA256: dbSum, Mode: formatMode(0o600)})

	progress("verifying")
	facts, err := inspectDatabase(ctx, stagedDB, false)
	if err != nil {
		return nil, fail(CodeDBUnreadable, err, "check snapshot")
	}
	if facts.Integrity != "ok" {
		return nil, fail(CodeIntegrityFailed, nil, "snapshot integrity_check: %s", facts.Integrity)
	}
	if facts.ForeignKeyViolations != 0 {
		return nil, fail(CodeForeignKeyViolations, nil, "snapshot has %d foreign-key violations", facts.ForeignKeyViolations)
	}

	secretFP, err := secretKeyFingerprint(dataDir)
	if err != nil {
		return nil, fail(CodeUnsafePath, err, "inspect secret key")
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })
	m := &Manifest{
		Format:    FormatV1,
		BackupID:  id,
		Kind:      opts.Kind,
		CreatedAt: time.Now().UTC(),
		Tool:      opts.Tool,
		Source: Source{
			DataDirFingerprint:   fingerprint("data-dir", []byte(dataDir)),
			InstallationID:       installationID,
			SecretKeyFingerprint: secretFP,
			JournalMode:          strings.ToLower(journalMode),
		},
		Schema:      Schema{GooseVersion: facts.GooseVersion, BinaryHead: head},
		Method:      MethodVacuumInto,
		Consistency: ConsistencySingleReadTxn,
		Checks:      Checks{IntegrityCheck: facts.Integrity, ForeignKeyViolations: facts.ForeignKeyViolations},
		Assets:      assets,
		Notes:       opts.Note,
	}
	if m.Tool.Name == "" {
		m.Tool.Name = "ao"
	}
	if findings := m.validate(); len(findings) > 0 {
		return nil, fail(CodeManifestInvalid, nil, "refusing to write an invalid manifest: %s", findings[0].Detail)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fail(CodeIO, err, "encode manifest")
	}
	if err := writeFileAtomic(filepath.Join(staging, ManifestName), append(data, '\n'), 0o600); err != nil {
		return nil, fail(CodeIO, err, "write manifest")
	}

	progress("finalizing")
	if err := syncTree(staging); err != nil {
		return nil, fail(CodeIO, err, "fsync staging")
	}
	if ctx.Err() != nil {
		return nil, fail(CodeCanceled, ctx.Err(), "canceled")
	}
	final := filepath.Join(root, id)
	if _, err := os.Lstat(final); err == nil {
		return nil, fail(CodeIO, nil, "%s already exists", final)
	}
	if err := opts.hooks.move(staging, final); err != nil {
		return nil, fail(CodeIO, err, "promote backup")
	}
	promoted = true
	if err := syncDir(root); err != nil {
		return nil, failedf(CodeIO, err, "fsync backup root (the backup at %s is complete but its directory entry may not be durable)", final)
	}

	var total int64
	for _, a := range assets {
		total += a.Size
	}
	return &CreateResult{Path: final, BackupID: id, Kind: opts.Kind, SizeBytes: total,
		DurationMs: time.Since(start).Milliseconds(), Manifest: m}, nil
}

// copySkillCatalog copies <data>/skills/catalog into the staging dir. A symlink
// or special file anywhere in it fails the backup: silently skipping one would
// be silent data loss, and following one could copy files from anywhere.
func copySkillCatalog(ctx context.Context, dataDir, staging string) ([]Asset, error) {
	if _, ok, err := present(filepath.Join(dataDir, "skills")); err != nil || !ok {
		return nil, err
	}
	src := filepath.Join(dataDir, filepath.FromSlash(SkillCatalogPath))
	fi, ok, err := present(src)
	if err != nil || !ok {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, &Error{Code: CodeUnsafePath, Class: ClassFailed, Msg: fmt.Sprintf("%s is not a directory", src)}
	}
	var assets []Asset
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(staging, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return &Error{Code: CodeUnsafePath, Class: ClassFailed, Msg: fmt.Sprintf("%s is a symlink; the skill catalog must contain only files", p)}
		case d.IsDir():
			return os.MkdirAll(dst, 0o700)
		case d.Type().IsRegular():
			slash := filepath.ToSlash(rel)
			if err := ValidateAssetPath(slash); err != nil {
				return &Error{Code: CodeUnsafePath, Class: ClassFailed, Msg: fmt.Sprintf("skill file %q: %v", slash, err)}
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := ownerOnly(info.Mode().Perm())
			n, sum, err := copyFileHashed(ctx, p, dst, mode)
			if err != nil {
				return err
			}
			assets = append(assets, Asset{Path: slash, Role: RoleSkillPackage, Size: n, SHA256: sum, Mode: formatMode(uint32(mode))})
			return nil
		default:
			return &Error{Code: CodeUnsafePath, Class: ClassFailed, Msg: fmt.Sprintf("%s is not a regular file", p)}
		}
	})
	return assets, err
}

// syncTree fsyncs every directory under dir (files were fsynced when written).
func syncTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return syncDir(p)
		}
		return nil
	})
}

// secretKeyFingerprint fingerprints <data>/secret.key without decoding it.
func secretKeyFingerprint(dataDir string) (string, error) {
	f, err := openRegular(filepath.Join(dataDir, secretKeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", err
	}
	return fingerprint("secret-key", []byte(strings.TrimSpace(string(raw)))), nil
}
