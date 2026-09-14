package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
)

// IdentityPolicy decides what happens when the backup's installation identity
// differs from the destination's.
type IdentityPolicy string

const (
	// IdentityStrict refuses a mismatch (the default).
	IdentityStrict IdentityPolicy = ""
	// IdentityFromBackup installs the backup's identity: disaster recovery of
	// the installation the backup came from.
	IdentityFromBackup IdentityPolicy = "backup"
	// IdentityKeepDestination keeps the destination's identity: a restore as a
	// copy into another installation.
	IdentityKeepDestination IdentityPolicy = "destination"
)

// Restore results.
const (
	ResultRestored       = "RESTORED"
	ResultRefused        = "RESTORE_REFUSED"
	ResultFailed         = "RESTORE_FAILED"
	ResultRolledBack     = "RESTORE_FAILED_ROLLED_BACK"
	ResultRollbackFailed = "RESTORE_FAILED_ROLLBACK_FAILED"
)

const daemonLockFile = "daemon.lock"

// RestoreOptions configures a restore.
type RestoreOptions struct {
	DataDir string
	Source  string
	// Root receives the pre-restore backup; empty means DefaultRoot(DataDir).
	Root                   string
	Identity               IdentityPolicy
	AllowSecretKeyMismatch bool
	// PreserveBrokenState allows restoring over a database too damaged to
	// produce a VALID pre-restore backup: its files are kept as a byte-for-byte
	// forensic copy instead. Space, I/O and cancellation failures still refuse.
	PreserveBrokenState bool
	// CheckDaemon must prove that no AO daemon serves DataDir -- live,
	// unhealthy or unverified -- without signalling anything. It is required:
	// a restore that cannot ask fails closed.
	CheckDaemon func(context.Context) error
	Tool        ToolInfo
	Progress    func(phase string)

	hooks *testHooks
}

// RestoreReport describes what a restore did.
type RestoreReport struct {
	Result             string `json:"result"`
	RestoreID          string `json:"restoreId,omitempty"`
	Source             string `json:"source"`
	SourceBackupID     string `json:"sourceBackupId,omitempty"`
	DataDir            string `json:"dataDir"`
	RollbackBackupID   string `json:"rollbackBackupId,omitempty"`
	RollbackBackupPath string `json:"rollbackBackupPath,omitempty"`
	// RollbackKind says what RollbackBackupPath is: a verified_backup, or a
	// forensic_copy of a damaged state that is NOT restorable.
	RollbackKind       string        `json:"rollbackKind,omitempty"`
	GooseVersion       int64         `json:"gooseVersion,omitempty"`
	BinaryHead         int64         `json:"binaryHead,omitempty"`
	Compatibility      Compatibility `json:"compatibility,omitempty"`
	IdentityAction     string        `json:"identityAction,omitempty"`
	DestinationTouched bool          `json:"destinationTouched"`
	// RecoverRequired is set when the restore journal could not be cleared:
	// whatever the result, `ao backup recover` must run before AO starts.
	RecoverRequired bool      `json:"recoverRequired,omitempty"`
	Reason          *Finding  `json:"reason,omitempty"`
	Reasons         []Finding `json:"reasons,omitempty"`
	Warnings        []Finding `json:"warnings"`
	DurationMs      int64     `json:"durationMs"`
}

func (r *RestoreReport) warn(code Code, format string, args ...any) {
	r.Warnings = append(r.Warnings, Finding{Code: code, Detail: fmt.Sprintf(format, args...)})
}

