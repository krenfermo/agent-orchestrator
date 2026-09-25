package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite" // the probe below opens the database directly

	"github.com/aoagents/agent-orchestrator/backend/internal/backup"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/usagebackfill"
)

// usage_backfill.go -- the one maintenance command that opens the store
// directly, and the reasons it is allowed to.
//
// This package's rule is that commands talk to the running daemon's loopback
// API and never to SQLite. `ao import` is the standing exception and this is
// the second, for the same reason: the daemon is the sole writer of the
// database, so an operation that rewrites rows underneath it cannot be done
// through it. It refuses to run while a daemon is alive, exactly as `ao import`
// does.
//
// DRY RUN IS THE DEFAULT AND --apply IS THE ONLY WAY TO WRITE. A maintenance
// command whose default mutates is one typo away from an incident, and this one
// touches the ledger.

type usageBackfillOptions struct {
	apply bool
	json  bool
}

func newUsageCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Maintain AO's token usage ledger",
	}
	cmd.AddCommand(newUsageBackfillCacheTTLCommand(ctx))
	cmd.AddCommand(newUsageCompactionVerdictsCommand(ctx))
	cmd.AddCommand(newUsageCompactionObservationsCommand(ctx))
	return cmd
}

func newUsageBackfillCacheTTLCommand(ctx *commandContext) *cobra.Command {
	var opts usageBackfillOptions
	cmd := &cobra.Command{
		Use:   "backfill-cache-ttl",
		Short: "Recover the cache-creation lifetime of historical usage events",
		Long: "Creating a cache entry that lives for an hour costs more than creating one that " +
			"lives five minutes, and the provider reports which it made on every call. Events " +
			"recorded before AO stored that distinction (migration 0170) carry no lifetime, so " +
			"every cost computed from them prices the dear kind at the cheap kind's rate.\n\n" +
			"This command reads it back from the transcripts AO already has, using AO's own " +
			"parser, matching each event by its exactly-once source_event_key, and verifying the " +
			"whole token vector before it writes. Nothing is estimated: an event whose lifetime " +
			"cannot be reconstructed exactly keeps its unknown, and a row that already has one is " +
			"never overwritten. Transcripts are read and never modified; the only thing written is " +
			"two integer columns.\n\n" +
			"Dry run by default. Pass --apply to write. The daemon must be stopped: it is the " +
			"sole writer of the database. Running it twice is safe -- the second run updates " +
			"nothing.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.runUsageBackfillCacheTTL(cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.apply, "apply", false, "Write the reconstructed lifetimes (default: dry run, no writes)")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output the report as JSON")
	return cmd
}

func (c *commandContext) runUsageBackfillCacheTTL(cmd *cobra.Command, opts usageBackfillOptions) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	release, err := holdDataDirOffline(cfg, "backfilling the usage ledger")
	if err != nil {
		return err
	}
	defer release()

	store, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	report, err := usagebackfill.Run(cmd.Context(), store, usagebackfill.Options{Apply: opts.apply})
	if err != nil {
		return err
	}
	if opts.json {
		return writeJSON(cmd.OutOrStdout(), newUsageBackfillReportJSON(report))
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), report.String())
	if err != nil {
		return err
	}
	if report.DryRun && report.RowsUpdated > 0 {
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"\nNothing was written. Re-run with --apply to record these %d lifetimes.\n", report.RowsUpdated)
	}
	return err
}

// usageBackfillReportJSON is the machine-readable shape. Counts and token
// totals only -- the same discipline the text report follows.
type usageBackfillReportJSON struct {
	DryRun              bool           `json:"dryRun"`
	SourcesConsidered   int            `json:"sourcesConsidered"`
	SourcesRead         int            `json:"sourcesRead"`
	SourcesUnsupported  int            `json:"sourcesUnsupported"`
	ParserErrors        int            `json:"parserErrors"`
	EventsReconstructed int            `json:"eventsReconstructed"`
	RowsMatched         int            `json:"rowsMatched"`
	RowsUpdated         int            `json:"rowsUpdated"`
	EventsSkipped       int            `json:"eventsSkipped"`
	SkippedByReason     map[string]int `json:"skippedByReason"`
	Recovered5mTokens   int64          `json:"recovered5mTokens"`
	Recovered1hTokens   int64          `json:"recovered1hTokens"`
	RecoveredTotal      int64          `json:"recoveredTotalTokens"`
}

func newUsageBackfillReportJSON(r *usagebackfill.Report) usageBackfillReportJSON {
	byReason := make(map[string]int, len(r.Skipped))
	for reason, n := range r.Skipped {
		byReason[string(reason)] = n
	}
	return usageBackfillReportJSON{
		DryRun:              r.DryRun,
		SourcesConsidered:   r.SourcesConsidered,
		SourcesRead:         r.SourcesRead,
		SourcesUnsupported:  r.SourcesUnsupported,
		ParserErrors:        r.ParserErrors,
		EventsReconstructed: r.EventsReconstructed,
		RowsMatched:         r.RowsMatched,
		RowsUpdated:         r.RowsUpdated,
		EventsSkipped:       r.SkippedTotal(),
		SkippedByReason:     byReason,
		Recovered5mTokens:   r.Recovered5m,
		Recovered1hTokens:   r.Recovered1h,
		RecoveredTotal:      r.RecoverableTotal,
	}
}

