package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/backup"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

// backup.go -- `ao backup` and `ao restore` (P10).
//
// Like `ao import` and `ao usage backfill-cache-ttl`, these commands work on
// storage directly instead of through the daemon's API, for the same reason: a
// restore replaces the database the daemon owns, so it can only run with the
// daemon stopped; and a backup is a read-only SQLite snapshot that needs no
// daemon cooperation and must keep working when the daemon is down. The logic
// lives in internal/backup. Here: configuration, P9 daemon discovery as the
// "AO is stopped" proof, rendering, and stable exit codes.

// Exit codes for the backup family, beyond 0 (ok), 1 (failure) and 2 (usage).
const (
	exitBackupRefused        = 3
	exitBackupInvalid        = 4
	exitBackupIncompatible   = 5
	exitRestoreRolledBack    = 6
	exitRestoreRollbackFails = 7
)

// exitCodeError carries a specific process exit code.
type exitCodeError struct {
	code int
	err  error
}

func (e exitCodeError) Error() string { return e.err.Error() }
func (e exitCodeError) Unwrap() error { return e.err }

// backupExit maps a backup package error to its exit code.
func backupExit(err error) error {
	if err == nil {
		return nil
	}
	e, ok := backup.AsError(err)
	if !ok {
		return err
	}
	switch e.Class {
	case backup.ClassRefused:
		if e.Code == backup.CodeInvalidArgument {
			return usageError{err}
		}
		return exitCodeError{exitBackupRefused, err}
	case backup.ClassInvalid:
		return exitCodeError{exitBackupInvalid, err}
	case backup.ClassIncompatible:
		return exitCodeError{exitBackupIncompatible, err}
	case backup.ClassRolledBack:
		return exitCodeError{exitRestoreRolledBack, err}
	case backup.ClassRollbackFailed:
		return exitCodeError{exitRestoreRollbackFails, err}
	default:
		return err
	}
}

func newBackupCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Create, verify, list, prune and restore AO backups",
		Long: "An AO backup is a directory holding a consistent SQLite snapshot of the database " +
			"(taken with VACUUM INTO, safe while the daemon runs), the installation identity, the " +
			"installed skill packages, and a versioned manifest with a SHA-256 for each. Secrets " +
			"(secret.key, credentials, provider logins) are never included: back secret.key up " +
			"separately.\n\n" +
			"Verification proves integrity, not authenticity: restore only backups whose origin you know.\n\n" +
			"Exit codes: 0 ok, 1 failure, 2 usage, 3 refused as unsafe, 4 backup invalid, " +
			"5 backup unsupported or newer than this binary, 6 restore failed and rolled back, " +
			"7 restore failed and rollback failed.",
	}
	cmd.AddCommand(newBackupCreateCommand(ctx))
	cmd.AddCommand(newBackupVerifyCommand(ctx))
	cmd.AddCommand(newBackupListCommand(ctx))
	cmd.AddCommand(newBackupPruneCommand(ctx))
	cmd.AddCommand(newRestoreCommand(ctx, "restore <backup-dir>"))
	cmd.AddCommand(newBackupRecoverCommand(ctx))
	return cmd
}

func (c *commandContext) backupRoot(cfg config.Config, flag string) string {
	if flag != "" {
		return flag
	}
	if v, ok := c.deps.LookupEnv("AO_BACKUP_DIR"); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return backup.DefaultRoot(cfg.DataDir)
}

func backupTool() backup.ToolInfo {
	return backup.ToolInfo{Name: "ao", Version: Version, Commit: Commit}
}

func progressTo(w io.Writer) func(string) {
	return func(phase string) { _, _ = fmt.Fprintf(w, "ao backup: %s...\n", phase) }
}

// interruptible turns Ctrl+C / SIGTERM into a cancelled context instead of a
// killed process, so a backup can clean its staging and a restore can finish
// (or roll back) its critical section.
func interruptible(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
}

// ---- create ----

type backupCreateOptions struct {
	root string
	note string
	json bool
}