// Restore replaces the durable state of a STOPPED AO data dir with a backup.
//
// Before anything in the data dir changes it verifies the backup in full,
// proves AO is stopped (the caller's P9 discovery, the data dir's daemon.lock
// held for the whole restore, and a SQLite exclusive probe), applies the
// identity and secret-key policies, checks free space, and takes a verified
// pre-restore backup of the current state. The backup is then copied into a
// staging dir on the destination filesystem and checked again. Only then are
// the managed entries swapped by rename -- the current ones, SQLite sidecars
// first, moved aside; the staged ones promoted -- and the result verified. A
// failure after the swap began puts the previous files back; a crash at any
// point leaves a journal that `Recover` resolves and the daemon refuses to boot
// over. Migrations never run here: the database is restored exactly as backed
// up, and the next daemon start decides whether to migrate.
func Restore(ctx context.Context, opts RestoreOptions) (rep *RestoreReport, err error) {
	start := time.Now()
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	rep = &RestoreReport{Source: opts.Source, DataDir: opts.DataDir, Warnings: []Finding{}}
	var root string
	defer func() {
		rep.DurationMs = time.Since(start).Milliseconds()
		rec := OpRecord{At: time.Now(), Op: "restore", BackupID: rep.SourceBackupID, DurationMs: rep.DurationMs,
			GooseVersion: rep.GooseVersion, RollbackBackupID: rep.RollbackBackupID}
		if err == nil {
			rep.Result = ResultRestored
		} else {
			e, ok := AsError(err)
			if !ok {
				e = failedf(CodeIO, err, "restore")
				err = e
			}
			rep.Reason = &Finding{Code: e.Code, Detail: e.Error()}
			rec.Reason = e.Code
			switch e.Class {
			case ClassRolledBack:
				rep.Result = ResultRolledBack
			case ClassRollbackFailed:
				rep.Result = ResultRollbackFailed
			case ClassRefused, ClassInvalid, ClassIncompatible:
				rep.Result = ResultRefused
			default:
				rep.Result = ResultFailed
			}
		}
		rec.Result = rep.Result
		appendOp(root, rec)
	}()

	// ---- Preflight: nothing below replaces anything in the data dir. (The SQLite
	// probe may checkpoint committed WAL frames into ao.db -- the same logical
	// state -- and leave an empty -shm; daemon.lock is created if absent.) ----
	if opts.CheckDaemon == nil {
		return rep, refusedf(CodeInvalidArgument, "restore requires a daemon check; refusing to assume AO is stopped")
	}
	switch opts.Identity {
	case IdentityStrict, IdentityFromBackup, IdentityKeepDestination:
	default:
		return rep, refusedf(CodeInvalidArgument, "unknown identity policy %q (use backup or destination)", opts.Identity)
	}
	dataDir, err := resolveDataDir(opts.DataDir)
	if err != nil {
		return rep, err
	}
	rep.DataDir = dataDir
	if err := refuseUnresolvedJournal(dataDir); err != nil {
		return rep, err
	}

	srcAbs, err := filepath.Abs(opts.Source)
	if err != nil {
		return rep, refusedf(CodeInvalidArgument, "backup path: %v", err)
	}
	if li, err := os.Lstat(srcAbs); err == nil && li.Mode()&os.ModeSymlink != 0 {
		return rep, refusedf(CodeUnsafePath, "%s is a symlink; pass the backup directory itself", srcAbs)
	}
	source, err := resolveExisting(srcAbs)
	if err != nil {
		return rep, refusedf(CodeInvalidArgument, "backup path: %v", err)
	}
	rep.Source = source
	if within(source, dataDir) {
		return rep, refusedf(CodeSourceInsideDestination,
			"backup %s lives inside the data dir %s being restored; move it outside first", source, dataDir)
	}
	root, err = resolveRoot(dataDir, opts.Root)
	if err != nil {
		return rep, err
	}
	// The backup is locked, in its own root, for the whole restore so retention
	// there cannot delete it between verification and copy.
	if ValidBackupID(filepath.Base(source)) {
		srcRoot := filepath.Dir(source)
		srcLock, err := lockBackup(srcRoot, filepath.Base(source))
		switch {
		case errors.Is(err, daemonlock.ErrHeld):
			return rep, refusedf(CodeBackupInUse, "backup %s is in use by another operation", filepath.Base(source))
		case err != nil:
			rep.warn(CodeIO, "could not lock the backup in %s (%v); retention there is not blocked during this restore", srcRoot, err)
		default:
			defer func() { _ = srcLock.Release() }()
		}
	}

	progress("verifying backup")
	vrep, err := Verify(ctx, source, VerifyOptions{Progress: opts.Progress, hooks: opts.hooks})
	if err != nil {
		if ctx.Err() != nil {
			return rep, &Error{Code: CodeCanceled, Class: ClassFailed, Msg: "restore canceled during verification; nothing was changed", Err: err}
		}
		return rep, failedf(CodeIO, err, "verify backup")
	}
	rep.SourceBackupID, rep.GooseVersion, rep.BinaryHead, rep.Compatibility = vrep.BackupID, vrep.GooseVersion, vrep.BinaryHead, vrep.Compatibility
	switch {
	case vrep.Status == StatusUnsupported:
		rep.Reasons = vrep.Reasons
		return rep, &Error{Code: CodeUnsupportedManifest, Class: ClassIncompatible, Msg: "backup format is not supported by this binary: " + firstReason(vrep.Reasons)}
	case vrep.Status != StatusValid:
		rep.Reasons = vrep.Reasons
		return rep, &Error{Code: CodeBackupInvalid, Class: ClassInvalid, Msg: "backup is INVALID: " + firstReason(vrep.Reasons)}
	case vrep.Compatibility != CompatCompatible && vrep.Compatibility != CompatUpgradeRequired:
		rep.Reasons = vrep.Reasons
		return rep, &Error{Code: CodeNewerThanBinary, Class: ClassIncompatible,
			Msg: fmt.Sprintf("backup schema %d is newer than this binary's head %d; restoring it would be a downgrade this binary cannot read", vrep.GooseVersion, vrep.BinaryHead)}
	}
	m := vrep.manifest
	if vrep.Compatibility == CompatUpgradeRequired {
		rep.warn(CodeUpgradeRequired, "backup schema %d is older than this binary's head %d: the restore does not migrate, but the next daemon start will", vrep.GooseVersion, vrep.BinaryHead)
	}

	progress("checking AO is stopped")
	if err := opts.CheckDaemon(ctx); err != nil {
		if e, ok := AsError(err); ok {
			return rep, e
		}
		return rep, &Error{Code: CodeDaemonAmbiguous, Class: ClassRefused, Msg: "could not prove AO is stopped", Err: err}
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return rep, failedf(CodeIO, err, "create data dir")
	}
	if dataDir, err = filepath.EvalSymlinks(dataDir); err != nil {
		return rep, failedf(CodeIO, err, "resolve data dir")
	}
	rep.DataDir = dataDir
	dirLock, err := daemonlock.Acquire(filepath.Join(dataDir, daemonLockFile))
	if errors.Is(err, daemonlock.ErrHeld) {
		return rep, refusedf(CodeDataDirLocked, "an AO daemon (or another restore) holds %s; stop it with `ao stop` first", filepath.Join(dataDir, daemonLockFile))
	} else if err != nil {
		return rep, failedf(CodeIO, err, "lock data dir")
	}
	defer func() { _ = dirLock.Release() }()
	if err := refuseUnresolvedJournal(dataDir); err != nil {
		return rep, err
	}
	for _, e := range append(slices.Clone(managedEntries), secretKeyFile, "skills") {
		if _, _, err := present(filepath.Join(dataDir, filepath.FromSlash(e))); err != nil {
			return rep, refusedf(CodeUnsafePath, "destination %s: %v", e, err)
		}
	}
	dbPath := filepath.Join(dataDir, DatabaseAsset)
	damage, err := probeQuiet(dbPath, opts.hooks)
	if err != nil {
		return rep, failedf(CodeIO, err, "probe destination database")
	}
	if damage != nil && !opts.PreserveBrokenState {
		return rep, &Error{Code: CodeDestinationDamaged, Class: ClassRefused, Err: damage,
			Msg: "the destination database is damaged, so no VALID pre-restore backup can be taken; nothing was changed. To restore over it, pass --preserve-broken-state: its files are then kept as a byte-for-byte forensic copy"}
	}
	if err := refuseOpenHolders(opts.hooks, "the destination database", dbFamily(dataDir)); err != nil {
		return rep, err
	}

	destID, err := readIdentity(dataDir)
	if err != nil {
		return rep, err
	}
	promoteIdentity, action, err := decideIdentity(m.Source.InstallationID, destID, opts.Identity)
	if err != nil {
		return rep, err
	}
	rep.IdentityAction = action

	// Without a database there can be no pre-restore backup. Refuse when the
	// restore would still replace something that exists only in this data dir.
	if _, ok, _ := present(dbPath); !ok {
		if unbacked := unbackedEntries(dataDir, promoteIdentity, destID, m.Source.InstallationID); len(unbacked) > 0 {
			return rep, refusedf(CodeNoRollbackPossible,
				"the data dir has no database, so no pre-restore backup can be taken, yet the restore would replace %s; move it aside or restore into an empty data dir",
				strings.Join(unbacked, " and "))
		}
	}

	destKey, err := secretKeyFingerprint(dataDir)
	if err != nil {
		return rep, refusedf(CodeUnsafePath, "destination secret key: %v", err)
	}
	if m.Source.SecretKeyFingerprint != "" && destKey != m.Source.SecretKeyFingerprint {
		what := "a different secret.key"
		if destKey == "" {
			what = "no secret.key"
		}
		if !opts.AllowSecretKeyMismatch {
			return rep, refusedf(CodeSecretKeyMismatch,
				"the destination has %s than the one this backup's encrypted settings were sealed with; the SMTP password, work-item token and skill secrets would be unreadable. Put the original secret.key back, or pass --allow-secret-key-mismatch to accept re-entering them", what)
		}
		rep.warn(CodeSecretKeyMismatch, "destination has %s: encrypted settings in the restored database will be unreadable until re-entered", what)
	}
	if fingerprint("data-dir", []byte(dataDir)) != m.Source.DataDirFingerprint {
		rep.warn(CodeDataDirDiffers, "the backup was taken from another data dir; paths the database stores into its data dir (skills, worktrees) may not resolve")
	}

	var stagedBytes, currentBytes int64
	for _, a := range m.Assets {
		if a.Role != RoleInstallationIdentity || promoteIdentity {
			stagedBytes += a.Size
		}
	}
	for _, e := range append(slices.Clone(sqliteSidecars), DatabaseAsset) {
		if fi, ok, _ := present(filepath.Join(dataDir, e)); ok {
			currentBytes += fi.Size()
		}
	}
	if free, ok := opts.hooks.available(dataDir); ok && free < uint64(stagedBytes+currentBytes+freeSpaceMargin) { //nolint:gosec // sizes are non-negative
		return rep, refusedf(CodeInsufficientSpace, "%s has %d bytes free; staging and the swap need about %d", dataDir, free, stagedBytes+currentBytes+freeSpaceMargin)
	}
	if free, ok := opts.hooks.available(root); ok && free < uint64(currentBytes+freeSpaceMargin) { //nolint:gosec // sizes are non-negative
		return rep, refusedf(CodeInsufficientSpace, "%s has %d bytes free; the pre-restore backup needs about %d", root, free, currentBytes+freeSpaceMargin)
	}
	if ctx.Err() != nil {
		return rep, &Error{Code: CodeCanceled, Class: ClassFailed, Msg: "restore canceled; nothing in the data dir was replaced", Err: ctx.Err()}
	}

	// ---- Execution. ----
	restoreID, err := newID("aor-", time.Now())
	if err != nil {
		return rep, failedf(CodeIO, err, "mint restore id")
	}
	rep.RestoreID = restoreID
	workDir := filepath.Join(dataDir, restoreWorkPrefix+restoreID)
	j := &journal{RestoreID: restoreID, SourceBackupID: m.BackupID, SourcePath: source, WorkDir: filepath.Base(workDir), Phase: PhasePreparing}
	record := func() error { return opts.hooks.recordJournal(dataDir, j) }
	dropJournal := func() error { return opts.hooks.dropJournal(dataDir) }
	if err := record(); err != nil {
		return rep, failedf(CodeIO, err, "write restore journal")
	}

	// abandon ends a restore that never reached the swap: the destination's
	// managed entries were not touched, so removing the work dir and the
	// journal is the whole cleanup.
	abandon := func(cause error) error {
		// The journal goes first: a work dir no journal names is harmless, while a
		// journal whose work dir is gone is one recover cannot act on -- and the
		// journal may already read "swapping" if that write landed but failed.
		if rerr := dropJournal(); rerr != nil {
			rep.RecoverRequired = true
			rep.warn(CodeIO, "could not remove the restore journal (%v); %s is kept, and `ao backup recover` must run before AO starts", rerr, workDir)
		} else if rerr := os.RemoveAll(workDir); rerr != nil {
			rep.warn(CodeIO, "could not remove %s (%v); no journal refers to it and it is safe to delete by hand", workDir, rerr)
		}
		if ctx.Err() != nil {
			if e, ok := AsError(cause); !ok || e.Class != ClassRefused {
				return &Error{Code: CodeCanceled, Class: ClassFailed, Msg: "restore canceled before the swap; nothing in the data dir was replaced", Err: cause}
			}
		}
		return cause
	}

	if _, ok, _ := present(dbPath); ok {
		var cerr error
		if damage == nil {
			progress("creating pre-restore backup")
			create := Create
			if opts.hooks != nil && opts.hooks.createRollback != nil {
				create = opts.hooks.createRollback
			}
			var cres *CreateResult
			cres, cerr = create(ctx, CreateOptions{DataDir: dataDir, Root: root, Kind: KindPreRestore,
				Note: "before restoring " + m.BackupID, Tool: opts.Tool, Progress: opts.Progress, hooks: opts.hooks})
			if cerr == nil {
				vr, verr := Verify(ctx, cres.Path, VerifyOptions{Quick: true, hooks: opts.hooks})
				if verr != nil || vr.Status != StatusValid {
					reason := "verification did not run"
					if vr != nil {
						reason = firstReason(vr.Reasons)
					}
					return rep, abandon(&Error{Code: CodeRollbackBackupFailed, Class: ClassFailed,
						Msg: "the pre-restore backup did not verify (" + reason + "); nothing in the data dir was replaced", Err: verr})
				}
				rep.RollbackBackupID, rep.RollbackBackupPath, rep.RollbackKind = cres.BackupID, cres.Path, RollbackVerifiedBackup
			}
		}
		switch {
		case damage == nil && cerr == nil:
		case damage != nil || destinationDamaged(cerr):
			// The current state cannot be backed up because it is broken -- the
			// very reason to restore. Without explicit consent, refuse; with it,
			// keep every byte of it before anything is replaced.
			if damage == nil {
				damage = cerr
			}
			if !opts.PreserveBrokenState {
				return rep, abandon(&Error{Code: CodeDestinationDamaged, Class: ClassRefused, Err: damage,
					Msg: "the current database cannot produce a VALID pre-restore backup; nothing in the data dir was replaced. To restore over it, pass --preserve-broken-state: its files are then kept as a byte-for-byte forensic copy"})
			}
			progress("preserving the damaged state as a forensic copy")
			fpath, ferr := forensicCopy(ctx, dataDir, root, restoreID)
			if ferr != nil {
				return rep, abandon(&Error{Code: CodeRollbackBackupFailed, Class: ClassFailed,
					Msg: "could not preserve the damaged state; nothing in the data dir was replaced", Err: ferr})
			}
			rep.RollbackBackupID, rep.RollbackBackupPath, rep.RollbackKind = filepath.Base(fpath), fpath, RollbackForensicCopy
			rep.warn(CodeDestinationDamaged, "the current database could not be backed up (%v); its files were kept byte for byte in %s, which is evidence and NOT a restorable backup", damage, fpath)
		default:
			return rep, abandon(&Error{Code: CodeRollbackBackupFailed, Class: ClassFailed,
				Msg: "could not create the pre-restore backup; nothing in the data dir was replaced", Err: cerr})
		}
		j.RollbackBackupID, j.RollbackBackupPath = rep.RollbackBackupID, rep.RollbackBackupPath
	} else {
		rep.warn(CodeAssetMissing, "the data dir had no database, so there was nothing to back up before restoring")
	}
	j.Phase = PhaseRollbackReady
	if err := record(); err != nil {
		return rep, abandon(failedf(CodeIO, err, "write restore journal"))
	}
	if err := opts.hooks.phase(PhaseRollbackReady); err != nil {
		return rep, abandon(failedf(CodeStagingFailed, err, "before staging"))
	}

	progress("staging")
	promote, err := stageRestore(ctx, source, workDir, m, promoteIdentity, opts.hooks)
	if err != nil {
		return rep, abandon(failedf(CodeStagingFailed, err, "stage the backup; nothing in the data dir was replaced"))
	}
	j.Phase, j.Promote = PhaseStaged, promote
	if err := record(); err != nil {
		return rep, abandon(failedf(CodeIO, err, "write restore journal"))
	}
	if err := opts.hooks.phase(PhaseStaged); err != nil {
		return rep, abandon(failedf(CodeStagingFailed, err, "after staging"))
	}
	if ctx.Err() != nil {
		return rep, abandon(ctx.Err())
	}
	// Second probe: the pre-restore backup took minutes on a large database.
	if _, err := probeQuiet(dbPath, opts.hooks); err != nil {
		return rep, abandon(failedf(CodeIO, err, "probe destination database"))
	}
	if err := refuseOpenHolders(opts.hooks, "the destination database", dbFamily(dataDir)); err != nil {
		return rep, abandon(err)
	}

	pre := map[string]os.FileInfo{}
	for _, e := range managedEntries {
		fi, ok, err := present(filepath.Join(dataDir, filepath.FromSlash(e)))
		if err != nil {
			return rep, abandon(refusedf(CodeUnsafePath, "destination %s: %v", e, err))
		}
		if ok {
			pre[e] = fi
			j.PreExisting = append(j.PreExisting, e)
		}
	}
	if fi := pre[DatabaseAsset]; fi != nil {
		j.PreDatabase = stampOf(fi)
	}
	j.Phase = PhaseSwapping
	if err := record(); err != nil {
		return rep, abandon(failedf(CodeIO, err, "write restore journal"))
	}

	// ---- Critical section: finishes or rolls back; cancellation is ignored. ----
	rep.DestinationTouched = true
	critical := context.WithoutCancel(ctx)
	// settle ends a restore whose data dir holds one whole state again. The
	// journal goes first and the work dir only once no journal names it; a
	// journal that cannot be removed is rewritten to the settled phase; when
	// neither works the work dir stays, so the recover that the journal still
	// demands can finish. It reports whether that recover is required.
	settle := func(settled Phase) bool {
		derr := dropJournal()
		if derr == nil {
			if err := os.RemoveAll(workDir); err != nil {
				rep.warn(CodeIO, "could not remove %s (%v); no journal refers to it and it is safe to delete by hand", workDir, err)
			}
			return false
		}
		j.Phase = settled
		if err := record(); err == nil {
			rep.warn(CodeIO, "the restore journal could not be removed (%v); it records %s, and `ao backup recover` will clean up", derr, settled)
			return false
		}
		rep.warn(CodeIO, "the restore journal could be neither removed nor updated (%v); %s is kept, and `ao backup recover` must run before AO starts", derr, workDir)
		return true
	}
	rollback := func(code Code, cause error) error {
		progress("rolling back")
		// Rolling back moves the restored database out from under its name. A
		// process that opened it since the swap would then attach its -wal/-shm,
		// by name, to the previous database: leave the journal demanding recover,
		// which checks again, rather than corrupt the state being put back.
		if herr := refuseOpenHolders(opts.hooks, "the restored database", restoredDatabaseFiles(dataDir, workDir)); herr != nil {
			j.Phase = PhaseRollbackFailed
			_ = record()
			return &Error{Code: CodeRollbackFailed, Class: ClassRollbackFailed,
				Msg: fmt.Sprintf("restore failed (%s: %v) and cannot be rolled back safely while the restored database is open; do not start AO. Close that process, then run `ao backup recover`", code, cause),
				Err: herr}
		}
		j.Phase = PhaseRollingBack
		_ = record()
		rbErr := rollbackEntries(dataDir, workDir, j.Promote, j.PreExisting, opts.hooks)
		if rbErr == nil {
			rbErr = checkRolledBack(dataDir, j, pre)
		}
		if rbErr != nil {
			j.Phase = PhaseRollbackFailed
			_ = record()
			where := "none was needed"
			if rep.RollbackBackupPath != "" {
				where = rep.RollbackBackupPath
			}
			return &Error{Code: CodeRollbackFailed, Class: ClassRollbackFailed,
				Msg: fmt.Sprintf("restore failed (%s: %v) AND putting the previous state back failed; do not start AO. Run `ao backup recover`; the pre-restore backup is %s", code, cause, where),
				Err: rbErr}
		}
		msg := "restore failed after the swap began; the previous state was put back"
		if rep.RecoverRequired = settle(PhaseRolledBack); rep.RecoverRequired {
			msg += ", but its journal could not be cleared: run `ao backup recover` before starting AO"
		}
		return &Error{Code: code, Class: ClassRolledBack, Msg: msg, Err: cause}
	}

	if err := opts.hooks.phase(PhaseSwapping); err != nil {
		return rep, rollback(CodeSwapFailed, err)
	}
	progress("swapping")
	if err := swapIn(dataDir, workDir, promote, opts.hooks); err != nil {
		return rep, rollback(CodeSwapFailed, err)
	}
	j.Phase = PhaseSwapped
	if err := record(); err != nil {
		return rep, rollback(CodeSwapFailed, err)
	}
	if err := opts.hooks.phase(PhaseSwapped); err != nil {
		return rep, rollback(CodeRestoreVerifyFailed, err)
	}
	progress("verifying restore")
	if err := finalVerify(critical, dataDir, workDir, m, promoteIdentity, destID, opts.hooks); err != nil {
		return rep, rollback(CodeRestoreVerifyFailed, err)
	}

	j.Phase = PhaseComplete
	if err := record(); err != nil {
		// RESTORED is reported only once the journal durably says complete. A
		// journal still reading "swapped" -- or a completion record that may not
		// have reached the disk -- describes a restore recover would undo, so undo
		// it now: the report and recover can then never disagree. Re-run it.
		return rep, rollback(CodeIO, fmt.Errorf("record the completed restore: %w", err))
	}
	if err := dropJournal(); err != nil {
		rep.warn(CodeIO, "the restore is complete and recorded, but its journal could not be removed (%v); `ao backup recover` will clean up", err)
		return rep, nil
	}
	if err := os.RemoveAll(workDir); err != nil {
		rep.warn(CodeIO, "could not remove %s (%v); no journal refers to it and it is safe to delete by hand", workDir, err)
	}
	return rep, nil
}

