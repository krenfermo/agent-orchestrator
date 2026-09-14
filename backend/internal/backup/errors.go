// Package backup creates, verifies, lists, prunes and restores AO backups (P10).
//
// A backup is an auditable directory: a consistent SQLite snapshot of ao.db
// taken with VACUUM INTO, the installation identity, the installed skill
// packages, and a versioned manifest carrying a SHA-256 for each of them.
// Secrets (secret.key, credentials, provider logins) are never included; the
// manifest carries a one-way fingerprint of the secret key only, so a restore
// can refuse to put a database next to a key that cannot decrypt it.
//
// This package never signals or kills a process and never runs migrations.
// Restoring requires AO stopped: the caller proves no daemon is alive, and the
// package itself holds the data dir's P9 daemon.lock and probes SQLite for any
// other connection before it touches anything.
//
// Verification proves integrity, not authenticity: a VALID backup matches its
// manifest and passes SQLite's checks; nothing here proves who produced it.
package backup

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable reason. Scripts and the CLI's exit codes
// depend on these strings; never rename one.
type Code string

// Reason codes.
const (
	// Refusals: unsafe to proceed, nothing was changed.

	CodeDaemonActive            Code = "daemon_active"
	CodeDaemonUnverified        Code = "daemon_unverified"
	CodeDaemonAmbiguous         Code = "daemon_ambiguous"
	CodeDataDirLocked           Code = "data_dir_locked"
	CodeDBInUse                 Code = "db_in_use"
	CodeBackupInUse             Code = "backup_in_use"
	CodeInstallationMismatch    Code = "installation_mismatch"
	CodeSecretKeyMismatch       Code = "secret_key_mismatch"
	CodeSourceInsideDestination Code = "source_inside_destination"
	CodeInsufficientSpace       Code = "insufficient_space"
	CodeRestoreInterrupted      Code = "restore_interrupted"
	CodeUnsafePath              Code = "unsafe_path"
	CodeInvalidBackupRoot       Code = "invalid_backup_root"
	CodeInvalidArgument         Code = "invalid_argument"
	CodeNoRollbackPossible      Code = "no_rollback_possible"

	// Verification findings.

	CodeManifestMissing      Code = "manifest_missing"
	CodeManifestInvalid      Code = "manifest_invalid"
	CodeUnsupportedManifest  Code = "unsupported_manifest"
	CodeAssetMissing         Code = "asset_missing"
	CodeAssetUnexpected      Code = "asset_unexpected"
	CodeSizeMismatch         Code = "size_mismatch"
	CodeHashMismatch         Code = "hash_mismatch"
	CodeSymlinkRejected      Code = "symlink_rejected"
	CodeStraySidecar         Code = "stray_sqlite_sidecar"
	CodeStagingIncomplete    Code = "staging_incomplete"
	CodeDBUnreadable         Code = "db_unreadable"
	CodeIntegrityFailed      Code = "db_integrity_failed"
	CodeForeignKeyViolations Code = "db_foreign_key_violations"
	CodeSchemaMismatch       Code = "schema_version_mismatch"
	CodeNewerThanBinary      Code = "backup_newer_than_binary"
	CodeBackupInvalid        Code = "backup_invalid"
	CodeUpgradeRequired      Code = "upgrade_required"
	CodeDataDirDiffers       Code = "data_dir_differs"

	// Operation failures.

	CodeSnapshotFailed       Code = "snapshot_failed"
	CodeRollbackBackupFailed Code = "rollback_backup_failed"
	CodeStagingFailed        Code = "restore_staging_failed"
	CodeRestoreVerifyFailed  Code = "restore_verify_failed"
	CodeSwapFailed           Code = "restore_swap_failed"
	CodeRollbackFailed       Code = "rollback_failed"
	CodeCrossDevice          Code = "cross_device"
	CodeCanceled             Code = "canceled"
	CodeIO                   Code = "io_error"
)

// Class groups codes by what the operator must do next. The CLI maps a class to
// its exit code.
type Class string

const (
	// ClassRefused means a safety precondition failed; nothing was changed.
	ClassRefused Class = "refused"
	// ClassInvalid means the backup is not intact.
	ClassInvalid Class = "invalid"
	// ClassIncompatible means the backup is intact but this binary must not use it.
	ClassIncompatible Class = "incompatible"
	// ClassFailed means the operation failed; the destination was not modified.
	ClassFailed Class = "failed"
	// ClassRolledBack means a restore replaced the destination, failed, and put the
	// previous state back.
	ClassRolledBack Class = "rolled_back"
	// ClassRollbackFailed means a restore failed and so did putting the previous
	// state back. The journal says where everything is.
	ClassRollbackFailed Class = "rollback_failed"
)

// Error is every failure this package reports on purpose.
type Error struct {
	Code  Code
	Class Class
	Msg   string
	Err   error
}

func (e *Error) Error() string {
	s := string(e.Code) + ": " + e.Msg
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// AsError extracts the package error from err, if there is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

func refusedf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Class: ClassRefused, Msg: fmt.Sprintf(format, args...)}
}

// failedf wraps err as a failure. A package error that is not a plain failure
// keeps its class, and a generic io_error wrapper never hides a more specific
// code underneath it.
func failedf(code Code, err error, format string, args ...any) *Error {
	if e, ok := AsError(err); ok && (e.Class != ClassFailed || code == CodeIO) {
		return e
	}
	return &Error{Code: code, Class: ClassFailed, Msg: fmt.Sprintf(format, args...), Err: err}
}

// Status is a verification verdict.
type Status string

// Verification verdicts.
const (
	StatusValid       Status = "VALID"
	StatusInvalid     Status = "INVALID"
	StatusUnsupported Status = "UNSUPPORTED"
)

// Finding is one reason attached to a verdict or a report.
type Finding struct {
	Code   Code   `json:"code"`
	Detail string `json:"detail"`
}