// assertNoLiveDaemon refuses to touch the database while something else has it.
//
// TWO LAYERS, AND THE SECOND ONE IS THE LOAD-BEARING ONE.
//
// The run-file check is the cheap, friendly layer: it names the pid and tells
// the operator to run `ao stop`. It is NOT sufficient on its own, and this is
// not a theoretical objection. `ao server --data-dir X` overrides the run-file
// path to X/running.json (server.go), ignoring both AO_RUN_FILE and the default
// under ~/.ao -- so the canonical location an offline command computes from
// config.Load() is NOT where a daemon launched that way writes. A live daemon
// was observed on this machine doing exactly that, with ~/.ao/running.json
// absent and ~/.ao/data/running.json holding its pid. A guard that trusted one
// path would have concluded "stopped" and rewritten the ledger underneath it.
// So both paths are checked.
//
// The second layer trusts no file at all. SQLite itself knows whether another
// connection is attached: opening with locking_mode=exclusive and taking a
// write transaction acquires a file lock that no other connection can hold, and
// a daemon with the database open -- even an idle one, because WAL keeps shared
// state mapped -- makes it fail. That is a fact about the database rather than
// about a handshake file somebody may have moved, and it is what makes this
// command safe to run when the run files disagree, are stale, are missing, or
// belong to an instance nobody remembers starting.
//
// Nothing is killed and nothing is waited on: the command refuses and returns.
func assertNoLiveDaemon(cfg config.Config) error {
	if err := assertNoLiveRunFiles(cfg); err != nil {
		return err
	}
	return assertDatabaseQuiet(cfg.DataDir)
}

// assertNoLiveRunFiles is the run-file layer of assertNoLiveDaemon: both
// conventions, a live PID refuses.
func assertNoLiveRunFiles(cfg config.Config) error {
	candidates := []string{cfg.RunFilePath}
	if cfg.DataDir != "" {
		dataDirRunFile := filepath.Join(cfg.DataDir, "running.json")
		if dataDirRunFile != cfg.RunFilePath {
			candidates = append(candidates, dataDirRunFile)
		}
	}
	for _, path := range candidates {
		live, err := runfile.CheckStale(path)
		if err != nil {
			return fmt.Errorf("inspect run-file %s: %w", path, err)
		}
		if live != nil {
			return usageError{fmt.Errorf(
				"the AO daemon is running (pid %d); stop it first with `ao stop`", live.PID)}
		}
	}
	return nil
}

// holdDataDirOffline is the guard for an offline command that WRITES the
// database (P10, closing the P9 import debt): both run-file conventions and
// the SQLite probe (assertNoLiveDaemon), then the data dir's P9 daemon.lock,
// held until release -- so no daemon can start underneath the write either.
// Nothing is signalled; a held lock refuses.
func holdDataDirOffline(cfg config.Config, doing string) (release func(), err error) {
	// Offline writers open the store with sqlite.Open, which migrates it: the
	// same production guardrail as the daemon applies (Frente 3 incident).
	if err := config.AuthorizeDefaultDataDir(cfg, os.LookupEnv); err != nil {
		return nil, usageError{err}
	}
	if err := assertNoLiveRunFiles(cfg); err != nil {
		return nil, err
	}
	if cfg.DataDir == "" {
		return func() {}, nil
	}
	lock, err := daemonlock.Acquire(filepath.Join(cfg.DataDir, "daemon.lock"))
	if errors.Is(err, daemonlock.ErrHeld) {
		return nil, usageError{fmt.Errorf("an AO daemon holds data dir %s; stop it with `ao stop` before %s", cfg.DataDir, doing)}
	}
	if err != nil {
		return nil, fmt.Errorf("lock data dir: %w", err)
	}
	// A restore interrupted mid-swap may leave a mix of two states: a write now
	// would be lost when recover rolls back, or would break the rollback itself.
	if err := backup.CheckStartup(cfg.DataDir); err != nil {
		_ = lock.Release()
		return nil, usageError{err}
	}
	// A fresh data dir (the import bootstrap) has no database for anyone to hold.
	if _, statErr := os.Stat(filepath.Join(cfg.DataDir, "ao.db")); statErr == nil {
		if err := assertDatabaseQuiet(cfg.DataDir); err != nil {
			_ = lock.Release()
			return nil, err
		}
	}
	return func() { _ = lock.Release() }, nil
}

// assertDatabaseQuiet asks SQLite whether anybody else has the database open.
//
// busy_timeout(0) is deliberate: this is a probe, not an attempt to win. A
// database another process holds must fail immediately and loudly rather than
// block an operator for seconds and then proceed.
func assertDatabaseQuiet(dataDir string) error {
	if dataDir == "" {
		return nil
	}
	path := filepath.Join(dataDir, "ao.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)&_pragma=locking_mode(exclusive)")
	if err != nil {
		return fmt.Errorf("probe database: %w", err)
	}
	defer func() { _ = db.Close() }()
	// BEGIN EXCLUSIVE forces the lock to be taken now rather than lazily at the
	// first write, which is what makes this a probe and not a hope.
	tx, err := db.Begin()
	if err == nil {
		_, err = tx.Exec("CREATE TABLE IF NOT EXISTS ao_backfill_lock_probe_never_created (x INTEGER)")
		if rollbackErr := tx.Rollback(); rollbackErr != nil && err == nil {
			err = rollbackErr
		}
	}
	if err != nil {
		if isDatabaseBusy(err) {
			return usageError{errors.New(
				"another process has AO's database open; stop the AO daemon with `ao stop` first")}
		}
		return fmt.Errorf("probe database %s: %w", path, err)
	}
	return nil
}

// isDatabaseBusy recognises SQLite's two flavours of "somebody else has it".
// Matched on the message because the driver does not export a typed sentinel
// for them, and a probe that failed to recognise a busy database would be worse
// than no probe.
func isDatabaseBusy(err error) bool {
	msg := err.Error()
	return containsAny(msg, "database is locked", "SQLITE_BUSY", "database table is locked", "locked")
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