func firstReason(findings []Finding) string {
	if len(findings) == 0 {
		return "no reason recorded"
	}
	return string(findings[0].Code) + ": " + findings[0].Detail
}

func refuseUnresolvedJournal(dataDir string) error {
	j, err := readJournal(dataDir)
	if err != nil {
		return refusedf(CodeRestoreInterrupted, "a restore journal in %s cannot be read (%v); inspect it and run `ao backup recover`", dataDir, err)
	}
	if j != nil {
		return refusedf(CodeRestoreInterrupted, "restore %s (phase %s) has not been resolved; run `ao backup recover` first", j.RestoreID, j.Phase)
	}
	return nil
}

// unbackedEntries names what a restore would replace in a data dir that has no
// database (and so no pre-restore backup): a non-empty skill catalog, or an
// installation identity the backup's would overwrite.
func unbackedEntries(dataDir string, promoteIdentity bool, destID, backupID string) []string {
	var out []string
	catalogHasFiles := false
	_ = filepath.WalkDir(filepath.Join(dataDir, filepath.FromSlash(SkillCatalogPath)), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			catalogHasFiles = true
			return filepath.SkipAll
		}
		return nil
	})
	if catalogHasFiles {
		out = append(out, SkillCatalogPath)
	}
	if promoteIdentity && destID != "" && destID != backupID {
		out = append(out, IdentityAsset)
	}
	return out
}

