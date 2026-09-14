package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Compatibility is how a backup's schema relates to this binary's.
type Compatibility string

const (
	CompatCompatible      Compatibility = "compatible"
	CompatUpgradeRequired Compatibility = "upgrade_required"
	CompatNewerThanBinary Compatibility = "newer_than_binary"
	CompatUnknown         Compatibility = "unknown"
)

func compatibility(goose, head int64) Compatibility {
	switch {
	case goose <= 0 || head <= 0:
		return CompatUnknown
	case goose == head:
		return CompatCompatible
	case goose < head:
		return CompatUpgradeRequired
	default:
		return CompatNewerThanBinary
	}
}

// VerifyOptions configures verification.
type VerifyOptions struct {
	// Quick runs quick_check instead of the full integrity_check.
	Quick    bool
	Progress func(phase string)

	hooks *testHooks
}

// VerifyReport is a verdict with its reasons.
type VerifyReport struct {
	Path          string        `json:"path"`
	BackupID      string        `json:"backupId,omitempty"`
	Kind          Kind          `json:"kind,omitempty"`
	CreatedAt     *time.Time    `json:"createdAt,omitempty"`
	Status        Status        `json:"status"`
	Compatibility Compatibility `json:"compatibility"`
	GooseVersion  int64         `json:"gooseVersion,omitempty"`
	BinaryHead    int64         `json:"binaryHead"`
	CheckMode     string        `json:"checkMode"`
	Integrity     string        `json:"integrity,omitempty"`
	SizeBytes     int64         `json:"sizeBytes,omitempty"`
	Reasons       []Finding     `json:"reasons"`

	manifest *Manifest
	dir      string
}

// Restorable reports whether this binary may restore the backup.
func (r *VerifyReport) Restorable() bool {
	return r.Status == StatusValid && r.Compatibility != CompatNewerThanBinary && r.Compatibility != CompatUnknown
}