func newBackupCreateCommand(ctx *commandContext) *cobra.Command {
	var opts backupCreateOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Take a consistent backup (safe while AO runs)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			runCtx, stop := interruptible(cmd)
			defer stop()
			res, err := backup.Create(runCtx, backup.CreateOptions{
				DataDir: cfg.DataDir, Root: ctx.backupRoot(cfg, opts.root), Note: opts.note,
				Tool: backupTool(), Progress: progressTo(cmd.ErrOrStderr()),
			})
			if err != nil {
				return backupExit(err)
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writeCreateSummary(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&opts.root, "root", "", "Backup root (default $AO_BACKUP_DIR, else <data dir parent>/backups)")
	cmd.Flags().StringVar(&opts.note, "note", "", "Free-text note stored in the manifest (no secrets)")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output the result as JSON")
	return cmd
}

func writeCreateSummary(w io.Writer, res *backup.CreateResult) error {
	m := res.Manifest
	var b strings.Builder
	fmt.Fprintf(&b, "Backup created: %s\n", res.Path)
	fmt.Fprintf(&b, "  id:         %s\n", res.BackupID)
	fmt.Fprintf(&b, "  kind:       %s\n", res.Kind)
	fmt.Fprintf(&b, "  goose:      %d (this binary: %d)\n", m.Schema.GooseVersion, m.Schema.BinaryHead)
	fmt.Fprintf(&b, "  size:       %s in %d files\n", humanBytes(res.SizeBytes), len(m.Assets))
	fmt.Fprintf(&b, "  integrity:  %s, %d foreign-key violations\n", m.Checks.IntegrityCheck, m.Checks.ForeignKeyViolations)
	fmt.Fprintf(&b, "  duration:   %s\n", (time.Duration(res.DurationMs) * time.Millisecond).String())
	if m.Source.SecretKeyFingerprint != "" {
		b.WriteString("  note:       secret.key is NOT in the backup; keep your own copy of it\n")
	}
	fmt.Fprintf(&b, "Verify with: ao backup verify %s\n", res.Path)
	_, err := io.WriteString(w, b.String())
	return err
}

// ---- verify ----

type backupVerifyOptions struct {
	quick bool
	json  bool
}

func newBackupVerifyCommand(_ *commandContext) *cobra.Command {
	var opts backupVerifyOptions
	cmd := &cobra.Command{
		Use:   "verify <backup-dir>",
		Short: "Verify a backup's manifest, checksums and database (writes nothing)",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError{errors.New("verify takes exactly one backup directory")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			runCtx, stop := interruptible(cmd)
			defer stop()
			rep, err := backup.Verify(runCtx, args[0], backup.VerifyOptions{Quick: opts.quick, Progress: progressTo(cmd.ErrOrStderr())})
			if err != nil {
				return err
			}
			if opts.json {
				if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
			} else if err := writeVerifySummary(cmd.OutOrStdout(), rep); err != nil {
				return err
			}
			return verifyExit(rep)
		},
	}
	cmd.Flags().BoolVar(&opts.quick, "quick", false, "Run SQLite quick_check instead of the full integrity_check")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output the report as JSON")
	return cmd
}

func verifyExit(rep *backup.VerifyReport) error {
	reason := "no reason recorded"
	if len(rep.Reasons) > 0 {
		reason = string(rep.Reasons[0].Code) + ": " + rep.Reasons[0].Detail
	}
	switch {
	case rep.Status == backup.StatusUnsupported:
		return exitCodeError{exitBackupIncompatible, fmt.Errorf("backup is UNSUPPORTED (%s)", reason)}
	case rep.Status != backup.StatusValid:
		return exitCodeError{exitBackupInvalid, fmt.Errorf("backup is INVALID (%s)", reason)}
	case !rep.Restorable():
		return exitCodeError{exitBackupIncompatible, fmt.Errorf("backup is VALID but this binary cannot restore it (%s)", reason)}
	}
	return nil
}

func writeVerifySummary(w io.Writer, rep *backup.VerifyReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", rep.Status, rep.Path)
	if rep.BackupID != "" {
		fmt.Fprintf(&b, "  id:            %s (%s)\n", rep.BackupID, rep.Kind)
	}
	if rep.CreatedAt != nil {
		fmt.Fprintf(&b, "  created:       %s\n", rep.CreatedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "  schema:        goose %d, this binary %d: %s\n", rep.GooseVersion, rep.BinaryHead, rep.Compatibility)
	if rep.Integrity != "" {
		fmt.Fprintf(&b, "  %-14s %s\n", rep.CheckMode+":", rep.Integrity)
	}
	if rep.SizeBytes > 0 {
		fmt.Fprintf(&b, "  verified size: %s\n", humanBytes(rep.SizeBytes))
	}
	if rep.Compatibility == backup.CompatUpgradeRequired {
		b.WriteString("  note:          restoring does not migrate; the next AO start will\n")
	}
	if len(rep.Reasons) > 0 {
		b.WriteString("Reasons:\n")
		for _, r := range rep.Reasons {
			fmt.Fprintf(&b, "  - %s: %s\n", r.Code, r.Detail)
		}
	}
	b.WriteString("Verification proves integrity, not who produced the backup.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// ---- list ----

func newBackupListCommand(ctx *commandContext) *cobra.Command {
	var root string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List AO backups in the backup root (reads manifests only)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			rep, err := backup.List(cfg.DataDir, ctx.backupRoot(cfg, root))
			if err != nil {
				return backupExit(err)
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), rep)
			}
			return writeListSummary(cmd.OutOrStdout(), rep)
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "Backup root (default $AO_BACKUP_DIR, else <data dir parent>/backups)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the list as JSON")
	return cmd
}

func writeListSummary(w io.Writer, rep *backup.ListReport) error {
	if _, err := fmt.Fprintf(w, "Backup root: %s (this binary: goose %d)\n", rep.Root, rep.BinaryHead); err != nil {
		return err
	}
	if len(rep.Entries) == 0 {
		_, err := io.WriteString(w, "No AO backups.\n")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tCREATED (UTC)\tSIZE\tGOOSE\tCOMPATIBILITY\tSTATE")
	for _, e := range rep.Entries {
		created, size, goose := "-", "-", "-"
		if e.CreatedAt != nil {
			created = e.CreatedAt.UTC().Format("2006-01-02 15:04:05")
		}
		if e.SizeBytes > 0 {
			size = humanBytes(e.SizeBytes)
		}
		if e.GooseVersion > 0 {
			goose = strconv.FormatInt(e.GooseVersion, 10)
		}
		kind, compat := string(e.Kind), string(e.Compatibility)
		if kind == "" {
			kind = "-"
		}
		if compat == "" {
			compat = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.BackupID, kind, created, size, goose, compat, e.State)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	var b strings.Builder
	if op := rep.LastBackup; op != nil {
		fmt.Fprintf(&b, "Last backup:  %s at %s (verified at creation)\n", op.BackupID, op.At.UTC().Format(time.RFC3339))
	}
	if op := rep.LastRestore; op != nil {
		fmt.Fprintf(&b, "Last restore: %s of %s at %s", op.Result, op.BackupID, op.At.UTC().Format(time.RFC3339))
		if op.RollbackBackupID != "" {
			fmt.Fprintf(&b, ", pre-restore backup %s", op.RollbackBackupID)
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// ---- prune ----

type backupPruneOptions struct {
	root   string
	keep   int
	maxAge string
	apply  bool
	json   bool
}

func newBackupPruneCommand(ctx *commandContext) *cobra.Command {
	var opts backupPruneOptions
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Apply retention to manual backups (dry run unless --apply)",
		Long: "Deletes only manual AO backups outside the newest --keep (and, with --max-age, older " +
			"than it). Never deletes pre-restore or pre-migration backups, the newest backup, a backup " +
			"in use, one whose manifest cannot be read, or anything that is not an AO backup. Also " +
			"removes staging left by crashed creates and interrupted deletions.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			age, err := parseRetentionAge(opts.maxAge)
			if err != nil {
				return usageError{err}
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			runCtx, stop := interruptible(cmd)
			defer stop()
			rep, err := backup.Prune(runCtx, backup.PruneOptions{DataDir: cfg.DataDir, Root: ctx.backupRoot(cfg, opts.root),
				KeepLast: opts.keep, MaxAge: age, Apply: opts.apply})
			if err != nil {
				return backupExit(err)
			}
			if opts.json {
				if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
			} else if err := writePruneSummary(cmd.OutOrStdout(), rep); err != nil {
				return err
			}
			if rep.Errors > 0 {
				return fmt.Errorf("prune: %d deletions failed; the remaining backups are intact", rep.Errors)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.root, "root", "", "Backup root (default $AO_BACKUP_DIR, else <data dir parent>/backups)")
	cmd.Flags().IntVar(&opts.keep, "keep", backup.DefaultKeepLast, "Manual backups always kept")
	cmd.Flags().StringVar(&opts.maxAge, "max-age", "", "Also keep manual backups younger than this (e.g. 72h, 30d)")
	cmd.Flags().BoolVar(&opts.apply, "apply", false, "Delete (default: dry run)")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output the decisions as JSON")
	return cmd
}

func parseRetentionAge(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("--max-age %q: want a duration such as 72h or 30d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("--max-age %q: want a duration such as 72h or 30d", s)
	}
	return d, nil
}

func writePruneSummary(w io.Writer, rep *backup.PruneReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Backup root: %s (keep %d", rep.Root, rep.KeepLast)
	if rep.MaxAge != "" {
		fmt.Fprintf(&b, ", max age %s", rep.MaxAge)
	}
	b.WriteString(")\n")
	pending := 0
	for _, d := range rep.Decisions {
		mark := d.Action
		if d.Done {
			mark += " (done)"
		} else if d.Action == backup.ActionDelete || d.Action == backup.ActionRemoveIncomplete {
			pending++
		}
		fmt.Fprintf(&b, "  %-26s %s  %s", mark, d.Name, d.Reason)
		if d.Error != "" {
			fmt.Fprintf(&b, "  ERROR: %s", d.Error)
		}
		b.WriteString("\n")
	}
	if rep.DryRun {
		fmt.Fprintf(&b, "Dry run: nothing was deleted. %d entries would be removed; re-run with --apply.\n", pending)
	} else {
		fmt.Fprintf(&b, "Deleted %d backups; %d errors.\n", rep.Deleted, rep.Errors)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// ---- restore ----

type restoreOptions struct {
	root                   string
	identity               string
	allowSecretKeyMismatch bool
	yes                    bool
	json                   bool
}

// newRestoreCommand serves both `ao backup restore` and the top-level `ao restore`.
func newRestoreCommand(ctx *commandContext, use string) *cobra.Command {
	var opts restoreOptions
	cmd := &cobra.Command{
		Use:   use,
		Short: "Restore a backup over a STOPPED AO (takes a pre-restore backup first)",
		Long: "Restores a verified backup into AO's data dir. AO must be stopped: the restore refuses " +
			"when any daemon of this installation is running, unhealthy or unverifiable, when the data " +
			"dir's daemon lock is held, or when anything has the database open. It never signals or " +
			"stops a process.\n\n" +
			"Before replacing anything it takes a verified pre-restore backup of the current state. The " +
			"backup is staged and checked on the destination filesystem, swapped in by rename, and " +
			"verified again; a failure after the swap puts the previous state back. Migrations do not " +
			"run: the next `ao start` decides.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError{errors.New("restore takes exactly one backup directory")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.runRestore(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.root, "root", "", "Where the pre-restore backup goes (default $AO_BACKUP_DIR, else <data dir parent>/backups)")
	cmd.Flags().StringVar(&opts.identity, "identity", "", "On an installation identity mismatch: backup (adopt the backup's) or destination (keep this one's)")
	cmd.Flags().BoolVar(&opts.allowSecretKeyMismatch, "allow-secret-key-mismatch", false, "Restore even though secret.key differs (encrypted settings must be re-entered)")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Do not ask for confirmation")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output the report as JSON")
	return cmd
}

func (c *commandContext) runRestore(cmd *cobra.Command, source string, opts restoreOptions) error {
	var policy backup.IdentityPolicy
	switch opts.identity {
	case "":
	case "backup":
		policy = backup.IdentityFromBackup
	case "destination":
		policy = backup.IdentityKeepDestination
	default:
		return usageError{fmt.Errorf("--identity must be backup or destination, not %q", opts.identity)}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !opts.yes {
		if !stdinIsInteractive(c.deps.In) {
			return usageError{errors.New("restore replaces AO's database; pass --yes to confirm in a non-interactive shell")}
		}
		ok, err := confirm(c.deps.In, cmd.OutOrStdout(),
			fmt.Sprintf("Replace the AO data in %s with the backup at %s? A pre-restore backup is taken first.", cfg.DataDir, source), false)
		if err != nil {
			return err
		}
		if !ok {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Restore cancelled; nothing was changed.")
			return err
		}
	}
	runCtx, stop := interruptible(cmd)
	defer stop()
	rep, rerr := backup.Restore(runCtx, backup.RestoreOptions{
		DataDir: cfg.DataDir, Source: source, Root: c.backupRoot(cfg, opts.root),
		Identity: policy, AllowSecretKeyMismatch: opts.allowSecretKeyMismatch,
		CheckDaemon: c.daemonStoppedCheck(cfg), Tool: backupTool(), Progress: progressTo(cmd.ErrOrStderr()),
	})
	if opts.json {
		if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
			return err
		}
	} else if err := writeRestoreSummary(cmd.OutOrStdout(), rep); err != nil {
		return err
	}
	return backupExit(rerr)
}

func writeRestoreSummary(w io.Writer, rep *backup.RestoreReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", rep.Result)
	fmt.Fprintf(&b, "  backup:        %s", rep.Source)
	if rep.SourceBackupID != "" {
		fmt.Fprintf(&b, " (%s)", rep.SourceBackupID)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  data dir:      %s\n", rep.DataDir)
	if rep.GooseVersion > 0 {
		fmt.Fprintf(&b, "  schema:        goose %d, this binary %d: %s\n", rep.GooseVersion, rep.BinaryHead, rep.Compatibility)
	}
	if rep.RollbackBackupPath != "" {
		fmt.Fprintf(&b, "  pre-restore:   %s\n", rep.RollbackBackupPath)
	}
	if rep.IdentityAction != "" {
		fmt.Fprintf(&b, "  identity:      %s\n", rep.IdentityAction)
	}
	fmt.Fprintf(&b, "  data changed:  %t\n", rep.DestinationTouched && rep.Result == backup.ResultRestored)
	for _, warn := range rep.Warnings {
		fmt.Fprintf(&b, "  warning: %s: %s\n", warn.Code, warn.Detail)
	}
	for _, r := range rep.Reasons {
		fmt.Fprintf(&b, "  - %s: %s\n", r.Code, r.Detail)
	}
	switch {
	case rep.Result == backup.ResultRollbackFailed:
		b.WriteString("DO NOT start AO. Run `ao backup recover`.\n")
	case rep.RecoverRequired:
		b.WriteString("The restore journal could not be cleared. Run `ao backup recover` before starting AO.\n")
	case rep.Result == backup.ResultRestored:
		b.WriteString("Start AO with `ao start`.\n")
	case rep.Result == backup.ResultRolledBack:
		b.WriteString("The previous state is back. Nothing else needs to be done before starting AO.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// daemonStoppedCheck is the P9 proof a restore requires: every run-file
// location for this installation is inspected and probed, and anything alive
// -- verified, unhealthy or unverifiable -- refuses. Nothing is signalled and
// no run-file is removed.
func (c *commandContext) daemonStoppedCheck(cfg config.Config) func(context.Context) error {
	return func(ctx context.Context) error {
		_, cands, err := c.discoverDaemon(ctx, cfg)
		if err != nil {
			return &backup.Error{Code: backup.CodeDaemonAmbiguous, Class: backup.ClassRefused, Msg: err.Error()}
		}
		for _, cand := range cands {
			switch cand.State {
			case stateReady, stateNotReady, stateUnhealthy:
				return &backup.Error{Code: backup.CodeDaemonActive, Class: backup.ClassRefused, Msg: fmt.Sprintf(
					"an AO daemon is running for %s (pid %d, %s, run-file %s); stop it with `ao stop` first",
					cfg.DataDir, cand.PID, cand.State, cand.RunFile)}
			case stateUnverified:
				return &backup.Error{Code: backup.CodeDaemonUnverified, Class: backup.ClassRefused, Msg: fmt.Sprintf(
					"a live process (pid %d) is recorded in %s but could not be verified as this installation's daemon (%s). "+
						"AO will not signal it: check `ao status`, confirm what pid %d is, and only then deal with the run-file by hand",
					cand.PID, cand.RunFile, cand.Error, cand.PID)}
			}
		}
		return nil
	}
}

// ---- recover ----

func newBackupRecoverCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Resolve a restore that was interrupted (rolls back, never forward)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			runCtx, stop := interruptible(cmd)
			defer stop()
			rep, rerr := backup.Recover(runCtx, backup.RecoverOptions{DataDir: cfg.DataDir,
				CheckDaemon: ctx.daemonStoppedCheck(cfg), Progress: progressTo(cmd.ErrOrStderr())})
			if rep != nil {
				if asJSON {
					if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
						return err
					}
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", rep.Result)
					if rep.RestoreID != "" {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  restore: %s (phase %s)\n", rep.RestoreID, rep.Phase)
					}
					if rep.Detail != "" {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", rep.Detail)
					}
					if rep.RollbackBackupPath != "" {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  pre-restore backup: %s\n", rep.RollbackBackupPath)
					}
				}
			}
			return backupExit(rerr)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the report as JSON")
	return cmd
}