func readIdentity(dataDir string) (string, error) {
	p := filepath.Join(dataDir, IdentityAsset)
	_, ok, err := present(p)
	if err != nil {
		return "", refusedf(CodeUnsafePath, "destination installation identity: %v", err)
	}
	if !ok {
		return "", nil
	}
	f, err := openRegular(p)
	if err != nil {
		return "", refusedf(CodeUnsafePath, "destination installation identity: %v", err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	id := strings.TrimSpace(string(buf[:n]))
	if !daemonmeta.ValidInstallationID(id) {
		return "", refusedf(CodeInstallationMismatch, "the destination installation identity is malformed; refusing to decide over it")
	}
	return id, nil
}

// decideIdentity applies the installation identity policy (P10 §19).
func decideIdentity(backupID, destID string, policy IdentityPolicy) (promote bool, action string, err error) {
	switch {
	case backupID == "":
		return false, "kept_destination", nil
	case destID == "":
		return true, "restored", nil
	case destID == backupID:
		return true, "unchanged", nil
	case policy == IdentityFromBackup:
		return true, "replaced_with_backup", nil
	case policy == IdentityKeepDestination:
		return false, "kept_destination", nil
	default:
		return false, "", refusedf(CodeInstallationMismatch,
			"the backup belongs to installation %s but this data dir is installation %s; pass --identity=backup to recover that installation here, or --identity=destination to restore as a copy that keeps this one", backupID, destID)
	}
}

// stageRestore copies the backup into <workDir>/staged on the destination
// filesystem, checking every byte against the verified manifest, and returns
// the entries to promote.
func stageRestore(ctx context.Context, source, workDir string, m *Manifest, promoteIdentity bool, h *testHooks) ([]string, error) {
	staged := filepath.Join(workDir, "staged")
	if err := os.MkdirAll(filepath.Join(staged, filepath.FromSlash(SkillCatalogPath)), 0o700); err != nil {
		return nil, err
	}
	if h != nil && h.beforeStagingCopy != nil {
		if err := h.beforeStagingCopy(); err != nil {
			return nil, err
		}
	}
	for _, a := range m.Assets {
		if a.Role == RoleInstallationIdentity && !promoteIdentity {
			continue
		}
		src := filepath.Join(source, filepath.FromSlash(a.Path))
		dst := filepath.Join(staged, filepath.FromSlash(a.Path))
		mode := os.FileMode(0o600)
		if a.Role == RoleSkillPackage {
			pm, err := parseMode(a.Mode)
			if err != nil {
				return nil, err
			}
			mode = ownerOnly(os.FileMode(pm))
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		n, sum, err := copyFileHashed(ctx, src, dst, mode)
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", a.Path, err)
		}
		if n != a.Size || sum != a.SHA256 {
			return nil, fmt.Errorf("%s changed after the backup was verified", a.Path)
		}
	}
	if err := syncTree(staged); err != nil {
		return nil, err
	}
	facts, err := inspectDatabase(ctx, filepath.Join(staged, DatabaseAsset), true)
	if err != nil {
		return nil, fmt.Errorf("check staged database: %w", err)
	}
	if facts.Integrity != "ok" || facts.GooseVersion != m.Schema.GooseVersion {
		return nil, fmt.Errorf("staged database failed its check (quick_check %q, goose %d)", facts.Integrity, facts.GooseVersion)
	}
	promote := []string{DatabaseAsset}
	if promoteIdentity {
		promote = append(promote, IdentityAsset)
	}
	return append(promote, SkillCatalogPath), nil
}

// swapIn moves the current managed entries aside (SQLite sidecars first) and
// promotes the staged ones. Every step is a rename inside one filesystem.
func swapIn(dataDir, workDir string, promote []string, h *testHooks) error {
	prev := filepath.Join(workDir, "previous")
	staged := filepath.Join(workDir, "staged")
	if err := os.MkdirAll(filepath.Join(prev, "skills"), 0o700); err != nil {
		return err
	}
	for _, e := range managedEntries {
		if e == IdentityAsset && !slices.Contains(promote, IdentityAsset) {
			continue // the destination keeps its identity: never move it
		}
		src := filepath.Join(dataDir, filepath.FromSlash(e))
		if _, ok, err := present(src); err != nil {
			return err
		} else if !ok {
			continue
		}
		if err := h.move(src, filepath.Join(prev, filepath.FromSlash(e))); err != nil {
			return err
		}
	}
	for _, s := range sqliteSidecars {
		if _, ok, _ := present(filepath.Join(dataDir, s)); ok {
			return fmt.Errorf("%s reappeared during the swap: something opened the database", s)
		}
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "skills"), 0o750); err != nil {
		return err
	}
	for _, p := range promote {
		if err := h.move(filepath.Join(staged, filepath.FromSlash(p)), filepath.Join(dataDir, filepath.FromSlash(p))); err != nil {
			return err
		}
	}
	return errors.Join(syncDir(dataDir), syncDir(filepath.Join(dataDir, "skills")),
		syncDir(prev), syncDir(filepath.Join(prev, "skills")), syncDir(staged))
}

// rollbackEntries puts the pre-swap entries back. It is idempotent and needs
// no memory of how far the swap -- or an earlier rollback -- got, only the
// journal's promote and pre-existing lists:
//
//   - a promoted entry whose staged copy is gone was promoted at some point.
//     The file at its name is the RESTORED one (and goes to failed/) while its
//     original still waits in previous/, or when no original ever existed.
//     With the original already back from previous/, it is the original: kept;
//   - a SQLite sidecar that did not exist before the swap was grown by the
//     restored database (or by a probe since) and goes to failed/: before the
//     swap the data dir was locked and probed quiet, so it holds no data of
//     the original database;
//   - anything still in previous/ goes back to its name.
func rollbackEntries(dataDir, workDir string, promote, preExisting []string, h *testHooks) error {
	prev := filepath.Join(workDir, "previous")
	staged := filepath.Join(workDir, "staged")
	failed := filepath.Join(workDir, "failed")
	for _, d := range []string{workDir, prev, staged, failed,
		filepath.Join(prev, "skills"), filepath.Join(staged, "skills"), filepath.Join(failed, "skills")} {
		if fi, err := os.Lstat(d); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing to move files through it", d)
		}
	}
	if err := os.MkdirAll(filepath.Join(failed, "skills"), 0o700); err != nil {
		return err
	}
	var errs []error
	exists := func(p string) bool {
		_, err := os.Lstat(p)
		return err == nil
	}
	moveOut := func(e string) {
		if !exists(filepath.Join(dataDir, filepath.FromSlash(e))) {
			return
		}
		dst := filepath.Join(failed, filepath.FromSlash(e))
		for i := 1; exists(dst); i++ {
			dst = filepath.Join(failed, filepath.FromSlash(e)) + fmt.Sprintf(".%d", i)
		}
		if err := h.move(filepath.Join(dataDir, filepath.FromSlash(e)), dst); err != nil {
			errs = append(errs, err)
		}
	}

	for _, p := range promote {
		promoted := !exists(filepath.Join(staged, filepath.FromSlash(p)))
		originalWaiting := exists(filepath.Join(prev, filepath.FromSlash(p)))
		if promoted && (originalWaiting || !slices.Contains(preExisting, p)) {
			moveOut(p)
		}
	}
	for _, s := range sqliteSidecars {
		if !slices.Contains(preExisting, s) {
			moveOut(s)
		}
	}
	for _, e := range managedEntries {
		from := filepath.Join(prev, filepath.FromSlash(e))
		if !exists(from) {
			continue
		}
		to := filepath.Join(dataDir, filepath.FromSlash(e))
		moveOut(e) // something squats the name: never overwrite it
		if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := h.move(from, to); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, syncDir(dataDir))
	if exists(filepath.Join(dataDir, "skills")) {
		errs = append(errs, syncDir(filepath.Join(dataDir, "skills")))
	}
	return errors.Join(errs...)
}