// Verify checks a backup directory and never writes anything: not to the
// backup (the database is opened immutable), not beside it, not to a log.
//
// The returned error is reserved for cancellation and failures to run the
// checks at all; everything wrong with the backup itself is a finding.
func Verify(ctx context.Context, backupPath string, opts VerifyOptions) (*VerifyReport, error) {
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	head, err := opts.hooks.head()
	if err != nil {
		return nil, fmt.Errorf("read binary migration head: %w", err)
	}
	rep := &VerifyReport{Path: backupPath, BinaryHead: head, Compatibility: CompatUnknown, CheckMode: "integrity_check", Reasons: []Finding{}}
	if opts.Quick {
		rep.CheckMode = "quick_check"
	}
	add := func(code Code, format string, args ...any) {
		rep.Reasons = append(rep.Reasons, Finding{Code: code, Detail: fmt.Sprintf(format, args...)})
	}
	finish := func() (*VerifyReport, error) {
		if rep.Status == "" {
			rep.Status = StatusValid
			if len(rep.Reasons) > 0 {
				rep.Status = StatusInvalid
			}
		}
		if rep.Status == StatusValid && rep.Compatibility == CompatNewerThanBinary {
			add(CodeNewerThanBinary, "backup schema %d is newer than this binary's head %d; this binary cannot use it", rep.GooseVersion, head)
		}
		return rep, nil
	}

	abs, err := filepath.Abs(backupPath)
	if err != nil {
		return nil, err
	}
	rep.Path = abs
	li, err := os.Lstat(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		add(CodeManifestMissing, "backup directory %s does not exist", abs)
		return finish()
	case err != nil:
		return nil, err
	case li.Mode()&os.ModeSymlink != 0:
		add(CodeSymlinkRejected, "%s is a symlink; pass the backup directory itself", abs)
		return finish()
	case !li.IsDir():
		add(CodeManifestMissing, "%s is not a backup directory", abs)
		return finish()
	}
	base := filepath.Base(abs)
	if strings.HasPrefix(base, stagingPrefix) || strings.HasPrefix(base, deletingPrefix) {
		add(CodeStagingIncomplete, "%s is an incomplete backup (a create or prune did not finish)", base)
		return finish()
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(parent, base)
	rep.dir = dir
	if err := sqlitePathSafe(dir); err != nil {
		add(CodeUnsafePath, "%v", err)
		return finish()
	}

	progress("reading manifest")
	f, err := openRegular(filepath.Join(dir, ManifestName))
	if errors.Is(err, os.ErrNotExist) {
		add(CodeManifestMissing, "%s has no %s", base, ManifestName)
		return finish()
	}
	if err != nil {
		add(CodeSymlinkRejected, "manifest: %v", err)
		return finish()
	}
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	_ = f.Close()
	if err != nil {
		add(CodeManifestInvalid, "read manifest: %v", err)
		return finish()
	}
	m, status, findings := ParseManifest(data)
	if m != nil {
		rep.BackupID, rep.Kind, rep.GooseVersion = m.BackupID, m.Kind, m.Schema.GooseVersion
		created := m.CreatedAt
		rep.CreatedAt = &created
	}
	if status != StatusValid {
		rep.Reasons = append(rep.Reasons, findings...)
		rep.Status = status
		return finish()
	}
	rep.manifest = m
	rep.Compatibility = compatibility(m.Schema.GooseVersion, head)

	progress("scanning")
	scanBackupTree(dir, m, add)

	progress("hashing")
	dbIntact := false
	for _, a := range m.Assets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := filepath.Join(dir, filepath.FromSlash(a.Path))
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			add(CodeAssetMissing, "%s is listed in the manifest but missing", a.Path)
			continue
		case err != nil:
			add(CodeIO, "%s: %v", a.Path, err)
			continue
		case fi.Mode()&os.ModeSymlink != 0:
			add(CodeSymlinkRejected, "%s is a symlink", a.Path)
			continue
		case !fi.Mode().IsRegular():
			add(CodeAssetUnexpected, "%s is not a regular file", a.Path)
			continue
		case fi.Size() != a.Size:
			add(CodeSizeMismatch, "%s is %d bytes, manifest says %d", a.Path, fi.Size(), a.Size)
			continue
		}
		_, sum, err := hashFile(ctx, p)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			add(CodeIO, "hash %s: %v", a.Path, err)
			continue
		}
		if sum != a.SHA256 {
			add(CodeHashMismatch, "%s does not match its manifest checksum", a.Path)
			continue
		}
		rep.SizeBytes += a.Size
		switch a.Role {
		case RoleDatabase:
			dbIntact = true
		case RoleInstallationIdentity:
			raw, err := os.ReadFile(p)
			if err != nil || strings.TrimSpace(string(raw)) != m.Source.InstallationID {
				add(CodeManifestInvalid, "installation identity file does not match the manifest")
			}
		}
	}

	if dbIntact {
		progress(rep.CheckMode)
		facts, err := inspectDatabase(ctx, filepath.Join(dir, DatabaseAsset), opts.Quick)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			add(CodeDBUnreadable, "database: %v", err)
		default:
			rep.Integrity = facts.Integrity
			if facts.Integrity != "ok" {
				add(CodeIntegrityFailed, "%s: %s", rep.CheckMode, facts.Integrity)
			}
			if facts.ForeignKeyViolations != 0 {
				add(CodeForeignKeyViolations, "%d foreign-key violations", facts.ForeignKeyViolations)
			}
			if facts.GooseVersion != m.Schema.GooseVersion {
				add(CodeSchemaMismatch, "database is at goose %d, manifest says %d", facts.GooseVersion, m.Schema.GooseVersion)
			}
		}
	}
	return finish()
}

// scanBackupTree reports anything in the backup the manifest does not list: a
// stray -wal/-shm would silently attach to the database when it is opened
// normally, and any other extra file means the directory is not what the
// manifest describes.
func scanBackupTree(dir string, m *Manifest, add func(Code, string, ...any)) {
	files := map[string]bool{ManifestName: true}
	dirs := map[string]bool{}
	for _, a := range m.Assets {
		files[a.Path] = true
		for parent := path.Dir(a.Path); parent != "."; parent = path.Dir(parent) {
			dirs[parent] = true
		}
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			add(CodeIO, "scan %s: %v", p, err)
			return nil
		}
		if p == dir {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		switch {
		case files[rel]:
			// Listed; the asset pass inspects it.
		case d.Type()&fs.ModeSymlink != 0:
			add(CodeSymlinkRejected, "%s is a symlink", rel)
		case d.IsDir():
			inCatalog := rel == "skills" || rel == SkillCatalogPath || strings.HasPrefix(rel, SkillCatalogPath+"/")
			if !dirs[rel] && !inCatalog {
				add(CodeAssetUnexpected, "unexpected directory %s", rel)
				return filepath.SkipDir
			}
		case isSQLiteSidecar(path.Base(rel)):
			add(CodeStraySidecar, "%s would attach to the database when opened; a backup snapshot must stand alone", rel)
		default:
			add(CodeAssetUnexpected, "unexpected file %s", rel)
		}
		return nil
	})
}

func isSQLiteSidecar(name string) bool {
	return strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal")
}