// checkRolledBack proves the data dir holds exactly the pre-swap entries: the
// same names, and -- when this process saw them -- the same files.
func checkRolledBack(dataDir string, j *journal, pre map[string]os.FileInfo) error {
	for _, e := range managedEntries {
		if slices.Contains(sqliteSidecars, e) {
			// Not state: SQLite deletes an empty -wal/-shm whenever anything opens
			// and closes the database (an operator's read between a failed restore
			// and recover does), and rollbackEntries already failed if one could
			// not be moved back. ao.db's stamp below proves no frame was lost: a
			// WAL with frames is only deleted after checkpointing them into ao.db.
			continue
		}
		fi, ok, err := present(filepath.Join(dataDir, filepath.FromSlash(e)))
		if err != nil {
			return err
		}
		want := slices.Contains(j.PreExisting, e)
		if ok != want {
			return fmt.Errorf("after rollback %s present=%t, before the swap present=%t", e, ok, want)
		}
		if ok && pre != nil && pre[e] != nil && !os.SameFile(pre[e], fi) {
			return fmt.Errorf("after rollback %s is not the file that was there before the swap", e)
		}
	}
	if j.PreDatabase != nil {
		fi, err := os.Lstat(filepath.Join(dataDir, DatabaseAsset))
		if err != nil {
			return err
		}
		if fi.Size() != j.PreDatabase.Size || fi.ModTime().UnixNano() != j.PreDatabase.ModTimeUnixNano {
			return errors.New("after rollback ao.db differs in size or modification time from the pre-swap database")
		}
	}
	return nil
}

// finalVerify proves the data dir now holds the backup: no stale sidecar, the
// database byte-identical to the manifest and passing quick_check at the same
// goose version, the expected identity, exactly the backup's skill files, and
// no process still holding the database the swap replaced.
func finalVerify(ctx context.Context, dataDir, workDir string, m *Manifest, promoteIdentity bool, destID string, h *testHooks) error {
	for _, s := range sqliteSidecars {
		if _, ok, _ := present(filepath.Join(dataDir, s)); ok {
			return fmt.Errorf("stale %s survived the swap", s)
		}
	}
	skills := map[string]Asset{}
	for _, a := range m.Assets {
		p := filepath.Join(dataDir, filepath.FromSlash(a.Path))
		switch a.Role {
		case RoleDatabase:
			n, sum, err := hashFile(ctx, p)
			if err != nil {
				return fmt.Errorf("hash restored database: %w", err)
			}
			if n != a.Size || sum != a.SHA256 {
				return errors.New("restored database does not match the backup")
			}
			facts, err := inspectDatabase(ctx, p, true)
			if err != nil {
				return fmt.Errorf("check restored database: %w", err)
			}
			if facts.Integrity != "ok" || facts.ForeignKeyViolations != 0 || facts.GooseVersion != m.Schema.GooseVersion {
				return fmt.Errorf("restored database failed its checks (quick_check %q, %d fk violations, goose %d)",
					facts.Integrity, facts.ForeignKeyViolations, facts.GooseVersion)
			}
		case RoleSkillPackage:
			skills[a.Path] = a
			n, sum, err := hashFile(ctx, p)
			if err != nil || n != a.Size || sum != a.SHA256 {
				return fmt.Errorf("restored skill file %s does not match the backup", a.Path)
			}
		}
	}
	want := destID
	if promoteIdentity {
		want = m.Source.InstallationID
	}
	got, err := readIdentity(dataDir)
	if err != nil || got != want {
		return fmt.Errorf("installation identity after restore is %q, want %q", got, want)
	}
	catalog := filepath.Join(dataDir, filepath.FromSlash(SkillCatalogPath))
	werr := filepath.WalkDir(catalog, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dataDir, p)
		if _, ok := skills[filepath.ToSlash(rel)]; !ok {
			return fmt.Errorf("restored skill catalog has %s, which the backup does not", filepath.ToSlash(rel))
		}
		return nil
	})
	if werr != nil {
		return werr
	}
	// A process that opened ao.db before the swap still holds the OLD file, now
	// in previous/, while the -wal/-shm it would create attach by name to the
	// restored database. No process can gain such a descriptor once the renames
	// are done, so this check, after them, closes the window the probes leave.
	if err := refuseOpenHolders(h, "the database this restore replaced", dbFamily(filepath.Join(workDir, "previous"))); err != nil {
		return err
	}
	for _, s := range sqliteSidecars {
		if _, ok, _ := present(filepath.Join(dataDir, s)); ok {
			return fmt.Errorf("%s appeared while verifying the restore", s)
		}
	}
	if h != nil && h.finalVerify != nil {
		return h.finalVerify()
	}
	return nil
}

// Recover results.
const (
	RecoverNothing    = "NOTHING_TO_RECOVER"
	RecoverNoSwap     = "RECOVERED_NO_SWAP"
	RecoverRolledBack = "RECOVERED_ROLLED_BACK"
	RecoverCompleted  = "RECOVERED_COMPLETE"
	RecoverFailed     = "RECOVER_FAILED"
)

// RecoverOptions configures Recover.
type RecoverOptions struct {
	DataDir     string
	CheckDaemon func(context.Context) error
	Progress    func(phase string)

	hooks *testHooks
}

// RecoverReport describes what Recover did.
type RecoverReport struct {
	Result             string `json:"result"`
	DataDir            string `json:"dataDir"`
	RestoreID          string `json:"restoreId,omitempty"`
	Phase              Phase  `json:"phase,omitempty"`
	RollbackBackupPath string `json:"rollbackBackupPath,omitempty"`
	Detail             string `json:"detail,omitempty"`
}

// Recover resolves a restore a crash interrupted. A restore that never began
// its swap is discarded (the data dir was not changed); one interrupted while
// swapping, verifying or rolling back is rolled back to the pre-swap state; a
// completed one only has its leftovers removed. It never rolls forward: a
// restore worth keeping is re-run.
func Recover(ctx context.Context, opts RecoverOptions) (*RecoverReport, error) {
	dataDir, err := resolveDataDir(opts.DataDir)
	if err != nil {
		return nil, err
	}
	rep := &RecoverReport{Result: RecoverNothing, DataDir: dataDir}
	if _, err := readJournal(dataDir); err != nil {
		rep.Result = RecoverFailed
		return rep, refusedf(CodeRestoreInterrupted, "the restore journal cannot be trusted (%v); inspect %s by hand -- recover will not guess", err, filepath.Join(dataDir, journalName))
	}
	if j, _ := readJournal(dataDir); j == nil {
		return rep, nil
	}
	if opts.CheckDaemon == nil {
		return rep, refusedf(CodeInvalidArgument, "recover requires a daemon check; refusing to assume AO is stopped")
	}
	if err := opts.CheckDaemon(ctx); err != nil {
		if e, ok := AsError(err); ok {
			return rep, e
		}
		return rep, &Error{Code: CodeDaemonAmbiguous, Class: ClassRefused, Msg: "could not prove AO is stopped", Err: err}
	}
	dirLock, err := daemonlock.Acquire(filepath.Join(dataDir, daemonLockFile))
	if errors.Is(err, daemonlock.ErrHeld) {
		return rep, refusedf(CodeDataDirLocked, "an AO daemon or a running restore holds %s", filepath.Join(dataDir, daemonLockFile))
	} else if err != nil {
		return rep, failedf(CodeIO, err, "lock data dir")
	}
	defer func() { _ = dirLock.Release() }()
	j, err := readJournal(dataDir)
	if err != nil || j == nil {
		return rep, err
	}
	rep.RestoreID, rep.Phase, rep.RollbackBackupPath = j.RestoreID, j.Phase, j.RollbackBackupPath
	// No SQLite probe here: opening a database to probe it checkpoints its WAL
	// and deletes its -wal/-shm, changing the very pre-swap files a rollback must
	// prove are back (and making a recover after a finished rollback fail for
	// ever). The operating system's list of open files is the proof that does
	// not mutate; daemon.lock is already held.
	if err := refuseOpenHolders(opts.hooks, "the database", dbFamily(dataDir)); err != nil {
		rep.Result = RecoverFailed
		return rep, err
	}
	workDir := filepath.Join(dataDir, j.WorkDir)
	cleanup := func(result, detail string) (*RecoverReport, error) {
		// The journal goes first; the work dir only once no journal names it.
		if err := opts.hooks.dropJournal(dataDir); err != nil {
			rep.Result = RecoverFailed
			return rep, failedf(CodeIO, err, "remove restore journal")
		}
		if err := os.RemoveAll(workDir); err != nil {
			detail += fmt.Sprintf("; %s could not be removed (%v) and, with no journal naming it, is safe to delete by hand", workDir, err)
		}
		rep.Result, rep.Detail = result, detail
		return rep, nil
	}

	switch {
	case j.unsettled(dataDir):
		// The data dir may mix two states: rolled back below.
	case j.Phase == PhaseComplete:
		return cleanup(RecoverCompleted, "the restore had completed; its leftovers were removed")
	case j.Phase == PhaseRolledBack:
		return cleanup(RecoverRolledBack, "the restore had already been rolled back; its leftovers were removed")
	default:
		return cleanup(RecoverNoSwap, "the restore stopped before its swap; the data dir was never changed")
	}

	if fi, err := os.Lstat(workDir); err != nil || !fi.IsDir() {
		rep.Result = RecoverFailed
		return rep, &Error{Code: CodeRollbackFailed, Class: ClassRollbackFailed,
			Msg: fmt.Sprintf("restore %s was interrupted in phase %s but its work dir %s is missing; recover cannot tell which files are which. Restore the pre-restore backup %s by hand",
				j.RestoreID, j.Phase, workDir, j.RollbackBackupPath)}
	}
	if err := refuseOpenHolders(opts.hooks, "the restored database", restoredDatabaseFiles(dataDir, workDir)); err != nil {
		rep.Result = RecoverFailed
		return rep, err
	}
	if opts.Progress != nil {
		opts.Progress("rolling back")
	}
	// Best effort: the journal on disk already demands this rollback, and
	// will keep demanding it until cleanup removes the journal.
	j.Phase = PhaseRollingBack
	_ = opts.hooks.recordJournal(dataDir, j)
	err = rollbackEntries(dataDir, workDir, j.Promote, j.PreExisting, opts.hooks)
	if err == nil {
		err = checkRolledBack(dataDir, j, nil)
	}
	if err != nil {
		j.Phase = PhaseRollbackFailed
		_ = opts.hooks.recordJournal(dataDir, j)
		rep.Result = RecoverFailed
		return rep, &Error{Code: CodeRollbackFailed, Class: ClassRollbackFailed,
			Msg: fmt.Sprintf("could not put the pre-swap state back; do not start AO. The pre-restore backup is %s", j.RollbackBackupPath), Err: err}
	}
	j.Phase = PhaseRolledBack
	_ = opts.hooks.recordJournal(dataDir, j)
	return cleanup(RecoverRolledBack, "the interrupted restore was rolled back to the pre-swap state")
}
